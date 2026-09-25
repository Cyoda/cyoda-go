package memory_test

import (
	"context"
	"errors"
	"sync"
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

// Two joining writes on one transaction, made at the same time, each plan
// from the view that includes the other's ops, or from the view before it,
// but never both from the same view: neither post-image may overwrite the
// other. A StampSegment staged first is kept by the Fail after it; a Fail
// staged first makes the StampSegment stale.
func TestTasks_ConcurrentJoiningWritesOnOneTransactionBothSurvive(t *testing.T) {
	for i := 0; i < 500; i++ {
		fx := newTaskFixture(t)
		bg := context.Background()
		arm(t, bg, fx.sts, taskTenantA, "e1", "T")
		c := claimDue(t, fx.sts, uuid.New(), false)[0]
		txID, txCtx := fx.begin(t, taskTenantA)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var stampErr, failErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			stampErr = fx.sts.StampSegment(txCtx, refOf(c), true)
		}()
		go func() {
			defer wg.Done()
			<-start
			failErr = fx.sts.Fail(txCtx, refOf(c), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: 3_000})
		}()
		close(start)
		wg.Wait()

		if failErr != nil {
			t.Fatalf("iteration %d: Fail = %v, want nil: nothing stages before it can make it stale", i, failErr)
		}
		if stampErr != nil && !errors.Is(stampErr, spi.ErrStaleClaim) {
			t.Fatalf("iteration %d: StampSegment = %v, want nil or ErrStaleClaim", i, stampErr)
		}
		got, ok := getTask(t, txCtx, fx.sts, taskTenantA, c.ID)
		if !ok || got.Status != spi.ScheduledTaskFailed {
			t.Fatalf("iteration %d: staged task = %+v, %v; want FAILED: the Fail was overwritten", i, got, ok)
		}
		if stampErr == nil && !got.PartialCommit {
			t.Fatalf("iteration %d: the accepted StampSegment was overwritten: PartialCommit is false", i)
		}
		fx.rollback(t, taskTenantA, txID)
	}
}

func TestTasks_AJoiningWriteOnARolledBackTransactionIsRefused(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	txID, txCtx := fx.begin(t, taskTenantA)
	fx.rollback(t, taskTenantA, txID)

	_, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
		Arm: []spi.ScheduledTask{armTask(taskTenantA, "e1", "T")},
	})
	if !errors.Is(err, spi.ErrTxRolledBack) {
		t.Fatalf("err = %v, want ErrTxRolledBack", err)
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); ok {
		t.Fatal("a write refused on a rolled-back transaction was applied")
	}
}
