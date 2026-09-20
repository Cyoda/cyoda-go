package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// innerCoordinator is what a Coordinator does as far as the fence is concerned:
// begin under the callback's pairs, advance, wait for the cnode — which never
// answers — until released, end.
type innerCoordinator struct {
	f       *fence.Fence
	entered chan struct{} // closed when the first callout is waiting
	proceed chan struct{} // the released callout returns only once this is closed
	n       atomic.Int32
}

func (c *innerCoordinator) run(ctx context.Context, txID string) error {
	id := "inner-" + string(rune('0'+c.n.Add(1)))
	cctx, end := c.f.Begin(ctx, id, txID, fence.Pairs(ctx))
	defer end()
	c.f.Advance(id, 1)
	if c.n.Load() == 1 {
		close(c.entered)
	}
	<-cctx.Done()
	<-c.proceed
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return cctx.Err()
}

func (c *innerCoordinator) DispatchProcessor(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
	return e, c.run(ctx, txID)
}

func (c *innerCoordinator) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, txID string) (bool, string, error) {
	return true, "", c.run(ctx, txID)
}

func (c *innerCoordinator) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, txID string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, c.run(ctx, txID)
}

// refusedOps counts every audit write, entity store operation, scheduled-task
// reconcile and savepoint operation made on a context the fence refuses.
type refusedOps struct{ n atomic.Int32 }

func (r *refusedOps) note(ctx context.Context) {
	if fence.Check(ctx) != nil {
		r.n.Add(1)
	}
}

type watchedFactory struct {
	spi.StoreFactory
	ops *refusedOps
}

func (w watchedFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	s, err := w.StoreFactory.StateMachineAuditStore(ctx)
	return watchedAudit{s, w.ops}, err
}

func (w watchedFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	w.ops.note(ctx)
	return w.StoreFactory.EntityStore(ctx)
}

func (w watchedFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	w.ops.note(ctx)
	return w.StoreFactory.ModelStore(ctx)
}

func (w watchedFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	w.ops.note(ctx)
	return w.StoreFactory.ScheduledTaskStore(ctx)
}

type watchedAudit struct {
	spi.StateMachineAuditStore
	ops *refusedOps
}

func (w watchedAudit) Record(ctx context.Context, id string, ev spi.StateMachineEvent) error {
	w.ops.note(ctx)
	return w.StateMachineAuditStore.Record(ctx, id, ev)
}

type watchedTxMgr struct {
	spi.TransactionManager
	ops      *refusedOps
	undone   atomic.Int32
	released atomic.Int32
}

func (w *watchedTxMgr) Savepoint(ctx context.Context, txID string) (string, error) {
	w.ops.note(ctx)
	return w.TransactionManager.Savepoint(ctx, txID)
}

func (w *watchedTxMgr) RollbackToSavepoint(ctx context.Context, txID, sp string) error {
	w.ops.note(ctx)
	w.undone.Add(1)
	return w.TransactionManager.RollbackToSavepoint(ctx, txID, sp)
}

func (w *watchedTxMgr) ReleaseSavepoint(ctx context.Context, txID, sp string) error {
	w.ops.note(ctx)
	w.released.Add(1)
	return w.TransactionManager.ReleaseSavepoint(ctx, txID, sp)
}

type supersedeEnv struct {
	base    context.Context
	factory spi.StoreFactory
	txMgr   *watchedTxMgr
	gate    *txgate.Registry
	f       *fence.Fence
	ext     *innerCoordinator
	engine  *Engine
	ops     *refusedOps
	txID    string
	txCtx   context.Context
}

func newSupersedeEnv(t *testing.T) *supersedeEnv {
	t.Helper()
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { mem.Close() })
	uuids := common.NewTestUUIDGenerator()
	ops := &refusedOps{}
	txMgr := &watchedTxMgr{TransactionManager: mem.NewTransactionManager(uuids), ops: ops}
	gate := txgate.New()
	f := fence.New(gate)
	ext := &innerCoordinator{f: f, entered: make(chan struct{}), proceed: make(chan struct{})}
	factory := watchedFactory{StoreFactory: mem, ops: ops}
	env := &supersedeEnv{base: ctxWithTenant(testTenant), factory: factory, txMgr: txMgr, gate: gate, f: f, ext: ext, ops: ops,
		engine: NewEngine(factory, uuids, txMgr, WithExternalProcessing(ext))}
	var err error
	env.txID, env.txCtx, err = txMgr.Begin(env.base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return env
}

// callback runs fn as the join layer runs a callback of callout "outer" at
// major: joined, admitted, under the lock with the suspendable handle, checked.
func (env *supersedeEnv) callback(t *testing.T, major uint32, fn func(ctx context.Context) error) error {
	t.Helper()
	joined, err := env.txMgr.Join(env.base, env.txID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	admitted, err := env.f.Admit(joined, []fence.Pair{{Callout: "outer", Major: major}})
	if err != nil {
		return err
	}
	release := env.gate.Acquire(env.txID)
	defer func() { release() }()
	admitted, _ = txgate.WithHeld(admitted, env.gate, env.txID, &release)
	if err := fence.Check(admitted); err != nil {
		return err
	}
	return fn(admitted)
}

func assertSuperseded(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != 410 || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want 410 CALLOUT_SUPERSEDED (the callback must be answered 410, not 200)", err)
	}
	if !errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("the engine's wraps must keep the refusal findable: %v", err)
	}
}

