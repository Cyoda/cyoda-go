# Stream W — entity deletes, update conflicts, workflow import

Packages: `internal/common` (the retry), `internal/testing/taskconflict` (new,
test support), `internal/domain/entity`, `internal/domain/workflow`
(`handler.go`, a new `import_tasks.go`), `internal/grpc` (tests only), `internal/e2e` (tests
only), `e2e/parity/scheduledtransition` (tests only), `api/openapi.yaml`,
`cmd/cyoda/help/content/errors/CONFLICT.md`.

Spec: §7 (workflow import, entity delete and its retry table, "a client write
can get a retryable 409"), §8.1, §13 "Entity writes and workflow import".

Depends on: **S** (`DeleteForEntities`, `DeleteForModel`, `Query`, the new
`ReconcileForEntity` semantics), **BM / BQ / BP** (the three stores implement
them; BP gives the table columns the e2e helpers touch). W-8 also needs
**Q-2** (`GET /scheduled-tasks` is how a parity scenario sees task rows).

## Facts this stream relies on, read in the code

- Every delete path already opens its transaction through `beginScope`
  (`internal/domain/entity/txscope.go:53`), which joins a transaction already
  on the context (`handler.go:107-113`). "Owned" is therefore exactly
  `spi.GetTransaction(ctx) == nil` before the call.
- The delete paths and their commit-conflict mapping:
  - `DeleteEntity` `service.go:643`, commit conflict `:687-689`;
  - `DeleteAllEntities` `:765`, `:826-828`, also reached from the fast path
    `:1217-1230`;
  - `DeleteEntitiesConditional` single-transaction loop `:1230-1322`,
    `:1311-1313`;
  - `deleteBatched` `:1427`, each batch in `deleteOneBatch` `:1644`, commit
    conflict `:1691-1693`, fold into `IDToError` `:1699-1721`.
  - Today each 409 is built without a cause
    (`common.Operational(...).AsRetryable()`), so `errors.Is(err,
    spi.ErrConflict)` is false on it. The retry needs the cause, so these
    sites switch to one helper that attaches it (`WithCause`,
    `internal/common/errors.go:107`).
- A per-id `entityStore.Delete` error in the single-transaction loop
  (`:1301-1303`) and in `deleteOneBatch` (`:1684-1687`) goes into `IDToError`
  and the loop continues. On PostgreSQL a 40001 aborts the transaction, so
  every later statement of that attempt fails too. W-5 and W-6 treat a
  conflict there as the attempt's failure, which the retry then handles.
- `common.Internal` turns an `spi.ErrConflict` into a retryable 409 CONFLICT
  (`internal/common/errors.go:190-192`), but without a cause.
- The update doors map **any** engine `spi.ErrConflict` to 412
  `ENTITY_MODIFIED` before `classifyWorkflowError` sees it
  (`service.go:2181-2188` loopback, `:2200-2207` named transition). The
  reconcile marks its store error with `ErrScheduledTaskInfra` through
  `errors.Join` (`internal/domain/workflow/arm.go:177-179`), so the conflict
  is still visible. On PostgreSQL a task row changed after the snapshot
  fails the reconcile statement itself (40001, spec §10.2), so today an
  update racing a claim answers **412** there and **409** on memory and
  SQLite, where the conflict appears at commit. The collection door does
  worse: with an `IfMatch` it isolates the item as `ENTITY_MODIFIED` and
  keeps writing into the aborted transaction (`service.go:2551-2564`). W-3
  fixes both.
- `ErrScheduledTaskInfra` reaching `classifyWorkflowError` becomes
  `common.Internal(...)` (`service.go:2789-2791`), which answers 409 CONFLICT
  for a conflict and a ticketed 500 for anything else. W-3 relies on that
  and adds no new branch there.
- The workflow import saves the workflows outside any transaction
  (`internal/domain/workflow/handler.go:373`; the spec cites `:369`, four
  lines stale) and answers 200 at `:419`. `workflow.Handler` has no
  transaction manager of its own; its engine has one (`engine.go:166`), in
  the same package.
- `internal/domain/entity` imports `internal/domain/workflow`
  (`service.go:26`), so a helper in `entity` cannot serve the import. The
  retry therefore lives in `internal/common` (Open point 1).
- The arm rule "scheduled, not manual, not disabled" is written out twice
  today: `arm.go:119` and `fire_scheduled.go:555`. The import needs it a
  third time. E-2 adds the one predicate, `armsOnSchedule`, in `arm.go` and
  uses it at both sites and in `modelHasSchedule`; W-7 calls it (README
  C-P7).
- The gRPC doors call the same functions: single delete
  `internal/grpc/entity.go:200`; delete-all `:501`, which passes a nil
  condition — the fast path without `verbose`, the single-transaction loop
  with it, and `deleteBatched` with `transactionSize`. Errors become
  `CLIENT_ERROR`, a message `CODE: …`, and `retryable` only when true
  (`internal/grpc/errors.go:42-66`).
- E2E facts:
  - `doAuthRaw` retries a retryable 409 up to five times on the client side
    (`internal/e2e/helpers_test.go:149-183`). A test that must see the
    server's own answer (a 409, or a 200 that only the **server's** retry
    can give) uses a single-shot helper. W-2 adds `doAuthOnceRaw`.
  - `dbPool` is a direct pool on the suite's Postgres
    (`internal/e2e/e2e_test.go:36`). `testApp`'s scheduler is off
    (`e2e_test.go:177`). Every task in this stream is armed one hour ahead,
    so no other scheduler on that database finds one due. The only changes
    to these task rows are the ones the test makes.
  - `CONFLICT` is exempt from the error-code matrix
    (`internal/e2e/zzz_errorcode_matrix_test.go`,
    `universalCrossCuttingCodes`), so no matrix row is needed.
  - `deleteSingleEntity` and `importEntityModelWorkflow` declare no 409
    today (`api/openapi.yaml:1512-1571`, `:5195-5219`). `deleteEntities`
    declares 409 with a description that covers only
    `DELETE_NOT_CONVERGED` (`:2208-2234`).
- Parity:
  - scheduled scenarios register through `parity.Register` in
    `e2e/parity/scheduledtransition` (`scheduledtransition.go:44-55`); every
    backend wrapper imports that package. That keeps them outside
    `wantParityScenarioCount` (`registry_count_test.go:9`).
  - `client.DoJSONBodyRaw` (`e2e/parity/client/http.go:1828`) reaches any
    path.

## How a test makes a task-row conflict happen

Three methods, each used where it gives a reliable result.

1. **Unit and gRPC tests: a store double.** `taskconflict.Factory` wraps the
   real memory factory. Its `ScheduledTaskStore` refuses the first *n* calls
   of `DeleteForEntities`, `DeleteForModel` or `ReconcileForEntity` with an
   error that wraps `spi.ErrConflict`, and counts every call. The refusal
   comes back from the statement, which is where PostgreSQL raises it. That
   is the harder case for the handler. The result is exact: the number of
   retries is asserted by the call count.
2. **E2E, "racing one claim": a real row lock and a real update.**
   - The test opens a transaction on `dbPool` and runs `SELECT … FOR UPDATE`
     on the entity's task rows.
   - It sends the request, which blocks on that lock.
   - It waits until `pg_stat_activity` shows a backend whose
     `pg_blocking_pids` contains the test's own pid. That is exact, not a
     sleep.
   - It then changes the rows the way a claim does (`status='RUNNING'`, new
     claim token and owner) and commits.
   - The blocked statement runs in a REPEATABLE READ transaction whose
     snapshot is older than that commit. PostgreSQL therefore fails it with
     40001, and the plugin maps that to `spi.ErrConflict`
     (`plugins/postgres/classifying_querier.go:19-23`).
   - The server's retry starts a new transaction after the commit, and it
     succeeds.
   - The delete's first statement (the entity read) runs before the lock
     wait, so its snapshot predates the commit. For the import, the snapshot
     is taken at `DeleteForModel`'s first statement, which also starts before
     the commit.
3. **E2E, "the conflict persists": a trigger.**
   - A `BEFORE DELETE` row trigger on `scheduled_tasks`, scoped to the test's
     own entity ids or model name, raises SQLSTATE 40001.
   - The trigger counts its refusals with `nextval` on its own sequence.
     `nextval` is not undone by the rollback, so the sequence value is the
     number of attempts the server made.
   - Cleanup drops the trigger, the function and the sequence.
   - A lock cannot do this job. After the first commit the test would have to
     re-lock the row before the server's retry reaches it, and that race
     cannot be won reliably.

## Order and dependencies

```
W-1 ── W-2 ── W-3
        ├──── W-4 ── W-5 ── W-6
        └──── W-7
W-2…W-7 ── W-8 (also needs Q-5 and Q-6: after Q-6, README C-P6)
```

W-1 … W-6 may run beside E. W-7 runs after E completes (it calls E-2's
`armsOnSchedule`, README C-P7). W-8 runs after Q-6.

W-2 creates the shared test support (`internal/testing/taskconflict`, the
entity-package env, the gRPC env, the e2e race helpers). The later tasks
extend those files.

---

### Task W-1: `common.RetryOnTaskConflict`

**Spec:** §7 "Server-side retry on a task-row conflict (C1)". §13 U cells of
"an owned single-transaction delete … retried up to 3 times" and "a delete in
a joined transaction is not retried".

**Files:**
- Create: `internal/common/task_conflict.go`
- Test: `internal/common/task_conflict_test.go`

**Interfaces:**
- Produces:
  - `const common.TaskConflictRetries = 3`
  - `func common.RetryOnTaskConflict(ctx context.Context, owned bool, op func() error) error`
- The binding names `entity.taskConflictRetries` and
  `entity.retryOnTaskConflict` are replaced by these (Open point 1).

- [ ] **Step 1: Write the failing test**

```go
package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// countingOp returns an op that answers errs in order, then nil, and a
// pointer to how many times it ran.
func countingOp(errs ...error) (func() error, *int) {
	calls := 0
	return func() error {
		calls++
		if calls <= len(errs) {
			return errs[calls-1]
		}
		return nil
	}, &calls
}

func conflict() error { return fmt.Errorf("task row changed: %w", spi.ErrConflict) }

func TestRetryOnTaskConflict_SuccessRunsOnce(t *testing.T) {
	op, calls := countingOp()
	if err := RetryOnTaskConflict(context.Background(), true, op); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}

func TestRetryOnTaskConflict_ConflictThenSuccess(t *testing.T) {
	op, calls := countingOp(conflict())
	if err := RetryOnTaskConflict(context.Background(), true, op); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2", *calls)
	}
}

func TestRetryOnTaskConflict_PersistentConflict_RunsOncePlusRetries(t *testing.T) {
	errs := make([]error, 10)
	for i := range errs {
		errs[i] = conflict()
	}
	op, calls := countingOp(errs...)
	err := RetryOnTaskConflict(context.Background(), true, op)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want the last conflict", err)
	}
	if want := 1 + TaskConflictRetries; *calls != want {
		t.Errorf("calls = %d, want %d", *calls, want)
	}
}

func TestRetryOnTaskConflict_ConflictCarriedByAppError(t *testing.T) {
	appErr := Operational(http.StatusConflict, ErrCodeConflict, "transaction conflict — retry").AsRetryable().WithCause(conflict())
	op, calls := countingOp(appErr)
	if err := RetryOnTaskConflict(context.Background(), true, op); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2 (an AppError whose cause is a conflict is retried)", *calls)
	}
}

func TestRetryOnTaskConflict_OtherErrorNotRetried(t *testing.T) {
	boom := errors.New("storage down")
	op, calls := countingOp(boom, boom)
	if err := RetryOnTaskConflict(context.Background(), true, op); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}

func TestRetryOnTaskConflict_JoinedRunsOnce(t *testing.T) {
	op, calls := countingOp(conflict(), conflict())
	err := RetryOnTaskConflict(context.Background(), false, op)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want the conflict", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1: a joined op's conflict belongs to its owner", *calls)
	}
}

func TestRetryOnTaskConflict_StopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op, calls := countingOp(conflict(), conflict())
	if err := RetryOnTaskConflict(ctx, true, op); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want the conflict", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/common/ -run TestRetryOnTaskConflict`
Expected: build failure, `undefined: RetryOnTaskConflict` and `undefined: TaskConflictRetries`.

- [ ] **Step 3: Implement**

```go
package common

import (
	"context"
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TaskConflictRetries is how many more times an owned operation that removes
// scheduled tasks runs after a first-committer-wins refusal (spi.ErrConflict).
// The scheduler writes task rows when it claims, stamps, records, fails and
// gives back runs, so an entity delete or a workflow import can lose a race
// with it. Each run starts a new transaction, so running it again is safe.
const TaskConflictRetries = 3

// RetryOnTaskConflict runs op. While op fails with spi.ErrConflict it runs op
// again, at most TaskConflictRetries more times, and returns op's last error.
//
// owned=false means op joined a transaction that another request owns. Such
// an op runs once: its conflict belongs to that owner, whose commit reports
// it. A done ctx also stops the retries.
func RetryOnTaskConflict(ctx context.Context, owned bool, op func() error) error {
	err := op()
	if !owned {
		return err
	}
	for retry := 0; retry < TaskConflictRetries; retry++ {
		if err == nil || !errors.Is(err, spi.ErrConflict) || ctx.Err() != nil {
			return err
		}
		err = op()
	}
	return err
}
```

- [ ] **Step 4: Run it and see it pass**

Run: `go test ./internal/common/ -run TestRetryOnTaskConflict`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/common/task_conflict.go internal/common/task_conflict_test.go
git commit -m "feat(common): retry an owned write on a task-row conflict

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-2: single delete removes the entity's tasks; server retry; 409 declared

**Spec:** §7 entity-delete table row `Handler.DeleteEntity`; retry table rows
"single delete … owned" and "joined → none"; §8.1 row `deleteSingleEntity`;
§7 "`help/errors/CONFLICT.md` is widened". §13 rows "deleting one entity
removes its tasks" (U, E), "a delete … racing one claim succeeds after the
server retry" (E, single delete), "an owned single-transaction delete …
persistent conflict → 409" (U, E), "a delete in a joined transaction is not
retried" (U, E), "gRPC entity doors" (single delete).

**Files:**
- Create: `internal/testing/taskconflict/taskconflict.go`
- Modify: `internal/domain/entity/service.go` (`DeleteEntity` `:641-704`; two new helpers next to it)
- Modify: `api/openapi.yaml` (`deleteSingleEntity` responses, after `"404"` at `:1543-1560`)
- Modify: `cmd/cyoda/help/content/errors/CONFLICT.md`
- Test: `internal/domain/entity/scheduled_tasks_env_test.go` (new, shared env)
- Test: `internal/domain/entity/service_delete_tasks_test.go` (new)
- Test: `internal/grpc/entity_delete_tasks_test.go` (new)
- Test: `api/openapi_conflict_cells_test.go` (new)
- Test: `internal/e2e/scheduled_task_race_helpers_test.go` (new)
- Test: `internal/e2e/scheduled_task_writes_test.go` (new)

**Interfaces:**
- Consumes: `spi.ScheduledTaskStore.DeleteForEntities`, `Query`,
  `spi.ScheduledTaskQuery{EntityID, ModelName, ModelVersion, Limit}`;
  `common.RetryOnTaskConflict`, `common.TaskConflictRetries` (W-1).
- Produces:
  - `internal/testing/taskconflict`: `type Method string`; `DeleteForEntities`,
    `DeleteForModel`, `ReconcileForEntity`; `NewPlan() *Plan`;
    `(*Plan).Refuse(Method, int) *Plan`; `(*Plan).Fail(Method, error) *Plan`;
    `(*Plan).Reset()`; `(*Plan).Calls(Method) int`;
    `type Factory struct{ spi.StoreFactory; Plan *Plan }`.
  - `entity`: `func conflictError(err error) *common.AppError`;
    `func deleteWriteError(msg string, err error) *common.AppError`;
    `func (h *Handler) deleteEntityTasks(txCtx context.Context, ids []string) error`;
    `func txTenant(txCtx context.Context) (spi.TenantID, error)`.
  - e2e: `doAuthOnceRaw`, `setupScheduledModel`, `taskRows`,
    `holdTaskRows` / `(*taskRowHold).awaitBlocked` / `.claimAndCommit`,
    `installConflictTrigger` / `(*conflictTrigger).refusals` / `.remove`.

- [ ] **Step 1: Write the test support package**

It is test support, imported only by `_test.go` files. It is written first
because every test below needs it.

