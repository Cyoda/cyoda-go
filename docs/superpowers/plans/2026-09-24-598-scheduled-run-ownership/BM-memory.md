# Stream BM — memory store

Spec sections: §10.1 (the contract), §10.3 (memory and SQLite), §5.2
(`RemoveLife`, `StampSegment`), §5.5 (the mark), §5.6 (`RecordAttempt`,
`ErrStoreRejected`), §6.1 (claiming), §6.2 (liveness). §13 rows with an **S**
tick run on this backend through `spitest` (stream S).

Needs: stream S merged on its SPI branch, and the worktree's `go.work` `use`
line pointing at it. Until then `plugins/memory` does not build against the new
interface.

## Facts settled by reading the code

1. **Rows today.** `StoreFactory.scheduledTasks` is `map[string]spi.ScheduledTask`,
   keyed by task id alone (`plugins/memory/store_factory.go:135-140`, `:169`),
   guarded by `entityMu`.
2. **Staging today.** A joining write is buffered as `scheduledTaskOp{kind, id,
   task}` (`scheduled_task_store.go:11-29`) in
   `TransactionManager.scheduledTaskOps[txID]` (`txmanager.go:162-174`,
   `:278-287`). `Commit` captures the ops at `:522` and applies them at step 5.5
   (`:783-785`), after the entity flush of step 4 (`:601-727`). Savepoints
   truncate the slice (`:1100-1102`); every abort path deletes it (`:514`,
   `:548`, `:817`, `:886`).
3. **Reads today ignore staged ops.** `Get` (`scheduled_task_store.go:116-125`)
   and `ReconcileForEntity`'s scan (`:163-174`) read committed state only.
   `Delete` reports existence against committed state (`:97-114`).
4. **`stage` does not check `tx.Closed`** (`scheduled_task_store.go:75-91`),
   while `EntityStore.Save` does (`entity_store.go:251-252`). A staged op after
   commit lands in `scheduledTaskOps[txID]` after its cleanup and is never
   freed. Fixed in BM-1.
5. **The conflict check already orders by a sequence number.**
   `committedTx.seq` (`txmanager.go:28-33`), `commitSeq` and `txSnapshotSeq`
   (`:89-105`); Begin captures `txSnapshotSeq` under `mu` (`:381-392`); the
   check is `committed.seq > snapshotSeq` over `committed.writeSet`, a set of
   entity ids (`:502-520`). Step 6 assigns the seq and appends (`:794-800`),
   then prunes by `submitTime` against the oldest open snapshot (`:821-838`).
6. **Lock order.** `tx.OpMu` → `entityMu` → `mu` (`Commit`, `:473`, `:485`,
   `:497`). `mu` is a leaf; `nextSubmitTime` takes it (`:301-310`).
7. **Store clock.** `StoreFactory.clock` (`store_factory.go:65`), injected with
   `WithClock` (`:18-20`); the conformance harness passes a `TestClock`
   (`conformance_test.go:14-19`).
8. **Epoch-claim precedent.** `AsyncSearchStore.ClaimStale` sorts candidates
   before cutting at the limit so a capped claim is reproducible
   (`search_store.go:347-425`). `ClaimDue` follows it.

## V1, resolved for memory

**The entity check is not disturbed.** Task rows get their own key type,
`taskKey{tenant, id}`, and their own field on the log entry,
`committedTx.taskWrites map[taskKey]bool`. The existing loop over
`committed.writeSet` (entity ids) is kept as it is; a second loop compares
`committed.taskWrites` with the committing transaction's task writes. An entity
id and a task id can never be confused, because they live in different sets.

**Every check is evaluated before anything is applied.**

1. *At staging.* A joining write (`ReconcileForEntity`, `RemoveLife`,
   `StampSegment`, `DeleteForEntities`, `DeleteForModel`, `Fail`) runs its check
   inside `scheduledTaskStore.write`, while it holds `tx.OpMu` (read) and
   `entityMu` (read). It sees a `taskView`: the committed rows, then the
   transaction's earlier staged ops in order. The check decides, and the write
   stages the row's **post-image** (or a removal, or a touch). A fenced write
   whose tokens do not match returns `ErrStaleClaim` there, and stages nothing.
2. *At commit, step 3, before step 4 flushes any entity.* The conflict check
   refuses the commit if any log entry with `seq > txSnapshotSeq[txID]` wrote
   one of the transaction's task rows. Every task-row write reaches the log:
   a transaction's at its step 6, and every write that commits on its own
   through `commitTaskWrites` (BM-3), in the same `mu` section that applies it.
3. *So step 5.5 evaluates nothing.* If step 3 passed, no writer changed those
   rows since Begin. The rows staging read are therefore the rows commit
   applies to, and the post-images are exact. `applyTaskOps` cannot fail, and
   nothing after step 4 can abort.

Worked example, the run's final transaction:

| Step | Committed row `e1:S:T` | Staged ops of run tx R |
|---|---|---|
| ClaimDue (own write, seq 7) | RUNNING, claim c1 | — |
| R Begins (`txSnapshotSeq` = 7) | same | — |
| R: `StampSegment(ref c1)` — view = committed; tokens match | same | post-image {RUNNING, c1, Partial} |
| R: `RemoveLife(arm a1)` — view = committed + stamp; life a1 | same | + removal |
| R Commits: step 3 finds no entry > 7 with `e1:S:T` → step 5.5 applies stamp, then removal | removed | — |

If a client re-armed `e1:S:T` between Begin and Commit, its commit is entry 8
with `e1:S:T` in `taskWrites`, and R fails at step 3 with `ErrConflict` before
anything is applied.

A staging read sees the latest committed row, not the snapshot. A row changed
after Begin is therefore visible at staging, but any write to it then fails at
commit, which is what C1 requires.

## Working rules for this stream

- Every command runs from the plugin module:
  `cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory`.
  The plugin is its own Go module; the root's `./...` does not reach it.
- The SPI comes from the local `go.work` `use` line. Never stage `go.work`.
  Before each commit, `git diff --cached --name-only` must not list it.
- New code follows the Go conventions of the README: `defer Unlock()` on the
  line after each `Lock()`, errors wrapped as `failed to X: %w`, `uuid.UUID`
  for tokens, no test hooks in production code.
- No token (arm, claim) appears in an error message or a log line. A task id
  may: it is a hash.
- Between BM-1 and BM-2 the never-joining methods answer
  `errors.ErrUnsupported`. BM-2 removes that and its exit check proves it.
  `TestConformance` (the whole `spitest` suite) is run only in BM-7.

---

### Task BM-1: Rows per life, staged post-images, joining writes and reads

**Spec:** §10.1 table rows `ReconcileForEntity`, `RemoveLife`, `StampSegment`,
`DeleteForEntities`, `DeleteForModel`, `Get`, `Query`, `Fail`; C2; §10.3
"Staged removals are expanded to task ids when they are staged"; §7 "Arm".

**Files:**
- Replace: `plugins/memory/scheduled_task_store.go` (all of `:1-196`)
- Create: `plugins/memory/scheduled_task_claims.go` (interim; BM-2 replaces it)
- Modify: `plugins/memory/txmanager.go` (`stageScheduledTaskOp` `:278-287`;
  step 5.5 `:783-785`; field doc `:162-174`)
- Modify: `plugins/memory/store_factory.go` (fields `:135-140`, init `:169`,
  accessor `:242-248`, imports)
- Replace: `plugins/memory/scheduled_task_store_test.go` (all of it)

**Interfaces:**
- Consumes (stream S): `spi.ScheduledTaskStore`, `spi.ScheduledTask` with the
  new fields, `spi.ScheduledTaskStatus` constants, `spi.TaskRef`,
  `spi.ScheduledTaskQuery`, `spi.ScheduledTaskPage`, `spi.ScheduledTaskCursor`,
  `spi.Failure`, `spi.ErrStaleClaim`.
- Produces (package-internal, used by BM-2…BM-5): `taskKey`, `markKey`,
  `scheduledTaskOp{key, after, touch}`, `applyTaskOps`, `taskView`,
  `(*StoreFactory).withMarkLocked`, `(*scheduledTaskStore).write`, `fenced`,
  `newLife`, `copyScheduledTask`, `(*TransactionManager).stagedTaskOps`,
  `stageTaskOps`, `commitTaskWrites`; factory maps `scheduledTasks`,
  `taskMarks`, `schedulerOwners`.

- [ ] **Step 1: Write the failing tests**

Replace `plugins/memory/scheduled_task_store_test.go` with:

```go
package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const (
	taskTenantA spi.TenantID = "tenant-A"
	taskTenantB spi.TenantID = "tenant-B"
)

type taskFixture struct {
	f     *memory.StoreFactory
	clock *memory.TestClock
	sts   spi.ScheduledTaskStore
	tm    spi.TransactionManager
}

// newTaskFixture returns a factory on a frozen clock: nothing advances it
// unless the test does.
func newTaskFixture(t *testing.T) taskFixture {
	t.Helper()
	clock := memory.NewTestClockAt(time.UnixMilli(1_000_000))
	f := memory.NewStoreFactory(memory.WithClock(clock))
	t.Cleanup(func() { _ = f.Close() })
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return taskFixture{f: f, clock: clock, sts: sts, tm: tm}
}

func (fx taskFixture) begin(t *testing.T, tenant spi.TenantID) (string, context.Context) {
	t.Helper()
	txID, txCtx, err := fx.tm.Begin(ctxWithTenant(tenant))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return txID, txCtx
}

func (fx taskFixture) commit(tenant spi.TenantID, txID string) error {
	return fx.tm.Commit(ctxWithTenant(tenant), txID)
}

func (fx taskFixture) rollback(t *testing.T, tenant spi.TenantID, txID string) {
	t.Helper()
	if err := fx.tm.Rollback(ctxWithTenant(tenant), txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

// armTask is the arm request for entity's transition out of state S, due at
// 1 000 ms.
func armTask(tenant spi.TenantID, entity, transition string) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID:            entity + ":S:" + transition,
		TenantID:      tenant,
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: 1_000,
		EntityID:      entity,
		ModelName:     "M",
		ModelVersion:  1,
		Transition:    transition,
		SourceState:   "S",
		ArmedAt:       500,
	}
}

// arm arms the given transitions of entity as one ReconcileForEntity on ctx.
func arm(t *testing.T, ctx context.Context, sts spi.ScheduledTaskStore, tenant spi.TenantID, entity string, transitions ...string) []spi.ScheduledTask {
	t.Helper()
	req := spi.ReconcileRequest{TenantID: tenant, EntityID: entity, CurrentState: "S"}
	for _, tr := range transitions {
		req.Arm = append(req.Arm, armTask(tenant, entity, tr))
	}
	removed, err := sts.ReconcileForEntity(ctx, req)
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	return removed
}

func getTask(t *testing.T, ctx context.Context, sts spi.ScheduledTaskStore, tenant spi.TenantID, id string) (spi.ScheduledTask, bool) {
	t.Helper()
	got, found, err := sts.Get(ctx, tenant, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		return spi.ScheduledTask{}, false
	}
	return *got, true
}

func TestTasks_ArmStartsANewLife(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()

	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	first, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if !ok {
		t.Fatal("armed task not found")
	}
	if first.Status != spi.ScheduledTaskWaiting || first.ArmToken == uuid.Nil ||
		first.NextAttemptTime != first.ScheduledTime || first.Claim != nil || first.Attempts != 0 {
		t.Fatalf("armed task = %+v, want WAITING, a drawn arm token, NextAttemptTime = ScheduledTime, no claim, no attempts", first)
	}

	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	second, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if second.ArmToken == first.ArmToken {
		t.Fatal("a re-arm kept the arm token; want a new life")
	}
}

func TestTasks_ReconcileRemovesEveryOtherTaskOfTheEntity(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()

	arm(t, bg, fx.sts, taskTenantA, "e1", "T1", "T2")
	removed := arm(t, bg, fx.sts, taskTenantA, "e1", "T1")
	if len(removed) != 1 || removed[0].ID != "e1:S:T2" {
		t.Fatalf("removed = %+v, want only e1:S:T2", removed)
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T2"); ok {
		t.Fatal("e1:S:T2 is still stored")
	}
}

func TestTasks_StagedArmIsDiscardedOnRollback(t *testing.T) {
	fx := newTaskFixture(t)
	txID, txCtx := fx.begin(t, taskTenantA)
	arm(t, txCtx, fx.sts, taskTenantA, "e1", "T")
	fx.rollback(t, taskTenantA, txID)

	if _, ok := getTask(t, context.Background(), fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("an arm staged in a rolled-back transaction is stored")
	}
}

func TestTasks_SavepointTruncatesStagedOps(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	arm(t, txCtx, fx.sts, taskTenantA, "e1", "T")
	spID, err := fx.tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	arm(t, txCtx, fx.sts, taskTenantA, "e2", "T")
	if err := fx.tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	bg := context.Background()
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !ok {
		t.Fatal("the arm staged before the savepoint was not committed")
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e2:S:T"); ok {
		t.Fatal("the arm staged after the savepoint survived RollbackToSavepoint")
	}
}

func TestTasks_JoiningGetSeesStagedOps(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	txID, txCtx := fx.begin(t, taskTenantA)

	arm(t, txCtx, fx.sts, taskTenantA, "e1", "T")
	staged, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T")
	if !ok {
		t.Fatal("a joining Get did not see the transaction's own staged arm")
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("a non-joining Get saw an uncommitted arm")
	}

	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", staged.ArmToken); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if _, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("a joining Get still sees a task the transaction removed")
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("the removed task is stored after commit")
	}
}

// A removal is expanded to the task ids it covers when it is staged. A task
// armed afterwards, by another writer, is not removed by it.
func TestTasks_RemovalsAreExpandedWhenStaged(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.DeleteForModel(txCtx, taskTenantA, "M", 1, nil); err != nil {
		t.Fatalf("DeleteForModel: %v", err)
	}
	arm(t, bg, fx.sts, taskTenantA, "e2", "T")
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("e1's task survived DeleteForModel")
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e2:S:T"); !ok {
		t.Fatal("DeleteForModel removed a task armed after it was staged")
	}
}

func TestTasks_StagingIntoACommittedTransactionIsRefused(t *testing.T) {
	fx := newTaskFixture(t)
	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
		Arm: []spi.ScheduledTask{armTask(taskTenantA, "e1", "T")},
	})
	if !errors.Is(err, spi.ErrTxAlreadyCommitted) {
		t.Fatalf("err = %v, want ErrTxAlreadyCommitted", err)
	}
}

func TestTasks_AWriteForAnotherTenantThanTheTransactionIsRefused(t *testing.T) {
	fx := newTaskFixture(t)
	_, txCtx := fx.begin(t, taskTenantA)
	_, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenantB, EntityID: "e1", CurrentState: "S",
		Arm: []spi.ScheduledTask{armTask(taskTenantB, "e1", "T")},
	})
	if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("err = %v, want ErrTxTenantMismatch", err)
	}
}

func TestTasks_GetIsTenantScoped(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	if _, ok := getTask(t, bg, fx.sts, taskTenantB, "e1:S:T"); ok {
		t.Fatal("tenant B read tenant A's task by its id")
	}
}

// The request names the tenant and the entity; the arm task's own fields for
// them are ignored.
func TestTasks_ArmTakesTenantAndEntityFromTheRequest(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	stray := armTask(taskTenantB, "other", "T")
	stray.ID = "e1:S:T"
	if _, err := fx.sts.ReconcileForEntity(bg, spi.ReconcileRequest{
		TenantID: taskTenantA, EntityID: "e1", CurrentState: "S", Arm: []spi.ScheduledTask{stray},
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	got, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if !ok || got.TenantID != taskTenantA || got.EntityID != "e1" {
		t.Fatalf("armed task = %+v, %v; want it under tenant A and entity e1", got, ok)
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantB, "e1:S:T"); ok {
		t.Fatal("the arm task's own tenant was used")
	}
}

// Query orders ids byte-wise: "B" (0x42) < "a" (0x61) < "é" (0xC3 0xA9).
// A case-insensitive or Unicode collation would give a, B, é.
func TestTasks_QueryOrdersIdsByteWise(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	for _, e := range []string{"a", "é", "B"} {
		arm(t, bg, fx.sts, taskTenantA, e, "T")
	}
	first, err := fx.sts.Query(bg, taskTenantA, spi.ScheduledTaskQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(first.Items) != 2 || first.Items[0].ID != "B:S:T" || first.Items[1].ID != "a:S:T" || first.Next == nil {
		t.Fatalf("first page = %+v, want B:S:T, a:S:T and a cursor", first)
	}
	second, err := fx.sts.Query(bg, taskTenantA, spi.ScheduledTaskQuery{Limit: 2, After: first.Next})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].ID != "é:S:T" || second.Next != nil {
		t.Fatalf("second page = %+v, want only é:S:T and no cursor", second)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_'
```

Expected: `FAIL github.com/cyoda-platform/cyoda-go/plugins/memory [build failed]`,
with errors such as `*scheduledTaskStore does not implement
spi.ScheduledTaskStore (missing method ClaimDue)` and `t.RedispatchAfter
undefined`.

- [ ] **Step 3: Write the implementation**

Replace `plugins/memory/scheduled_task_store.go` with:

```go
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
```

Create `plugins/memory/scheduled_task_claims.go`:

```go
package memory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Not implemented yet: each of these answers errors.ErrUnsupported.

func (s *scheduledTaskStore) ClaimDue(context.Context, spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	return nil, errors.ErrUnsupported
}

func (s *scheduledTaskStore) Heartbeat(context.Context, uuid.UUID) error { return errors.ErrUnsupported }

func (s *scheduledTaskStore) RetireOwner(context.Context, uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) SweepOwners(context.Context, time.Duration) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) GiveBackIdle(context.Context, uuid.UUID, []uuid.UUID) (int, error) {
	return 0, errors.ErrUnsupported
}

func (s *scheduledTaskStore) MarkUnsafe(context.Context, spi.TaskRef) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) RecordAttempt(context.Context, spi.TaskRef, spi.Attempt) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) SweepMarks(context.Context) error { return errors.ErrUnsupported }
```

In `plugins/memory/txmanager.go`, replace `stageScheduledTaskOp` (`:278-287`)
with:

```go
// stagedTaskOps returns a copy of the task-row ops staged for txID, in order.
// Protected by mu.
func (m *TransactionManager) stagedTaskOps(txID string) []scheduledTaskOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]scheduledTaskOp(nil), m.scheduledTaskOps[txID]...)
}

// stageTaskOps appends ops to txID's staged task-row ops. Commit applies them
// in its entityMu section, atomically with the entity flush; every abort
// path discards them. Protected by mu.
func (m *TransactionManager) stageTaskOps(txID string, ops []scheduledTaskOp) {
	if len(ops) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduledTaskOps[txID] = append(m.scheduledTaskOps[txID], ops...)
}

// commitTaskWrites applies task-row writes that commit on their own: a
// never-joining method, or a joining one called without a transaction.
// Caller holds factory.entityMu for writing; lock order entityMu → mu.
func (m *TransactionManager) commitTaskWrites(ops []scheduledTaskOp) {
	if len(ops) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	applyTaskOps(m.factory.scheduledTasks, ops)
}
```

Replace step 5.5 (`:783-785`) with:

```go
		// 5.5. Apply the staged task-row post-images. Their checks ran when
		// they were staged, and step 3 proved that no other writer changed
		// those rows since this transaction began, so nothing is evaluated
		// here and nothing can fail after the entity flush.
		applyTaskOps(m.factory.scheduledTasks, capturedScheduledTaskOps)
```

Rewrite the `scheduledTaskOps` field doc (`:162-174`) to: "holds the task-row
ops staged while the transaction is open, as post-images (see
`scheduledTaskOp`). Applied by Commit inside its `entityMu` section; discarded
on Rollback and on every abort path; truncated by `RollbackToSavepoint`.
Protected by mu."

In `plugins/memory/store_factory.go`, add `"time"` and
`"github.com/google/uuid"` to the imports, and replace the `scheduledTasks`
field (`:135-140`) with:

```go
	// scheduledTasks holds the task rows, keyed by (tenant, id). taskMarks
	// holds the unsafe marks, at most one per life, each naming the claim that
	// wrote it. schedulerOwners holds each owner's last heartbeat on the store
	// clock. All three are guarded by entityMu — the mutex of the entity data —
	// so a transaction's staged task writes apply in the same critical section
	// as its entity flush, and every never-joining method is serialised with
	// every commit.
	scheduledTasks  map[taskKey]spi.ScheduledTask
	taskMarks       map[markKey]uuid.UUID
	schedulerOwners map[uuid.UUID]time.Time
```

Replace `scheduledTasks: make(map[string]spi.ScheduledTask),` (`:169`) with:

```go
		scheduledTasks:  make(map[taskKey]spi.ScheduledTask),
		taskMarks:       make(map[markKey]uuid.UUID),
		schedulerOwners: make(map[uuid.UUID]time.Time),
```

Replace the accessor comment (`:242-245`) with:

```go
// ScheduledTaskStore returns the scheduled-task store. No tenant is resolved
// from ctx: every tenant-facing method takes its tenant as an argument, and
// ClaimDue, GiveBackIdle and the owner and sweep methods are cross-tenant.
```

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -skip 'TestConformance'
```

Expected: `ok  github.com/cyoda-platform/cyoda-go/plugins/memory`. The
existing transaction and savepoint tests run too and stay green.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/memory/scheduled_task_store.go plugins/memory/scheduled_task_claims.go plugins/memory/txmanager.go plugins/memory/store_factory.go plugins/memory/scheduled_task_store_test.go && git commit -m "feat(memory): scheduled tasks as lives with staged post-images

Rows are keyed by tenant and id. Every arm draws a new life. A joining
write checks against the committed rows plus the transaction's staged
ops and stages the row's post-image; a joining Get sees them. Removals
are expanded to task ids when staged. Staging into a committed or
another tenant's transaction is refused.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BM-2: Never-joining methods — owners, claims, marks, attempts, sweeps

**Spec:** §10.1 rows `ClaimDue`, `Heartbeat`, `RetireOwner`, `SweepOwners`,
`GiveBackIdle`, `MarkUnsafe`, `RecordAttempt`, `SweepMarks`; §6.1 (limits,
claimable tasks, one task per entity); §6.2 (liveness on the store clock);
§5.5 (mark results); §5.6 (`RecordAttempt`); §10.3 "never-joining methods apply
at once, under the store's lock: memory: `entityMu`"; C3.

**Files:**
- Replace: `plugins/memory/scheduled_task_claims.go`
- Create: `plugins/memory/scheduled_task_claims_test.go`
- Create: `plugins/memory/scheduled_task_sweep_internal_test.go`

**Interfaces:**
- Consumes: BM-1's `taskKey`, `markKey`, `taskView`, `fenced`,
  `commitTaskWrites`, `withMarkLocked`, `copyScheduledTask`; `spi.ClaimRequest`,
  `spi.Attempt`, `spi.TaskClaim`, `spi.ErrMarkedByAnotherClaim`.
- Produces: `selectClaims(cands []spi.ScheduledTask, req spi.ClaimRequest)
  []spi.ScheduledTask` (package-internal; the same function exists in BQ-2).

- [ ] **Step 1: Write the failing tests**

`plugins/memory/scheduled_task_claims_test.go`:

```go
package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func claimDue(t *testing.T, sts spi.ScheduledTaskStore, owner uuid.UUID, allowLost bool) []spi.ScheduledTask {
	t.Helper()
	got, err := sts.ClaimDue(context.Background(), spi.ClaimRequest{
		Owner: owner, NowMs: 2_000, StaleAfter: time.Minute,
		Limit: 100, PerTenantLimit: 100, AllowLostOwner: allowLost,
	})
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	return got
}

