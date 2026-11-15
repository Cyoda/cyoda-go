package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
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

// The same write from the callback of a COMMIT_BEFORE_DISPATCH processor
// with startNewTxOnDispatch=true, which joins the segment the boundary opens.
func TestCallback_WritesFiredEntity_AtSegmentBoundary_RunFires(t *testing.T) {
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
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	startNewTx := true
	p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
	p1.StartNewTxOnDispatch = &startNewTx
	claimed = env.claimed(t, "cbwseg-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{p1}, nil))

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	if got := env.state(t, "cbwseg-e1"); got != "CLOSED" {
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

// A self-loop whose callback deletes only the entity: the run itself removes
// its life, and does not re-arm the transition for an entity that is gone.
func TestCallback_DeletesFiredEntity_SelfLoop_RemovesLifeWithoutReArm(t *testing.T) {
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
		return nil, es.Delete(jctx, claimed.EntityID)
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbdl-e1", oneHopWF("OPEN", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeCancelled || r.Err != nil {
		t.Fatalf("report = %+v, want cancelled and committed", r)
	}
	if env.exists(t, "cbdl-e1") {
		t.Errorf("the entity exists: the run re-created an entity its own callback deleted")
	}
	if got, found := env.task(t, claimed.ID); found {
		t.Errorf("task = %+v, want none: the run must remove its life and not re-arm", got)
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

// A failed re-read at the segment boundary is an infrastructure failure, not
// taken for a delete or for another transaction's write.
func TestCallback_BoundaryReReadFails_RunFails(t *testing.T) {
	armed := false
	storeErr := errors.New("store unavailable")
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		armed = true
		return nil, nil
	}}
	env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
		return failingGetFactory{StoreFactory: f, armed: &armed, err: storeErr}
	}, nil)
	startNewTx := true
	p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
	p1.StartNewTxOnDispatch = &startNewTx
	claimed := env.claimed(t, "cbbr-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{p1}, nil))

	r := runWithin(t, env, claimed)
	armed = false
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, storeErr) || !errors.Is(r.Err, ErrCommitBeforeDispatchInfra) {
		t.Fatalf("report = %+v, want failed on the store error, marked infrastructure", r)
	}
	if got := env.state(t, "cbbr-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

// failingStoreFactory fails EntityStore itself once *armed is set.
type failingStoreFactory struct {
	spi.StoreFactory
	armed *bool
	err   error
}

func (f failingStoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	if *f.armed {
		return nil, f.err
	}
	return f.StoreFactory.EntityStore(ctx)
}

// TX_post's read of the anchor before the dispatch cannot be made: the run
// fails as an infrastructure failure and nothing is dispatched.
func TestCallback_TXPostAnchorReadFails_RunFails(t *testing.T) {
	cases := []struct {
		name string
		wrap func(f spi.StoreFactory, armed *bool, err error) spi.StoreFactory
	}{
		{"read fails", func(f spi.StoreFactory, armed *bool, err error) spi.StoreFactory {
			return failingGetFactory{StoreFactory: f, armed: armed, err: err}
		}},
		{"store unavailable", func(f spi.StoreFactory, armed *bool, err error) spi.StoreFactory {
			return failingStoreFactory{StoreFactory: f, armed: armed, err: err}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			armed := false
			storeErr := errors.New("store unavailable")
			ext := &scriptedExtProc{}
			env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
				return tc.wrap(f, &armed, storeErr)
			}, func(tm spi.TransactionManager) spi.TransactionManager {
				// The second Begin is TX_post's, after TX_pre committed.
				return &beforeNthBeginTxMgr{TransactionManager: tm, n: 2, hook: func() error { armed = true; return nil }}
			})
			startNewTx := true
			p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
			p1.StartNewTxOnDispatch = &startNewTx
			claimed := env.claimed(t, "cbtp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{p1}, nil))

			r := runWithin(t, env, claimed)
			armed = false
			if r.Outcome != OutcomeFailed || !errors.Is(r.Err, storeErr) || !errors.Is(r.Err, ErrCommitBeforeDispatchInfra) {
				t.Fatalf("report = %+v, want failed on the store error, marked infrastructure", r)
			}
			if n := ext.count("p1"); n != 0 {
				t.Errorf("p1 dispatched %d times, want 0", n)
			}
		})
	}
}

// nthFailFactory fails the nth EntityStore call (failStoreAt) or the nth
// EntityStore.Get (failGetAt), counting from 1; 0 fails none.
type nthFailFactory struct {
	spi.StoreFactory
	failStoreAt, failGetAt int
	stores, gets           *int
	err                    error
}

type nthFailEntityStore struct {
	spi.EntityStore
	f nthFailFactory
}

func (f nthFailFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	*f.stores++
	if *f.stores == f.failStoreAt {
		return nil, f.err
	}
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return nthFailEntityStore{EntityStore: es, f: f}, nil
}

func (s nthFailEntityStore) Get(ctx context.Context, id string) (*spi.Entity, error) {
	*s.f.gets++
	if *s.f.gets == s.f.failGetAt {
		return nil, s.f.err
	}
	return s.EntityStore.Get(ctx, id)
}

// The reads a SYNC dispatch makes around the callout to observe a callback's
// write, and the read before the save that precedes them, cannot be made: the
// run fails as an infrastructure failure.
func TestCallback_SyncObservationFails_RunFails(t *testing.T) {
	// In a one-hop run with one SYNC processor, the first EntityStore call and
	// the first Get are the fire's own read. The second of each is the store
	// and the read before the save; that read is also the view before the
	// dispatch, as the fired entity is unchanged and nothing is saved. The
	// third EntityStore call is the store for the read after the dispatch,
	// which is the third Get.
	cases := []struct {
		name                   string
		failStoreAt, failGetAt int
		wantDispatches         int
		wantErr                error
		wantText               string
	}{
		{"store before save", 2, 0, 0, ErrAnchorSaveInfra, "before a callout"},
		{"read before save", 0, 2, 0, ErrAnchorSaveInfra, "before a callout"},
		{"store before dispatch", 3, 0, 0, ErrProcessorOutputInfra, "before processor dispatch"},
		{"read after dispatch", 0, 3, 1, ErrProcessorOutputInfra, "after processor dispatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores, gets := 0, 0
			storeErr := errors.New("store unavailable")
			ext := &scriptedExtProc{}
			var env *runEnv
			env = newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
				return nthFailFactory{StoreFactory: f, stores: &stores, gets: &gets, err: storeErr}
			}, nil)
			claimed := env.claimed(t, "cbso-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))
			stores, gets = 0, 0
			env.engine.factory = cachedFactory(nthFailFactory{StoreFactory: env.factory.(nthFailFactory).StoreFactory,
				failStoreAt: tc.failStoreAt, failGetAt: tc.failGetAt, stores: &stores, gets: &gets, err: storeErr})

			r := runWithin(t, env, claimed)
			if r.Outcome != OutcomeFailed || !errors.Is(r.Err, storeErr) || !errors.Is(r.Err, tc.wantErr) ||
				!strings.Contains(r.Err.Error(), tc.wantText) {
				t.Fatalf("report = %+v, want failed %s, marked infrastructure", r, tc.wantText)
			}
			if n := ext.count("p1"); n != tc.wantDispatches {
				t.Errorf("p1 dispatched %d times, want %d", n, tc.wantDispatches)
			}
		})
	}
}

