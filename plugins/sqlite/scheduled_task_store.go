package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// taskKey identifies one task row: the table's primary key.
type taskKey struct {
	tenant spi.TenantID
	id     string
}

// scheduledTaskOp is one staged write to a task row: the row as it is after
// the write, or nil when the write removes it. A touch changes nothing; it
// only puts the row in the transaction's write set, so that a RemoveLife of a
// life replaced after the transaction began still fails the commit (see
// RemoveLife).
//
// Every check a write makes is evaluated when it is staged (see write). The
// commit's conflict check proves that no other writer changed those rows
// since the transaction began, so flushToSQLite writes the post-images as
// they are.
type scheduledTaskOp struct {
	key   taskKey
	after *spi.ScheduledTask
	touch bool
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const taskColumns = `id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
	model_version, transition, source_state, armed_at, armed_by_id, armed_by_kind, arm_token,
	status, next_attempt_time, attempts, lost_owners, last_attempt_time, last_error,
	failure_reason, failed_time, partial_commit, claim_token, claim_owner`

// selectTaskSQL reads task rows as t, with UnsafeMarked: a mark exists for
// the row's current life.
const selectTaskSQL = `SELECT t.id, t.tenant_id, t.type, t.scheduled_time, t.timeout_ms,
	t.entity_id, t.model_name, t.model_version, t.transition, t.source_state, t.armed_at,
	t.armed_by_id, t.armed_by_kind, t.arm_token, t.status, t.next_attempt_time, t.attempts,
	t.lost_owners, t.last_attempt_time, t.last_error, t.failure_reason, t.failed_time,
	t.partial_commit, t.claim_token, t.claim_owner,
	EXISTS (SELECT 1 FROM scheduled_task_marks m
	        WHERE m.tenant_id = t.tenant_id AND m.task_id = t.id AND m.arm_token = t.arm_token)
	FROM scheduled_tasks t`

// upsertTaskSQL writes a whole row: every column but the key is replaced.
const upsertTaskSQL = `INSERT INTO scheduled_tasks (` + taskColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (tenant_id, id) DO UPDATE SET
	  type = excluded.type, scheduled_time = excluded.scheduled_time,
	  timeout_ms = excluded.timeout_ms, entity_id = excluded.entity_id,
	  model_name = excluded.model_name, model_version = excluded.model_version,
	  transition = excluded.transition, source_state = excluded.source_state,
	  armed_at = excluded.armed_at, armed_by_id = excluded.armed_by_id,
	  armed_by_kind = excluded.armed_by_kind, arm_token = excluded.arm_token,
	  status = excluded.status, next_attempt_time = excluded.next_attempt_time,
	  attempts = excluded.attempts, lost_owners = excluded.lost_owners,
	  last_attempt_time = excluded.last_attempt_time, last_error = excluded.last_error,
	  failure_reason = excluded.failure_reason, failed_time = excluded.failed_time,
	  partial_commit = excluded.partial_commit, claim_token = excluded.claim_token,
	  claim_owner = excluded.claim_owner`

func copyInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// copyScheduledTask returns a copy of t that shares no pointer with it.
func copyScheduledTask(t spi.ScheduledTask) spi.ScheduledTask {
	cp := t
	cp.TimeoutMs = copyInt64(t.TimeoutMs)
	cp.LastAttemptTime = copyInt64(t.LastAttemptTime)
	cp.FailedTime = copyInt64(t.FailedTime)
	if t.Claim != nil {
		c := *t.Claim
		cp.Claim = &c
	}
	return cp
}

// newLife is the row an arm writes: the caller's schedule fields and a fresh
// life. The tenant and the entity come from the reconcile request; the
// status, the tokens and the run bookkeeping belong to the store. Whatever
// the caller set in those fields of a is ignored.
func newLife(tenant spi.TenantID, entityID string, a spi.ScheduledTask) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID:              a.ID,
		TenantID:        tenant,
		Type:            a.Type,
		ScheduledTime:   a.ScheduledTime,
		TimeoutMs:       copyInt64(a.TimeoutMs),
		EntityID:        entityID,
		ModelName:       a.ModelName,
		ModelVersion:    a.ModelVersion,
		Transition:      a.Transition,
		SourceState:     a.SourceState,
		ArmedAt:         a.ArmedAt,
		ArmedBy:         a.ArmedBy,
		Status:          spi.ScheduledTaskWaiting,
		ArmToken:        uuid.New(),
		NextAttemptTime: a.ScheduledTime,
	}
}

func taskArgs(t spi.ScheduledTask) []any {
	var claimToken, claimOwner any
	if t.Claim != nil {
		claimToken, claimOwner = t.Claim.Token.String(), t.Claim.Owner.String()
	}
	partial := 0
	if t.PartialCommit {
		partial = 1
	}
	return []any{
		t.ID, string(t.TenantID), string(t.Type), t.ScheduledTime, t.TimeoutMs, t.EntityID,
		t.ModelName, t.ModelVersion, t.Transition, t.SourceState, t.ArmedAt, t.ArmedBy.ID,
		string(t.ArmedBy.Kind), t.ArmToken.String(), string(t.Status), t.NextAttemptTime,
		t.Attempts, t.LostOwners, t.LastAttemptTime, t.LastError, string(t.FailureReason),
		t.FailedTime, partial, claimToken, claimOwner,
	}
}

// scanTask scans one row of selectTaskSQL.
func scanTask(scan func(dest ...any) error) (spi.ScheduledTask, error) {
	var t spi.ScheduledTask
	var tenantID, taskType, armedByID, armedByKind, armToken, status, reason string
	var timeoutMs, lastAttempt, failedTime sql.NullInt64
	var claimToken, claimOwner sql.NullString
	var partial, marked int64
	if err := scan(&t.ID, &tenantID, &taskType, &t.ScheduledTime, &timeoutMs, &t.EntityID,
		&t.ModelName, &t.ModelVersion, &t.Transition, &t.SourceState, &t.ArmedAt, &armedByID,
		&armedByKind, &armToken, &status, &t.NextAttemptTime, &t.Attempts, &t.LostOwners,
		&lastAttempt, &t.LastError, &reason, &failedTime, &partial, &claimToken, &claimOwner,
		&marked); err != nil {
		return spi.ScheduledTask{}, err
	}
	var err error
	if t.ArmToken, err = uuid.Parse(armToken); err != nil {
		return spi.ScheduledTask{}, fmt.Errorf("failed to read the arm token of scheduled task %s: %w", t.ID, err)
	}
	if claimToken.Valid {
		token, err := uuid.Parse(claimToken.String)
		if err != nil {
			return spi.ScheduledTask{}, fmt.Errorf("failed to read the claim of scheduled task %s: %w", t.ID, err)
		}
		owner, err := uuid.Parse(claimOwner.String)
		if err != nil {
			return spi.ScheduledTask{}, fmt.Errorf("failed to read the claim owner of scheduled task %s: %w", t.ID, err)
		}
		t.Claim = &spi.TaskClaim{Token: token, Owner: owner}
	}
	if timeoutMs.Valid {
		t.TimeoutMs = &timeoutMs.Int64
	}
	if lastAttempt.Valid {
		t.LastAttemptTime = &lastAttempt.Int64
	}
	if failedTime.Valid {
		t.FailedTime = &failedTime.Int64
	}
	t.TenantID = spi.TenantID(tenantID)
	t.Type = spi.ScheduledTaskType(taskType)
	t.ArmedBy = spi.Principal{ID: armedByID, Kind: spi.PrincipalKind(armedByKind)}
	t.Status = spi.ScheduledTaskStatus(status)
	t.FailureReason = spi.ScheduledTaskFailureReason(reason)
	t.PartialCommit = partial == 1
	t.UnsafeMarked = marked == 1
	return t, nil
}

// readTasks runs a selectTaskSQL query and closes its rows before returning,
// so the caller may issue the next statement on the single writer connection.
func readTasks(ctx context.Context, q queryer, query string, args ...any) ([]spi.ScheduledTask, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
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

// applyTaskOp writes op through exec.
func applyTaskOp(ctx context.Context, exec execer, op scheduledTaskOp) error {
	switch {
	case op.touch:
		return nil
	case op.after == nil:
		_, err := exec.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE tenant_id = ? AND id = ?`,
			string(op.key.tenant), op.key.id)
		return err
	default:
		_, err := exec.ExecContext(ctx, upsertTaskSQL, taskArgs(*op.after)...)
		return err
	}
}

