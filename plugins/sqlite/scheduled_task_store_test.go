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

const (
	taskTenantA spi.TenantID = "tenant-A"
	taskTenantB spi.TenantID = "tenant-B"
)

// begin starts a transaction for tenant and fails the test on error.
func begin(t *testing.T, tf taskFixture, tenant spi.TenantID) (string, context.Context) {
	t.Helper()
	txID, txCtx, err := tf.tm.Begin(tenantCtx(tenant))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return txID, txCtx
}

// commit commits txID and fails the test on error.
func commit(t *testing.T, tf taskFixture, ctx context.Context, txID string) {
	t.Helper()
	if err := tf.tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// rollback rolls back txID and fails the test on error.
func rollback(t *testing.T, tf taskFixture, ctx context.Context, txID string) {
	t.Helper()
	if err := tf.tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

// armTask returns a ScheduledTask suitable for req.Arm: the fields the
// caller controls. The store ignores everything else and sets ID, TenantID
// and EntityID from the reconcile request, never from this value.
func armTask(id, transition string, scheduledTime int64) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID:            id,
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime,
		ModelName:     "Order",
		ModelVersion:  1,
		Transition:    transition,
		SourceState:   "S",
		ArmedAt:       scheduledTime,
	}
}

// arm reconciles tenant's entityID with a single arm item, fails the test on
// error, and returns the tasks the reconcile removed.
func arm(t *testing.T, ctx context.Context, sts spi.ScheduledTaskStore, tenant spi.TenantID, entityID string, tasks ...spi.ScheduledTask) []spi.ScheduledTask {
	t.Helper()
	removed, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID:     tenant,
		EntityID:     entityID,
		CurrentState: "S",
		Arm:          tasks,
	})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	return removed
}

// getTask reads tenant's task id and fails the test on error. It does not
// fail when the task is missing: callers check found themselves.
func getTask(t *testing.T, ctx context.Context, sts spi.ScheduledTaskStore, tenant spi.TenantID, id string) (*spi.ScheduledTask, bool) {
	t.Helper()
	task, found, err := sts.Get(ctx, tenant, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return task, found
}

func TestTasks_ArmStartsANewLife(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)

	arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 1_500_000))

	got, found := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T")
	if !found {
		t.Fatal("expected the armed task to be found")
	}
	if got.Status != spi.ScheduledTaskWaiting {
		t.Errorf("Status = %v, want WAITING", got.Status)
	}
	if got.ArmToken == uuid.Nil {
		t.Error("ArmToken is nil, want a fresh token")
	}
	if got.NextAttemptTime != got.ScheduledTime {
		t.Errorf("NextAttemptTime = %d, want ScheduledTime %d", got.NextAttemptTime, got.ScheduledTime)
	}
	if got.Attempts != 0 || got.LostOwners != 0 {
		t.Errorf("Attempts = %d, LostOwners = %d, want 0, 0", got.Attempts, got.LostOwners)
	}
	if got.Claim != nil {
		t.Errorf("Claim = %+v, want nil", got.Claim)
	}
	if got.UnsafeMarked {
		t.Error("UnsafeMarked = true, want false for a new life")
	}
}

func TestTasks_ReconcileRemovesEveryOtherTaskOfTheEntity(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)

	arm(t, ctx, tf.sts, taskTenantA, "e1",
		armTask("e1:S:T1", "T1", 1_500_000), armTask("e1:S:T2", "T2", 1_600_000))

	removed := arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T3", "T3", 1_700_000))

	if len(removed) != 2 {
		t.Fatalf("removed = %+v, want e1:S:T1 and e1:S:T2", removed)
	}
	gotIDs := map[string]bool{removed[0].ID: true, removed[1].ID: true}
	if !gotIDs["e1:S:T1"] || !gotIDs["e1:S:T2"] {
		t.Fatalf("removed = %+v, want e1:S:T1 and e1:S:T2", removed)
	}
	if _, found := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T1"); found {
		t.Error("e1:S:T1 still present, want removed")
	}
	if _, found := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T2"); found {
		t.Error("e1:S:T2 still present, want removed")
	}
	if _, found := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T3"); !found {
		t.Error("e1:S:T3 not found, want armed")
	}
}

func TestTasks_StagedArmIsDiscardedOnRollback(t *testing.T) {
	tf := newTaskFixture(t)
	txID, txCtx := begin(t, tf, taskTenantA)

	arm(t, txCtx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 1_500_000))

	rollback(t, tf, tenantCtx(taskTenantA), txID)

	if _, found := getTask(t, context.Background(), tf.sts, taskTenantA, "e1:S:T"); found {
		t.Error("expected the staged arm to be discarded on rollback")
	}
}