func refOf(t spi.ScheduledTask) spi.TaskRef {
	return spi.TaskRef{TenantID: t.TenantID, ID: t.ID, ArmToken: t.ArmToken, ClaimToken: t.Claim.Token}
}

func TestTasks_ClaimTakesOneTaskPerEntity(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T1", "T2")
	arm(t, bg, fx.sts, taskTenantA, "e2", "T1")

	owner := uuid.New()
	first := claimDue(t, fx.sts, owner, false)
	if len(first) != 2 || first[0].EntityID == first[1].EntityID {
		t.Fatalf("claimed %+v, want one task of e1 and one of e2", first)
	}
	for _, c := range first {
		if c.Status != spi.ScheduledTaskRunning || c.Claim == nil || c.Claim.Owner != owner || c.Claim.Token == uuid.Nil {
			t.Fatalf("claimed task = %+v, want RUNNING under %s with a drawn claim token", c, owner)
		}
	}
	if again := claimDue(t, fx.sts, owner, false); len(again) != 0 {
		t.Fatalf("claimed %+v while e1 has a RUNNING task, want nothing", again)
	}
}

func TestTasks_ClaimHonoursTenantLimitsAndTurns(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	for _, e := range []string{"a1", "a2", "a3"} {
		arm(t, bg, fx.sts, taskTenantA, e, "T")
	}
	arm(t, bg, fx.sts, taskTenantB, "b1", "T")

	got, err := fx.sts.ClaimDue(bg, spi.ClaimRequest{
		Owner: uuid.New(), NowMs: 2_000, StaleAfter: time.Minute,
		Limit: 2, PerTenantLimit: 5,
	})
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(got) != 2 || got[0].TenantID == got[1].TenantID {
		t.Fatalf("claimed %+v, want one task of each tenant: tenants take turns", got)
	}

	got, err = fx.sts.ClaimDue(bg, spi.ClaimRequest{
		Owner: uuid.New(), NowMs: 2_000, StaleAfter: time.Minute,
		Limit: 10, PerTenantLimit: 2, TenantInProgress: map[spi.TenantID]int{taskTenantA: 1},
	})
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(got) != 1 || got[0].TenantID != taskTenantA {
		t.Fatalf("claimed %+v, want exactly one more task of tenant A (limit 2, 1 in progress)", got)
	}
}

func TestTasks_ClaimRejectsALimitBelowOne(t *testing.T) {
	fx := newTaskFixture(t)
	_, err := fx.sts.ClaimDue(context.Background(), spi.ClaimRequest{Owner: uuid.New(), NowMs: 2_000, Limit: 0, PerTenantLimit: 1})
	if err == nil {
		t.Fatal("ClaimDue accepted Limit 0")
	}
}

func TestTasks_LostOwnerClaimUsesTheStoreClock(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")

	first := uuid.New()
	if err := fx.sts.Heartbeat(bg, first); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	claimed := claimDue(t, fx.sts, first, false)[0]

	second := uuid.New()
	if got := claimDue(t, fx.sts, second, true); len(got) != 0 {
		t.Fatalf("claimed %+v from an owner that is not stale", got)
	}
	fx.clock.Advance(2 * time.Minute)
	if got := claimDue(t, fx.sts, second, false); len(got) != 0 {
		t.Fatalf("claimed %+v from a stale owner without AllowLostOwner", got)
	}
	got := claimDue(t, fx.sts, second, true)
	if len(got) != 1 || got[0].LostOwners != 1 || got[0].Claim.Owner != second || got[0].Claim.Token == claimed.Claim.Token {
		t.Fatalf("reclaim = %+v, want one task under the new owner, lostOwners 1, a new claim token", got)
	}
}

func TestTasks_ARetiredOwnerIsLostAtOnce(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	owner := uuid.New()
	if err := fx.sts.Heartbeat(bg, owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	claimDue(t, fx.sts, owner, false)
	if err := fx.sts.RetireOwner(bg, owner); err != nil {
		t.Fatalf("RetireOwner: %v", err)
	}
	if got := claimDue(t, fx.sts, uuid.New(), true); len(got) != 1 {
		t.Fatalf("claimed %d tasks, want the retired owner's task", len(got))
	}
}

func TestTasks_MarkUnsafe(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	first := claimDue(t, fx.sts, uuid.New(), false)[0]

	for i := 0; i < 2; i++ {
		if err := fx.sts.MarkUnsafe(bg, refOf(first)); err != nil {
			t.Fatalf("MarkUnsafe #%d for the same claim: %v", i+1, err)
		}
	}

	fx.clock.Advance(2 * time.Minute) // the first owner never heartbeated: missing is stale
	second := claimDue(t, fx.sts, uuid.New(), true)[0]
	if !second.UnsafeMarked {
		t.Fatal("the reclaimed task does not report the mark of its life")
	}
	if err := fx.sts.MarkUnsafe(bg, refOf(second)); !errors.Is(err, spi.ErrMarkedByAnotherClaim) {
		t.Fatalf("MarkUnsafe by the new claim = %v, want ErrMarkedByAnotherClaim", err)
	}
	if err := fx.sts.MarkUnsafe(bg, refOf(first)); !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("MarkUnsafe by the old claim = %v, want ErrStaleClaim", err)
	}
}

// A never-joining method commits on its own: rolling back the transaction on
// its ctx does not undo it.
func TestTasks_NeverJoiningMethodsIgnoreTheTransaction(t *testing.T) {
	fx := newTaskFixture(t)
	arm(t, context.Background(), fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.MarkUnsafe(txCtx, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	fx.rollback(t, taskTenantA, txID)

	got, _ := getTask(t, context.Background(), fx.sts, taskTenantA, "e1:S:T")
	if !got.UnsafeMarked {
		t.Fatal("the mark did not survive the rollback of the transaction on ctx")
	}
}

func TestTasks_RecordAttempt(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]
	if err := fx.sts.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}

	if err := fx.sts.RecordAttempt(bg, refOf(c), spi.Attempt{
		Error: "boom", AtMs: 2_000, NextAttemptTime: 5_000, ClearOwnMark: true,
	}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskWaiting || got.Attempts != 1 || got.LastError != "boom" ||
		got.LastAttemptTime == nil || *got.LastAttemptTime != 2_000 || got.NextAttemptTime != 5_000 ||
		got.Claim != nil || got.UnsafeMarked {
		t.Fatalf("after RecordAttempt: %+v", got)
	}
	if err := fx.sts.RecordAttempt(bg, refOf(c), spi.Attempt{AtMs: 2_000, NextAttemptTime: 5_000}); !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("a second RecordAttempt for the same claim = %v, want ErrStaleClaim", err)
	}

	fx.clock.Advance(time.Second)
	c2, err := fx.sts.ClaimDue(bg, spi.ClaimRequest{Owner: uuid.New(), NowMs: 5_000, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: 1})
	if err != nil || len(c2) != 1 {
		t.Fatalf("ClaimDue at 5000: %v, %d tasks", err, len(c2))
	}
	if err := fx.sts.RecordAttempt(bg, refOf(c2[0]), spi.Attempt{Error: "cut", AtMs: 5_000, NextAttemptTime: 5_000, NotCounted: true}); err != nil {
		t.Fatalf("RecordAttempt NotCounted: %v", err)
	}
	got, _ = getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if got.Attempts != 1 || got.LastError != "cut" || got.LastAttemptTime == nil || *got.LastAttemptTime != 5_000 {
		t.Fatalf("after a NotCounted attempt: %+v; want attempts 1, and the error and time recorded", got)
	}
}

// Fail always overwrites LastError, with an empty text too.
func TestTasks_FailOverwritesLastError(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]
	if err := fx.sts.RecordAttempt(bg, refOf(c), spi.Attempt{Error: "first", AtMs: 2_000, NextAttemptTime: 2_000}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	c = claimDue(t, fx.sts, uuid.New(), false)[0]
	if err := fx.sts.Fail(bg, refOf(c), spi.Failure{Reason: spi.FailureOwnerLostRepeatedly, AtMs: 3_000}); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskFailed || got.FailureReason != spi.FailureOwnerLostRepeatedly ||
		got.LastError != "" || got.FailedTime == nil || *got.FailedTime != 3_000 || got.Claim != nil {
		t.Fatalf("after Fail: %+v; want FAILED, the reason, LastError overwritten to empty, FailedTime 3000, no claim", got)
	}
}

func TestTasks_GiveBackIdleKeepsLiveClaims(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	arm(t, bg, fx.sts, taskTenantA, "e2", "T")
	owner := uuid.New()
	claimed := claimDue(t, fx.sts, owner, false)
	keep := claimed[0]

	n, err := fx.sts.GiveBackIdle(bg, owner, []uuid.UUID{keep.Claim.Token})
	if err != nil || n != 1 {
		t.Fatalf("GiveBackIdle = %d, %v; want 1, nil", n, err)
	}
	kept, _ := getTask(t, bg, fx.sts, taskTenantA, keep.ID)
	given, _ := getTask(t, bg, fx.sts, taskTenantA, claimed[1].ID)
	if kept.Status != spi.ScheduledTaskRunning {
		t.Fatalf("kept task = %+v, want RUNNING", kept)
	}
	if given.Status != spi.ScheduledTaskWaiting || given.Claim != nil || given.Attempts != 0 {
		t.Fatalf("given-back task = %+v, want WAITING, no claim, not counted", given)
	}
}
```

`plugins/memory/scheduled_task_sweep_internal_test.go`:

```go
package memory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestSweeps_RemoveEndedLivesAndDeadUnusedOwners(t *testing.T) {
	clock := NewTestClockAt(time.UnixMilli(1_000_000))
	f := NewStoreFactory(WithClock(clock))
	t.Cleanup(func() { _ = f.Close() })
	sts := &scheduledTaskStore{f: f}
	bg := context.Background()
	task := spi.ScheduledTask{
		ID: "e1:S:T", TenantID: "tenant-A", Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: 1_000, EntityID: "e1", ModelName: "M", ModelVersion: 1,
		Transition: "T", SourceState: "S",
	}
	armReq := spi.ReconcileRequest{TenantID: "tenant-A", EntityID: "e1", CurrentState: "S", Arm: []spi.ScheduledTask{task}}
	if _, err := sts.ReconcileForEntity(bg, armReq); err != nil {
		t.Fatalf("arm: %v", err)
	}

	busyOwner, idleOwner := uuid.New(), uuid.New()
	for _, o := range []uuid.UUID{busyOwner, idleOwner} {
		if err := sts.Heartbeat(bg, o); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	claimed, err := sts.ClaimDue(bg, spi.ClaimRequest{Owner: busyOwner, NowMs: 2_000, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: 1})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: %v, %d", err, len(claimed))
	}
	c := claimed[0]
	if err := sts.MarkUnsafe(bg, spi.TaskRef{TenantID: c.TenantID, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: c.Claim.Token}); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}

	if err := sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if len(f.taskMarks) != 1 {
		t.Fatalf("SweepMarks removed the mark of a live life")
	}
	if _, err := sts.ReconcileForEntity(bg, armReq); err != nil { // a new life ends the marked one
		t.Fatalf("re-arm: %v", err)
	}
	if err := sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if len(f.taskMarks) != 0 {
		t.Fatalf("SweepMarks kept %d marks of ended lives", len(f.taskMarks))
	}

	// The re-arm left no RUNNING task; claim again so busyOwner is referenced.
	if _, err := sts.ClaimDue(bg, spi.ClaimRequest{Owner: busyOwner, NowMs: 2_000, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: 1}); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	clock.Advance(time.Hour)
	if err := sts.SweepOwners(bg, time.Minute); err != nil {
		t.Fatalf("SweepOwners: %v", err)
	}
	if _, ok := f.schedulerOwners[idleOwner]; ok {
		t.Fatal("SweepOwners kept a dead owner no task references")
	}
	if _, ok := f.schedulerOwners[busyOwner]; !ok {
		t.Fatal("SweepOwners removed an owner a RUNNING task references")
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_|TestSweeps_'
```

Expected: the new tests fail with `ClaimDue: unsupported operation` (or
`Heartbeat: unsupported operation`); the BM-1 tests still pass.

- [ ] **Step 3: Write the implementation**

Replace `plugins/memory/scheduled_task_claims.go` with:

```go
package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The methods in this file never join a transaction: each ignores any
// transaction on ctx and commits on its own, under entityMu. That is how a
// mark survives the rollback of the run's transaction. Holding entityMu also
// serialises MarkUnsafe with ClaimDue (C3) and every one of them with every
// commit.

func (s *scheduledTaskStore) Heartbeat(_ context.Context, owner uuid.UUID) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	s.f.schedulerOwners[owner] = s.f.clock.Now()
	return nil
}

