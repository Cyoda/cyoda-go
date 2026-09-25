# Stream E — the engine: run guard, fire door, mark, stamp, cancellation, reconcile

Plan section for spec §4, §5.1–§5.5, §5.7 (engine part), §7 (reconcile and the
model-level flag) and the engine U-rows of §13. Package:
`internal/domain/workflow`.

**What was checked, and how.** Every `file:line` below was read in this
worktree at `ce9ff2bb`. The code blocks were written against that source and
against the binding names in `interfaces.md`; they were **not compiled**, because
the SPI of stream S and the memory store of stream BM do not exist yet. The
implementer fixes imports and helper names where the compiler says so, and does
not change behaviour.

**Who writes what.** The engine decides and commits the endings that commit
(fired, declined, expired, cancelled). It never writes `RecordAttempt` or
`Fail`. `Fail` and the `SCHEDULED_TRANSITION_FAIL` audit event (§5.7) are written
by the scheduler (stream R), in one transaction, from the `RunReport` this
stream returns. So the §13 rows "`SCHEDULED_TRANSITION_FAIL` recorded with its
reason", the `lastError` rows and the bookkeeping rows are R's.

## Order, and the seams with other streams

```
E-1 → E-2 → E-3 → E-4 → E-5 → E-6 → E-7
                                 │
                                 └── needs #599 merged into release/v0.9.0 (README "Prerequisite")
```

- E needs S (the SPI), BM (the memory store, which every test here runs on) and
  K (`contract.NoHandOffProof`, `contract.ProvesNoHandOff`, used from E-5 on).
- R consumes `RunGuard`, `WithRunGuard`, `RunReport`, the outcomes and
  `FireScheduledTransition` from E-1, and `NoNewUnsafe` / `UnsafeInFlight` from
  E-5.
- **Build.** The package `internal/domain/workflow` does not compile against the
  new SPI until E-1 lands (`fire_scheduled.go:106, 228` call the removed
  `Get(ctx, id)` and `Delete`). After E-1 the packages `app`, `internal/cluster`
  and `internal/scheduler` still do not compile: they call the old
  `FireScheduledTransition` (`internal/cluster/scheduler_rpc.go:58, 354`,
  `internal/scheduler/executor.go:46`) and `WithExpiryGrace` (`app/app.go:586`),
  and they already use the removed store methods. Stream R deletes or rewires
  them. Until R lands, E verifies with package-scoped `go test` only.

## Tests: store and fixtures

The existing tests use the memory plugin (`engine_test.go:22-30`,
`arm_test.go:48-57`) and wrap its stores for fault injection
(`fire_scheduled_test.go:984-1031`, `workflow_selection_test.go:830-854`). This
stream does the same. What it relies on from BM, beyond the §10.1 contract:

- a joining method called with no transaction on `ctx` applies at once — as
  `plugins/memory/scheduled_task_store.go:75-81` does today;
- `ClaimDue` with `AllowLostOwner` treats an owner with no liveness record as
  stale (spec §6.1: "missing or older");
- a task row staged by an open transaction makes `MarkUnsafe` answer
  `ErrTaskBusy` (C6), and a joining `Get` or fenced write sees the staged
  operations (C2).

## Coverage: the engine U-rows of spec §13

Rows not listed are not the engine's; the owner is named where a row is split.

| §13 row | Test (task) |
|---|---|
| fired on time | `TestFireScheduled_FiresOnTime` (E-1, ported) |
| fired after one safe failure (compute node down, then up) | `TestUnsafeReached_NoComputeNode_ThenFires` (E-5) |
| self-loop fires and re-arms the same id as a new life | `TestFireScheduled_SelfLoopReArmsSameIDAsNewLife` (E-1) |
| declined | `TestFireScheduled_DeclineOnCriterionFalse` (E-1, ported) |
| expired, late on the first attempt | `TestFireScheduled_ExpiredOnFirstAttempt` (E-1) |
| criterion error → WAITING, attempts 1, error recorded | `TestFireScheduled_CriterionErrorIsASafeFailure` (E-1); WAITING and the error text are R's bookkeeping |
| no compute node → `NotHandedOff`, mark cleared, WAITING | `TestUnsafeReached_Fact/no_member_proof_resets`, `TestUnsafeReached_NoComputeNode_ThenFires` (E-5); the clear is R's `RecordAttempt{ClearOwnMark}` |
| idempotent processor fails → WAITING | `TestUnsafeMark_IdempotentProcessorIsNotMarked` (E-4) |
| retry delay …; runs up to `RETRY_DELAY` past it; later → FAILED | `TestPreRunDecision` (E-1) for the §5.1 half; doubling, saturation and clamp are R's |
| late after failed attempts → FAILED | `TestPreRunDecision`, `TestFireScheduled_LateAfterFailedAttempt` (E-1) |
| unsafe processor fails → FAILED, never re-run | `TestUnsafeReached_Fact/error_without_proof_stays` (E-5), `TestUnsafeMark_MarkedTaskIsNeverRerun` (E-5) |
| a later step fails after an unsafe hand-off → FAILED | `TestUnsafeReached_Fact/later_proof_does_not_reset` (E-5) |
| failure after a successful unsafe dispatch in the same step → FAILED | `TestUnsafeReached_Fact/apply_fails_after_successful_dispatch` (E-5) |
| savepoint error replaces the dispatch error → FAILED | `TestUnsafeReached_SavepointErrorReplacesProof` (E-5) |
| cancelled before `Send` returned nil → WAITING | `TestUnsafeReached_Fact/cancelled_before_send` (E-5) |
| cancelled after `Send` returned nil → FAILED | `TestUnsafeReached_Fact/cancelled_after_send` (E-5) |
| database outage during `MarkUnsafe` → `RecordAttempt{ClearOwnMark}` | `TestUnsafeMark_Results/other_error` (E-4): `MarkErrored`; the write is R's |
| `RecordAttempt{ClearOwnMark}` retried; pnode dies first → FAILED at the next claim | `TestUnsafeMark_MarkedTaskIsNeverRerun` (E-5) for the next claim; the retry is R's |
| `ErrMarkedByAnotherClaim` → FAILED | `TestUnsafeMark_Results/marked_by_another_claim` (E-4) |
| `ErrTaskBusy` → safe failure | `TestUnsafeMark_Results/task_busy` (E-4) |
| unsafe `ASYNC_NEW_TX` fails, the run commits → completed | `TestUnsafeMark_AsyncNewTxFailure_RunCommits` (E-4) |
| CBD on the fired transition, later failure, all idempotent → retried from the TX_pre state | `TestStamp_FiredTransitionSegment_NotPartial_RetriedFromTXPre` (E-6) |
| CBD in a cascade step, later failure → FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | `TestStamp_CascadeStepSegment_SetsPartialCommit` (E-6) |
| cascade looping back into the source state with a CBD step sets `PartialCommit` | `TestStamp_CascadeBackInSourceState_SetsPartialCommit` (E-6) |
| owner killed after a cascade-step commit → next claim FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | `TestStamp_NextClaimAfterPartialCommit_Failed` (E-6) |
| cancellation after TX_pre stops the run at the next step | `TestRunCancel_AfterTXPre_StopsAtNextStep` (E-3) |
| joined callback writes the fired entity, no unsafe processor follows | `TestCallback_WritesFiredEntity_RunFires` (E-7) |
| joined callback writes the fired entity, then an unsafe processor → `ErrTaskBusy` | `TestCallbackAntiPattern_WritesFiredEntity_ThenUnsafe_TaskBusy` (E-4) |
| joined callback writes the fired entity in a segmented run → stamp refused, re-read classifies | `TestStamp_CallbackReArmedInRunTx_StampRefused` (E-6) |
| joined callback deletes the fired entity → the run commits | `TestCallback_DeletesFiredEntity_RunCommitsWithoutRecreating` (E-7) |
| fire-time CANCEL: no longer scheduled; no transaction id | `TestFireScheduled_OrphanedTransitionCancelled`, `TestFireScheduled_UnstampedEntity_Cancelled` (E-1, ported) |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | `TestPreRunDecision` (E-1) |
| after a re-arm, every fenced write of the old life is refused | `TestFireScheduled_ReArmedWhileClaimed_Superseded` (E-1), `TestUnsafeMark_SupersededOwnerSendsNoUnsafeProcessor` (E-4) |
| a reclaimed or re-armed task makes the old run's commit fail (C1) | `TestFireScheduled_ChangeCommittedDuringRun_Superseded` (E-1) |
| a replaced owner's segment commit is refused by its stamp | `TestStamp_ReplacedOwnerSegmentRefused` (E-6) |
| ABA: the old token is refused after a re-arm and a new claim | `TestFireScheduled_ABA_OldClaimAfterReArmAndNewClaim_Superseded` (E-1) |
| a superseded owner sends no unsafe processor | `TestUnsafeMark_SupersededOwnerSendsNoUnsafeProcessor` (E-4) |
| a refusal from inside the run's own transaction goes through §5.6 | `TestStamp_CallbackReArmedInRunTx_StampRefused` (E-6): `OutcomeFailed`, not superseded |
| a re-arm resets `PartialCommit` | `TestReconcile_ReArmResetsPartialCommit` (E-2) |
| no new commit after the watchdog fires; an in-flight commit lands | `TestRunCancel_BeforeFinalCommit_NoCommit`, `TestRunCancel_BeforeSegmentCommit_NoCommit` (E-3), `TestFireScheduled_CommitInFlightNotCutByCancellation` (E-1); the watchdog is R's |
| run cut after the drain, nothing handed off → WAITING | E-3 cancellation tests (`UnsafeReached` false); the decision is R's |
| an unsafe callout in flight at shutdown is not cut | `TestUnsafeReached_InFlightIsVisible` (E-5) for `UnsafeInFlight`; the exemption is R's |
| after the signal, a run reaching a new unsafe dispatch counts as cut | `TestUnsafeMark_NoNewUnsafe_CountsAsCut` (E-5) |
| a run cut after the drain whose unsafe work was handed off earlier → FAILED | `TestUnsafeReached_Fact/error_without_proof_stays` (E-5) for the fact; the decision is R's |
| FAILED task re-armed by an update in the state (new life, no mark) | `TestReconcile_FailedTaskReArmedAsNewLife` (E-2) |
| FAILED task cancelled when the entity leaves the state | `TestReconcile_FailedTaskCancelledWhenEntityLeavesState` (E-2) |
| a task whose transition is no longer scheduled is removed at the next write | `TestReconcile_ModelLevelFlag_RemovesTaskArmedByAnotherWorkflow`, `TestReconcile_LoopbackStateNotInWorkflow_RemovesTasks` (E-2) |

**V2** (existing scheduled tests rewritten, not deleted) is done in E-1, with a
disposition for every test. **V3** is resolved in E-2: the engine already loads
all of the model's workflows on every write, at `engine.go:694`
(`wfStore.Get(ctx, entity.Meta.ModelRef)` inside `resolveWorkflowWith`), and
every door goes through it (`engine.go:331, 442, 527`,
`fire_scheduled.go:305`). The flag is computed there, with no extra read.

---

### Task E-1: the fire door — claimed record, re-reads, `RemoveLife`, shielded commits

**Spec:** §4 (endings that commit), §5.1, §5.2 (all but the stamp), §5.3
("Commits are shielded"), §16 V2.

**Files:**
- Create: `internal/domain/workflow/run_guard.go`
- Modify: `internal/domain/workflow/fire_scheduled.go` (all of `:14-524`; `findFireableTransitionInState` at `:526-561` stays)
- Modify: `internal/domain/workflow/engine.go` (`:170-179` clock and grace fields, `:192` constructor, `:44-58` `ErrCriterionNotMatched` doc, `:770` and `:775-852` `fireTransition` loses `matched`)
- Modify: `internal/domain/workflow/engine_processors.go` (`:338-357` and `:401-407` segment re-read; `:526-535` `commitAndBeginNextSegment`)
- Create: `internal/domain/workflow/fire_run_helpers_test.go`, `internal/domain/workflow/fire_run_test.go`
- Modify: `fire_scheduled_test.go`, `fire_scheduled_concurrency_test.go`, `heartbeat_test.go`, `workflow_selection_test.go`, `arm_test.go`, `arm_function_test.go` (same package)

**Interfaces:**
- Consumes (S): `spi.TaskRef`, `spi.ScheduledTask` with `ArmToken`, `Claim`,
  `Attempts`, `LostOwners`, `UnsafeMarked`, `PartialCommit`;
  `ScheduledTaskStore.Get(ctx, tenant, id)`, `RemoveLife`, `ReconcileForEntity`,
  `Query`, `ClaimDue`, `RecordAttempt`; `spi.ClaimRequest`, `spi.Attempt`;
  `spi.ErrStaleClaim`; the five `spi.Failure*` reasons.
- Produces: `RunGuard{Ref, Store, Done}`, `WithRunGuard`, `runGuardFrom`,
  `RunReport`, `OutcomeFired|Declined|Expired|Cancelled|Superseded|Failed`,
  `(*Engine).FireScheduledTransition(ctx, task, maxLostOwners, retryDelay) RunReport`,
  `preRunDecision`. Removes `OutcomeDropped`, `WithExpiryGrace`,
  `defaultExpiryGraceMs`, `Engine.expiryGraceMs`.

- [ ] **Step 1: Write the test fixtures.** `internal/domain/workflow/fire_run_helpers_test.go`:

```go
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
		Ref:   spi.TaskRef{TenantID: task.TenantID, ID: task.ID, ArmToken: task.ArmToken, ClaimToken: task.Claim.Token},
		Store: sts,
		Done:  done,
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

var functionCriterion = json.RawMessage(`{"type":"function","function":{"name":"crit"}}`)
```

- [ ] **Step 2: Write the new door tests.** `internal/domain/workflow/fire_run_test.go`:

