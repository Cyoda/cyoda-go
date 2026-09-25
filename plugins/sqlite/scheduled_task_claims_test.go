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

// claimDueNowMs is a caller clock far past any ScheduledTime this file
// arms, so a WAITING task armed by armTask is always due: it lets claimDue
// stand in for a full ClaimRequest in every test that does not itself
// exercise NowMs or the limits.
const claimDueNowMs = int64(1) << 40

// claimDue claims due tasks for owner, across every tenant, on a background
// context, and fails the test on error. allowLostOwner sets
// ClaimRequest.AllowLostOwner; StaleAfter is a minute, as production
// defaults it.
func claimDue(t *testing.T, sts spi.ScheduledTaskStore, owner uuid.UUID, allowLostOwner bool) []spi.ScheduledTask {
	t.Helper()
	got, err := sts.ClaimDue(context.Background(), spi.ClaimRequest{
		Owner:          owner,
		NowMs:          claimDueNowMs,
		StaleAfter:     time.Minute,
		Limit:          100,
		PerTenantLimit: 100,
		AllowLostOwner: allowLostOwner,
	})
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	return got
}

// refOf builds the TaskRef of c's current claim.
func refOf(c spi.ScheduledTask) spi.TaskRef {
	return spi.TaskRef{TenantID: c.TenantID, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: c.Claim.Token}
}

func TestTasks_ClaimTakesOneTaskPerEntity(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, tf.sts, taskTenantA, "e1",
		armTask("e1:S:T1", "T1", 400_000), armTask("e1:S:T2", "T2", 400_000))

	first := claimDue(t, tf.sts, uuid.New(), false)
	if len(first) != 1 {
		t.Fatalf("first claim = %d tasks, want 1", len(first))
	}
	second := claimDue(t, tf.sts, uuid.New(), false)
	if len(second) != 0 {
		t.Fatalf("second claim = %+v, want none: a RUNNING sibling blocks the entity", second)
	}
}

func TestTasks_ClaimHonoursTenantLimitsAndTurns(t *testing.T) {
	tf := newTaskFixture(t)
	arm(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "eA1", armTask("eA1:S:T", "T", 400_000))
	arm(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "eA2", armTask("eA2:S:T", "T", 400_000))
	arm(t, tenantCtx(taskTenantB), tf.sts, taskTenantB, "eB1", armTask("eB1:S:T", "T", 400_000))

	got, err := tf.sts.ClaimDue(context.Background(), spi.ClaimRequest{
		Owner: uuid.New(), NowMs: claimDueNowMs, StaleAfter: time.Minute,
		Limit: 2, PerTenantLimit: 1,
	})
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("claimed %d tasks, want 2: one per tenant per turn", len(got))
	}
	byTenant := map[spi.TenantID]int{}
	for _, c := range got {
		byTenant[c.TenantID]++
	}
	if byTenant[taskTenantA] != 1 || byTenant[taskTenantB] != 1 {
		t.Fatalf("claimed %+v, want exactly one per tenant: PerTenantLimit honoured, tenants take turns", got)
	}
}

func TestTasks_ClaimRejectsALimitBelowOne(t *testing.T) {
	tf := newTaskFixture(t)
	arm(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))

	for name, req := range map[string]spi.ClaimRequest{
		"Limit 0":           {Owner: uuid.New(), NowMs: claimDueNowMs, StaleAfter: time.Minute, Limit: 0, PerTenantLimit: 1},
		"Limit -1":          {Owner: uuid.New(), NowMs: claimDueNowMs, StaleAfter: time.Minute, Limit: -1, PerTenantLimit: 1},
		"PerTenantLimit 0":  {Owner: uuid.New(), NowMs: claimDueNowMs, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: 0},
		"PerTenantLimit -1": {Owner: uuid.New(), NowMs: claimDueNowMs, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: -1},
	} {
		if _, err := tf.sts.ClaimDue(context.Background(), req); err == nil {
			t.Errorf("%s: ClaimDue accepted, want an error", name)
		}
	}
	got, found := getTask(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "e1:S:T")
	if !found || got.Status != spi.ScheduledTaskWaiting {
		t.Fatalf("task = %+v, found = %v, want left WAITING after a refused call", got, found)
	}
}

func TestTasks_LostOwnerClaimUsesTheStoreClock(t *testing.T) {
	tf := newTaskFixture(t)
	arm(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))

	owner := uuid.New()
	if err := tf.sts.Heartbeat(context.Background(), owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if first := claimDue(t, tf.sts, owner, false); len(first) != 1 {
		t.Fatalf("first claim = %d, want 1", len(first))
	}

	if again := claimDue(t, tf.sts, uuid.New(), true); len(again) != 0 {
		t.Fatalf("claimed %+v while the owner's heartbeat is fresh", again)
	}

	tf.clock.Advance(2 * time.Minute)
	lost := claimDue(t, tf.sts, uuid.New(), true)
	if len(lost) != 1 || lost[0].LostOwners != 1 || !lost[0].ClaimedFromLostOwner {
		t.Fatalf("lost-owner claim = %+v, want LostOwners 1 and ClaimedFromLostOwner", lost)
	}
}