```go
// Package taskconflict gives tests a scheduled-task store that refuses
// chosen calls the way first-committer-wins does (spi.ErrConflict), and
// otherwise delegates to the real store. A test uses it to put a task-row
// conflict exactly where it wants one. It counts the calls, so a test can
// assert how many retries ran.
//
// Only _test.go files import it.
package taskconflict

import (
	"context"
	"fmt"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Method names a ScheduledTaskStore method a Plan can refuse.
type Method string

const (
	DeleteForEntities  Method = "DeleteForEntities"
	DeleteForModel     Method = "DeleteForModel"
	ReconcileForEntity Method = "ReconcileForEntity"
)

// Plan says which calls fail, and counts every call. Safe for concurrent use.
type Plan struct {
	mu     sync.Mutex
	refuse map[Method]int
	fail   map[Method]error
	calls  map[Method]int
}

// NewPlan returns a plan that refuses nothing.
func NewPlan() *Plan {
	return &Plan{refuse: map[Method]int{}, fail: map[Method]error{}, calls: map[Method]int{}}
}

// Refuse makes the next n calls of m fail with a conflict.
func (p *Plan) Refuse(m Method, n int) *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse[m] = n
	return p
}

// Fail makes every later call of m fail with err.
func (p *Plan) Fail(m Method, err error) *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail[m] = err
	return p
}

// Reset removes every refusal and failure. The counts stay.
func (p *Plan) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = map[Method]int{}
	p.fail = map[Method]error{}
}

// Calls reports how many times m was called.
func (p *Plan) Calls(m Method) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[m]
}

func (p *Plan) next(m Method) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[m]++
	if err := p.fail[m]; err != nil {
		return err
	}
	if p.refuse[m] > 0 {
		p.refuse[m]--
		return fmt.Errorf("taskconflict: %s: task row written after this transaction began: %w", m, spi.ErrConflict)
	}
	return nil
}

// Factory is a StoreFactory whose ScheduledTaskStore applies Plan.
type Factory struct {
	spi.StoreFactory
	Plan *Plan
}

// ScheduledTaskStore wraps the real store of the embedded factory.
func (f *Factory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	inner, err := f.StoreFactory.ScheduledTaskStore(ctx)
	if err != nil {
		return nil, err
	}
	return &store{ScheduledTaskStore: inner, plan: f.Plan}, nil
}

type store struct {
	spi.ScheduledTaskStore
	plan *Plan
}

func (s *store) DeleteForEntities(ctx context.Context, tenant spi.TenantID, entityIDs []string) error {
	if err := s.plan.next(DeleteForEntities); err != nil {
		return err
	}
	return s.ScheduledTaskStore.DeleteForEntities(ctx, tenant, entityIDs)
}

func (s *store) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int,
	keep func(sourceState, transition string) bool) error {
	if err := s.plan.next(DeleteForModel); err != nil {
		return err
	}
	return s.ScheduledTaskStore.DeleteForModel(ctx, tenant, modelName, modelVersion, keep)
}

func (s *store) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	if err := s.plan.next(ReconcileForEntity); err != nil {
		return nil, err
	}
	return s.ScheduledTaskStore.ReconcileForEntity(ctx, req)
}
```

- [ ] **Step 2: Write the failing entity tests**

`internal/domain/entity/scheduled_tasks_env_test.go` (shared by W-2 … W-6):

```go
package entity

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// taskTenant is the tenant deleteBatchedUserCtx binds.
const taskTenant spi.TenantID = "delete-batched-tenant"

// taskEnv is the Person model on memory. Its workflow arms AutoClose one
// hour after an entity enters OPEN, so every seeded entity has exactly one
// task and no scheduler finds it due. The handler reaches the task store
// through a taskconflict.Factory.
type taskEnv struct {
	h     *Handler
	ctx   context.Context
	real  *memory.StoreFactory
	txMgr spi.TransactionManager
	plan  *taskconflict.Plan
}

func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	real := memory.NewStoreFactory()
	t.Cleanup(func() { real.Close() })
	txMgr := mustTxMgr(t, real)
	ctx := newDeleteBatchedCtx(t, real)
	saveScheduledPersonWorkflow(t, real, ctx)
	plan := taskconflict.NewPlan()
	h := buildDeleteBatchedHandler(t, &taskconflict.Factory{StoreFactory: real, Plan: plan}, txMgr)
	return &taskEnv{h: h, ctx: ctx, real: real, txMgr: txMgr, plan: plan}
}

// withEntityStore rebuilds the handler so that EntityStore is store and the
// task store still goes through the plan.
func (e *taskEnv) withEntityStore(t *testing.T, store spi.EntityStore) {
	t.Helper()
	f := &entityStoreOverride{StoreFactory: &taskconflict.Factory{StoreFactory: e.real, Plan: e.plan}, store: store}
	e.h = buildDeleteBatchedHandler(t, f, e.txMgr)
}

type entityStoreOverride struct {
	spi.StoreFactory
	store spi.EntityStore
}

func (f *entityStoreOverride) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

func saveScheduledPersonWorkflow(t *testing.T, factory spi.StoreFactory, ctx context.Context) {
	t.Helper()
	wfStore, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := wfStore.Save(ctx, spi.ModelRef{EntityName: "Person", ModelVersion: "1"}, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "person-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 3_600_000}},
				{Name: "Leave", Next: "LEFT", Manual: true},
			}},
			"CLOSED": {},
			"LEFT":   {},
		},
	}}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}
}

// tasksOf counts entityID's tasks, read from the real store.
func (e *taskEnv) tasksOf(t *testing.T, entityID string) int {
	t.Helper()
	sts, err := e.real.ScheduledTaskStore(e.ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(e.ctx, taskTenant, spi.ScheduledTaskQuery{EntityID: entityID, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(page.Items)
}

// modelTasks counts the tasks of a model, read from the real store.
func (e *taskEnv) modelTasks(t *testing.T, modelName string) int {
	t.Helper()
	sts, err := e.real.ScheduledTaskStore(e.ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(e.ctx, taskTenant, spi.ScheduledTaskQuery{ModelName: modelName, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(page.Items)
}

func (e *taskEnv) exists(t *testing.T, id string) bool {
	t.Helper()
	store, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = store.Get(e.ctx, id)
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("Get %s: %v", id, err)
	}
	return err == nil
}

func requireConflict409(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T), want *common.AppError", err, err)
	}
	if appErr.Status != 409 || appErr.Code != common.ErrCodeConflict || !appErr.Retryable {
		t.Fatalf("got %d %s retryable=%v, want 409 %s retryable=true", appErr.Status, appErr.Code, appErr.Retryable, common.ErrCodeConflict)
	}
}
```

`internal/domain/entity/service_delete_tasks_test.go`:

```go
package entity

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
)

func TestDeleteEntity_RemovesTheEntitysTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	if n := e.tasksOf(t, ids[0]); n != 1 {
		t.Fatalf("tasks of %s before the delete = %d, want 1", ids[0], n)
	}

	if _, err := e.h.DeleteEntity(e.ctx, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	if n := e.tasksOf(t, ids[0]); n != 0 {
		t.Errorf("tasks of the deleted entity = %d, want 0", n)
	}
	if n := e.tasksOf(t, ids[1]); n != 1 {
		t.Errorf("tasks of the other entity = %d, want 1", n)
	}
}

func TestDeleteEntity_TaskConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForEntities, 2)

	if _, err := e.h.DeleteEntity(e.ctx, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 3 {
		t.Errorf("DeleteForEntities calls = %d, want 3 (two refusals, then success)", got)
	}
	if e.exists(t, ids[0]) {
		t.Error("entity still exists after a successful delete")
	}
	if n := e.tasksOf(t, ids[0]); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestDeleteEntity_TaskConflictPersists_Retryable409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)

	_, err := e.h.DeleteEntity(e.ctx, ids[0])
	requireConflict409(t, err)
	if got, want := e.plan.Calls(taskconflict.DeleteForEntities), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForEntities calls = %d, want %d", got, want)
	}
	if !e.exists(t, ids[0]) {
		t.Error("entity removed although every attempt rolled back")
	}
	if n := e.tasksOf(t, ids[0]); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

func TestDeleteEntity_Joined_NotRetried(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)

	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })

	_, err = e.h.DeleteEntity(joinedCtx, ids[0])
	requireConflict409(t, err)
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1: a joined delete is not retried", got)
	}
}

func TestDeleteEntity_TaskStoreFailure_Is500AndNotRetried(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Fail(taskconflict.DeleteForEntities, errors.New("task store unreachable"))

	_, err := e.h.DeleteEntity(e.ctx, ids[0])
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != 500 {
		t.Fatalf("err = %v, want a 500 AppError", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1", got)
	}
	if !e.exists(t, ids[0]) {
		t.Error("entity removed although the task removal failed")
	}
}
```

- [ ] **Step 3: Write the failing gRPC tests**

`internal/grpc/entity_delete_tasks_test.go`:

```go
package grpc

import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const taskDeleteTenant spi.TenantID = "task-delete-tenant"

// newTaskDeleteEnv is newTestEnv with a taskconflict.Factory in front of the
// task store and a person workflow that arms AutoClose one hour after an
// entity enters OPEN.
func newTaskDeleteEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context, *memory.StoreFactory, *taskconflict.Plan) {
	t.Helper()
	real := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
	t.Cleanup(func() { real.Close() })
	real.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := real.GetTransactionManager()
	plan := taskconflict.NewPlan()
	factory := &taskconflict.Factory{StoreFactory: real, Plan: plan}

	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "task-delete-user", UserName: "Task Delete",
		Tenant: spi.Tenant{ID: taskDeleteTenant, Name: "Task Delete"},
		Roles:  []string{"ADMIN"},
	})

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New()),
		modelHandler:  model.New(factory),
		searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore),
	}
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	wfStore, err := real.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := wfStore.Save(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "person-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 3_600_000}}}},
			"CLOSED": {},
		},
	}}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}
	return svc, ctx, real, plan
}

func createPersonRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context) string {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "create", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info := parseResponsePayload(t, resp)["transactionInfo"].(map[string]any)
	return info["entityIds"].([]any)[0].(string)
}

func personTasks(t *testing.T, real *memory.StoreFactory, ctx context.Context, entityID string) int {
	t.Helper()
	sts, err := real.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(ctx, taskDeleteTenant, spi.ScheduledTaskQuery{EntityID: entityID, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(page.Items)
}

func requireConflictEnvelope(t *testing.T, success bool, code, message string, retryable *bool) {
	t.Helper()
	if success {
		t.Fatal("success = true, want false")
	}
	if code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", code)
	}
	if !strings.HasPrefix(message, common.ErrCodeConflict+":") {
		t.Errorf("message = %q, want prefix %q", message, common.ErrCodeConflict+":")
	}
	if retryable == nil || !*retryable {
		t.Errorf("retryable = %v, want true", retryable)
	}
}

func TestRPC_EntityDelete_RemovesTheEntitysTasks(t *testing.T) {
	svc, ctx, real, _ := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	if n := personTasks(t, real, ctx, id); n != 1 {
		t.Fatalf("tasks before the delete = %d, want 1", n)
	}

	resp, err := svc.EntityManage(ctx, makeCE(EntityDeleteRequest, map[string]any{"id": "del", "entityId": id}))
	if err != nil {
		t.Fatalf("EntityManage: %v", err)
	}
	var typed events.EntityDeleteResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("delete failed: %+v", typed.Error)
	}
	if n := personTasks(t, real, ctx, id); n != 0 {
		t.Errorf("tasks after the delete = %d, want 0", n)
	}
}

func TestRPC_EntityDelete_TaskConflictPersists_ConflictEnvelope(t *testing.T) {
	svc, ctx, real, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	plan.Refuse(taskconflict.DeleteForEntities, 100)

	resp, err := svc.EntityManage(ctx, makeCE(EntityDeleteRequest, map[string]any{"id": "del", "entityId": id}))
	if err != nil {
		t.Fatalf("EntityManage: %v", err)
	}
	var typed events.EntityDeleteResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Fatalf("error = nil, want the conflict envelope; success=%v", typed.Success)
	}
	requireConflictEnvelope(t, typed.Success, typed.Error.Code, typed.Error.Message, typed.Error.Retryable)
	if n := personTasks(t, real, ctx, id); n != 1 {
		t.Errorf("tasks = %d, want 1 (every attempt rolled back)", n)
	}
}
```

- [ ] **Step 4: Write the failing OpenAPI test**

`api/openapi_conflict_cells_test.go`:

```go
package api

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func operationByID(t *testing.T, doc *openapi3.T, id string) *openapi3.Operation {
	t.Helper()
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.OperationID == id {
				return op
			}
		}
	}
	t.Fatalf("operation %s not found", id)
	return nil
}

// TestConflictCells asserts the 409 cells of spec §8.1: each operation that
// can lose a task-row race declares a 409 problem response, and says it is
// retryable and what it races.
func TestConflictCells(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, id := range []string{"deleteSingleEntity"} {
		t.Run(id, func(t *testing.T) {
			ref := operationByID(t, doc, id).Responses.Status(409)
			if ref == nil || ref.Value == nil {
				t.Fatal("no 409 response declared")
			}
			if ref.Value.Content.Get("application/problem+json") == nil {
				t.Error("409 has no application/problem+json content")
			}
			desc := ""
			if ref.Value.Description != nil {
				desc = *ref.Value.Description
			}
			for _, want := range []string{"CONFLICT", "etryable", "scheduler"} {
				if !strings.Contains(desc, want) {
					t.Errorf("409 description %q does not mention %q", desc, want)
				}
			}
		})
	}
}
```

- [ ] **Step 5: Write the failing E2E tests**

`internal/e2e/scheduled_task_race_helpers_test.go`:

```go
package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// schedWritesWorkflow arms AutoClose one hour after an entity enters OPEN.
// No test runs for an hour, so no scheduler on the suite's database finds
// one of these tasks due. Every change to their rows is one the test makes.
const schedWritesWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "sched-writes-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}},
				{"name": "Leave", "next": "LEFT", "manual": true}
			]},
			"CLOSED": {},
			"LEFT": {}
		}
	}]
}`

const schedWritesPayload = `{"name":"Order","amount":100,"status":"draft"}`

func setupScheduledModel(t *testing.T, model string) {
	t.Helper()
	setupModelWithWorkflow(t, model, schedWritesWorkflow)
}

// doAuthOnceRaw sends one authenticated request and never retries. The
// tests here must see the server's own answer: doAuthRaw retries a
// retryable 409 on the client side, which would hide both a 409 and a
// missing server-side retry.
func doAuthOnceRaw(ctx context.Context, method, path, body string) (*http.Response, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := authRequestRaw(ctx, method, path, r)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// taskRows counts scheduled-task rows matching where, read from Postgres.
// where is a constant of the calling test; values go in args.
func taskRows(t *testing.T, where string, args ...any) int {
	t.Helper()
	return queryDB(t, "test-tenant", "SELECT count(*) FROM scheduled_tasks WHERE "+where, args...)
}

// taskRowHold is a transaction on the verification pool that holds the row
// locks of some scheduled-task rows.
type taskRowHold struct {
	tx    pgx.Tx
	pid   int32
	where string
	args  []any
}

// holdTaskRows locks the rows matching where (FOR UPDATE) and keeps them
// locked until claimAndCommit, or until the test ends.
func holdTaskRows(t *testing.T, where string, args ...any) *taskRowHold {
	t.Helper()
	ctx := context.Background()
	tx, err := dbPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM (SELECT id FROM scheduled_tasks WHERE "+where+" FOR UPDATE) held", args...).Scan(&n); err != nil {
		t.Fatalf("lock task rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("no task rows match %q: the test would prove nothing", where)
	}
	return &taskRowHold{tx: tx, pid: pid, where: where, args: args}
}

// awaitBlocked returns once another backend waits on this hold's locks.
func (h *taskRowHold) awaitBlocked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := dbPool.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))", h.pid).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no statement ever waited on the held task rows")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claimAndCommit changes the held rows as a claim does and commits. The
// statement waiting on the lock began its snapshot before this commit, so
// PostgreSQL fails it with 40001.
func (h *taskRowHold) claimAndCommit(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.tx.Exec(ctx,
		"UPDATE scheduled_tasks SET status = 'RUNNING', claim_token = gen_random_uuid(), claim_owner = gen_random_uuid() WHERE "+h.where,
		h.args...); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := h.tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
}

var inlineSafe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// conflictTrigger fails every delete of a matching scheduled-task row with
// SQLSTATE 40001, the refusal first-committer-wins gives. It counts the
// refusals in a sequence, which a rollback does not undo.
type conflictTrigger struct{ name string }

