package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const (
	testMaxLostOwners = 3
	testRetryDelay    = 30 * time.Second
	runStartMs        = int64(1_700_000_000_000)
)

var (
	testOwner  = uuid.MustParse("00000000-0000-4000-8000-00000000a001")
	otherOwner = uuid.MustParse("00000000-0000-4000-8000-00000000b002")
)

func taskStore(t *testing.T, factory spi.StoreFactory, ctx context.Context) spi.ScheduledTaskStore {
	t.Helper()
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	return sts
}

// armable keeps only the fields an arm sets; the store draws the rest.
func armable(task spi.ScheduledTask) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID: task.ID, TenantID: task.TenantID, Type: task.Type, ScheduledTime: task.ScheduledTime,
		TimeoutMs: task.TimeoutMs, EntityID: task.EntityID, ModelName: task.ModelName,
		ModelVersion: task.ModelVersion, Transition: task.Transition, SourceState: task.SourceState,
		ArmedAt: task.ArmedAt, ArmedBy: task.ArmedBy,
	}
}

// armTask arms task as a new life, as an entity write does, and keeps the
// entity's other tasks (re-armed as new lives too). No transaction on ctx:
// the store applies it at once. Returns the stored record.
func armTask(t *testing.T, factory spi.StoreFactory, ctx context.Context, task spi.ScheduledTask) spi.ScheduledTask {
	t.Helper()
	sts := taskStore(t, factory, ctx)
	page, err := sts.Query(ctx, task.TenantID, spi.ScheduledTaskQuery{EntityID: task.EntityID, Limit: 1000})
	if err != nil {
		t.Fatalf("Query tasks: %v", err)
	}
	arm := []spi.ScheduledTask{armable(task)}
	for _, existing := range page.Items {
		if existing.ID != task.ID {
			arm = append(arm, armable(existing))
		}
	}
	if _, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: task.TenantID, EntityID: task.EntityID, CurrentState: task.SourceState, Arm: arm,
	}); err != nil {
		t.Fatalf("arm task: %v", err)
	}
	got, found, err := sts.Get(ctx, task.TenantID, task.ID)
	if err != nil || !found {
		t.Fatalf("armed task not readable: found=%v err=%v", found, err)
	}
	return *got
}

func getTask(t *testing.T, factory spi.StoreFactory, ctx context.Context, id string) (*spi.ScheduledTask, bool) {
	t.Helper()
	task, found, err := taskStore(t, factory, ctx).Get(ctx, testTenant, id)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	return task, found
}

func claimRequest(owner uuid.UUID, nowMs int64, allowLostOwner bool) spi.ClaimRequest {
	return spi.ClaimRequest{
		Owner: owner, NowMs: nowMs, StaleAfter: time.Minute,
		Limit: 8, PerTenantLimit: 8, AllowLostOwner: allowLostOwner,
	}
}

// testRun is one run with its guard and a handle that closes Done.
type testRun struct {
	guard *RunGuard
	done  chan struct{}
	once  sync.Once
}

func newTestRun(sts spi.ScheduledTaskStore, task spi.ScheduledTask) *testRun {
	done := make(chan struct{})
	return &testRun{done: done, guard: &RunGuard{
		Ref:    spi.TaskRef{TenantID: task.TenantID, ID: task.ID, ArmToken: task.ArmToken, ClaimToken: task.Claim.Token},
		Store:  sts,
		Done:   done,
		Unsafe: &UnsafeFlight{},
	}}
}

func (r *testRun) cancel() { r.once.Do(func() { close(r.done) }) }

func (r *testRun) fire(engine *Engine, ctx context.Context, task spi.ScheduledTask) RunReport {
	return engine.FireScheduledTransition(WithRunGuard(ctx, r.guard), task, testMaxLostOwners, testRetryDelay)
}

// fireDue claims the due task id for testOwner at the engine's clock and runs it.
func fireDue(t *testing.T, engine *Engine, ctx context.Context, id string) RunReport {
	t.Helper()
	sts := taskStore(t, engine.factory, ctx)
	claimed, err := sts.ClaimDue(ctx, claimRequest(testOwner, engine.now().UnixMilli(), false))
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	for _, task := range claimed {
		if task.ID == id {
			return newTestRun(sts, task).fire(engine, ctx, task)
		}
	}
	t.Fatalf("task %s was not claimed; ClaimDue returned %d tasks", id, len(claimed))
	return RunReport{}
}

// runEnv is one memory-backed engine with a steppable clock.
type runEnv struct {
	engine  *Engine
	factory spi.StoreFactory       // what the engine uses
	sts     spi.ScheduledTaskStore // from factory
	txMgr   spi.TransactionManager // the memory manager, never wrapped
	ctx     context.Context
	advance func(deltaMs int64)
}

func newRunEnv(t *testing.T, ext contract.ExternalProcessingService) *runEnv {
	return newRunEnvWith(t, ext, nil, nil)
}

func newRunEnvWith(t *testing.T, ext contract.ExternalProcessingService,
	wrapFactory func(spi.StoreFactory) spi.StoreFactory,
	wrapTx func(spi.TransactionManager) spi.TransactionManager) *runEnv {
	t.Helper()
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { mem.Close() })
	var factory spi.StoreFactory = mem
	if wrapFactory != nil {
		factory = wrapFactory(mem)
	}
	uuids := common.NewTestUUIDGenerator()
	txMgr := mem.NewTransactionManager(uuids)
	var engineTx spi.TransactionManager = txMgr
	if wrapTx != nil {
		engineTx = wrapTx(txMgr)
	}
	clock, advance := steppableClock(runStartMs)
	opts := []EngineOption{WithScheduledClock(clock)}
	if ext != nil {
		opts = append(opts, WithExternalProcessing(ext))
	}
	engine := NewEngine(factory, uuids, engineTx, opts...)
	ctx := ctxWithTenant(testTenant)
	return &runEnv{engine: engine, factory: factory, sts: taskStore(t, factory, ctx), txMgr: txMgr, ctx: ctx, advance: advance}
}

