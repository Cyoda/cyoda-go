package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// scheduledTaskStore implements spi.ScheduledTaskStore backed by PostgreSQL.
// Unlike the memory/sqlite plugins (buffer-and-flush tx staging), postgres
// writes through q — a context-resolving Querier (see StoreFactory.querier)
// that re-resolves the active pgx.Tx from ctx on every call. That means
// Upsert/Delete/ReconcileForEntity automatically join whatever entity-write
// transaction the caller's ctx carries: no tx-buffer staging and no
// savepoint bookkeeping are needed here, because PostgreSQL's own
// SAVEPOINT/ROLLBACK TO SAVEPOINT/COMMIT/ROLLBACK already govern everything
// written through q.
type scheduledTaskStore struct {
	q Querier
}

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

const scheduledTaskColumns = `id, tenant_id, type, scheduled_time, timeout_ms, redispatch_after, entity_id, model_name, model_version, transition, source_state, armed_at, attempt_count`

// upsertScheduledTaskSQL sets every non-id column from excluded.* so a
// re-arm Upsert on an existing ID fully replaces the row rather than
// merging a subset. model_version is not part of the deterministic ID hash
// (see spi.ScheduledTask.ID godoc) and can legitimately change between two
// arms of the same id, so it must be included here — a partial SET list
// would leave a stale model_version behind after a model bump.
const upsertScheduledTaskSQL = `INSERT INTO scheduled_tasks (` + scheduledTaskColumns + `)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	ON CONFLICT (id) DO UPDATE SET
	  tenant_id = excluded.tenant_id,
	  type = excluded.type,
	  scheduled_time = excluded.scheduled_time,
	  timeout_ms = excluded.timeout_ms,
	  redispatch_after = excluded.redispatch_after,
	  entity_id = excluded.entity_id,
	  model_name = excluded.model_name,
	  model_version = excluded.model_version,
	  transition = excluded.transition,
	  source_state = excluded.source_state,
	  armed_at = excluded.armed_at,
	  attempt_count = excluded.attempt_count`

func (s *scheduledTaskStore) Upsert(ctx context.Context, task spi.ScheduledTask) error {
	_, err := s.q.Exec(ctx, upsertScheduledTaskSQL,
		task.ID, string(task.TenantID), string(task.Type), task.ScheduledTime,
		task.TimeoutMs, task.RedispatchAfter, task.EntityID, task.ModelName,
		task.ModelVersion, task.Transition, task.SourceState, task.ArmedAt, task.AttemptCount)
	if err != nil {
		return fmt.Errorf("upsert scheduled task %s: %w", task.ID, err)
	}
	return nil
}