// installConflictTrigger scopes the trigger to rows whose column equals one
// of values. column is entity_id or model_name.
func installConflictTrigger(t *testing.T, column string, values ...string) *conflictTrigger {
	t.Helper()
	if column != "entity_id" && column != "model_name" {
		t.Fatalf("column %q is not entity_id or model_name", column)
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		if !inlineSafe.MatchString(v) {
			t.Fatalf("value %q is not safe to inline in DDL", v)
		}
		quoted = append(quoted, "'"+v+"'")
	}
	c := &conflictTrigger{name: fmt.Sprintf("w_conflict_%d", time.Now().UnixNano())}
	ddl := fmt.Sprintf(`
CREATE SEQUENCE %[1]s;
CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF OLD.%[2]s IN (%[3]s) THEN
    PERFORM nextval('%[1]s');
    RAISE EXCEPTION 'test: task row changed after this transaction began' USING ERRCODE = '40001';
  END IF;
  RETURN OLD;
END
$fn$;
CREATE TRIGGER %[1]s BEFORE DELETE ON scheduled_tasks FOR EACH ROW EXECUTE FUNCTION %[1]s();`,
		c.name, column, strings.Join(quoted, ", "))
	if _, err := dbPool.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("install conflict trigger: %v", err)
	}
	t.Cleanup(func() { c.remove(t) })
	return c
}

// refusals reports how many deletes the trigger refused.
func (c *conflictTrigger) refusals(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := dbPool.QueryRow(context.Background(),
		fmt.Sprintf("SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM %s", c.name)).Scan(&n); err != nil {
		t.Fatalf("read refusals: %v", err)
	}
	return n
}

// remove drops the trigger. Safe to call twice.
func (c *conflictTrigger) remove(t *testing.T) {
	t.Helper()
	if _, err := dbPool.Exec(context.Background(), fmt.Sprintf(
		"DROP TRIGGER IF EXISTS %[1]s ON scheduled_tasks; DROP FUNCTION IF EXISTS %[1]s(); DROP SEQUENCE IF EXISTS %[1]s;", c.name)); err != nil {
		t.Errorf("remove conflict trigger: %v", err)
	}
}
```

`internal/e2e/scheduled_task_writes_test.go`:

```go
package e2e_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func requireConflictProblem(t *testing.T, res httpResult) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("request: %v", res.err)
	}
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", res.status, res.body)
	}
	pd := decodeProblem(t, res.body)
	if pd.errorCode() != common.ErrCodeConflict || !pd.retryable() {
		t.Fatalf("errorCode=%q retryable=%v, want CONFLICT retryable; body: %s", pd.errorCode(), pd.retryable(), res.body)
	}
}

func TestScheduledTaskWrites_DeleteEntity_RemovesItsTasks(t *testing.T) {
	const model = "e2e-stw-delete-one"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	other := createEntityE2E(t, model, 1, schedWritesPayload)
	if n := taskRows(t, "entity_id = $1", id); n != 1 {
		t.Fatalf("task rows before the delete = %d, want 1", n)
	}

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, "/api/entity/"+id, ""))
	if res.status != http.StatusOK {
		t.Fatalf("delete: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows of the deleted entity = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1", other); n != 1 {
		t.Errorf("task rows of the other entity = %d, want 1", n)
	}
}

func TestScheduledTaskWrites_DeleteEntity_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-delete-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() { done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, "/api/entity/"+id, "")) }()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	res := <-done
	if res.status != http.StatusOK {
		t.Fatalf("delete racing a claim: %d %s, want 200 after the server's retry", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
	if st := resultOf(doAuthOnceRaw(ctx, http.MethodGet, "/api/entity/"+id, "")).status; st != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", st)
	}
}

func TestScheduledTaskWrites_DeleteEntity_PersistentConflict_409(t *testing.T) {
	const model = "e2e-stw-delete-persist"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "entity_id", id)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, "/api/entity/"+id, "")))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 1 {
		t.Errorf("task rows = %d, want 1", n)
	}
	if st := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodGet, "/api/entity/"+id, "")).status; st != http.StatusOK {
		t.Errorf("GET after the refused delete = %d, want 200", st)
	}
}

// A delete that joined another request's transaction is not retried on the
// server. Its conflict reaches the joined caller, and the owner's
// transaction does not commit.
func TestScheduledTaskWrites_DeleteEntity_Joined_NotRetried(t *testing.T) {
	h := newCallbackHarness(t)
	const target = "e2e-stw-joined-target"
	const primary = "e2e-stw-joined-primary"
	h.SetupModelWithWorkflow(t, target, schedWritesWorkflow)
	targetID, status, body := h.CreateEntity(t, target, 1, schedWritesPayload)
	if status != http.StatusOK {
		t.Fatalf("create target: %d %s", status, body)
	}

	var (
		mu       sync.Mutex
		callback callbackResult
	)
	h.RegisterProc("stw-delete-target", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.callback(http.MethodDelete, "/api/entity/"+targetID, "", rc.token)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		callback = res
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("joined delete: %d", res.StatusCode)
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "stw-joined-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
					"processors": [{"type": "calculator", "name": "stw-delete-target", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"DONE": {}
			}
		}]
	}`)

	hold := holdTaskRows(t, "entity_id = $1", targetID)
	h.token(t)
	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(primary, 1, schedWritesPayload) }()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	var outer createEntityResult
	select {
	case outer = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("primary create did not finish")
	}
	mu.Lock()
	got := callback
	mu.Unlock()
	if got.StatusCode != http.StatusConflict {
		t.Fatalf("joined delete answered %d %s, want 409", got.StatusCode, got.Body)
	}
	if pd := decodeProblem(t, got.Body); pd.errorCode() != common.ErrCodeConflict || !pd.retryable() {
		t.Errorf("joined delete errorCode=%q retryable=%v, want CONFLICT retryable", pd.errorCode(), pd.retryable())
	}
	if outer.status == http.StatusOK {
		t.Errorf("primary create = 200, want a failure: its transaction met the conflict")
	}
	if _, code := h.GetEntityState(t, targetID); code != http.StatusOK {
		t.Errorf("target GET = %d, want 200: nothing the joined delete did may commit", code)
	}
	if n := taskRows(t, "entity_id = $1 AND status = 'RUNNING'", targetID); n != 1 {
		t.Errorf("target task rows = %d, want 1 (the test's claim, untouched)", n)
	}
}
```

- [ ] **Step 6: Run the tests and see them fail**

Run:
```
go build ./internal/testing/taskconflict/
go test ./internal/domain/entity/ -run 'TestDeleteEntity_'
go test ./internal/grpc/ -run 'TestRPC_EntityDelete_'
go test ./api/ -run TestConflictCells
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_DeleteEntity_'
```
Expected:
- entity:
  - `TestDeleteEntity_RemovesTheEntitysTasks`: `tasks of the deleted entity = 1, want 0`;
  - `…RetriedThenSucceeds`: `DeleteForEntities calls = 0, want 3`;
  - `…Persists_Retryable409`: `err = <nil>`;
  - `…Joined_NotRetried`: `err = <nil>`;
  - `…TaskStoreFailure…`: `err = <nil>, want a 500 AppError`.
- gRPC: `tasks after the delete = 1, want 0`, and `error = nil, want the conflict envelope`.
- api: `no 409 response declared`.
- e2e:
  - `task rows of the deleted entity = 1, want 0`;
  - the race test times out in `awaitBlocked`, because no statement touches
    the task rows;
  - the persistent test gets 200;
  - the joined test fails in `awaitBlocked`.

- [ ] **Step 7: Implement**

In `internal/domain/entity/service.go`, replace `DeleteEntity`
(`:641-704`) with:

```go
// DeleteEntity deletes a single entity by ID, with its scheduled tasks, in
// one transaction, and returns the deleted entity's metadata for the
// response. An owned delete that loses a task-row race with the scheduler
// runs again (common.RetryOnTaskConflict); a joined one does not.
func (h *Handler) DeleteEntity(ctx context.Context, entityID string) (*deleteEntityResult, error) {
	var result *deleteEntityResult
	err := common.RetryOnTaskConflict(ctx, spi.GetTransaction(ctx) == nil, func() error {
		r, err := h.deleteEntityOnce(ctx, entityID)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// deleteEntityOnce is one attempt of DeleteEntity, in its own transaction.
func (h *Handler) deleteEntityOnce(ctx context.Context, entityID string) (*deleteEntityResult, error) {
	// Begin a fresh tx, or PARTICIPATE in a joined tx already on ctx.
	scope, err := h.beginScope(ctx)
	if err != nil {
		return nil, classifyBeginErr(err)
	}
	defer scope.Release()

	txID, txCtx, owned := scope.TxID(), scope.Ctx(), scope.Owned()

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	// Load entity before deleting to get ModelRef for response (adds to read set).
	entity, err := entityStore.Get(txCtx, entityID)
	if err != nil {
		// Only a genuine miss is a 404. A store outage reported as "it does not
		// exist" is a substituted answer that stops the caller retrying — see
		// .claude/rules/correctness-over-availability.md — so anything else keeps
		// its cause and routes to the retryable 503 / ticketed 500 classifier.
		if !errors.Is(err, spi.ErrNotFound) {
			return nil, common.Internal("failed to read entity for delete", err)
		}
		appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("entity id=%s not found", entityID))
		appErr.Props = map[string]any{
			"entityId": entityID,
		}
		return nil, appErr
	}

	// Finalize: gate the OWNER's Delete+Commit against a concurrent joined
	// callback's buffer write; on the joined path the join layer holds the gate.
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		// Soft delete within transaction.
		if err := entityStore.Delete(txCtx, entityID); err != nil {
			return deleteWriteError("failed to delete entity", err)
		}
		// The entity's scheduled tasks go in the same transaction.
		if err := h.deleteEntityTasks(txCtx, []string{entityID}); err != nil {
			return deleteWriteError("failed to delete scheduled tasks", err)
		}
		// Commit transaction (no-op when participating in a joined tx).
		if err := scope.Commit(); err != nil {
			return deleteWriteError("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	ver, _ := strconv.Atoi(entity.Meta.ModelRef.ModelVersion)
	return &deleteEntityResult{
		EntityID:      entityID,
		ModelName:     entity.Meta.ModelRef.EntityName,
		ModelVersion:  ver,
		TransactionID: txID,
	}, nil
}

// conflictError is the retryable 409 for a first-committer-wins refusal. The
// refusal stays its cause, so common.RetryOnTaskConflict recognises it.
func conflictError(err error) *common.AppError {
	return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").
		AsRetryable().WithCause(err)
}

// deleteWriteError classifies a failed write or commit inside a delete's
// transaction: a first-committer-wins refusal is conflictError, anything
// else is common.Internal.
func deleteWriteError(msg string, err error) *common.AppError {
	if errors.Is(err, spi.ErrConflict) {
		return conflictError(err)
	}
	return common.Internal(msg, err)
}

// txTenant is the tenant of the transaction on txCtx.
func txTenant(txCtx context.Context) (spi.TenantID, error) {
	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		return "", errors.New("no transaction on the context")
	}
	return tx.TenantID, nil
}

// deleteEntityTasks removes the scheduled tasks of ids inside the
// transaction on txCtx. No ids, no call.
func (h *Handler) deleteEntityTasks(txCtx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tenant, err := txTenant(txCtx)
	if err != nil {
		return err
	}
	sts, err := h.factory.ScheduledTaskStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to access scheduled task store: %w", err)
	}
	return sts.DeleteForEntities(txCtx, tenant, ids)
}
```

In `api/openapi.yaml`, under `deleteSingleEntity` responses, after the
`"404"` block (ending `:1560`) and before `"401"`:

```yaml
        "409":
          description: >-
            Conflict (`CONFLICT`, retryable). Another write committed first to
            the entity or to one of its scheduled tasks — the scheduler changes
            a task when it claims, records, fails or gives it back. The server
            retries the delete up to 3 times before it answers 409; retry the
            request. A request that joins an open transaction (`X-Tx-Token`) is
            not retried on the server.
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
              example:
                type: about:blank
                title: Conflict
                status: 409
                detail: "CONFLICT: transaction conflict — retry"
                instance: /api/entity/2ed54d80-f3c2-11ee-9e63-ae468cd3ed16
                properties:
                  errorCode: CONFLICT
                  retryable: true
```

Replace `cmd/cyoda/help/content/errors/CONFLICT.md` with:

```markdown
---
topic: errors.CONFLICT
title: "CONFLICT — another write committed first"
stability: stable
see_also:
  - errors
  - errors.TX_CONFLICT
  - errors.IDEMPOTENCY_CONFLICT
  - errors.EPOCH_MISMATCH
---

# errors.CONFLICT

## NAME

CONFLICT — the write lost a race: another transaction committed a change to the same entity, or to one of its scheduled tasks, after this write began.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `yes`.

## DESCRIPTION

When a write commits, the server checks that nothing it wrote was changed by another transaction that committed after it began. Two kinds of change are checked:

- **The entity.** Another client or a workflow changed the entity first.
- **A scheduled task of the entity.** The scheduler changed one of the entity's scheduled tasks first. The scheduler changes a task when it claims it, records an attempt, marks it failed, or gives it back. A write that arms or cancels the entity's scheduled transitions writes those tasks, and so can race the scheduler.

Both are normal outcomes under concurrent load.

How each operation handles a race with the scheduler:

- **Entity delete and workflow import.** The server retries up to 3 times before it answers 409.
- **Batched delete (`transactionSize`).** The whole request does not answer 409 for a task race. A batch that still conflicts lists its ids in `idToError` with this code, and the other batches run.
- **Request that joined an open transaction (`X-Tx-Token`).** The server does not retry it. The transaction's owner gets the conflict.

Retry the whole read-modify-write cycle with the current entity state. Replaying the original write without re-reading produces stale data. A retried workflow import saves the same workflows again and then removes the tasks.

On the gRPC entity operations this error is `code` `CLIENT_ERROR`, with a message that starts with `CONFLICT:` and `retryable` true.

## SEE ALSO

- errors
- errors.TX_CONFLICT
- errors.IDEMPOTENCY_CONFLICT
- errors.EPOCH_MISMATCH
```

- [ ] **Step 8: Run the tests and see them pass**

Run:
```
go test ./internal/domain/entity/ -run 'TestDeleteEntity_'
go test ./internal/grpc/ -run 'TestRPC_EntityDelete_'
go test ./api/ -run TestConflictCells
go generate ./api && git diff --stat api/generated.go
go test ./cmd/cyoda/help/...
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_DeleteEntity_'
go test ./internal/domain/entity/ ./internal/grpc/
```
Expected:
- all `ok`;
- `git diff --stat api/generated.go` prints nothing: `std-http-server` models
  do not model responses (`api/config.yaml`).

- [ ] **Step 9: Commit**

```bash
git add internal/testing/taskconflict/taskconflict.go \
  internal/domain/entity/service.go \
  internal/domain/entity/scheduled_tasks_env_test.go internal/domain/entity/service_delete_tasks_test.go \
  internal/grpc/entity_delete_tasks_test.go \
  api/openapi.yaml api/openapi_conflict_cells_test.go \
  cmd/cyoda/help/content/errors/CONFLICT.md \
  internal/e2e/scheduled_task_race_helpers_test.go internal/e2e/scheduled_task_writes_test.go
git commit -m "feat(entity): single delete removes the entity's tasks; retry a task-row conflict

A delete removes the entity's scheduled tasks in its own transaction. An
owned delete that loses a task-row race with the scheduler runs again up to
3 times, then answers a retryable 409; a joined delete runs once.
deleteSingleEntity declares the 409 and CONFLICT.md covers the scheduler.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-3: an update racing the scheduler answers 409 CONFLICT, not 412

**Spec:** §7 "An update then fails with `409` (retryable)"; §8.1 row
`updateSingle`, `updateSingleWithLoopback`, `updateCollection`; §13 row "an
update racing a claim → retryable 409" (E isolated); §13 row "gRPC entity
doors: … 409 on a race". Review Focus 2.

**Files:**
- Modify: `internal/domain/entity/service.go` (`:2181`, `:2200`, `:2529-2553`)
- Test: `internal/domain/entity/service_update_task_conflict_test.go` (new)
- Test: `internal/grpc/entity_delete_tasks_test.go` (one test added)
- Test: `internal/e2e/scheduled_task_writes_test.go` (one test added)

**Interfaces:**
- Consumes: `wfengine.ErrScheduledTaskInfra` joined with the store's error
  by `reconcileScheduledTasks` (`arm.go:177-179`). Stream E must keep that
  join (Open point 4).
- Produces: nothing new.

- [ ] **Step 1: Write the failing tests**

`internal/domain/entity/service_update_task_conflict_test.go`:

```go
package entity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
)

