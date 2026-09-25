package memory

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// taskKey identifies one task row. The tenant is part of the key, so no call
// can reach a row of another tenant by naming its id.
type taskKey struct {
	tenant spi.TenantID
	id     string
}

// markKey identifies the unsafe mark of one life of one task.
type markKey struct {
	task taskKey
	arm  uuid.UUID
}

// scheduledTaskOp is one staged write to a task row: the row as it is after
// the write, or nil when the write removes it. A touch changes nothing; it
// only puts the row in the transaction's write set, so that a RemoveLife of a
// life that was replaced after the transaction began still fails the commit,
// as it does on PostgreSQL.
//
// Every check a write makes is evaluated when it is staged, against the
// committed rows and the transaction's earlier ops (see write). Commit only
// has to prove that no other writer changed those rows since the transaction
// began, which its conflict check does; it then applies the post-images as
// they are.
type scheduledTaskOp struct {
	key   taskKey
	after *spi.ScheduledTask
	touch bool
}

// applyTaskOps applies ops to dst in order. Caller holds entityMu for writing.
func applyTaskOps(dst map[taskKey]spi.ScheduledTask, ops []scheduledTaskOp) {
	for _, op := range ops {
		switch {
		case op.touch:
		case op.after == nil:
			delete(dst, op.key)
		default:
			row := copyScheduledTask(*op.after)
			row.UnsafeMarked = false // derived from taskMarks on every read, never stored
			dst[op.key] = row
		}
	}
}

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

// withMarkLocked returns a copy of t with UnsafeMarked set from taskMarks.
// Caller holds entityMu.
func (f *StoreFactory) withMarkLocked(t spi.ScheduledTask) spi.ScheduledTask {
	cp := copyScheduledTask(t)
	_, cp.UnsafeMarked = f.taskMarks[markKey{task: taskKey{tenant: t.TenantID, id: t.ID}, arm: t.ArmToken}]
	return cp
}

// taskView is the set of task rows one call sees: the committed rows, then
// staged, in order. Caller holds entityMu.
type taskView struct {
	f      *StoreFactory
	staged []scheduledTaskOp
}

func (v taskView) get(k taskKey) (spi.ScheduledTask, bool) {
	t, ok := v.f.scheduledTasks[k]
	for _, op := range v.staged {
		if op.key != k || op.touch {
			continue
		}
		if op.after == nil {
			ok = false
			continue
		}
		t, ok = *op.after, true
	}
	if !ok {
		return spi.ScheduledTask{}, false
	}
	return v.f.withMarkLocked(t), true
}

