package postgres_test

// fcw_test.go — end-to-end first-committer-wins (FCW) concurrency tests.
//
// These tests exercise the SI + FCW mechanisms end-to-end against a real
// PostgreSQL instance (provided by CYODA_TEST_DB_URL / testcontainers-go
// in CI). Each test is completely self-contained: it creates its own pool,
// runs migrations, seeds data, exercises concurrency, and cleans up.
//
// Conflict detection mechanisms (see DESIGN NOTE in the task brief):
//   - Read-set staleness: readSet validated via FOR SHARE at commit time.
//   - Write-write: caught by postgres native tuple-level DML locks (SQLSTATE 40001).

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// setupFCWTest creates an isolated pool + schema + wired StoreFactory+TM.
// The schema is dropped on t.Cleanup.
func setupFCWTest(t *testing.T) (*postgres.StoreFactory, *postgres.TransactionManager) {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	uuids := newTestUUIDGenerator()
	tm := postgres.NewTransactionManager(pool, uuids)
	factory := postgres.NewStoreFactoryWithTMForTest(pool, tm)
	return factory, tm
}

// ---------------------------------------------------------------------------
// Test 1: Regression — disjoint concurrent inserts must not conflict (#17)
// ---------------------------------------------------------------------------

// TestFCW_DisjointConcurrentInserts_NoFalseConflicts is a regression guard for
// issue #17. Under SSI, page-level SIReadLocks caused serialization failures
// (~40001) for concurrent inserts of distinct UUIDs into the same tenant after
// the b-tree had ≥200 rows. Under SI + FCW, disjoint inserts must all commit.
//
// Run with -count=5 for stability verification:
//
//	go test ./plugins/postgres/ -run TestFCW_DisjointConcurrentInserts -v -count=5
func TestFCW_DisjointConcurrentInserts_NoFalseConflicts(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("fcw-tenant-1")

	// Seed 200 rows to fill the b-tree (mirrors #17 reproducer).
	seedStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	for i := 0; i < 200; i++ {
		e := makeEntity(fmt.Sprintf("seed-%04d", i))
		if _, err := seedStore.Save(ctx, e); err != nil {
			t.Fatalf("seed Save %d: %v", i, err)
		}
	}

	// 8 goroutines each insert one unique entity in its own transaction.
	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			txID, txCtx, err := tm.Begin(ctx)
			if err != nil {
				errs <- fmt.Errorf("worker %d Begin: %w", i, err)
				return
			}
			txStore, err := factory.EntityStore(txCtx)
			if err != nil {
				_ = tm.Rollback(txCtx, txID)
				errs <- fmt.Errorf("worker %d EntityStore: %w", i, err)
				return
			}
			e := makeEntity(fmt.Sprintf("concurrent-%d", i))
			if _, err := txStore.Save(txCtx, e); err != nil {
				_ = tm.Rollback(txCtx, txID)
				errs <- fmt.Errorf("worker %d Save: %w", i, err)
				return
			}
			if err := tm.Commit(ctx, txID); err != nil {
				errs <- fmt.Errorf("worker %d Commit: %w", i, err)
				return
			}
			errs <- nil
		}()
	}
	wg.Wait()
	close(errs)

	for e := range errs {
		if e != nil {
			t.Errorf("unexpected error (expected zero conflicts for disjoint inserts): %v", e)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 2: Same-entity race — first committer wins
// ---------------------------------------------------------------------------

// TestFCW_SameEntityUpdate_FirstCommitterWins verifies that when two
// transactions both read the same entity and then write it, exactly one
// succeeds and the other gets spi.ErrConflict.
//
// Conflict detection mechanism: The second committer's readSet validation
// detects the version change committed by the first committer. Under
// REPEATABLE READ (SI), two concurrent UPDATEs on the same row will be
// serialized by postgres's tuple-level locks — the second UPDATE blocks
// until the first commits (then either proceeds or gets 40001). Both
// mechanisms (readSet staleness + DB-level 40001) map to spi.ErrConflict.
//
// Design note: we do NOT use a commit barrier here. Under REPEATABLE READ,
// one goroutine's Save (UPDATE) will block on the row lock until the other's
// transaction ends. We let the two goroutines race naturally; one of them
// either gets 40001 from the UPDATE itself or ErrConflict from the readSet
// validation at commit. Both outcomes satisfy the FCW contract.
func TestFCW_SameEntityUpdate_FirstCommitterWins(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("fcw-tenant-2")

	// Seed shared entity at v=1.
	seedStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	e := makeEntity("shared")
	if _, err := seedStore.Save(ctx, e); err != nil {
		t.Fatalf("seed shared: %v", err)
	}

	// Tx A and Tx B: begin both, read shared, then write + commit concurrently.
	// Both transactions start before any commit so both see v=1 in their snapshot.
	txIDA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	txIDB, txCtxB, err := tm.Begin(ctx)
	if err != nil {
		_ = tm.Rollback(txCtxA, txIDA)
		t.Fatalf("Tx B Begin: %v", err)
	}

	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		_ = tm.Rollback(txCtxA, txIDA)
		_ = tm.Rollback(txCtxB, txIDB)
		t.Fatalf("Tx A EntityStore: %v", err)
	}
	storeB, err := factory.EntityStore(txCtxB)
	if err != nil {
		_ = tm.Rollback(txCtxA, txIDA)
		_ = tm.Rollback(txCtxB, txIDB)
		t.Fatalf("Tx B EntityStore: %v", err)
	}

	// Both read shared while in their snapshot — both see v=1.
	gotA, err := storeA.Get(txCtxA, "shared")
	if err != nil {
		_ = tm.Rollback(txCtxA, txIDA)
		_ = tm.Rollback(txCtxB, txIDB)
		t.Fatalf("Tx A Get: %v", err)
	}
	gotB, err := storeB.Get(txCtxB, "shared")
	if err != nil {
		_ = tm.Rollback(txCtxA, txIDA)
		_ = tm.Rollback(txCtxB, txIDB)
		t.Fatalf("Tx B Get: %v", err)
	}

	// Now commit-race them concurrently. Under REPEATABLE READ the two
	// UPDATEs contend on the same row; exactly one will win.
	type result struct{ err error }
	results := make(chan result, 2)

	go func() {
		gotA.Meta.State = "UPDATED_BY_A"
		if _, err := storeA.Save(txCtxA, gotA); err != nil {
			_ = tm.Rollback(txCtxA, txIDA)
			results <- result{err}
			return
		}
		results <- result{tm.Commit(ctx, txIDA)}
	}()
	go func() {
		gotB.Meta.State = "UPDATED_BY_B"
		if _, err := storeB.Save(txCtxB, gotB); err != nil {
			_ = tm.Rollback(txCtxB, txIDB)
			results <- result{err}
			return
		}
		results <- result{tm.Commit(ctx, txIDB)}
	}()

	var successes, conflicts int
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			successes++
		} else if errors.Is(r.err, spi.ErrConflict) {
			conflicts++
		} else {
			t.Errorf("unexpected non-conflict error: %v", r.err)
		}
	}

	if successes != 1 || conflicts != 1 {
		t.Errorf("expected successes=1 conflicts=1, got successes=%d conflicts=%d",
			successes, conflicts)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Read-then-concurrent-delete conflict
// ---------------------------------------------------------------------------

// TestFCW_ReadThenConcurrentDelete_Conflict verifies that a commit is
// rejected when another transaction has deleted an entity that this
// transaction read (the readSet version is bumped by the soft-delete).
//
// Scenario (sequential, not concurrent):
//  1. Tx A: Begin, Get e1 (readSet captures v=1).
//  2. Tx B: Begin, Delete e1, Commit (marks deleted, bumps to v=2).
//  3. Tx A: Commit → ErrConflict (readSet v=1 vs current v=2).
func TestFCW_ReadThenConcurrentDelete_Conflict(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("fcw-tenant-3")

	// Seed e1 at v=1.
	seedStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	if _, err := seedStore.Save(ctx, makeEntity("e1")); err != nil {
		t.Fatalf("seed e1: %v", err)
	}

	// Tx A: Begin and read e1.
	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtxA, txA) }()

	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("Tx A EntityStore: %v", err)
	}
	got, err := storeA.Get(txCtxA, "e1")
	if err != nil {
		t.Fatalf("Tx A Get e1: %v", err)
	}
	if got.Meta.Version != 1 {
		t.Fatalf("expected e1@v1, got v%d", got.Meta.Version)
	}

	// Tx B: Begin, Delete e1, Commit (bumps version to 2, marks deleted).
	txB, txCtxB, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx B Begin: %v", err)
	}
	storeB, err := factory.EntityStore(txCtxB)
	if err != nil {
		t.Fatalf("Tx B EntityStore: %v", err)
	}
	if err := storeB.Delete(txCtxB, "e1"); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Delete e1: %v", err)
	}
	if err := tm.Commit(ctx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Tx A: Commit must fail with ErrConflict.
	err = tm.Commit(ctx, txA)
	if err == nil {
		t.Fatal("expected ErrConflict on Tx A commit, got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("expected ErrConflict, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Large read-set chunked validation
// ---------------------------------------------------------------------------

// TestFCW_LargeReadSet_ChunkedValidation verifies that a transaction with
// 2500+ readSet entries commits successfully via multiple FOR SHARE chunks
// within 5 seconds, when no concurrent modifications have occurred.
func TestFCW_LargeReadSet_ChunkedValidation(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("fcw-tenant-4")

	const entityCount = 2500

	// Seed 2500 entities.
	seedStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	for i := 0; i < entityCount; i++ {
		e := makeEntity(fmt.Sprintf("large-%04d", i))
		if _, err := seedStore.Save(ctx, e); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Begin tx and manually populate readSet with all 2500 entities at v=1.
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()

	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	for i := 0; i < entityCount; i++ {
		state.RecordRead(fmt.Sprintf("large-%04d", i), 1)
	}

	// Commit — must succeed with no concurrent modifications; assert < 5s.
	start := time.Now()
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit with large readSet failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("large readSet commit took %v, want < 5s", elapsed)
	}
	t.Logf("large readSet (%d entities) commit elapsed: %v", entityCount, elapsed)
}

// ---------------------------------------------------------------------------
// Test 5: Savepoint rollback drops read-set entries, commit succeeds
// ---------------------------------------------------------------------------

// TestFCW_SavepointRollback_DropsReadSet_CommitSucceeds verifies that after a
// rollback-to-savepoint, entity IDs added to the readSet after the savepoint
// are dropped. A concurrent modification to those dropped entities must not
// cause a conflict on the outer transaction's commit.
//
// Scenario:
//  1. Tx A: Begin, Get x (readSet: {x:1}).
//  2. Tx A: Savepoint sp (snapshot: {x:1}).
//  3. Tx A: Get y (readSet: {x:1, y:1}).
//  4. Tx B: Begin, Delete y, Commit (y→v2 deleted).
//  5. Tx A: RollbackToSavepoint sp → readSet restored to {x:1}; y dropped.
//  6. Tx A: Commit → SUCCEEDS (x unchanged; y not in readSet).
func TestFCW_SavepointRollback_DropsReadSet_CommitSucceeds(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("fcw-tenant-5")

	// Seed x and y at v=1.
	seedStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	for _, id := range []string{"sp-x", "sp-y"} {
		if _, err := seedStore.Save(ctx, makeEntity(id)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Tx A: Begin.
	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtxA, txA) }()

	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("Tx A EntityStore: %v", err)
	}

	// Step 1: Get x — readSet captures {sp-x: 1}.
	if _, err := storeA.Get(txCtxA, "sp-x"); err != nil {
		t.Fatalf("Tx A Get sp-x: %v", err)
	}

	// Verify readSet has x.
	stateA, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState not found")
	}
	if v := postgres.ReadSetVersionForTest(stateA, "sp-x"); v != 1 {
		t.Fatalf("expected readSet[sp-x]=1, got %d", v)
	}

	// Step 2: Savepoint.
	spID, err := tm.Savepoint(txCtxA, txA)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	// Step 3: Get y — readSet becomes {sp-x: 1, sp-y: 1}.
	if _, err := storeA.Get(txCtxA, "sp-y"); err != nil {
		t.Fatalf("Tx A Get sp-y: %v", err)
	}
	if v := postgres.ReadSetVersionForTest(stateA, "sp-y"); v != 1 {
		t.Fatalf("expected readSet[sp-y]=1 before rollback, got %d", v)
	}

	// Step 4: Tx B deletes y and commits (y's version becomes 2, deleted=true).
	txB, txCtxB, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx B Begin: %v", err)
	}
	storeB, err := factory.EntityStore(txCtxB)
	if err != nil {
		t.Fatalf("Tx B EntityStore: %v", err)
	}
	if err := storeB.Delete(txCtxB, "sp-y"); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Delete sp-y: %v", err)
	}
	if err := tm.Commit(ctx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Step 5: RollbackToSavepoint — y drops from readSet; x stays.
	if err := tm.RollbackToSavepoint(txCtxA, txA, spID); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}

	if v := postgres.ReadSetVersionForTest(stateA, "sp-y"); v != 0 {
		t.Errorf("sp-y should be absent from readSet after rollback, got version %d", v)
	}
	if v := postgres.ReadSetVersionForTest(stateA, "sp-x"); v != 1 {
		t.Errorf("sp-x should remain in readSet at v1 after rollback, got %d", v)
	}

	// Step 6: Tx A commits — must succeed because y is no longer validated.
	if err := tm.Commit(ctx, txA); err != nil {
		t.Errorf("Tx A Commit must succeed after savepoint rollback dropped y: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 6: Pure readSet-only conflict — no write-write overlap
// ---------------------------------------------------------------------------

// TestFCW_PureReadSetConflict_NoWriteOverlap proves that readSet validation
// catches staleness even when the conflicting entity has no write-write overlap.
//
// Scenario:
//  1. Seed x at v=1 and y at v=1.
//  2. Tx A: Begin, Get x (readSet: {x:1}), Save y (writeSet: {y:1}).
//  3. Tx B: Begin, update x to v=2 via raw SQL, Commit.
//  4. Tx A: Commit → spi.ErrConflict (readSet[x]=1 vs current x=2).
//
// Without FCW readSet validation this would silently commit: PostgreSQL DML
// locks only catch write-write contention on the same row; y≠x, so no DML
// conflict arises. The only protection is the application-layer readSet check.
func TestFCW_PureReadSetConflict_NoWriteOverlap(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("fcw-tenant-6")

	// Step 1: Seed x and y at v=1.
	seedStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	for _, id := range []string{"prs-x", "prs-y"} {
		if _, err := seedStore.Save(ctx, makeEntity(id)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Step 2: Tx A: Begin, Get x (records x in readSet), Save y (records y in writeSet).
	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtxA, txA) }()

	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("Tx A EntityStore: %v", err)
	}

	gotX, err := storeA.Get(txCtxA, "prs-x")
	if err != nil {
		t.Fatalf("Tx A Get prs-x: %v", err)
	}
	if gotX.Meta.Version != 1 {
		t.Fatalf("expected prs-x@v1, got v%d", gotX.Meta.Version)
	}

	// Verify x is captured in the readSet.
	stateA, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState A not found")
	}
	if v := postgres.ReadSetVersionForTest(stateA, "prs-x"); v != 1 {
		t.Fatalf("expected readSet[prs-x]=1, got %d", v)
	}

	// Save y — puts y into writeSet; prs-x must NOT appear in writeSet.
	gotY, err := storeA.Get(txCtxA, "prs-y")
	if err != nil {
		t.Fatalf("Tx A Get prs-y (pre-save): %v", err)
	}
	gotY.Meta.State = "UPDATED_BY_A"
	if _, err := storeA.Save(txCtxA, gotY); err != nil {
		t.Fatalf("Tx A Save prs-y: %v", err)
	}
	if _, inWrite := postgres.WriteSetVersionForTest(stateA, "prs-x"); inWrite {
		t.Fatal("prs-x must not be in Tx A writeSet — it was only read, not written")
	}

	// Step 3: Tx B: Begin, update x to v=2 via raw SQL, Commit.
	txB, txCtxB, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Tx B Begin: %v", err)
	}
	pgxTxB, _ := tm.LookupTx(txB)
	if _, err := pgxTxB.Exec(txCtxB,
		`UPDATE entities SET version=2 WHERE tenant_id='fcw-tenant-6' AND entity_id='prs-x'`); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B update prs-x: %v", err)
	}
	// Use ctx (which carries the test tenant) so the post-#199 PR-C2 tenant
	// gate accepts this commit. context.Background() has no UserContext and
	// would be rejected with a "tenant mismatch" error.
	if err := tm.Commit(ctx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Step 4: Tx A Commit must fail with ErrConflict.
	// readSet[prs-x]=1 but current version is 2; y has no overlap with B's writes.
	err = tm.Commit(ctx, txA)
	if err == nil {
		t.Fatal("expected ErrConflict on Tx A commit (stale readSet[prs-x]), got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("expected spi.ErrConflict, got: %v", err)
	}
}