```go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestPreRunDecision(t *testing.T) {
	timeout := int64(1_000)
	retry := testRetryDelay
	base := spi.ScheduledTask{ScheduledTime: 10_000, TimeoutMs: &timeout}
	deadline := base.ScheduledTime + timeout
	with := func(f func(*spi.ScheduledTask)) spi.ScheduledTask { tk := base; f(&tk); return tk }

	cases := []struct {
		name       string
		task       spi.ScheduledTask
		nowMs      int64
		wantReason spi.ScheduledTaskFailureReason
		wantExpire bool
	}{
		{"mark first, before partial and lateness", with(func(tk *spi.ScheduledTask) { tk.UnsafeMarked, tk.PartialCommit, tk.Attempts = true, true, 5 }), deadline + 999_999, spi.FailureUnsafeWorkNotCompleted, false},
		{"partial commit", with(func(tk *spi.ScheduledTask) { tk.PartialCommit = true }), 0, spi.FailureStoppedAfterPartialCommit, false},
		{"owner lost max times", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 3 }), 0, spi.FailureOwnerLostRepeatedly, false},
		{"owner lost once less", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 2 }), deadline, "", false},
		{"no timeout, many attempts", spi.ScheduledTask{ScheduledTime: 10_000, Attempts: 40}, 9_999_999, "", false},
		{"first attempt at the deadline", base, deadline, "", false},
		{"first attempt 1ms late", base, deadline + 1, "", true},
		{"after a failed attempt, at deadline+retry", with(func(tk *spi.ScheduledTask) { tk.Attempts = 1 }), deadline + retry.Milliseconds(), "", false},
		{"after a failed attempt, 1ms beyond", with(func(tk *spi.ScheduledTask) { tk.Attempts = 1 }), deadline + retry.Milliseconds() + 1, spi.FailureExpiredAfterFailedAttempts, false},
		{"after a lost owner, late but within retry", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 1 }), deadline + 1, "", false},
		{"after a lost owner, beyond retry", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 1 }), deadline + retry.Milliseconds() + 1, spi.FailureExpiredAfterFailedAttempts, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, expire := preRunDecision(tc.task, tc.nowMs, testMaxLostOwners, retry)
			if reason != tc.wantReason || expire != tc.wantExpire {
				t.Fatalf("preRunDecision = (%q, %v), want (%q, %v)", reason, expire, tc.wantReason, tc.wantExpire)
			}
		})
	}
}

func TestFireScheduled_PreRunFailureTouchesNothing(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "prerun-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
	claimed.LostOwners = testMaxLostOwners

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureOwnerLostRepeatedly || r.Err != nil {
		t.Fatalf("report = %+v, want failed OWNER_LOST_REPEATEDLY without an error", r)
	}
	if n := ext.count("p1"); n != 0 {
		t.Errorf("p1 dispatched %d times, want 0", n)
	}
	if got, _ := env.task(t, claimed.ID); got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want still RUNNING under this claim (the scheduler writes Fail)", got)
	}
}

func TestFireScheduled_ExpiredOnFirstAttempt(t *testing.T) {
	env := newRunEnv(t, nil)
	wf := oneHopWF("CLOSED", nil, nil)
	armed := env.setup(t, "exp-e1", wf)
	timeout := int64(1_000)
	armed.TimeoutMs = &timeout
	armTask(t, env.factory, env.ctx, armed)
	env.advance(timeout + 1)
	claimed := env.claimOne(t, testOwner, false)

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeExpired || r.Err != nil {
		t.Fatalf("report = %+v, want expired", r)
	}
	if _, found := env.task(t, claimed.ID); found {
		t.Error("expired task must be removed")
	}
	if got := env.state(t, "exp-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "exp-e1", spi.SMEventScheduledTransitionExpired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_EXPIRE events = %d, want 1", n)
	}
}

func TestFireScheduled_LateAfterFailedAttempt(t *testing.T) {
	timeout := int64(1_000)
	for _, tc := range []struct {
		name    string
		lateMs  int64
		want    ScheduledOutcome
		wantFor spi.ScheduledTaskFailureReason
	}{
		{"within_retry_delay_runs", timeout + testRetryDelay.Milliseconds(), OutcomeFired, ""},
		{"beyond_retry_delay_failed", timeout + testRetryDelay.Milliseconds() + 1, OutcomeFailed, spi.FailureExpiredAfterFailedAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunEnv(t, nil)
			armed := env.setup(t, "late-e1", oneHopWF("CLOSED", nil, nil))
			armed.TimeoutMs = &timeout
			armTask(t, env.factory, env.ctx, armed)
			first := env.claimOne(t, testOwner, false)
			ref := spi.TaskRef{TenantID: testTenant, ID: first.ID, ArmToken: first.ArmToken, ClaimToken: first.Claim.Token}
			if err := env.sts.RecordAttempt(env.ctx, ref, spi.Attempt{Error: "boom", AtMs: env.nowMs(), NextAttemptTime: env.nowMs()}); err != nil {
				t.Fatalf("RecordAttempt: %v", err)
			}
			env.advance(tc.lateMs)
			second := env.claimOne(t, testOwner, false)

			r, _ := env.run(second)
			if r.Outcome != tc.want || r.FailReason != tc.wantFor {
				t.Fatalf("report = %+v, want %s %q", r, tc.want, tc.wantFor)
			}
		})
	}
}

func TestFireScheduled_SelfLoopReArmsSameIDAsNewLife(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "loop-e1", oneHopWF("OPEN", nil, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	got, found := env.task(t, claimed.ID)
	if !found {
		t.Fatal("self-loop must re-arm the same task id")
	}
	if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
		t.Errorf("task = %+v, want a new WAITING life without a claim", got)
	}
	if got.ScheduledTime != env.nowMs()+60_000 {
		t.Errorf("ScheduledTime = %d, want %d", got.ScheduledTime, env.nowMs()+60_000)
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "loop-e1", spi.SMEventScheduledTransitionCancelled); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 0", n)
	}
}

func TestFireScheduled_CriterionErrorIsASafeFailure(t *testing.T) {
	ext := &scriptedExtProc{criterion: func(context.Context) (bool, string, error) {
		return false, "", errors.New("criterion member failed")
	}}
	env := newRunEnv(t, ext)
	wf := oneHopWF("CLOSED", nil, nil)
	st := wf.States["OPEN"]
	st.Transitions[0].Criterion = functionCriterion
	wf.States["OPEN"] = st
	claimed := env.claimed(t, "crit-e1", wf)

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || r.Err == nil || r.FailReason != "" {
		t.Fatalf("report = %+v, want a safe failure with its error", r)
	}
	if got, _ := env.task(t, claimed.ID); got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want untouched under this claim", got)
	}
	if got := env.state(t, "crit-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestFireScheduled_ReArmedWhileClaimed_Superseded(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "rearm-e1", oneHopWF("CLOSED", nil, nil))
	if err := env.rearm(claimed); err != nil {
		t.Fatalf("rearm: %v", err)
	}

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded || r.Err != nil {
		t.Fatalf("report = %+v, want superseded", r)
	}
	got, _ := env.task(t, claimed.ID)
	if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting {
		t.Errorf("task = %+v, want the new WAITING life", got)
	}
	if got := env.state(t, "rearm-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestFireScheduled_ABA_OldClaimAfterReArmAndNewClaim_Superseded(t *testing.T) {
	env := newRunEnv(t, nil)
	old := env.claimed(t, "aba-e1", oneHopWF("CLOSED", nil, nil))
	if err := env.rearm(old); err != nil {
		t.Fatalf("rearm: %v", err)
	}
	current := env.claimOne(t, otherOwner, false)

	r, _ := env.run(old)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("old run outcome = %s, want superseded", r.Outcome)
	}
	got, _ := env.task(t, old.ID)
	if got.Claim == nil || got.Claim.Token != current.Claim.Token || got.ArmToken != current.ArmToken {
		t.Errorf("task = %+v, want the new claim of the new life untouched", got)
	}
}

func TestFireScheduled_ChangeCommittedDuringRun_Superseded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(env *runEnv, claimed spi.ScheduledTask) error
		check  func(t *testing.T, got *spi.ScheduledTask, claimed spi.ScheduledTask)
	}{
		{"rearm", func(env *runEnv, claimed spi.ScheduledTask) error { return env.rearm(claimed) },
			func(t *testing.T, got *spi.ScheduledTask, claimed spi.ScheduledTask) {
				if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting {
					t.Errorf("task = %+v, want the new WAITING life", got)
				}
			}},
		{"reclaim", func(env *runEnv, _ spi.ScheduledTask) error {
			got, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
			if err == nil && len(got) != 1 {
				err = fmt.Errorf("reclaim claimed %d tasks", len(got))
			}
			return err
		},
			func(t *testing.T, got *spi.ScheduledTask, _ spi.ScheduledTask) {
				if got.Claim == nil || got.Claim.Owner != otherOwner {
					t.Errorf("task = %+v, want RUNNING under the other owner", got)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env *runEnv
			var claimed spi.ScheduledTask
			ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
				return nil, tc.change(env, claimed)
			}}
			env = newRunEnv(t, ext)
			claimed = env.claimed(t, "c1-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

			r, _ := env.run(claimed)
			if r.Outcome != OutcomeSuperseded || r.Err != nil {
				t.Fatalf("report = %+v, want superseded (the commit conflicts on the task row)", r)
			}
			if got := env.state(t, "c1-e1"); got != "OPEN" {
				t.Errorf("entity state = %q, want OPEN", got)
			}
			got, _ := env.task(t, claimed.ID)
			tc.check(t, got, claimed)
		})
	}
}

func TestFireScheduled_GuardOfAnotherTenant_SeesNoTask(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "tenant-e1", oneHopWF("CLOSED", nil, nil))
	forged := claimed
	forged.TenantID = testTenantB
	run := newTestRun(env.sts, forged)

	r := run.fire(env.engine, ctxWithTenant(testTenantB), forged)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("outcome = %s, want superseded (another tenant's re-read finds nothing)", r.Outcome)
	}
	if got, _ := env.task(t, claimed.ID); got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("tenant A's task = %+v, want untouched", got)
	}
	if got := env.state(t, "tenant-e1"); got != "OPEN" {
		t.Errorf("tenant A's entity state = %q, want OPEN", got)
	}
}

// cancelOnCommitTxMgr cancels the run inside Commit, before the commit reads
// its context: the moment a watchdog could fire while a commit is in flight.
type cancelOnCommitTxMgr struct {
	spi.TransactionManager
	cancel context.CancelFunc
}

func (m *cancelOnCommitTxMgr) Commit(ctx context.Context, txID string) error {
	if m.cancel != nil {
		m.cancel()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit ran on a cancelled context: %w", err)
	}
	return m.TransactionManager.Commit(ctx, txID)
}

func TestFireScheduled_CommitInFlightNotCutByCancellation(t *testing.T) {
	var wrap *cancelOnCommitTxMgr
	env := newRunEnvWith(t, nil, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		wrap = &cancelOnCommitTxMgr{TransactionManager: tm}
		return wrap
	})
	claimed := env.claimed(t, "shield-e1", oneHopWF("CLOSED", nil, nil))
	runCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	wrap.cancel = cancel
	run := newTestRun(env.sts, claimed)
	run.guard.Done = runCtx.Done()

	r := run.fire(env.engine, runCtx, claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired: a commit in flight is shielded", r)
	}
	if got := env.state(t, "shield-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
}

func TestScheduledRun_ReArmedDuringCBDDispatch_Superseded(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			return nil, env.rearm(claimed)
		}
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "seg-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeCommitBeforeDispatch),
		safeProc("p2", ExecutionModeSync),
	}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded at the re-read of TX_post", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0", n)
	}
	if got := env.state(t, "seg-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre only)", got)
	}
}
```

