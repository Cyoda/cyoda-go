package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// scheduledTaskOpKind discriminates the two mutation shapes a transaction can
// stage against the ScheduledTask store.
type scheduledTaskOpKind int

const (
	scheduledTaskUpsert scheduledTaskOpKind = iota
	scheduledTaskDelete
)

// scheduledTaskOp is one staged ScheduledTaskStore mutation, buffered on the
// transactionManager (keyed by txID — see transactionManager.scheduledTaskOps)
// while a transaction is open, and flushed to SQLite inside flushToSQLite's
// single sqlTx (after the entity flush) so it commits atomically with the
// entity write. Mirrors the memory plugin's staging pattern, and the
// txUniqueKeys staging already used by this plugin.
type scheduledTaskOp struct {
	kind scheduledTaskOpKind
	id   string
	task spi.ScheduledTask // populated for scheduledTaskUpsert
}

// upsertScheduledTaskSQL and deleteScheduledTaskSQL are shared between the
// non-transactional immediate-apply path (applyScheduledTaskOpDirect) and
// the transactional flush path (flushToSQLite), so both use the exact same
// statements.
// The ON CONFLICT DO UPDATE clause sets every non-id column from excluded.*
// so a re-arm upsert on an existing ID fully replaces the row (matching the
// memory plugin's full-replace semantics) rather than merging a subset —
// notably model_version is NOT part of the deterministic ID hash and can
// legitimately change between two arms, so it must be included here.
const upsertScheduledTaskSQL = `INSERT INTO scheduled_tasks
	 (id,tenant_id,type,scheduled_time,timeout_ms,redispatch_after,entity_id,model_name,model_version,transition,source_state,armed_at,attempt_count,armed_by_id,armed_by_kind)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	 ON CONFLICT(id) DO UPDATE SET tenant_id=excluded.tenant_id,
	   type=excluded.type, scheduled_time=excluded.scheduled_time,
	   timeout_ms=excluded.timeout_ms, redispatch_after=excluded.redispatch_after,
	   entity_id=excluded.entity_id, model_name=excluded.model_name,
	   model_version=excluded.model_version, transition=excluded.transition,
	   source_state=excluded.source_state, armed_at=excluded.armed_at,
	   attempt_count=excluded.attempt_count, armed_by_id=excluded.armed_by_id,
	   armed_by_kind=excluded.armed_by_kind`

const deleteScheduledTaskSQL = `DELETE FROM scheduled_tasks WHERE id=?`

// copyScheduledTask returns a value copy of t that shares no pointers with
// t, so that neither the caller nor the store can mutate the other's copy
// after the call returns. A transactional Upsert only stages t on the
// transactionManager (see stage) — it is not serialized to SQL args until
// flushToSQLite runs the deferred ExecContext call, so the caller mutating
// its *int64 fields between Upsert and Commit would otherwise leak through.
// Mirrors the memory plugin's copyScheduledTask (store copy-on-write
// discipline, matching copyEntity).
func copyScheduledTask(t spi.ScheduledTask) spi.ScheduledTask {
	cp := t
	if t.TimeoutMs != nil {
		v := *t.TimeoutMs
		cp.TimeoutMs = &v
	}
	if t.RedispatchAfter != nil {
		v := *t.RedispatchAfter
		cp.RedispatchAfter = &v
	}
	return cp
}

// upsertScheduledTaskArgs returns the positional args for upsertScheduledTaskSQL.
func upsertScheduledTaskArgs(t spi.ScheduledTask) []any {
	return []any{
		t.ID, string(t.TenantID), string(t.Type), t.ScheduledTime,
		t.TimeoutMs, t.RedispatchAfter, t.EntityID, t.ModelName,
		t.ModelVersion, t.Transition, t.SourceState, t.ArmedAt, t.AttemptCount,
		t.ArmedBy.ID, string(t.ArmedBy.Kind),
	}
}

// applyScheduledTaskOp executes op against exec (either a *sql.DB for the
// non-transactional immediate-apply path or a *sql.Tx inside flushToSQLite).
func applyScheduledTaskOp(ctx context.Context, exec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, op scheduledTaskOp) error {
	switch op.kind {
	case scheduledTaskUpsert:
		_, err := exec.ExecContext(ctx, upsertScheduledTaskSQL, upsertScheduledTaskArgs(op.task)...)
		return err
	case scheduledTaskDelete:
		_, err := exec.ExecContext(ctx, deleteScheduledTaskSQL, op.id)
		return err
	}
	return nil
}

type scheduledTaskStore struct {
	db *sql.DB
	tm *transactionManager
}

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// stage applies op immediately (implicit auto-commit) if ctx carries no
// transaction, matching entityStore.Save's non-transaction mode. Otherwise it
// buffers op on the transaction's transactionManager entry so Commit's
// flushToSQLite can apply it atomically with the entity buffer flush (inside
// the same sqlTx), or Rollback can discard it untouched.
//
// Locking discipline: mirrors entityStore.Save — tx.OpMu.RLock is held for
// the duration of the staging so Commit/Rollback (which take tx.OpMu.Lock)
// cannot race with us.
func (s *scheduledTaskStore) stage(ctx context.Context, op scheduledTaskOp) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		return applyScheduledTaskOp(ctx, s.db, op)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	s.tm.stageScheduledTaskOp(tx.ID, op)
	return nil
}