// The reconcile's refusal comes from the statement, as on PostgreSQL. It
// must answer 409 CONFLICT, not 412 ENTITY_MODIFIED: nothing modified the
// entity.
func TestUpdateEntity_Loopback_TaskRowConflict_Is409Conflict(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.ReconcileForEntity, 1)

	_, err := e.h.UpdateEntity(e.ctx, UpdateEntityInput{
		EntityID: ids[0], Format: "JSON", Data: json.RawMessage(`{"name":"Q","age":7}`),
	})
	requireConflict409(t, err)
}

func TestUpdateEntity_NamedTransition_TaskRowConflict_Is409Conflict(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.ReconcileForEntity, 1)

	_, err := e.h.UpdateEntity(e.ctx, UpdateEntityInput{
		EntityID: ids[0], Format: "JSON", Data: json.RawMessage(`{"name":"Q","age":7}`), Transition: "Leave",
	})
	requireConflict409(t, err)
}

// With an IfMatch the collection door isolates an ENTITY_MODIFIED item and
// carries on. A task-row conflict must not be isolated: on PostgreSQL the
// transaction is already aborted, so the whole request fails with 409.
func TestUpdateEntityCollection_IfMatch_TaskRowConflict_FailsTheRequest(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	store, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	cur, err := store.Get(e.ctx, ids[0])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	e.plan.Refuse(taskconflict.ReconcileForEntity, 1)

	res, err := e.h.UpdateEntityCollection(e.ctx, []UpdateCollectionItem{{
		EntityID: ids[0], Payload: json.RawMessage(`{"name":"Q","age":7}`), IfMatch: cur.Meta.TransactionID,
	}})
	if err == nil {
		t.Fatalf("err = nil, result %+v; want the request to fail with 409", res)
	}
	requireConflict409(t, err)
}
```

Add to `internal/grpc/entity_delete_tasks_test.go`:

```go
func TestRPC_EntityUpdate_TaskRowConflict_ConflictEnvelope(t *testing.T) {
	svc, ctx, _, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	plan.Refuse(taskconflict.ReconcileForEntity, 1)

	resp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id": "upd", "dataFormat": "JSON",
		"payload": map[string]any{"entityId": id, "data": map[string]any{"name": "Alicia"}},
	}))
	if err != nil {
		t.Fatalf("EntityManage: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Fatalf("error = nil, want the conflict envelope; success=%v", typed.Success)
	}
	requireConflictEnvelope(t, typed.Success, typed.Error.Code, typed.Error.Message, typed.Error.Retryable)
}
```

Add to `internal/e2e/scheduled_task_writes_test.go`:

```go
// An update has no server retry. Racing a claim, it answers 409 CONFLICT,
// and the retried update re-arms the task as a new life.
func TestScheduledTaskWrites_UpdateRacingOneClaim_409ThenRetrySucceeds(t *testing.T) {
	const model = "e2e-stw-update-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	path := "/api/entity/JSON/" + id
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodPut, path, `{"name":"Order","amount":101,"status":"draft"}`))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)
	requireConflictProblem(t, <-done)

	retry := resultOf(doAuthOnceRaw(ctx, http.MethodPut, path, `{"name":"Order","amount":101,"status":"draft"}`))
	if retry.status != http.StatusOK {
		t.Fatalf("retried update: %d %s", retry.status, retry.body)
	}
	if n := taskRows(t, "entity_id = $1 AND status = 'WAITING' AND claim_token IS NULL", id); n != 1 {
		t.Errorf("WAITING unclaimed task rows = %d, want 1 (the retry re-armed the task)", n)
	}
}
```

The `internal/grpc` test file gains no new import. The e2e file gains none.

- [ ] **Step 2: Run the tests and see them fail**

Run:
```
go test ./internal/domain/entity/ -run 'TestUpdateEntity_.*TaskRowConflict|TestUpdateEntityCollection_IfMatch_TaskRowConflict'
go test ./internal/grpc/ -run TestRPC_EntityUpdate_TaskRowConflict
go test ./internal/e2e/ -run TestScheduledTaskWrites_UpdateRacingOneClaim
```
Expected:
- the two single-update tests fail with
  `got 412 ENTITY_MODIFIED retryable=false, want 409 CONFLICT retryable=true`;
- the collection test fails with `err = nil, result &{… Failed:[{… Code:ENTITY_MODIFIED …}]}`;
- gRPC: the message starts with `ENTITY_MODIFIED:` and `retryable = <nil>`;
- e2e: `status = 412, want 409`.

- [ ] **Step 3: Implement**

`internal/domain/entity/service.go`, loopback branch (`:2181`):

```go
			// A task-row conflict (the reconcile lost a race with the
			// scheduler) is not an entity modification. It keeps its cause
			// and classifyWorkflowError answers the retryable 409 CONFLICT.
			if errors.Is(lbErr, spi.ErrConflict) && !errors.Is(lbErr, wfengine.ErrScheduledTaskInfra) {
```

Named-transition branch (`:2200`):

```go
			if errors.Is(mtErr, spi.ErrConflict) && !errors.Is(mtErr, wfengine.ErrScheduledTaskInfra) {
```

Collection branch (`:2551-2553`):

```go
			if item.ifMatch != "" && errors.Is(engineErr, spi.ErrConflict) &&
				!errors.Is(engineErr, wfengine.ErrPostSegmentConflict) &&
				!errors.Is(engineErr, wfengine.ErrCommitBeforeDispatchInfra) &&
				!errors.Is(engineErr, wfengine.ErrScheduledTaskInfra) {
```

In the comment above that branch (`:2529`), change "Two other shapes" to
"Three other shapes", and add this item after the `ErrCommitBeforeDispatchInfra`
item (`:2546`):

```go
			//   - ErrScheduledTaskInfra: the reconcile's task-row write lost a
			//     race with the scheduler. On PostgreSQL that statement's
			//     40001 has aborted the transaction. It leaves through
			//     classifyWorkflowError → common.Internal → a retryable 409.
```

- [ ] **Step 4: Run the tests and see them pass**

Run the Step 2 commands, then `go test ./internal/domain/entity/ ./internal/grpc/`.
Expected: `ok`. The existing IfMatch tests
(`service_classify_test.go`, `service_rollback_test.go:356-374`) stay green:
they carry no `ErrScheduledTaskInfra`.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/service_update_task_conflict_test.go \
  internal/grpc/entity_delete_tasks_test.go internal/e2e/scheduled_task_writes_test.go
git commit -m "fix(entity): an update racing the scheduler answers 409 CONFLICT

A reconcile that loses a task-row race was reported as 412 ENTITY_MODIFIED
on PostgreSQL, and the collection door isolated it as a per-item failure in
an aborted transaction. It now answers the retryable 409 on every backend.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-4: delete-all and the fast path remove the model's tasks

**Spec:** §7 entity-delete table row `DeleteAllEntities` (`keep = none`);
retry table row "delete-all fast path"; §8.1 row `deleteEntities`
(conditional loop and delete-all fast path). §13 rows "delete-all removes the
model's tasks" (U, E), "conditional delete removes tasks: … fast path" (U, E),
"`DeleteForModel` in tenant A leaves tenant B's tasks" (E), "a delete …
racing one claim succeeds after the server retry" (fast path), "an owned
single-transaction delete … persistent conflict → 409" (fast path), and the
gRPC delete-all door.

**Files:**
- Modify: `internal/domain/entity/service.go` (`DeleteAllEntities` `:764-843`)
- Modify: `api/openapi.yaml` (`deleteEntities` `"409"` `:2208-2234`)
- Test: `internal/domain/entity/service_delete_tasks_test.go`
- Test: `internal/grpc/entity_delete_tasks_test.go`
- Test: `api/openapi_conflict_cells_test.go`
- Test: `internal/e2e/scheduled_task_writes_test.go`

**Interfaces:**
- Consumes: `spi.ScheduledTaskStore.DeleteForModel(ctx, tenant, name, version, keep)`,
  where **`keep == nil` removes every task of the model** (Open point 2).
- Produces: `func (h *Handler) deleteModelTasks(txCtx context.Context, ref spi.ModelRef) error`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/domain/entity/service_delete_tasks_test.go` (add
`spi "github.com/cyoda-platform/cyoda-go-spi"` to its imports):

```go
// armForeignModelTask arms one task of another model in the same tenant, so
// a test can see that a model-wide removal stays inside its model.
func armForeignModelTask(t *testing.T, e *taskEnv) {
	t.Helper()
	txID, txCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sts, err := e.real.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if _, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenant, EntityID: "foreign-entity", CurrentState: "OPEN",
		Arm: []spi.ScheduledTask{{
			ID: "foreign-task", TenantID: taskTenant, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: 9_999_999_999_999, EntityID: "foreign-entity",
			ModelName: "Pet", ModelVersion: 1, Transition: "AutoClose", SourceState: "OPEN",
		}},
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if err := e.txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestDeleteAllEntities_RemovesTheModelsTasks(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	armForeignModelTask(t, e)
	if n := e.modelTasks(t, "Person"); n != 3 {
		t.Fatalf("Person tasks before = %d, want 3", n)
	}

	if _, err := e.h.DeleteAllEntities(e.ctx, "Person", "1"); err != nil {
		t.Fatalf("DeleteAllEntities: %v", err)
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
	if n := e.modelTasks(t, "Pet"); n != 1 {
		t.Errorf("Pet tasks = %d, want 1: another model's tasks stay", n)
	}
}

func TestDeleteEntitiesConditional_FastPath_RemovesTheModelsTasks(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)

	if _, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", nil, nil, false, 0); err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1 (the fast path)", got)
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
}

func TestDeleteAllEntities_TaskConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForModel, 2)

	res, err := e.h.DeleteAllEntities(e.ctx, "Person", "1")
	if err != nil {
		t.Fatalf("DeleteAllEntities: %v", err)
	}
	if res.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2 (each attempt counts afresh)", res.TotalCount)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 3 {
		t.Errorf("DeleteForModel calls = %d, want 3", got)
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
}

func TestDeleteAllEntities_TaskConflictPersists_Retryable409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForModel, 100)

	_, err := e.h.DeleteAllEntities(e.ctx, "Person", "1")
	requireConflict409(t, err)
	if got, want := e.plan.Calls(taskconflict.DeleteForModel), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForModel calls = %d, want %d", got, want)
	}
	if !e.exists(t, ids[0]) || !e.exists(t, ids[1]) {
		t.Error("entities removed although every attempt rolled back")
	}
}

func TestDeleteAllEntities_Joined_NotRetried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForModel, 100)
	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })

	_, err = e.h.DeleteAllEntities(joinedCtx, "Person", "1")
	requireConflict409(t, err)
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1", got)
	}
}
```

Add to `internal/grpc/entity_delete_tasks_test.go`:

```go
func deleteAllRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, extra map[string]any) events.EntityDeleteAllResponseJson {
	t.Helper()
	fields := map[string]any{"id": "del-all", "model": map[string]any{"name": "person", "version": 1}}
	for k, v := range extra {
		fields[k] = v
	}
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(makeCE(EntityDeleteAllRequest, fields), stream); err != nil {
		t.Fatalf("EntityManageCollection: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("responses = %d, want 1", len(stream.sent))
	}
	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	return typed
}

func TestRPC_EntityDeleteAll_FastPath_RemovesTasks(t *testing.T) {
	svc, ctx, real, _ := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)

	if typed := deleteAllRPC(t, svc, ctx, nil); !typed.Success {
		t.Fatalf("delete-all failed: %+v", typed.Error)
	}
	if n := personTasks(t, real, ctx, id); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestRPC_EntityDeleteAll_FastPath_TaskConflictPersists_ConflictEnvelope(t *testing.T) {
	svc, ctx, real, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	plan.Refuse(taskconflict.DeleteForModel, 100)

	typed := deleteAllRPC(t, svc, ctx, nil)
	if typed.Error == nil {
		t.Fatalf("error = nil, want the conflict envelope; success=%v", typed.Success)
	}
	requireConflictEnvelope(t, typed.Success, typed.Error.Code, typed.Error.Message, typed.Error.Retryable)
	if n := personTasks(t, real, ctx, id); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}
```

Extend `api/openapi_conflict_cells_test.go`: the loop's list becomes
`[]string{"deleteSingleEntity", "deleteEntities"}`.

Add to `internal/e2e/scheduled_task_writes_test.go` (add `"io"` to its
imports):

```go
func TestScheduledTaskWrites_DeleteAll_RemovesModelTasks_OtherTenantKept(t *testing.T) {
	const model = "e2e-stw-delete-all"
	setupScheduledModel(t, model)
	createEntityE2E(t, model, 1, schedWritesPayload)
	createEntityE2E(t, model, 1, schedWritesPayload)

	// Tenant B: same model name, its own entity and task.
	bID, bSecret := createM2MClient(t, "tenant-b-stw", "user-b", []string{"ROLE_ADMIN", "ROLE_M2M"})
	asB := func(method, path, body string) {
		t.Helper()
		var raw []byte
		if body != "" {
			raw = []byte(body)
		}
		resp := adminRequestAs(t, bID, bSecret, method, path, raw)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("tenant B %s %s: %d %s", method, path, resp.StatusCode, b)
		}
	}
	asB(http.MethodPost, fmt.Sprintf("/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel)
	asB(http.MethodPut, fmt.Sprintf("/model/%s/1/lock", model), "")
	asB(http.MethodPost, fmt.Sprintf("/model/%s/1/workflow/import", model), schedWritesWorkflow)
	asB(http.MethodPost, fmt.Sprintf("/entity/JSON/%s/1", model), schedWritesPayload)

	if n := taskRows(t, "tenant_id = $1 AND model_name = $2", "test-tenant", model); n != 2 {
		t.Fatalf("tenant A task rows before = %d, want 2", n)
	}
	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), ""))
	if res.status != http.StatusOK {
		t.Fatalf("delete-all: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "tenant_id = $1 AND model_name = $2", "test-tenant", model); n != 0 {
		t.Errorf("tenant A task rows = %d, want 0", n)
	}
	if n := taskRows(t, "tenant_id = $1 AND model_name = $2", "tenant-b-stw", model); n != 1 {
		t.Errorf("tenant B task rows = %d, want 1: a delete never reaches another tenant", n)
	}
}

func TestScheduledTaskWrites_DeleteAll_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-delete-all-race"
	setupScheduledModel(t, model)
	createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "tenant_id = 'test-tenant' AND model_name = $1", model)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), ""))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	if res := <-done; res.status != http.StatusOK {
		t.Fatalf("delete-all racing a claim: %d %s, want 200", res.status, res.body)
	}
	if n := taskRows(t, "tenant_id = 'test-tenant' AND model_name = $1", model); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_DeleteAll_PersistentConflict_409(t *testing.T) {
	const model = "e2e-stw-delete-all-persist"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "model_name", model)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), "")))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
	if st := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodGet, "/api/entity/"+id, "")).status; st != http.StatusOK {
		t.Errorf("GET after the refused delete-all = %d, want 200", st)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

Run:
```
go test ./internal/domain/entity/ -run 'TestDeleteAllEntities_|TestDeleteEntitiesConditional_FastPath_RemovesTheModelsTasks'
go test ./internal/grpc/ -run 'TestRPC_EntityDeleteAll_FastPath'
go test ./api/ -run TestConflictCells
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_DeleteAll_'
```
Expected:
- entity: `Person tasks = 3, want 0`; `DeleteForModel calls = 0, want 1`;
  `DeleteForModel calls = 0, want 3`; `err = <nil>` (twice);
- gRPC: `tasks = 1, want 0`; `error = nil`;
- api: `409 description "Batched delete did not converge …" does not mention "scheduler"`;
- e2e: `tenant A task rows = 2, want 0`; the race test fails in
  `awaitBlocked`; the persistent test gets 200.

- [ ] **Step 3: Implement**

Replace `DeleteAllEntities` (`service.go:764-843`) with:

```go
// DeleteAllEntities deletes all entities of a model, and all the model's
// scheduled tasks, in one transaction. An owned delete that loses a
// task-row race with the scheduler runs again; a joined one does not.
func (h *Handler) DeleteAllEntities(ctx context.Context, entityName string, modelVersion string) (*DeleteAllResult, error) {
	var result *DeleteAllResult
	err := common.RetryOnTaskConflict(ctx, spi.GetTransaction(ctx) == nil, func() error {
		r, err := h.deleteAllEntitiesOnce(ctx, entityName, modelVersion)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// deleteAllEntitiesOnce is one attempt of DeleteAllEntities.
func (h *Handler) deleteAllEntitiesOnce(ctx context.Context, entityName string, modelVersion string) (*DeleteAllResult, error) {
```

The body of `deleteAllEntitiesOnce` is the old body of `DeleteAllEntities`
from `ref := spi.ModelRef{` (`:766`) to its end (`:842`), unchanged except
the finalize block (`:820-833`), which becomes:

```go
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		if err := entityStore.DeleteAll(txCtx, ref); err != nil {
			return deleteWriteError("failed to delete entities", err)
		}
		// Every task of the model goes in the same transaction.
		if err := h.deleteModelTasks(txCtx, ref); err != nil {
			return deleteWriteError("failed to delete scheduled tasks", err)
		}
		// Commit transaction (no-op when participating in a joined tx).
		if err := scope.Commit(); err != nil {
			return deleteWriteError("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}
```

Add next to `deleteEntityTasks`:

```go
// deleteModelTasks removes every scheduled task of ref inside the
// transaction on txCtx.
func (h *Handler) deleteModelTasks(txCtx context.Context, ref spi.ModelRef) error {
	tenant, err := txTenant(txCtx)
	if err != nil {
		return err
	}
	version, err := strconv.Atoi(ref.ModelVersion)
	if err != nil {
		return fmt.Errorf("failed to parse model version %q: %w", ref.ModelVersion, err)
	}
	sts, err := h.factory.ScheduledTaskStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to access scheduled task store: %w", err)
	}
	return sts.DeleteForModel(txCtx, tenant, ref.EntityName, version, nil)
}
```

The fast path (`:1217-1230`) calls `DeleteAllEntities` and needs no change:
the retry sits inside it.

In `api/openapi.yaml`, replace the `deleteEntities` `"409"` block
(`:2208-2234`) with:

```yaml
        "409":
          description: >-
            Conflict, retryable. `CONFLICT`: another write committed first to a
            selected entity or to one of its scheduled tasks — the scheduler
            changes a task when it claims, records, fails or gives it back. The
            server retries the delete up to 3 times first; a request that joins
            an open transaction is not retried on the server. With
            `transactionSize`, a batch that still conflicts is not a 409: its
            ids are listed in `idToError` and the other batches run.
            `DELETE_NOT_CONVERGED`: only with `transactionSize` and no
            `pointInTime`. The delete re-selects matching entities before each
            batch, and entities matching the condition were created at least as
            fast as they were removed, so it was stopped at its batch cap.
            Batches committed before the failure stay deleted. Stop the
            concurrent writers, narrow the condition, or retry.
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
              examples:
                Conflict:
                  description: Conflict
                  value:
                    type: about:blank
                    title: Conflict
                    status: 409
                    detail: "CONFLICT: transaction conflict — retry"
                    instance: /api/entity/my-model/1
                    properties:
                      errorCode: CONFLICT
                      retryable: true
                Delete Not Converged:
                  description: Delete Not Converged
                  value:
                    type: about:blank
                    title: Conflict
                    status: 409
                    detail: "DELETE_NOT_CONVERGED: delete did not converge after 1000000 batches:
                      entities matching the condition are being created as fast as they are
                      removed; stop the concurrent writers, narrow the condition, or retry"
                    instance: /api/entity/my-model/1
                    properties:
                      errorCode: DELETE_NOT_CONVERGED
                      retryable: true
```

- [ ] **Step 4: Run the tests and see them pass**

Run the Step 2 commands, then:
```
go generate ./api && git diff --stat api/generated.go
go test ./internal/domain/entity/ ./internal/grpc/ ./api/
```
Expected:
- all `ok`;
- no diff in `generated.go`;
- the existing delete-all tests stay green
  (`service_delete_unconditional_test.go`, `entity_deleteall_*_test.go`).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/service_delete_tasks_test.go \
  internal/grpc/entity_delete_tasks_test.go api/openapi.yaml api/openapi_conflict_cells_test.go \
  internal/e2e/scheduled_task_writes_test.go
git commit -m "feat(entity): delete-all removes the model's tasks; retry a task-row conflict

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-5: the conditional single-transaction delete removes the deleted entities' tasks

**Spec:** §7 entity-delete table row `DeleteEntitiesConditional` single-
transaction loop (`:1301`) "with the ids actually deleted"; retry table row
"the conditional single-transaction loop". §13 rows "conditional delete
removes tasks: single-tx" (U, E), "an owned single-transaction delete …
retried up to 3 times; a persistent conflict → 409" (loop), "a delete …
racing one claim succeeds" (loop), and the gRPC delete-all door with
`verbose`.

**Files:**
- Modify: `internal/domain/entity/service.go` (`DeleteEntitiesConditional` `:1230-1322`)
- Test: `internal/domain/entity/service_delete_tasks_test.go`
- Test: `internal/grpc/entity_delete_tasks_test.go`
- Test: `internal/e2e/scheduled_task_writes_test.go`

**Interfaces:**
- Produces: `func (h *Handler) deleteConditionalSingleTx(ctx context.Context, ref spi.ModelRef, cond predicate.Condition, pointInTime *time.Time, verbose bool) (*DeleteResult, error)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/domain/entity/service_delete_tasks_test.go` (add
`"context"`, `"fmt"` and `"sync"` to its imports):

```go
var ageAtLeastOne = []byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_OR_EQUAL","value":1}`)

// deleteRefusingStore wraps an EntityStore. Delete refuses the next
// conflicts calls with a first-committer-wins conflict, and always fails
// for failID with a plain error.
type deleteRefusingStore struct {
	spi.EntityStore
	mu        sync.Mutex
	conflicts int
	failID    string
}

func (s *deleteRefusingStore) takeConflict() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflicts == 0 {
		return false
	}
	s.conflicts--
	return true
}

func (s *deleteRefusingStore) Delete(ctx context.Context, id string) error {
	if id == s.failID {
		return errors.New("entity row unreadable")
	}
	if s.takeConflict() {
		return fmt.Errorf("entity row changed: %w", spi.ErrConflict)
	}
	return s.EntityStore.Delete(ctx, id)
}

func (e *taskEnv) refusingStore(t *testing.T, conflicts int, failID string) {
	t.Helper()
	real, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e.withEntityStore(t, &deleteRefusingStore{EntityStore: real, conflicts: conflicts, failID: failID})
}

func TestDeleteEntitiesConditional_SingleTx_RemovesOnlyTheDeletedEntitiesTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 3) // ages 0, 1, 2

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 {
		t.Fatalf("RemovedCount = %d, want 2", res.RemovedCount)
	}
	for i, want := range []int{1, 0, 0} {
		if n := e.tasksOf(t, ids[i]); n != want {
			t.Errorf("tasks of entity %d = %d, want %d", i, n, want)
		}
	}
}

func TestDeleteEntitiesConditional_SingleTx_FailedIDKeepsItsTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 3)
	e.refusingStore(t, 0, ids[2])

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if _, failed := res.IDToError[ids[2]]; !failed {
		t.Fatalf("IDToError = %v, want an entry for %s", res.IDToError, ids[2])
	}
	if n := e.tasksOf(t, ids[1]); n != 0 {
		t.Errorf("tasks of the deleted entity = %d, want 0", n)
	}
	if n := e.tasksOf(t, ids[2]); n != 1 {
		t.Errorf("tasks of the entity whose delete failed = %d, want 1", n)
	}
}

func TestDeleteEntitiesConditional_SingleTx_TaskConflict_RetriedWithAFreshResult(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	e.plan.Refuse(taskconflict.DeleteForEntities, 1)

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, true, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.MatchedCount != 2 || res.RemovedCount != 2 || len(res.IDs) != 2 {
		t.Errorf("Matched=%d Removed=%d IDs=%v, want 2, 2 and two ids: a retry starts a new result",
			res.MatchedCount, res.RemovedCount, res.IDs)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 2 {
		t.Errorf("DeleteForEntities calls = %d, want 2", got)
	}
}

func TestDeleteEntitiesConditional_SingleTx_TaskConflictPersists_Retryable409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)

	_, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	requireConflict409(t, err)
	if got, want := e.plan.Calls(taskconflict.DeleteForEntities), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForEntities calls = %d, want %d", got, want)
	}
	if !e.exists(t, ids[1]) {
		t.Error("entity removed although every attempt rolled back")
	}
}

func TestDeleteEntitiesConditional_SingleTx_Joined_NotRetried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)
	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })

	_, err = e.h.DeleteEntitiesConditional(joinedCtx, "Person", "1", ageAtLeastOne, nil, false, 0)
	requireConflict409(t, err)
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1", got)
	}
}

