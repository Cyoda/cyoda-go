package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func runWithin(t *testing.T, env *runEnv, task spi.ScheduledTask) RunReport {
	t.Helper()
	done := make(chan RunReport, 1)
	go func() { r, _ := env.run(task); done <- r }()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the run hung")
		return RunReport{}
	}
}

func TestCallback_WritesFiredEntity_RunFires(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, _ spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		jctx, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		es, err := env.factory.EntityStore(jctx)
		if err != nil {
			return nil, err
		}
		entity, err := es.Get(jctx, claimed.EntityID)
		if err != nil {
			return nil, err
		}
		if _, err := es.Save(jctx, entity); err != nil {
			return nil, err
		}
		return nil, nil // wrote the entity itself; returns no mutations
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbw-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	if got := env.state(t, "cbw-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
	if _, found := env.task(t, claimed.ID); found {
		t.Errorf("the fired task's life is still stored")
	}
}

func TestCallback_DeletesFiredEntity_RunCommitsWithoutRecreating(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, _ spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		jctx, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		es, err := env.factory.EntityStore(jctx)
		if err != nil {
			return nil, err
		}
		if err := es.Delete(jctx, claimed.EntityID); err != nil {
			return nil, err
		}
		return nil, env.sts.DeleteForEntities(jctx, testTenant, []string{claimed.EntityID})
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbd-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeCancelled || r.Err != nil {
		t.Fatalf("report = %+v, want cancelled and committed", r)
	}
	if env.exists(t, "cbd-e1") {
		t.Errorf("the entity exists: the run re-created an entity its own callback deleted")
	}
	if _, found := env.task(t, claimed.ID); found {
		t.Errorf("the task is still stored")
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "cbd-e1", spi.SMEventScheduledTransitionFired); n != 0 {
		t.Errorf("FIRE events = %d, want 0: the transition did not take effect", n)
	}
}

// A delete committed by another transaction while the run is open is not the
// run's own delete: the run's snapshot still sees the entity, and the final
// persist conflicts. Only the entity is deleted here, not its task, so the
// run is not superseded and the conflict is reported as it is.
func TestCallback_OtherTransactionDeletesFiredEntity_RunConflicts(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		otherTxID, otherCtx, err := env.txMgr.Begin(env.ctx)
		if err != nil {
			return nil, err
		}
		es, err := env.factory.EntityStore(otherCtx)
		if err != nil {
			return nil, err
		}
		if err := es.Delete(otherCtx, claimed.EntityID); err != nil {
			return nil, err
		}
		return nil, env.txMgr.Commit(env.ctx, otherTxID)
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbo-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, spi.ErrConflict) {
		t.Fatalf("report = %+v, want failed on a conflict", r)
	}
	if env.exists(t, "cbo-e1") {
		t.Errorf("the entity exists: the run re-created an entity another transaction deleted")
	}
	if got, found := env.task(t, claimed.ID); !found || got.ArmToken != claimed.ArmToken {
		t.Errorf("task = %+v (found %v), want the claimed life, untouched", got, found)
	}
}

// A joined callback's write and a write committed by another transaction:
// the run's final persist compares against its own write, and the other
// transaction's write still makes the run's commit conflict.
func TestCallback_WritesFiredEntity_OtherTransactionWrites_RunConflicts(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, _ spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		jctx, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		es, err := env.factory.EntityStore(jctx)
		if err != nil {
			return nil, err
		}
		entity, err := es.Get(jctx, claimed.EntityID)
		if err != nil {
			return nil, err
		}
		if _, err := es.Save(jctx, entity); err != nil {
			return nil, err
		}

		otherTxID, otherCtx, err := env.txMgr.Begin(env.ctx)
		if err != nil {
			return nil, err
		}
		oes, err := env.factory.EntityStore(otherCtx)
		if err != nil {
			return nil, err
		}
		other, err := oes.Get(otherCtx, claimed.EntityID)
		if err != nil {
			return nil, err
		}
		other.Data = []byte(`{"by":"other"}`)
		if _, err := oes.Save(otherCtx, other); err != nil {
			return nil, err
		}
		return nil, env.txMgr.Commit(env.ctx, otherTxID)
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbow-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, spi.ErrConflict) {
		t.Fatalf("report = %+v, want failed on a conflict", r)
	}
	if got := env.state(t, "cbow-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN: the other transaction's write stands", got)
	}
	if _, found := env.task(t, claimed.ID); !found {
		t.Errorf("the task is gone: the run's removal of its life must roll back")
	}
}

// failingGetFactory fails EntityStore.Get once *armed is set.
type failingGetFactory struct {
	spi.StoreFactory
	armed *bool
	err   error
}

type failingGetEntityStore struct {
	spi.EntityStore
	armed *bool
	err   error
}

func (f failingGetFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return failingGetEntityStore{EntityStore: es, armed: f.armed, err: f.err}, nil
}

func (s failingGetEntityStore) Get(ctx context.Context, id string) (*spi.Entity, error) {
	if *s.armed {
		return nil, s.err
	}
	return s.EntityStore.Get(ctx, id)
}

// A failed re-read before the final persist is a failed run: nothing
// commits, and it is not taken for a delete.
func TestCallback_FinalReReadFails_RunFails(t *testing.T) {
	armed := false
	storeErr := errors.New("store unavailable")
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		armed = true
		return nil, nil
	}}
	env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
		return failingGetFactory{StoreFactory: f, armed: &armed, err: storeErr}
	}, nil)
	claimed := env.claimed(t, "cbr-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := runWithin(t, env, claimed)
	armed = false
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, storeErr) {
		t.Fatalf("report = %+v, want failed on the store error", r)
	}
	if got := env.state(t, "cbr-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
	if _, found := env.task(t, claimed.ID); !found {
		t.Errorf("the task is gone: nothing of the failed run may commit")
	}
}