func (env *runEnv) nowMs() int64 { return env.engine.now().UnixMilli() }

// setup saves wf for a model named after entityID, seeds the entity in OPEN
// and arms OPEN's AutoClose due now. Returns the armed record.
func (env *runEnv) setup(t *testing.T, entityID string, wf spi.WorkflowDefinition) spi.ScheduledTask {
	t.Helper()
	model := spi.ModelRef{EntityName: "run-" + entityID, ModelVersion: "1.0"}
	saveWorkflow(t, env.factory, env.ctx, model, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, env.factory, env.ctx, entityID, model, "OPEN", "seed-tx-1", map[string]any{})
	now := env.nowMs()
	return armTask(t, env.factory, env.ctx, spi.ScheduledTask{
		ID: taskID(testTenant, entityID, "OPEN", "AutoClose"), TenantID: testTenant,
		Type: spi.ScheduledTaskFireTransition, ScheduledTime: now, EntityID: entityID,
		ModelName: model.EntityName, ModelVersion: 1, Transition: "AutoClose", SourceState: "OPEN",
		ArmedAt: now,
	})
}

// claimed is setup followed by a claim for testOwner.
func (env *runEnv) claimed(t *testing.T, entityID string, wf spi.WorkflowDefinition) spi.ScheduledTask {
	t.Helper()
	env.setup(t, entityID, wf)
	return env.claimOne(t, testOwner, false)
}

func (env *runEnv) claimOne(t *testing.T, owner uuid.UUID, allowLostOwner bool) spi.ScheduledTask {
	t.Helper()
	got, err := env.sts.ClaimDue(env.ctx, claimRequest(owner, env.nowMs(), allowLostOwner))
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ClaimDue claimed %d tasks, want 1", len(got))
	}
	return got[0]
}

func (env *runEnv) run(task spi.ScheduledTask) (RunReport, *testRun) {
	run := newTestRun(env.sts, task)
	return run.fire(env.engine, env.ctx, task), run
}

func (env *runEnv) task(t *testing.T, id string) (*spi.ScheduledTask, bool) {
	t.Helper()
	task, found, err := env.sts.Get(env.ctx, testTenant, id)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	return task, found
}

func (env *runEnv) state(t *testing.T, entityID string) string {
	t.Helper()
	return getEntityState(t, env.factory, env.ctx, entityID)
}

// exists reports whether the entity exists in committed state.
func (env *runEnv) exists(t *testing.T, entityID string) bool {
	t.Helper()
	es, err := env.factory.EntityStore(env.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = es.Get(env.ctx, entityID)
	if errors.Is(err, spi.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("Get entity %s: %v", entityID, err)
	}
	return true
}

// rearm re-arms task as a new life in a transaction of its own that commits,
// as a client write to the entity does.
func (env *runEnv) rearm(task spi.ScheduledTask) error {
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		return err
	}
	if _, err := env.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: task.TenantID, EntityID: task.EntityID, CurrentState: task.SourceState,
		Arm: []spi.ScheduledTask{armable(task)},
	}); err != nil {
		_ = env.txMgr.Rollback(env.ctx, txID)
		return err
	}
	return env.txMgr.Commit(env.ctx, txID)
}

// oneHopWF: OPEN --AutoClose (scheduled, processors)--> next, plus extra states.
func oneHopWF(next string, procs []spi.ProcessorDefinition, extra map[string]spi.StateDefinition) spi.WorkflowDefinition {
	states := map[string]spi.StateDefinition{
		"OPEN": {Transitions: []spi.TransitionDefinition{{
			Name: "AutoClose", Next: next, Processors: procs,
			Schedule: &spi.TransitionSchedule{DelayMs: 60_000},
		}}},
	}
	for name, st := range extra {
		states[name] = st
	}
	if _, ok := states[next]; !ok {
		states[next] = spi.StateDefinition{}
	}
	return spi.WorkflowDefinition{Version: "1.1", Name: "RunWF", InitialState: "OPEN", Active: true, States: states}
}

func autoStep(next string, procs ...spi.ProcessorDefinition) spi.StateDefinition {
	return spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "Step", Next: next, Processors: procs}}}
}

func unsafeProc(name, mode string) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: name, ExecutionMode: mode}
}

func safeProc(name, mode string) spi.ProcessorDefinition {
	p := unsafeProc(name, mode)
	p.Config.Idempotent = true
	return p
}

// scriptedExtProc counts dispatches and answers from scripts.
type scriptedExtProc struct {
	mu        sync.Mutex
	calls     map[string]int
	processor func(ctx context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error)
	criterion func(ctx context.Context) (bool, string, error)
	function  func(ctx context.Context) (contract.FunctionResult, error)
}

func (m *scriptedExtProc) record(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[name]++
}

func (m *scriptedExtProc) count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[name]
}

func (m *scriptedExtProc) DispatchProcessor(ctx context.Context, _ *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
	m.record(proc.Name)
	if m.processor == nil {
		return nil, nil
	}
	return m.processor(ctx, proc, txID)
}

func (m *scriptedExtProc) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	m.record("criterion")
	if m.criterion == nil {
		return true, "", nil
	}
	return m.criterion(ctx)
}

func (m *scriptedExtProc) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, _ string) (contract.FunctionResult, error) {
	m.record("function")
	if m.function == nil {
		return contract.FunctionResult{}, errors.New("no function scripted")
	}
	return m.function(ctx)
}