func (s *scheduledTaskStore) Upsert(ctx context.Context, task spi.ScheduledTask) error {
	return s.stage(ctx, scheduledTaskOp{kind: scheduledTaskUpsert, id: task.ID, task: copyScheduledTask(task)})
}

func (s *scheduledTaskStore) Delete(ctx context.Context, id string) (bool, error) {
	// Existence is reported against committed state, matching the memory
	// plugin's Delete contract (delete-gated terminal audit relies on it).
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM scheduled_tasks WHERE id=?`, id).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("check scheduled task %s: %w", id, err)
	}
	existed := err == nil

	if err := s.stage(ctx, scheduledTaskOp{kind: scheduledTaskDelete, id: id}); err != nil {
		return false, err
	}
	return existed, nil
}

// scanScheduledTaskRow scans one row into a ScheduledTask. row is either
// *sql.Row or *sql.Rows — both satisfy this minimal Scan signature.
func scanScheduledTaskRow(scan func(dest ...any) error) (spi.ScheduledTask, error) {
	var t spi.ScheduledTask
	var tenantID, taskType string
	var timeoutMs, redispatchAfter sql.NullInt64
	var armedByID, armedByKind string
	err := scan(&t.ID, &tenantID, &taskType, &t.ScheduledTime, &timeoutMs, &redispatchAfter,
		&t.EntityID, &t.ModelName, &t.ModelVersion, &t.Transition, &t.SourceState,
		&t.ArmedAt, &t.AttemptCount, &armedByID, &armedByKind)
	if err != nil {
		return spi.ScheduledTask{}, err
	}
	t.TenantID = spi.TenantID(tenantID)
	t.Type = spi.ScheduledTaskType(taskType)
	if timeoutMs.Valid {
		v := timeoutMs.Int64
		t.TimeoutMs = &v
	}
	if redispatchAfter.Valid {
		v := redispatchAfter.Int64
		t.RedispatchAfter = &v
	}
	// A legacy row (pre-dating these columns) backfills both to '' via the
	// migration's DEFAULT '', which yields the zero Principal here — never a
	// synthesized one.
	t.ArmedBy = spi.Principal{ID: armedByID, Kind: spi.PrincipalKind(armedByKind)}
	return t, nil
}

const selectScheduledTaskColumns = `id,tenant_id,type,scheduled_time,timeout_ms,redispatch_after,entity_id,model_name,model_version,transition,source_state,armed_at,attempt_count,armed_by_id,armed_by_kind`

func (s *scheduledTaskStore) Get(ctx context.Context, id string) (*spi.ScheduledTask, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+selectScheduledTaskColumns+` FROM scheduled_tasks WHERE id=?`, id)
	t, err := scanScheduledTaskRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get scheduled task %s: %w", id, err)
	}
	return &t, true, nil
}

// ScanDue is cross-tenant by design (see spi.ScheduledTaskStore godoc) — no
// tenant filter.
func (s *scheduledTaskStore) ScanDue(ctx context.Context, nowMs int64, limit int) ([]spi.ScheduledTask, error) {
	// sqlite treats a negative LIMIT as unbounded.
	limitArg := limit
	if limitArg <= 0 {
		limitArg = -1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectScheduledTaskColumns+` FROM scheduled_tasks
		 WHERE scheduled_time<=? AND (redispatch_after IS NULL OR redispatch_after<=?)
		 ORDER BY scheduled_time
		 LIMIT ?`, nowMs, nowMs, limitArg)
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

// MarkRedispatch is a plain (non-transactional) write — it never
// participates in the caller's transaction (see spi.ScheduledTaskStore
// godoc), so it always applies directly against s.db.
func (s *scheduledTaskStore) MarkRedispatch(ctx context.Context, id string, redispatchAfterMs int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_tasks SET redispatch_after=?, attempt_count=attempt_count+1 WHERE id=?`,
		redispatchAfterMs, id)
	if err != nil {
		return fmt.Errorf("mark redispatch for scheduled task %s: %w", id, err)
	}
	return nil
}

func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectScheduledTaskColumns+` FROM scheduled_tasks
		 WHERE tenant_id=? AND entity_id=? AND source_state<>?`,
		string(req.TenantID), req.EntityID, req.CurrentState)
	if err != nil {
		return nil, fmt.Errorf("select other-state scheduled tasks for %s: %w", req.EntityID, err)
	}
	var cancelled []spi.ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTaskRow(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("select other-state scheduled tasks for %s: %w", req.EntityID, err)
		}
		cancelled = append(cancelled, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("select other-state scheduled tasks for %s: %w", req.EntityID, err)
	}
	rows.Close()

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
	// req.Cancel deletes are staged in the same transaction as the above,
	// but deliberately excluded from the returned cancelled slice: the
	// caller audits them separately as EXPIRE (born-expired scheduled
	// transitions), not CANCEL (SourceState mismatch).
	for _, id := range req.Cancel {
		if _, err := s.Delete(ctx, id); err != nil {
			return nil, err
		}
	}
	return cancelled, nil
}