// An entity row changed after the snapshot fails the attempt, as on
// PostgreSQL, where the 40001 aborts the transaction. It is not one id's
// outcome, and the retry deletes every matched id.
func TestDeleteEntitiesConditional_SingleTx_EntityRowConflict_Retried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	e.refusingStore(t, 1, "")

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 || len(res.IDToError) != 0 {
		t.Errorf("Removed=%d IDToError=%v, want 2 and none", res.RemovedCount, res.IDToError)
	}
}
```

Add to `internal/grpc/entity_delete_tasks_test.go`:

```go
func TestRPC_EntityDeleteAll_Verbose_RemovesTasks(t *testing.T) {
	svc, ctx, real, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)

	if typed := deleteAllRPC(t, svc, ctx, map[string]any{"verbose": true}); !typed.Success {
		t.Fatalf("delete-all failed: %+v", typed.Error)
	}
	if got := plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1 (verbose takes the single-transaction loop)", got)
	}
	if n := personTasks(t, real, ctx, id); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}
```

Add to `internal/e2e/scheduled_task_writes_test.go`:

```go
const amountOver50 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}`

func TestScheduledTaskWrites_ConditionalDelete_RemovesTheDeletedEntitiesTasks(t *testing.T) {
	const model = "e2e-stw-cond"
	setupScheduledModel(t, model)
	kept := createEntityE2E(t, model, 1, `{"name":"Order","amount":10,"status":"draft"}`)
	gone := createEntityE2E(t, model, 1, schedWritesPayload)

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), amountOver50))
	if res.status != http.StatusOK {
		t.Fatalf("conditional delete: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", gone); n != 0 {
		t.Errorf("task rows of the deleted entity = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1", kept); n != 1 {
		t.Errorf("task rows of the kept entity = %d, want 1", n)
	}
}

func TestScheduledTaskWrites_ConditionalDelete_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-cond-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), amountOver50))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	if res := <-done; res.status != http.StatusOK {
		t.Fatalf("conditional delete racing a claim: %d %s, want 200", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_ConditionalDelete_PersistentConflict_409(t *testing.T) {
	const model = "e2e-stw-cond-persist"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "entity_id", id)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), amountOver50)))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

Run:
```
go test ./internal/domain/entity/ -run 'TestDeleteEntitiesConditional_SingleTx_'
go test ./internal/grpc/ -run TestRPC_EntityDeleteAll_Verbose_RemovesTasks
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_ConditionalDelete_'
```
Expected:
- entity:
  - `tasks of entity 1 = 1, want 0`;
  - `tasks of the deleted entity = 1, want 0`;
  - `DeleteForEntities calls = 0, want 2`;
  - `err = <nil>` (twice);
  - `Removed=1 IDToError=map[…: …ticket…], want 2 and none`;
- gRPC: `DeleteForEntities calls = 0, want 1`;
- e2e: `task rows of the deleted entity = 1, want 0`; the race test fails in
  `awaitBlocked`; the persistent test gets 200.

- [ ] **Step 3: Implement**

In `DeleteEntitiesConditional`, replace everything from `scope, err :=
h.beginScope(ctx)` (`:1230`) to the function's end (`:1322`) with:

```go
	var result *DeleteResult
	err := common.RetryOnTaskConflict(ctx, spi.GetTransaction(ctx) == nil, func() error {
		r, err := h.deleteConditionalSingleTx(ctx, ref, cond, pointInTime, verbose)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// deleteConditionalSingleTx is one attempt of the single-transaction
// conditional delete: select, delete each matched id, remove the scheduled
// tasks of the ids actually deleted, commit. Every attempt builds its own
// result, so a retried attempt never counts an id twice.
func (h *Handler) deleteConditionalSingleTx(ctx context.Context, ref spi.ModelRef, cond predicate.Condition, pointInTime *time.Time, verbose bool) (*DeleteResult, error) {
	scope, err := h.beginScope(ctx)
	if err != nil {
		return nil, classifyBeginErr(err)
	}
	defer scope.Release()

	txID, txCtx, owned := scope.TxID(), scope.Ctx(), scope.Owned()

	modelStore, err := h.factory.ModelStore(txCtx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if _, err := modelStore.Get(txCtx, ref); err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			return nil, common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%s", ref.EntityName, ref.ModelVersion))
		}
		return nil, common.Internal("failed to load model", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	plan, planErr := h.planDeleteSelection(txCtx, modelStore, ref, cond)
	if planErr != nil {
		return nil, planErr
	}

	// Select ALL matching ids via a streamed Iterate drain, scoped to
	// txCtx (tx-visible; honours pointInTime) — see selectDeleteIDs/
	// drainDeleteSelection for why only ids are ever retained and why the
	// iterator is fully closed before the delete loop below starts.
	ids, err := selectDeleteIDs(txCtx, entityStore, ref, plan, pointInTime)
	if err != nil {
		// A classified 4xx from planDeleteSelection/selection (unknown
		// field path, invalid condition) is the caller's error, not a
		// server fault — common.Internal would bury it as a 500 + ticket.
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, common.Internal("failed to select entities for delete", err)
	}

	result := &DeleteResult{
		EntityModelID: deterministicModelID(ref).String(),
		MatchedCount:  len(ids),
		IDToError:     map[string]string{},
		IDs:           []string{},
	}
	deleted := make([]string, 0, len(ids))

	// Finalize: gate the per-id deletes + commit against a concurrent joined
	// callback's buffer write (mirror DeleteAllEntities).
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		for _, id := range ids {
			// Generic cancellation check at the iteration head (spec D9) —
			// fires on ANY ctx cancellation, not only our own feature
			// deadline. Fails the IIFE (not break-and-commit) so the tx
			// rolls back: a partial delete pass must not be committed as if
			// it were the complete, requested set (fail closed).
			if err := ctx.Err(); err != nil {
				return classifyError(fmt.Errorf("operation aborted: %w", err))
			}
			if verbose {
				result.IDs = append(result.IDs, id)
			}
			if err := entityStore.Delete(txCtx, id); err != nil {
				// A first-committer-wins refusal is not this id's outcome.
				// The transaction is spent (PostgreSQL aborts it on 40001),
				// so the attempt fails and the whole call runs again.
				if errors.Is(err, spi.ErrConflict) {
					return conflictError(err)
				}
				result.IDToError[id] = perIDDeleteError(id, err)
				continue
			}
			deleted = append(deleted, id)
			result.RemovedCount++
		}
		// The scheduled tasks of the ids actually deleted go in the same
		// transaction. An id whose delete failed keeps its tasks.
		if err := h.deleteEntityTasks(txCtx, deleted); err != nil {
			return deleteWriteError("failed to delete scheduled tasks", err)
		}
		// Do NOT roll back after a failed commit — it has already aborted
		// the tx. Mirrors DeleteAllEntities.
		if err := scope.Commit(); err != nil {
			return deleteWriteError("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	return result, nil
}
```

The only changes to the moved code are:
- `entityName` / `modelVersion` in the 404 message become `ref.EntityName`
  / `ref.ModelVersion`;
- the conflict branch in the loop;
- `deleted` and the task removal;
- `deleteWriteError` at the commit.

- [ ] **Step 4: Run the tests and see them pass**

Run the Step 2 commands, then `go test ./internal/domain/entity/ ./internal/grpc/`.
Expected:
- all `ok`;
- the existing conditional-delete tests stay green (`delete_*_test.go`,
  `service_txjoin_test.go`, `untranslatable_refused_test.go`).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/service_delete_tasks_test.go \
  internal/grpc/entity_delete_tasks_test.go internal/e2e/scheduled_task_writes_test.go
git commit -m "feat(entity): conditional delete removes the deleted entities' tasks

The single-transaction loop removes the tasks of the ids it actually deleted,
and an owned call that loses a first-committer-wins race runs again with a
fresh result.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-6: batched delete removes each batch's tasks; a batch retries its own conflict

**Spec:** §7 entity-delete table row `deleteBatched`; retry table row
"`deleteBatched`: each batch … up to 3 times … persists → that batch's
`IDToError` … never 409"; §8.1 row `deleteEntities` (batched) 200. §13 rows
"conditional delete removes tasks: … batched" (U, E), "batched delete: a
conflicting batch retried up to 3 times; a persistent conflict reported per
id, never 409" (U, E), and the gRPC delete-all door with `transactionSize`.

**Files:**
- Modify: `internal/domain/entity/service.go` (`deleteOneBatch` and its doc comment, `:1630-1724`)
- Test: `internal/domain/entity/service_delete_tasks_test.go`
- Test: `internal/grpc/entity_delete_tasks_test.go`
- Test: `internal/e2e/scheduled_task_writes_test.go`

**Interfaces:**
- Produces: `type batchAttempt struct{ removed []string; idErrors map[string]string; failure *common.AppError }`;
  `func (h *Handler) deleteOneBatchOnce(ctx context.Context, chunk []batchTarget) (batchAttempt, error)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/domain/entity/service_delete_tasks_test.go`:

```go
func TestDeleteBatched_RemovesEachBatchsTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 3)

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 1)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 {
		t.Fatalf("RemovedCount = %d, want 2", res.RemovedCount)
	}
	for i, want := range []int{1, 0, 0} {
		if n := e.tasksOf(t, ids[i]); n != want {
			t.Errorf("tasks of entity %d = %d, want %d", i, n, want)
		}
	}
}

func TestDeleteBatched_TaskConflict_BatchRetried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	e.plan.Refuse(taskconflict.DeleteForEntities, 1)

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 1)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 || len(res.IDToError) != 0 {
		t.Errorf("Removed=%d IDToError=%v, want 2 and none", res.RemovedCount, res.IDToError)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 3 {
		t.Errorf("DeleteForEntities calls = %d, want 3 (one refusal, then one per batch)", got)
	}
}

func TestDeleteBatched_TaskConflictPersists_ReportedPerIDNever409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 3)
	e.plan.Refuse(taskconflict.DeleteForEntities, 1000)

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 1)
	if err != nil {
		t.Fatalf("err = %v, want a 200 result with per-id errors", err)
	}
	if res.RemovedCount != 0 {
		t.Errorf("RemovedCount = %d, want 0", res.RemovedCount)
	}
	for _, id := range ids[1:] {
		msg, ok := res.IDToError[id]
		if !ok || !strings.HasPrefix(msg, common.ErrCodeConflict+":") {
			t.Errorf("IDToError[%s] = %q, want a CONFLICT entry", id, msg)
		}
		if !e.exists(t, id) || e.tasksOf(t, id) != 1 {
			t.Errorf("entity %s or its task is gone although its batch never committed", id)
		}
	}
	if got, want := e.plan.Calls(taskconflict.DeleteForEntities), 2*(1+common.TaskConflictRetries); got != want {
		t.Errorf("DeleteForEntities calls = %d, want %d", got, want)
	}
}

