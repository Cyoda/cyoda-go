package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
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

// Marks and owner liveness are durable: a restart with a mark set ends
// FAILED at the next claim, it is never re-run.
func TestTasks_MarksAndOwnersSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	clock := sqlite.NewTestClockAt(time.UnixMilli(1_000_000))
	open := func() (*sqlite.StoreFactory, spi.ScheduledTaskStore) {
		f, err := sqlite.NewStoreFactoryForTest(context.Background(), path, sqlite.WithClock(clock))
		if err != nil {
			t.Fatalf("NewStoreFactoryForTest: %v", err)
		}
		sts, err := f.ScheduledTaskStore(context.Background())
		if err != nil {
			t.Fatalf("ScheduledTaskStore: %v", err)
		}
		return f, sts
	}
	bg := context.Background()

	f1, sts1 := open()
	arm(t, bg, sts1, taskTenantA, "e1", "T")
	owner := uuid.New()
	if err := sts1.Heartbeat(bg, owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	c := claimDue(t, sts1, owner, false)[0]
	if err := sts1.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := f1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f2, sts2 := open()
	defer f2.Close()
	got, _ := getTask(t, bg, sts2, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Owner != owner || !got.UnsafeMarked {
		t.Fatalf("after restart: %+v, want RUNNING under the old owner with the mark", got)
	}
	if again := claimDue(t, sts2, uuid.New(), true); len(again) != 0 {
		t.Fatalf("claimed %+v while the old owner's heartbeat is fresh", again)
	}
	clock.Advance(2 * time.Minute)
	re := claimDue(t, sts2, uuid.New(), true)
	if len(re) != 1 || re[0].LostOwners != 1 || !re[0].UnsafeMarked {
		t.Fatalf("reclaim after restart = %+v, want lostOwners 1 and the mark", re)
	}
}

func TestTasks_SweepsRemoveEndedLivesAndDeadUnusedOwners(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	db := sqlite.DBForTest(fx.f)
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	busyOwner, idleOwner := uuid.New(), uuid.New()
	for _, o := range []uuid.UUID{busyOwner, idleOwner} {
		if err := fx.sts.Heartbeat(bg, o); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	c := claimDue(t, fx.sts, busyOwner, false)[0]
	if err := fx.sts.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := fx.sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if n := count(`SELECT count(*) FROM scheduled_task_marks`); n != 1 {
		t.Fatalf("SweepMarks left %d marks, want the live life's 1", n)
	}
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	if err := fx.sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if n := count(`SELECT count(*) FROM scheduled_task_marks`); n != 0 {
		t.Fatalf("SweepMarks kept %d marks of ended lives", n)
	}

	claimDue(t, fx.sts, busyOwner, false)
	fx.clock.Advance(time.Hour)
	if err := fx.sts.SweepOwners(bg, time.Minute); err != nil {
		t.Fatalf("SweepOwners: %v", err)
	}
	if n := count(`SELECT count(*) FROM scheduler_owners WHERE owner = '` + idleOwner.String() + `'`); n != 0 {
		t.Fatal("SweepOwners kept a dead owner no task references")
	}
	if n := count(`SELECT count(*) FROM scheduler_owners WHERE owner = '` + busyOwner.String() + `'`); n != 1 {
		t.Fatal("SweepOwners removed an owner a RUNNING task references")
	}
}

// The unique index on RUNNING rows holds: two claimers never leave an entity
// with two RUNNING tasks.
func TestTasks_ConcurrentClaimsAreDisjoint(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	for i := 0; i < 20; i++ {
		arm(t, bg, fx.sts, taskTenantA, fmt.Sprintf("e%02d", i), "T1", "T2")
	}
	var mu sync.Mutex
	seen := make(map[string]int)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := fx.sts.ClaimDue(bg, spi.ClaimRequest{Owner: uuid.New(), NowMs: 2_000, StaleAfter: time.Minute, Limit: 100, PerTenantLimit: 100})
			if err != nil {
				t.Errorf("ClaimDue: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range got {
				seen[c.EntityID]++
			}
		}()
	}
	wg.Wait()
	if len(seen) != 20 {
		t.Fatalf("claimed tasks of %d entities, want 20", len(seen))
	}
	for e, n := range seen {
		if n != 1 {
			t.Fatalf("entity %s got %d claims, want 1", e, n)
		}
	}
}

// --- SQLite-only addition: Query's Limit < 1 is a deterministic caller
// error, not merely "an error". Not part of the shared claims test file
// (Query is not a claims method), so it stays here rather than in
// scheduled_task_store_test.go, which is copied from memory unchanged. ---

func TestTasks_QueryRejectsALimitBelowOneAsStoreRejected(t *testing.T) {
	fx := newTaskFixture(t)
	_, err := fx.sts.Query(context.Background(), taskTenantA, spi.ScheduledTaskQuery{Limit: 0})
	if !errors.Is(err, spi.ErrStoreRejected) {
		t.Fatalf("Query Limit 0: err = %v, want ErrStoreRejected", err)
	}
}

// --- A staged post-image never freezes UnsafeMarked. The mark table can
// change while a joining transaction is open (a never-joining MarkUnsafe
// commits on its own, whatever transaction is on another caller's ctx), so
// a joining read of a row this transaction staged must derive UnsafeMarked
// fresh, the same way a committed read does, never trust a copy taken when
// the row was staged (matches the memory backend's
// withMarkLocked/applyTaskOps, which always re-derives on every read).
//
// The mark is written here BEFORE the transaction stages anything against
// the row, while it is not busy: marking a row an open transaction has
// already written is a different case (SPI C6, ErrTaskBusy — not yet
// implemented on this backend) and must not be asserted to succeed. Both
// tests below also pass unmodified against the memory backend (checked in
// a scratch copy); they are not yet in memory's own test file. ---

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
