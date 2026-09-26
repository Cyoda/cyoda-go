package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// scheduledTaskStore implements spi.ScheduledTaskStore on PostgreSQL.
//
// The SPI says which methods join the transaction on ctx; the querier follows:
//
//   - q joins it (ctxQuerier): ReconcileForEntity, RemoveLife, StampSegment,
//     DeleteForEntities, DeleteForModel, Fail, and Get when the transaction
//     is open and of Get's tenant. Task rows are written
//     straight into the open entity transaction, which runs at REPEATABLE
//     READ. A task row that another transaction changed after this one's
//     snapshot raises 40001, which the querier maps to spi.ErrConflict (C1,
//     C5). A row this transaction wrote or locked stays locked until it ends,
//     so ClaimDue's and GiveBackIdle's SKIP LOCKED pass it over, MarkUnsafe's
//     NOWAIT answers ErrTaskBusy, and RecordAttempt answers ErrTaskBusy once
//     lock_timeout ends its wait (C6).
//   - query never joins and runs on the main pool: Query, and every other Get.
//   - sched never joins and runs on the scheduler pool (READ COMMITTED,
//     lock_timeout 2s): ClaimDue, MarkUnsafe, RecordAttempt, GiveBackIdle,
//     RetireOwner, SweepOwners, SweepMarks.
//   - heartbeat never joins and has one connection of its own: Heartbeat.
//
// Tenant scoping. Every tenant-facing statement filters on tenant_id, and every
// joining write refuses a tenant that is not the transaction's (joinTenant).
// ClaimDue, GiveBackIdle, the owner methods and the sweeps are cross-tenant;
// no API reaches them. None of the tables is under row-level security (000014).
//
// Input that no backend stores as given — an arm request with an empty id or
// an id in both Arm and Cancel, an unknown failure reason, an error text that
// is too long, not UTF-8 or holds a NUL — is refused with spi.ErrStoreRejected
// before any statement runs, by the SPI's shared validators.
//
// Refusals come in one order on every backend: input validation
// (ErrStoreRejected), then the transaction's tenant (ErrTxTenantMismatch),
// then the fence and busy checks.
type scheduledTaskStore struct {
	q     Querier
	query Querier
	pool  *pgxpool.Pool
	// txOpen reports whether the transaction manager still holds txID: a
	// transaction leaves its registry on every Commit and Rollback path.
	txOpen    func(txID string) bool
	sched     schedulerQuerier
	heartbeat schedulerQuerier
}

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// taskColumns is every column scanTask reads, in order, on the alias st.
const taskColumns = `st.id, st.tenant_id, st.type, st.scheduled_time, st.timeout_ms, st.entity_id,
	st.model_name, st.model_version, st.transition, st.source_state, st.armed_at,
	st.armed_by_id, st.armed_by_kind, st.arm_token, st.status, st.next_attempt_time,
	st.attempts, st.lost_owners, st.last_attempt_time, st.last_error, st.failure_reason,
	st.failed_time, st.partial_commit, st.claim_token, st.claim_owner`

// markedColumn reports whether a mark exists for the row's current life.
const markedColumn = `EXISTS (SELECT 1 FROM scheduled_task_marks m
	WHERE m.tenant_id = st.tenant_id AND m.task_id = st.id AND m.arm_token = st.arm_token)`

// scanTask reads taskColumns, then any extra destinations. scan is a pgx.Row's
// or pgx.Rows' Scan. scheduled_tasks_claim_pair_chk guarantees claim_token and
// claim_owner are both set or both NULL.
func scanTask(scan func(dest ...any) error, extra ...any) (spi.ScheduledTask, error) {
	var (
		t                                  spi.ScheduledTask
		tenantID, taskType, status, reason string
		armedByID, armedByKind             string
		claimToken, claimOwner             *uuid.UUID
	)
	dest := append([]any{
		&t.ID, &tenantID, &taskType, &t.ScheduledTime, &t.TimeoutMs, &t.EntityID,
		&t.ModelName, &t.ModelVersion, &t.Transition, &t.SourceState, &t.ArmedAt,
		&armedByID, &armedByKind, &t.ArmToken, &status, &t.NextAttemptTime,
		&t.Attempts, &t.LostOwners, &t.LastAttemptTime, &t.LastError, &reason,
		&t.FailedTime, &t.PartialCommit, &claimToken, &claimOwner,
	}, extra...)
	if err := scan(dest...); err != nil {
		return spi.ScheduledTask{}, err
	}
	t.TenantID = spi.TenantID(tenantID)
	t.Type = spi.ScheduledTaskType(taskType)
	// A row armed before the attribution columns existed reads back as the
	// zero Principal, never a synthesized one.
	t.ArmedBy = spi.Principal{ID: armedByID, Kind: spi.PrincipalKind(armedByKind)}
	t.Status = spi.ScheduledTaskStatus(status)
	t.FailureReason = spi.ScheduledTaskFailureReason(reason)
	if claimToken != nil && claimOwner != nil {
		t.Claim = &spi.TaskClaim{Token: *claimToken, Owner: *claimOwner}
	}
	return t, nil
}

