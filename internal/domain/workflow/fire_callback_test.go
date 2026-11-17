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
	return startRun(env, task)(t)
}

// startRun starts the run on a goroutine and returns a wait for its report.
func startRun(env *runEnv, task spi.ScheduledTask) func(t *testing.T) RunReport {
	done := make(chan RunReport, 1)
	go func() { r, _ := env.run(task); done <- r }()
	return func(t *testing.T) RunReport {
		t.Helper()
		select {
		case r := <-done:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("the run hung")
			return RunReport{}
		}
	}
}

// heldRun starts the run with its processor held, lets rival commit in a
// transaction of its own while the processor runs, then releases the
// processor and returns the run's report.
func heldRun(t *testing.T, entityID string, rival func(env *runEnv, task spi.ScheduledTask)) (*runEnv, spi.ScheduledTask, RunReport) {
	t.Helper()
	held := newHeldStub(t)
	env := newRunEnv(t, &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		held.hold()
		return nil, nil
	}})
	claimed := env.claimed(t, entityID, oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))
	wait := startRun(env, claimed)
	held.awaitRunning(t)
	rival(env, claimed)
	held.releaseNow()
	return env, claimed, wait(t)
}

// A delete committed by another transaction while the run is open: the run's
// snapshot still sees the entity, and the final persist conflicts. Only the
// entity is deleted here, not its task, so the run is not superseded and the
// conflict is reported as it is.
func TestCallback_OtherTransactionDeletesFiredEntity_RunConflicts(t *testing.T) {
	env, claimed, r := heldRun(t, "cbo-e1", func(env *runEnv, task spi.ScheduledTask) {
		rivalDelete(t, env.factory, env.txMgr, env.ctx, task.EntityID)
	})
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

// A write of the fired entity committed by another transaction while the run
// is open makes the run conflict: nothing of it commits, and the other write
// stands.
func TestCallback_OtherTransactionWritesFiredEntity_RunConflicts(t *testing.T) {
	env, claimed, r := heldRun(t, "cbow-e1", func(env *runEnv, task spi.ScheduledTask) {
		rivalSave(t, env.factory, env.txMgr, env.ctx, task.EntityID, []byte(`{"by":"other"}`))
	})
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

// A failed read before the final persist is a failed run: nothing commits.
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

// The store or the read before a SYNC dispatch cannot be had: the run fails as
// an infrastructure failure and nothing is dispatched.
func TestCallback_SyncPreCalloutReadFails_RunFails(t *testing.T) {
	// In a one-hop run with one SYNC processor, the first EntityStore call and
	// the first Get are the fire's own read. The second of each is the store
	// and the read of the save before the callout.
	cases := []struct {
		name                   string
		failStoreAt, failGetAt int
		wantDispatches         int
		wantErr                error
		wantText               string
	}{
		{"store before save", 2, 0, 0, ErrAnchorSaveInfra, "before a callout"},
		{"read before save", 0, 2, 0, ErrAnchorSaveInfra, "before a callout"},
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
// callbacks are admitted, read the anchor the fire saved before the callout,
// in the target state, and their writes of other entities commit with the
// fire.
func TestFireScheduled_ScheduleFunctionCallbackAdmitted(t *testing.T) {
	var env *runEnv
	var enterErr, writeErr error
	var seen *spi.Entity
	var seenErr error
	var fireTxID string
	probe := &calloutProbe{function: func(ctx context.Context, entity *spi.Entity, txID string) (contract.FunctionResult, error) {
		fireTxID = txID
		sctx, end, err := enterAsCallback(env, ctx, txID, "fn-callout")
		enterErr = err
		if err == nil {
			seen, seenErr = readUnderToken(sctx, env.factory, entity.Meta.ID)
			writeErr = saveUnderToken(sctx, env.factory, makeEntity("fsfa-other", entity.Meta.ModelRef, map[string]any{"by": "callback"}))
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
	if writeErr != nil {
		t.Fatalf("the callback's write of another entity: %v", writeErr)
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
	other, err := readUnderToken(env.ctx, env.factory, "fsfa-other")
	if err != nil {
		t.Fatalf("the callback's entity after the fire: %v", err)
	}
	if other.Meta.TransactionID != fireTxID {
		t.Errorf("the callback's entity carries transactionId %q, want the fire's %q", other.Meta.TransactionID, fireTxID)
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
