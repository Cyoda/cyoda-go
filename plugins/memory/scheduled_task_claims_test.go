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
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	for name, req := range map[string]spi.ClaimRequest{
		"Limit 0":          {Owner: uuid.New(), NowMs: 2_000, Limit: 0, PerTenantLimit: 1},
		"PerTenantLimit 0": {Owner: uuid.New(), NowMs: 2_000, Limit: 1, PerTenantLimit: 0},
	} {
		if _, err := fx.sts.ClaimDue(bg, req); !errors.Is(err, spi.ErrStoreRejected) {
			t.Fatalf("%s: ClaimDue = %v, want ErrStoreRejected", name, err)
		}
	}
	if got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T"); got.Status != spi.ScheduledTaskWaiting {
		t.Fatalf("task status = %s after refused claims, want WAITING", got.Status)
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
	if claimed.ClaimedFromLostOwner {
		t.Fatalf("a claim of a WAITING task is flagged as from a lost owner: %+v", claimed)
	}

	second := uuid.New()
	if got := claimDue(t, fx.sts, second, true); len(got) != 0 {
		t.Fatalf("claimed %+v from an owner that is not stale", got)
	}
	fx.clock.Advance(2 * time.Minute)
	if got := claimDue(t, fx.sts, second, false); len(got) != 0 {
		t.Fatalf("claimed %+v from a stale owner without AllowLostOwner", got)
	}
	got := claimDue(t, fx.sts, second, true)
	if len(got) != 1 || got[0].LostOwners != 1 || got[0].Claim.Owner != second || got[0].Claim.Token == claimed.Claim.Token ||
		!got[0].ClaimedFromLostOwner {
		t.Fatalf("reclaim = %+v, want one task under the new owner, lostOwners 1, a new claim token, flagged as from a lost owner", got)
	}
	if stored, _ := getTask(t, context.Background(), fx.sts, taskTenantA, got[0].ID); stored.ClaimedFromLostOwner {
		t.Fatal("Get returned ClaimedFromLostOwner; only a ClaimDue result carries it")
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

// --- A staged post-image never freezes UnsafeMarked. The mark table can
// change while a joining transaction is open (a never-joining MarkUnsafe
// commits on its own, whatever transaction is on another caller's ctx), so
// a joining read of a row this transaction staged must derive UnsafeMarked
// fresh, the same way a committed read does, never trust a copy taken when
// the row was staged.
//
// The mark is written here BEFORE the transaction stages anything against
// the row, while it is not busy: marking a row an open transaction has
// already written is a different case (C6, ErrTaskBusy). The SQLite backend
// runs the same two test bodies. ---

// v.get: a joining Get of a row this transaction staged still reports a
// mark written before the transaction began.
func TestTasks_StagedGetDerivesTheMarkFreshInsideATransaction(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]

	if err := fx.sts.MarkUnsafe(context.Background(), refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}

	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.sts.StampSegment(txCtx, refOf(c), true); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}

	got, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T")
	if !ok {
		t.Fatal("a joining Get did not see the transaction's own staged row")
	}
	if !got.UnsafeMarked {
		t.Fatal("UnsafeMarked = false, want true: a staged post-image must derive the mark fresh, not freeze it at staging time")
	}
	fx.rollback(t, taskTenantA, txID)
}

// v.where: ReconcileForEntity's removed list, built through where(), must
// derive UnsafeMarked fresh for a row this same transaction's earlier write
// already staged.
func TestTasks_ReconcileRemovedListDerivesTheMarkFreshForAStagedRow(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, fx.sts, taskTenantA, "e1", "T1", "T2")
	c := claimDue(t, fx.sts, uuid.New(), false)[0] // claims e1:S:T1 or e1:S:T2

	if err := fx.sts.MarkUnsafe(context.Background(), refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}

	txID, txCtx := fx.begin(t, taskTenantA)
	// Stage a write to the claimed task's row first, in the same
	// transaction; the re-arm below then reads it a second time, through
	// where(), and must still see the mark.
	if err := fx.sts.StampSegment(txCtx, refOf(c), true); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}

	removed, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
		Arm: []spi.ScheduledTask{armTask(taskTenantA, "e1", "T3")},
	})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	var found bool
	for _, r := range removed {
		if r.ID != c.ID {
			continue
		}
		found = true
		if !r.UnsafeMarked {
			t.Fatal("UnsafeMarked = false on the removed staged row, want true: where() must derive the mark fresh")
		}
	}
	if !found {
		t.Fatalf("removed = %+v, want it to include the claimed task %s", removed, c.ID)
	}
	fx.rollback(t, taskTenantA, txID)
}