func TestTasks_ARetiredOwnerIsLostAtOnce(t *testing.T) {
	tf := newTaskFixture(t)
	arm(t, tenantCtx(taskTenantA), tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))

	owner := uuid.New()
	if err := tf.sts.Heartbeat(context.Background(), owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if first := claimDue(t, tf.sts, owner, false); len(first) != 1 {
		t.Fatalf("first claim = %d, want 1", len(first))
	}
	if err := tf.sts.RetireOwner(context.Background(), owner); err != nil {
		t.Fatalf("RetireOwner: %v", err)
	}

	// No clock advance: retiring removes the liveness record outright, so
	// the task is lost at once, not after StaleAfter elapses.
	lost := claimDue(t, tf.sts, uuid.New(), true)
	if len(lost) != 1 || lost[0].LostOwners != 1 {
		t.Fatalf("claim after RetireOwner = %+v, want lost at once", lost)
	}
}

func TestTasks_MarkUnsafe(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))

	a := claimDue(t, tf.sts, uuid.New(), false)[0]
	if err := tf.sts.MarkUnsafe(context.Background(), refOf(a)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := tf.sts.MarkUnsafe(context.Background(), refOf(a)); err != nil {
		t.Fatalf("MarkUnsafe (idempotent for the same claim): %v", err)
	}
	got, _ := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T")
	if !got.UnsafeMarked {
		t.Fatal("UnsafeMarked = false, want true after MarkUnsafe")
	}

	tf.clock.Advance(2 * time.Minute)
	b := claimDue(t, tf.sts, uuid.New(), true)[0]
	if !b.UnsafeMarked {
		t.Fatal("the claim that took over the marked life did not see the mark")
	}
	if err := tf.sts.MarkUnsafe(context.Background(), refOf(b)); !errors.Is(err, spi.ErrMarkedByAnotherClaim) {
		t.Fatalf("MarkUnsafe by a later claim of the marked life: err = %v, want ErrMarkedByAnotherClaim", err)
	}
	if err := tf.sts.MarkUnsafe(context.Background(), refOf(a)); !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("MarkUnsafe by the superseded claim: err = %v, want ErrStaleClaim", err)
	}
}

func TestTasks_NeverJoiningMethodsIgnoreTheTransaction(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))
	a := claimDue(t, tf.sts, uuid.New(), false)[0]

	txID, txCtx := begin(t, tf, taskTenantA)
	if err := tf.sts.MarkUnsafe(txCtx, refOf(a)); err != nil {
		t.Fatalf("MarkUnsafe on a transaction's ctx: %v", err)
	}
	if err := tf.sts.RecordAttempt(txCtx, refOf(a), spi.Attempt{Error: "E", AtMs: 1, NextAttemptTime: 2}); err != nil {
		t.Fatalf("RecordAttempt on a transaction's ctx: %v", err)
	}
	rollback(t, tf, tenantCtx(taskTenantA), txID)

	got, _ := getTask(t, context.Background(), tf.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
		t.Fatalf("Status = %v, Claim = %+v, want WAITING with no claim: RecordAttempt must commit on its own, not join the rolled-back tx on ctx", got.Status, got.Claim)
	}
	if !got.UnsafeMarked {
		t.Fatal("the mark did not survive the rollback of the transaction on ctx: MarkUnsafe must never join it")
	}
}

func TestTasks_RecordAttempt(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))
	a := claimDue(t, tf.sts, uuid.New(), false)[0]

	if err := tf.sts.MarkUnsafe(context.Background(), refOf(a)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := tf.sts.RecordAttempt(context.Background(), refOf(a), spi.Attempt{
		Error: "boom", AtMs: 42, NextAttemptTime: 99, ClearOwnMark: true,
	}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}

	got, _ := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
		t.Fatalf("after RecordAttempt: Status = %v, Claim = %+v, want WAITING, nil claim", got.Status, got.Claim)
	}
	if got.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", got.Attempts)
	}
	if got.LastAttemptTime == nil || *got.LastAttemptTime != 42 {
		t.Fatalf("LastAttemptTime = %v, want 42", got.LastAttemptTime)
	}
	if got.LastError != "boom" {
		t.Fatalf("LastError = %q, want %q", got.LastError, "boom")
	}
	if got.NextAttemptTime != 99 {
		t.Fatalf("NextAttemptTime = %d, want 99", got.NextAttemptTime)
	}
	if got.UnsafeMarked {
		t.Fatal("UnsafeMarked = true, want false: ClearOwnMark should have removed the mark")
	}

	b := claimDue(t, tf.sts, uuid.New(), false)
	if len(b) != 1 {
		t.Fatalf("claimed %d, want 1 (claimable now that NextAttemptTime has passed)", len(b))
	}
	if err := tf.sts.RecordAttempt(context.Background(), refOf(b[0]), spi.Attempt{
		Error: "again", AtMs: 100, NextAttemptTime: 200, NotCounted: true,
	}); err != nil {
		t.Fatalf("RecordAttempt (NotCounted): %v", err)
	}
	got2, _ := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T")
	if got2.Attempts != 1 {
		t.Fatalf("Attempts after a NotCounted attempt = %d, want unchanged 1", got2.Attempts)
	}
	if got2.LastError != "again" {
		t.Fatalf("LastError after a NotCounted attempt = %q, want %q: it is still recorded", got2.LastError, "again")
	}
}