- [ ] **Step 3: Port the existing scheduled tests (V2).** Mechanical rules, applied
  to every file listed under **Files**:

  | Before | After |
  |---|---|
  | `outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})` | `r := fireDue(t, engine, ctx, id)`; then `err` → `r.Err`, `outcome` → `r.Outcome` |
  | `if _, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant}); err != nil {` | `if r := fireDue(t, engine, ctx, id); r.Err != nil {` (and `err` → `r.Err` in the body) |
  | `sts.Get(ctx, X)` / `sts.Get(ownerCtx, X)` (`arm_test.go`, `arm_function_test.go`) | `sts.Get(ctx, testTenant, X)` / `sts.Get(ownerCtx, testTenant, X)` — every such context is `testTenant` (`engine_test.go:32-35`, `arm_test.go:261-267`) |
  | old `armTask` / `getTask` bodies (`fire_scheduled_test.go:48-73`) | deleted; the fixtures above replace them (same names, compatible calls) |
  | `failingDeleteTaskStore.Delete(context.Context, string) (bool, error)` (`workflow_selection_test.go:838-840`) | `RemoveLife(context.Context, spi.TenantID, string, uuid.UUID) error { return s.err }` |
  | `raceInjectingScheduledTaskStore.Get(ctx, id)` (`fire_scheduled_test.go:996`) | `Get(ctx context.Context, tenant spi.TenantID, id string)`, delegating with `tenant` |
  | `defaultExpiryGraceMs` (`workflow_selection_test.go:511`) | removed from the expression: `advance(delayMs + timeoutMs + 1)` |

  Dispositions. "Port" means the rules above and nothing else.

  | Test | Disposition |
  |---|---|
  | `fire_scheduled_test.go` `FiresOnTime`, `DeclineOnCriterionFalse`, `CascadeAfterFire`, `SiblingScheduledTaskStillCancelled`, `AttributesToArmedByUser_IncludingCascade`, `LegacyZeroArmedBy_…`, `GraceBoundary_LatenessEqualsTimeout_Fires` (rename `FirstAttemptAtDeadline_Fires`), `CBDSegmentedFire_HappyPath`, `CBDIntermediateFlush_…`, `SiblingEntityWrite_…` | port |
  | `GuardEntityMovedOn` | port; `OutcomeDropped` → `OutcomeCancelled` |
  | `OrphanedTransitionDropped` | port, rename `OrphanedTransitionCancelled`; → `OutcomeCancelled` |
  | `UnstampedEntity_RefusesBeforeFiring` | port, rename `UnstampedEntity_Cancelled`; → `OutcomeCancelled` |
  | `CBDSegmentedFire_ErrorAfterTXPre_NoLeak` | port; → `OutcomeFailed`; the "task remains" assertion stays |
  | `GuardCASRace_DropsWithoutTornWrite` | port, rename `GuardCASRace_SafeFailureWithoutTornWrite`; → `OutcomeFailed`, keep `errors.Is(r.Err, spi.ErrConflict)` |
  | `ExpireBeyondGrace` | replaced by `TestFireScheduled_ExpiredOnFirstAttempt` above; delete the old body |
  | `DropInGraceBand`, `GraceBoundary_…PlusGrace_DropsAndWaits`, `GraceBoundary_…PlusGracePlusOne_Expires` | replaced by `TestFireScheduled_LateAfterFailedAttempt` and `TestPreRunDecision`; delete the old bodies (the grace band is removed, spec §5.2) |
  | `TenantMismatch_DropsWithoutDeletingVictimTask` | replaced by `TestFireScheduled_GuardOfAnotherTenant_SeesNoTask` |
  | `GuardReArmedToFuture` | replaced by `TestFireScheduled_ReArmedWhileClaimed_Superseded` |
  | `VerifyOrAbort_ArmedByChangedConcurrently` | rewrite in place as `TestFireScheduled_ReArmedAtReRead_Superseded`: the `raceInjectingTaskFactory` hook re-arms the task with `ArmedBy: {ID: "new-user", Kind: spi.PrincipalUser}` in its own committed transaction (`realFactory`'s manager, as `runEnv.rearm` does); `r := fireDue(t, engine, ctx, id)`; want `OutcomeSuperseded` (the final commit conflicts on the task row, and the non-joining re-read sees the new life), entity still `OPEN`, and its `ChangeUser` not `"new-user"` |
  | `ForgedTaskArgArmedByIgnored_DurableRowWins` | delete: the task argument is now the store's claimed record, not a peer payload; its intent (never attribute a fire to a principal of another life) is `ReArmedAtReRead_Superseded` |
  | `fire_scheduled_concurrency_test.go` (both tests) | rewritten in Step 4 |
  | `heartbeat_test.go` `UnconditionalScheduledCycleFiresRepeatedly` | port (each hop claims and runs the next task) |
  | `workflow_selection_test.go` `ResolvesWorkflowByCriterion`, `ObsoleteTaskDiscardIsAudited`, `SilentGuardsRecordNoSelectionEvents`, `ExpiresEvenWhenWorkflowCannotBeResolved` | port |
  | `TransitionAbsentFromSelectedWorkflow_Dropped`, `NeverFiresAManualTransition` | port; → `OutcomeCancelled` (rename the first `…_Cancelled`) |
  | `UnresolvableWorkflowLeavesTaskForRetry` | port; → `OutcomeFailed`, task still found |
  | `EntityMovedOnDeleteFailureIsSurfaced` | port, rename `EntityMovedOnRemoveFailureIsSurfaced`; → `OutcomeFailed`, keep `errors.Is(r.Err, deleteErr)` |

- [ ] **Step 4: Rewrite the dual-coordinator tests as claim races (V2).**
  Replace the whole of `fire_scheduled_concurrency_test.go`:

```go
package workflow

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Isolated single-backend concurrency tests (never parity,
// .claude/rules/test-coverage.md). They assert consistency — one committing
// run, the other superseded or not claimed, no torn write — not an interleave.
// Audit-event counts are not asserted: the memory audit store writes outside
// the transaction.

func TestClaimRace_TwoOwnersOneDueTask_OneRunCommits(t *testing.T) {
	env := newRunEnv(t, nil)
	armed := env.setup(t, "race-e1", oneHopWF("CLOSED", nil, nil))

	owners := []uuid.UUID{testOwner, otherOwner}
	reports := make([]*RunReport, len(owners))
	var wg sync.WaitGroup
	for i, owner := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := env.sts.ClaimDue(env.ctx, claimRequest(owner, env.nowMs(), false))
			if err != nil {
				t.Errorf("ClaimDue(%d): %v", i, err)
				return
			}
			for _, task := range claimed {
				r, _ := env.run(task)
				reports[i] = &r
			}
		}()
	}
	wg.Wait()

	ran := 0
	for _, r := range reports {
		if r == nil {
			continue
		}
		ran++
		if r.Outcome != OutcomeFired || r.Err != nil {
			t.Errorf("report = %+v, want fired", *r)
		}
	}
	if ran != 1 {
		t.Fatalf("%d runs, want exactly 1 (one claim per life)", ran)
	}
	if got := env.state(t, "race-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
	if _, found := env.task(t, armed.ID); found {
		t.Error("task must be removed once")
	}
}

func TestClaimRace_StaleOwnerRunVersusReclaim_OneRunCommits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int64 // 0: none
		lateMs  int64
	}{
		{"fire", 0, 0},
		// A's first attempt is late → expired; B's reclaim counts a lost owner → runs within RETRY_DELAY.
		{"expire_versus_fire", 1_000, 2_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunEnv(t, nil)
			armed := env.setup(t, "stale-e1", oneHopWF("CLOSED", nil, nil))
			if tc.timeout > 0 {
				armed.TimeoutMs = &tc.timeout
				armTask(t, env.factory, env.ctx, armed)
			}
			env.advance(tc.lateMs)
			claimedA := env.claimOne(t, testOwner, false)

			var rA, rB RunReport
			bRan := false
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); rA, _ = env.run(claimedA) }()
			go func() {
				defer wg.Done()
				claimed, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
				if err != nil {
					t.Errorf("reclaim: %v", err)
					return
				}
				if len(claimed) == 1 {
					bRan = true
					rB, _ = env.run(claimed[0])
				}
			}()
			wg.Wait()

			reports := []RunReport{rA}
			if bRan {
				reports = append(reports, rB)
			}
			committed := 0
			for _, r := range reports {
				switch r.Outcome {
				case OutcomeFired, OutcomeExpired:
					committed++
				case OutcomeSuperseded:
				default:
					t.Errorf("report = %+v, want fired, expired or superseded", r)
				}
			}
			if committed != 1 {
				t.Fatalf("committing runs = %d, want 1 (A=%+v B=%+v ran=%v)", committed, rA, rB, bRan)
			}
			if _, found := env.task(t, armed.ID); found {
				t.Error("task must be removed by the committing run")
			}
			want := "CLOSED"
			if rA.Outcome == OutcomeExpired {
				want = "OPEN"
			}
			if got := env.state(t, "stale-e1"); got != want {
				t.Errorf("entity state = %q, want %q", got, want)
			}
		})
	}
}
```

- [ ] **Step 5: Run to verify RED**
Run: `go test ./internal/domain/workflow/... -run 'TestPreRunDecision|TestFireScheduled|TestScheduledRun|TestClaimRace|TestHeartbeat|TestReconcile'`
Expected: FAIL — build errors, among them `undefined: RunGuard`, `undefined: WithRunGuard`, `undefined: RunReport`, `undefined: preRunDecision`, `undefined: OutcomeCancelled`, `too many arguments in call to engine.FireScheduledTransition`, and the old fire path's `sts.Delete undefined` against the new SPI.

- [ ] **Step 6: Implement `run_guard.go`.**

```go
package workflow

import (
	"context"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// RunGuard travels on the context of one scheduled run (spec §5.3). The
// scheduler builds it from the task it claimed and attaches it with
// WithRunGuard before it calls FireScheduledTransition. The engine reads it
// wherever a run differs from a client request: the re-read at the start of
// each segment, the task-row writes, the unsafe mark and the cancellation
// checkpoints.
//
// Ref names this claim of this life. Store is the scheduled-task store the
// guarded calls use; the context each call is given decides whether it joins
// the open transaction. Done is the run's cancellation. It is closed on
// self-cancel, on the panic latch and at shutdown step 3.
type RunGuard struct {
	Ref   spi.TaskRef
	Store spi.ScheduledTaskStore
	Done  <-chan struct{}
}

type runGuardKey struct{}

// WithRunGuard returns ctx carrying g. Every context the engine derives from
// it carries g too, including the context.WithoutCancel segments after a
// COMMIT_BEFORE_DISPATCH commit: WithoutCancel keeps values.
func WithRunGuard(ctx context.Context, g *RunGuard) context.Context {
	return context.WithValue(ctx, runGuardKey{}, g)
}

func RunGuardFrom(ctx context.Context) *RunGuard {
	g, _ := ctx.Value(runGuardKey{}).(*RunGuard)
	return g
}

// errRunSuperseded ends a run whose task was re-armed, reclaimed or removed.
// FireScheduledTransition reports it as OutcomeSuperseded; it never reaches
// RunReport.Err.
var errRunSuperseded = errors.New("scheduled run superseded")

// holds reports whether cur is still this run's life and claim.
func (g *RunGuard) holds(cur *spi.ScheduledTask) bool {
	return cur.ArmToken == g.Ref.ArmToken && cur.Claim != nil && cur.Claim.Token == g.Ref.ClaimToken
}

// rereadTask is the first read of every segment of a run (spec §5.2). On
// PostgreSQL it also fixes the segment's snapshot before any other statement,
// so a later change to the task row by another transaction makes this
// segment's own task-row write conflict (C1).
func rereadTask(ctx context.Context, g *RunGuard) (*spi.ScheduledTask, error) {
	cur, found, err := g.Store.Get(ctx, g.Ref.TenantID, g.Ref.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to re-read scheduled task: %w", err)
	}
	if !found || !g.holds(cur) {
		return nil, errRunSuperseded
	}
	return cur, nil
}

// rereadSegment is rereadTask for a segment opened after a
// COMMIT_BEFORE_DISPATCH commit. It does nothing outside a scheduled run.
func rereadSegment(ctx context.Context) error {
	g := RunGuardFrom(ctx)
	if g == nil {
		return nil
	}
	_, err := rereadTask(ctx, g)
	return err
}

// removeOwnLife removes this run's task in the transaction on ctx. It does
// nothing if the same transaction already replaced or removed the task.
func removeOwnLife(ctx context.Context, g *RunGuard) error {
	if err := g.Store.RemoveLife(ctx, g.Ref.TenantID, g.Ref.ID, g.Ref.ArmToken); err != nil {
		return fmt.Errorf("failed to remove the scheduled task: %w", err)
	}
	return nil
}
```

- [ ] **Step 7: Rewrite `fire_scheduled.go` `:1-524`** (keep `findFireableTransitionInState`, `:526-561`, unchanged):

```go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// ScheduledOutcome reports how FireScheduledTransition ended one claimed run.
type ScheduledOutcome string

const (
	// OutcomeFired: the transition fired and the run committed.
	OutcomeFired ScheduledOutcome = "fired"
	// OutcomeDeclined: the criterion evaluated false; the task was removed.
	OutcomeDeclined ScheduledOutcome = "declined"
	// OutcomeExpired: late on its first attempt; the task was removed with
	// SCHEDULED_TRANSITION_EXPIRE.
	OutcomeExpired ScheduledOutcome = "expired"
	// OutcomeCancelled: the task was removed without firing — the entity is
	// gone or moved on, the transition is no longer scheduled in the selected
	// workflow, or the entity has no transaction id to guard the fire.
	OutcomeCancelled ScheduledOutcome = "cancelled"
	// OutcomeSuperseded: the task's life or claim changed. The run committed
	// nothing, and nothing is recorded.
	OutcomeSuperseded ScheduledOutcome = "superseded"
	// OutcomeFailed: the run did not commit. The scheduler records the
	// outcome (spec §5.6).
	OutcomeFailed ScheduledOutcome = "failed"
)

// RunReport is what the scheduler needs to record a run's outcome (spec
// §5.6). Err is nil when the run committed or was superseded.
type RunReport struct {
	Outcome       ScheduledOutcome
	Err           error
	MarkHeld      bool                           // a MarkUnsafe was accepted in this run
	UnsafeReached bool                           // the in-memory fact of spec §5.5
	MarkErrored   bool                           // the last MarkUnsafe failed with a non-refusal error
	PartialCommit bool                           // a stamp with partial=true committed in this run
	FailReason    spi.ScheduledTaskFailureReason // the engine decided FAILED itself
}

// firePrincipalSystemID identifies the platform system principal the fire
// path executes as and attributes legacy (zero-ArmedBy) rows to. The same
// identity as common.SystemPrincipal(), defined here because
// internal/domain/workflow must not import internal/scheduler.
const firePrincipalSystemID = "system"

var systemPrincipal = spi.Principal{ID: firePrincipalSystemID, Kind: spi.PrincipalSystem}

// preRunDecision applies the checks the owner makes on the claimed record
// before it runs (spec §5.1), in order. It returns a failure reason, or
// expire=true for a first attempt past its deadline, or neither.
func preRunDecision(task spi.ScheduledTask, nowMs int64, maxLostOwners int, retryDelay time.Duration) (reason spi.ScheduledTaskFailureReason, expire bool) {
	switch {
	case task.UnsafeMarked:
		return spi.FailureUnsafeWorkNotCompleted, false
	case task.PartialCommit:
		return spi.FailureStoppedAfterPartialCommit, false
	case task.LostOwners >= maxLostOwners:
		return spi.FailureOwnerLostRepeatedly, false
	}
	if task.TimeoutMs == nil {
		return "", false
	}
	deadline := task.ScheduledTime + *task.TimeoutMs
	if task.Attempts == 0 && task.LostOwners == 0 {
		return "", nowMs > deadline
	}
	if nowMs > deadline+retryDelay.Milliseconds() {
		return spi.FailureExpiredAfterFailedAttempts, false
	}
	return "", false
}

// FireScheduledTransition runs one claimed scheduled task. It is the
// scheduler's only door into the engine; no HTTP or gRPC handler calls it.
//
// task is the record ClaimDue returned. ctx carries the system identity of
// task.TenantID and a RunGuard whose Ref is this claim; the scheduler builds
// both. The engine makes the §5.1 decisions on the claimed record, then runs
// the fire in transactions that each start by re-reading the task (§5.2) and
// each write the task row before they commit. It commits the endings that
// remove or re-arm the task (fired, declined, expired, cancelled). It never
// writes RecordAttempt or Fail: the scheduler records every other outcome
// from the returned report (§5.6, §5.7).
func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) RunReport {
	g := RunGuardFrom(ctx)
	reason, expire := preRunDecision(task, e.now().UnixMilli(), maxLostOwners, retryDelay)
	if reason != "" {
		return RunReport{Outcome: OutcomeFailed, FailReason: reason}
	}
	outcome, err := e.fireScheduled(ctx, g, task, expire)
	return e.runReport(ctx, g, outcome, err)
}

// runReport turns the run's result into a report. It runs after the run's
// open segment was rolled back (fireScheduled's deferred rollback).
func (e *Engine) runReport(ctx context.Context, g *RunGuard, outcome ScheduledOutcome, err error) RunReport {
	r := RunReport{}
	switch {
	case err == nil:
		r.Outcome = outcome
	case supersededBy(ctx, g, err):
		r.Outcome = OutcomeSuperseded
	default:
		r.Outcome = OutcomeFailed
		r.Err = err
	}
	return r
}

// supersededBy decides whether a failed run was superseded (spec §5.2). A
// refusal or a conflict is classified by re-reading the task with a read that
// does not join any transaction: only the committed state says whether the
// life or the claim changed.
func supersededBy(ctx context.Context, g *RunGuard, err error) bool {
	if errors.Is(err, errRunSuperseded) {
		return true
	}
	if !errors.Is(err, spi.ErrStaleClaim) && !errors.Is(err, spi.ErrConflict) {
		return false
	}
	readCtx := spi.WithTransaction(context.WithoutCancel(ctx), nil)
	cur, found, rerr := g.Store.Get(readCtx, g.Ref.TenantID, g.Ref.ID)
	if rerr != nil {
		// Undecided. Reported as a failure: the scheduler's bookkeeping write
		// is fenced, so it is refused if the run was in fact superseded.
		slog.WarnContext(ctx, "scheduled run: re-read after a refused write failed",
			"pkg", "workflow", "taskId", g.Ref.ID, "err", rerr)
		return false
	}
	return !found || !g.holds(cur)
}

func (e *Engine) fireScheduled(ctx context.Context, g *RunGuard, task spi.ScheduledTask, expire bool) (ScheduledOutcome, error) {
	// The claimed record carries ArmedBy. Seeded before Begin, so that
	// spi.ResolveOrigin inside Begin sees it as the fire's root origin and
	// every write of the cascade inherits it. If the life changed since the
	// claim, the re-read below ends the run before anything is written.
	ctx = spi.WithAmbientOrigin(ctx, task.ArmedBy)

	txID, txCtx, err := e.txMgr.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to begin scheduled-fire transaction: %w", err)
	}
	// curCtx/curTxID track the open segment. A COMMIT_BEFORE_DISPATCH
	// processor commits the entry segment and opens a new one; every exit
	// that does not commit rolls back the segment open now.
	curCtx, curTxID := txCtx, txID
	committed := false
	defer func() {
		if !committed {
			rbCtx, cancel := common.RollbackContext(curCtx)
			defer cancel()
			_ = e.txMgr.Rollback(rbCtx, curTxID)
		}
	}()
	commit := func(id string) error {
		if err := e.commitRun(ctx, id); err != nil {
			return err
		}
		committed = true
		return nil
	}

	cur, err := rereadTask(txCtx, g)
	if err != nil {
		return "", err
	}
	auditStore, err := e.factory.StateMachineAuditStore(txCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get audit store: %w", err)
	}

	if expire {
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		lateness := e.now().UnixMilli() - cur.ScheduledTime
		e.recordEvent(auditStore, txCtx, cur.EntityID, txID, cur.SourceState,
			spi.SMEventScheduledTransitionExpired,
			fmt.Sprintf("Scheduled transition %q expired (lateness %dms > timeout %dms)",
				cur.Transition, lateness, *task.TimeoutMs), nil)
		slog.InfoContext(txCtx, "scheduled transition expired",
			"pkg", "workflow", "taskId", cur.ID, "entityId", cur.EntityID, "transition", cur.Transition)
		return OutcomeExpired, commit(txID)
	}

	es, err := e.factory.EntityStore(txCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get entity store: %w", err)
	}
	entity, err := es.Get(txCtx, cur.EntityID)
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		return "", fmt.Errorf("failed to read entity: %w", err)
	}
	if err != nil || entity.Meta.State != cur.SourceState {
		// Gone, or moved on: removed without an audit event (spec §4).
		slog.DebugContext(txCtx, "scheduled task's entity is gone or has left the source state; removing",
			"pkg", "workflow", "taskId", cur.ID, "entityId", cur.EntityID)
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		return OutcomeCancelled, commit(txID)
	}

	// Selection is criterion-based, as on the client doors: a fire runs the
	// definition the entity is bound to now. A resolution failure is a safe
	// failure; it never falls through to another definition.
	wf, err := e.resolveWorkflow(txCtx, entity, auditStore, txID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve workflow for scheduled fire: %w", err)
	}
	transition := findFireableTransitionInState(wf, entity.Meta.State, cur.Transition)
	if transition == nil {
		// Audited: a vanished timer must be attributable.
		slog.DebugContext(txCtx, "scheduled task references a transition the selected workflow does not declare as scheduled; removing",
			"pkg", "workflow", "entityId", cur.EntityID, "workflowName", wf.Name,
			"sourceState", cur.SourceState, "transition", cur.Transition)
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		e.recordEvent(auditStore, txCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventScheduledTransitionCancelled,
			fmt.Sprintf("Scheduled transition %q cancelled (not a scheduled transition of state %q in the selected workflow %q)",
				cur.Transition, cur.SourceState, wf.Name),
			map[string]any{"transition": cur.Transition, "sourceState": cur.SourceState, "workflowName": wf.Name})
		return OutcomeCancelled, commit(txID)
	}

	// Anchor stamp, before any processor can flush the entity: attributed to
	// the arming principal (system for legacy rows), executed by the system.
	armed := cur.ArmedBy
	if armed == (spi.Principal{}) {
		armed = systemPrincipal
	}
	entity.Meta.ChangeUser = armed.ID
	entity.Meta.ChangeUserKind = armed.Kind
	entity.Meta.ChangeExecutor = systemPrincipal

	// expectedTxID is the entity's last committed transaction id as read in
	// this transaction: the precondition of the final persist.
	expectedTxID := entity.Meta.TransactionID
	if expectedTxID == "" {
		// No precondition to fire under. Refused before any processor runs,
		// because a COMMIT_BEFORE_DISPATCH segment would already have
		// committed. Nothing rewrites a stored transaction id, so the task is
		// removed and audited; a later write re-arms it.
		slog.Error("scheduled fire refused: stored entity carries no transaction ID to guard against",
			"pkg", "workflow", "taskID", cur.ID, "entityID", cur.EntityID)
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		e.recordEvent(auditStore, txCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventScheduledTransitionCancelled,
			fmt.Sprintf("Scheduled transition %q cancelled (entity carries no committed transaction ID, so the fire cannot be guarded)",
				cur.Transition),
			map[string]any{"transition": cur.Transition, "sourceState": cur.SourceState})
		return OutcomeCancelled, commit(txID)
	}
	fireCtx := withIfMatch(txCtx, expectedTxID)

	newCtx, newTxID, fireErr := e.fireTransition(fireCtx, entity, wf, transition, auditStore, txID)
	curCtx, curTxID = newCtx, newTxID
	if fireErr != nil {
		if errors.Is(fireErr, ErrCriterionNotMatched) {
			// Declined; fireTransition already recorded the criterion event.
			if err := removeOwnLife(newCtx, g); err != nil {
				return "", err
			}
			return OutcomeDeclined, commit(newTxID)
		}
		return "", fireErr
	}

	finalCtx, finalTxID, err := e.cascadeAutomated(newCtx, entity, wf, auditStore, newTxID)
	curCtx, curTxID = finalCtx, finalTxID
	if err != nil {
		return "", err
	}

	// Order at the end (spec §5.2): the run removes its own life first, then
	// the re-arm step runs for the final state. A self-loop therefore re-arms
	// the same id as a new life. A joining read sees the operations staged
	// earlier in the same transaction on every backend (C2): reconcile does
	// not see the life removed here, and RemoveLife does nothing if a joined
	// callback already replaced the task in this transaction. PostgreSQL
	// writes task rows into the open transaction at once; memory and SQLite
	// overlay the staged operations on the committed state.
	if err := removeOwnLife(finalCtx, g); err != nil {
		return "", err
	}
	if err := e.reconcileScheduledTasks(finalCtx, entity, wf, finalTxID, auditStore, task.ID); err != nil {
		return "", fmt.Errorf("failed to reconcile scheduled tasks after fire: %w", err)
	}

	finalEntityStore, err := e.factory.EntityStore(finalCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get entity store for persist: %w", err)
	}
	if finalTxID != txID {
		// Segmented: the first segment flush already applied the precondition.
		if _, err := finalEntityStore.Save(finalCtx, entity); err != nil {
			return "", fmt.Errorf("failed to save fired entity: %w", err)
		}
	} else if _, err := finalEntityStore.CompareAndSave(finalCtx, entity, expectedTxID); err != nil {
		return "", err
	}

	e.recordEvent(auditStore, finalCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventScheduledTransitionFired,
		fmt.Sprintf("Scheduled transition %q fired", cur.Transition), nil)
	return OutcomeFired, commit(finalTxID)
}

// commitRun commits one entity transaction of a scheduled run, shielded: a
// cancellation that arrives while the commit is in flight does not cut it
// (spec §5.3).
func (e *Engine) commitRun(ctx context.Context, txID string) error {
	return common.ShieldedCommitWithBudget(ctx, e.commitBudget, func(commitCtx context.Context) error {
		if err := e.txMgr.Commit(commitCtx, txID); err != nil {
			return fmt.Errorf("failed to commit scheduled run: %w", err)
		}
		return nil
	})
}
```

- [ ] **Step 8: `engine.go` edits.**
  - `:170-179`: replace the `clock` comment's "FireScheduledTransition's lateness/grace-band math and the scheduler's scan loop" with "FireScheduledTransition's deadline decisions"; delete the `expiryGraceMs` field and its comment.
  - `:192`: drop `expiryGraceMs: defaultExpiryGraceMs,` from the literal.
  - `:44-49` (`ErrCriterionNotMatched` doc): "every other fireTransition failure (criterion-evaluation error, processor failure — both retried on the next scan)" → "every other fireTransition failure (criterion-evaluation error, processor failure — a failure the scheduler records and retries)".
  - `fireTransition` (`:775-852`): drop the `retMatched bool` result and its doc paragraph (`:780-784`); every `return x, y, false, err` / `return newCtx, newTxID, true, nil` loses the bool. Its callers: `engine.go:770` becomes `newCtx, newTxID, err := e.fireTransition(...)`; `fire_scheduled.go` is already written that way above. The only reader of `matched` was the unreachable branch at `fire_scheduled.go:446-452`.

- [ ] **Step 9: `engine_processors.go` — the re-read at every segment start** (spec §5.2).
  - `commitAndBeginNextSegment` (`:526-535`):

```go
func (e *Engine) commitAndBeginNextSegment(ctx context.Context, entity *spi.Entity, txID, expectedTxID string, applyIfMatch bool) (newTxID string, newCtx context.Context, err error) {
	if fcErr := e.flushAndCommitSegment(ctx, entity, txID, expectedTxID, applyIfMatch); fcErr != nil {
		return "", nil, fcErr
	}
	newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx))
	if err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: begin TX_post: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
	}
	// A scheduled run re-reads its task first in every segment (spec §5.2).
	// On failure TX_post is handed back with the error, so the caller's guard
	// rolls it back.
	if err := rereadSegment(newCtx); err != nil {
		return newTxID, newCtx, err
	}
	return newTxID, newCtx, nil
}
```

  and in its doc comment (`:517-525`) add: "On a failed re-read it returns the new segment with the error; the caller rolls it back."
  - The `=true` branch comment at `:339-342` ("commitAndBeginNextSegment returns ("", nil, err) on failure …") becomes: "On a failed flush it returns ("", nil, err) and rollbackSegment no-ops; on a failed re-read it returns TX_post, which the guard rolls back."
  - The `=false` branch, after `:403-407`:

```go
		newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx))
		segCtx, segTxID = newCtx, newTxID
		if err != nil {
			return nil, "", fmt.Errorf("commit-before-dispatch: begin TX_post: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
		// First read of the new segment (spec §5.2); the guard above rolls
		// TX_post back on failure.
		if err := rereadSegment(newCtx); err != nil {
			return nil, "", err
		}
```

- [ ] **Step 10: Run to verify GREEN**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS.

- [ ] **Step 11: Vet the package and check the exit greps for this stream**
Run: `go vet ./internal/domain/workflow/ && git grep -n -e ExpiryGrace -e expiryGrace -e OutcomeDropped -e 'sts\.Delete(' -e '\.Upsert(ctx, task' -- internal/domain/workflow`
Expected: vet clean; grep prints nothing.

- [ ] **Step 12: Commit**

```
git add internal/domain/workflow/run_guard.go internal/domain/workflow/fire_scheduled.go \
  internal/domain/workflow/engine.go internal/domain/workflow/engine_processors.go \
  internal/domain/workflow/fire_run_helpers_test.go internal/domain/workflow/fire_run_test.go \
  internal/domain/workflow/fire_scheduled_test.go internal/domain/workflow/fire_scheduled_concurrency_test.go \
  internal/domain/workflow/heartbeat_test.go internal/domain/workflow/workflow_selection_test.go \
  internal/domain/workflow/arm_test.go internal/domain/workflow/arm_function_test.go
git commit -m "feat(workflow): the fire door runs a claimed task under a run guard

The engine decides the claimed record's FAILED reasons and first-attempt
expiry, re-reads the task at the start of every segment, removes its own
life before the re-arm step, commits shielded, and reports superseded
after a non-joining re-read. The origin pre-read, the ArmedBy verify,
the re-armed-future guard, the tenant guard and the grace band are gone.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task E-2: reconcile — remove every task not in the arm set; the model-level flag

**Spec:** §7 ("Arm", "Cancel", "When reconcile does nothing"), §16 V3.

**Files:**
- Modify: `internal/domain/workflow/arm.go` (`:32-45` `workflowHasSchedule` deleted; `:66-209` reconcile)
- Modify: `internal/domain/workflow/engine.go` (`:331, 365, 442, 461, 527-550, 563, 659-708`)
- Modify: `internal/domain/workflow/fire_scheduled.go` (the `resolveWorkflow` and `reconcileScheduledTasks` calls)
- Modify: `internal/domain/workflow/transitions.go:61` (unchanged call, `resolveWorkflowForQuery` keeps its signature)
- Test: `internal/domain/workflow/arm_test.go`

**Interfaces:**
- Consumes (S/BM): `ReconcileForEntity` removes every task of the entity not in `req.Arm`; an arm resets status, attempts, lost owners, errors, `PartialCommit`, the mark and the claim.
- Produces: `modelHasSchedule(wfs []spi.WorkflowDefinition) bool`; `resolveWorkflow` returns `(wf, modelScheduled bool, err)`; `reconcileScheduledTasks(ctx, entity, wf, modelScheduled, txID, auditStore, suppressCancelAuditFor)`.

- [ ] **Step 1: Write the failing tests** (append to `arm_test.go`):

```go
// loopbackInTx runs Loopback on the stored entity in a transaction of its own and commits.
func loopbackInTx(t *testing.T, engine *Engine, factory spi.StoreFactory, ctx context.Context, entityID string) {
	t.Helper()
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(txCtx, entityID)
	if err != nil {
		t.Fatalf("Get entity: %v", err)
	}
	entity.Meta.TransactionID = txID
	if _, err := engine.Loopback(txCtx, entity); err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestReconcile_ModelLevelFlag_RemovesTaskArmedByAnotherWorkflow(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "flag-order", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{
		{Version: "1.1", Name: "kind-a-wf", InitialState: "OPEN", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
			States: map[string]spi.StateDefinition{
				"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
				"CLOSED": {},
			}},
		{Version: "1.1", Name: "kind-b-wf", InitialState: "OPEN", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
			States: map[string]spi.StateDefinition{
				"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "close", Next: "CLOSED", Manual: true}}},
				"CLOSED": {},
			}},
	})
	// Armed under kind-a-wf; the entity's data now binds it to kind-b-wf, which schedules nothing.
	seedFireEntity(t, factory, ctx, "flag-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{"kind": "b"})
	id := taskID(testTenant, "flag-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: nowMs + 1000, EntityID: "flag-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: nowMs})

	loopbackInTx(t, engine, factory, ctx, "flag-e1")

	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("a task the selected workflow does not arm must be removed at the next write")
	}
	if n := countAuditEvents(t, factory, ctx, "flag-e1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1", n)
	}
}