func TestDeleteBatched_EntityRowConflict_BatchRetried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	e.refusingStore(t, 1, "")

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 || len(res.IDToError) != 0 {
		t.Errorf("Removed=%d IDToError=%v, want 2 and none", res.RemovedCount, res.IDToError)
	}
}
```

Add `"strings"` to that file's imports.

Add to `internal/grpc/entity_delete_tasks_test.go`:

```go
func TestRPC_EntityDeleteAll_Batched_RemovesTasks(t *testing.T) {
	svc, ctx, real, _ := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)

	if typed := deleteAllRPC(t, svc, ctx, map[string]any{"transactionSize": 1}); !typed.Success {
		t.Fatalf("delete-all failed: %+v", typed.Error)
	}
	if n := personTasks(t, real, ctx, id); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestRPC_EntityDeleteAll_Batched_TaskConflictPersists_ErrorsByID(t *testing.T) {
	svc, ctx, real, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	plan.Refuse(taskconflict.DeleteForEntities, 1000)

	typed := deleteAllRPC(t, svc, ctx, map[string]any{"transactionSize": 1})
	if !typed.Success {
		t.Fatalf("success = false (%+v), want true: a batched delete reports a conflict per id", typed.Error)
	}
	msg, _ := typed.ErrorsByID[id].(string)
	if !strings.HasPrefix(msg, common.ErrCodeConflict+":") {
		t.Errorf("ErrorsByID[%s] = %q, want a CONFLICT entry", id, msg)
	}
	if n := personTasks(t, real, ctx, id); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}
```

Add to `internal/e2e/scheduled_task_writes_test.go` (add `"encoding/json"`
to its imports):

```go
type batchedDeleteBody struct {
	DeleteResult struct {
		IDToError map[string]string `json:"idToError"`
		Removed   int               `json:"numberOfEntititesRemoved"`
	} `json:"deleteResult"`
}

func decodeBatched(t *testing.T, body string) batchedDeleteBody {
	t.Helper()
	var b batchedDeleteBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("decode delete result: %v; body: %s", err, body)
	}
	return b
}

func TestScheduledTaskWrites_BatchedDelete_RacingOneClaim_BatchSucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-batched-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?transactionSize=1", model), amountOver50))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	res := <-done
	if res.status != http.StatusOK {
		t.Fatalf("batched delete racing a claim: %d %s", res.status, res.body)
	}
	if b := decodeBatched(t, res.body); b.DeleteResult.Removed != 1 || len(b.DeleteResult.IDToError) != 0 {
		t.Errorf("removed=%d idToError=%v, want 1 and none", b.DeleteResult.Removed, b.DeleteResult.IDToError)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_BatchedDelete_PersistentConflict_PerIDNot409(t *testing.T) {
	const model = "e2e-stw-batched-persist"
	setupScheduledModel(t, model)
	stuck := createEntityE2E(t, model, 1, schedWritesPayload)
	free := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "entity_id", stuck)

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?transactionSize=1", model), amountOver50))
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: a batched delete never answers 409 for a batch; body: %s", res.status, res.body)
	}
	b := decodeBatched(t, res.body)
	if msg := b.DeleteResult.IDToError[stuck]; !strings.HasPrefix(msg, common.ErrCodeConflict+":") {
		t.Errorf("idToError[stuck] = %q, want a CONFLICT entry", msg)
	}
	if b.DeleteResult.Removed != 1 {
		t.Errorf("removed = %d, want 1 (the other batch ran)", b.DeleteResult.Removed)
	}
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("attempts of the stuck batch = %d, want %d", got, want)
	}
	if n := taskRows(t, "entity_id = $1", free); n != 0 {
		t.Errorf("task rows of the deleted entity = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1", stuck); n != 1 {
		t.Errorf("task rows of the stuck entity = %d, want 1", n)
	}
}
```

Add `"strings"` to that file's imports.

- [ ] **Step 2: Run the tests and see them fail**

Run:
```
go test ./internal/domain/entity/ -run 'TestDeleteBatched_'
go test ./internal/grpc/ -run 'TestRPC_EntityDeleteAll_Batched_'
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_BatchedDelete_'
```
Expected:
- entity:
  - `tasks of entity 1 = 1, want 0`;
  - `DeleteForEntities calls = 0, want 3`;
  - `RemovedCount = 2, want 0`;
  - `Removed=1 IDToError=map[…], want 2 and none`;
- gRPC: `tasks = 1, want 0`; `ErrorsByID[…] = "", want a CONFLICT entry`;
- e2e: the race test fails in `awaitBlocked`; the persistent test fails with
  `idToError[stuck] = "", want a CONFLICT entry`.

- [ ] **Step 3: Implement**

Replace `deleteOneBatch` and its doc comment (`service.go:1630-1724`) with:

```go
// batchAttempt is one run of a batch's transaction.
type batchAttempt struct {
	// removed are the ids whose delete was staged. They are durable only
	// when failure is nil.
	removed []string
	// idErrors are per-id outcomes that do not fail the batch: a missing
	// entity, a version changed since resolution, a failed delete.
	idErrors map[string]string
	// failure is the batch's own failure — its task removal or its commit —
	// or nil when the batch committed.
	failure *common.AppError
}

// deleteOneBatch deletes one chunk of ≤batchSize targets under its own owned
// transaction, with the tasks of the ids it deletes (spec D4's version guard
// applies to each id). A batch that loses a first-committer-wins race runs
// again, at most common.TaskConflictRetries more times, against the same
// baselines. Per-id outcomes of the last attempt go into result.IDToError. A
// batch that still fails puts its message on every id it did not resolve,
// and adds nothing to RemovedCount — its deletes never became durable. Only a
// failure to begin the transaction or to reach the entity store is
// returned; deleteBatched treats that as fatal for the request.
func (h *Handler) deleteOneBatch(ctx context.Context, chunk []batchTarget, result *DeleteResult) error {
	var last batchAttempt
	err := common.RetryOnTaskConflict(ctx, spi.GetTransaction(ctx) == nil, func() error {
		a, err := h.deleteOneBatchOnce(ctx, chunk)
		if err != nil {
			return err
		}
		last = a
		if a.failure != nil && errors.Is(a.failure, spi.ErrConflict) {
			return a.failure
		}
		return nil
	})
	if err != nil && !errors.Is(err, spi.ErrConflict) {
		return err
	}

	for id, msg := range last.idErrors {
		result.IDToError[id] = msg
	}
	if last.failure == nil {
		result.RemovedCount += len(last.removed)
		return nil
	}
	// Operational (4xx-class) failures — a conflict above all — are
	// client-safe by construction: fold the message as-is. Anything else
	// gets ONE ticket for the whole batch, its cause logged under it, and
	// the ticketed client-safe message — never the raw AppError.Message.
	msg := last.failure.Message
	if last.failure.Level != common.LevelOperational {
		cause := error(last.failure)
		if last.failure.Err != nil {
			cause = last.failure.Err
		}
		msg = mintDeleteTicket("", cause)
	}
	for _, t := range chunk {
		if _, resolved := last.idErrors[t.id]; !resolved {
			result.IDToError[t.id] = msg
		}
	}
	return nil
}

// deleteOneBatchOnce is one attempt of a batch. It re-reads each target,
// deletes it if its version still equals the resolution baseline, removes
// the tasks of the ids it deleted, and commits.
func (h *Handler) deleteOneBatchOnce(ctx context.Context, chunk []batchTarget) (batchAttempt, error) {
	a := batchAttempt{idErrors: map[string]string{}}

	scope, err := h.beginScope(ctx)
	if err != nil {
		return a, classifyBeginErr(err)
	}
	defer scope.Release()

	txID, txCtx, owned := scope.TxID(), scope.Ctx(), scope.Owned()

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		return a, common.Internal("failed to access entity store", err)
	}

	// Finalize: gate the per-id deletes + commit against a concurrent joined
	// callback's buffer write (mirror the single-tx path / DeleteAllEntities).
	a.failure = func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		for _, t := range chunk {
			// Generic cancellation check at the iteration head (spec D9) —
			// fails the batch closed so its tx rolls back rather than
			// committing a partial pass through the chunk.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return classifyError(fmt.Errorf("operation aborted: %w", ctxErr))
			}
			cur, gErr := entityStore.Get(txCtx, t.id)
			if gErr != nil {
				if errors.Is(gErr, spi.ErrConflict) {
					return conflictError(gErr)
				}
				a.idErrors[t.id] = perIDDeleteError(t.id, gErr)
				continue
			}
			if cur.Meta.Version != t.baselineVersion {
				a.idErrors[t.id] = fmt.Sprintf("%s: entity id=%s modified after delete resolution; not deleted",
					common.ErrCodeEntityModified, t.id)
				continue
			}
			if dErr := entityStore.Delete(txCtx, t.id); dErr != nil {
				// A first-committer-wins refusal fails the batch, which runs
				// again; it is not this id's outcome.
				if errors.Is(dErr, spi.ErrConflict) {
					return conflictError(dErr)
				}
				a.idErrors[t.id] = perIDDeleteError(t.id, dErr)
				continue
			}
			a.removed = append(a.removed, t.id)
		}
		if err := h.deleteEntityTasks(txCtx, a.removed); err != nil {
			return deleteWriteError("failed to delete scheduled tasks", err)
		}
		if err := scope.Commit(); err != nil {
			return deleteWriteError("failed to commit transaction", err)
		}
		return nil
	}()
	return a, nil
}
```

The closure appends to `a.removed` and writes `a.idErrors` before its return
value is assigned to `a.failure`. The closure runs synchronously, so the
order is defined.

One change to the old folding: when a batch fails, its message now also goes
on the ids the batch never reached (a conflict, or a cancellation part-way
through). Before, those ids were in neither `IDToError` nor `RemovedCount`,
although `MatchedCount` counted them. The existing tests
`TestDeleteEntitiesConditional_Batched_FailedBatchContinues` and
`…_FailedBatchIsTicketed` fail at commit, after every id was reached, so
their counts do not change.

- [ ] **Step 4: Run the tests and see them pass**

Run the Step 2 commands, then:
```
go test ./internal/domain/entity/ ./internal/grpc/
go test ./internal/e2e/ -run 'TestDeleteEntities_Batched|TestScheduledTaskWrites_'
```
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/service_delete_tasks_test.go \
  internal/grpc/entity_delete_tasks_test.go internal/e2e/scheduled_task_writes_test.go
git commit -m "feat(entity): batched delete removes each batch's tasks; a batch retries its conflict

A batch that loses a first-committer-wins race runs again up to 3 times; one
that still conflicts is reported per id and the other batches run.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-7: workflow import removes the tasks of transitions no longer scheduled

**Spec:**
- §7 "Workflow import": save first, then remove in the import's own
  transaction with `keep` taken from all the model's workflows; retry up to
  3 times; persistent conflict → 409; a retried import saves the same
  workflows again.
- §8.1 row `importEntityModelWorkflow`.
- §4 "the transition stops being scheduled → removed at the next write or
  import".
- §13 rows:
  - "a workflow import that drops schedules removes the model's tasks" (U, E);
  - "import: workflows saved before task removal; the removal retried; a
    persistent conflict → 409; re-import succeeds" (U, E);
  - "a delete or import racing one claim succeeds after the server retry"
    (import).

**Order:** after stream E completes (README "Order of work", C-P7). W-7
calls E-2's `armsOnSchedule` and does not edit `arm.go` or
`fire_scheduled.go`, which E owns. W-1 … W-6 may run beside E.

**Files:**
- Create: `internal/domain/workflow/import_tasks.go`
- Modify: `internal/domain/workflow/handler.go` (after `wfStore.Save`, `:373-376`)
- Modify: `api/openapi.yaml` (`importEntityModelWorkflow` responses, after `"404"` `:5207-5212`)
- Test: `internal/domain/workflow/import_tasks_test.go` (new)
- Test: `api/openapi_conflict_cells_test.go`
- Test: `internal/e2e/scheduled_task_import_test.go` (new)

**Interfaces:**
- Consumes: `spi.ScheduledTaskStore.DeleteForModel` with a non-nil `keep`;
  `common.RetryOnTaskConflict`; E-2's `armsOnSchedule(tr *spi.TransitionDefinition) bool`,
  the one arm rule (README C-P7).
- Produces:
  - `func scheduledTransitions(wfs []spi.WorkflowDefinition) func(sourceState, transition string) bool`;
  - `func (h *Handler) removeUnscheduledTasks(ctx context.Context, ref spi.ModelRef, wfs []spi.WorkflowDefinition) *common.AppError`.

- [ ] **Step 1: Write the failing tests**

`internal/domain/workflow/import_tasks_test.go`:

```go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const importTenant spi.TenantID = "import-tasks-tenant"

// dropRemindImport keeps AutoClose scheduled and makes Remind manual with no
// schedule.
const dropRemindImport = `{"importMode":"REPLACE","workflows":[{
	"version":"1.1","name":"import-sched","initialState":"OPEN","active":true,
	"states":{
		"OPEN":{"transitions":[
			{"name":"AutoClose","next":"CLOSED","manual":false,"schedule":{"delayMs":3600000}},
			{"name":"Remind","next":"REMINDED","manual":true}]},
		"CLOSED":{},
		"REMINDED":{}}}]}`

type importTaskEnv struct {
	h     *Handler
	ctx   context.Context
	real  *memory.StoreFactory
	txMgr spi.TransactionManager
	plan  *taskconflict.Plan
}

func newImportTaskEnv(t *testing.T) *importTaskEnv {
	t.Helper()
	real := memory.NewStoreFactory()
	t.Cleanup(func() { real.Close() })
	txMgr, err := real.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "import-user", UserName: "Import", Tenant: spi.Tenant{ID: importTenant, Name: "Import"}, Roles: []string{"ROLE_ADMIN"},
	})
	node := schema.NewObjectNode()
	node.SetChild("k", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := real.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	for _, name := range []string{"sched-import", "sched-other"} {
		if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: spi.ModelRef{EntityName: name, ModelVersion: "1"}, State: spi.ModelLocked, Schema: raw}); err != nil {
			t.Fatalf("ModelStore.Save: %v", err)
		}
	}
	plan := taskconflict.NewPlan()
	factory := &taskconflict.Factory{StoreFactory: real, Plan: plan}
	engine := NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	return &importTaskEnv{h: New(factory, engine, 60*time.Second), ctx: ctx, real: real, txMgr: txMgr, plan: plan}
}

