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
//     DeleteForEntities, DeleteForModel, Fail, and Get. Task rows are written
//     straight into the open entity transaction, which runs at REPEATABLE
//     READ. A task row that another transaction changed after this one's
//     snapshot raises 40001, which the querier maps to spi.ErrConflict (C1,
//     C5). A row this transaction wrote or locked stays locked until it ends,
//     so ClaimDue's and GiveBackIdle's SKIP LOCKED pass it over, MarkUnsafe's
//     NOWAIT answers ErrTaskBusy, and RecordAttempt answers ErrTaskBusy once
//     lock_timeout ends its wait (C6).
//   - query never joins and runs on the main pool: Query.
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
	q         Querier
	query     Querier
	pool      *pgxpool.Pool
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
// or pgx.Rows' Scan. scheduled_tasks_claim_chk guarantees claim_token and
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

// Get joins the transaction on ctx when there is one, for reads only: it
// answers from tenant, whatever the transaction's tenant, and never refuses.
func (s *scheduledTaskStore) Get(ctx context.Context, tenant spi.TenantID, id string) (*spi.ScheduledTask, bool, error) {
	var marked bool
	t, err := scanTask(s.q.QueryRow(ctx, `SELECT `+taskColumns+`, `+markedColumn+`
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
	if q.Limit < 1 {
		return spi.ScheduledTaskPage{}, fmt.Errorf("query scheduled tasks: Limit must be >= 1, got %d: %w",
			q.Limit, spi.ErrStoreRejected)
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

// claimableCondition is the row's own claim condition, on alias st, with
// $1 = NowMs, $2 = AllowLostOwner, $3 = StaleAfter in microseconds. A RUNNING
// row qualifies, whatever its next_attempt_time, when its owner's liveness
// record is missing or older than StaleAfter by the database clock.
const claimableCondition = `(
	(st.status = 'WAITING' AND st.next_attempt_time <= $1)
	OR ($2::boolean AND st.status = 'RUNNING'
	    AND NOT EXISTS (SELECT 1 FROM scheduler_owners o
	                     WHERE o.owner = st.claim_owner
	                       AND o.heartbeat_at >= now() - ($3::bigint * interval '1 microsecond'))))`

// rankClaimsSQL is ClaimDue's ranking. It returns the tasks one claim takes,
// in the order spi.SelectClaims defines:
//
//   - candidates: claimableCondition, less any task whose entity has another
//     RUNNING task, less the (tenant, id) pairs in $8/$9 — rows an earlier
//     round of this claim found busy or no longer claimable;
//   - one task per entity: the first in (next_attempt_time, id) order;
//   - within a tenant, turn = the task's place in (next_attempt_time, id)
//     order; a tenant's turns stop at PerTenantLimit minus its runs in
//     progress ($4, $5, $6);
//   - tenants take turns, one task per turn: turn first, then the tenant with
//     the earliest candidate, ties broken by tenant id;
//   - at most Limit ($7) tasks.
//
// Ids and tenant ids compare byte-wise (COLLATE "C"), as Go compares strings.
const rankClaimsSQL = `WITH candidate AS (
	SELECT st.id, st.tenant_id, st.entity_id, st.next_attempt_time
	  FROM scheduled_tasks st
	 WHERE ` + claimableCondition + `
	   AND NOT EXISTS (SELECT 1 FROM scheduled_tasks r
	                    WHERE r.tenant_id = st.tenant_id AND r.entity_id = st.entity_id
	                      AND r.status = 'RUNNING' AND r.id <> st.id)
	   AND NOT EXISTS (SELECT 1 FROM unnest($8::text[], $9::text[]) AS x(tenant_id, id)
	                    WHERE x.tenant_id = st.tenant_id AND x.id = st.id)
), one_per_entity AS (
	SELECT DISTINCT ON (tenant_id, entity_id) id, tenant_id, next_attempt_time
	  FROM candidate
	 ORDER BY tenant_id, entity_id, next_attempt_time, id COLLATE "C"
), ranked AS (
	SELECT id, tenant_id,
	       row_number() OVER (PARTITION BY tenant_id ORDER BY next_attempt_time, id COLLATE "C") AS turn,
	       min(next_attempt_time) OVER (PARTITION BY tenant_id) AS tenant_first
	  FROM one_per_entity
)
SELECT r.tenant_id, r.id
  FROM ranked r
  LEFT JOIN unnest($5::text[], $6::int[]) AS busy(tenant_id, runs) ON busy.tenant_id = r.tenant_id
 WHERE r.turn <= $4::int - COALESCE(busy.runs, 0)
 ORDER BY r.turn, r.tenant_first, r.tenant_id COLLATE "C"
 LIMIT $7`

// lockClaimsSQL locks ranked rows, skipping any row another transaction holds
// (C6), and repeats the claim condition against each row's latest version.
// It returns each locked row's status before the claim: RUNNING means the
// claim takes the task from a stale or missing owner. The row lock holds that
// status until claimSQL runs.
const lockClaimsSQL = `SELECT st.tenant_id, st.id, st.status
  FROM scheduled_tasks st
  JOIN unnest($4::text[], $5::text[]) AS c(tenant_id, id) ON c.tenant_id = st.tenant_id AND c.id = st.id
 WHERE ` + claimableCondition + `
 ORDER BY st.tenant_id, st.id
 FOR UPDATE OF st SKIP LOCKED`

// claimSQL is ClaimDue's claim. A new statement, so it sees claims other
// pnodes committed since the ranking; the full condition, including "no other
// RUNNING task of the entity", closes that race. A concurrent claim of a
// sibling that has not committed yet meets this one at the unique index.
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
// pool and returns them in spi.SelectClaims order. A claim that loses a race
// for a sibling task rolls back and claims nothing (see lostClaimRace).
func (s *scheduledTaskStore) ClaimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if req.Limit < 1 || req.PerTenantLimit < 1 {
		return nil, fmt.Errorf("claim scheduled tasks: Limit and PerTenantLimit must be >= 1, got %d and %d: %w",
			req.Limit, req.PerTenantLimit, spi.ErrStoreRejected)
	}
	claimed, err := s.claimDue(ctx, req)
	if lostClaimRace(err) {
		slog.Log(ctx, lostClaimRaceLevel(err),
			"scheduled task claim met a concurrent lock or claim; claiming nothing this call",
			"pkg", "postgres", "err", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to claim scheduled tasks: %w", err)
	}
	return claimed, nil
}

// claimDue ranks, locks, and ranks again until every ranked row is locked. A
// ranked row the lock skipped — busy under an open transaction, or no longer
// claimable — is excluded and the ranking runs again, so its turn goes to the
// next task and the result is spi.SelectClaims over the claimable rows no
// other transaction holds. Rows locked in an earlier round stay locked. Every
// round but the last excludes at least one row, so the loop ends.
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

	fromLostOwner := make(map[taskKey]bool) // every row locked so far
	exclTenants, exclIDs := []string{}, []string{}
	var chosen []taskKey
	for {
		chosen, err = rankClaims(ctx, q, req, stale, tenants, running, exclTenants, exclIDs)
		if err != nil {
			return nil, err
		}
		var lockTenants, lockIDs []string
		for _, k := range chosen {
			if _, ok := fromLostOwner[k]; !ok {
				lockTenants, lockIDs = append(lockTenants, k.tenant), append(lockIDs, k.id)
			}
		}
		if len(lockIDs) == 0 {
			break
		}
		if err := lockClaims(ctx, q, req, stale, lockTenants, lockIDs, fromLostOwner); err != nil {
			return nil, err
		}
		skipped := false
		for i, id := range lockIDs {
			k := taskKey{tenant: lockTenants[i], id: id}
			if _, ok := fromLostOwner[k]; !ok {
				exclTenants, exclIDs = append(exclTenants, k.tenant), append(exclIDs, k.id)
				skipped = true
			}
		}
		if !skipped {
			break
		}
	}
	if len(chosen) == 0 {
		return nil, nil
	}

	chosenTenants := make([]string, len(chosen))
	chosenIDs := make([]string, len(chosen))
	for i, k := range chosen {
		chosenTenants[i], chosenIDs[i] = k.tenant, k.id
	}
	rows, err := q.Query(ctx, claimSQL, req.NowMs, req.AllowLostOwner, stale, chosenIDs, req.Owner, chosenTenants)
	if err != nil {
		return nil, claimStepError(err)
	}
	updated, err := scanTasks(rows)
	if err != nil {
		return nil, claimStepError(err)
	}
	byKey := make(map[taskKey]spi.ScheduledTask, len(updated))
	for _, t := range updated {
		// Set on the returned copy only; the row never stores it.
		t.ClaimedFromLostOwner = fromLostOwner[keyOf(t)]
		byKey[keyOf(t)] = t
	}
	claimed := make([]spi.ScheduledTask, 0, len(updated))
	for _, k := range chosen {
		if t, ok := byKey[k]; ok {
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

func rankClaims(ctx context.Context, q Querier, req spi.ClaimRequest, stale int64,
	tenants []string, running []int, exclTenants, exclIDs []string) ([]taskKey, error) {
	rows, err := q.Query(ctx, rankClaimsSQL, req.NowMs, req.AllowLostOwner, stale, req.PerTenantLimit,
		tenants, running, req.Limit, exclTenants, exclIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chosen []taskKey
	for rows.Next() {
		var k taskKey
		if err := rows.Scan(&k.tenant, &k.id); err != nil {
			return nil, err
		}
		chosen = append(chosen, k)
	}
	return chosen, rows.Err()
}

// lockClaims locks the rows it can and records each one's pre-claim status in
// locked (true: RUNNING, taken from a lost owner).
func lockClaims(ctx context.Context, q Querier, req spi.ClaimRequest, stale int64,
	tenants, ids []string, locked map[taskKey]bool) error {
	rows, err := q.Query(ctx, lockClaimsSQL, req.NowMs, req.AllowLostOwner, stale, tenants, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k taskKey
		var status string
		if err := rows.Scan(&k.tenant, &k.id, &status); err != nil {
			return err
		}
		locked[k] = status == string(spi.ScheduledTaskRunning)
	}
	return rows.Err()
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

// errClaimStep marks an error raised by claimSQL, the one statement that can
// wait on another claim's entry in the one-RUNNING-task-per-entity index.
var errClaimStep = errors.New("claim statement")

func claimStepError(err error) error {
	return fmt.Errorf("%w: %w", errClaimStep, err)
}

// lostClaimRace reports a claim that met a concurrent claim or lock and must
// roll back and claim nothing: the one-RUNNING-task-per-entity index refused
// it after the rival committed (23505), a lock wait reached lock_timeout
// (55P03), or two transactions waited on each other (40P01).
func lostClaimRace(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case pgerrcode.DeadlockDetected, pgerrcode.LockNotAvailable:
		return true
	case pgerrcode.UniqueViolation:
		return pgErr.ConstraintName == "scheduled_tasks_one_running_per_entity_uq"
	}
	return false
}

// lostClaimRaceLevel is the level a lost claim is logged at. Only claimSQL can
// wait on the one-RUNNING-task-per-entity index, and meeting a sibling's claim
// there is expected: DEBUG. A lock wait or deadlock anywhere else in the claim
// is not expected: WARN.
func lostClaimRaceLevel(err error) slog.Level {
	if errors.Is(err, errClaimStep) {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable
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