func (s *scheduledTaskStore) RetireOwner(_ context.Context, owner uuid.UUID) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	delete(s.f.schedulerOwners, owner)
	return nil
}

// SweepOwners removes the liveness record of every owner that has not
// heartbeated for deadFor, once no RUNNING task references it.
func (s *scheduledTaskStore) SweepOwners(_ context.Context, deadFor time.Duration) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	cutoff := s.f.clock.Now().Add(-deadFor)
	referenced := make(map[uuid.UUID]bool)
	for _, t := range s.f.scheduledTasks {
		if t.Status == spi.ScheduledTaskRunning {
			referenced[t.Claim.Owner] = true
		}
	}
	for owner, beat := range s.f.schedulerOwners {
		if beat.Before(cutoff) && !referenced[owner] {
			delete(s.f.schedulerOwners, owner)
		}
	}
	return nil
}

// ownerStaleLocked reports whether owner's liveness record is missing or
// older than cutoff. Caller holds entityMu.
func (s *scheduledTaskStore) ownerStaleLocked(owner uuid.UUID, cutoff time.Time) bool {
	beat, ok := s.f.schedulerOwners[owner]
	return !ok || beat.Before(cutoff)
}

// ClaimDue claims due tasks for req.Owner. A WAITING task is due when its
// NextAttemptTime is at or before req.NowMs (the pnode clock). With
// AllowLostOwner, a RUNNING task whose owner is stale by the store clock is
// claimable too, and the claim adds 1 to its lostOwners. A task is never
// claimed while another task of its entity is RUNNING.
func (s *scheduledTaskStore) ClaimDue(_ context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if req.Limit < 1 || req.PerTenantLimit < 1 {
		return nil, fmt.Errorf("claim due scheduled tasks: limit and per-tenant limit must be >= 1, got %d and %d", req.Limit, req.PerTenantLimit)
	}
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()

	cutoff := s.f.clock.Now().Add(-req.StaleAfter)
	running := make(map[entityTenantKey]taskKey)
	for k, t := range s.f.scheduledTasks {
		if t.Status == spi.ScheduledTaskRunning {
			running[entityTenantKey{tenant: string(k.tenant), id: t.EntityID}] = k
		}
	}
	var cands []spi.ScheduledTask
	for k, t := range s.f.scheduledTasks {
		switch {
		case t.Status == spi.ScheduledTaskWaiting && t.NextAttemptTime <= req.NowMs:
		case req.AllowLostOwner && t.Status == spi.ScheduledTaskRunning && s.ownerStaleLocked(t.Claim.Owner, cutoff):
		default:
			continue
		}
		if r, ok := running[entityTenantKey{tenant: string(k.tenant), id: t.EntityID}]; ok && r != k {
			continue
		}
		cands = append(cands, t)
	}

	chosen := selectClaims(cands, req)
	ops := make([]scheduledTaskOp, 0, len(chosen))
	for _, c := range chosen {
		t := copyScheduledTask(c)
		if t.Status == spi.ScheduledTaskRunning {
			t.LostOwners++
		}
		t.Status = spi.ScheduledTaskRunning
		t.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: req.Owner}
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}, after: &t})
	}
	s.f.txManager.commitTaskWrites(ops)

	out := make([]spi.ScheduledTask, 0, len(ops))
	for _, op := range ops {
		out = append(out, s.f.withMarkLocked(*op.after))
	}
	return out, nil
}

// selectClaims picks the tasks one ClaimDue call takes from cands: one per
// entity, at most PerTenantLimit − TenantInProgress per tenant, at most Limit
// in all. Within a tenant the order is (NextAttemptTime, ID). Tenants take
// turns, one task per turn; the tenant with the earliest candidate goes
// first, ties broken by tenant id.
func selectClaims(cands []spi.ScheduledTask, req spi.ClaimRequest) []spi.ScheduledTask {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.NextAttemptTime != b.NextAttemptTime {
			return a.NextAttemptTime < b.NextAttemptTime
		}
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.ID < b.ID
	})
	var tenants []spi.TenantID
	queues := make(map[spi.TenantID][]spi.ScheduledTask)
	for _, c := range cands {
		if _, ok := queues[c.TenantID]; !ok {
			tenants = append(tenants, c.TenantID)
		}
		queues[c.TenantID] = append(queues[c.TenantID], c)
	}
	quota := make(map[spi.TenantID]int, len(tenants))
	for _, tn := range tenants {
		quota[tn] = req.PerTenantLimit - req.TenantInProgress[tn]
	}

	seen := make(map[entityTenantKey]bool)
	var out []spi.ScheduledTask
	for progress := true; progress && len(out) < req.Limit; {
		progress = false
		for _, tn := range tenants {
			if len(out) >= req.Limit {
				break
			}
			for quota[tn] > 0 && len(queues[tn]) > 0 {
				c := queues[tn][0]
				queues[tn] = queues[tn][1:]
				ek := entityTenantKey{tenant: string(tn), id: c.EntityID}
				if seen[ek] {
					continue
				}
				seen[ek] = true
				quota[tn]--
				out = append(out, c)
				progress = true
				break
			}
		}
	}
	return out
}

// GiveBackIdle returns to WAITING, uncounted, every task RUNNING under owner
// whose claim token is not in keep.
func (s *scheduledTaskStore) GiveBackIdle(_ context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error) {
	kept := make(map[uuid.UUID]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	var ops []scheduledTaskOp
	for k, t := range s.f.scheduledTasks {
		if t.Status != spi.ScheduledTaskRunning || t.Claim.Owner != owner || kept[t.Claim.Token] {
			continue
		}
		back := copyScheduledTask(t)
		back.Status = spi.ScheduledTaskWaiting
		back.Claim = nil
		ops = append(ops, scheduledTaskOp{key: k, after: &back})
	}
	s.f.txManager.commitTaskWrites(ops)
	return len(ops), nil
}

// MarkUnsafe writes the mark of ref's life, naming ref's claim. It is
// idempotent for the same claim.
func (s *scheduledTaskStore) MarkUnsafe(_ context.Context, ref spi.TaskRef) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	k := taskKey{tenant: ref.TenantID, id: ref.ID}
	if _, err := fenced(taskView{f: s.f}, ref); err != nil {
		return err
	}
	mk := markKey{task: k, arm: ref.ArmToken}
	if holder, ok := s.f.taskMarks[mk]; ok {
		if holder == ref.ClaimToken {
			return nil
		}
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrMarkedByAnotherClaim)
	}
	s.f.taskMarks[mk] = ref.ClaimToken
	return nil
}

// RecordAttempt ends ref's claim: the task goes back to WAITING with the
// attempt recorded. With ClearOwnMark it also removes the mark this claim
// wrote, in the same critical section.
func (s *scheduledTaskStore) RecordAttempt(_ context.Context, ref spi.TaskRef, a spi.Attempt) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	k := taskKey{tenant: ref.TenantID, id: ref.ID}
	t, err := fenced(taskView{f: s.f}, ref)
	if err != nil {
		return err
	}
	if !a.NotCounted {
		t.Attempts++
	}
	at := a.AtMs
	t.Status = spi.ScheduledTaskWaiting
	t.Claim = nil
	t.LastAttemptTime = &at
	t.LastError = a.Error
	t.NextAttemptTime = a.NextAttemptTime
	s.f.txManager.commitTaskWrites([]scheduledTaskOp{{key: k, after: &t}})
	if a.ClearOwnMark {
		mk := markKey{task: k, arm: ref.ArmToken}
		if s.f.taskMarks[mk] == ref.ClaimToken {
			delete(s.f.taskMarks, mk)
		}
	}
	return nil
}

// SweepMarks removes the marks of ended lives: those whose task is gone or
// has been re-armed.
func (s *scheduledTaskStore) SweepMarks(_ context.Context) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	for mk := range s.f.taskMarks {
		if t, ok := s.f.scheduledTasks[mk.task]; !ok || t.ArmToken != mk.arm {
			delete(s.f.taskMarks, mk)
		}
	}
	return nil
}
```

`t.Claim` is not nil-checked for a RUNNING row: the store sets `Claim` exactly
when it sets RUNNING (`ClaimDue`) and clears it on every exit from RUNNING.

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_|TestSweeps_'
```

