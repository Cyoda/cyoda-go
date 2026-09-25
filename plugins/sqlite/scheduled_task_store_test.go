package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

const (
	taskTenantA spi.TenantID = "tenant-A"
	taskTenantB spi.TenantID = "tenant-B"
)

type taskFixture struct {
	f     *sqlite.StoreFactory
	clock *sqlite.TestClock
	sts   spi.ScheduledTaskStore
	tm    spi.TransactionManager
	path  string
}

// newTaskFixture returns a factory on a frozen clock: nothing advances it
// unless the test does.
func newTaskFixture(t *testing.T) taskFixture {
	t.Helper()
	clock := sqlite.NewTestClockAt(time.UnixMilli(1_000_000))
	path := filepath.Join(t.TempDir(), "tasks.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), path, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return taskFixture{f: f, clock: clock, sts: sts, tm: tm, path: path}
}

func tenantCtx(tenant spi.TenantID) context.Context { return testCtx(string(tenant)) }

func (fx taskFixture) begin(t *testing.T, tenant spi.TenantID) (string, context.Context) {
	t.Helper()
	txID, txCtx, err := fx.tm.Begin(tenantCtx(tenant))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return txID, txCtx
}

func (fx taskFixture) commit(tenant spi.TenantID, txID string) error {
	return fx.tm.Commit(tenantCtx(tenant), txID)
}

func (fx taskFixture) rollback(t *testing.T, tenant spi.TenantID, txID string) {
	t.Helper()
	if err := fx.tm.Rollback(tenantCtx(tenant), txID); err != nil {
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
	ctx := tenantCtx(taskTenantA)
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

// SQLite-only addition (not in the shared memory test file): a write staged
// earlier in a transaction — never yet committed to SQLite — is still
// visible to a later write in the SAME transaction that removes it: this
// proves DeleteForEntities reads the transaction's own staged rows (C2), not
// only the rows already committed to disk.
func TestTasks_StagedDeleteForEntitiesRemovesAStagedArmInTheSameTransaction(t *testing.T) {
	fx := newTaskFixture(t)
	txID, txCtx := fx.begin(t, taskTenantA)

	arm(t, txCtx, fx.sts, taskTenantA, "e1", "T")
	if err := fx.sts.DeleteForEntities(txCtx, taskTenantA, []string{"e1"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, ok := getTask(t, context.Background(), fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("e1:S:T found after commit, want the staged arm removed by the staged DeleteForEntities")
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