// where returns the rows of tenant that match, as this view sees them,
// sorted by id.
func (v taskView) where(tenant spi.TenantID, match func(spi.ScheduledTask) bool) []spi.ScheduledTask {
	keys := make(map[taskKey]bool)
	for k := range v.f.scheduledTasks {
		if k.tenant == tenant {
			keys[k] = true
		}
	}
	for _, op := range v.staged {
		if op.key.tenant == tenant {
			keys[op.key] = true
		}
	}
	var out []spi.ScheduledTask
	for k := range keys {
		if t, ok := v.get(k); ok && match(t) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// fenced returns the row ref names if its current life and claim are ref's.
// Otherwise, or when the row is missing, the answer is spi.ErrStaleClaim.
func fenced(v taskView, ref spi.TaskRef) (spi.ScheduledTask, error) {
	t, ok := v.get(taskKey{tenant: ref.TenantID, id: ref.ID})
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

type scheduledTaskStore struct{ f *StoreFactory }

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// write runs one joining write. plan sees the rows as this write sees them
// and returns the ops to apply. With a transaction on ctx the ops are staged
// on it, and plan's view includes the transaction's earlier ops (C2). Without
// one, they are applied at once and commit on their own.
//
// Lock order: tx.OpMu (read) → entityMu → mu, the order Commit uses. Holding
// tx.OpMu keeps Commit, Rollback and RollbackToSavepoint of this transaction
// out while plan reads its staged ops.
func (s *scheduledTaskStore) write(ctx context.Context, tenant spi.TenantID, plan func(v taskView) ([]scheduledTaskOp, error)) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		s.f.entityMu.Lock()
		defer s.f.entityMu.Unlock()
		ops, err := plan(taskView{f: s.f})
		if err != nil {
			return err
		}
		s.f.txManager.commitTaskWrites(ops)
		return nil
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

	s.f.entityMu.RLock()
	defer s.f.entityMu.RUnlock()
	ops, err := plan(taskView{f: s.f, staged: s.f.txManager.stagedTaskOps(tx.ID)})
	if err != nil {
		return err
	}
	s.f.txManager.stageTaskOps(tx.ID, ops)
	return nil
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
		for _, t := range v.where(req.TenantID, func(t spi.ScheduledTask) bool { return t.EntityID == req.EntityID }) {
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

// RemoveLife removes the task if its current life is armToken. Otherwise it
// changes nothing, but the row still enters the transaction's write set.
func (s *scheduledTaskStore) RemoveLife(ctx context.Context, tenant spi.TenantID, id string, armToken uuid.UUID) error {
	k := taskKey{tenant: tenant, id: id}
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		if t, ok := v.get(k); ok && t.ArmToken == armToken {
			return []scheduledTaskOp{{key: k}}, nil
		}
		return []scheduledTaskOp{{key: k, touch: true}}, nil
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
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		return removals(v.where(tenant, func(t spi.ScheduledTask) bool { return ids[t.EntityID] })), nil
	})
}

// DeleteForModel removes the model's tasks, except those whose (source state,
// transition) keep retains. A nil keep retains none.
func (s *scheduledTaskStore) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int, keep func(sourceState, transition string) bool) error {
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		return removals(v.where(tenant, func(t spi.ScheduledTask) bool {
			return t.ModelName == modelName && t.ModelVersion == modelVersion &&
				(keep == nil || !keep(t.SourceState, t.Transition))
		})), nil
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
		staged = s.f.txManager.stagedTaskOps(tx.ID)
	}
	s.f.entityMu.RLock()
	defer s.f.entityMu.RUnlock()
	t, ok := taskView{f: s.f, staged: staged}.get(taskKey{tenant: tenant, id: id})
	if !ok {
		return nil, false, nil
	}
	return &t, true, nil
}

// afterCursor reports whether t sorts after c in (ScheduledTime, ID) order.
func afterCursor(t spi.ScheduledTask, c spi.ScheduledTaskCursor) bool {
	return t.ScheduledTime > c.ScheduledTime || (t.ScheduledTime == c.ScheduledTime && t.ID > c.ID)
}

// Query returns one page of tenant's committed tasks in (ScheduledTime, ID)
// order. IDs compare byte-wise (Go string order), as SQLite's BINARY
// collation and PostgreSQL's COLLATE "C" do, so every backend pages the same
// way. It never joins a transaction.
func (s *scheduledTaskStore) Query(_ context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	if q.Limit < 1 {
		return spi.ScheduledTaskPage{}, fmt.Errorf("query scheduled tasks: limit must be >= 1, got %d", q.Limit)
	}
	statuses := make(map[spi.ScheduledTaskStatus]bool, len(q.Statuses))
	for _, st := range q.Statuses {
		statuses[st] = true
	}

	s.f.entityMu.RLock()
	defer s.f.entityMu.RUnlock()
	var rows []spi.ScheduledTask
	for k, t := range s.f.scheduledTasks {
		switch {
		case k.tenant != tenant,
			len(statuses) > 0 && !statuses[t.Status],
			q.ModelName != "" && t.ModelName != q.ModelName,
			q.ModelVersion != 0 && t.ModelVersion != q.ModelVersion,
			q.EntityID != "" && t.EntityID != q.EntityID,
			q.After != nil && !afterCursor(t, *q.After):
			continue
		}
		rows = append(rows, s.f.withMarkLocked(t))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ScheduledTime != rows[j].ScheduledTime {
			return rows[i].ScheduledTime < rows[j].ScheduledTime
		}
		return rows[i].ID < rows[j].ID
	})

	var page spi.ScheduledTaskPage
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		last := rows[q.Limit-1]
		page.Next = &spi.ScheduledTaskCursor{ScheduledTime: last.ScheduledTime, ID: last.ID}
	}
	page.Items = rows
	return page, nil
}