// enterAsCallback begins a callout of the transaction txID from the chain's
// context ctx and opens a joined write scope under it, as a compute node's
// callback is admitted. It returns the scope's context and a function that
// ends the scope and the callout, or the admission's refusal.
func enterAsCallback(env *runEnv, ctx context.Context, txID, calloutID string) (context.Context, func(), error) {
	coord := env.engine.coord
	_, endCallout := coord.BeginCallout(ctx, txID, calloutID)
	coord.Advance(calloutID, 1)
	joined, err := env.txMgr.Join(env.ctx, txID)
	if err != nil {
		endCallout()
		return nil, nil, err
	}
	sc, sctx, err := coord.Enter(joined, txID, []txcoord.Pair{{Callout: calloutID, Major: 1}}, txcoord.Write, 0)
	if err != nil {
		endCallout()
		return nil, nil, err
	}
	return sctx, func() {
		sc.End(nil)
		endCallout()
	}, nil
}

// fireArmsFunctionWF: OPEN --AutoClose--> DONE, and DONE arms Tick by the
// schedule function "tick".
func fireArmsFunctionWF() spi.WorkflowDefinition {
	return oneHopWF("DONE", nil, map[string]spi.StateDefinition{
		"DONE": {Transitions: []spi.TransitionDefinition{{Name: "Tick", Next: "OPEN",
			Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "tick", ResultKind: "Schedule", CalculationNodesTags: "sched"}}}}},
	})
}

func fireInAMinute() contract.FunctionResult {
	return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":60000}`)}
}