// A callback waiting on a callout of its own when its cnode is replaced: it is
// released, the inner callout ends, the chain is refused on re-taking the lock,
// and nothing further touches a store — in every shape the engine can be
// waiting in.
func TestSupersededChain_TouchesNoStore(t *testing.T) {
	fnCriterion, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "crit", "config": map[string]any{"calculationNodesTags": "t"}}})
	proc := func(mode string) []spi.ProcessorDefinition {
		return []spi.ProcessorDefinition{{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: mode}}
	}
	tests := []struct {
		name       string
		transition spi.TransitionDefinition
		wfCrit     json.RawMessage
	}{
		{name: "SYNC processor", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeSync)}},
		{name: "ASYNC_SAME_TX processor", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeAsyncSameTx)}},
		{name: "ASYNC_NEW_TX as the LAST processor", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeAsyncNewTx)}},
		{name: "transition FUNCTION criterion", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Criterion: fnCriterion}},
		{name: "workflow FUNCTION criterion", transition: spi.TransitionDefinition{Name: "T", Next: "DONE"}, wfCrit: fnCriterion},
		{name: "arming function", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "when", CalculationNodesTags: "t"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newSupersedeEnv(t)
			modelRef := spi.ModelRef{EntityName: "superseded", ModelVersion: "1.0"}
			saveWorkflow(t, env.factory, env.base, modelRef, []spi.WorkflowDefinition{{
				Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true, Criterion: tc.wfCrit,
				States: map[string]spi.StateDefinition{"INITIAL": {Transitions: []spi.TransitionDefinition{tc.transition}}, "DONE": {}},
			}})

			_, endOuter := env.f.Begin(env.base, "outer", env.txID, nil)
			defer endOuter()
			env.f.Advance("outer", 1)

			var state string
			done := make(chan error, 1)
			go func() {
				done <- env.callback(t, 1, func(ctx context.Context) error {
					entity := makeEntity("child-1", modelRef, map[string]any{"x": 1})
					entity.Meta.TransactionID = env.txID
					_, err := env.engine.Execute(ctx, entity, "")
					state = entity.Meta.State
					return err
				})
			}()

			<-env.ext.entered
			env.f.Advance("outer", 2) // the owner gives the work to another cnode
			before := env.ops.n.Load()
			close(env.ext.proceed)

			select {
			case err := <-done:
				assertSuperseded(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the callback was not released")
			}
			if state == "DONE" {
				t.Fatal("the superseded chain carried the entity on to the next state")
			}
			if got := env.ops.n.Load() - before; got != 0 {
				t.Fatalf("the refused chain made %d store operations (audit rows included); want none", got)
			}
			if env.ext.n.Load() != 1 {
				t.Fatalf("%d callouts were made; the chain must stop at the first", env.ext.n.Load())
			}
		})
	}
}

// In ASYNC_NEW_TX the superseded chain neither undoes nor releases its
// savepoint: what the replacement cnode wrote meanwhile is kept.
func TestSupersededChain_AsyncNewTx_LeavesItsSavepointAlone(t *testing.T) {
	env := newSupersedeEnv(t)
	modelRef := spi.ModelRef{EntityName: "superseded-sp", ModelVersion: "1.0"}
	saveWorkflow(t, env.factory, env.base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "T", Next: "DONE", Processors: []spi.ProcessorDefinition{
				{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeAsyncNewTx},
				{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeAsyncNewTx}, // must never be dispatched
			}}}},
			"DONE": {},
		},
	}})
	_, endOuter := env.f.Begin(env.base, "outer", env.txID, nil)
	defer endOuter()
	env.f.Advance("outer", 1)

	done := make(chan error, 1)
	go func() {
		done <- env.callback(t, 1, func(ctx context.Context) error {
			entity := makeEntity("child-sp", modelRef, map[string]any{"x": 1})
			entity.Meta.TransactionID = env.txID
			_, err := env.engine.Execute(ctx, entity, "")
			return err
		})
	}()
	<-env.ext.entered
	env.f.Advance("outer", 2)

	// The replacement cnode writes while the superseded chain is still waiting.
	const keptID = "replacement-write"
	if err := env.callback(t, 2, func(ctx context.Context) error {
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			return err
		}
		kept := makeEntity(keptID, modelRef, map[string]any{"kept": true})
		kept.Meta.TransactionID = env.txID
		_, err = es.Save(ctx, kept)
		return err
	}); err != nil {
		t.Fatalf("replacement write: %v", err)
	}

	close(env.ext.proceed)
	assertSuperseded(t, <-done)
	if u, r := env.txMgr.undone.Load(), env.txMgr.released.Load(); u != 0 || r != 0 {
		t.Fatalf("savepoint undone %d times, released %d times; a refused chain touches neither", u, r)
	}
	if err := env.txMgr.Commit(env.txCtx, env.txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, _ := env.factory.EntityStore(env.base)
	if _, err := es.Get(env.base, keptID); err != nil {
		t.Fatalf("the replacement cnode's write was lost: %v", err)
	}
}
