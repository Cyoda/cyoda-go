package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
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
// begin under the callback's pairs, advance, wait for the compute node, end.
//
// succeedAfterRelease decides what the compute node finally says, and it is the
// difference between a test with teeth and one without. When it is false the
// callout comes back with the refusal, and the chain would stop on that error
// alone — every check in the engine could be deleted and the chain would still
// fail. When it is true the compute node ANSWERS SUCCESSFULLY: it did its work
// and never learned it had been replaced, so nothing in its answer tells the
// engine to stop. Only the fence's check does, and that is the race the checks
// exist for.
type innerCoordinator struct {
	f                   *fence.Fence
	entered             chan struct{} // closed when the first callout is waiting
	proceed             chan struct{} // the released callout returns only once this is closed
	n                   atomic.Int32
	succeedAfterRelease bool // set before any goroutine starts; never written afterwards
}

func (c *innerCoordinator) run(ctx context.Context, txID string) error {
	id := "inner-" + strconv.Itoa(int(c.n.Add(1)))
	cctx, end := c.f.Begin(ctx, id, txID, fence.Pairs(ctx))
	defer end()
	c.f.Advance(id, 1)
	if c.n.Load() == 1 {
		close(c.entered)
	}
	<-cctx.Done()
	<-c.proceed
	if c.succeedAfterRelease {
		return nil
	}
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return cctx.Err()
}

func (c *innerCoordinator) DispatchProcessor(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
	if err := c.run(ctx, txID); err != nil {
		return nil, err
	}
	// Data comes back, so without its check executeSyncProcessor would go on to
	// applyProcessorData — a model read, and possibly a model extension.
	return e, nil
}

func (c *innerCoordinator) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, txID string) (bool, string, error) {
	if err := c.run(ctx, txID); err != nil {
		return false, "", err
	}
	// Matched, so without its check evaluateCriterion would let the transition
	// fire and the chain carry the entity on to the next state.
	return true, "", nil
}

func (c *innerCoordinator) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, txID string) (contract.FunctionResult, error) {
	if err := c.run(ctx, txID); err != nil {
		return contract.FunctionResult{}, err
	}
	// A schedule that resolves and is not born expired, so without its check
	// armViaFunction would hand a task to ReconcileForEntity and its audit row.
	return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":60000}`)}, nil
}

// chainKey carries, on a callback's context, the fencing major its pass was
// admitted under. Absent from the test's own setup contexts and from the
// owner's chain.
type chainKey struct{}

// supersededOps counts the store operations — audit writes, entity and model
// reads, the scheduled-task reconcile, savepoint work — made by a chain the
// test has already superseded.
//
// Its predicate is deliberately the test's OWN bookkeeping: the major it put on
// the callback's context, against the major it made current when it superseded
// that callback. It never asks fence.Check. A Check that wrongly answered "still
// current" would otherwise silence this counter at the same moment it silenced
// the production guard, and the test would pass while the bug it exists for was
// live.
type supersededOps struct {
	current atomic.Uint32 // the major the owner has given the work to
	n       atomic.Int32
}

func (r *supersededOps) note(ctx context.Context) {
	major, ok := ctx.Value(chainKey{}).(uint32)
	if !ok {
		return
	}
	if cur := r.current.Load(); cur != 0 && major < cur {
		r.n.Add(1)
	}
}

type watchedFactory struct {
	spi.StoreFactory
	ops *supersededOps
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
	ops *supersededOps
}

func (w watchedAudit) Record(ctx context.Context, id string, ev spi.StateMachineEvent) error {
	w.ops.note(ctx)
	return w.StateMachineAuditStore.Record(ctx, id, ev)
}

type watchedTxMgr struct {
	spi.TransactionManager
	ops      *supersededOps
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
	ops     *supersededOps
	txID    string
	txCtx   context.Context
}