func TestReconcile_LoopbackStateNotInWorkflow_RemovesTasks(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "legacy-order", ModelVersion: "1.0"}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "wf", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
			"CLOSED": {},
		},
	}})
	seedFireEntity(t, factory, ctx, "legacy-e1", modelRef, "LEGACY", "seed-tx-1", map[string]any{})
	id := taskID(testTenant, "legacy-e1", "LEGACY", "Tick")
	armTask(t, factory, ctx, spi.ScheduledTask{ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: nowMs + 1000, EntityID: "legacy-e1", ModelName: modelRef.EntityName,
		Transition: "Tick", SourceState: "LEGACY", ArmedAt: nowMs})

	loopbackInTx(t, engine, factory, ctx, "legacy-e1")

	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("a write to an entity whose state the selected workflow does not declare must remove its tasks")
	}
}

func TestReconcile_FailedTaskReArmedAsNewLife(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "failed-e1", oneHopWF("CLOSED", nil, nil))
	ref := spi.TaskRef{TenantID: testTenant, ID: claimed.ID, ArmToken: claimed.ArmToken, ClaimToken: claimed.Claim.Token}
	if err := env.sts.MarkUnsafe(env.ctx, ref); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := env.sts.Fail(env.ctx, ref, spi.Failure{Reason: spi.FailureUnsafeWorkNotCompleted, Error: "x", AtMs: env.nowMs()}); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	loopbackInTx(t, env.engine, env.factory, env.ctx, "failed-e1")

	got, found := env.task(t, claimed.ID)
	if !found {
		t.Fatal("an update in the state must re-arm the FAILED task")
	}
	if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting || got.Attempts != 0 ||
		got.LostOwners != 0 || got.UnsafeMarked || got.FailureReason != "" || got.LastError != "" || got.Claim != nil {
		t.Errorf("task = %+v, want a fresh WAITING life with no mark", got)
	}
}

func TestReconcile_FailedTaskCancelledWhenEntityLeavesState(t *testing.T) {
	env := newRunEnv(t, nil)
	wf := oneHopWF("CLOSED", nil, nil)
	st := wf.States["OPEN"]
	st.Transitions = append(st.Transitions, spi.TransitionDefinition{Name: "advance", Next: "CLOSED", Manual: true})
	wf.States["OPEN"] = st
	claimed := env.claimed(t, "leave-e1", wf)
	ref := spi.TaskRef{TenantID: testTenant, ID: claimed.ID, ArmToken: claimed.ArmToken, ClaimToken: claimed.Claim.Token}
	if err := env.sts.Fail(env.ctx, ref, spi.Failure{Reason: spi.FailureOwnerLostRepeatedly, AtMs: env.nowMs()}); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, _ := env.factory.EntityStore(txCtx)
	entity, err := es.Get(txCtx, "leave-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Meta.TransactionID = txID
	if _, err := env.engine.ManualTransition(txCtx, entity, "advance"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := env.txMgr.Commit(env.ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, found := env.task(t, claimed.ID); found {
		t.Error("a FAILED task must be removed when the entity leaves its state")
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "leave-e1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1", n)
	}
}

func TestReconcile_ReArmResetsPartialCommit(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "partial-e1", oneHopWF("CLOSED", nil, nil))
	ref := spi.TaskRef{TenantID: testTenant, ID: claimed.ID, ArmToken: claimed.ArmToken, ClaimToken: claimed.Claim.Token}
	if err := env.sts.StampSegment(env.ctx, ref, true); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}

	loopbackInTx(t, env.engine, env.factory, env.ctx, "partial-e1")

	if got, _ := env.task(t, claimed.ID); got.PartialCommit {
		t.Errorf("task = %+v, want PartialCommit false on the new life", got)
	}
}

// countingWorkflowFactory counts WorkflowStore().Get calls.
type countingWorkflowFactory struct {
	spi.StoreFactory
	gets *int
}

type countingWorkflowStore struct {
	spi.WorkflowStore
	gets *int
}

func (f countingWorkflowFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	ws, err := f.StoreFactory.WorkflowStore(ctx)
	if err != nil {
		return nil, err
	}
	return countingWorkflowStore{WorkflowStore: ws, gets: f.gets}, nil
}

func (s countingWorkflowStore) Get(ctx context.Context, ref spi.ModelRef) ([]spi.WorkflowDefinition, error) {
	*s.gets++
	return s.WorkflowStore.Get(ctx, ref)
}

func TestReconcile_ModelFlagNeedsNoExtraWorkflowRead(t *testing.T) {
	gets := 0
	env := newRunEnvWith(t, nil, func(f spi.StoreFactory) spi.StoreFactory {
		return countingWorkflowFactory{StoreFactory: f, gets: &gets}
	}, nil)
	env.setup(t, "reads-e1", oneHopWF("CLOSED", nil, nil))
	gets = 0

	loopbackInTx(t, env.engine, env.factory, env.ctx, "reads-e1")

	if gets != 1 {
		t.Errorf("workflow reads per write = %d, want 1", gets)
	}
}
```

(`countingWorkflowStore.Get` has the signature of `spi.WorkflowStore.Get`, `SPI/persistence.go:314`.)

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... -run 'TestReconcile_'`
Expected: FAIL — `TestReconcile_ModelLevelFlag_RemovesTaskArmedByAnotherWorkflow` ("a task the selected workflow does not arm must be removed at the next write": `arm.go:96` returns early because `kind-b-wf` schedules nothing) and `TestReconcile_LoopbackStateNotInWorkflow_RemovesTasks` (Loopback returns at `engine.go:529-547` without reconciling). The other four pass already (the store does the reset; they guard it).

