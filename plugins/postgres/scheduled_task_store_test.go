package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// newTaskStore is a migrated schema, a factory with a transaction manager, and
// its scheduled-task store. maxConns sizes the main pool.
func newTaskStore(t *testing.T, maxConns int32) (*postgres.StoreFactory, spi.ScheduledTaskStore) {
	t.Helper()
	pool := newTestPoolSized(t, maxConns)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	return f, sts
}

func taskSpec(tenant spi.TenantID, entityID, state, transition string, scheduledTime int64) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID: entityID + ":" + state + ":" + transition, TenantID: tenant,
		Type: spi.ScheduledTaskFireTransition, ScheduledTime: scheduledTime,
		EntityID: entityID, ModelName: "M", ModelVersion: 1, SourceState: state, Transition: transition,
	}
}

// arm arms one entity's tasks outside any transaction, so the arm commits.
func arm(t *testing.T, sts spi.ScheduledTaskStore, tenant spi.TenantID, entityID, state string, tasks ...spi.ScheduledTask) {
	t.Helper()
	if _, err := sts.ReconcileForEntity(context.Background(), spi.ReconcileRequest{
		TenantID: tenant, EntityID: entityID, CurrentState: state, Arm: tasks,
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
}

func claimRequest(owner uuid.UUID) spi.ClaimRequest {
	return spi.ClaimRequest{Owner: owner, NowMs: time.Now().UnixMilli(), StaleAfter: time.Minute, Limit: 10, PerTenantLimit: 10}
}

func claimAll(t *testing.T, sts spi.ScheduledTaskStore) []spi.ScheduledTask {
	t.Helper()
	got, err := sts.ClaimDue(context.Background(), claimRequest(uuid.New()))
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	return got
}

func refOf(task spi.ScheduledTask) spi.TaskRef {
	return spi.TaskRef{TenantID: task.TenantID, ID: task.ID, ArmToken: task.ArmToken, ClaimToken: task.Claim.Token}
}

// beginEntityTx opens an entity transaction (REPEATABLE READ, snapshot taken
// inside Begin) and returns its context and an early rollback.
func beginEntityTx(t *testing.T, f *postgres.StoreFactory, tenant spi.TenantID) (context.Context, func()) {
	t.Helper()
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx := postgres.BeginGuardedForTest(t, tm, ctxWithTenant(tenant))
	return txCtx, func() {
		if err := tm.Rollback(txCtx, txID); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// An arm issued inside an entity transaction is part of it.
func TestPostgres_ScheduledTaskArm_RollbackIsAtomic(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	if _, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: "tenant-A", EntityID: "e1", CurrentState: "S",
		Arm: []spi.ScheduledTask{taskSpec("tenant-A", "e1", "S", "T", 1000)},
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	rollback()
	if _, found, err := sts.Get(context.Background(), "tenant-A", "e1:S:T"); err != nil || found {
		t.Fatalf("after rollback: found=%v err=%v, want absent", found, err)
	}
}

// ClaimDue is cross-tenant: one call claims every tenant's due tasks.
func TestPostgres_ScheduledTaskStore_ClaimDueIsCrossTenant(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	for _, tenant := range []spi.TenantID{"tenant-A", "tenant-B", "tenant-C"} {
		arm(t, sts, tenant, "e", "S", taskSpec(tenant, "e", "S", "T", 1000))
	}
	seen := map[spi.TenantID]bool{}
	for _, task := range claimAll(t, sts) {
		seen[task.TenantID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("ClaimDue claimed tenants %v, want all three", seen)
	}
}

// None of the scheduler's tables is under row-level security: ClaimDue,
// GiveBackIdle, the owner methods and the sweeps are cross-tenant.
func TestPostgres_ScheduledTaskTables_NotRLSEnrolled(t *testing.T) {
	f, _ := newTaskStore(t, 5)
	pool := postgres.PoolForTest(f)
	for _, table := range []string{"scheduled_tasks", "scheduled_task_marks", "scheduler_owners"} {
		var rls bool
		if err := pool.QueryRow(context.Background(),
			`SELECT relrowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&rls); err != nil {
			t.Fatalf("read relrowsecurity for %s: %v", table, err)
		}
		if rls {
			t.Errorf("%s has row-level security enabled; the scheduler's cross-tenant methods read it with no tenant set", table)
		}
	}
}

// Every tenant-facing method filters on the tenant it is given.
func TestPostgres_ScheduledTaskStore_TenantFacingMethodsFilterOnTenant(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	ctx := context.Background()
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	claimed := claimAll(t, sts)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d tasks, want 1", len(claimed))
	}
	a := claimed[0]
	wrong := refOf(a)
	wrong.TenantID = "tenant-B"

	if _, found, err := sts.Get(ctx, "tenant-B", a.ID); err != nil || found {
		t.Errorf("Get as tenant-B: found=%v err=%v", found, err)
	}
	fenced := []struct {
		name string
		call func() error
	}{
		{"StampSegment", func() error { return sts.StampSegment(ctx, wrong, true) }},
		{"MarkUnsafe", func() error { return sts.MarkUnsafe(ctx, wrong) }},
		{"RecordAttempt", func() error {
			return sts.RecordAttempt(ctx, wrong, spi.Attempt{Error: "x", AtMs: 1, NextAttemptTime: 1, ClearOwnMark: true})
		}},
		{"Fail", func() error {
			return sts.Fail(ctx, wrong, spi.Failure{Reason: spi.FailureRunPanicked, Error: "x", AtMs: 1})
		}},
	}
	for _, c := range fenced {
		if err := c.call(); !errors.Is(err, spi.ErrStaleClaim) {
			t.Errorf("%s as tenant-B: err = %v, want ErrStaleClaim", c.name, err)
		}
	}
	if err := sts.RemoveLife(ctx, "tenant-B", a.ID, a.ArmToken); err != nil {
		t.Errorf("RemoveLife as tenant-B: %v", err)
	}
	if err := sts.DeleteForEntities(ctx, "tenant-B", []string{"e1"}); err != nil {
		t.Errorf("DeleteForEntities as tenant-B: %v", err)
	}
	if err := sts.DeleteForModel(ctx, "tenant-B", "M", 1, nil); err != nil {
		t.Errorf("DeleteForModel as tenant-B: %v", err)
	}
	if removed, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: "tenant-B", EntityID: "e1", CurrentState: "S",
	}); err != nil || len(removed) != 0 {
		t.Errorf("ReconcileForEntity as tenant-B: removed=%v err=%v", removed, err)
	}
	if page, err := sts.Query(ctx, "tenant-B", spi.ScheduledTaskQuery{Limit: 10}); err != nil || len(page.Items) != 0 {
		t.Errorf("Query as tenant-B: items=%v err=%v", page.Items, err)
	}

	got, found, err := sts.Get(ctx, "tenant-A", a.ID)
	if err != nil || !found {
		t.Fatalf("tenant-A's task was removed through tenant-B: found=%v err=%v", found, err)
	}
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != a.Claim.Token ||
		got.PartialCommit || got.UnsafeMarked || got.LastError != "" {
		t.Errorf("tenant-A's task was changed through tenant-B: %+v", got)
	}
}

// A joining write whose tenant is not the tenant of the transaction on ctx is
// refused before any statement runs, as on memory and SQLite: a task row of
// tenant B never enters tenant A's transaction.
func TestPostgres_ScheduledTaskStore_JoiningWriteOfAnotherTenantRefused(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-B", "e1", "S", taskSpec("tenant-B", "e1", "S", "T", 1000))
	claimed := claimAll(t, sts)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d tasks, want 1", len(claimed))
	}
	b := claimed[0]
	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	defer rollback()

	joining := []struct {
		name string
		call func() error
	}{
		{"ReconcileForEntity", func() error {
			_, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
				TenantID: "tenant-B", EntityID: "e1", CurrentState: "S",
				Arm: []spi.ScheduledTask{taskSpec("tenant-B", "e1", "S", "T", 1000)},
			})
			return err
		}},
		{"RemoveLife", func() error { return sts.RemoveLife(txCtx, "tenant-B", b.ID, b.ArmToken) }},
		{"StampSegment", func() error { return sts.StampSegment(txCtx, refOf(b), true) }},
		{"DeleteForEntities", func() error { return sts.DeleteForEntities(txCtx, "tenant-B", []string{"e1"}) }},
		{"DeleteForModel", func() error { return sts.DeleteForModel(txCtx, "tenant-B", "M", 1, nil) }},
		{"Fail", func() error {
			return sts.Fail(txCtx, refOf(b), spi.Failure{Reason: spi.FailureRunPanicked, Error: "x", AtMs: 1})
		}},
	}
	for _, c := range joining {
		if err := c.call(); !errors.Is(err, spi.ErrTxTenantMismatch) {
			t.Errorf("%s for tenant-B in tenant-A's transaction: err = %v, want ErrTxTenantMismatch", c.name, err)
		}
	}
	got := mustGet(t, sts, "tenant-B", b.ID)
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != b.Claim.Token || got.PartialCommit {
		t.Errorf("tenant-B's task was changed through tenant-A's transaction: %+v", got)
	}
}

// C1 and C5: a task row that a claim changed after the entity transaction's
// snapshot fails the transaction's write with ErrConflict, at the statement.
func TestPostgres_ScheduledTaskStore_RowChangedAfterSnapshotIsErrConflict(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	before := mustGet(t, sts, "tenant-A", "e1:S:T")

	txCtx, _ := beginEntityTx(t, f, "tenant-A")
	if len(claimAll(t, sts)) != 1 {
		t.Fatal("the claim did not take the task")
	}
	err := sts.RemoveLife(txCtx, "tenant-A", before.ID, before.ArmToken)
	if !errors.Is(err, spi.ErrConflict) || sqlState(err) != pgerrcode.SerializationFailure {
		t.Fatalf("RemoveLife after a concurrent claim: err = %v, want ErrConflict over 40001", err)
	}
}

// C6: a row an open transaction wrote answers ErrTaskBusy at once (NOWAIT).
func TestPostgres_MarkUnsafe_RowWrittenByAnOpenTxIsBusyAtOnce(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	task := claimAll(t, sts)[0]
	txCtx, _ := beginEntityTx(t, f, "tenant-A")
	if err := sts.StampSegment(txCtx, refOf(task), false); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	start := time.Now()
	err := sts.MarkUnsafe(context.Background(), refOf(task))
	if !errors.Is(err, spi.ErrTaskBusy) {
		t.Fatalf("MarkUnsafe: err = %v, want ErrTaskBusy", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("MarkUnsafe waited %s; NOWAIT must answer at once", elapsed)
	}
}

// A bookkeeping write blocked by a row lock gives up at lock_timeout and
// answers ErrTaskBusy over the 55P03 (C6) — a retryable error, not a refusal
// and not a rejection — and makes no write.
func TestPostgres_ScheduledTaskStore_RowLockWaitEndsAtLockTimeout(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	task := claimAll(t, sts)[0]
	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	if err := sts.StampSegment(txCtx, refOf(task), false); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	attempt := spi.Attempt{Error: "x", AtMs: 1, NextAttemptTime: 1}
	start := time.Now()
	err := sts.RecordAttempt(context.Background(), refOf(task), attempt)
	elapsed := time.Since(start)
	if !errors.Is(err, spi.ErrTaskBusy) || sqlState(err) != pgerrcode.LockNotAvailable {
		t.Fatalf("RecordAttempt under a row lock: err = %v, want ErrTaskBusy over 55P03", err)
	}
	if errors.Is(err, spi.ErrStaleClaim) || errors.Is(err, spi.ErrConflict) || errors.Is(err, spi.ErrStoreRejected) {
		t.Errorf("55P03 carries a refusal or rejection sentinel: %v", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("gave up after %s, want about the 2s lock_timeout", elapsed)
	}
	if got := mustGet(t, sts, "tenant-A", task.ID); got.Status != spi.ScheduledTaskRunning || got.Attempts != 0 {
		t.Errorf("a busy RecordAttempt wrote the row: %+v", got)
	}
	rollback()
	if err := sts.RecordAttempt(context.Background(), refOf(task), attempt); err != nil {
		t.Fatalf("RecordAttempt after the lock was released: %v", err)
	}
}

// C4: with every main-pool connection held by an entity transaction, the
// scheduler's never-joining methods still run.
func TestPostgres_ScheduledTaskStore_NotStarvedByAnExhaustedMainPool(t *testing.T) {
	f, sts := newTaskStore(t, 2)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	for i := 0; i < 2; i++ {
		beginEntityTx(t, f, "tenant-A")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	owner := uuid.New()
	if err := sts.Heartbeat(ctx, owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	claimed, err := sts.ClaimDue(ctx, claimRequest(owner))
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: claimed=%d err=%v", len(claimed), err)
	}
	if err := sts.MarkUnsafe(ctx, refOf(claimed[0])); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := sts.RecordAttempt(ctx, refOf(claimed[0]),
		spi.Attempt{Error: "x", AtMs: now, NextAttemptTime: now, ClearOwnMark: true}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if n, err := sts.GiveBackIdle(ctx, owner, nil); err != nil || n != 0 {
		t.Fatalf("GiveBackIdle: n=%d err=%v", n, err)
	}
}

// Heartbeat has a connection of its own: a saturated scheduler pool does not
// delay it.
func TestPostgres_ScheduledTaskHeartbeat_HasItsOwnConnection(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	work := postgres.SchedulerPoolForTest(t, f)
	for i := int32(0); i < work.Config().MaxConns; i++ {
		c, err := work.Acquire(context.Background())
		if err != nil {
			t.Fatalf("hold scheduler connection %d: %v", i, err)
		}
		t.Cleanup(c.Release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sts.Heartbeat(ctx, uuid.New()); err != nil {
		t.Fatalf("Heartbeat with the scheduler pool saturated: %v", err)
	}
}

// V5: a claim that loses a race for a sibling task rolls back and claims
// nothing, within lock_timeout, whether the rival commits or stalls.
func TestPostgres_ClaimDue_LosingASiblingRaceClaimsNothing(t *testing.T) {
	cases := []struct {
		name       string
		rivalHolds time.Duration
		within     time.Duration
	}{
		{"rival commits while the claim waits", 300 * time.Millisecond, 1500 * time.Millisecond},
		{"rival outlasts lock_timeout", 4 * time.Second, 3500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, sts := newTaskStore(t, 5)
			bg := context.Background()
			// T2 is due first, so the claim ranks T2.
			arm(t, sts, "tenant-A", "e1", "S",
				taskSpec("tenant-A", "e1", "S", "T1", 1000),
				taskSpec("tenant-A", "e1", "S", "T2", 500))

			// Another pnode has claimed T1 and not committed. The ranking cannot
			// see it, so the claim takes T2 and waits on the unique index.
			rival, err := postgres.PoolForTest(f).Begin(bg)
			if err != nil {
				t.Fatalf("begin rival: %v", err)
			}
			if _, err := rival.Exec(bg, `UPDATE scheduled_tasks
				SET status = 'RUNNING', claim_token = gen_random_uuid(), claim_owner = gen_random_uuid()
				WHERE id = 'e1:S:T1'`); err != nil {
				t.Fatalf("rival claim: %v", err)
			}
			rivalDone := make(chan error, 1)
			go func() {
				time.Sleep(tc.rivalHolds)
				rivalDone <- rival.Commit(context.Background())
			}()

			start := time.Now()
			claimed, claimErr := sts.ClaimDue(bg, claimRequest(uuid.New()))
			elapsed := time.Since(start)
			if err := <-rivalDone; err != nil {
				t.Fatalf("rival commit: %v", err)
			}
			if claimErr != nil {
				t.Fatalf("ClaimDue lost the race and returned an error: %v", claimErr)
			}
			if len(claimed) != 0 {
				t.Fatalf("ClaimDue claimed %d tasks, want none", len(claimed))
			}
			if elapsed > tc.within {
				t.Errorf("ClaimDue took %s, want under %s", elapsed, tc.within)
			}
			if got := mustGet(t, sts, "tenant-A", "e1:S:T2"); got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
				t.Errorf("T2 after the lost race = %+v, want WAITING and unclaimed", got)
			}
		})
	}
}

// Tenants keep their order in every round, as spi.SelectClaims orders them:
// the tenant with the earliest candidate goes first in round two as well,
// even when another tenant's second task is due before its own.
func TestPostgres_ClaimDue_TenantOrderHoldsEveryRound(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "a1", "S", taskSpec("tenant-A", "a1", "S", "T", 1))
	arm(t, sts, "tenant-A", "a2", "S", taskSpec("tenant-A", "a2", "S", "T", 100))
	arm(t, sts, "tenant-B", "b1", "S", taskSpec("tenant-B", "b1", "S", "T", 2))
	arm(t, sts, "tenant-B", "b2", "S", taskSpec("tenant-B", "b2", "S", "T", 3))

	req := claimRequest(uuid.New())
	req.Limit = 3
	claimed, err := sts.ClaimDue(context.Background(), req)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	got := map[string]bool{}
	for _, task := range claimed {
		got[task.ID] = true
	}
	want := map[string]bool{"a1:S:T": true, "b1:S:T": true, "a2:S:T": true}
	if len(got) != len(want) {
		t.Fatalf("claimed %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("claimed %v, want %v: tenant-A's second turn comes before tenant-B's", got, want)
		}
	}
}

// A RemoveLife that names a life already replaced when its transaction took
// its snapshot removes nothing and takes no lock: the row stays claimable and
// markable while the transaction is open (C6).
func TestPostgres_RemoveLife_OfAReplacedLifeLeavesTheRowFree(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	old := mustGet(t, sts, "tenant-A", "e1:S:T")
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))

	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	defer rollback()
	if err := sts.RemoveLife(txCtx, "tenant-A", old.ID, old.ArmToken); err != nil {
		t.Fatalf("RemoveLife of a replaced life: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claimed, err := sts.ClaimDue(ctx, claimRequest(uuid.New()))
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue while the transaction is open: claimed=%d err=%v, want the task", len(claimed), err)
	}
	if err := sts.MarkUnsafe(ctx, refOf(claimed[0])); err != nil {
		t.Fatalf("MarkUnsafe while the transaction is open: %v", err)
	}
}

// Two tenants' tasks may share an id. One claim that takes both reports each
// task's own mark and its own lost-owner flag.
func TestPostgres_ClaimDue_SameIDInTwoTenantsKeepsItsOwnFlags(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	ctx := context.Background()
	arm(t, sts, "tenant-A", "e", "S", taskSpec("tenant-A", "e", "S", "T", 1000))
	first := claimAll(t, sts) // this owner never heartbeats: lost to the next lost-owner claim
	if len(first) != 1 {
		t.Fatalf("claimed %d tasks, want tenant-A's", len(first))
	}
	if err := sts.MarkUnsafe(ctx, refOf(first[0])); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	arm(t, sts, "tenant-B", "e", "S", taskSpec("tenant-B", "e", "S", "T", 1000))

	req := claimRequest(uuid.New())
	req.AllowLostOwner = true
	claimed, err := sts.ClaimDue(ctx, req)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ClaimDue: claimed=%d err=%v, want both tenants' tasks", len(claimed), err)
	}
	for _, task := range claimed {
		lost := task.TenantID == "tenant-A"
		if task.ClaimedFromLostOwner != lost || task.UnsafeMarked != lost {
			t.Errorf("%s's task: ClaimedFromLostOwner=%v UnsafeMarked=%v, want %v and %v",
				task.TenantID, task.ClaimedFromLostOwner, task.UnsafeMarked, lost, lost)
		}
	}
}