// taskView is the set of task rows one call sees: the committed rows, read
// from db, then staged, in order.
type taskView struct {
	ctx    context.Context
	db     *sql.DB
	staged []scheduledTaskOp
}

// markExists reports whether a mark is on file for id's current life (k,
// armToken). UnsafeMarked is never a stored column: a committed row's SQL
// read (selectTaskSQL) already derives it from scheduled_task_marks, but a
// staged row's post-image was built before this call and cannot reflect a
// mark written since — for example a never-joining MarkUnsafe that landed
// before this transaction staged its first write to the row (a MarkUnsafe
// against a row this transaction has already staged a write to is refused
// with ErrTaskBusy instead, C6 — not yet implemented on this backend). So a
// staged row's UnsafeMarked is always re-derived with this, never trusted
// from the post-image (matches the memory backend's withMarkLocked, which
// does the same on every read).
func markExists(ctx context.Context, q queryer, k taskKey, armToken uuid.UUID) (bool, error) {
	var marked int64
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM scheduled_task_marks WHERE tenant_id = ? AND task_id = ? AND arm_token = ?)`,
		string(k.tenant), k.id, armToken.String()).Scan(&marked)
	if err != nil {
		return false, fmt.Errorf("failed to read the mark of scheduled task %s: %w", k.id, err)
	}
	return marked == 1, nil
}

func (v taskView) get(k taskKey) (spi.ScheduledTask, bool, error) {
	var t spi.ScheduledTask
	found := false
	rows, err := readTasks(v.ctx, v.db, selectTaskSQL+` WHERE t.tenant_id = ? AND t.id = ?`, string(k.tenant), k.id)
	if err != nil {
		return spi.ScheduledTask{}, false, fmt.Errorf("failed to read scheduled task %s: %w", k.id, err)
	}
	if len(rows) == 1 {
		t, found = rows[0], true
	}
	staged := false
	for _, op := range v.staged {
		if op.key != k || op.touch {
			continue
		}
		if op.after == nil {
			t, found, staged = spi.ScheduledTask{}, false, false
			continue
		}
		t, found, staged = copyScheduledTask(*op.after), true, true
	}
	if found && staged {
		marked, err := markExists(v.ctx, v.db, k, t.ArmToken)
		if err != nil {
			return spi.ScheduledTask{}, false, err
		}
		t.UnsafeMarked = marked
	}
	return t, found, nil
}

// where returns the rows of tenant that match, as this view sees them, sorted
// by id. filter narrows the committed rows in SQL, with args; match decides
// on every row the view holds, staged ones included.
func (v taskView) where(tenant spi.TenantID, filter string, args []any, match func(spi.ScheduledTask) bool) ([]spi.ScheduledTask, error) {
	committed, err := readTasks(v.ctx, v.db, selectTaskSQL+` WHERE t.tenant_id = ? AND `+filter,
		append([]any{string(tenant)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("failed to read scheduled tasks: %w", err)
	}
	rows := make(map[taskKey]spi.ScheduledTask, len(committed))
	for _, t := range committed {
		rows[taskKey{tenant: t.TenantID, id: t.ID}] = t
	}
	staged := make(map[taskKey]bool)
	for _, op := range v.staged {
		if op.key.tenant != tenant || op.touch {
			continue
		}
		if op.after == nil {
			delete(rows, op.key)
			delete(staged, op.key)
			continue
		}
		rows[op.key] = copyScheduledTask(*op.after)
		staged[op.key] = true
	}
	// A staged row's UnsafeMarked is a snapshot from staging time; re-derive
	// it fresh, the same way get() does (see markExists).
	for k := range staged {
		t := rows[k]
		marked, err := markExists(v.ctx, v.db, k, t.ArmToken)
		if err != nil {
			return nil, err
		}
		t.UnsafeMarked = marked
		rows[k] = t
	}
	var out []spi.ScheduledTask
	for _, t := range rows {
		if match(t) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// fenced returns the row ref names if its current life and claim are ref's.
// Otherwise, or when the row is missing, the answer is spi.ErrStaleClaim.
func fenced(v taskView, ref spi.TaskRef) (spi.ScheduledTask, error) {
	t, ok, err := v.get(taskKey{tenant: ref.TenantID, id: ref.ID})
	if err != nil {
		return spi.ScheduledTask{}, err
	}
	if !ok || t.ArmToken != ref.ArmToken || t.Status != spi.ScheduledTaskRunning ||
		t.Claim == nil || t.Claim.Token != ref.ClaimToken {
		return spi.ScheduledTask{}, fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrStaleClaim)
	}
	return t, nil
}

func removals(ts []spi.ScheduledTask) []scheduledTaskOp {
	ops := make([]scheduledTaskOp, 0, len(ts))
	for _, t := range ts {
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}})
	}
	return ops
}

type scheduledTaskStore struct {
	db     *sql.DB
	readDB *sql.DB
	tm     *transactionManager
	clock  Clock
}

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// write runs one joining write. plan sees the rows as this write sees them
// and returns the ops to apply. With a transaction on ctx the ops are staged
// on it, and plan's view includes the transaction's earlier ops (C2). Without
// one they are written at once, under the commit gate, and commit on their own.
//
// Holding tx.OpMu (read) keeps Commit, Rollback and RollbackToSavepoint of this
// transaction out while plan reads its staged ops. With a transaction, plan
// runs inside stageTaskWrite, which reads the staged ops, plans and appends
// under one lock, so two joining writes on one transaction are serialised.
// Without one, the commit gate is held from plan's first read to the log
// entry, so the call is atomic with respect to every other writer and to
// Begin.
func (s *scheduledTaskStore) write(ctx context.Context, tenant spi.TenantID, plan func(v taskView) ([]scheduledTaskOp, error)) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		_ = s.tm.acquireCommitGate(context.Background())
		defer s.tm.releaseCommitGate()
		ops, err := plan(taskView{ctx: ctx, db: s.db})
		if err != nil {
			return err
		}
		return s.tm.commitTaskWrites(ctx, ops, nil)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	if tx.Closed {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
	}
	if tx.TenantID != tenant {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
	}
	return s.tm.stageTaskWrite(tx.ID, func(staged []scheduledTaskOp) ([]scheduledTaskOp, error) {
		return plan(taskView{ctx: ctx, db: s.db, staged: staged})
	})
}

// ReconcileForEntity arms req.Arm, each as a new life, and removes every
// other task of the entity. It returns the removed tasks, except those named
// in req.Cancel, which the caller audits on their own.
func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	var removed []spi.ScheduledTask
	err := s.write(ctx, req.TenantID, func(v taskView) ([]scheduledTaskOp, error) {
		removed = nil
		cancel := make(map[string]bool, len(req.Cancel))
		for _, id := range req.Cancel {
			cancel[id] = true
		}
		armed := make(map[string]bool, len(req.Arm))
		ops := make([]scheduledTaskOp, 0, len(req.Arm))
		for _, a := range req.Arm {
			row := newLife(req.TenantID, req.EntityID, a)
			armed[a.ID] = true
			ops = append(ops, scheduledTaskOp{key: taskKey{tenant: req.TenantID, id: a.ID}, after: &row})
		}
		current, err := v.where(req.TenantID, `t.entity_id = ?`, []any{req.EntityID},
			func(t spi.ScheduledTask) bool { return t.EntityID == req.EntityID })
		if err != nil {
			return nil, err
		}
		for _, t := range current {
			if armed[t.ID] {
				continue
			}
			ops = append(ops, scheduledTaskOp{key: taskKey{tenant: req.TenantID, id: t.ID}})
			if !cancel[t.ID] {
				removed = append(removed, t)
			}
		}
		return ops, nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// RemoveLife removes the task if its current life is armToken. It is a write
// exactly when armToken is the life the transaction's snapshot shows, after
// its own staged ops. When the row now shows another life, or none, it
// changes nothing: if a write logged after Begin changed the row, the named
// life may have been current at the snapshot, so the row enters the write
// set (a touch) and the commit fails; otherwise the snapshot already showed
// the life replaced or missing, and the call is no write at all. With no
// transaction, only a current life is a write.
func (s *scheduledTaskStore) RemoveLife(ctx context.Context, tenant spi.TenantID, id string, armToken uuid.UUID) error {
	k := taskKey{tenant: tenant, id: id}
	tx := spi.GetTransaction(ctx)
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		t, ok, err := v.get(k)
		if err != nil {
			return nil, err
		}
		if ok && t.ArmToken == armToken {
			return []scheduledTaskOp{{key: k}}, nil
		}
		if tx != nil && s.tm.taskRowWrittenAfterBegin(tx.ID, k) {
			return []scheduledTaskOp{{key: k, touch: true}}, nil
		}
		return nil, nil
	})
}

// StampSegment writes the task row of ref, and sets PartialCommit when partial.
func (s *scheduledTaskStore) StampSegment(ctx context.Context, ref spi.TaskRef, partial bool) error {
	return s.write(ctx, ref.TenantID, func(v taskView) ([]scheduledTaskOp, error) {
		t, err := fenced(v, ref)
		if err != nil {
			return nil, err
		}
		t.PartialCommit = t.PartialCommit || partial
		return []scheduledTaskOp{{key: taskKey{tenant: ref.TenantID, id: ref.ID}, after: &t}}, nil
	})
}

func (s *scheduledTaskStore) DeleteForEntities(ctx context.Context, tenant spi.TenantID, entityIDs []string) error {
	ids := make(map[string]bool, len(entityIDs))
	for _, id := range entityIDs {
		ids[id] = true
	}
	list, err := json.Marshal(entityIDs)
	if err != nil {
		return fmt.Errorf("failed to encode entity ids: %w", err)
	}
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		ts, err := v.where(tenant, `t.entity_id IN (SELECT value FROM json_each(?))`, []any{string(list)},
			func(t spi.ScheduledTask) bool { return ids[t.EntityID] })
		if err != nil {
			return nil, err
		}
		return removals(ts), nil
	})
}

// DeleteForModel removes the model's tasks, except those whose (source state,
// transition) keep retains. A nil keep retains none.
func (s *scheduledTaskStore) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int, keep func(sourceState, transition string) bool) error {
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		ts, err := v.where(tenant, `t.model_name = ? AND t.model_version = ?`, []any{modelName, modelVersion},
			func(t spi.ScheduledTask) bool {
				return t.ModelName == modelName && t.ModelVersion == modelVersion &&
					(keep == nil || !keep(t.SourceState, t.Transition))
			})
		if err != nil {
			return nil, err
		}
		return removals(ts), nil
	})
}

// Fail sets the task of ref to FAILED and clears its claim.
func (s *scheduledTaskStore) Fail(ctx context.Context, ref spi.TaskRef, f spi.Failure) error {
	return s.write(ctx, ref.TenantID, func(v taskView) ([]scheduledTaskOp, error) {
		t, err := fenced(v, ref)
		if err != nil {
			return nil, err
		}
		at := f.AtMs
		t.Status = spi.ScheduledTaskFailed
		t.FailureReason = f.Reason
		t.LastError = f.Error
		t.FailedTime = &at
		t.Claim = nil
		return []scheduledTaskOp{{key: taskKey{tenant: ref.TenantID, id: ref.ID}, after: &t}}, nil
	})
}

// Get reads one task of tenant. With a transaction on ctx it sees that
// transaction's staged ops (C2).
func (s *scheduledTaskStore) Get(ctx context.Context, tenant spi.TenantID, id string) (*spi.ScheduledTask, bool, error) {
	var staged []scheduledTaskOp
	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		staged = s.tm.stagedTaskOps(tx.ID)
	}
	t, ok, err := taskView{ctx: ctx, db: s.db, staged: staged}.get(taskKey{tenant: tenant, id: id})
	if err != nil || !ok {
		return nil, false, err
	}
	return &t, true, nil
}

// Query returns one page of tenant's committed tasks in (scheduled_time, id)
// order. id has SQLite's default BINARY collation, so ids compare byte-wise,
// as Go strings (memory) and PostgreSQL's COLLATE "C" do, and every backend
// pages the same way. It never joins a transaction and reads on readDB, so it
// never waits for the writer.
func (s *scheduledTaskStore) Query(ctx context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	if q.Limit < 1 {
		return spi.ScheduledTaskPage{}, fmt.Errorf("query scheduled tasks: limit must be >= 1, got %d: %w", q.Limit, spi.ErrStoreRejected)
	}
	var where strings.Builder
	args := []any{string(tenant)}
	where.WriteString(` WHERE t.tenant_id = ?`)
	if len(q.Statuses) > 0 {
		statuses, err := json.Marshal(q.Statuses)
		if err != nil {
			return spi.ScheduledTaskPage{}, fmt.Errorf("failed to encode statuses: %w", err)
		}
		where.WriteString(` AND t.status IN (SELECT value FROM json_each(?))`)
		args = append(args, string(statuses))
	}
	if q.ModelName != "" {
		where.WriteString(` AND t.model_name = ?`)
		args = append(args, q.ModelName)
	}
	if q.ModelVersion != 0 {
		where.WriteString(` AND t.model_version = ?`)
		args = append(args, q.ModelVersion)
	}
	if q.EntityID != "" {
		where.WriteString(` AND t.entity_id = ?`)
		args = append(args, q.EntityID)
	}
	if q.After != nil {
		where.WriteString(` AND (t.scheduled_time > ? OR (t.scheduled_time = ? AND t.id > ?))`)
		args = append(args, q.After.ScheduledTime, q.After.ScheduledTime, q.After.ID)
	}
	args = append(args, q.Limit+1)
	rows, err := readTasks(ctx, s.readDB, selectTaskSQL+where.String()+` ORDER BY t.scheduled_time, t.id LIMIT ?`, args...)
	if err != nil {
		return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	var page spi.ScheduledTaskPage
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		last := rows[q.Limit-1]
		page.Next = &spi.ScheduledTaskCursor{ScheduledTime: last.ScheduledTime, ID: last.ID}
	}
	page.Items = rows
	return page, nil
}
