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