func TestTasks_FailOverwritesLastError(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))

	claimDue(t, tf.sts, uuid.New(), false) // never heartbeats
	tf.clock.Advance(2 * time.Minute)
	b := claimDue(t, tf.sts, uuid.New(), true)[0] // lost-owner reclaim: LostOwners = 1
	if b.LostOwners != 1 {
		t.Fatalf("LostOwners = %d, want 1 before Fail", b.LostOwners)
	}
	if err := tf.sts.RecordAttempt(context.Background(), refOf(b), spi.Attempt{
		Error: "first", AtMs: 55, NextAttemptTime: 400_000,
	}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	c := claimDue(t, tf.sts, uuid.New(), false)[0]

	if err := tf.sts.Fail(ctx, refOf(c), spi.Failure{
		Reason: spi.FailureRunPanicked, Error: "", AtMs: 900_000,
	}); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskFailed {
		t.Fatalf("Status = %v, want FAILED", got.Status)
	}
	if got.LastError != "" {
		t.Fatalf("LastError = %q, want overwritten to empty", got.LastError)
	}
	if got.LastAttemptTime == nil || *got.LastAttemptTime != 55 {
		t.Fatalf("LastAttemptTime = %v, want unchanged at 55", got.LastAttemptTime)
	}
	if got.LostOwners != 1 {
		t.Fatalf("LostOwners = %d, want unchanged at 1", got.LostOwners)
	}
	if got.FailedTime == nil || *got.FailedTime != 900_000 {
		t.Fatalf("FailedTime = %v, want 900000", got.FailedTime)
	}
	if got.Claim != nil {
		t.Fatalf("Claim = %+v, want nil after Fail", got.Claim)
	}
}

func TestTasks_GiveBackIdleKeepsLiveClaims(t *testing.T) {
	tf := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	arm(t, ctx, tf.sts, taskTenantA, "e1", armTask("e1:S:T1", "T1", 400_000))
	arm(t, ctx, tf.sts, taskTenantA, "e2", armTask("e2:S:T2", "T2", 400_000))

	owner := uuid.New()
	claimed := claimDue(t, tf.sts, owner, false)
	if len(claimed) != 2 {
		t.Fatalf("claimed %d, want 2", len(claimed))
	}
	var live, idle spi.ScheduledTask
	for _, c := range claimed {
		if c.ID == "e1:S:T1" {
			live = c
		} else {
			idle = c
		}
	}

	n, err := tf.sts.GiveBackIdle(context.Background(), owner, []uuid.UUID{live.Claim.Token})
	if err != nil {
		t.Fatalf("GiveBackIdle: %v", err)
	}
	if n != 1 {
		t.Fatalf("GiveBackIdle returned %d, want 1", n)
	}

	gotLive, _ := getTask(t, ctx, tf.sts, taskTenantA, "e1:S:T1")
	if gotLive.Status != spi.ScheduledTaskRunning || gotLive.Claim == nil || gotLive.Claim.Token != live.Claim.Token {
		t.Fatalf("live claim = %+v, want unchanged RUNNING", gotLive)
	}
	gotIdle, _ := getTask(t, ctx, tf.sts, taskTenantA, "e2:S:T2")
	if gotIdle.Status != spi.ScheduledTaskWaiting || gotIdle.Claim != nil {
		t.Fatalf("idle claim = %+v, want given back to WAITING", gotIdle)
	}
	if gotIdle.NextAttemptTime != idle.NextAttemptTime {
		t.Fatalf("NextAttemptTime = %d, want unchanged %d", gotIdle.NextAttemptTime, idle.NextAttemptTime)
	}
	if gotIdle.Attempts != 0 || gotIdle.LostOwners != 0 {
		t.Fatalf("Attempts = %d, LostOwners = %d, want both 0: a give-back is not counted", gotIdle.Attempts, gotIdle.LostOwners)
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
	arm(t, bg, sts1, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))
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

	arm(t, bg, fx.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))
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
	arm(t, bg, fx.sts, taskTenantA, "e1", armTask("e1:S:T", "T", 400_000))
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
		arm(t, bg, fx.sts, taskTenantA, fmt.Sprintf("e%02d", i),
			armTask(fmt.Sprintf("e%02d:S:T1", i), "T1", 400_000),
			armTask(fmt.Sprintf("e%02d:S:T2", i), "T2", 400_000))
	}
	var mu sync.Mutex
	seen := make(map[string]int)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := fx.sts.ClaimDue(bg, spi.ClaimRequest{Owner: uuid.New(), NowMs: 2_000_000, StaleAfter: time.Minute, Limit: 100, PerTenantLimit: 100})
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