Expected: `ok`. Exit check, must print nothing:

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && grep -n 'ErrUnsupported' plugins/memory/*.go
```

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/memory/scheduled_task_claims.go plugins/memory/scheduled_task_claims_test.go plugins/memory/scheduled_task_sweep_internal_test.go && git commit -m "feat(memory): claims, owner liveness, marks and attempts

ClaimDue takes one task per entity, honours the per-tenant limits with
tenants taking turns, and reclaims from owners stale by the store
clock. MarkUnsafe, RecordAttempt and GiveBackIdle commit on their own,
under entityMu, whatever transaction is on ctx.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BM-3: C1 — task rows under first-committer-wins

**Spec:** §10.1 C1, C5 (a C1 refusal is `spi.ErrConflict`); §10.3 C1 bullets;
§5.2 "Every commit writes the task row"; §13 rows "a reclaimed or re-armed task
makes the old run's commit fail (C1)", "SQLite: a run never conflicts with its
own claim under a frozen clock" (the memory side of the same property).

**Files:**
- Modify: `plugins/memory/txmanager.go` (`committedTx` `:28-33`; conflict check
  `:496-528`; step 6 `:787-839`; `commitTaskWrites` from BM-1)
- Create: `plugins/memory/scheduled_task_c1_test.go`

**Interfaces:**
- Consumes: BM-1 `scheduledTaskOp`, `taskKey`, `commitTaskWrites`.
- Produces: `committedTx.taskWrites`, `taskWriteSet(ops) map[taskKey]bool`,
  `(*TransactionManager).pruneCommittedLogLocked()`.

- [ ] **Step 1: Write the failing tests**

`plugins/memory/scheduled_task_c1_test.go`:

```go
package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestTasks_C1_AClaimAfterBeginFailsTheCommit(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 1 {
		t.Fatalf("claimed %d tasks, want 1", len(got))
	}
	if err := fx.sts.DeleteForEntities(txCtx, taskTenantA, []string{"e1"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit = %v, want ErrConflict: the row was claimed after the transaction began", err)
	}
	if got, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !ok || got.Status != spi.ScheduledTaskRunning {
		t.Fatalf("task = %+v, %v; want it still RUNNING", got, ok)
	}
}

func TestTasks_C1_TwoTransactionsWritingOneRow_TheSecondCommitConflicts(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	cur, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

	aID, aCtx := fx.begin(t, taskTenantA)
	bID, bCtx := fx.begin(t, taskTenantA)
	arm(t, aCtx, fx.sts, taskTenantA, "e1", "T")
	if err := fx.sts.RemoveLife(bCtx, taskTenantA, "e1:S:T", cur.ArmToken); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if err := fx.commit(taskTenantA, aID); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := fx.commit(taskTenantA, bID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("second Commit = %v, want ErrConflict", err)
	}
}

// RemoveLife of a life replaced after Begin changes nothing, yet the commit
// fails: the row was written after the transaction began.
func TestTasks_C1_RemoveLifeOfALifeReplacedAfterBeginConflicts(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	old, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

	txID, txCtx := fx.begin(t, taskTenantA)
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", old.ArmToken); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit = %v, want ErrConflict", err)
	}
}

func TestTasks_C1_RemoveLifeOfALifeReplacedBeforeBeginCommits(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	old, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", old.ArmToken); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit = %v, want nil", err)
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !ok {
		t.Fatal("RemoveLife of an old life removed the new one")
	}
}

// The clock never moves in this fixture. A run's own claim precedes its
// transaction's Begin, so the run's commit must not conflict with it.
func TestTasks_C1_ARunDoesNotConflictWithItsOwnClaimUnderAFrozenClock(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.StampSegment(txCtx, refOf(c), true); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit = %v, want nil", err)
	}
	if got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !got.PartialCommit {
		t.Fatal("the stamp did not set PartialCommit")
	}
}

func TestTasks_C1_WritesWithNoOpenTransactionLeaveNoLogEntries(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	owner := uuid.New()
	for i := 0; i < 50; i++ {
		claimDue(t, fx.sts, owner, false)
		if _, err := fx.sts.GiveBackIdle(bg, owner, nil); err != nil {
			t.Fatalf("GiveBackIdle: %v", err)
		}
	}
	tm := fx.f.GetTransactionManager().(*memory.TransactionManager)
	if n := tm.CommittedLogLen(); n != 0 {
		t.Fatalf("committed log holds %d entries with no transaction open, want 0", n)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_C1_'
```

Expected: `AClaimAfterBeginFailsTheCommit`, `TheSecondCommitConflicts` and
`RemoveLifeOfALifeReplacedAfterBeginConflicts` fail with `Commit = <nil>, want
ErrConflict`. The other three pass; they guard against a false conflict and a
log leak once C1 is in.

- [ ] **Step 3: Write the implementation**

In `plugins/memory/txmanager.go`, add the field to `committedTx` (`:28-33`)
and extend its doc comment with the last paragraph below:

```go
type committedTx struct {
	id         string
	submitTime time.Time
	seq        int64
	writeSet   map[string]bool
	// taskWrites holds the task rows the write changed. It is apart from
	// writeSet, whose keys are entity ids, so the two checks never mix. A
	// write that committed on its own (commitTaskWrites) has only taskWrites.
	taskWrites map[taskKey]bool
}

// taskWriteSet returns the task rows ops write, touches included.
func taskWriteSet(ops []scheduledTaskOp) map[taskKey]bool {
	if len(ops) == 0 {
		return nil
	}
	set := make(map[taskKey]bool, len(ops))
	for _, op := range ops {
		set[op.key] = true
	}
	return set
}
```

Replace the body of the conflict-check closure (`:497-525`) with:

```go
			m.mu.Lock()
			defer m.mu.Unlock()
			// FCW ordering uses commitSeq, not submitTime — see commitSeq's
			// doc comment. Entity ids and task rows are checked in two
			// separate loops over two separate sets.
			snapshotSeq := m.txSnapshotSeq[txID]
			taskWrites := taskWriteSet(m.scheduledTaskOps[txID])
			for _, committed := range m.committedLog {
				if committed.seq <= snapshotSeq {
					continue
				}
				conflict := false
				for entityID := range committed.writeSet {
					if tx.ReadSet[entityID] || tx.WriteSet[entityID] {
						conflict = true
						break
					}
				}
				for k := range committed.taskWrites {
					if taskWrites[k] {
						conflict = true
						break
					}
				}
				if conflict {
					delete(m.committing, txID)
					delete(m.active, txID)
					delete(m.savepoints, txID)
					delete(m.txUniqueKeys, txID)
					delete(m.txSnapshotSeq, txID)
					delete(m.supersededSaves, txID)
					delete(m.deletedBufferModels, txID)
					delete(m.scheduledTaskOps, txID)
					m.factory.discardAuditTxIndex(tid, txID)
					return spi.ErrConflict
				}
			}
			capturedKeys = m.txUniqueKeys[txID]                       // safe: tx.OpMu.Lock() prevents new recordUniqueKeys
			capturedScheduledTaskOps = m.scheduledTaskOps[txID]       // safe: tx.OpMu.Lock() prevents new stageTaskOps
			capturedSuperseded = m.supersededSaves[txID]              // safe: tx.OpMu.Lock() prevents new stageSuperseded
			capturedDeletedBufferModels = m.deletedBufferModels[txID] // safe: tx.OpMu.Lock() prevents new stageDeletedBufferModel
			return nil
```

In step 6 (`:795-800`), add `taskWrites: taskWriteSet(capturedScheduledTaskOps),`
to the appended `committedTx`, and replace the prune block (`:821-838`) with
`m.pruneCommittedLogLocked()`. Add the extracted function, unchanged in
behaviour:

```go
// pruneCommittedLogLocked drops the log entries no open transaction can
// conflict with: those stamped before the oldest open snapshot, or all of
// them when no transaction is open. Caller holds mu.
func (m *TransactionManager) pruneCommittedLogLocked() {
	var oldest time.Time
	for _, activeTx := range m.active {
		if oldest.IsZero() || activeTx.SnapshotTime.Before(oldest) {
			oldest = activeTx.SnapshotTime
		}
	}
	if oldest.IsZero() {
		m.committedLog = m.committedLog[:0]
		return
	}
	pruned := m.committedLog[:0]
	for _, c := range m.committedLog {
		if !c.submitTime.Before(oldest) {
			pruned = append(pruned, c)
		}
	}
	m.committedLog = pruned
}
```

Replace `commitTaskWrites` (from BM-1) with:

```go
// commitTaskWrites applies task-row writes that commit on their own — a
// never-joining method, or a joining one called without a transaction — and
// records them in the committed log, so that a transaction that began before
// them and writes one of the same rows fails at commit (C1).
//
// The apply, the stamp and the log entry share one mu section. Begin reads
// commitSeq under mu, so every Begin is ordered wholly before or wholly after
// the write: a transaction that began after it can see it and never conflicts
// with it. The entry is stamped under the same monotonic floor as a commit,
// so it is at or after every open snapshot and pruning keeps it while one of
// them is open.
//
// Caller holds factory.entityMu for writing; lock order entityMu → mu.
func (m *TransactionManager) commitTaskWrites(ops []scheduledTaskOp) {
	if len(ops) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	applyTaskOps(m.factory.scheduledTasks, ops)
	now := m.factory.clock.Now()
	if !now.After(m.lastSubmitTime) {
		now = m.lastSubmitTime.Add(time.Microsecond)
	}
	m.lastSubmitTime = now
	m.commitSeq++
	m.committedLog = append(m.committedLog, committedTx{
		submitTime: now,
		seq:        m.commitSeq,
		taskWrites: taskWriteSet(ops),
	})
	m.pruneCommittedLogLocked()
}
```

Extend `commitSeq`'s doc comment (`:89-103`) with one sentence: "A write that
commits on its own also takes a sequence number (see `commitTaskWrites`)."

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -skip 'TestConformance'
```

Expected: `ok`. Every test of the plugin except the `spitest` suite runs,
so the existing transaction tests (`txmanager_test.go`,
`concurrency_*_test.go`, `tx_*_test.go`) prove the entity check unchanged.
`TestConformance` is skipped until BM-7: its ScheduledTasks cases for C6 and
`ErrStoreRejected` go green only after BM-4 and BM-5.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/memory/txmanager.go plugins/memory/scheduled_task_c1_test.go && git commit -m "feat(memory): first-committer-wins covers task rows

Task rows join the commit's conflict check in a set of their own, next
to the entity ids. A task-row write that commits on its own takes a
sequence number and a log entry in the same critical section.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BM-4: C6 — a row an open transaction wrote is busy

**Spec:** §10.1 C6, `MarkUnsafe` row (`ErrTaskBusy`); §10.3 C6 bullet; §5.5
"The callback anti-pattern"; §13 rows "a row written by an open transaction is
not claimable (C6)", "`ErrTaskBusy` → safe failure", "joined callback writes
the fired entity, then an unsafe processor → `ErrTaskBusy`".

**Files:**
- Modify: `plugins/memory/txmanager.go` (new `busyTaskKeys`)
- Modify: `plugins/memory/scheduled_task_claims.go` (`ClaimDue`,
  `GiveBackIdle`, `MarkUnsafe`, `RecordAttempt`)
- Create: `plugins/memory/scheduled_task_busy_test.go`

**Interfaces:**
- Consumes: BM-1 staged ops, BM-2 methods; `spi.ErrTaskBusy`.
- Produces: `(*TransactionManager).busyTaskKeys() map[taskKey]bool`.

- [ ] **Step 1: Write the failing tests**

`plugins/memory/scheduled_task_busy_test.go`:

```go
package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestTasks_C6_ARowStagedByAnOpenTransactionIsNotClaimable(t *testing.T) {
	fx := newTaskFixture(t)
	arm(t, context.Background(), fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.DeleteForEntities(txCtx, taskTenantA, []string{"e1"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 0 {
		t.Fatalf("claimed %+v while an open transaction had staged its removal", got)
	}
	fx.rollback(t, taskTenantA, txID)
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 1 {
		t.Fatalf("claimed %d tasks after the rollback, want 1", len(got))
	}
}

// The run's own transaction wrote the row (a joined callback wrote the fired
// entity). The never-joining calls answer busy and never wait.
func TestTasks_C6_NeverJoiningWritesOnABusyRow(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.StampSegment(txCtx, refOf(c), false); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	if err := fx.sts.MarkUnsafe(bg, refOf(c)); !errors.Is(err, spi.ErrTaskBusy) {
		t.Fatalf("MarkUnsafe = %v, want ErrTaskBusy", err)
	}
	if err := fx.sts.RecordAttempt(bg, refOf(c), spi.Attempt{AtMs: 2_000, NextAttemptTime: 3_000}); !errors.Is(err, spi.ErrTaskBusy) {
		t.Fatalf("RecordAttempt = %v, want ErrTaskBusy", err)
	}
	if n, err := fx.sts.GiveBackIdle(bg, c.Claim.Owner, nil); err != nil || n != 0 {
		t.Fatalf("GiveBackIdle = %d, %v; want 0, nil", n, err)
	}

	fx.rollback(t, taskTenantA, txID)
	if err := fx.sts.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe after the rollback: %v", err)
	}
}

// A stale ref is refused as stale even when the row is busy.
func TestTasks_C6_AStaleRefIsStaleNotBusy(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]
	stale := refOf(c)
	stale.ClaimToken = uuid.New()

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.StampSegment(txCtx, refOf(c), false); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	if err := fx.sts.MarkUnsafe(bg, stale); !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("MarkUnsafe = %v, want ErrStaleClaim", err)
	}
	fx.rollback(t, taskTenantA, txID)
}

// A touch — RemoveLife of a life that is not the current one — changes
// nothing and does not make the row busy.
func TestTasks_C6_ATouchDoesNotMakeARowBusy(t *testing.T) {
	fx := newTaskFixture(t)
	arm(t, context.Background(), fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", uuid.New()); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 1 {
		t.Fatalf("claimed %d tasks, want 1: a touch is not a change", len(got))
	}
	fx.rollback(t, taskTenantA, txID)
}
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_C6_'
```

Expected: `ARowStagedByAnOpenTransactionIsNotClaimable` fails with `claimed
[...] while an open transaction had staged its removal`;
`NeverJoiningWritesOnABusyRow` fails with `MarkUnsafe = <nil>, want
ErrTaskBusy`. The other two pass.

- [ ] **Step 3: Write the implementation**

Add to `plugins/memory/txmanager.go`:

```go
// busyTaskKeys returns the task rows an open transaction has staged a change
// to. Such a row is not claimable, and MarkUnsafe and RecordAttempt answer
// spi.ErrTaskBusy for it, until the transaction ends (C6). A touch is not a
// change. Caller holds factory.entityMu, so no Commit is between reading its
// ops and applying them; lock order entityMu → mu.
func (m *TransactionManager) busyTaskKeys() map[taskKey]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := make(map[taskKey]bool)
	for _, ops := range m.scheduledTaskOps {
		for _, op := range ops {
			if !op.touch {
				busy[op.key] = true
			}
		}
	}
	return busy
}
```

In `plugins/memory/scheduled_task_claims.go`:

- `ClaimDue`: after `cutoff := …`, add `busy := s.f.txManager.busyTaskKeys()`;
  at the top of the candidate loop add

  ```go
  		if busy[k] {
  			continue
  		}
  ```
- `GiveBackIdle`: after the lock, add `busy := s.f.txManager.busyTaskKeys()`,
  and extend the skip condition to
  `if t.Status != spi.ScheduledTaskRunning || t.Claim.Owner != owner || kept[t.Claim.Token] || busy[k] {`.
- `MarkUnsafe` and `RecordAttempt`: right after the `fenced` check, add

  ```go
  	if s.f.txManager.busyTaskKeys()[k] {
  		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrTaskBusy)
  	}
  ```

  The fence comes first: a ref that is stale is refused as stale whether or
  not the row is busy, as PostgreSQL's `SELECT … FOR SHARE NOWAIT` on the
  tokens finds no row before it tries a lock.

Extend the package comment at the top of the file with: "A row an open
transaction has staged a change to is busy (C6): `ClaimDue` and `GiveBackIdle`
skip it; `MarkUnsafe` and `RecordAttempt` answer `spi.ErrTaskBusy`, which the
caller retries."

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_|TestSweeps_'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/memory/txmanager.go plugins/memory/scheduled_task_claims.go plugins/memory/scheduled_task_busy_test.go && git commit -m "feat(memory): a task row an open transaction wrote is busy

A row with a staged change is not claimable and not given back;
MarkUnsafe and RecordAttempt answer ErrTaskBusy for it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BM-5: `ErrStoreRejected` for what no backend stores

**Spec:** §5.6 "The node latches only on a deterministic rejection by the
store … Every store sets it"; §5.8 (the text is sanitised to valid UTF-8, no
NUL, at most 1 024 bytes); §13 row "`spi.ErrStoreRejected` → … (every backend
sets the marker)".

**Files:**
- Create: `plugins/memory/scheduled_task_validate.go`
- Modify: `plugins/memory/scheduled_task_store.go` (`ReconcileForEntity`, `Fail`)
- Modify: `plugins/memory/scheduled_task_claims.go` (`RecordAttempt`)
- Create: `plugins/memory/scheduled_task_rejected_test.go`

**Interfaces:**
- Consumes: `spi.ErrStoreRejected`, the five `spi.Failure*` reasons.
- Produces: `rejectErrorText(string) error`,
  `rejectFailureReason(spi.ScheduledTaskFailureReason) error`,
  `rejectArm(spi.ReconcileRequest) error`, `maxTaskErrorBytes = 1024`. BQ-6
  has the same three functions with the same rules. BM-6 marks the audit
  store's one deterministic failure the same way.

- [ ] **Step 1: Write the failing test**

`plugins/memory/scheduled_task_rejected_test.go`:

```go
package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestTasks_TheStoreRejectsWhatNoBackendStores(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]
	ref := refOf(c)
	const secret = "do-not-echo"

	noID := armTask(taskTenantA, "e1", "T")
	noID.ID = ""
	cases := []struct {
		name string
		call func() error
	}{
		{"attempt error with NUL", func() error {
			return fx.sts.RecordAttempt(bg, ref, spi.Attempt{Error: secret + "\x00", AtMs: 2_000, NextAttemptTime: 3_000})
		}},
		{"attempt error not UTF-8", func() error {
			return fx.sts.RecordAttempt(bg, ref, spi.Attempt{Error: secret + "\xff", AtMs: 2_000, NextAttemptTime: 3_000})
		}},
		{"attempt error over 1024 bytes", func() error {
			return fx.sts.RecordAttempt(bg, ref, spi.Attempt{Error: secret + strings.Repeat("x", 1024), AtMs: 2_000, NextAttemptTime: 3_000})
		}},
		{"failure with an unknown reason", func() error {
			return fx.sts.Fail(bg, ref, spi.Failure{Reason: "NOT_A_REASON", Error: secret, AtMs: 2_000})
		}},
		{"failure error with NUL", func() error {
			return fx.sts.Fail(bg, ref, spi.Failure{Reason: spi.FailureRunPanicked, Error: secret + "\x00", AtMs: 2_000})
		}},
		{"arm without an id", func() error {
			_, err := fx.sts.ReconcileForEntity(bg, spi.ReconcileRequest{TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
				Arm: []spi.ScheduledTask{noID}})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, spi.ErrStoreRejected) {
				t.Fatalf("err = %v, want ErrStoreRejected", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the rejection repeats the rejected text: %v", err)
			}
		})
	}

	got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != c.Claim.Token || got.ArmToken != c.ArmToken {
		t.Fatalf("a rejected write changed the task: %+v", got)
	}
}
```

- [ ] **Step 2: Run the test and see it fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_TheStoreRejects'
```

Expected: `attempt error with NUL` fails with `err = <nil>, want
ErrStoreRejected`; the later subtests fail with `ErrStaleClaim` or `<nil>`.

- [ ] **Step 3: Write the implementation**

`plugins/memory/scheduled_task_validate.go`:

```go
package memory

import (
	"fmt"
	"strings"
	"unicode/utf8"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// maxTaskErrorBytes is the most a recorded error text may take, in bytes.
const maxTaskErrorBytes = 1024

// rejectErrorText refuses an error text that no backend stores as given:
// PostgreSQL refuses NUL and invalid UTF-8 (SQLSTATE class 22). The message
// never repeats the text, which may carry anything a compute node sent.
func rejectErrorText(s string) error {
	switch {
	case len(s) > maxTaskErrorBytes:
		return fmt.Errorf("scheduled task error text is %d bytes, over %d: %w", len(s), maxTaskErrorBytes, spi.ErrStoreRejected)
	case !utf8.ValidString(s):
		return fmt.Errorf("scheduled task error text is not valid UTF-8: %w", spi.ErrStoreRejected)
	case strings.IndexByte(s, 0) >= 0:
		return fmt.Errorf("scheduled task error text contains NUL: %w", spi.ErrStoreRejected)
	}
	return nil
}

func rejectFailureReason(r spi.ScheduledTaskFailureReason) error {
	switch r {
	case spi.FailureUnsafeWorkNotCompleted, spi.FailureOwnerLostRepeatedly,
		spi.FailureExpiredAfterFailedAttempts, spi.FailureRunPanicked,
		spi.FailureStoppedAfterPartialCommit:
		return nil
	}
	return fmt.Errorf("scheduled task failure reason is not a known reason: %w", spi.ErrStoreRejected)
}

// rejectArm refuses an arm task without an id. The tenant and the entity
// come from the request (see newLife), so the task's own fields for them are
// not checked.
func rejectArm(req spi.ReconcileRequest) error {
	for _, a := range req.Arm {
		if a.ID == "" {
			return fmt.Errorf("scheduled task arm for entity %s names a task without an id: %w", req.EntityID, spi.ErrStoreRejected)
		}
	}
	return nil
}
```

Add the checks, each before any lock is taken:

- `ReconcileForEntity`, first statement:
  ```go
  	if err := rejectArm(req); err != nil {
  		return nil, err
  	}
  ```
- `Fail`, first statements:
  ```go
  	if err := rejectFailureReason(f.Reason); err != nil {
  		return err
  	}
  	if err := rejectErrorText(f.Error); err != nil {
  		return err
  	}
  ```
- `RecordAttempt`, first statement:
  ```go
  	if err := rejectErrorText(a.Error); err != nil {
  		return err
  	}
  ```

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestTasks_|TestSweeps_'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/memory/scheduled_task_validate.go plugins/memory/scheduled_task_store.go plugins/memory/scheduled_task_claims.go plugins/memory/scheduled_task_rejected_test.go && git commit -m "feat(memory): deterministic task-store rejections carry ErrStoreRejected

An error text with NUL, invalid UTF-8 or over 1024 bytes, an unknown
failure reason, and an arm task without an id are refused before
anything is written. The message never repeats the text.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BM-6: Audit events join the transaction

**Spec:** §5.7 ("`Fail` and the `SCHEDULED_TRANSITION_FAIL` audit event are
written in one transaction"); §5.2 (a superseded run records nothing); §5.6
(`ErrStoreRejected`); CLAUDE.md "a storage backend diverging from the others
on the same contract is a bug".

**The divergence this fixes.** PostgreSQL's audit store writes through the
context-resolving querier (`plugins/postgres/store_factory.go:255`,
`:162-164`), so an event recorded inside a transaction is rolled back with it.
The memory store appends at once, whatever is on `ctx`
(`plugins/memory/sm_audit_store.go:20-50`), and keeps the event after a
rollback; `TestSMAuditTxIndex_PrunedOnRollback` pins that today
(`sm_audit_txindex_prune_test.go:137-149`, "The event itself is not
transactional"). So on memory a superseded or rolled-back run leaves its
`SCHEDULED_TRANSITION_FIRE`, `TRANSITION_*` and `SCHEDULED_TRANSITION_FAIL`
rows visible, and PostgreSQL does not. This holds for every rolled-back
transaction, not only scheduled runs; the engine's comment at
`internal/domain/workflow/engine.go:1272-1277` already names the two
behaviours.

The fix: `Record` with a transaction on `ctx` stages the event on it. Commit
appends the staged events inside its `entityMu` section, just before the
commit-instant stamp; Rollback, every abort path and `RollbackToSavepoint`
drop them. A read with a transaction on `ctx` sees that transaction's staged
events, as PostgreSQL's does. `Record` without a transaction is unchanged.

**Files:**
- Modify: `plugins/memory/sm_audit_store.go` (`Record` `:20-50`, `GetEvents`
  `:59-72`, `GetEventsByTransaction` `:74-92`)
- Modify: `plugins/memory/txmanager.go` (`savepointSnapshot` `:45-66`; fields
  and constructor `:72-203`; the conflict abort, `abortClaim`, step 3 capture,
  the stamp at `:612-616`, step 6 cleanup, Rollback `:875-887`, `Savepoint`
  `:1020-1028`, `RollbackToSavepoint` `:1100-1102`)
- Modify: `plugins/memory/sm_audit_txindex_prune_test.go`
  (`TestSMAuditTxIndex_PrunedOnCommit` `:58`,
  `TestSMAuditTxIndex_PrunedOnRollback` `:98-150`)
- Modify: `plugins/memory/sm_audit_no_generator_test.go`
- Create: `plugins/memory/sm_audit_tx_join_test.go`

**Interfaces:**
- Consumes: `spi.StateMachineAuditStore` (unchanged), `spi.ErrStoreRejected`.
- Produces: `stagedAuditEvent{entityID, event}`,
  `(*TransactionManager).stageAuditEvent`, `stagedAuditEvents`,
  `(*StoreFactory).appendEventLocked`, `appendStagedAuditEvents`.

- [ ] **Step 1: Write the failing tests**

`plugins/memory/sm_audit_tx_join_test.go`:

```go
package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func auditStore(t *testing.T, fx taskFixture, ctx context.Context) spi.StateMachineAuditStore {
	t.Helper()
	as, err := fx.f.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	return as
}

func eventsOf(t *testing.T, as spi.StateMachineAuditStore, ctx context.Context) []spi.StateMachineEvent {
	t.Helper()
	got, err := as.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	return got
}

func TestAudit_ARecordInARolledBackTransactionIsDiscarded(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, State: "S", Details: "in tx", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := eventsOf(t, as, txCtx); len(got) != 1 || got[0].TimeUUID == "" {
		t.Fatalf("inside the transaction: %+v, want its own event with an id", got)
	}
	if got := eventsOf(t, as, ctx); len(got) != 0 {
		t.Fatalf("outside the transaction before commit: %d events, want 0", len(got))
	}
	fx.rollback(t, taskTenantA, txID)
	if got := eventsOf(t, as, ctx); len(got) != 0 {
		t.Fatalf("after rollback: %d events, want 0", len(got))
	}
}

func TestAudit_ARecordInACommittedTransactionIsKeptAtTheCommitInstant(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, State: "S", Details: "in tx", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	submit, err := fx.tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	got := eventsOf(t, as, ctx)
	if len(got) != 1 || !got[0].Timestamp.Equal(submit) {
		t.Fatalf("after commit: %+v, want one event at the commit instant %s", got, submit)
	}
}

func TestAudit_ARecordAfterARolledBackSavepointIsDropped(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	record := func(details string) {
		t.Helper()
		if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
			EventType: spi.SMEventTransitionMade, TransactionID: txID, Details: details, Timestamp: fx.clock.Now(),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	record("kept")
	spID, err := fx.tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	record("dropped")
	if err := fx.tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := eventsOf(t, as, ctx); len(got) != 1 || got[0].Details != "kept" {
		t.Fatalf("after commit: %+v, want only the event recorded before the savepoint", got)
	}
}

// An event whose JSON form cannot be written is refused on every backend:
// the SQL stores marshal it, and the marshal fails the same way every time.
func TestAudit_AnEventThatCannotBeWrittenIsAStoreRejection(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	as := auditStore(t, fx, ctx)
	err := as.Record(ctx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, Details: "bad", Data: map[string]any{"c": make(chan int)},
	})
	if !errors.Is(err, spi.ErrStoreRejected) {
		t.Fatalf("Record = %v, want ErrStoreRejected", err)
	}
}
```

In `plugins/memory/sm_audit_no_generator_test.go`, change the assertion
`}); err == nil {` to `}); !errors.Is(err, spi.ErrStoreRejected) {`, its
message to `"Record with no generator configured: err = %v, want ErrStoreRejected"`
with `err` as its argument (`t.Fatalf`), and add `"errors"` to its imports.

In `TestSMAuditTxIndex_PrunedOnCommit` (`sm_audit_txindex_prune_test.go:28-96`),
record the event with `ctx` instead of `txCtx` (the `audit.Record(txCtx, "e-1", …)` call at `:58`): an event recorded
outside the transaction but labelled with its id is indexed at once, which is
the case the index serves once in-transaction events are staged. Add to its
comment: "Recorded outside the transaction: an event recorded inside it is
staged and indexed only at commit."

Rewrite `TestSMAuditTxIndex_PrunedOnRollback` (`sm_audit_txindex_prune_test.go:98-150`)
to the new behaviour: after `Record` in the transaction the index holds **0**
transactions (a staged event is indexed only when its transaction commits);
after `Rollback` it holds 0; and `GetEvents(ctx, "e-1")` returns **0** events.
Its comment becomes: "A rolled-back transaction's events are never appended,
so they are never indexed; nothing of it reaches the trail."

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -run 'TestAudit_|TestSMAudit'
```

Expected: `ARecordInARolledBackTransactionIsDiscarded` fails with `outside the
transaction before commit: 1 events, want 0`;
`ARecordAfterARolledBackSavepointIsDropped` with two events;
`AnEventThatCannotBeWrittenIsAStoreRejection` with `Record = <nil>`;
`TestSMAuditStore_Record_NoGenerator` with an error that is not
`ErrStoreRejected`; the rewritten `TestSMAuditTxIndex_PrunedOnRollback` with
`holds 1 transactions`.

- [ ] **Step 3: Write the implementation**

In `plugins/memory/txmanager.go`:

```go
// stagedAuditEvent is one audit event recorded inside an open transaction,
// with its id already assigned. Commit appends it to the trail; every abort
// path drops it.
type stagedAuditEvent struct {
	entityID string
	event    spi.StateMachineEvent
}
```

Add to `TransactionManager` (after `scheduledTaskOps`):

```go
	// auditOps holds the audit events recorded while the transaction is open,
	// in order. Commit appends them inside its entityMu section, before the
	// commit-instant stamp; Rollback and every abort path drop them;
	// RollbackToSavepoint truncates them. Protected by mu. PostgreSQL gets the
	// same behaviour from recording on the transaction's connection.
	auditOps map[string][]stagedAuditEvent // txID → staged events
```

with `auditOps: make(map[string][]stagedAuditEvent),` in
`NewTransactionManager`, and:

```go
// stageAuditEvent appends ev to txID's staged audit events. Protected by mu.
func (m *TransactionManager) stageAuditEvent(txID string, ev stagedAuditEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditOps[txID] = append(m.auditOps[txID], ev)
}

// stagedAuditEvents returns a copy of txID's staged audit events. Protected by mu.
func (m *TransactionManager) stagedAuditEvents(txID string) []stagedAuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]stagedAuditEvent(nil), m.auditOps[txID]...)
}
```

Add `auditOpsLen int` to `savepointSnapshot`; set it in `Savepoint` with
`auditOpsLen: len(m.auditOps[txID]),`; in `RollbackToSavepoint`, next to the
`scheduledTaskOps` truncation:

```go
	if n := snap.auditOpsLen; n < len(m.auditOps[txID]) {
		m.auditOps[txID] = m.auditOps[txID][:n]
	}
```

Add `delete(m.auditOps, txID)` next to every `delete(m.scheduledTaskOps, txID)`
in `txmanager.go` (the conflict abort, `abortClaim`, the step 6 cleanup and
Rollback). Verify with
`grep -c 'delete(m.scheduledTaskOps, txID)' plugins/memory/txmanager.go` and
`grep -c 'delete(m.auditOps, txID)' plugins/memory/txmanager.go` — the counts
are equal.

In step 3's capture block add
`capturedAudit = m.auditOps[txID] // safe: tx.OpMu.Lock() prevents new stageAuditEvent`
(declared `var capturedAudit []stagedAuditEvent` beside the other captures),
and replace the stamp at `:612-616` with:

```go
		// Audit events recorded inside this transaction join the trail now,
		// after the last abort path, and then take the commit instant with
		// every other event labelled with this transaction — see
		// stampAuditEventsForTx.
		m.factory.appendStagedAuditEvents(tid, capturedAudit)
		m.factory.stampAuditEventsForTx(tid, txID, submitTime)
```

In `plugins/memory/sm_audit_store.go`, add `"encoding/json"` to the imports and
replace `Record` with:

```go
// Record assigns the event its id (see spi.StateMachineAuditStore): a
// caller's TimeUUID is ignored. With a transaction on ctx the event is staged
// on it and joins the trail only if the transaction commits.
func (s *StateMachineAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.factory.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured: %w", entityID, spi.ErrStoreRejected)
	}
	// The SQL stores write the event as JSON; one they cannot write is
	// refused here too, so no backend keeps what another refuses.
	if _, err := json.Marshal(event); err != nil {
		return fmt.Errorf("failed to record state machine event for entity %s: %w: %w", entityID, spi.ErrStoreRejected, err)
	}

	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		if tx.TenantID != s.tenant {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
		}
		event.TimeUUID = uuid.UUID(s.factory.uuids.NewTimeUUID()).String()
		s.factory.txManager.stageAuditEvent(tx.ID, stagedAuditEvent{entityID: entityID, event: copyEvent(event)})
		return nil
	}

	s.factory.smAuditMu.Lock()
	defer s.factory.smAuditMu.Unlock()
	// Minted under the lock, so id-assignment order and append order are the
	// same atomic step for events recorded outside a transaction (see
	// TestSMAudit_ConcurrentRecord_IDOrderMatchesAppendOrder).
	event.TimeUUID = uuid.UUID(s.factory.uuids.NewTimeUUID()).String()
	s.factory.appendEventLocked(s.tenant, entityID, copyEvent(event))
	return nil
}

// appendEventLocked appends cp to the trail of (tenant, entityID) and indexes
// it under its transaction label, in one critical section, so the
// commit-phase stamp is a lookup. Caller holds smAuditMu.
func (f *StoreFactory) appendEventLocked(tenant spi.TenantID, entityID string, cp spi.StateMachineEvent) {
	if f.smAudit[tenant] == nil {
		f.smAudit[tenant] = make(map[string][]spi.StateMachineEvent)
	}
	events := append(f.smAudit[tenant][entityID], cp)
	f.smAudit[tenant][entityID] = events
	if cp.TransactionID != "" {
		if f.smAuditTxIndex[tenant] == nil {
			f.smAuditTxIndex[tenant] = make(map[string][]auditEventRef)
		}
		f.smAuditTxIndex[tenant][cp.TransactionID] = append(
			f.smAuditTxIndex[tenant][cp.TransactionID],
			auditEventRef{entityID: entityID, index: len(events) - 1})
	}
}

// appendStagedAuditEvents appends a committing transaction's staged events in
// the order they were recorded. Called by Commit inside its entityMu section;
// entityMu → smAuditMu, and smAuditMu is a leaf.
func (f *StoreFactory) appendStagedAuditEvents(tenant spi.TenantID, staged []stagedAuditEvent) {
	if len(staged) == 0 {
		return
	}
	f.smAuditMu.Lock()
	defer f.smAuditMu.Unlock()
	for _, st := range staged {
		f.appendEventLocked(tenant, st.entityID, st.event)
	}
}

// stagedFor returns the events the transaction on ctx has recorded for
// entityID and not yet committed, so a read inside the transaction sees them.
func (s *StateMachineAuditStore) stagedFor(ctx context.Context, entityID string) []spi.StateMachineEvent {
	tx := spi.GetTransaction(ctx)
	if tx == nil || tx.TenantID != s.tenant {
		return nil
	}
	var out []spi.StateMachineEvent
	for _, st := range s.factory.txManager.stagedAuditEvents(tx.ID) {
		if st.entityID == entityID {
			out = append(out, copyEvent(st.event))
		}
	}
	return out
}
```

In `GetEvents`, compute `staged := s.stagedFor(ctx, entityID)` before taking
`smAuditMu`, and return `append(copyEvents(events), staged...)` (and
`staged` in place of the empty slice when the tenant or entity has no
committed events, keeping a non-nil empty slice when both are empty). In
`GetEventsByTransaction`, append the staged events whose `TransactionID`
equals `transactionID` to `filtered` the same way.

Update `discardAuditTxIndex`'s comment list of sibling staged maps
(`sm_audit_store.go:146`) to include `auditOps`.

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... -skip 'TestConformance'
```

Expected: `ok`, including `TestSMAudit_*` (the stamp, id-order, event-id and
tx-index tests) and `TestAudit_*`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/memory/sm_audit_store.go plugins/memory/txmanager.go plugins/memory/sm_audit_txindex_prune_test.go plugins/memory/sm_audit_no_generator_test.go plugins/memory/sm_audit_tx_join_test.go && git commit -m "fix(memory): audit events recorded in a transaction roll back with it

PostgreSQL records an audit event on the transaction's connection, so a
rolled-back transaction leaves no event; memory appended at once and
kept it. Record now stages the event on the transaction on ctx, Commit
appends it before the commit-instant stamp, and every abort path drops
it. An event that cannot be written as JSON, and a factory without an
id generator, are store rejections.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BM-7: Close-out — the whole ScheduledTasks suite, exit checks

**Spec:** §10.1 "Conformance"; §15 exit checks (the memory part).

**Files:**
- Verify only; modify whatever the runs below show.

**Interfaces:**
- Consumes: stream S's `runScheduledTasks`, run through the existing
  `TestConformance` (`plugins/memory/conformance_test.go:13-20`).
- Produces: nothing new.

- [ ] **Step 1: Run the whole plugin, conformance included**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/memory && go test ./... && go vet ./...
```

Expected: `ok` and no vet output. Every `TestConformance/ScheduledTasks/…`
subtest passes. If one fails, write the smallest plugin-local test that
reproduces it, see it fail, fix the store, and add the test to the file of
the task whose area it is; then rerun.

- [ ] **Step 2: Exit checks**

Each must print nothing:

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git grep -n -e RedispatchAfter -e AttemptCount -e ScanDue -e MarkRedispatch -e 'Upsert(' -e ErrUnsupported -e RunScheduledTaskStoreConformance -- plugins/memory
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git grep -n -e 'stageScheduledTaskOp' -e 'scheduledTaskUpsert' -e 'scheduledTaskDelete' -- plugins/memory
```

- [ ] **Step 3: Commit (only if Step 1 or 2 changed a file)**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add <each changed file by name> && git commit -m "test(memory): <what the conformance run showed>

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Coverage carried forward (§13 rows with an S tick, on memory)

| §13 row | Where it is proven on memory |
|---|---|
| every fenced method refuses a stale token; ABA | S suite (BM-7); `TestTasks_MarkUnsafe`, `TestTasks_RecordAttempt` |
| after a re-arm, every fenced write of the old life is refused | S suite; `TestTasks_C1_RemoveLifeOf…` |
| a reclaimed or re-armed task makes the old run's commit fail (C1) | `TestTasks_C1_*`; S suite |
| a replaced owner's segment commit is refused by its stamp | S suite; `fenced` in `StampSegment` |
| a mark survives the rollback of the transaction on `ctx` | `TestTasks_NeverJoiningMethodsIgnoreTheTransaction`; S suite |
| a joining read sees its own staged writes (C2) | `TestTasks_JoiningGetSeesStagedOps`; S suite |
| a row written by an open transaction is not claimable (C6); `ErrTaskBusy` | `TestTasks_C6_*`; S suite |
| `MarkUnsafe` racing `ClaimDue` (C3) | serialised by `entityMu`; S suite |
| concurrent `ClaimDue` calls get disjoint sets; two due siblings: one claim | `TestTasks_ClaimTakesOneTaskPerEntity`; S suite |
| per-tenant limit and turn-taking | `TestTasks_ClaimHonoursTenantLimitsAndTurns`; S suite |
| owner lost 3 times; a lost claim reply → `GiveBackIdle` | S suite; `TestTasks_GiveBackIdleKeepsLiveClaims` |
| dead owners and the marks of ended lives are swept | `TestSweeps_RemoveEndedLivesAndDeadUnusedOwners`; S suite |
| `GiveBackIdle` is not counted; `RetireOwner` removes liveness | `TestTasks_GiveBackIdleKeepsLiveClaims`, `TestTasks_ARetiredOwnerIsLostAtOnce` |
| a re-arm resets `PartialCommit`; self-loop re-arms the same id as a new life | `newLife`; `TestTasks_ArmStartsANewLife`; S suite |
| `ErrStoreRejected`; `lastError` of 1 024 bytes with multi-byte text stored | `TestTasks_TheStoreRejectsWhatNoBackendStores`; S suite |
| run never conflicts with its own claim under a frozen clock | `TestTasks_C1_ARunDoesNotConflictWithItsOwnClaimUnderAFrozenClock` |
| `DeleteForModel` in tenant A leaves tenant B's tasks; query tenant isolation | S suite; keys carry the tenant |
| `GET /scheduled-tasks` rows (store side: pages, filters, byte-wise id order) | `TestTasks_QueryOrdersIdsByteWise`; S suite (`Query`) |
| `SCHEDULED_TRANSITION_FAIL` written with `Fail` in one transaction; a rolled-back run leaves no audit event | `TestAudit_*` (BM-6) |

## Stream interface summary

Consumed from stream S (`interfaces.md`, unchanged): the whole
`spi.ScheduledTaskStore` interface, `spi.ScheduledTask` and its status and
reason constants, `spi.TaskRef`, `spi.TaskClaim`, `spi.ClaimRequest`,
`spi.Attempt`, `spi.Failure`, `spi.ScheduledTaskQuery`, `spi.ScheduledTaskPage`,
`spi.ScheduledTaskCursor`, `spi.ErrStaleClaim`, `spi.ErrMarkedByAnotherClaim`,
`spi.ErrTaskBusy`, `spi.ErrStoreRejected`, `spi.ErrTxTenantMismatch`,
`spi.ErrTxAlreadyCommitted`.

Produced for other streams: nothing exported. `memory.NewStoreFactory`,
`WithClock`, `NewTestClockAt` and `(*StoreFactory).ScheduledTaskStore` keep
their signatures; the engine (E), the runner (R), W, Q and T reach the store
through `spi.StoreFactory`.

Behaviour other streams rely on:
- `ClaimDue` errors on `Limit < 1` or `PerTenantLimit < 1`.
- `DeleteForModel` with a nil `keep` removes all of the model's tasks.
- `RecordAttempt` with `NotCounted` leaves `attempts` alone but still records
  `Error` as `LastError` and `AtMs` as `LastAttemptTime`.
- `Fail` always overwrites `LastError`, with an empty text too.
- `ReconcileForEntity` takes the tenant and the entity from the request; an
  arm task's own `TenantID` and `EntityID` are ignored.
- `Query` orders by `(ScheduledTime, ID)` with ids compared byte-wise, as
  SQLite (BINARY) and PostgreSQL (`COLLATE "C"`) do.
- A deterministic rejection satisfies `errors.Is(err, spi.ErrStoreRejected)`:
  an error text with NUL, invalid UTF-8 or over 1 024 bytes; an unknown
  failure reason; an arm task without an id; and, in the audit store, an event
  whose JSON form cannot be written or a factory without an id generator.
- An audit event recorded with a transaction on `ctx` is staged on it and
  rolls back with it (BM-6), as on PostgreSQL. So the §5.7 `Fail` + audit
  transaction is all-or-nothing on memory too.
- `RecordAttempt` can answer `spi.ErrTaskBusy` (C6). The runner's §5.6 retry
  covers it: it is neither a refusal nor `ErrStoreRejected`.
- `ReconcileForEntity`, `DeleteForEntities`, `DeleteForModel`, `RemoveLife`,
  `StampSegment` and `Fail` return `spi.ErrTxTenantMismatch` when the tenant
  argument is not the tenant of the transaction on `ctx`.
- A joining `Get` returns the latest committed row plus the transaction's
  staged ops, not the row as of Begin.

## Open points

1. **`spitest` subtest names are not in `interfaces.md`.** The per-task runs
   above use plugin-local tests; only BM-7 runs the whole suite. If stream S
   fixes names per clause, the lead can add `-run` filters per task.
2. **Shared harness factory.** `TestConformance` runs every `spitest` suite on
   one factory (`conformance_test.go:14-15`). `ClaimDue`, `GiveBackIdle`,
   `SweepOwners` and `SweepMarks` are cross-tenant, so a ScheduledTasks case
   sees tasks armed by other cases. Stream S must write its cases to tolerate
   that (for example a `NowMs` that only its own tasks satisfy, or a fresh
   owner per case), or give the suite its own factory.
3. **The set of deterministic rejections must be the same on every backend.**
   This stream rejects: an error text with NUL, invalid UTF-8, or over 1 024
   bytes; an unknown failure reason; an arm task with an empty id; an audit
   event whose JSON form cannot be written; an audit store without an id
   generator. BQ-6 and BQ-7 reject the same set, plus what SQLite itself
   refuses (a constraint, too big, a type mismatch, a bind out of range). BP
   and the S conformance case must agree. Suggest the lead fold this list into
   `interfaces.md`.
4. **Tenant of the method against tenant of the transaction.** Memory and
   SQLite refuse a joining write whose tenant argument is not the
   transaction's tenant, with `spi.ErrTxTenantMismatch`. PostgreSQL gets this
   for free only if BP adds the same check. Suggest it for `interfaces.md`.
5. **`RecordAttempt` / `GiveBackIdle` on a busy row.** Memory and SQLite
   answer `ErrTaskBusy` / skip the row at once. PostgreSQL waits up to
   `lock_timeout` (2 s) and then errors (55P03), which the caller retries.
   Both end in a retry; `interfaces.md` should say that `RecordAttempt` may
   return `ErrTaskBusy` and that it is retryable.
6. **Claim order.** `selectClaims` orders tenants by their earliest candidate
   (`NextAttemptTime`, then tenant id) and takes one task per tenant per turn.
   BQ-2 uses the same function; BP ranks in SQL. The S case for turn-taking
   must assert only what all three satisfy, or `interfaces.md` should fix this
   order. A shared pure helper in the SPI would remove the duplicate in BM and
   BQ; that is the lead's call.
7. **`Fail` leaves `LastAttemptTime` as it was** and sets `FailedTime` to
   `Failure.AtMs`. Confirm against the S case and Q's DTO rule
   (`lastAttemptTime` "after a failed attempt").
8. **`ReconcileRequest.Cancel` ids are removed only as tasks of the request's
   entity.** The engine only puts that entity's born-expired ids there
   (`internal/domain/workflow/arm.go:127-131`); an id of another entity is
   ignored rather than removed.
9. **The audit fix (BM-6) is wider than scheduled runs.** Memory and SQLite
   kept the audit events of every rolled-back transaction; PostgreSQL does
   not. BM-6 and BQ-7 align them, and change one memory test that pinned the
   old behaviour (`TestSMAuditTxIndex_PrunedOnRollback`). Two consequences
   for other streams: an event recorded on `ctx` of a transaction that then
   aborts — `EmitTransitionAborted` included — is now gone on memory and
   SQLite as it already is on PostgreSQL; and a test outside the plugins that
   relied on the leak shows at `make test`. Suggest stream S add an `Audit`
   conformance case, "an event recorded in a rolled-back transaction is not
   kept", so the rule is held on every backend, Cassandra included.
10. **In-transaction audit reads.** Memory returns the committed events and
    then the transaction's staged ones, in slice order; SQLite and PostgreSQL
    order by timestamp. That order difference predates this stream (memory
    has always returned append order) and no caller reads audit inside a
    transaction today (the only readers are `internal/domain/audit/handler.go:132, 240`).
