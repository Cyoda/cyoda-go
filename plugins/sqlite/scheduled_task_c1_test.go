package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
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
	if n := sqlite.CommittedLogLenForTest(fx.f); n != 0 {
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

// A RemoveLife naming a life the snapshot already shows replaced is no
// write. The row then changes after the RemoveLife is staged, and the commit
// still succeeds: a store that counted the stale call as a write would
// refuse it.
func TestTasks_C1_AStaleRemoveLifeIsNoWriteWhenTheRowChangesLater(t *testing.T) {
	outside := []struct {
		name   string
		change func(t *testing.T, fx taskFixture)
	}{
		{"Claim", func(t *testing.T, fx taskFixture) {
			if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 1 {
				t.Fatalf("claimed %d tasks, want 1", len(got))
			}
		}},
		{"Rearm", func(t *testing.T, fx taskFixture) {
			arm(t, context.Background(), fx.sts, taskTenantA, "e1", "T")
		}},
	}
	for _, o := range outside {
		t.Run(o.name, func(t *testing.T) {
			fx := newTaskFixture(t)
			bg := context.Background()
			arm(t, bg, fx.sts, taskTenantA, "e1", "T")
			old, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
			arm(t, bg, fx.sts, taskTenantA, "e1", "T") // replaced before Begin

			txID, txCtx := fx.begin(t, taskTenantA)
			if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", old.ArmToken); err != nil {
				t.Fatalf("RemoveLife: %v", err)
			}
			o.change(t, fx) // after the RemoveLife is staged
			if err := fx.commit(taskTenantA, txID); err != nil {
				t.Fatalf("Commit = %v, want nil: the stale RemoveLife wrote nothing", err)
			}
			if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !ok {
				t.Fatal("the stale RemoveLife removed the current life")
			}
		})
	}
}

// A task-row write made with no transaction on ctx commits on its own and is
// logged, so a transaction that began before it and writes the same row
// fails at commit. DeleteForModel stands for every such write.
func TestTasks_C1_ADeleteForModelWithNoTransactionFailsAnOlderCommit(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	cur, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.DeleteForModel(bg, taskTenantA, "M", 1, nil); err != nil {
		t.Fatalf("DeleteForModel: %v", err)
	}
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", cur.ArmToken); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit = %v, want ErrConflict: the life was removed after Begin", err)
	}
}

// Two ReconcileForEntity calls on one entity with no transaction on ctx are
// each atomic: each removes every other task of the entity, so exactly one
// of the two arms survives. Were the read and the write of one call split,
// both could read the same rows and both arms would stay.
func TestTasks_ConcurrentReconcilesWithNoTransactionAreAtomic(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	for i := 0; i < 100; i++ {
		entity := fmt.Sprintf("e%d", i)
		arm(t, bg, fx.sts, taskTenantA, entity, "T0")
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for j, tr := range []string{"T1", "T2"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[j] = fx.sts.ReconcileForEntity(bg, spi.ReconcileRequest{
					TenantID: taskTenantA, EntityID: entity, CurrentState: "S",
					Arm: []spi.ScheduledTask{armTask(taskTenantA, entity, tr)},
				})
			}()
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: ReconcileForEntity: %v", i, err)
			}
		}
		page, err := fx.sts.Query(bg, taskTenantA, spi.ScheduledTaskQuery{EntityID: entity, Limit: 10})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("iteration %d: entity holds %d tasks, want 1: two reconciles interleaved", i, len(page.Items))
		}
	}
}

// A RemoveLife with no transaction on ctx that names a life the row does not
// hold is no write: it logs nothing, so a transaction open across it that
// writes the row commits.
func TestTasks_C1_ARemoveLifeWithNoTransactionOfAnotherLifeIsNoWrite(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.StampSegment(txCtx, refOf(c), true); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	if err := fx.sts.RemoveLife(bg, taskTenantA, c.ID, uuid.New()); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if n := sqlite.CommittedLogLenForTest(fx.f); n != 0 {
		t.Fatalf("committed log holds %d entries, want 0: the RemoveLife wrote nothing", n)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit = %v, want nil", err)
	}
}

// The snapshot already shows the named life replaced; the row changes again
// after Begin, before the RemoveLife. The transaction sees the replacing
// life, not the named one, so the call is no write and the commit succeeds,
// as on PostgreSQL.
func TestTasks_C1_ARemoveLifeOfALifeTheSnapshotShowsReplacedIsNoWriteAfterALaterChange(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	old, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	arm(t, bg, fx.sts, taskTenantA, "e1", "T") // replaced before Begin

	txID, txCtx := fx.begin(t, taskTenantA)
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 1 { // changed after Begin
		t.Fatalf("claimed %d tasks, want 1", len(got))
	}
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", old.ArmToken); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit = %v, want nil: the snapshot never showed the named life", err)
	}
	if got, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !ok || got.Status != spi.ScheduledTaskRunning {
		t.Fatalf("task = %+v, %v; want it still RUNNING", got, ok)
	}
}