func scanTasks(rows pgx.Rows) ([]spi.ScheduledTask, error) {
	defer rows.Close()
	var out []spi.ScheduledTask
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// taskKey names a task row: a task is keyed by (tenant, id).
type taskKey struct{ tenant, id string }

func keyOf(t spi.ScheduledTask) taskKey { return taskKey{tenant: string(t.TenantID), id: t.ID} }

func staleClaim(verb, id string) error {
	return fmt.Errorf("%s scheduled task %s: %w", verb, id, spi.ErrStaleClaim)
}

// taskBusy wraps the 55P03 of a never-joining write that met a row an open
// transaction wrote (C6). The SQLSTATE stays in the chain.
func taskBusy(verb, id string, err error) error {
	return fmt.Errorf("%s scheduled task %s: %w: %w", verb, id, spi.ErrTaskBusy, err)
}

// joinTenant refuses a joining write whose tenant is not the tenant of the
// transaction on ctx, before any statement runs: a task row of tenant B never
// enters tenant A's transaction. Memory and SQLite refuse the same way.
// Without a transaction on ctx there is nothing to compare.
func joinTenant(ctx context.Context, tenant spi.TenantID) error {
	if tx := spi.GetTransaction(ctx); tx != nil && tx.TenantID != tenant {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
	}
	return nil
}

// armTaskSQL arms one task as a new life: a new arm token, WAITING, due at its
// scheduled time, every counter and record cleared, no claim. A task is keyed
// by (tenant_id, id), so the same id in another tenant is another task.
const armTaskSQL = `INSERT INTO scheduled_tasks (
	id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name, model_version,
	transition, source_state, armed_at, armed_by_id, armed_by_kind,
	arm_token, status, next_attempt_time)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, gen_random_uuid(), 'WAITING', $4)
ON CONFLICT (tenant_id, id) DO UPDATE SET
	type = excluded.type, scheduled_time = excluded.scheduled_time, timeout_ms = excluded.timeout_ms,
	entity_id = excluded.entity_id, model_name = excluded.model_name,
	model_version = excluded.model_version, transition = excluded.transition,
	source_state = excluded.source_state, armed_at = excluded.armed_at,
	armed_by_id = excluded.armed_by_id, armed_by_kind = excluded.armed_by_kind,
	arm_token = excluded.arm_token, status = 'WAITING', next_attempt_time = excluded.next_attempt_time,
	attempts = 0, lost_owners = 0, last_attempt_time = NULL, last_error = '', failure_reason = '',
	failed_time = NULL, partial_commit = false, claim_token = NULL, claim_owner = NULL`

// ReconcileForEntity arms req.Arm, each as a new life, removes the entity's
// tasks named in req.Cancel, then removes every other task of the entity and
// returns those, sorted by id byte-wise, each with its life's mark. The tenant
// and entity come from req, never from the task structs. Without a
// transaction on ctx it runs in one of its own, so it is all or nothing.
func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	if err := spi.ValidateArm(req); err != nil {
		return nil, err
	}
	if err := joinTenant(ctx, req.TenantID); err != nil {
		return nil, err
	}
	var removed []spi.ScheduledTask
	err := s.atomically(ctx, pgx.ReadCommitted, func(q Querier) error {
		var err error
		removed, err = reconcile(ctx, q, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

func reconcile(ctx context.Context, q Querier, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	armIDs := make([]string, 0, len(req.Arm))
	for _, t := range req.Arm {
		if _, err := q.Exec(ctx, armTaskSQL,
			t.ID, string(req.TenantID), string(t.Type), t.ScheduledTime, t.TimeoutMs, req.EntityID,
			t.ModelName, t.ModelVersion, t.Transition, t.SourceState, t.ArmedAt,
			t.ArmedBy.ID, string(t.ArmedBy.Kind)); err != nil {
			return nil, fmt.Errorf("failed to arm scheduled task %s: %w", t.ID, err)
		}
		armIDs = append(armIDs, t.ID)
	}
	if len(req.Cancel) > 0 {
		if _, err := q.Exec(ctx,
			`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND entity_id = $2 AND id = ANY($3::text[])`,
			string(req.TenantID), req.EntityID, req.Cancel); err != nil {
			return nil, fmt.Errorf("failed to cancel scheduled tasks of %s: %w", req.EntityID, err)
		}
	}
	// markedColumn reads the marks as they were before this statement: the
	// statement removes task rows, never marks.
	rows, err := q.Query(ctx, `DELETE FROM scheduled_tasks st
		WHERE st.tenant_id = $1 AND st.entity_id = $2 AND NOT (st.id = ANY($3::text[]))
		RETURNING `+taskColumns+`, `+markedColumn, string(req.TenantID), req.EntityID, armIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to remove scheduled tasks of %s: %w", req.EntityID, err)
	}
	defer rows.Close()
	var removed []spi.ScheduledTask
	for rows.Next() {
		var marked bool
		t, err := scanTask(rows.Scan, &marked)
		if err != nil {
			return nil, fmt.Errorf("failed to remove scheduled tasks of %s: %w", req.EntityID, err)
		}
		t.UnsafeMarked = marked
		removed = append(removed, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to remove scheduled tasks of %s: %w", req.EntityID, err)
	}
	slices.SortFunc(removed, func(a, b spi.ScheduledTask) int { return strings.Compare(a.ID, b.ID) })
	return removed, nil
}

// atomically runs fn in the transaction on ctx, through q, when there is one.
// Without one it runs fn in a private transaction on the main pool at iso and
// commits it, so a method of several statements is all or nothing either way.
func (s *scheduledTaskStore) atomically(ctx context.Context, iso pgx.TxIsoLevel, fn func(q Querier) error) error {
	if spi.GetTransaction(ctx) != nil {
		return fn(s.q)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		return fmt.Errorf("failed to begin a scheduled task write: %w", classifyError(err))
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(classifiedQuerier{inner: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit a scheduled task write: %w", classifyError(err))
	}
	return nil
}

// RemoveLife removes the task if its life is armToken. At REPEATABLE READ a
// row whose snapshot version names armToken but that another transaction
// changed after the snapshot raises 40001 (C1). A call that matches no row
// takes no lock, so it does not make the row busy (C6).
func (s *scheduledTaskStore) RemoveLife(ctx context.Context, tenant spi.TenantID, id string, armToken uuid.UUID) error {
	if err := joinTenant(ctx, tenant); err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx,
		`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND id = $2 AND arm_token = $3`,
		string(tenant), id, armToken); err != nil {
		return fmt.Errorf("failed to remove scheduled task %s: %w", id, err)
	}
	return nil
}

// StampSegment always writes the row, partial or not: the write is what puts
// the segment's commit under first-committer-wins on this row (C1).
func (s *scheduledTaskStore) StampSegment(ctx context.Context, ref spi.TaskRef, partial bool) error {
	if err := joinTenant(ctx, ref.TenantID); err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, `UPDATE scheduled_tasks SET partial_commit = partial_commit OR $5
		WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'`,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken, partial)
	if err != nil {
		return fmt.Errorf("failed to stamp scheduled task %s: %w", ref.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return staleClaim("stamp", ref.ID)
	}
	return nil
}

func (s *scheduledTaskStore) DeleteForEntities(ctx context.Context, tenant spi.TenantID, entityIDs []string) error {
	if err := joinTenant(ctx, tenant); err != nil {
		return err
	}
	if len(entityIDs) == 0 {
		return nil
	}
	if _, err := s.q.Exec(ctx,
		`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`,
		string(tenant), entityIDs); err != nil {
		return fmt.Errorf("failed to remove scheduled tasks of %d entities: %w", len(entityIDs), err)
	}
	return nil
}

// DeleteForModel removes the model's tasks whose (source state, transition)
// keep does not retain. A nil keep retains nothing, and the removal is one
// statement. Otherwise the pairs are read, keep runs after the read has
// finished and before the delete starts, so no statement is open on the
// connection while it runs, and the delete removes the pairs keep dropped.
// Without a transaction on ctx the read and the delete run in a private
// REPEATABLE READ transaction, so the delete sees the rows the read saw.
func (s *scheduledTaskStore) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int,
	keep func(sourceState, transition string) bool) error {
	if err := joinTenant(ctx, tenant); err != nil {
		return err
	}
	if keep == nil {
		if _, err := s.q.Exec(ctx,
			`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
			string(tenant), modelName, modelVersion); err != nil {
			return fmt.Errorf("failed to remove scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
		}
		return nil
	}
	return s.atomically(ctx, pgx.RepeatableRead, func(q Querier) error {
		pairs, err := modelPairs(ctx, q, tenant, modelName, modelVersion)
		if err != nil {
			return err
		}
		var states, transitions []string
		for _, p := range pairs {
			if keep(p[0], p[1]) {
				continue
			}
			states = append(states, p[0])
			transitions = append(transitions, p[1])
		}
		if len(states) == 0 {
			return nil
		}
		if _, err := q.Exec(ctx, `DELETE FROM scheduled_tasks st
			USING unnest($4::text[], $5::text[]) AS gone(source_state, transition)
			WHERE st.tenant_id = $1 AND st.model_name = $2 AND st.model_version = $3
			  AND st.source_state = gone.source_state AND st.transition = gone.transition`,
			string(tenant), modelName, modelVersion, states, transitions); err != nil {
			return fmt.Errorf("failed to remove scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
		}
		return nil
	})
}

// modelPairs returns the distinct (source state, transition) pairs of the
// model version's tasks.
func modelPairs(ctx context.Context, q Querier, tenant spi.TenantID, modelName string, modelVersion int) ([][2]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT source_state, transition FROM scheduled_tasks
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(tenant), modelName, modelVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to read scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
	}
	defer rows.Close()
	var pairs [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, fmt.Errorf("failed to read scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
	}
	return pairs, nil
}

// Get joins the transaction on ctx only when that transaction is open and its
// tenant is tenant; then it sees the transaction's staged writes (C2).
// Otherwise — no transaction, another tenant's, or one that has committed or
// rolled back — it reads the committed row off any transaction. It answers
// from tenant and never refuses.
func (s *scheduledTaskStore) Get(ctx context.Context, tenant spi.TenantID, id string) (*spi.ScheduledTask, bool, error) {
	q := s.query
	if tx := spi.GetTransaction(ctx); tx != nil && tx.TenantID == tenant && s.txOpen(tx.ID) {
		q = s.q
	}
	var marked bool
	t, err := scanTask(q.QueryRow(ctx, `SELECT `+taskColumns+`, `+markedColumn+`
		FROM scheduled_tasks st WHERE st.tenant_id = $1 AND st.id = $2`, string(tenant), id).Scan, &marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to get scheduled task %s: %w", id, err)
	}
	t.UnsafeMarked = marked
	return &t, true, nil
}

// Query pages the tenant's tasks in (scheduled_time, id) order, ids compared
// byte-wise. It reads one row past the page to know whether another follows.
func (s *scheduledTaskStore) Query(ctx context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	if err := spi.ValidateScheduledTaskQuery(q); err != nil {
		return spi.ScheduledTaskPage{}, err
	}
	statuses := make([]string, 0, len(q.Statuses))
	for _, st := range q.Statuses {
		statuses = append(statuses, string(st))
	}
	var afterTime *int64
	afterID := ""
	if q.After != nil {
		afterTime, afterID = &q.After.ScheduledTime, q.After.ID
	}
	rows, err := s.query.Query(ctx, `SELECT `+taskColumns+`, `+markedColumn+`
		  FROM scheduled_tasks st
		 WHERE st.tenant_id = $1
		   AND (cardinality($2::text[]) = 0 OR st.status = ANY($2::text[]))
		   AND ($3::text = '' OR st.model_name = $3)
		   AND ($4::int = 0 OR st.model_version = $4)
		   AND ($5::text = '' OR st.entity_id = $5)
		   AND ($6::bigint IS NULL OR (st.scheduled_time, st.id COLLATE "C") > ($6, $7::text COLLATE "C"))
		 ORDER BY st.scheduled_time, st.id COLLATE "C"
		 LIMIT $8`,
		string(tenant), statuses, q.ModelName, q.ModelVersion, q.EntityID, afterTime, afterID, q.Limit+1)
	if err != nil {
		return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	defer rows.Close()
	items := make([]spi.ScheduledTask, 0, q.Limit)
	for rows.Next() {
		var marked bool
		t, err := scanTask(rows.Scan, &marked)
		if err != nil {
			return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
		}
		t.UnsafeMarked = marked
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	page := spi.ScheduledTaskPage{Items: items}
	if len(items) > q.Limit {
		page.Items = items[:q.Limit]
		last := page.Items[q.Limit-1]
		page.Next = &spi.ScheduledTaskCursor{ScheduledTime: last.ScheduledTime, ID: last.ID}
	}
	return page, nil
}

// ownerLostCondition holds when the owner of the RUNNING row st has no
// liveness record, or one older than StaleAfter ($3, in microseconds) by the
// database clock.
const ownerLostCondition = `NOT EXISTS (SELECT 1 FROM scheduler_owners o
	                     WHERE o.owner = st.claim_owner
	                       AND o.heartbeat_at >= now() - ($3::bigint * interval '1 microsecond'))`

// claimableCondition is the row's own claim condition, on alias st, with
// $1 = NowMs, $2 = AllowLostOwner, $3 = StaleAfter in microseconds. A RUNNING
// row qualifies, whatever its next_attempt_time, when its owner is lost
// (ownerLostCondition).
const claimableCondition = `(
	(st.status = 'WAITING' AND st.next_attempt_time <= $1)
	OR ($2::boolean AND st.status = 'RUNNING' AND ` + ownerLostCondition + `))`

// rankClaimsSQL is ClaimDue's ranking. It returns the tasks one claim takes,
// in the order spi.SelectClaims defines:
//
//   - candidates: claimableCondition, less any task whose entity has another
//     RUNNING task, less the (tenant, id) pairs in $8/$9 — rows an earlier
//     round of this claim found busy or no longer claimable — and less every
//     task of the (tenant, entity) pairs in $10/$11 — entities another
//     claimer holds;
//   - one task per entity: the first in (next_attempt_time, id) order;
//   - within a tenant, turn = the task's place in (next_attempt_time, id)
//     order; a tenant's turns stop at its quota, PerTenantLimit minus its
//     runs in progress ($4, $5, $6);
//   - tenants take turns, one task per turn: turn first, then the tenant with
//     the earliest candidate, ties broken by tenant id;
//   - at most Limit ($7) tasks.
//
// Bounded work. The ranking reads, per tenant, only the tasks that can hold
// one of its first n turns, n = min(quota, Limit); it never reads a tenant's
// whole due backlog. The result is the same as ranking every candidate:
//
//   - tenant enumerates the tenants that hold a WAITING task, one index probe
//     each (a loose scan of scheduled_tasks_waiting_due_idx), and, with
//     AllowLostOwner, those that hold a RUNNING task;
//   - per tenant, the WAITING branch walks the tenant's due WAITING tasks in
//     (next_attempt_time, id) order and keeps a task only when it is its
//     entity's candidate: no RUNNING task on the entity, neither it nor its
//     entity excluded, and no earlier WAITING task of the entity that is not
//     itself excluded. Each kept task is a different entity's first
//     candidate, met in turn order, so the first n kept are exactly the
//     tenant's turns 1..n. The cut is taken after the one-per-entity
//     collapse, never before it: a cut of n rows before the collapse could
//     spend turns on one entity's later tasks and leave the quota unfilled;
//   - the lost-owner branch takes the tenant's first n claimable RUNNING
//     tasks. An entity with a RUNNING task has no WAITING candidate, and
//     scheduled_tasks_one_running_per_entity_uq allows one RUNNING task per
//     entity, so the two branches name different entities and their union is
//     one task per entity; turns 1..n of the union lie within the first n of
//     each branch;
//   - tenant_first is each tenant's turn 1, which the cut keeps; a turn above
//     Limit never survives the final LIMIT, since the tenant's turns 1..Limit
//     precede it.
//
// So a claim reads, per tenant with a WAITING task, one probe plus the rows
// the walk passes before its n-th kept task: the n kept, the tasks of
// entities with a RUNNING task or excluded, the excluded rows, and the later
// tasks of entities already met — each checked by a probe of
// scheduled_tasks_waiting_entity_idx. None of these grows with the tenant's
// backlog: RUNNING tasks are bounded by the runs in progress, exclusions by
// this claim's rounds, and an entity's tasks by its state's scheduled
// transitions.
//
// Ids and tenant ids compare byte-wise (COLLATE "C"), as Go compares strings.
// The loose scan orders tenant ids by the database collation; it only lists
// them.
const rankClaimsSQL = `WITH RECURSIVE waiting_tenant AS (
	(SELECT st.tenant_id FROM scheduled_tasks st
	  WHERE st.status = 'WAITING'
	  ORDER BY st.tenant_id, st.next_attempt_time LIMIT 1)
	UNION ALL
	SELECT (SELECT st.tenant_id FROM scheduled_tasks st
	         WHERE st.status = 'WAITING' AND st.tenant_id > w.tenant_id
	         ORDER BY st.tenant_id, st.next_attempt_time LIMIT 1)
	  FROM waiting_tenant w
	 WHERE w.tenant_id IS NOT NULL
), tenant AS (
	SELECT tenant_id FROM waiting_tenant WHERE tenant_id IS NOT NULL
	UNION
	SELECT st.tenant_id FROM scheduled_tasks st WHERE $2::boolean AND st.status = 'RUNNING'
), quota AS (
	SELECT t.tenant_id, LEAST($4::int - COALESCE(busy.runs, 0), $7::int) AS n
	  FROM tenant t
	  LEFT JOIN unnest($5::text[], $6::int[]) AS busy(tenant_id, runs) ON busy.tenant_id = t.tenant_id
), candidate AS (
	SELECT c.id, c.tenant_id, c.entity_id, c.next_attempt_time
	  FROM quota q
	 CROSS JOIN LATERAL (
		(SELECT st.id, st.tenant_id, st.entity_id, st.next_attempt_time
		   FROM scheduled_tasks st
		  WHERE st.tenant_id = q.tenant_id
		    AND st.status = 'WAITING' AND st.next_attempt_time <= $1
		    AND NOT EXISTS (SELECT 1 FROM scheduled_tasks r
		                     WHERE r.tenant_id = st.tenant_id AND r.entity_id = st.entity_id
		                       AND r.status = 'RUNNING')
		    AND NOT EXISTS (SELECT 1 FROM unnest($8::text[], $9::text[]) AS x(tenant_id, id)
		                     WHERE x.tenant_id = st.tenant_id AND x.id = st.id)
		    AND NOT EXISTS (SELECT 1 FROM unnest($10::text[], $11::text[]) AS x(tenant_id, entity_id)
		                     WHERE x.tenant_id = st.tenant_id AND x.entity_id = st.entity_id)
		    AND NOT EXISTS (SELECT 1 FROM scheduled_tasks e
		                     WHERE e.tenant_id = st.tenant_id AND e.entity_id = st.entity_id
		                       AND e.status = 'WAITING'
		                       AND (e.next_attempt_time, e.id COLLATE "C") < (st.next_attempt_time, st.id COLLATE "C")
		                       AND NOT EXISTS (SELECT 1 FROM unnest($8::text[], $9::text[]) AS x(tenant_id, id)
		                                        WHERE x.tenant_id = e.tenant_id AND x.id = e.id))
		  ORDER BY st.next_attempt_time, st.id COLLATE "C"
		  LIMIT LEAST($4::int, $7::int))
		UNION ALL
		(SELECT st.id, st.tenant_id, st.entity_id, st.next_attempt_time
		   FROM scheduled_tasks st
		  WHERE $2::boolean AND st.tenant_id = q.tenant_id AND st.status = 'RUNNING'
		    AND ` + ownerLostCondition + `
		    AND NOT EXISTS (SELECT 1 FROM unnest($8::text[], $9::text[]) AS x(tenant_id, id)
		                     WHERE x.tenant_id = st.tenant_id AND x.id = st.id)
		    AND NOT EXISTS (SELECT 1 FROM unnest($10::text[], $11::text[]) AS x(tenant_id, entity_id)
		                     WHERE x.tenant_id = st.tenant_id AND x.entity_id = st.entity_id)
		  ORDER BY st.next_attempt_time, st.id COLLATE "C"
		  LIMIT LEAST($4::int, $7::int))
	 ) c
	 WHERE q.n > 0
), ranked AS (
	SELECT id, tenant_id, entity_id,
	       row_number() OVER (PARTITION BY tenant_id ORDER BY next_attempt_time, id COLLATE "C") AS turn,
	       min(next_attempt_time) OVER (PARTITION BY tenant_id) AS tenant_first
	  FROM candidate
)
SELECT r.tenant_id, r.id, r.entity_id
  FROM ranked r
  JOIN quota q ON q.tenant_id = r.tenant_id
 WHERE r.turn <= q.n
 ORDER BY r.turn, r.tenant_first, r.tenant_id COLLATE "C"
 LIMIT $7`

// lockClaimsSQL locks ranked rows, skipping any row another transaction holds
// (C6), and repeats the claim condition against each row's latest version.
// It returns each locked row's status before the claim: RUNNING means the
// claim takes the task from a stale or missing owner. The row lock holds that
// status until claimSQL runs.
const lockClaimsSQL = `SELECT st.tenant_id, st.id, st.entity_id, st.status
  FROM scheduled_tasks st
  JOIN unnest($4::text[], $5::text[]) AS c(tenant_id, id) ON c.tenant_id = st.tenant_id AND c.id = st.id
 WHERE ` + claimableCondition + `
 ORDER BY st.tenant_id, st.id
 FOR UPDATE OF st SKIP LOCKED`

// entityLockKey is the 64-bit advisory-lock key of an entity, from SQL
// expressions for its tenant id and entity id. The tenant id is length-prefixed
// so that no two (tenant, entity) pairs concatenate to the same text. Two
// (tenant, entity) pairs, of one tenant or of two, can still share a key
// through a 64-bit hash collision. The only effect is that one claim round
// skips that entity and a later call takes it: the lock is tried without
// waiting, so no one blocks, and the key carries no data, so nothing is
// exposed.
func entityLockKey(tenantExpr, entityExpr string) string {
	return `hashtextextended(length(` + tenantExpr + `)::text || ':' || ` + tenantExpr + ` || ` + entityExpr + `, 0)`
}

// lockEntitiesSQL takes, without waiting, the advisory lock of each (tenant,
// entity) in $1/$2 for the rest of the claim's transaction, and returns the
// ones another claimer holds. Only claims take these locks: claimers serialise
// per entity, so a claimer whose ranking chose a task can only reach claimSQL
// on it once any rival claimer of the same entity has committed or rolled
// back — the rival holds the entity's lock until its transaction ends, and
// PostgreSQL makes that transaction's commit visible before it releases the
// lock. Entity transactions take none, so a row busy under an entity
// transaction still passes its turn to a sibling on the same entity.
var lockEntitiesSQL = `SELECT k.tenant_id, k.entity_id
  FROM unnest($1::text[], $2::text[]) AS k(tenant_id, entity_id)
 WHERE NOT pg_try_advisory_xact_lock(` + entityLockKey("k.tenant_id", "k.entity_id") + `)`

// claimSQL is ClaimDue's claim. Every row it names is locked by this
// transaction, so the row's own columns are as lockClaimsSQL saw them.
// Claimers serialise per entity on the advisory lock lockEntitiesSQL takes, so
// no rival claimer can be mid-claim on the same entity here. It is a new
// statement, so it sees what other transactions committed since the ranking:
//   - a sibling claim committed after the ranking: the NOT EXISTS passes over
//     that entity, whose sibling this claim's ranking chose before the rival's
//     commit made it RUNNING;
//   - a heartbeat of a RUNNING row's owner: that owner is live again and keeps
//     its task.
//
// lost_owners counts the claim only when it takes a RUNNING row.
const claimSQL = `UPDATE scheduled_tasks st
   SET status      = 'RUNNING',
       claim_token = gen_random_uuid(),
       claim_owner = $5,
       lost_owners = st.lost_owners + CASE WHEN st.status = 'RUNNING' THEN 1 ELSE 0 END
  FROM unnest($6::text[], $4::text[]) AS c(tenant_id, id)
 WHERE st.tenant_id = c.tenant_id AND st.id = c.id
   AND ` + claimableCondition + `
   AND NOT EXISTS (SELECT 1 FROM scheduled_tasks r
                    WHERE r.tenant_id = st.tenant_id AND r.entity_id = st.entity_id
                      AND r.status = 'RUNNING' AND r.id <> st.id)
RETURNING ` + taskColumns

// claimedMarksSQL reads the claimed lives' marks while the row locks are held
// (C3).
const claimedMarksSQL = `SELECT m.tenant_id, m.task_id
  FROM scheduled_task_marks m
  JOIN unnest($1::text[], $2::text[], $3::uuid[]) AS c(tenant_id, task_id, arm_token)
    ON m.tenant_id = c.tenant_id AND m.task_id = c.task_id AND m.arm_token = c.arm_token`

// ClaimDue claims due tasks in one READ COMMITTED transaction on the scheduler
// pool and returns them in spi.SelectClaims order. Claimers serialise per
// entity on the advisory lock lockEntitiesSQL takes, so a claim never meets a
// rival claimer's write to a sibling task; any failure is returned
// (claimError).
func (s *scheduledTaskStore) ClaimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if err := spi.ValidateClaimRequest(req); err != nil {
		return nil, err
	}
	claimed, err := s.claimDue(ctx, req)
	if err != nil {
		switch {
		case isLockNotAvailable(err) || isDeadlock(err):
			slog.Warn("scheduled task claim met a lock wait or deadlock",
				"pkg", "postgres", "err", err)
		case isRunningIndexViolation(err):
			slog.Error("scheduled task claim violated the one-RUNNING-task-per-entity invariant",
				"pkg", "postgres", "err", err)
		}
		return nil, fmt.Errorf("failed to claim scheduled tasks: %w", claimError(err))
	}
	return claimed, nil
}

// claimDue ranks, locks, and ranks again until every ranked row is locked.
//
// Each round locks the newly ranked rows (SKIP LOCKED), then takes the
// advisory lock of each newly locked row's entity (see lockEntitiesSQL). A
// ranked row the row lock skipped — busy under an open transaction, or no
// longer claimable — is excluded, so its turn goes to the next task, a sibling
// on the same entity included. An entity whose advisory lock another claimer
// holds is excluded whole, so the claim never waits behind that claimer on the
// one-RUNNING index. The result is spi.SelectClaims over the claimable rows no
// other transaction holds, on the entities no other claimer holds. Row and
// entity locks taken in earlier rounds stay held. Every round but the last
// excludes at least one row or entity, so the loop ends.
func (s *scheduledTaskStore) claimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	tenants := make([]string, 0, len(req.TenantInProgress))
	running := make([]int, 0, len(req.TenantInProgress))
	for tenant, n := range req.TenantInProgress {
		tenants = append(tenants, string(tenant))
		running = append(running, n)
	}
	stale := req.StaleAfter.Microseconds()

	tx, err := s.sched.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := classifiedQuerier{inner: tx}

	r := claimRound{
		fromLostOwner: make(map[taskKey]bool),
		heldEntities:  make(map[taskKey]bool),
		excl:          exclusions{rowTenants: []string{}, rowIDs: []string{}, entTenants: []string{}, entIDs: []string{}},
	}
	var chosen []rankedTask
	for {
		chosen, err = rankClaims(ctx, q, req, stale, tenants, running, r.excl)
		if err != nil {
			return nil, err
		}
		excluded, err := r.lock(ctx, q, req, stale, chosen)
		if err != nil {
			return nil, err
		}
		if !excluded {
			break
		}
	}
	if len(chosen) == 0 {
		return nil, nil
	}

	chosenTenants := make([]string, len(chosen))
	chosenIDs := make([]string, len(chosen))
	for i, c := range chosen {
		chosenTenants[i], chosenIDs[i] = c.tenant, c.id
	}
	rows, err := q.Query(ctx, claimSQL, req.NowMs, req.AllowLostOwner, stale, chosenIDs, req.Owner, chosenTenants)
	if err != nil {
		return nil, err
	}
	updated, err := scanTasks(rows)
	if err != nil {
		return nil, err
	}
	byKey := make(map[taskKey]spi.ScheduledTask, len(updated))
	for _, t := range updated {
		// Set on the returned copy only; the row never stores it.
		t.ClaimedFromLostOwner = r.fromLostOwner[keyOf(t)]
		byKey[keyOf(t)] = t
	}
	claimed := make([]spi.ScheduledTask, 0, len(updated))
	for _, c := range chosen {
		if t, ok := byKey[c.key()]; ok {
			claimed = append(claimed, t)
		}
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	if err := markClaimed(ctx, q, claimed); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, classifyError(err)
	}
	return claimed, nil
}

// rankedTask is one task rankClaimsSQL chose.
type rankedTask struct{ tenant, id, entity string }

func (c rankedTask) key() taskKey { return taskKey{tenant: c.tenant, id: c.id} }

// exclusions are the rows and entities later rankings of one claim leave out.
type exclusions struct{ rowTenants, rowIDs, entTenants, entIDs []string }

// claimRound is what one claim holds across its ranking rounds.
type claimRound struct {
	fromLostOwner map[taskKey]bool // every row locked so far; true when it was RUNNING
	heldEntities  map[taskKey]bool // every entity whose advisory lock this claim holds, keyed (tenant, entity)
	excl          exclusions
}

// lock locks the chosen rows not yet locked and their entities not yet held,
// and reports whether it excluded anything, which means another ranking round
// is needed.
func (r *claimRound) lock(ctx context.Context, q Querier, req spi.ClaimRequest, stale int64, chosen []rankedTask) (bool, error) {
	var lockTenants, lockIDs []string
	for _, c := range chosen {
		if _, ok := r.fromLostOwner[c.key()]; !ok {
			lockTenants, lockIDs = append(lockTenants, c.tenant), append(lockIDs, c.id)
		}
	}
	if len(lockIDs) == 0 {
		return false, nil
	}
	lockedEntities, err := lockClaims(ctx, q, req, stale, lockTenants, lockIDs, r.fromLostOwner)
	if err != nil {
		return false, err
	}
	excluded := false
	for i, id := range lockIDs {
		if _, ok := r.fromLostOwner[taskKey{tenant: lockTenants[i], id: id}]; !ok {
			r.excl.rowTenants, r.excl.rowIDs = append(r.excl.rowTenants, lockTenants[i]), append(r.excl.rowIDs, id)
			excluded = true
		}
	}
	var entTenants, entIDs []string
	for _, e := range lockedEntities {
		if !r.heldEntities[e] {
			entTenants, entIDs = append(entTenants, e.tenant), append(entIDs, e.id)
		}
	}
	if len(entIDs) == 0 {
		return excluded, nil
	}
	taken, err := lockEntities(ctx, q, entTenants, entIDs)
	if err != nil {
		return false, err
	}
	for i, id := range entIDs {
		e := taskKey{tenant: entTenants[i], id: id}
		if taken[e] {
			r.heldEntities[e] = true
			continue
		}
		r.excl.entTenants, r.excl.entIDs = append(r.excl.entTenants, e.tenant), append(r.excl.entIDs, e.id)
		excluded = true
	}
	return excluded, nil
}

func rankClaims(ctx context.Context, q Querier, req spi.ClaimRequest, stale int64,
	tenants []string, running []int, excl exclusions) ([]rankedTask, error) {
	rows, err := q.Query(ctx, rankClaimsSQL, req.NowMs, req.AllowLostOwner, stale, req.PerTenantLimit,
		tenants, running, req.Limit, excl.rowTenants, excl.rowIDs, excl.entTenants, excl.entIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chosen []rankedTask
	for rows.Next() {
		var c rankedTask
		if err := rows.Scan(&c.tenant, &c.id, &c.entity); err != nil {
			return nil, err
		}
		chosen = append(chosen, c)
	}
	return chosen, rows.Err()
}

// lockClaims locks the rows it can, records each one's pre-claim status in
// locked (true: RUNNING, taken from a lost owner), and returns the distinct
// (tenant, entity) pairs of the rows it locked.
func lockClaims(ctx context.Context, q Querier, req spi.ClaimRequest, stale int64,
	tenants, ids []string, locked map[taskKey]bool) ([]taskKey, error) {
	rows, err := q.Query(ctx, lockClaimsSQL, req.NowMs, req.AllowLostOwner, stale, tenants, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[taskKey]bool)
	var entities []taskKey
	for rows.Next() {
		var k taskKey
		var entity, status string
		if err := rows.Scan(&k.tenant, &k.id, &entity, &status); err != nil {
			return nil, err
		}
		locked[k] = status == string(spi.ScheduledTaskRunning)
		if e := (taskKey{tenant: k.tenant, id: entity}); !seen[e] {
			seen[e] = true
			entities = append(entities, e)
		}
	}
	return entities, rows.Err()
}

// lockEntities tries the advisory lock of each (tenant, entity) and reports
// which it took.
func lockEntities(ctx context.Context, q Querier, tenants, entities []string) (map[taskKey]bool, error) {
	taken := make(map[taskKey]bool, len(entities))
	for i, e := range entities {
		taken[taskKey{tenant: tenants[i], id: e}] = true
	}
	rows, err := q.Query(ctx, lockEntitiesSQL, tenants, entities)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e taskKey
		if err := rows.Scan(&e.tenant, &e.id); err != nil {
			return nil, err
		}
		taken[e] = false
	}
	return taken, rows.Err()
}

// markClaimed sets UnsafeMarked on each claimed task whose life has a mark.
func markClaimed(ctx context.Context, q Querier, claimed []spi.ScheduledTask) error {
	tenantIDs := make([]string, len(claimed))
	ids := make([]string, len(claimed))
	arms := make([]uuid.UUID, len(claimed))
	at := make(map[taskKey]int, len(claimed))
	for i, t := range claimed {
		tenantIDs[i], ids[i], arms[i], at[keyOf(t)] = string(t.TenantID), t.ID, t.ArmToken, i
	}
	rows, err := q.Query(ctx, claimedMarksSQL, tenantIDs, ids, arms)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k taskKey
		if err := rows.Scan(&k.tenant, &k.id); err != nil {
			return err
		}
		claimed[at[k]].UnsafeMarked = true
	}
	return rows.Err()
}

// claimError classifies a claim failure the way the store does elsewhere: a
// lock wait is ErrTaskBusy, a deadlock ErrConflict. The SQLSTATE stays in the
// chain.
func claimError(err error) error {
	switch {
	case isLockNotAvailable(err):
		return fmt.Errorf("%w: %w", spi.ErrTaskBusy, err)
	case isDeadlock(err) && !errors.Is(err, spi.ErrConflict):
		return fmt.Errorf("%w: %w", spi.ErrConflict, err)
	}
	return err
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.DeadlockDetected
}

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable
}

// oneRunningPerEntityIndex is the partial unique index that enforces at most
// one RUNNING task per entity — see lockEntitiesSQL for why a claim is never
// meant to meet it.
const oneRunningPerEntityIndex = "scheduled_tasks_one_running_per_entity_uq"

// isRunningIndexViolation reports whether err is a unique violation of
// oneRunningPerEntityIndex: two tasks of the same entity RUNNING at once,
// which the per-entity advisory lock exists to make impossible.
func isRunningIndexViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == oneRunningPerEntityIndex
}

// MarkUnsafe records, before an unsafe dispatch, that this claim of this life
// is about to hand work off. One READ COMMITTED transaction on the scheduler
// pool, never the caller's, so the mark survives the run's rollback.
//
// Step 1 share-locks the task row without waiting: no row means the claim is
// stale; 55P03 means another transaction holds the row (C6), or a claim is in
// progress on it. Either way the mark and a claim serialise (C3): a claim that
// committed first changed the tokens; a claim in progress holds the row; a
// mark that holds its share lock first makes ClaimDue skip the row, and the
// next claim sees the mark.
func (s *scheduledTaskStore) MarkUnsafe(ctx context.Context, ref spi.TaskRef) error {
	tx, err := s.sched.begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := classifiedQuerier{inner: tx}

	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM scheduled_tasks
		WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'
		FOR SHARE NOWAIT`, string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken).Scan(&one)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return staleClaim("mark", ref.ID)
	case isLockNotAvailable(err):
		return taskBusy("mark", ref.ID, err)
	case err != nil:
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}

	var holder uuid.UUID
	err = q.QueryRow(ctx, `INSERT INTO scheduled_task_marks (tenant_id, task_id, arm_token, claim_token)
		VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id, task_id, arm_token) DO NOTHING RETURNING claim_token`,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `SELECT claim_token FROM scheduled_task_marks
			WHERE tenant_id = $1 AND task_id = $2 AND arm_token = $3`,
			string(ref.TenantID), ref.ID, ref.ArmToken).Scan(&holder)
	}
	if err != nil {
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}
	if holder != ref.ClaimToken {
		return fmt.Errorf("mark scheduled task %s: %w", ref.ID, spi.ErrMarkedByAnotherClaim)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit the mark of scheduled task %s: %w", ref.ID, classifyError(err))
	}
	return nil
}

// recordAttemptSQL sets the task WAITING, counts the attempt unless $5 = 0,
// records the error, clears the claim, and — with $9 — removes this claim's
// mark, all in one statement.
const recordAttemptSQL = `WITH attempt AS (
	UPDATE scheduled_tasks
	   SET status = 'WAITING', attempts = attempts + $5, last_error = $6,
	       last_attempt_time = $7, next_attempt_time = $8,
	       claim_token = NULL, claim_owner = NULL
	 WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'
	RETURNING tenant_id, id, arm_token
), cleared AS (
	DELETE FROM scheduled_task_marks m
	 USING attempt a
	 WHERE $9::boolean AND m.tenant_id = a.tenant_id AND m.task_id = a.id AND m.arm_token = a.arm_token
	   AND m.claim_token = $4
)
SELECT count(*) FROM attempt`

// RecordAttempt ends ref's claim with the attempt recorded. A row an open
// transaction wrote makes the statement wait; at lock_timeout it gives up with
// 55P03, answered as ErrTaskBusy, and nothing is written (C6).
func (s *scheduledTaskStore) RecordAttempt(ctx context.Context, ref spi.TaskRef, a spi.Attempt) error {
	if err := spi.ValidateTaskErrorText(a.Error); err != nil {
		return fmt.Errorf("record an attempt of scheduled task %s: %w", ref.ID, err)
	}
	counted := 1
	if a.NotCounted {
		counted = 0
	}
	var n int
	err := s.sched.QueryRow(ctx, recordAttemptSQL,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken,
		counted, a.Error, a.AtMs, a.NextAttemptTime, a.ClearOwnMark).Scan(&n)
	switch {
	case isLockNotAvailable(err):
		return taskBusy("record an attempt of", ref.ID, err)
	case err != nil:
		return fmt.Errorf("failed to record an attempt of scheduled task %s: %w", ref.ID, err)
	case n == 0:
		return staleClaim("record an attempt of", ref.ID)
	}
	return nil
}

// Fail joins the transaction on ctx, so the FAILED status and its audit event
// commit together. The mark, if any, stays with the life.
func (s *scheduledTaskStore) Fail(ctx context.Context, ref spi.TaskRef, f spi.Failure) error {
	if err := spi.ValidateFailureReason(f.Reason); err != nil {
		return fmt.Errorf("fail scheduled task %s: %w", ref.ID, err)
	}
	if err := spi.ValidateTaskErrorText(f.Error); err != nil {
		return fmt.Errorf("fail scheduled task %s: %w", ref.ID, err)
	}
	if err := joinTenant(ctx, ref.TenantID); err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, `UPDATE scheduled_tasks
		   SET status = 'FAILED', failure_reason = $5, last_error = $6, failed_time = $7,
		       claim_token = NULL, claim_owner = NULL
		 WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'`,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken, string(f.Reason), f.Error, f.AtMs)
	if err != nil {
		return fmt.Errorf("failed to fail scheduled task %s: %w", ref.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return staleClaim("fail", ref.ID)
	}
	return nil
}

// GiveBackIdle returns to WAITING, uncounted, every task owner holds RUNNING
// whose claim is not in keep. A row an open transaction holds is skipped, not
// waited for, and not counted (C6). Zero rows is its normal result.
func (s *scheduledTaskStore) GiveBackIdle(ctx context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error) {
	live := append(make([]uuid.UUID, 0, len(keep)), keep...) // never NULL: <> ALL(NULL) matches nothing
	tag, err := s.sched.Exec(ctx, `UPDATE scheduled_tasks st
		   SET status = 'WAITING', claim_token = NULL, claim_owner = NULL
		  FROM (SELECT tenant_id, id FROM scheduled_tasks
		         WHERE status = 'RUNNING' AND claim_owner = $1 AND claim_token <> ALL($2::uuid[])
		         FOR UPDATE SKIP LOCKED) idle
		 WHERE st.tenant_id = idle.tenant_id AND st.id = idle.id`, owner, live)
	if err != nil {
		return 0, fmt.Errorf("failed to give back idle scheduled tasks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Heartbeat stamps the owner's liveness record with the database clock. It is
// an upsert, so a record swept during a long outage comes back.
func (s *scheduledTaskStore) Heartbeat(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.heartbeat.Exec(ctx, `INSERT INTO scheduler_owners (owner, heartbeat_at) VALUES ($1, now())
		ON CONFLICT (owner) DO UPDATE SET heartbeat_at = now()`, owner); err != nil {
		return fmt.Errorf("failed to record the scheduler heartbeat: %w", err)
	}
	return nil
}

func (s *scheduledTaskStore) RetireOwner(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.sched.Exec(ctx, `DELETE FROM scheduler_owners WHERE owner = $1`, owner); err != nil {
		return fmt.Errorf("failed to retire the scheduler owner: %w", err)
	}
	return nil
}

// SweepOwners removes liveness records older than deadFor that no RUNNING task
// references.
func (s *scheduledTaskStore) SweepOwners(ctx context.Context, deadFor time.Duration) error {
	if _, err := s.sched.Exec(ctx, `DELETE FROM scheduler_owners o
		WHERE o.heartbeat_at < now() - ($1::bigint * interval '1 microsecond')
		  AND NOT EXISTS (SELECT 1 FROM scheduled_tasks st
		                   WHERE st.status = 'RUNNING' AND st.claim_owner = o.owner)`,
		deadFor.Microseconds()); err != nil {
		return fmt.Errorf("failed to sweep scheduler owners: %w", err)
	}
	return nil
}

// SweepMarks removes the marks of ended lives: no task row carries their
// (task id, arm token) any more.
func (s *scheduledTaskStore) SweepMarks(ctx context.Context) error {
	if _, err := s.sched.Exec(ctx, `DELETE FROM scheduled_task_marks m
		WHERE NOT EXISTS (SELECT 1 FROM scheduled_tasks st
		                   WHERE st.tenant_id = m.tenant_id AND st.id = m.task_id
		                     AND st.arm_token = m.arm_token)`); err != nil {
		return fmt.Errorf("failed to sweep scheduled task marks: %w", err)
	}
	return nil
}