// Delete removes the task, reporting whether a row was actually removed via
// the command tag's affected-row count (delete-gated terminal audit relies
// on this, matching the memory/sqlite plugins' Delete contract).
func (s *scheduledTaskStore) Delete(ctx context.Context, id string) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM scheduled_tasks WHERE id=$1`, id)
	if err != nil {
		return false, fmt.Errorf("delete scheduled task %s: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// scanScheduledTaskRow scans one row into a ScheduledTask. scan is either a
// *pgx.Row's or pgx.Rows' Scan method — both share this signature.
// TimeoutMs/RedispatchAfter scan directly into the *int64 struct fields
// (pgx v5 natively supports pointer-to-pointer destinations for nullable
// columns: NULL sets the field to nil, a non-null value allocates and
// populates it) — no intermediate sql.NullInt64 needed.
func scanScheduledTaskRow(scan func(dest ...any) error) (spi.ScheduledTask, error) {
	var t spi.ScheduledTask
	var tenantID, taskType string
	err := scan(&t.ID, &tenantID, &taskType, &t.ScheduledTime, &t.TimeoutMs, &t.RedispatchAfter,
		&t.EntityID, &t.ModelName, &t.ModelVersion, &t.Transition, &t.SourceState,
		&t.ArmedAt, &t.AttemptCount)
	if err != nil {
		return spi.ScheduledTask{}, err
	}
	t.TenantID = spi.TenantID(tenantID)
	t.Type = spi.ScheduledTaskType(taskType)
	return t, nil
}

func (s *scheduledTaskStore) Get(ctx context.Context, id string) (*spi.ScheduledTask, bool, error) {
	row := s.q.QueryRow(ctx, `SELECT `+scheduledTaskColumns+` FROM scheduled_tasks WHERE id=$1`, id)
	t, err := scanScheduledTaskRow(row.Scan)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get scheduled task %s: %w", id, err)
	}
	return &t, true, nil
}

// ScanDue is cross-tenant by design (see spi.ScheduledTaskStore godoc and
// migration 000004's RLS-exemption note) — no tenant filter. A non-positive
// limit is passed through as SQL NULL, which PostgreSQL's LIMIT clause
// treats as "no limit" (same as omitting LIMIT) — a literal negative
// integer would instead be rejected by the server.
func (s *scheduledTaskStore) ScanDue(ctx context.Context, nowMs int64, limit int) ([]spi.ScheduledTask, error) {
	var limitArg *int
	if limit > 0 {
		limitArg = &limit
	}

	rows, err := s.q.Query(ctx,
		`SELECT `+scheduledTaskColumns+` FROM scheduled_tasks
		 WHERE scheduled_time<=$1 AND (redispatch_after IS NULL OR redispatch_after<=$1)
		 ORDER BY scheduled_time
		 LIMIT $2`, nowMs, limitArg)
	if err != nil {
		return nil, fmt.Errorf("scan due scheduled tasks: %w", err)
	}
	defer rows.Close()

	var out []spi.ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTaskRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan due scheduled tasks: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan due scheduled tasks: %w", err)
	}
	return out, nil
}

// MarkRedispatch is a plain (non-transactional-by-contract) write — see
// spi.ScheduledTaskStore godoc. It still runs through q, so if the caller
// happens to invoke it with a tx-carrying ctx it joins that tx like any
// other call; callers are simply not required to provide one.
func (s *scheduledTaskStore) MarkRedispatch(ctx context.Context, id string, redispatchAfterMs int64) error {
	_, err := s.q.Exec(ctx,
		`UPDATE scheduled_tasks SET redispatch_after=$2, attempt_count=attempt_count+1 WHERE id=$1`,
		id, redispatchAfterMs)
	if err != nil {
		return fmt.Errorf("mark redispatch for scheduled task %s: %w", id, err)
	}
	return nil
}

func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	cancelled, err := s.selectOtherState(ctx, req.TenantID, req.EntityID, req.CurrentState)
	if err != nil {
		return nil, err
	}

	for _, t := range req.Arm {
		if err := s.Upsert(ctx, t); err != nil {
			return nil, err
		}
	}
	for _, c := range cancelled {
		if _, err := s.Delete(ctx, c.ID); err != nil {
			return nil, err
		}
	}
	// req.Cancel deletes join the same pgx.Tx as the above (via s.q), but are
	// deliberately excluded from the returned cancelled slice: the caller
	// audits them separately as EXPIRE (born-expired scheduled transitions),
	// not CANCEL (SourceState mismatch).
	for _, id := range req.Cancel {
		if _, err := s.Delete(ctx, id); err != nil {
			return nil, err
		}
	}
	return cancelled, nil
}

// selectOtherState returns entityID's pending tasks whose source_state !=
// currentState — the reconcile cancellation set.
func (s *scheduledTaskStore) selectOtherState(ctx context.Context, tenantID spi.TenantID, entityID, currentState string) ([]spi.ScheduledTask, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+scheduledTaskColumns+` FROM scheduled_tasks
		 WHERE tenant_id=$1 AND entity_id=$2 AND source_state<>$3`,
		string(tenantID), entityID, currentState)
	if err != nil {
		return nil, fmt.Errorf("select other-state scheduled tasks for %s: %w", entityID, err)
	}
	defer rows.Close()

	var out []spi.ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTaskRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("select other-state scheduled tasks for %s: %w", entityID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select other-state scheduled tasks for %s: %w", entityID, err)
	}
	return out, nil
}
