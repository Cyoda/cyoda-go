package memory

import (
	"context"
	"fmt"
	"sort"

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
// TransactionManager (keyed by txID — see TransactionManager.scheduledTaskOps)
// while a transaction is open, and applied to StoreFactory.scheduledTasks
// inside Commit's entityMu critical section so it commits atomically with the
// entity write. Mirrors the txUniqueKeys staging pattern.
type scheduledTaskOp struct {
	kind scheduledTaskOpKind
	id   string
	task spi.ScheduledTask // populated for scheduledTaskUpsert
}

// applyScheduledTaskOp mutates dst per op. Caller must hold the appropriate
// write lock (StoreFactory.entityMu) — this function does no locking itself
// so it can be reused both by the non-transactional immediate-apply path and
// by TransactionManager.Commit's flush.
func applyScheduledTaskOp(dst map[string]spi.ScheduledTask, op scheduledTaskOp) {
	switch op.kind {
	case scheduledTaskUpsert:
		dst[op.id] = op.task
	case scheduledTaskDelete:
		delete(dst, op.id)
	}
}

// copyScheduledTask returns a value copy of t that shares no pointers with
// t, so that neither the caller nor the store can mutate the other's copy
// after the call returns (store copy-on-write discipline, matching
// copyEntity).
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

type scheduledTaskStore struct{ f *StoreFactory }

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// stage applies op immediately (implicit auto-commit) if ctx carries no
// transaction, matching EntityStore.Save's non-transaction mode. Otherwise it
// buffers op on the transaction's TransactionManager entry so Commit can
// apply it atomically with the entity buffer flush, or Rollback can discard
// it untouched.
//
// Locking discipline: mirrors EntityStore.Save — tx.OpMu.RLock is held for
// the duration of the staging so Commit/Rollback (which take tx.OpMu.Lock)
// cannot race with us. Lock order: tx.OpMu before the manager mutex (via
// stageScheduledTaskOp), matching txmanager.Commit's documented order.
func (s *scheduledTaskStore) stage(ctx context.Context, op scheduledTaskOp) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		s.f.entityMu.Lock()
		defer s.f.entityMu.Unlock()
		applyScheduledTaskOp(s.f.scheduledTasks, op)
		return nil
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	s.f.txManager.stageScheduledTaskOp(tx.ID, op)
	return nil
}

func (s *scheduledTaskStore) Upsert(ctx context.Context, task spi.ScheduledTask) error {
	return s.stage(ctx, scheduledTaskOp{kind: scheduledTaskUpsert, id: task.ID, task: copyScheduledTask(task)})
}

func (s *scheduledTaskStore) Delete(ctx context.Context, id string) (bool, error) {
	// Existence is reported against committed state. A delete staged inside
	// an as-yet-uncommitted transaction still reports true here if the row
	// is currently committed — matching Delete's "removed" contract for the
	// callers that rely on it (delete-gated terminal audit).
	s.f.entityMu.RLock()
	_, existed := s.f.scheduledTasks[id]
	s.f.entityMu.RUnlock()

	if err := s.stage(ctx, scheduledTaskOp{kind: scheduledTaskDelete, id: id}); err != nil {
		return false, err
	}
	return existed, nil
}

func (s *scheduledTaskStore) Get(_ context.Context, id string) (*spi.ScheduledTask, bool, error) {
	s.f.entityMu.RLock()
	defer s.f.entityMu.RUnlock()
	t, ok := s.f.scheduledTasks[id]
	if !ok {
		return nil, false, nil
	}
	cp := copyScheduledTask(t)
	return &cp, true, nil
}

func (s *scheduledTaskStore) ScanDue(_ context.Context, nowMs int64, limit int) ([]spi.ScheduledTask, error) {
	s.f.entityMu.RLock()
	defer s.f.entityMu.RUnlock()

	var out []spi.ScheduledTask
	for _, t := range s.f.scheduledTasks {
		if t.ScheduledTime <= nowMs && (t.RedispatchAfter == nil || *t.RedispatchAfter <= nowMs) {
			out = append(out, copyScheduledTask(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledTime < out[j].ScheduledTime })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkRedispatch is a plain (non-transactional) write — it never
// participates in the caller's transaction (see spi.ScheduledTaskStore
// godoc), so it always applies directly under entityMu.
func (s *scheduledTaskStore) MarkRedispatch(_ context.Context, id string, redispatchAfterMs int64) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()

	t, ok := s.f.scheduledTasks[id]
	if !ok {
		return nil
	}
	after := redispatchAfterMs
	t.RedispatchAfter = &after
	t.AttemptCount++
	s.f.scheduledTasks[id] = t
	return nil
}

func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	s.f.entityMu.RLock()
	var cancelled []spi.ScheduledTask
	for _, t := range s.f.scheduledTasks {
		if t.EntityID == req.EntityID && t.TenantID == req.TenantID && t.SourceState != req.CurrentState {
			cancelled = append(cancelled, copyScheduledTask(t))
		}
	}
	s.f.entityMu.RUnlock()

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
	return cancelled, nil
}