// arm arms one OPEN task per transition for entityID of model.
func (e *importTaskEnv) arm(t *testing.T, model, entityID string, transitions ...string) {
	t.Helper()
	txID, txCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sts, err := e.real.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	arm := make([]spi.ScheduledTask, 0, len(transitions))
	for _, tr := range transitions {
		arm = append(arm, spi.ScheduledTask{
			ID: taskID(importTenant, entityID, "OPEN", tr), TenantID: importTenant, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: time.Now().Add(time.Hour).UnixMilli(), EntityID: entityID,
			ModelName: model, ModelVersion: 1, Transition: tr, SourceState: "OPEN",
		})
	}
	if _, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: importTenant, EntityID: entityID, CurrentState: "OPEN", Arm: arm,
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if err := e.txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// transitions lists the transitions of model's tasks, sorted.
func (e *importTaskEnv) transitions(t *testing.T, model string) []string {
	t.Helper()
	sts, err := e.real.ScheduledTaskStore(e.ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(e.ctx, importTenant, spi.ScheduledTaskQuery{ModelName: model, ModelVersion: 1, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	out := []string{}
	for _, it := range page.Items {
		out = append(out, it.Transition)
	}
	sort.Strings(out)
	return out
}

func (e *importTaskEnv) importWorkflows(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/model/sched-import/1/workflow/import", strings.NewReader(body)).WithContext(e.ctx)
	rec := httptest.NewRecorder()
	e.h.ImportEntityModelWorkflow(rec, req, "sched-import", 1)
	return rec
}

func (e *importTaskEnv) seed(t *testing.T) {
	t.Helper()
	e.arm(t, "sched-import", "e-1", "AutoClose", "Remind")
	e.arm(t, "sched-other", "e-2", "Remind")
}

func TestImport_RemovesTasksOfTransitionsNoLongerScheduled(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("sched-import tasks = %v, want [AutoClose]", got)
	}
	if got := e.transitions(t, "sched-other"); strings.Join(got, ",") != "Remind" {
		t.Errorf("sched-other tasks = %v, want [Remind]: another model's tasks stay", got)
	}
}

func TestImport_TaskConflict_RetriedThenSucceeds(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.plan.Refuse(taskconflict.DeleteForModel, 2)

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 3 {
		t.Errorf("DeleteForModel calls = %d, want 3", got)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("tasks = %v, want [AutoClose]", got)
	}
}

func TestImport_TaskConflictPersists_409_WorkflowsAlreadySaved_ReimportSucceeds(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.plan.Refuse(taskconflict.DeleteForModel, 100)

	rec := e.importWorkflows(t, dropRemindImport)
	if rec.Code != http.StatusConflict {
		t.Fatalf("import: %d %s, want 409", rec.Code, rec.Body)
	}
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pd.Properties.ErrorCode != common.ErrCodeConflict || !pd.Properties.Retryable {
		t.Errorf("errorCode=%q retryable=%v, want CONFLICT retryable", pd.Properties.ErrorCode, pd.Properties.Retryable)
	}
	if got, want := e.plan.Calls(taskconflict.DeleteForModel), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForModel calls = %d, want %d", got, want)
	}
	wfStore, err := e.real.WorkflowStore(e.ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	saved, err := wfStore.Get(e.ctx, spi.ModelRef{EntityName: "sched-import", ModelVersion: "1"})
	if err != nil || len(saved) != 1 || saved[0].Name != "import-sched" {
		t.Fatalf("saved workflows = %+v (err %v), want the imported one: the save comes first", saved, err)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose,Remind" {
		t.Errorf("tasks = %v, want both: every removal rolled back", got)
	}

	e.plan.Reset()
	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("re-import: %d %s", rec.Code, rec.Body)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("tasks after re-import = %v, want [AutoClose]", got)
	}
}

func TestImport_TaskStoreFailure_500NotRetried(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.plan.Fail(taskconflict.DeleteForModel, errors.New("task store unreachable"))

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusInternalServerError {
		t.Fatalf("import: %d %s, want 500", rec.Code, rec.Body)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1", got)
	}
}

func TestScheduledTransitions_KeepRule(t *testing.T) {
	sched := &spi.TransitionSchedule{DelayMs: 1000}
	keep := scheduledTransitions([]spi.WorkflowDefinition{
		{Name: "active", Active: true, States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "armed", Schedule: sched},
				{Name: "manual", Manual: true, Schedule: sched},
				{Name: "disabled", Disabled: true, Schedule: sched},
				{Name: "plain"},
			}},
		}},
		{Name: "inactive", Active: false, States: map[string]spi.StateDefinition{
			"B": {Transitions: []spi.TransitionDefinition{{Name: "armed-inactive", Schedule: sched}}},
		}},
	})
	cases := []struct {
		state, transition string
		want              bool
	}{
		{"A", "armed", true},
		{"A", "manual", false},
		{"A", "disabled", false},
		{"A", "plain", false},
		{"B", "armed-inactive", true},
		{"B", "armed", false},
		{"A", "missing", false},
	}
	for _, c := range cases {
		if got := keep(c.state, c.transition); got != c.want {
			t.Errorf("keep(%q, %q) = %v, want %v", c.state, c.transition, got, c.want)
		}
	}
}
```

Extend `api/openapi_conflict_cells_test.go`: the loop's list becomes
`[]string{"deleteSingleEntity", "deleteEntities", "importEntityModelWorkflow"}`.

`internal/e2e/scheduled_task_import_test.go`:

```go
package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const importBothScheduled = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "stw-import-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}},
				{"name": "Remind", "next": "REMINDED", "manual": false, "schedule": {"delayMs": 3600000}}
			]},
			"CLOSED": {},
			"REMINDED": {}
		}
	}]
}`

const importRemindUnscheduled = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "stw-import-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}},
				{"name": "Remind", "next": "REMINDED", "manual": true}
			]},
			"CLOSED": {},
			"REMINDED": {}
		}
	}]
}`

func importPath(model string) string { return fmt.Sprintf("/api/model/%s/1/workflow/import", model) }