func TestTasks_SavepointTruncatesStagedOps(t *testing.T) {
	tf := newTaskFixture(t)
	txID, txCtx := begin(t, tf, taskTenantA)

	arm(t, txCtx, tf.sts, taskTenantA, "e1", armTask("e1:S:T1", "T1", 1_500_000))

	sp, err := tf.tm.Savepoint(txCtx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	arm(t, txCtx, tf.sts, taskTenantA, "e2", armTask("e2:S:T2", "T2", 1_600_000))

	if err := tf.tm.RollbackToSavepoint(txCtx, txID, sp); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}

	commit(t, tf, txCtx, txID)

	if _, found := getTask(t, context.Background(), tf.sts, taskTenantA, "e1:S:T1"); !found {
		t.Error("e1:S:T1 not found, want it to survive the savepoint truncation")
	}
	if _, found := getTask(t, context.Background(), tf.sts, taskTenantA, "e2:S:T2"); found {
		t.Error("e2:S:T2 found, want it discarded by RollbackToSavepoint")
	}
}

func TestTasks_JoiningGetSeesStagedOps(t *testing.T) {
	tf := newTaskFixture(t)
	txID, txCtx := begin(t, tf, taskTenantA)

	arm(t, txCtx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 1_500_000))

	if _, found := getTask(t, txCtx, tf.sts, taskTenantA, "e1:S:T"); !found {
		t.Error("a joining Get did not see the transaction's own staged arm")
	}
	if _, found := getTask(t, context.Background(), tf.sts, taskTenantA, "e1:S:T"); found {
		t.Error("a non-joining Get saw an uncommitted staged arm")
	}

	rollback(t, tf, tenantCtx(taskTenantA), txID)
}

// TestTasks_RemovalsAreExpandedWhenStaged proves that a write staged earlier
// in a transaction — never yet committed to SQLite — is still visible to a
// later write in the same transaction that removes it: DeleteForEntities
// reads the transaction's own staged rows (C2), not only the committed ones.
func TestTasks_RemovalsAreExpandedWhenStaged(t *testing.T) {
	tf := newTaskFixture(t)
	txID, txCtx := begin(t, tf, taskTenantA)

	arm(t, txCtx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 1_500_000))

	if err := tf.sts.DeleteForEntities(txCtx, taskTenantA, []string{"e1"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}

	commit(t, tf, txCtx, txID)

	if _, found := getTask(t, context.Background(), tf.sts, taskTenantA, "e1:S:T"); found {
		t.Error("e1:S:T found after commit, want the staged arm removed by the staged DeleteForEntities")
	}
}

func TestTasks_StagingIntoACommittedTransactionIsRefused(t *testing.T) {
	tf := newTaskFixture(t)
	txID, txCtx := begin(t, tf, taskTenantA)
	commit(t, tf, txCtx, txID)

	err := tf.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", uuid.New())
	if !errors.Is(err, spi.ErrTxAlreadyCommitted) {
		t.Fatalf("RemoveLife into a committed tx: err = %v, want ErrTxAlreadyCommitted", err)
	}
}

func TestTasks_AWriteForAnotherTenantThanTheTransactionIsRefused(t *testing.T) {
	tf := newTaskFixture(t)
	_, txCtx := begin(t, tf, taskTenantA)

	err := tf.sts.RemoveLife(txCtx, taskTenantB, "e1:S:T", uuid.New())
	if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("RemoveLife for another tenant than the tx: err = %v, want ErrTxTenantMismatch", err)
	}
}

func TestTasks_GetIsTenantScoped(t *testing.T) {
	tf := newTaskFixture(t)
	arm(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 1_500_000))

	if _, found := getTask(t, context.Background(), tf.sts, taskTenantB, "e1:S:T"); found {
		t.Error("tenant B saw tenant A's task, want tenant isolation")
	}
	if _, found := getTask(t, context.Background(), tf.sts, taskTenantA, "e1:S:T"); !found {
		t.Error("tenant A did not see its own task")
	}
}

func TestTasks_ArmTakesTenantAndEntityFromTheRequest(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)

	// The Arm item's own tenant/entity are garbage the caller might supply;
	// the store must ignore them and use the request's TenantID/EntityID.
	bogus := armTask("e1:S:T", "T", 1_500_000)
	bogus.TenantID = taskTenantB
	bogus.EntityID = "not-e1"

	arm(t, ctx, tf.sts, taskTenantA, "e1", bogus)

	got, found := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T")
	if !found {
		t.Fatal("expected the armed task under tenant A")
	}
	if got.TenantID != taskTenantA {
		t.Errorf("TenantID = %q, want %q", got.TenantID, taskTenantA)
	}
	if got.EntityID != "e1" {
		t.Errorf("EntityID = %q, want e1", got.EntityID)
	}
	if _, found := getTask(t, context.Background(), tf.sts, taskTenantB, "e1:S:T"); found {
		t.Error("tenant B saw the task, want it armed only under tenant A")
	}
}

func TestTasks_QueryOrdersIdsByteWise(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)

	// Same ScheduledTime, so the tie is broken by ID. Byte-wise collation
	// puts every uppercase letter before every lowercase one; a
	// case-insensitive or locale collation would not.
	for _, e := range []struct{ id, entity string }{
		{"b:S:T", "eb"},
		{"B:S:T", "eB"},
		{"a:S:T", "ea"},
	} {
		arm(t, ctx, tf.sts, taskTenantA, e.entity, armTask(e.id, "T", 1_500_000))
	}

	page, err := tf.sts.Query(ctx, taskTenantA, spi.ScheduledTaskQuery{Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var ids []string
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	want := []string{"B:S:T", "a:S:T", "b:S:T"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}