// A life armed by another writer after Begin is not in the snapshot. A
// RemoveLife naming it is no write: it removes nothing and the commit
// succeeds, as on PostgreSQL.
func TestTasks_C1_ARemoveLifeOfALifeArmedAfterBeginIsNoWrite(t *testing.T) {
	cases := []struct {
		name string
		// before arms the row before Begin, or leaves it missing.
		before bool
	}{{"RowReplaced", true}, {"RowCreated", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newTaskFixture(t)
			bg := context.Background()
			if c.before {
				arm(t, bg, fx.sts, taskTenantA, "e1", "T")
			}
			txID, txCtx := fx.begin(t, taskTenantA)
			arm(t, bg, fx.sts, taskTenantA, "e1", "T") // after Begin
			fresh, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

			if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", fresh.ArmToken); err != nil {
				t.Fatalf("RemoveLife: %v", err)
			}
			if err := fx.commit(taskTenantA, txID); err != nil {
				t.Fatalf("Commit = %v, want nil: the snapshot never showed the named life", err)
			}
			if got, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); !ok || got.ArmToken != fresh.ArmToken {
				t.Fatalf("task = %+v, %v; want the life armed after Begin kept", got, ok)
			}
		})
	}
}

// Every joining write plans from the snapshot. A task the snapshot shows but
// another writer removed after Begin is still one of the entity's tasks, so
// a reconcile removes it and the commit fails, as on PostgreSQL.
func TestTasks_C1_AReconcileSeesATaskRemovedAfterBegin(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T1")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.DeleteForEntities(bg, taskTenantA, []string{"e1"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}
	removed := arm(t, txCtx, fx.sts, taskTenantA, "e1", "T2")
	if len(removed) != 1 || removed[0].ID != "e1:S:T1" {
		t.Fatalf("removed = %+v, want e1:S:T1: the snapshot shows it", removed)
	}
	if err := fx.commit(taskTenantA, txID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit = %v, want ErrConflict: e1:S:T1 was removed after Begin", err)
	}
}

// Get with a transaction on ctx reads the view its joining writes plan
// from: the snapshot, then the transaction's own staged ops.
func TestTasks_GetWithATransactionReadsTheSnapshot(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	old, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

	txID, txCtx := fx.begin(t, taskTenantA)
	defer fx.rollback(t, taskTenantA, txID)
	arm(t, bg, fx.sts, taskTenantA, "e1", "T") // after Begin
	if got, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T"); !ok || got.ArmToken != old.ArmToken {
		t.Fatalf("task = %+v, %v; want the life the snapshot shows", got, ok)
	}
	arm(t, txCtx, fx.sts, taskTenantA, "e1", "T") // staged
	if got, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T"); !ok || got.ArmToken == old.ArmToken {
		t.Fatalf("task = %+v, %v; want the transaction's own staged life", got, ok)
	}
}

// Get with a transaction that has ended reads the committed row, as the
// memory backend does: the ended transaction has no snapshot left to show.
// Another transaction stays open so that the log keeps its entries.
func TestTasks_GetWithAnEndedTransactionReadsTheCommittedRow(t *testing.T) {
	ends := []struct {
		name string
		end  func(t *testing.T, fx taskFixture, txID string)
	}{
		{"Committed", func(t *testing.T, fx taskFixture, txID string) {
			if err := fx.commit(taskTenantA, txID); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		}},
		{"RolledBack", func(t *testing.T, fx taskFixture, txID string) {
			fx.rollback(t, taskTenantA, txID)
		}},
	}
	for _, e := range ends {
		t.Run(e.name, func(t *testing.T) {
			fx := newTaskFixture(t)
			bg := context.Background()
			arm(t, bg, fx.sts, taskTenantA, "e1", "T")
			holdID, _ := fx.begin(t, taskTenantA)
			defer fx.rollback(t, taskTenantA, holdID)

			txID, txCtx := fx.begin(t, taskTenantA)
			arm(t, bg, fx.sts, taskTenantA, "e1", "T") // after Begin
			cur, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
			e.end(t, fx, txID)

			if got, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T"); !ok || got.ArmToken != cur.ArmToken {
				t.Fatalf("task = %+v, %v; want the committed life", got, ok)
			}
		})
	}
}

// A transaction of another tenant on ctx does not change what Get returns:
// neither its snapshot nor its staged ops apply to this tenant's rows.
func TestTasks_GetWithAnotherTenantsTransactionReadsTheCommittedRow(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantB)
	defer fx.rollback(t, taskTenantB, txID)
	arm(t, bg, fx.sts, taskTenantA, "e1", "T") // after tenant B's Begin
	cur, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

	if got, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T"); !ok || got.ArmToken != cur.ArmToken {
		t.Fatalf("task = %+v, %v; want the committed life", got, ok)
	}
}