// A scheduled fire seals its transaction after its schedule functions: their
// callbacks are admitted and read the anchor the fire saved before the
// callout, in the target state.
func TestFireScheduled_ScheduleFunctionCallbackAdmitted(t *testing.T) {
	var env *runEnv
	var enterErr error
	var seen *spi.Entity
	var seenErr error
	var fireTxID string
	probe := &calloutProbe{function: func(ctx context.Context, entity *spi.Entity, txID string) (contract.FunctionResult, error) {
		fireTxID = txID
		sctx, end, err := enterAsCallback(env, ctx, txID, "fn-callout")
		enterErr = err
		if err == nil {
			seen, seenErr = readUnderToken(sctx, env.factory, entity.Meta.ID)
			end()
		}
		return fireInAMinute(), nil
	}}
	env = newRunEnv(t, probe)
	claimed := env.claimed(t, "fsfa-e1", fireArmsFunctionWF())

	r := runWithin(t, env, claimed)
	if enterErr != nil {
		t.Fatalf("the schedule function's callback was refused: %v", enterErr)
	}
	if seenErr != nil {
		t.Fatalf("the callback's read: %v", seenErr)
	}
	if seen.Meta.State != "DONE" || seen.Meta.TransactionID != fireTxID {
		t.Errorf("callback read state %q, transactionId %q; want DONE and the fire's %q", seen.Meta.State, seen.Meta.TransactionID, fireTxID)
	}
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	if got := env.state(t, "fsfa-e1"); got != "DONE" {
		t.Errorf("entity state = %q, want DONE", got)
	}
}

// A schedule function's callback deletes the entity the fire has just moved
// and re-armed: the run fails and nothing of it commits.
func TestFireScheduled_ScheduleFunctionCallbackDeletesAnchor_RunFails(t *testing.T) {
	var env *runEnv
	var enterErr, deleteErr error
	probe := &calloutProbe{function: func(ctx context.Context, entity *spi.Entity, txID string) (contract.FunctionResult, error) {
		sctx, end, err := enterAsCallback(env, ctx, txID, "fn-callout")
		enterErr = err
		if err == nil {
			deleteErr = deleteUnderToken(sctx, env.factory, entity.Meta.ID)
			end()
		}
		return fireInAMinute(), nil
	}}
	env = newRunEnv(t, probe)
	claimed := env.claimed(t, "fsfd-e1", fireArmsFunctionWF())

	r := runWithin(t, env, claimed)
	if enterErr != nil {
		t.Fatalf("the schedule function's callback was refused: %v", enterErr)
	}
	if deleteErr != nil {
		t.Fatalf("the callback's delete: %v", deleteErr)
	}
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, errFiredEntityDeletedByScheduleFunction) {
		t.Fatalf("report = %+v, want failed: a schedule function's callback deleted the fired entity", r)
	}
	if got := env.state(t, "fsfd-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN: nothing of the failed run may commit", got)
	}
	if got, found := env.task(t, claimed.ID); !found || got.ArmToken != claimed.ArmToken {
		t.Errorf("task = %+v (found %v), want the claimed life: the run's removal of it must roll back", got, found)
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "fsfd-e1", spi.SMEventScheduledTransitionFired); n != 0 {
		t.Errorf("FIRE events = %d, want 0", n)
	}
}

// casRecordingFactory records the precondition and the transaction of every
// EntityStore.CompareAndSave.
type casRecordingFactory struct {
	spi.StoreFactory
	cas *[]casCall
}

type casCall struct{ expected, txID string }

type casRecordingEntityStore struct {
	spi.EntityStore
	cas *[]casCall
}

func (f casRecordingFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return casRecordingEntityStore{EntityStore: es, cas: f.cas}, nil
}

func (s casRecordingEntityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	call := casCall{expected: expectedTxID}
	if tx := spi.GetTransaction(ctx); tx != nil {
		call.txID = tx.ID
	}
	*s.cas = append(*s.cas, call)
	return s.EntityStore.CompareAndSave(ctx, entity, expectedTxID)
}

// A fire whose target state arms a function schedule has written the anchor
// in its transaction by the time of its final persist (the save before the
// schedule function): the final CompareAndSave names this transaction.
func TestFireScheduled_ArmsFunctionSchedule_FinalCASNamesThisTx(t *testing.T) {
	var cas []casCall
	ext := &scriptedExtProc{function: func(context.Context) (contract.FunctionResult, error) {
		return fireInAMinute(), nil
	}}
	env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
		return casRecordingFactory{StoreFactory: f, cas: &cas}
	}, nil)
	claimed := env.claimed(t, "fsfc-e1", fireArmsFunctionWF())
	cas = nil

	r := runWithin(t, env, claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	if len(cas) == 0 {
		t.Fatal("the fire made no CompareAndSave")
	}
	final := cas[len(cas)-1]
	if final.txID == "" || final.expected != final.txID {
		t.Errorf("final CompareAndSave expects %q in transaction %q; want this transaction's id", final.expected, final.txID)
	}
}
