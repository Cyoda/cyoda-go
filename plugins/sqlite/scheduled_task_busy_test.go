package sqlite_test

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

// A RemoveLife of a life that is not the current one is no write and does
// not make the row busy.
func TestTasks_C6_ARemoveLifeThatRemovesNothingLeavesTheRowFree(t *testing.T) {
	fx := newTaskFixture(t)
	arm(t, context.Background(), fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", uuid.New()); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 1 {
		t.Fatalf("claimed %d tasks, want 1: a RemoveLife that removes nothing is not a change", len(got))
	}
	fx.rollback(t, taskTenantA, txID)
}

// A row is busy only for the tenant whose transaction staged the change:
// another tenant's row with the same task id stays claimable and markable.
func TestTasks_C6_AnotherTenantsStagedWriteLeavesThisTenantsRowFree(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	arm(t, bg, fx.sts, taskTenantB, "e1", "T")
	var cA spi.ScheduledTask
	for _, c := range claimDue(t, fx.sts, uuid.New(), false) {
		if c.TenantID == taskTenantA {
			cA = c
		}
	}
	if cA.Claim == nil {
		t.Fatal("tenant A's task was not claimed")
	}
	arm(t, bg, fx.sts, taskTenantA, "e2", "T")
	arm(t, bg, fx.sts, taskTenantB, "e2", "T")

	txID, txCtx := fx.begin(t, taskTenantB)
	if err := fx.sts.DeleteForEntities(txCtx, taskTenantB, []string{"e1", "e2"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}
	got := claimDue(t, fx.sts, uuid.New(), false)
	if len(got) != 1 || got[0].TenantID != taskTenantA || got[0].EntityID != "e2" {
		t.Fatalf("claimed %+v, want only tenant A's e2", got)
	}
	if err := fx.sts.MarkUnsafe(bg, refOf(cA)); err != nil {
		t.Fatalf("MarkUnsafe on tenant A's row = %v, want nil: tenant B's transaction does not make it busy", err)
	}
	fx.rollback(t, taskTenantB, txID)
}

// RollbackToSavepoint that drops the only change to a row ends its busy
// answer; the RemoveLife that removed nothing before the savepoint does not
// keep it busy.
func TestTasks_C6_RollbackToSavepointFreesTheRow(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.RemoveLife(txCtx, taskTenantA, "e1:S:T", uuid.New()); err != nil {
		t.Fatalf("RemoveLife: %v", err)
	}
	sp, err := fx.tm.Savepoint(txCtx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	if err := fx.sts.DeleteForEntities(txCtx, taskTenantA, []string{"e1"}); err != nil {
		t.Fatalf("DeleteForEntities: %v", err)
	}
	if got := claimDue(t, fx.sts, uuid.New(), false); len(got) != 0 {
		t.Fatalf("claimed %+v while the transaction had staged its removal", got)
	}
	if err := fx.tm.RollbackToSavepoint(txCtx, txID, sp); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}
	got := claimDue(t, fx.sts, uuid.New(), false)
	if len(got) != 1 {
		t.Fatalf("claimed %d tasks after RollbackToSavepoint, want 1", len(got))
	}
	if err := fx.sts.MarkUnsafe(bg, refOf(got[0])); err != nil {
		t.Fatalf("MarkUnsafe after RollbackToSavepoint = %v, want nil", err)
	}
	fx.rollback(t, taskTenantA, txID)
}