- [ ] **Step 3: Implement.**
  - `arm.go`: delete `workflowHasSchedule` (`:32-45`) and add:

```go
// modelHasSchedule reports whether any workflow of the model — active or not,
// whatever the transition's manual or disabled flag — declares a scheduled
// transition. When none does, no task of the model's entities can be armed,
// and a workflow import has already removed the model's tasks (spec §7), so
// reconcile does nothing. It is computed from the workflows resolveWorkflow
// already loaded, so a write costs no extra read.
func modelHasSchedule(wfs []spi.WorkflowDefinition) bool {
	for i := range wfs {
		for _, st := range wfs[i].States {
			for _, tr := range st.Transitions {
				if tr.Schedule != nil {
					return true
				}
			}
		}
	}
	return false
}
```

  - `reconcileScheduledTasks` (`:95-98`): new signature and early return:

```go
func (e *Engine) reconcileScheduledTasks(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, modelScheduled bool, txID string, auditStore spi.StateMachineAuditStore, suppressCancelAuditFor string) error {
	if !modelScheduled {
		return nil
	}
```

  Doc comment `:66-94`: "cancels (deletes) any pending task left over from a state the entity is no longer in" → "removes every other task of the entity, in any status: one left over from another state, one whose transition is no longer scheduled, and a FAILED one"; "No-op … when the workflow has no scheduled transitions anywhere" → "No-op when no workflow of the entity's model has a scheduled transition (modelHasSchedule)".
  - The CANCEL audit at `:187-195`:

```go
	for _, c := range cancelled {
		if suppressCancelAuditFor != "" && c.ID == suppressCancelAuditFor {
			continue
		}
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, c.SourceState,
			spi.SMEventScheduledTransitionCancelled,
			fmt.Sprintf("Scheduled transition %q of state %q cancelled (not armed for state %q)", c.Transition, c.SourceState, state),
			map[string]any{"transition": c.Transition, "sourceState": c.SourceState})
	}
```

  - `engine.go` `resolveWorkflow`/`resolveWorkflowForQuery`/`resolveWorkflowWith` (`:659-708`):

```go
func (e *Engine) resolveWorkflow(ctx context.Context, entity *spi.Entity, auditStore spi.StateMachineAuditStore, txID string) (*spi.WorkflowDefinition, bool, error) {
	return e.resolveWorkflowWith(ctx, entity, auditStore, txID)
}

func (e *Engine) resolveWorkflowForQuery(ctx context.Context, entity *spi.Entity) (*spi.WorkflowDefinition, error) {
	wf, _, err := e.resolveWorkflowWith(ctx, entity, discardedAuditStore{}, "")
	return wf, err
}

// resolveWorkflowWith also reports modelScheduled: whether any stored
// workflow of the model declares a scheduled transition (spec §7).
func (e *Engine) resolveWorkflowWith(ctx context.Context, entity *spi.Entity, auditStore spi.StateMachineAuditStore, txID string) (*spi.WorkflowDefinition, bool, error) {
	wfStore, err := e.factory.WorkflowStore(ctx)
	if err != nil {
		return nil, false, common.Internal("failed to access workflow store", err)
	}
	workflows, err := wfStore.Get(ctx, entity.Meta.ModelRef)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		workflows = nil
	} else if err != nil {
		return nil, false, common.Internal("failed to load workflows", err)
	}
	modelScheduled := modelHasSchedule(workflows)
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no workflows imported for model — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_workflows_imported")
		workflows = e.defaultWorkflows
	}
	wf, err := e.selectWorkflow(ctx, workflows, entity, auditStore, txID)
	if err != nil {
		return nil, false, err
	}
	return wf, modelScheduled, nil
}
```

  (keep the existing comments of `:681-702` in place.)
  - `Execute` `:331`: `selectedWF, modelScheduled, err := e.resolveWorkflow(...)`; `:365`: `e.reconcileScheduledTasks(currentCtx, entity, selectedWF, modelScheduled, currentTxID, auditStore, "")`.
  - `ManualTransition` `:442` and `:461`: the same.
  - `Loopback` `:527` and `:563`: the same; and in the state-not-in-workflow branch, after the `SMEventForcedSuccess` event (`:533-535`) and before `SMEventFinished`:

```go
		// Its tasks are still reconciled: nothing can be armed for a state the
		// selected workflow does not declare, so every task of the entity is
		// removed (spec §7).
		if err := e.reconcileScheduledTasks(ctx, entity, wf, modelScheduled, txID, auditStore, ""); err != nil {
			return nil, err
		}
```

  - `fire_scheduled.go`: `wf, modelScheduled, err := e.resolveWorkflow(txCtx, entity, auditStore, txID)` and `e.reconcileScheduledTasks(finalCtx, entity, wf, modelScheduled, finalTxID, auditStore, task.ID)`.

- [ ] **Step 4: Run to verify GREEN**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/arm.go internal/domain/workflow/engine.go \
  internal/domain/workflow/fire_scheduled.go internal/domain/workflow/arm_test.go
git commit -m "feat(workflow): reconcile removes every task not armed; model-level flag

Reconcile returns early only when no workflow of the model schedules a
transition, computed from the workflows each write already loads. A
loopback on a state the selected workflow does not declare reconciles too.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task E-3: cancellation — checkpoints, and callouts that see the run's cancellation

**Spec:** §5.3 ("Callouts see the cancellation", "Checkpoints").

**Files:**
- Modify: `internal/domain/workflow/run_guard.go`
- Modify: `internal/domain/workflow/engine_processors.go` (loop top `:122`; dispatch sites `:230, :269, :360, :392`; `flushAndCommitSegment` `:487-509`)
- Modify: `internal/domain/workflow/engine.go` (`cascadeAutomated` `:889-891`; `evaluateCriterion` `:1023`)
- Modify: `internal/domain/workflow/arm.go` (`armViaFunction` `:243`)
- Modify: `internal/domain/workflow/fire_scheduled.go` (`commitRun`)
- Create: `internal/domain/workflow/fire_cancel_test.go`

**Interfaces:**
- Produces: `errRunCancelled` (unexported; every error built with it satisfies `errors.Is(err, context.Canceled)`), `runCheckpoint(ctx, where string) error`, `runCallCtx(ctx) (context.Context, context.CancelFunc)`.

- [ ] **Step 1: Write the failing tests.** `internal/domain/workflow/fire_cancel_test.go`:

```go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// sawCancel reports whether ctx is cancelled within two seconds.
func sawCancel(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func assertCancelled(t *testing.T, r RunReport) {
	t.Helper()
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("report = %+v, want failed with a cancellation", r)
	}
}

func TestRunCancel_BeforeProcessorDispatch(t *testing.T) {
	var run *testRun
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			run.cancel()
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "cp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeSync), safeProc("p2", ExecutionModeSync),
	}, nil))
	run = newTestRun(env.sts, claimed)

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times after the cancellation, want 0", n)
	}
	if got := env.state(t, "cp-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestRunCancel_AfterTXPre_StopsAtNextStep(t *testing.T) {
	var run *testRun
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			run.cancel()
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "txpre-e1", oneHopWF("MID",
		[]spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch)},
		map[string]spi.StateDefinition{"MID": autoStep("DONE", safeProc("p2", ExecutionModeSync))}))
	run = newTestRun(env.sts, claimed)

	r := run.fire(env.engine, env.ctx, claimed)
	assertCancelled(t, r)
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0 (the cascade step checks the guard)", n)
	}
	if got := env.state(t, "txpre-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre only)", got)
	}
	if r.UnsafeReached {
		t.Error("UnsafeReached must be false: only idempotent processors ran")
	}
}

func TestRunCancel_BeforeFinalCommit_NoCommit(t *testing.T) {
	var run *testRun
	ext := &scriptedExtProc{function: func(context.Context) (contract.FunctionResult, error) {
		run.cancel()
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":60000}`)}, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "final-e1", oneHopWF("DONE", nil, map[string]spi.StateDefinition{
		"DONE": {Transitions: []spi.TransitionDefinition{{Name: "Tick", Next: "OPEN",
			Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "tick", ResultKind: "Schedule", CalculationNodesTags: "sched"}}}}},
	}))
	run = newTestRun(env.sts, claimed)

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if got := env.state(t, "final-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (no commit after the cancellation)", got)
	}
	if got, _ := env.task(t, claimed.ID); got == nil || got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want untouched under this claim", got)
	}
}

// saveHookFactory calls onSave before every EntityStore.Save.
type saveHookFactory struct {
	spi.StoreFactory
	onSave *func()
}

type saveHookEntityStore struct {
	spi.EntityStore
	onSave *func()
}

func (f saveHookFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return saveHookEntityStore{EntityStore: es, onSave: f.onSave}, nil
}

func (s saveHookEntityStore) Save(ctx context.Context, e *spi.Entity) (int64, error) {
	if *s.onSave != nil {
		(*s.onSave)()
	}
	return s.EntityStore.Save(ctx, e)
}

func TestRunCancel_BeforeSegmentCommit_NoCommit(t *testing.T) {
	var onSave func()
	ext := &scriptedExtProc{}
	env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
		return saveHookFactory{StoreFactory: f, onSave: &onSave}
	}, nil)
	// p1's flush is a CompareAndSave (the first segment applies the
	// precondition); p2's flush is the first Save.
	claimed := env.claimed(t, "segc-e1", oneHopWF("MID",
		[]spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch)},
		map[string]spi.StateDefinition{"MID": autoStep("DONE", safeProc("p2", ExecutionModeCommitBeforeDispatch))}))
	run := newTestRun(env.sts, claimed)
	onSave = run.cancel

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0 (its segment must not commit)", n)
	}
	if got := env.state(t, "segc-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestRunCancel_CalloutsAfterCBDSeeTheCancellation(t *testing.T) {
	type probe struct{ saw bool }
	for _, tc := range []struct {
		name  string
		wire  func(ext *scriptedExtProc, run **testRun, p *probe)
		after spi.StateDefinition
	}{
		{"processor", func(ext *scriptedExtProc, run **testRun, p *probe) {
			ext.processor = func(ctx context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
				if proc.Name != "p2" {
					return nil, nil
				}
				(*run).cancel()
				p.saw = sawCancel(ctx)
				return nil, ctx.Err()
			}
		}, autoStep("DONE", safeProc("p2", ExecutionModeSync))},
		{"criterion", func(ext *scriptedExtProc, run **testRun, p *probe) {
			ext.criterion = func(ctx context.Context) (bool, string, error) {
				(*run).cancel()
				p.saw = sawCancel(ctx)
				return false, "", ctx.Err()
			}
		}, spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "Step", Next: "DONE", Criterion: functionCriterion}}}},
		{"schedule_function", func(ext *scriptedExtProc, run **testRun, p *probe) {
			ext.function = func(ctx context.Context) (contract.FunctionResult, error) {
				(*run).cancel()
				p.saw = sawCancel(ctx)
				return contract.FunctionResult{}, ctx.Err()
			}
		}, spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "Tick", Next: "OPEN",
			Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "tick", ResultKind: "Schedule", CalculationNodesTags: "sched"}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var run *testRun
			p := &probe{}
			ext := &scriptedExtProc{}
			tc.wire(ext, &run, p)
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "call-"+tc.name, oneHopWF("MID",
				[]spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch)},
				map[string]spi.StateDefinition{"MID": tc.after}))
			run = newTestRun(env.sts, claimed)

			assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
			if !p.saw {
				t.Error("a callout after a COMMIT_BEFORE_DISPATCH commit did not see the run's cancellation")
			}
		})
	}
}
```

  In `schedule_function`, `MID` is the stable final state that the re-arm step arms, so the Function runs inside reconcile after the CBD commit. In the other two cases `DONE` is not declared; no run reaches it, because the callout in `Step` fails the run first.

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... -run 'TestRunCancel_'`
Expected: FAIL — `BeforeProcessorDispatch` ("p2 dispatched 1 times"), `AfterTXPre_StopsAtNextStep` ("p2 dispatched 1 times": `engine.go:889` checks a `WithoutCancel` context), `BeforeFinalCommit_NoCommit` ("want failed with a cancellation": the run fires), `BeforeSegmentCommit_NoCommit` ("p2 dispatched 1 times": `engine_processors.go:493` checks a `WithoutCancel` context), and all three `CalloutsAfterCBDSeeTheCancellation` subtests ("did not see the run's cancellation").

- [ ] **Step 3: Implement.**
  - `run_guard.go`, add:

```go
// errRunCancelled marks a run stopped by the scheduler (spec §5.3).
var errRunCancelled = errors.New("scheduled run cancelled")

// runCancelled returns the error of a run stopped at a checkpoint. It
// satisfies errors.Is(err, context.Canceled), which is what the scheduler's
// recorded-error allow-list keys on (spec §5.8).
func runCancelled(where string) error {
	return fmt.Errorf("%s: %w", where, errors.Join(errRunCancelled, context.Canceled))
}

func (g *RunGuard) cancelled() bool {
	if g.Done == nil {
		return false
	}
	select {
	case <-g.Done:
		return true
	default:
		return false
	}
}

// runCheckpoint refuses to go on once the run is cancelled. It reads the
// guard, not ctx: after a COMMIT_BEFORE_DISPATCH commit the run continues on
// context.WithoutCancel segments, which never report a cancellation.
func runCheckpoint(ctx context.Context, where string) error {
	if g := RunGuardFrom(ctx); g != nil && g.cancelled() {
		return runCancelled(where)
	}
	return nil
}

// runCallCtx binds a callout to the run's cancellation, including on the
// WithoutCancel segments after a COMMIT_BEFORE_DISPATCH commit. Outside a run
// it returns ctx unchanged. The caller must call the returned cancel.
func runCallCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	g := RunGuardFrom(ctx)
	if g == nil || g.Done == nil {
		return ctx, func() {}
	}
	callCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-g.Done:
			cancel()
		case <-callCtx.Done():
		}
	}()
	return callCtx, cancel
}
```

  - `engine_processors.go` `executeProcessors`, first statement of the loop body (`:122`):

```go
	for _, proc := range processors {
		// Checkpoint before each processor dispatch (spec §5.3).
		if err := runCheckpoint(currentCtx, "processor "+proc.Name+" not dispatched"); err != nil {
			return currentCtx, currentTxID, err
		}
```

  - The four dispatch sites. `:230`:

```go
	callCtx, stop := runCallCtx(ctx)
	modifiedEntity, err := e.extProc.DispatchProcessor(callCtx, entity, proc, workflow, transition, txID)
	stop()
```

  `:269` the same with `_, dispatchErr :=`; `:360` with `runCallCtx(newCtx)`; `:392` with `runCallCtx(dispatchCtx)`.
  - `flushAndCommitSegment`, after the `ctx.Err()` check (`:493-495`) and before `ShieldedCommitWithBudget`:

```go
	// Checkpoint before each entity-transaction commit of a scheduled run
	// (spec §5.3). Not marked infra: it is the run's cancellation.
	if err := runCheckpoint(ctx, "commit-before-dispatch: segment not committed"); err != nil {
		return err
	}
```

  - `engine.go` `cascadeAutomated`, after `:889-891`:

```go
		// Checkpoint before each cascade step (spec §5.3). ctx.Err above is
		// inert after a segment commit; the guard is not.
		if err := runCheckpoint(currentCtx, "cascade aborted"); err != nil {
			return currentCtx, currentTxID, err
		}
```

  - `engine.go` `evaluateCriterion` `:1023`: `callCtx, stop := runCallCtx(cc.ctx)`, dispatch on `callCtx`, `stop()` after it.
  - `arm.go` `armViaFunction` `:243`: `callCtx, stop := runCallCtx(ctx)`, dispatch on `callCtx`, `stop()` after it.
  - `fire_scheduled.go` `commitRun`, first statement:

```go
	if err := runCheckpoint(ctx, "scheduled run not committed"); err != nil {
		return err
	}
```

- [ ] **Step 4: Run to verify GREEN**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/run_guard.go internal/domain/workflow/engine_processors.go \
  internal/domain/workflow/engine.go internal/domain/workflow/arm.go \
  internal/domain/workflow/fire_scheduled.go internal/domain/workflow/fire_cancel_test.go