func newSupersedeEnv(t *testing.T, succeedAfterRelease bool) *supersedeEnv {
	t.Helper()
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { mem.Close() })
	uuids := common.NewTestUUIDGenerator()
	ops := &supersededOps{}
	txMgr := &watchedTxMgr{TransactionManager: mem.NewTransactionManager(uuids), ops: ops}
	gate := txgate.New()
	f := fence.New(gate)
	ext := &innerCoordinator{f: f, entered: make(chan struct{}), proceed: make(chan struct{}), succeedAfterRelease: succeedAfterRelease}
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
// The major also travels as a plain context value for the store-op counter,
// which must not depend on the fence to know whose chain it is watching.
func (env *supersedeEnv) callback(t *testing.T, major uint32, fn func(ctx context.Context) error) error {
	t.Helper()
	joined, err := env.txMgr.Join(env.base, env.txID)
	if err != nil {
		// This runs on the callback's goroutine, where FailNow would only kill
		// that goroutine and leave the test to time out instead of reporting.
		t.Errorf("Join: %v", err)
		return err
	}
	joined = context.WithValue(joined, chainKey{}, major)
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

// supersede is the owner giving the work to another compute node. The counter's
// own record of the current major is set BEFORE the fence's, so no operation of
// the chain about to be superseded can slip past it unnoticed.
func (env *supersedeEnv) supersede(major uint32) {
	env.ops.current.Store(major)
	env.f.Advance("outer", major)
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

// supersedeShape is one place the engine can be waiting in a callout of the
// callback's own, and the check that must refuse the chain when it comes back.
type supersedeShape struct {
	name       string
	transition spi.TransitionDefinition
	wfCrit     json.RawMessage
	guardedBy  string
}

func supersedeShapes() []supersedeShape {
	fnCriterion, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "crit", "config": map[string]any{"calculationNodesTags": "t"}}})
	proc := func(mode string) []spi.ProcessorDefinition {
		return []spi.ProcessorDefinition{{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: mode}}
	}
	return []supersedeShape{
		{name: "SYNC processor", guardedBy: "executeSyncProcessor's check",
			transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeSync)}},
		{name: "ASYNC_SAME_TX processor", guardedBy: "executeSyncProcessor's check",
			transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeAsyncSameTx)}},
		{name: "ASYNC_NEW_TX as the LAST processor", guardedBy: "executeAsyncNewTx's check, before the savepoint is looked at",
			transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeAsyncNewTx)}},
		{name: "transition FUNCTION criterion", guardedBy: "evaluateCriterion's check",
			transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Criterion: fnCriterion}},
		{name: "workflow FUNCTION criterion", guardedBy: "evaluateCriterion's check",
			transition: spi.TransitionDefinition{Name: "T", Next: "DONE"}, wfCrit: fnCriterion},
		{name: "arming function", guardedBy: "armViaFunction's check",
			transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "when", CalculationNodesTags: "t"}}}},
	}
}

// runSupersedeScenario plays one shape. The callback's chain is inside a callout
// of its own; the owner gives the work to another compute node while it waits;
// the callout then comes back. Whatever it comes back with, the chain must be
// answered 410 and must touch no store from the moment of the supersede on.
// It is deliberately NOT a t.Helper: it is the body of the test, and marking it
// one would attribute every failure to the one-line subtest closure instead of
// to the assertion that fired.
func runSupersedeScenario(t *testing.T, tc supersedeShape, calloutSucceeds bool) {
	env := newSupersedeEnv(t, calloutSucceeds)
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
	env.supersede(2) // the owner gives the work to another compute node
	close(env.ext.proceed)

	select {
	case err := <-done:
		assertSuperseded(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the callback was not released")
	}
	if state == "DONE" {
		t.Fatalf("the superseded chain carried the entity on to the next state; %s did not refuse it", tc.guardedBy)
	}
	if got := env.ops.n.Load(); got != 0 {
		t.Fatalf("the superseded chain made %d store operations (audit rows included); want none — %s did not refuse it in time", got, tc.guardedBy)
	}
	if env.ext.n.Load() != 1 {
		t.Fatalf("%d callouts were made; the chain must stop at the first", env.ext.n.Load())
	}
}

// The callout comes back with the refusal — the compute node was released by
// the fence and reported CALLOUT_SUPERSEDED. The chain stops on that error, and
// nothing it unwinds through touches a store.
func TestSupersededChain_CalloutRefused_TouchesNoStore(t *testing.T) {
	for _, tc := range supersedeShapes() {
		t.Run(tc.name, func(t *testing.T) { runSupersedeScenario(t, tc, false) })
	}
}

// The race the checks exist for: the compute node finished its work and ANSWERED
// SUCCESSFULLY, never learning it had been replaced while the chain waited.
// Nothing in that answer tells the engine to stop — only the check after the
// chain re-takes the transaction's lock does. Without it the chain would apply
// the processor's data, let the transition fire, release its savepoint or arm a
// scheduled task on a transaction the owner has already handed on.
func TestSupersededChain_CalloutSucceeded_StillRefusedAndTouchesNoStore(t *testing.T) {
	for _, tc := range supersedeShapes() {
		t.Run(tc.name, func(t *testing.T) { runSupersedeScenario(t, tc, true) })
	}
}

// In ASYNC_NEW_TX the superseded chain neither undoes nor releases its
// savepoint: what the replacement compute node wrote meanwhile is kept. True
// whether the callout came back refused (the undo path) or successful (the
// release path).
func TestSupersededChain_AsyncNewTx_LeavesItsSavepointAlone(t *testing.T) {
	for _, calloutSucceeds := range []bool{false, true} {
		name := "callout refused"
		if calloutSucceeds {
			name = "callout succeeded"
		}
		t.Run(name, func(t *testing.T) {
			env := newSupersedeEnv(t, calloutSucceeds)
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
			env.supersede(2)

			// The replacement compute node writes while the superseded chain is
			// still waiting. Undoing the savepoint would take this with it.
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
			if got := env.ops.n.Load(); got != 0 {
				t.Fatalf("the superseded chain made %d store operations; want none", got)
			}
			if err := env.txMgr.Commit(env.txCtx, env.txID); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			es, _ := env.factory.EntityStore(env.base)
			if _, err := es.Get(env.base, keptID); err != nil {
				t.Fatalf("the replacement compute node's write was lost: %v", err)
			}
		})
	}
}