func TestScheduledTaskWrites_Import_DroppingAScheduleRemovesItsTasks(t *testing.T) {
	const model = "e2e-stw-import"
	setupModelWithWorkflow(t, model, importBothScheduled)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	if n := taskRows(t, "entity_id = $1", id); n != 2 {
		t.Fatalf("task rows before = %d, want 2", n)
	}

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodPost, importPath(model), importRemindUnscheduled))
	if res.status != http.StatusOK {
		t.Fatalf("import: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'Remind'", id); n != 0 {
		t.Errorf("Remind task rows = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'AutoClose'", id); n != 1 {
		t.Errorf("AutoClose task rows = %d, want 1", n)
	}
}

func TestScheduledTaskWrites_Import_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-import-race"
	setupModelWithWorkflow(t, model, importBothScheduled)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1 AND transition = 'Remind'", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() { done <- resultOf(doAuthOnceRaw(ctx, http.MethodPost, importPath(model), importRemindUnscheduled)) }()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	if res := <-done; res.status != http.StatusOK {
		t.Fatalf("import racing a claim: %d %s, want 200", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'Remind'", id); n != 0 {
		t.Errorf("Remind task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_Import_PersistentConflict_409ThenReimportSucceeds(t *testing.T) {
	const model = "e2e-stw-import-persist"
	setupModelWithWorkflow(t, model, importBothScheduled)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "model_name", model)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodPost, importPath(model), importRemindUnscheduled)))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
	status, exported := exportWorkflowE2E(t, model, 1)
	if status != http.StatusOK {
		t.Fatalf("export: %d", status)
	}
	if raw := fmt.Sprint(exported); !strings.Contains(raw, "Remind") || strings.Count(raw, "delayMs") != 1 {
		t.Errorf("exported workflows = %s, want Remind without a schedule: the save comes before the removal", raw)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 2 {
		t.Errorf("task rows = %d, want 2", n)
	}

	trig.remove(t)
	if res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodPost, importPath(model), importRemindUnscheduled)); res.status != http.StatusOK {
		t.Fatalf("re-import: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'Remind'", id); n != 0 {
		t.Errorf("Remind task rows after re-import = %d, want 0", n)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

Run:
```
go test ./internal/domain/workflow/ -run 'TestImport_.*Task|TestScheduledTransitions_KeepRule'
go test ./api/ -run TestConflictCells
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_Import_'
```
Expected:
- workflow: build failure, `undefined: scheduledTransitions` (Open point 6);
- api: `importEntityModelWorkflow`: `no 409 response declared`;
- e2e: `Remind task rows = 1, want 0`; the race test fails in
  `awaitBlocked`; the persistent test gets 200.

- [ ] **Step 3: Implement**

`internal/domain/workflow/import_tasks.go`:

```go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// scheduledTransitions returns the keep function for DeleteForModel: it
// keeps a task whose (source state, transition) some workflow of the model
// arms by the one arm rule, armsOnSchedule (arm.go), which modelHasSchedule
// uses too. Every workflow counts, active or not — a task a workflow could still
// fire is never removed here; the fire itself cancels one that no selected
// workflow schedules.
func scheduledTransitions(wfs []spi.WorkflowDefinition) func(sourceState, transition string) bool {
	type pair struct{ state, transition string }
	armed := make(map[pair]struct{})
	for _, wf := range wfs {
		for state, sd := range wf.States {
			for i := range sd.Transitions {
				if armsOnSchedule(&sd.Transitions[i]) {
					armed[pair{state, sd.Transitions[i].Name}] = struct{}{}
				}
			}
		}
	}
	return func(sourceState, transition string) bool {
		_, ok := armed[pair{sourceState, transition}]
		return ok
	}
}

// removeUnscheduledTasks removes the model's tasks whose transition no
// workflow of the model schedules any more. It runs after the workflows are
// saved, in its own transaction, so a failed removal never loses a timer that
// is still scheduled. A removal that loses a task-row race with the
// scheduler runs again, at most common.TaskConflictRetries more times; one
// that still conflicts is a retryable 409, and a retried import saves the
// same workflows and removes again.
func (h *Handler) removeUnscheduledTasks(ctx context.Context, ref spi.ModelRef, wfs []spi.WorkflowDefinition) *common.AppError {
	version, err := strconv.Atoi(ref.ModelVersion)
	if err != nil {
		return common.Internal("invalid model version", err)
	}
	keep := scheduledTransitions(wfs)
	err = common.RetryOnTaskConflict(ctx, true, func() error {
		return h.removeUnscheduledTasksOnce(ctx, ref.EntityName, version, keep)
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, spi.ErrConflict) {
		return common.Operational(http.StatusConflict, common.ErrCodeConflict,
			"workflows saved, but removing the tasks of transitions no longer scheduled conflicted with the scheduler — retry the import").
			AsRetryable().WithCause(err)
	}
	return common.Internal("failed to remove scheduled tasks", err)
}

// removeUnscheduledTasksOnce is one attempt, in its own transaction.
func (h *Handler) removeUnscheduledTasksOnce(ctx context.Context, name string, version int, keep func(string, string) bool) error {
	txMgr := h.engine.txMgr
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	done := false
	defer func() {
		if done {
			return
		}
		rbCtx, cancel := common.RollbackContext(txCtx)
		defer cancel()
		if rbErr := txMgr.Rollback(rbCtx, txID); rbErr != nil && !errors.Is(rbErr, spi.ErrTxNotFound) {
			slog.Warn("failed to roll back transaction", "pkg", "workflow", "txID", txID, "err", rbErr)
		}
	}()

	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		return errors.New("no transaction on the context after Begin")
	}
	sts, err := h.factory.ScheduledTaskStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to access scheduled task store: %w", err)
	}
	if err := sts.DeleteForModel(txCtx, tx.TenantID, name, version, keep); err != nil {
		return fmt.Errorf("failed to remove scheduled tasks: %w", err)
	}
	// After a commit attempt the transaction is finished, whatever the
	// outcome.
	done = true
	if err := common.ShieldedCommit(txCtx, func(commitCtx context.Context) error {
		return txMgr.Commit(commitCtx, txID)
	}); err != nil {
		return fmt.Errorf("failed to commit task removal: %w", err)
	}
	return nil
}
```

`internal/domain/workflow/handler.go`, right after the `wfStore.Save` block
(`:373-376`):

```go
	// The tasks of transitions that no workflow of the model schedules any
	// more go now, after the save.
	if appErr := h.removeUnscheduledTasks(r.Context(), ref, result); appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}
```

In `api/openapi.yaml`, under `importEntityModelWorkflow` responses, after the
`"404"` block (`:5207-5212`):

```yaml
        "409":
          description: >-
            Conflict (`CONFLICT`, retryable). The workflows are saved; removing
            the tasks of transitions that no workflow of the model schedules any
            more lost a race with the scheduler, which changes a task when it
            claims, records, fails or gives it back. The server retries the
            removal up to 3 times before it answers 409. Retry the import: it
            saves the same workflows again and removes the tasks.
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
```

- [ ] **Step 4: Run the tests and see them pass**

Run the Step 2 commands, then:
```
go generate ./api && git diff --stat api/generated.go
go test ./internal/domain/workflow/ ./api/
go test ./internal/e2e/ -run 'TestScheduledTaskWrites_|Workflow'
```
Expected:
- all `ok`;
- no diff in `generated.go`;
- the existing import tests (`handler_test.go`, full app on memory) stay
  green: an import on a model with no tasks calls `DeleteForModel` once and
  it removes nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/import_tasks.go internal/domain/workflow/import_tasks_test.go \
  internal/domain/workflow/handler.go \
  api/openapi.yaml api/openapi_conflict_cells_test.go internal/e2e/scheduled_task_import_test.go
git commit -m "feat(workflow): import removes the tasks of transitions no longer scheduled

The import saves the workflows, then removes, in its own transaction, the
model's tasks that no workflow schedules. A conflict with the scheduler is
retried up to 3 times, then answers a retryable 409; importEntityModelWorkflow
declares it. The import keeps a task only if its transition arms by
armsOnSchedule, the rule arm and fire use.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task W-8: parity scenarios — delete paths, import drop, re-arm

**Spec:** §13 P cells:
- "deleting one entity removes its tasks";
- "conditional delete removes tasks: single-tx, batched, fast path";
- "delete-all removes the model's tasks";
- "a workflow import that drops schedules removes the model's tasks".

This task also adds re-arm after a restored schedule. A write in the state
arms a new task for a transition that is scheduled again (§7 "Arm").

**Files:**
- Create: `e2e/parity/scheduledtransition/task_writes.go`

**Order:** after Q-6 (README "Order of work", C-P6). The scenarios read
`GET /scheduled-tasks`, which is wired only at Q-6, through Q-5's parity
client.

**Interfaces:**
- Consumes: Q-5's `(*client.Client).ListScheduledTasks(t, url.Values) (client.ScheduledTaskPage, error)`
  and `client.ScheduledTask` (`e2e/parity/client/scheduled_tasks.go`); the
  `entityId`, `limit` and `modelName` + `modelVersion` filters, wired at Q-6.
- Produces: five `parity.NamedTest`s registered through `parity.Register`.

TDD note: the behaviour's RED was observed in W-2 … W-7. This task adds the
cross-backend layer. Step 2 proves each scenario can fail: it runs them
against a build with the removal taken out.

- [ ] **Step 1: Write the scenarios**

```go
package scheduledtransition

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ScheduledTask_DeleteEntityRemovesItsTasks", Fn: RunScheduledTask_DeleteEntityRemovesItsTasks},
		parity.NamedTest{Name: "ScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks", Fn: RunScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks},
		parity.NamedTest{Name: "ScheduledTask_BatchedDeleteRemovesTasks", Fn: RunScheduledTask_BatchedDeleteRemovesTasks},
		parity.NamedTest{Name: "ScheduledTask_DeleteAllRemovesModelTasks", Fn: RunScheduledTask_DeleteAllRemovesModelTasks},
		parity.NamedTest{Name: "ScheduledTask_ImportDropRemovesTasksAndRestoreReArms", Fn: RunScheduledTask_ImportDropRemovesTasksAndRestoreReArms},
	)
}

// hourSchedule arms AutoClose one hour after an entity enters OPEN; no
// scenario runs that long, so no scheduler ever fires one of these tasks.
const hourSchedule = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "task-writes-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}}]},
			"CLOSED": {}
		}
	}]
}`

const hourScheduleDropped = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "task-writes-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": true}]},
			"CLOSED": {}
		}
	}]
}`

const kOver1 = `{"type":"simple","jsonPath":"$.k","operatorType":"GREATER_THAN","value":1}`

// listTasks reads GET /api/scheduled-tasks with query, through Q-5's client.
func listTasks(t *testing.T, c *client.Client, query url.Values) []client.ScheduledTask {
	t.Helper()
	query.Set("limit", "1000")
	page, err := c.ListScheduledTasks(t, query)
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	return page.Items
}

func entityTasks(t *testing.T, c *client.Client, id uuid.UUID) []client.ScheduledTask {
	t.Helper()
	return listTasks(t, c, url.Values{"entityId": {id.String()}})
}

func modelTaskCount(t *testing.T, c *client.Client, model string) int {
	t.Helper()
	return len(listTasks(t, c, url.Values{"modelName": {model}, "modelVersion": {"1"}}))
}

// seedK creates one entity per k value and checks each has one task.
func seedK(t *testing.T, c *client.Client, model string, ks ...int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, len(ks))
	for _, k := range ks {
		id, err := c.CreateEntity(t, model, 1, fmt.Sprintf(`{"k":%d}`, k))
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		if n := len(entityTasks(t, c, id)); n != 1 {
			t.Fatalf("tasks of a new entity = %d, want 1", n)
		}
		ids = append(ids, id)
	}
	return ids
}

func RunScheduledTask_DeleteEntityRemovesItsTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-delete-one"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1, 2)

	if err := c.DeleteEntity(t, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	if n := len(entityTasks(t, c, ids[0])); n != 0 {
		t.Errorf("tasks of the deleted entity = %d, want 0", n)
	}
	if n := len(entityTasks(t, c, ids[1])); n != 1 {
		t.Errorf("tasks of the other entity = %d, want 1", n)
	}
}

func RunScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-cond"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1, 2, 3)

	res, err := c.DeleteEntitiesConditional(t, model, 1, kOver1, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 {
		t.Fatalf("removed = %d, want 2", res.RemovedCount)
	}
	for i, want := range []int{1, 0, 0} {
		if n := len(entityTasks(t, c, ids[i])); n != want {
			t.Errorf("tasks of entity k=%d = %d, want %d", i+1, n, want)
		}
	}
}

func RunScheduledTask_BatchedDeleteRemovesTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-batched"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1, 2, 3)

	res, err := c.DeleteEntitiesConditional(t, model, 1, kOver1, 1)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 || len(res.IDToError) != 0 {
		t.Fatalf("removed=%d idToError=%v, want 2 and none", res.RemovedCount, res.IDToError)
	}
	for i, want := range []int{1, 0, 0} {
		if n := len(entityTasks(t, c, ids[i])); n != want {
			t.Errorf("tasks of entity k=%d = %d, want %d", i+1, n, want)
		}
	}
}

func RunScheduledTask_DeleteAllRemovesModelTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-delete-all"
	const other = "task-writes-delete-all-other"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	setupModelWithWorkflow(t, c, other, 1, `{"k":1}`, hourSchedule)
	seedK(t, c, model, 1, 2)
	seedK(t, c, other, 1)

	if err := c.DeleteEntitiesByModel(t, model, 1); err != nil {
		t.Fatalf("DeleteEntitiesByModel: %v", err)
	}
	if n := modelTaskCount(t, c, model); n != 0 {
		t.Errorf("tasks of %s = %d, want 0", model, n)
	}
	if n := modelTaskCount(t, c, other); n != 1 {
		t.Errorf("tasks of %s = %d, want 1: another model's tasks stay", other, n)
	}
}

func RunScheduledTask_ImportDropRemovesTasksAndRestoreReArms(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-import"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1)

	if err := c.ImportWorkflow(t, model, 1, hourScheduleDropped); err != nil {
		t.Fatalf("ImportWorkflow (drop): %v", err)
	}
	if n := len(entityTasks(t, c, ids[0])); n != 0 {
		t.Fatalf("tasks after the drop = %d, want 0", n)
	}

	if err := c.ImportWorkflow(t, model, 1, hourSchedule); err != nil {
		t.Fatalf("ImportWorkflow (restore): %v", err)
	}
	if n := len(entityTasks(t, c, ids[0])); n != 0 {
		t.Fatalf("tasks after the restore = %d, want 0: an import never arms", n)
	}
	if err := c.UpdateEntityData(t, ids[0], `{"k":1}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}
	got := entityTasks(t, c, ids[0])
	if len(got) != 1 || got[0].Transition != "AutoClose" || got[0].Status != "WAITING" || got[0].Attempts != 0 {
		t.Errorf("tasks after a write in the state = %+v, want one WAITING AutoClose task, attempts 0", got)
	}
}
```

- [ ] **Step 2: Prove the scenarios can fail, then run them for real**

The parity fixtures build the server in a subprocess the test cache cannot
see (CLAUDE.md, "Common Commands"). These runs use `-count=1`: this is the
one case where it is wanted.

1. Take out the single-delete removal. In `deleteEntityOnce`, delete the
   three lines of the `h.deleteEntityTasks(txCtx, []string{entityID})` call.
2. Run:
   ```
   go test -count=1 ./e2e/parity/memory/ -run 'TestParity/ScheduledTask_DeleteEntityRemovesItsTasks'
   ```
   Expected: FAIL, `tasks of the deleted entity = 1, want 0`.
3. Restore the file: `git checkout -- internal/domain/entity/service.go`.
4. Do the same with the `deleteModelTasks` call in `deleteAllEntitiesOnce`
   and `ScheduledTask_DeleteAllRemovesModelTasks`. Expected: FAIL. Restore.
5. Do the same with the `removeUnscheduledTasks` block in `handler.go` and
   `ScheduledTask_ImportDropRemovesTasksAndRestoreReArms`. Expected: FAIL
   `tasks after the drop = 1, want 0`. Restore with
   `git checkout -- internal/domain/workflow/handler.go`.
6. Then:
   ```
   go test -count=1 ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run 'TestParity/ScheduledTask_'
   ```
   Expected: `ok` on all three.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/scheduledtransition/task_writes.go
git commit -m "test(parity): scheduled tasks on delete, delete-all, conditional, batched and import

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Coverage matrix — this stream's rows (§13 "Entity writes and workflow import")

| Scenario | U | S | E | P | gRPC |
|---|---|---|---|---|---|
| FAILED task re-armed by an update in the state | — (T) | S | — (T) | — (T) | |
| FAILED task cancelled when the entity leaves the state | — (T) | | — (T) | — (T) | |
| a task whose transition is no longer scheduled is removed at the next write | — (E) | S | — (E/T) | — (E/T) | |
| a workflow import that drops schedules removes the model's tasks | W-7 `TestImport_RemovesTasksOfTransitionsNoLongerScheduled` | S | W-7 `…Import_DroppingAScheduleRemovesItsTasks` | W-8 `ImportDropRemovesTasksAndRestoreReArms` | |
| deleting one entity removes its tasks | W-2 `TestDeleteEntity_RemovesTheEntitysTasks` | S | W-2 `…DeleteEntity_RemovesItsTasks` | W-8 `DeleteEntityRemovesItsTasks` | W-2 |
| conditional delete removes tasks: single-tx | W-5 `…SingleTx_RemovesOnlyTheDeletedEntitiesTasks`, `…FailedIDKeepsItsTasks` | S | W-5 `…ConditionalDelete_RemovesTheDeletedEntitiesTasks` | W-8 `ConditionalDelete…` | W-5 (verbose) |
| — batched | W-6 `TestDeleteBatched_RemovesEachBatchsTasks` | S | W-6 (both batched tests assert removal) | W-8 `BatchedDeleteRemovesTasks` | W-6 |
| — fast path | W-4 `…FastPath_RemovesTheModelsTasks` | S | W-4 `…DeleteAll_RemovesModelTasks…` (no body = fast path) | W-8 `DeleteAllRemovesModelTasks` | W-4 |
| delete-all removes the model's tasks | W-4 `TestDeleteAllEntities_RemovesTheModelsTasks` | S | W-4 | W-8 | W-4 |
| `DeleteForModel` in tenant A leaves tenant B's tasks | | S | W-4 `…OtherTenantKept` | | |
| an update racing a claim → retryable 409 | W-3 (three tests) | S | W-3 `…UpdateRacingOneClaim_409ThenRetrySucceeds` | | W-3 |
| a delete racing one claim succeeds after the server retry | W-2/4/5/6 `…RetriedThenSucceeds` | S | W-2, W-4, W-5, W-6 `…RacingOneClaim…` | | |
| an import racing one claim succeeds after the server retry | W-7 `…RetriedThenSucceeds` | S | W-7 `…Import_RacingOneClaim…` | | |
| owned single-tx delete retried ×3; persistent → 409 | W-2, W-4, W-5 `…Persists_Retryable409` | | W-2, W-4, W-5 `…PersistentConflict_409` (attempts counted) | | W-2, W-4 |
| batched: batch retried ×3; persistent → per id, never 409 | W-6 `…BatchRetried`, `…ReportedPerIDNever409` | | W-6 `…PersistentConflict_PerIDNot409` | | W-6 |
| a delete in a joined transaction is not retried | W-2, W-4, W-5 `…Joined_NotRetried` | | W-2 `…DeleteEntity_Joined_NotRetried` | | |
| import: saved first; retried; persistent → 409; re-import succeeds | W-7 `…Persists_409_WorkflowsAlreadySaved_ReimportSucceeds` | | W-7 `…Import_PersistentConflict_409ThenReimportSucceeds` | | |
| gRPC entity doors: tasks removed on delete; 409 on a race | | | | | W-2 … W-6 |

Notes on the table:
- Concurrency cases stay out of P (`.claude/rules/test-coverage.md`).
- The FAILED rows need a FAILED task, and only a real run produces one, so
  they need streams E and R. Open point 5 hands them to T.
- The "next write" row is E's reconcile (Open point 5).
- **S cells are stream S's.** W's tests assume S covers:
  - `DeleteForEntities` / `DeleteForModel` on every backend, with `keep` and
    `keep == nil`;
  - the tenant filter;
  - C1 on these deletes.
- **gRPC waiver for the import.** The workflow import has no gRPC door.

## Stream interface summary

**Other streams may consume from W**

`internal/common`:
- `const TaskConflictRetries = 3`
- `func RetryOnTaskConflict(ctx context.Context, owned bool, op func() error) error`.
  - `owned=false` → op runs once.
  - A conflict is `errors.Is(err, spi.ErrConflict)`, including an
    `*AppError` whose cause is one.
  - The retries stop when ctx is done.

`internal/testing/taskconflict` (test support, `_test.go` importers only):
- `Method` with `DeleteForEntities`, `DeleteForModel`, `ReconcileForEntity`;
- `NewPlan`, `(*Plan).Refuse`, `Fail`, `Reset`, `Calls`;
- `Factory{spi.StoreFactory; Plan *Plan}`.
- Stream T, or anyone who needs a statement-level task conflict in a unit
  test, can add methods to it.

`internal/domain/entity`:
- Every delete path removes tasks in its own transaction:
  - single delete and the conditional loops → `DeleteForEntities` with the
    ids actually deleted;
  - delete-all / fast path → `DeleteForModel(…, nil)`.
- Owned paths retry. Joined paths do not.
- `deleteBatched` retries each batch and reports a persistent conflict per
  id.
- A first-committer-wins refusal of an entity-row delete fails the attempt
  instead of becoming a per-id error.
- An update whose reconcile hits a task-row conflict answers 409 CONFLICT
  (retryable) on every door.

`internal/domain/workflow`:
- `scheduledTransitions(wfs)`, built on E-2's `armsOnSchedule` (README C-P7);
- the import answers a retryable 409 when the task removal still conflicts.

e2e helpers (`internal/e2e`, stream T may reuse):
- `doAuthOnceRaw`;
- `holdTaskRows` / `awaitBlocked` / `claimAndCommit`;
- `installConflictTrigger` / `refusals` / `remove`;
- `taskRows`, `setupScheduledModel`, `schedWritesWorkflow`,
  `requireConflictProblem`.

OpenAPI:
- `deleteSingleEntity` 409 and `importEntityModelWorkflow` 409 are added;
- the `deleteEntities` 409 description now covers `CONFLICT`.
- Help: `errors.CONFLICT` is widened.

**W consumes**

- **S**:
  - `ScheduledTaskStore.DeleteForEntities(ctx, tenant, ids)`;
  - `DeleteForModel(ctx, tenant, name, version, keep)`, where `keep == nil`
    removes all (Open point 2);
  - `Query(ctx, tenant, ScheduledTaskQuery{EntityID | ModelName, ModelVersion, Limit})` → `Items`;
  - `ReconcileForEntity` with the new semantics.
  - All of these join the transaction on ctx, and a C1 refusal is
    `spi.ErrConflict` whether it comes from the statement or the commit.
- **BM / BQ / BP**: those methods, with the §10.2 table columns `status`,
  `claim_token`, `claim_owner`, `entity_id`, `model_name`, `tenant_id`,
  `transition` (the e2e helpers read and write them).
- **E**: `reconcileScheduledTasks` keeps marking its store error with
  `ErrScheduledTaskInfra` through `errors.Join`. W-3 depends on it.
- **Q-2**: `GET /scheduled-tasks` with the §8 response shape (W-8 only).

## Open points

1. **`retryOnTaskConflict` moved from `entity` to `common`.**
   - The binding in `interfaces.md` puts `taskConflictRetries` and
     `retryOnTaskConflict` in `internal/domain/entity`, unexported.
   - The workflow import needs the same retry, but `entity` imports
     `workflow` (`service.go:26`), so `workflow` cannot import `entity`.
   - Planned: `common.TaskConflictRetries` and `common.RetryOnTaskConflict`,
     one implementation for both callers.
   - The lead should update `interfaces.md`.
2. **`DeleteForModel` with `keep == nil`.** The spec writes "keep = none"
   (§7). W passes `nil` and needs `nil` to mean "remove every task of the
   model". Stream S should state that in the method's doc comment and give it
   a `spitest` case.
3. **Closed (README C-P7).** One arm rule, `armsOnSchedule`, lives in
   `arm.go` (E-2); arm, fire, `modelHasSchedule` and W-7's import `keep` all
   use it. W-7 runs after E and edits neither `arm.go` nor
   `fire_scheduled.go`.
4. **W-3 relies on `errors.Join(ErrScheduledTaskInfra, err)`** in
   `reconcileScheduledTasks` (`arm.go:177-179`). If E replaces that join,
   E must keep a marker that `errors.Is` can see next to `spi.ErrConflict`.
   Otherwise an update racing a claim goes back to answering 412 on
   PostgreSQL.
5. **Rows W does not own.**
   - "FAILED task re-armed by an update" (U, E, P) and "FAILED task
     cancelled when the entity leaves the state" (U, E, P) need a FAILED task.
     Only a real run of E and R produces one, and W lands before R
     (README wave 3 and wave 4). Proposed owner: **T**.
   - "A task whose transition is no longer scheduled is removed at the next
     write" is the new `ReconcileForEntity` ("remove every other task of
     the entity"). Proposed owners: **E** (U) and **T** (E, P).
   - README "Review Focus 2" names W-2 and W-3. Here they are the single
     delete (W-2) and the update 409 (W-3). The batched and import halves are
     W-6 and W-7.
6. **W-7's RED is a build failure.** `import_tasks_test.go` calls
   `scheduledTransitions`, which does not exist yet. The behavioural REDs of
   W-7 are the E2E failures in Step 2 (`Remind task rows = 1, want 0`; the
   persistent test gets 200). A reviewer who wants a behavioural unit RED
   can add an empty `scheduledTransitions` first. W-7 then fails with
   `sched-import tasks = [AutoClose Remind], want [AutoClose]`.
7. **The retry also covers an entity-row conflict.**
   - The store reports one `spi.ErrConflict` for any C1 refusal. An owned
     delete cannot tell a task-row conflict from a concurrent change to the
     entity, so the retry covers both.
   - Running a delete again in a new transaction is correct either way. The
     retry re-selects (conditional), re-counts (delete-all) or re-checks the
     version baseline (batched, where a changed entity then becomes
     `ENTITY_MODIFIED` for that id, as today).
   - The effect: a delete racing an entity update, which answers 409 today,
     now succeeds when a retry wins. The lead should confirm that is wanted.
     The spec's retry table speaks of task rows only.
8. **Batched fold widened.** When a batch fails, its message now also goes
   on the ids the batch never reached (W-6). Before, those ids were counted
   in `MatchedCount` but appeared in neither `IDToError` nor
   `RemovedCount`. This is a small change to what the response reports.
   `CHANGELOG` wording is stream D's.
9. **Trigger fixture and schema.** The e2e helpers assume the BP migration
   keeps the table name `scheduled_tasks` and the column names of §10.2, and
   puts no foreign key on `claim_owner`. `claimAndCommit` writes a random
   owner. If BP adds a foreign key to `scheduler_owners`, `claimAndCommit`
   must first insert that owner. It is a one-line change in the helper.
10. **The spec's `handler.go:369` citation is now `:373`**
    (`internal/domain/workflow/handler.go`). Stream D can correct it if the
    spec is touched again.