git commit -m "feat(workflow): a scheduled run stops at its checkpoints and its callouts see the cancellation

The guard is checked before each processor dispatch, each cascade step and
each entity-transaction commit, and every callout of a guarded run is bound
to its cancellation, also on the segments after a COMMIT_BEFORE_DISPATCH
commit.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task E-4: the unsafe mark before every unsafe dispatch

**Spec:** §5.5 (result table; coverage of `ASYNC_NEW_TX`; the callback
anti-pattern with an unsafe processor).

**Files:**
- Modify: `internal/domain/workflow/run_guard.go`
- Modify: `internal/domain/workflow/engine_processors.go` (`:179`, `:216-242`, `:253-293`, `:359-367`, `:390-399`)
- Modify: `internal/domain/workflow/fire_scheduled.go` (`runReport`)
- Create: `internal/domain/workflow/fire_mark_test.go`

**Interfaces:**
- Consumes (S): `MarkUnsafe(ctx, ref)`, `spi.ErrMarkedByAnotherClaim`, `spi.ErrTaskBusy`, `spi.ErrStaleClaim`.
- Produces: `RunReport.MarkHeld`, `MarkErrored`, and `FailReason = FailureUnsafeWorkNotCompleted` on `ErrMarkedByAnotherClaim`; `beforeDispatch(ctx, proc) (dispatched func(stepErr error), err error)`.

- [ ] **Step 1: Write the failing tests.** `internal/domain/workflow/fire_mark_test.go`:

```go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// scriptedMarkStore counts MarkUnsafe calls and, when markErr is set,
// answers with it instead of the real store.
type scriptedMarkStore struct {
	spi.ScheduledTaskStore
	mu      sync.Mutex
	marks   int
	markErr error
}

func (s *scriptedMarkStore) MarkUnsafe(ctx context.Context, ref spi.TaskRef) error {
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.marks++
	}()
	if s.markErr != nil {
		return s.markErr
	}
	return s.ScheduledTaskStore.MarkUnsafe(ctx, ref)
}

func (s *scriptedMarkStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks
}

func TestUnsafeMark_Results(t *testing.T) {
	for _, tc := range []struct {
		name            string
		markErr         error
		want            ScheduledOutcome
		wantReason      spi.ScheduledTaskFailureReason
		wantMarkErrored bool
	}{
		{"stale_claim", fmt.Errorf("fenced: %w", spi.ErrStaleClaim), OutcomeSuperseded, "", false},
		{"marked_by_another_claim", fmt.Errorf("fenced: %w", spi.ErrMarkedByAnotherClaim), OutcomeFailed, spi.FailureUnsafeWorkNotCompleted, false},
		{"task_busy", fmt.Errorf("locked: %w", spi.ErrTaskBusy), OutcomeFailed, "", false},
		{"other_error", errors.New("connection refused"), OutcomeFailed, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "mark-"+tc.name, oneHopWF("CLOSED",
				[]spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
			store := &scriptedMarkStore{ScheduledTaskStore: env.sts, markErr: tc.markErr}

			r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
			if r.Outcome != tc.want || r.FailReason != tc.wantReason || r.MarkErrored != tc.wantMarkErrored || r.MarkHeld {
				t.Fatalf("report = %+v, want %s reason=%q markErrored=%v markHeld=false", r, tc.want, tc.wantReason, tc.wantMarkErrored)
			}
			if tc.want == OutcomeFailed && r.Err == nil {
				t.Error("a failed run must carry its error")
			}
			if n := ext.count("p1"); n != 0 {
				t.Errorf("p1 dispatched %d times, want 0", n)
			}
			if got := env.state(t, "mark-"+tc.name); got != "OPEN" {
				t.Errorf("entity state = %q, want OPEN", got)
			}
		})
	}
}

func TestUnsafeMark_BeforeEveryUnsafeDispatch(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "every-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		unsafeProc("p1", ExecutionModeSync), safeProc("p2", ExecutionModeSync), unsafeProc("p3", ExecutionModeSync),
	}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}

	r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
	if r.Outcome != OutcomeFired || !r.MarkHeld {
		t.Fatalf("report = %+v, want fired with the mark held", r)
	}
	if n := store.count(); n != 2 {
		t.Errorf("MarkUnsafe calls = %d, want 2 (one per unsafe dispatch, none for the idempotent one)", n)
	}
}

func TestUnsafeMark_IdempotentProcessorIsNotMarked(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, errors.New("member failed")
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "idem-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}

	r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
	if r.Outcome != OutcomeFailed || r.MarkHeld || r.UnsafeReached || r.FailReason != "" {
		t.Fatalf("report = %+v, want a plain safe failure", r)
	}
	if n := store.count(); n != 0 {
		t.Errorf("MarkUnsafe calls = %d, want 0", n)
	}
}

func TestUnsafeMark_SupersededOwnerSendsNoUnsafeProcessor(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			return nil, env.rearm(claimed)
		}
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "sup-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync),
	}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded (the old life's mark is refused)", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0", n)
	}
}

func TestUnsafeMark_AsyncNewTxFailure_RunCommits(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, errors.New("side effect failed")
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "async-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeAsyncNewTx)}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFired || !r.MarkHeld {
		t.Fatalf("report = %+v, want fired with the mark held", r)
	}
	if _, found := env.task(t, claimed.ID); found {
		t.Error("the committed run completes the task")
	}
}

func TestCallbackAntiPattern_WritesFiredEntity_ThenUnsafe_TaskBusy(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		if proc.Name != "p1" {
			return nil, nil
		}
		// A joined callback updates the fired entity: the update re-arms its task in the run's transaction.
		jctx, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		_, err = env.sts.ReconcileForEntity(jctx, spi.ReconcileRequest{
			TenantID: testTenant, EntityID: claimed.EntityID, CurrentState: "OPEN", Arm: []spi.ScheduledTask{armable(claimed)},
		})
		return nil, err
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "busy-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync),
	}, nil))

	done := make(chan RunReport, 1)
	go func() { r, _ := env.run(claimed); done <- r }()
	var r RunReport
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run hung")
	}
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, spi.ErrTaskBusy) || r.MarkHeld || r.FailReason != "" {
		t.Fatalf("report = %+v, want a safe failure on ErrTaskBusy", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0", n)
	}
}
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... -run 'TestUnsafeMark_|TestCallbackAntiPattern_WritesFiredEntity_ThenUnsafe'`
Expected: FAIL — `Results/*` ("p1 dispatched 1 times" and wrong outcome: nothing calls `MarkUnsafe`), `BeforeEveryUnsafeDispatch` ("MarkUnsafe calls = 0"), `SupersededOwnerSendsNoUnsafeProcessor` ("p2 dispatched 1 times"), `AsyncNewTxFailure_RunCommits` (`MarkHeld` false), `…ThenUnsafe_TaskBusy` (p2 dispatched). `IdempotentProcessorIsNotMarked` passes.

- [ ] **Step 3: Implement.**
  - `run_guard.go`: add run-state fields and the mark step.

```go
type RunGuard struct {
	Ref   spi.TaskRef
	Store spi.ScheduledTaskStore
	Done  <-chan struct{}

	// Run state, written only by the run's own goroutine.
	markHeld    bool
	markErrored bool
	failReason  spi.ScheduledTaskFailureReason
}

// errUnsafeNotDispatched marks every refusal of beforeDispatch. It is fatal
// in every execution mode, ASYNC_NEW_TX included.
var errUnsafeNotDispatched = errors.New("unsafe processor not dispatched")

func notDispatched(proc spi.ProcessorDefinition, cause error) error {
	return fmt.Errorf("processor %s: %w", proc.Name, errors.Join(errUnsafeNotDispatched, cause))
}

// beforeDispatch applies spec §5.5 before a processor dispatch. Outside a
// run, and for a processor declared idempotent, it does nothing. Otherwise it
// writes the unsafe mark, even when this run already holds one: for the same
// claim the call is idempotent, and it is the check that stops a superseded
// run from sending more unsafe work. The caller dispatches only on a nil
// error, and then calls dispatched with the error its step ends with.
func beforeDispatch(ctx context.Context, proc spi.ProcessorDefinition) (dispatched func(stepErr error), err error) {
	g := RunGuardFrom(ctx)
	if g == nil || proc.Config.Idempotent {
		return func(error) {}, nil
	}
	if err := g.Store.MarkUnsafe(ctx, g.Ref); err != nil {
		switch {
		case errors.Is(err, spi.ErrStaleClaim):
			return nil, notDispatched(proc, errors.Join(errRunSuperseded, err))
		case errors.Is(err, spi.ErrMarkedByAnotherClaim):
			g.failReason = spi.FailureUnsafeWorkNotCompleted
			return nil, notDispatched(proc, err)
		case errors.Is(err, spi.ErrTaskBusy):
			g.markErrored = false
			return nil, notDispatched(proc, err)
		default:
			// The mark may have landed with its reply lost: the scheduler
			// clears this claim's mark when it records the attempt.
			g.markErrored = true
			return nil, notDispatched(proc, fmt.Errorf("failed to mark the scheduled task: %w", err))
		}
	}
	g.markHeld = true
	g.markErrored = false
	return func(error) {}, nil
}
```

  - `engine_processors.go`:
    - `:179` `nonFatal`:

```go
		nonFatal := proc.ExecutionMode == ExecutionModeAsyncNewTx &&
			!errors.Is(procErr, ErrSavepointInfra) && !errors.Is(procErr, errUnsafeNotDispatched)
```

    - `executeSyncProcessor` (`:216-242`) gets a named result and the mark before the gate is released:

```go
func (e *Engine) executeSyncProcessor(ctx context.Context, entity *spi.Entity, desc *modelDescMemo, proc spi.ProcessorDefinition, workflow, transition, txID string) (retErr error) {
	if e.extProc == nil {
		return nil
	}
	dispatched, err := beforeDispatch(ctx, proc)
	if err != nil {
		return err
	}
	defer func() { dispatched(retErr) }()
	resume := txgate.Suspend(ctx)
	// … unchanged from :229 on
```

    - `executeAsyncNewTx` (`:253-293`): named result `(retErr error)`; after the savepoint is created (`:258-261`) and before `txgate.Suspend`:

```go
	// After the savepoint: a savepoint that cannot be created dispatches nothing.
	dispatched, err := beforeDispatch(ctx, proc)
	if err != nil {
		return err
	}
	defer func() { dispatched(retErr) }()
```

    - `=true` branch (`:359-367`):

```go
		if e.extProc != nil {
			dispatched, mErr := beforeDispatch(newCtx, proc)
			if mErr != nil {
				return nil, "", mErr
			}
			callCtx, stop := runCallCtx(newCtx)
			modified, dispatchErr := e.extProc.DispatchProcessor(callCtx, entity, proc, workflow, transition, newTxID)
			stop()
			dispatched(dispatchErr)
			if dispatchErr != nil {
				return nil, "", dispatchErr
			}
			if modified != nil && modified.Data != nil {
				pending = modified.Data
			}
		}
```

    - `=false` branch (`:390-393`):

```go
		if e.extProc != nil {
			dispatched, mErr := beforeDispatch(dispatchCtx, proc)
			if mErr != nil {
				return nil, "", mErr
			}
			callCtx, stop := runCallCtx(dispatchCtx)
			modified, dispatchErr = e.extProc.DispatchProcessor(callCtx, entity, proc, workflow, transition, "")
			stop()
			dispatched(dispatchErr)
		}
```

  - `fire_scheduled.go` `runReport`:

```go
func (e *Engine) runReport(ctx context.Context, g *RunGuard, outcome ScheduledOutcome, err error) RunReport {
	r := RunReport{MarkHeld: g.markHeld, MarkErrored: g.markErrored}
	switch {
	case err == nil:
		r.Outcome = outcome
	case supersededBy(ctx, g, err):
		r.Outcome = OutcomeSuperseded
	default:
		r.Outcome = OutcomeFailed
		r.Err = err
		r.FailReason = g.failReason
	}
	return r
}
```

- [ ] **Step 4: Run to verify GREEN**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/run_guard.go internal/domain/workflow/engine_processors.go \
  internal/domain/workflow/fire_scheduled.go internal/domain/workflow/fire_mark_test.go
git commit -m "feat(workflow): mark the task before every unsafe dispatch of a scheduled run

A refused mark supersedes the run, a mark of another claim fails it
UNSAFE_WORK_NOT_COMPLETED, a busy row is a safe failure, and any other
error is a safe failure that clears this claim's mark. Mark refusals are
fatal in ASYNC_NEW_TX too.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task E-5: "unsafe work reached a compute node", the in-flight count, and no new unsafe work after the signal

**Spec:** §5.5 (the in-memory fact, `NotHandedOff`, absence of the proof),
§6.4 steps 1 and 3 (engine seams).

**Files:**
- Modify: `internal/domain/workflow/run_guard.go`
- Modify: `internal/domain/workflow/fire_scheduled.go` (`runReport`)
- Create: `internal/domain/workflow/fire_reached_test.go`

**Interfaces:**
- Consumes (K): `contract.NoHandOffProof{Err}`, `contract.ProvesNoHandOff(err) bool`.
- Produces: `RunGuard.NoNewUnsafe <-chan struct{}`, `RunGuard.Unsafe *UnsafeFlight` (`Begin`, `End`, `Since`), `(*RunGuard).UnsafeInFlight() bool`, `RunReport.UnsafeReached` (README C-G1).

- [ ] **Step 1: Write the failing tests.** `internal/domain/workflow/fire_reached_test.go`:

```go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func proofOf(cause error) error { return &contract.NoHandOffProof{Err: cause} }

func TestUnsafeReached_Fact(t *testing.T) {
	for _, tc := range []struct {
		name        string
		procs       []spi.ProcessorDefinition
		script      func(name string) (*spi.Entity, error)
		wantReached bool
	}{
		{"no_member_proof_resets", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, proofOf(errors.New("no compute member for tag")) }, false},
		{"error_without_proof_stays", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, errors.New("member failed") }, true},
		{"later_proof_does_not_reset", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync)},
			func(name string) (*spi.Entity, error) {
				if name == "p1" {
					return nil, nil
				}
				return nil, proofOf(errors.New("no compute member for tag"))
			}, true},
		{"cancelled_before_send", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, proofOf(context.Canceled) }, false},
		{"cancelled_after_send", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, fmt.Errorf("callout: %w", context.Canceled) }, true},
		{"apply_fails_after_successful_dispatch", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return &spi.Entity{Data: []byte("{not json")}, nil }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
				return tc.script(proc.Name)
			}}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "fact-"+tc.name, oneHopWF("CLOSED", tc.procs, nil))

			r, _ := env.run(claimed)
			if r.Outcome != OutcomeFailed || !r.MarkHeld || r.UnsafeReached != tc.wantReached {
				t.Fatalf("report = %+v, want failed, markHeld, unsafeReached=%v", r, tc.wantReached)
			}
		})
	}
}

// failingRollbackToSavepointTxMgr cannot undo a savepoint.
type failingRollbackToSavepointTxMgr struct{ spi.TransactionManager }

func (failingRollbackToSavepointTxMgr) RollbackToSavepoint(context.Context, string, string) error {
	return errors.New("savepoint gone")
}

func TestUnsafeReached_SavepointErrorReplacesProof(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, proofOf(errors.New("no compute member for tag"))
	}}
	env := newRunEnvWith(t, ext, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		return failingRollbackToSavepointTxMgr{TransactionManager: tm}
	})
	claimed := env.claimed(t, "sp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeAsyncNewTx)}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, ErrSavepointInfra) || !r.UnsafeReached {
		t.Fatalf("report = %+v, want failed on the savepoint with unsafeReached (the proof was replaced)", r)
	}
}

func TestUnsafeReached_NoComputeNode_ThenFires(t *testing.T) {
	down := true
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		if down {
			return nil, proofOf(errors.New("no compute member for tag"))
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "retry-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))

	r, _ := env.run(first)
	if r.Outcome != OutcomeFailed || !r.MarkHeld || r.UnsafeReached {
		t.Fatalf("first run = %+v, want a safe failure with the mark held and nothing reached", r)
	}
	// The scheduler's bookkeeping for that row (spec §5.6).
	ref := spi.TaskRef{TenantID: testTenant, ID: first.ID, ArmToken: first.ArmToken, ClaimToken: first.Claim.Token}
	if err := env.sts.RecordAttempt(env.ctx, ref, spi.Attempt{Error: "NO_COMPUTE_MEMBER_FOR_TAG", AtMs: env.nowMs(), NextAttemptTime: env.nowMs(), ClearOwnMark: true}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	down = false
	second := env.claimOne(t, testOwner, false)
	if second.UnsafeMarked || second.Attempts != 1 {
		t.Fatalf("second claim = %+v, want no mark and attempts 1", second)
	}

	r, _ = env.run(second)
	if r.Outcome != OutcomeFired {
		t.Fatalf("second run = %+v, want fired", r)
	}
	if got := env.state(t, "retry-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
}

func TestUnsafeMark_MarkedTaskIsNeverRerun(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, errors.New("member failed")
	}}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "never-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
	if r, _ := env.run(first); !r.UnsafeReached {
		t.Fatalf("first run = %+v, want unsafeReached", r)
	}
	// The owner dies before its bookkeeping: another pnode reclaims the task.
	second := env.claimOne(t, otherOwner, true)
	if !second.UnsafeMarked {
		t.Fatalf("reclaimed record = %+v, want UnsafeMarked", second)
	}

	r, _ := env.run(second)
	if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureUnsafeWorkNotCompleted {
		t.Fatalf("second run = %+v, want FAILED UNSAFE_WORK_NOT_COMPLETED", r)
	}
	if n := ext.count("p1"); n != 1 {
		t.Errorf("p1 dispatched %d times, want 1", n)
	}
}

func TestUnsafeMark_NoNewUnsafe_CountsAsCut(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "drain-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p0", ExecutionModeSync), unsafeProc("p1", ExecutionModeSync),
	}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}
	run := newTestRun(store, claimed)
	signalled := make(chan struct{})
	close(signalled)
	run.guard.NoNewUnsafe = signalled

	r := run.fire(env.engine, env.ctx, claimed)
	assertCancelled(t, r)
	if ext.count("p0") != 1 || ext.count("p1") != 0 || store.count() != 0 {
		t.Errorf("dispatches p0=%d p1=%d marks=%d, want 1, 0, 0", ext.count("p0"), ext.count("p1"), store.count())
	}
	if r.UnsafeReached {
		t.Error("nothing unsafe was sent")
	}
}

func TestUnsafeReached_InFlightIsVisible(t *testing.T) {
	var duringUnsafe, duringSafe bool
	ext := &scriptedExtProc{processor: func(ctx context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		switch proc.Name {
		case "p1":
			duringUnsafe = RunGuardFrom(ctx).UnsafeInFlight()
		case "p2":
			duringSafe = RunGuardFrom(ctx).UnsafeInFlight()
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "inflight-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		unsafeProc("p1", ExecutionModeSync), safeProc("p2", ExecutionModeSync),
	}, nil))

	r, run := env.run(claimed)
	if r.Outcome != OutcomeFired {
		t.Fatalf("report = %+v, want fired", r)
	}
	if !duringUnsafe || duringSafe || run.guard.UnsafeInFlight() {
		t.Errorf("in flight: during unsafe=%v, during safe=%v, after=%v; want true, false, false",
			duringUnsafe, duringSafe, run.guard.UnsafeInFlight())
	}
}
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... -run 'TestUnsafeReached_|TestUnsafeMark_MarkedTaskIsNeverRerun|TestUnsafeMark_NoNewUnsafe'`
Expected: FAIL — build errors `run.guard.NoNewUnsafe undefined` and `RunGuardFrom(ctx).UnsafeInFlight undefined`; with those two lines stubbed out, `TestUnsafeReached_Fact` fails on every `wantReached=true` row (the field is never set).

- [ ] **Step 3: Implement.**
  - `run_guard.go`: add to the struct and replace `beforeDispatch`. Add
    `"sync"` and `"time"` to the file's imports (E-1 created it with
    `context`, `errors`, `fmt` and the SPI only).

```go
type RunGuard struct {
	Ref   spi.TaskRef
	Store spi.ScheduledTaskStore
	Done  <-chan struct{}
	// NoNewUnsafe is closed at shutdown step 1: from then on the run starts
	// no new unsafe dispatch, and reaching one counts as cut (spec §6.4).
	// nil means never.
	NoNewUnsafe <-chan struct{}

	// Run state, written only by the run's own goroutine.
	markHeld      bool
	markErrored   bool
	unsafeReached bool
	failReason    spi.ScheduledTaskFailureReason
	// Unsafe records each unsafe dispatch in flight; the scheduler reads it at
	// shutdown steps 3 and 4 (README C-G1). nil is allowed (tests) and records
	// nothing.
	Unsafe *UnsafeFlight
}

// UnsafeFlight counts the run's unsafe dispatches in flight and remembers when
// the oldest one started. All methods are safe on a nil receiver.
type UnsafeFlight struct {
	mu    sync.Mutex
	n     int
	since time.Time
}

func (f *UnsafeFlight) Begin() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.since = time.Now()
	}
	f.n++
}

func (f *UnsafeFlight) End() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n > 0 {
		f.n--
	}
}

// Since returns the start of the oldest unsafe dispatch in flight, and false
// when none is.
func (f *UnsafeFlight) Since() (time.Time, bool) {
	if f == nil {
		return time.Time{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since, f.n > 0
}

// UnsafeInFlight reports whether an unsafe processor dispatch of this run is
// in progress. The scheduler exempts such a run at shutdown step 3 (spec
// §6.4). A dispatch counts from before its mark until its step's error is
// known. After NoNewUnsafe is closed the count only falls: beforeDispatch
// counts first and checks NoNewUnsafe second.
func (g *RunGuard) UnsafeInFlight() bool { _, ok := g.Unsafe.Since(); return ok }

func (g *RunGuard) noNewUnsafe() bool {
	if g.NoNewUnsafe == nil {
		return false
	}
	select {
	case <-g.NoNewUnsafe:
		return true
	default:
		return false
	}
}

func beforeDispatch(ctx context.Context, proc spi.ProcessorDefinition) (dispatched func(stepErr error), err error) {
	g := RunGuardFrom(ctx)
	if g == nil || proc.Config.Idempotent {
		return func(error) {}, nil
	}
	g.Unsafe.Begin()
	if g.cancelled() || g.noNewUnsafe() {
		g.Unsafe.End()
		return nil, notDispatched(proc, runCancelled("unsafe dispatch after the run was stopped"))
	}
	if err := g.Store.MarkUnsafe(ctx, g.Ref); err != nil {
		g.Unsafe.End()
		switch {
		case errors.Is(err, spi.ErrStaleClaim):
			return nil, notDispatched(proc, errors.Join(errRunSuperseded, err))
		case errors.Is(err, spi.ErrMarkedByAnotherClaim):
			g.failReason = spi.FailureUnsafeWorkNotCompleted
			return nil, notDispatched(proc, err)
		case errors.Is(err, spi.ErrTaskBusy):
			g.markErrored = false
			return nil, notDispatched(proc, err)
		default:
			g.markErrored = true
			return nil, notDispatched(proc, fmt.Errorf("failed to mark the scheduled task: %w", err))
		}
	}
	g.markHeld = true
	g.markErrored = false
	// "Unsafe work reached a compute node" (spec §5.5): set before every
	// unsafe dispatch, reset only by the NotHandedOff proof on the error the
	// step ends with, and only if it was false before this dispatch. A
	// success, a panic, and every error without the proof leave it set.
	reachedBefore := g.unsafeReached
	g.unsafeReached = true
	return func(stepErr error) {
		g.Unsafe.End()
		if !reachedBefore && contract.ProvesNoHandOff(stepErr) {
			g.unsafeReached = false
		}
	}, nil
}
```

  Imports gain `sync/atomic` and `github.com/cyoda-platform/cyoda-go/internal/contract`. The dispatch sites of E-4 already pass the step's final error: `executeSyncProcessor` and `executeAsyncNewTx` through their deferred `dispatched(retErr)` (so a savepoint error that replaces the dispatch error, and a failure of `applyProcessorData` after a successful dispatch, reach it without the proof); the two CBD branches with the dispatch error, after which any later failure in the step leaves the fact set.
  - `fire_scheduled.go` `runReport`: `r := RunReport{MarkHeld: g.markHeld, MarkErrored: g.markErrored, UnsafeReached: g.unsafeReached}`.

- [ ] **Step 4: Run to verify GREEN**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/run_guard.go internal/domain/workflow/fire_scheduled.go \
  internal/domain/workflow/fire_reached_test.go
git commit -m "feat(workflow): track whether unsafe work reached a compute node

The fact is set before each unsafe dispatch and reset only by the
callout layer's NotHandedOff proof on the step's final error. The guard
exposes the in-flight unsafe dispatch to the scheduler and refuses new
unsafe dispatches after the shutdown signal.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task E-6: the segment stamp and `PartialCommit`

**Spec:** §5.2 ("Every commit writes the task row"), §5.4, §5.5 (callback
anti-pattern in a segmented run), §14.

**Prerequisite: the invariant I-599.** This task does not depend on *how* #599
is fixed. It depends on one guarantee:

> **I-599.** While a scheduled run's transaction is open, only the run's own
> processor chain can commit it. A compute-node callback that joined the
> transaction never causes a commit of that transaction, directly or through a
> `COMMIT_BEFORE_DISPATCH` processor it reaches (today it can:
> `flushAndCommitSegment` commits whatever transaction the chain runs in,
> `engine_processors.go:481-486`, called at `:311` and `:349`).

- [ ] **Step 0: Check I-599 against the merged #599.**
  1. Run: `gh issue view 599 --json state -q .state && git fetch origin && git log --oneline HEAD..origin/release/v0.9.0 | head`.
     Expected: `CLOSED` and an empty log (this branch contains the release branch).
     If not: **stop, and tell the lead.**
  2. Read #599's merged change (`gh issue view 599 --comments`, then the PR it
     names). Decide which case applies:
     - **(a) A joined chain that reaches `COMMIT_BEFORE_DISPATCH` is refused**
       before anything is flushed (the fix the issue asks for). I-599 holds.
       Go on with Step 1, and add the pinning test in Step 1b.
     - **(b) The joined chain's `COMMIT_BEFORE_DISPATCH` commits something
       other than the joined transaction** (for example its own transaction).
       I-599 still holds, because the run's transaction is not committed.
       Go on with Step 1 and Step 1b.
     - **(c) Anything else**: some path still lets a joined callback commit the
       transaction it joined; the refusal comes only after the flush; or the
       fix is an import-time rule that leaves a runtime path open. I-599 does
       not hold. **Stop, and tell the lead.** The fallback, which changes the
       spec and needs the product owner's approval, is to attach the run guard
       to the *transaction* rather than to the context. Every commit of a
       guarded transaction is then stamped and checks the cancellation, from
       whichever chain it comes. A commit from a chain that is not the run's
       own counts as `partial = true`, because that chain cannot know whether
       the fired transition has changed the state yet.
  3. Rebase the branch on the release branch that contains #599 and re-check
     the line numbers this task cites in `engine_processors.go`. #599 edits
     that file.

- [ ] **Step 1b: Pin I-599 inside a scheduled run.** Add the test below to
  `fire_stamp_test.go`, and adapt the assertion on `err` to the error #599
  returns in case (a) (in case (b), assert only what follows the `err` check).
  The scripted processor `p1` joins the run's transaction and, through that
  join, runs an ordinary workflow on a second entity whose transition has a
  `COMMIT_BEFORE_DISPATCH` processor. Use the same join-and-run helper as
  #599's own tests.

```go
func TestI599_CallbackCannotCommitTheRunsTransaction(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	var callbackErr error
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		if proc.Name != "p1" {
			return nil, nil
		}
		callbackErr = env.runJoinedCBDWorkflow(t, txID, "other-e1") // #599's helper, adapted
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "i599-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	r := env.runReport(t, claimed)
	if callbackErr == nil {
		t.Fatalf("the joined chain reached COMMIT_BEFORE_DISPATCH and was not refused (I-599)")
	}
	// Whatever the run's outcome, nothing of its transaction may be committed
	// by the callback: the fired entity is either CLOSED by the run's own
	// commit, or still OPEN.
	if got := env.state(t, "i599-e1"); got != "CLOSED" && got != "OPEN" {
		t.Fatalf("entity state = %q", got)
	}
	if r.Outcome == OutcomeFired && env.state(t, "i599-e1") != "CLOSED" {
		t.Fatalf("fired, but the entity is not CLOSED")
	}
}
```

**Files:**
- Modify: `internal/domain/workflow/run_guard.go`
- Modify: `internal/domain/workflow/engine_processors.go` (`flushAndCommitSegment`, `:469-515`)
- Modify: `internal/domain/workflow/fire_scheduled.go` (after `fireTransition`; `runReport`)
- Create: `internal/domain/workflow/fire_stamp_test.go`

**Interfaces:**
- Consumes (S): `StampSegment(ctx, ref, partial)` (joins; fenced).
- Produces: `RunReport.PartialCommit`.

- [ ] **Step 1: Write the failing tests.** `internal/domain/workflow/fire_stamp_test.go`:

```go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func failing(name string) func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
	return func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == name {
			return nil, errors.New(name + " failed")
		}
		return nil, nil
	}
}

func TestStamp_FiredTransitionSegment_NotPartial_RetriedFromTXPre(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "txpre-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeCommitBeforeDispatch), safeProc("p2", ExecutionModeSync),
	}, nil))

	r, _ := env.run(first)
	if r.Outcome != OutcomeFailed || r.PartialCommit {
		t.Fatalf("first run = %+v, want a safe failure without PartialCommit", r)
	}
	if got, _ := env.task(t, first.ID); got.PartialCommit {
		t.Errorf("stored task = %+v, want PartialCommit false", got)
	}
	if got := env.state(t, "txpre-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre)", got)
	}
	ref := spi.TaskRef{TenantID: testTenant, ID: first.ID, ArmToken: first.ArmToken, ClaimToken: first.Claim.Token}
	if err := env.sts.RecordAttempt(env.ctx, ref, spi.Attempt{Error: "p2 failed", AtMs: env.nowMs(), NextAttemptTime: env.nowMs()}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	ext.processor = nil
	r, _ = env.run(env.claimOne(t, testOwner, false))
	if r.Outcome != OutcomeFired {
		t.Fatalf("retry = %+v, want fired", r)
	}
}

func cascadePartialWF() spi.WorkflowDefinition {
	return oneHopWF("MID", nil, map[string]spi.StateDefinition{
		"MID": autoStep("DONE", safeProc("p1", ExecutionModeCommitBeforeDispatch), safeProc("p2", ExecutionModeSync)),
	})
}

func TestStamp_CascadeStepSegment_SetsPartialCommit(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "cascade-e1", cascadePartialWF())

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !r.PartialCommit {
		t.Fatalf("report = %+v, want failed with PartialCommit", r)
	}
	if got, _ := env.task(t, claimed.ID); !got.PartialCommit {
		t.Errorf("stored task = %+v, want PartialCommit true", got)
	}
	if got := env.state(t, "cascade-e1"); got != "MID" {
		t.Errorf("entity state = %q, want MID (committed by the cascade step's segment)", got)
	}
}

func TestStamp_NextClaimAfterPartialCommit_Failed(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "killed-e1", cascadePartialWF())
	env.run(first) // the owner then dies without bookkeeping

	second := env.claimOne(t, otherOwner, true)
	r, _ := env.run(second)
	if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureStoppedAfterPartialCommit {
		t.Fatalf("report = %+v, want FAILED STOPPED_AFTER_PARTIAL_COMMIT", r)
	}
	if n := ext.count("p1"); n != 1 {
		t.Errorf("p1 dispatched %d times, want 1", n)
	}
}

func TestStamp_CascadeBackInSourceState_SetsPartialCommit(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	wf := oneHopWF("MID", nil, map[string]spi.StateDefinition{"MID": autoStep("OPEN")})
	open := wf.States["OPEN"]
	open.Transitions = append(open.Transitions, spi.TransitionDefinition{Name: "Leave", Next: "GONE",
		Processors: []spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch), safeProc("p2", ExecutionModeSync)}})
	wf.States["OPEN"] = open
	wf.States["GONE"] = spi.StateDefinition{}
	claimed := env.claimed(t, "back-e1", wf)

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !r.PartialCommit {
		t.Fatalf("report = %+v, want PartialCommit: the segment was committed by the cascade, back in the source state", r)
	}
}

func TestStamp_ReplacedOwnerSegmentRefused(t *testing.T) {
	var env *runEnv
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name != "p1" {
			return nil, nil
		}
		got, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
		if err == nil && len(got) != 1 {
			err = fmt.Errorf("reclaim claimed %d tasks", len(got))
		}
		return nil, err
	}}
	env = newRunEnv(t, ext)
	startNewTx := true
	p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
	p1.Config.StartNewTxOnDispatch = &startNewTx
	claimed := env.claimed(t, "replaced-e1", oneHopWF("MID", nil, map[string]spi.StateDefinition{
		"MID": autoStep("DONE", p1, safeProc("p2", ExecutionModeCommitBeforeDispatch)),
	}))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded (p2's stamp is refused)", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0: its segment must not commit", n)
	}
	if got, _ := env.task(t, claimed.ID); got.Claim == nil || got.Claim.Owner != otherOwner {
		t.Errorf("task = %+v, want RUNNING under the other owner", got)
	}
}

func TestStamp_CallbackReArmedInRunTx_StampRefused(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		if proc.Name != "p0" {
			return nil, nil
		}
		jctx, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		_, err = env.sts.ReconcileForEntity(jctx, spi.ReconcileRequest{
			TenantID: testTenant, EntityID: claimed.EntityID, CurrentState: "OPEN", Arm: []spi.ScheduledTask{armable(claimed)},
		})
		return nil, err
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbstamp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p0", ExecutionModeSync), safeProc("p1", ExecutionModeCommitBeforeDispatch),
	}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, spi.ErrStaleClaim) {
		t.Fatalf("report = %+v, want an ordinary failure: the refusal came from the run's own transaction", r)
	}
	if n := ext.count("p1"); n != 0 {
		t.Errorf("p1 dispatched %d times, want 0", n)
	}
	if got, _ := env.task(t, claimed.ID); got.ArmToken != claimed.ArmToken || got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want the claimed life, still under this claim", got)
	}
}
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... -run 'TestStamp_'`
Expected: FAIL — `CascadeStepSegment_SetsPartialCommit` and `CascadeBackInSourceState_SetsPartialCommit` ("want … PartialCommit"), `NextClaimAfterPartialCommit_Failed` (the run fires or fails without the reason; the store never saw a stamp), `ReplacedOwnerSegmentRefused` ("p2 dispatched 1 times"), `CallbackReArmedInRunTx_StampRefused` (outcome superseded: without the stamp TX_pre commits the callback's re-arm and TX_post's re-read then sees the new life). `FiredTransitionSegment_NotPartial_RetriedFromTXPre` passes and guards the fired-transition case.

- [ ] **Step 3: Implement.**
  - `run_guard.go`, run state:

```go
	// firedTransitionDone is set once the fired transition has changed the
	// state. Every segment committed after it sets PartialCommit (spec §5.4),
	// also a cascade that loops back into the source state.
	firedTransitionDone bool
	partialCommitted    bool
```

  - `fire_scheduled.go`, right after the `fireErr` block that follows `e.fireTransition(...)`:

```go
	g.firedTransitionDone = true
```

  and `runReport`: add `PartialCommit: g.partialCommitted` to the literal.
  - `engine_processors.go` `flushAndCommitSegment` (`:469-515`), after the Save/CompareAndSave block and before the `ctx.Err()` check; and after the commit:

```go
	// A scheduled run stamps its task as the last write of every segment
	// (spec §5.2). The stamp is fenced; a refused stamp stops the segment from
	// committing, and the run classifies the refusal with a non-joining
	// re-read (fire_scheduled.go supersededBy).
	g := RunGuardFrom(ctx)
	if g != nil {
		if err := g.Store.StampSegment(ctx, g.Ref, g.firedTransitionDone); err != nil {
			if errors.Is(err, spi.ErrStaleClaim) || errors.Is(err, spi.ErrConflict) {
				return fmt.Errorf("commit-before-dispatch: stamp scheduled task: %w", err)
			}
			return fmt.Errorf("commit-before-dispatch: stamp scheduled task: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit-before-dispatch: context expired before segment commit: %w", err)
	}
	if err := runCheckpoint(ctx, "commit-before-dispatch: segment not committed"); err != nil {
		return err
	}
	if err := common.ShieldedCommitWithBudget(ctx, e.commitBudget, func(commitCtx context.Context) error {
		if err := e.txMgr.Commit(commitCtx, txID); err != nil {
			return fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
		return nil
	}); err != nil {
		return err
	}
	if g != nil && g.firedTransitionDone {
		g.partialCommitted = true
	}
	return nil
```

  (the existing comments of `:487-508` stay in front of the `ctx.Err()` check and the commit.)

- [ ] **Step 4: Run to verify GREEN**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/run_guard.go internal/domain/workflow/engine_processors.go \
  internal/domain/workflow/fire_scheduled.go internal/domain/workflow/fire_stamp_test.go
git commit -m "feat(workflow): stamp the task as the last write of every run segment

The fenced stamp stops a replaced owner's segment from committing, and
records PartialCommit for every segment committed after the fired
transition changed the state.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task E-7: a joined callback that writes or deletes the fired entity

**Spec:** §5.2 ("`RemoveLife` removes only the life it names"), §13 rows "joined
callback writes the fired entity, no unsafe processor follows → same outcome as an
ordinary transition" and "joined callback deletes the fired entity → the run
commits". **Lead ruling (README Corrections, C-E1):** the workflow docs allow a
processor to write its own entity through a callback and return no mutations
(`help/workflows.md:185-192`), so a scheduled run must support it.

**Why it fails today.** A write in the run's own transaction stamps the entity
with that transaction's id (memory `plugins/memory/entity_store.go:331-345`; the
same value PostgreSQL's own uncommitted row carries). The final
`CompareAndSave(entity, expectedTxID)` then compares the last *committed* id with
the transaction's own buffered version and conflicts; a same-transaction delete
conflicts too (`:324-326`).

**The rule.** Immediately before `removeOwnLife`, the final segment re-reads the
fired entity inside its transaction:
- not found → this transaction deleted it (a delete by another transaction is
  invisible under the snapshot and conflicts at commit instead). Remove the life,
  skip the re-arm and the persist, record no FIRE event, commit, outcome
  `cancelled`.
- `Meta.TransactionID == finalTxID` → written earlier in this transaction.
  Persist with `CompareAndSave(entity, finalTxID)`: the engine's result is the
  last writer inside the transaction, as for an ordinary transition, and a write
  by another transaction still fails at commit (C1 on the entity).
- otherwise → the existing branches (segmented `Save`, else
  `CompareAndSave(entity, expectedTxID)`).

**Files:**
- Modify: `internal/domain/workflow/fire_scheduled.go` (the final-persist block
  written in E-1)
- Create: `internal/domain/workflow/fire_callback_test.go`

- [ ] **Step 1: Write the failing tests.**

```go
package workflow

import (
	"context"
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
}
```

`env.exists(t, id) bool` is added to the `runEnv` helpers in this step if E-1
did not add it: a committed-state `EntityStore.Get` that returns `false` on
`spi.ErrNotFound`.

- [ ] **Step 2: Run them to verify they fail**
Run: `go test ./internal/domain/workflow/... -run 'TestCallback_'`
Expected: FAIL — both reports show `Outcome: failed` with `spi.ErrConflict` from
the final `CompareAndSave`.

- [ ] **Step 3: Implement.** In `fire_scheduled.go`, replace the block that starts
at `if err := removeOwnLife(finalCtx, g); err != nil {` and ends after the
`CompareAndSave` branch with:

```go
	finalEntityStore, err := e.factory.EntityStore(finalCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get entity store for persist: %w", err)
	}
	// A joined callback may have written or deleted the fired entity earlier in
	// this transaction. A write or delete by another transaction is invisible
	// here and fails at commit instead.
	current, err := finalEntityStore.Get(finalCtx, entity.Meta.ID)
	if errors.Is(err, spi.ErrNotFound) {
		if err := removeOwnLife(finalCtx, g); err != nil {
			return "", err
		}
		return OutcomeCancelled, commit(finalTxID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to re-read fired entity: %w", err)
	}
	writtenHere := current.Meta.TransactionID == finalTxID

	if err := removeOwnLife(finalCtx, g); err != nil {
		return "", err
	}
	if err := e.reconcileScheduledTasks(finalCtx, entity, wf, finalTxID, auditStore, task.ID); err != nil {
		return "", fmt.Errorf("failed to reconcile scheduled tasks after fire: %w", err)
	}

	switch {
	case writtenHere:
		if _, err := finalEntityStore.CompareAndSave(finalCtx, entity, finalTxID); err != nil {
			return "", err
		}
	case finalTxID != txID:
		// Segmented: the first segment flush already applied the precondition.
		if _, err := finalEntityStore.Save(finalCtx, entity); err != nil {
			return "", fmt.Errorf("failed to save fired entity: %w", err)
		}
	default:
		if _, err := finalEntityStore.CompareAndSave(finalCtx, entity, expectedTxID); err != nil {
			return "", err
		}
	}
```

(`errors` is already imported by `fire_scheduled.go`.)

- [ ] **Step 4: Run**
Run: `go test ./internal/domain/workflow/...`
Expected: PASS, including the E-1 tests.

- [ ] **Step 5: Run the same two rows on SQLite and PostgreSQL.** They are the
spec's S and E rows for this case and live in the `spitest`-independent e2e
test T owns (`T-scenarios.md`, the callback rows); run
`go test ./internal/e2e/... -run 'Callback'` once T-… has landed.

- [ ] **Step 6: Commit**

```
git add internal/domain/workflow/fire_scheduled.go internal/domain/workflow/fire_callback_test.go
git commit -m "fix(workflow): a scheduled run supports a processor that writes its own entity

The final persist re-reads the fired entity inside the run's transaction: a
same-transaction write is compared against this transaction's id, and a
same-transaction delete commits without re-creating the entity.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Stream interface summary

**E produces (for R and the others)**

```go
// run_guard.go
type RunGuard struct {
	Ref         spi.TaskRef
	Store       spi.ScheduledTaskStore
	Done        <-chan struct{} // self-cancel, panic latch, shutdown step 3
	NoNewUnsafe <-chan struct{} // closed at shutdown step 1; nil = never   (addition, E-5)
	// unexported run state
}
func WithRunGuard(ctx context.Context, g *RunGuard) context.Context
func (g *RunGuard) UnsafeInFlight() bool                                    // addition, E-5
func RunGuardFrom(ctx context.Context) *RunGuard

// fire_scheduled.go
type RunReport struct {
	Outcome       ScheduledOutcome
	Err           error
	MarkHeld      bool
	UnsafeReached bool
	MarkErrored   bool
	PartialCommit bool
	FailReason    spi.ScheduledTaskFailureReason
}
const OutcomeFired, OutcomeDeclined, OutcomeExpired, OutcomeCancelled, OutcomeSuperseded, OutcomeFailed ScheduledOutcome
func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) RunReport

// arm.go
func modelHasSchedule(wfs []spi.WorkflowDefinition) bool
```

Removed: `OutcomeDropped`, `WithExpiryGrace`, `defaultExpiryGraceMs`,
`Engine.expiryGraceMs`, `workflowHasSchedule`, `fireTransition`'s `matched`
result.

**What R must do with them**
- Build the run context from its run context with
  `common.SystemUserContext(task.TenantID)` values, and attach
  `&RunGuard{Ref: {task.TenantID, task.ID, task.ArmToken, task.Claim.Token}, Store, Done, NoNewUnsafe}`
  with `WithRunGuard`. The engine no longer checks the tenant (spec §5.2); the
  re-read is tenant-scoped through `Ref.TenantID`.
- `Done` must close on self-cancel, on the panic latch and at shutdown step 3
  for runs where `UnsafeInFlight()` is false. `NoNewUnsafe` closes at step 1,
  before step 3 reads `UnsafeInFlight()`.
- Record every `OutcomeFailed` report per §5.6. `FailReason` set → `Fail(FailReason)`
  (after the panic row). It is set both for a §5.1 decision (then `Err` is nil)
  and for `ErrMarkedByAnotherClaim` during the run.
- Write `Fail` together with the `SCHEDULED_TRANSITION_FAIL` audit event (§5.7).
- A run cancelled by the scheduler returns an `Err` that satisfies
  `errors.Is(err, context.Canceled)`.

**E consumes**
- S: the §10.1 interface and types, `ErrStaleClaim`, `ErrMarkedByAnotherClaim`,
  `ErrTaskBusy`, the failure reasons.
- BM: memory store behaviour listed under "Tests: store and fixtures".
- K: `contract.NoHandOffProof`, `contract.ProvesNoHandOff` (E-5).
- #599 (E-6).

## Open points

1. **Fold into `interfaces.md`:** `RunGuard.NoNewUnsafe` and
   `(*RunGuard).UnsafeInFlight()`. The engine needs a signal for shutdown step 1
   ("from step 1 on, no run starts a new unsafe dispatch", §6.4) that is
   separate from `Done`, and the scheduler needs to know at step 3 which runs
   have an unsafe callout in flight. Neither is in the binding list.
2. **`RunReport.FailReason` comment in `interfaces.md`** says "before running".
   It is also set when `MarkUnsafe` answers `ErrMarkedByAnotherClaim` during the
   run (spec §5.5 table). R's `decideBookkeeping` must check it before the
   §5.6 rows other than the panic row.
3. **Resolved (README Corrections C-E1):** the final persist re-reads the fired entity; see E-7.
4. **A failed non-joining re-read** (spec §5.2: "retried as §5.6 retries").
   The engine re-reads once; if that read fails, it reports `OutcomeFailed`.
   The scheduler's bookkeeping write is fenced and retried per §5.6, so a run
   that was in fact superseded is refused there and ends the same way. This
   keeps the retry loop and the shutdown deadline in one place (R). Confirm.
5. **Build between waves.** After E-1 the root module does not build until R
   removes `internal/cluster/scheduler_rpc.go`, `internal/scheduler`'s old
   executor and `app/app.go:586`. It is already broken by S's removals
   (`ScanDue`, `MarkRedispatch`, `Upsert`). E verifies with package-scoped
   tests; `make test` is green only after R.
6. **`modelHasSchedule` counts every stored workflow, active or not, and every
   transition with a `Schedule`, manual or disabled included.** It is
   conservative: it runs reconcile more often, never less. W's
   `DeleteForModel(keep)` at import should use the same predicate so that a task
   the import keeps is always reconciled at the next write.
7. **Outcome of "entity gone, or moved on".** The spec table gives no outcome
   name; this section uses `OutcomeCancelled` (no audit), so the
   `cyoda.scheduler.runs` counter records it as `cancelled`. Confirm.
8. **README Review Focus item 3** names "Owner: E-8" for the callback followed
   by an unsafe processor. In this section that test is in E-4
   (`TestCallbackAntiPattern_WritesFiredEntity_ThenUnsafe_TaskBusy`), and the
   rows without an unsafe processor are E-7. Update the README.
9. **Removed guards need no replacement check.** The tenant guard is replaced by
   the tenant-scoped re-read (`TestFireScheduled_GuardOfAnotherTenant_SeesNoTask`);
   the grace band, the `ArmedBy` verify and the re-armed-future guard by the
   claim and the re-read (spec §5.2). `fireTransition`'s `matched` result is
   removed because its only reader was the unreachable branch at
   `fire_scheduled.go:446-452` (Gate 6).
10. **A panic inside a CBD dispatch leaves `unsafeInFlight` above zero** (the
    two CBD branches call `dispatched` explicitly, not deferred). The run is
    `RUN_PANICKED` and is never given back; R must not wait on
    `UnsafeInFlight()` of a run whose goroutine has already recovered a panic.
