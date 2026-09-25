package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// deleteRacers is the number of concurrent Deletes of the same entity both
// tests below fire at once. deleteOn reads entities.version before this
// task's fix takes any lock, so every racer that starts before the first one
// commits reads the SAME version and computes the SAME next entity_versions
// row — with enough racers dispatched from a closed start channel, the DB
// round trip for that first read is slower than the goroutine dispatch skew,
// so every racer is guaranteed to have read before any of them can possibly
// have committed (committing requires finishing deleteOn first, which is
// exactly what the contested INSERT/lock blocks). The outcome is therefore
// deterministic, not merely probable.
const deleteRacers = 8

// warmPool forces the pool to open n physical connections and hand them back
// idle before the timed race starts, so the race is not skewed by TCP/auth
// handshake latency on a cold connection: without this, a cold connection's
// goroutine routinely reaches its own read only after a warm one has already
// committed, starving the race of the simultaneity it needs to reproduce the
// entity_versions_pkey defect reliably.
func warmPool(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var one int
			if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
				t.Errorf("warm pool: %v", err)
			}
		}()
	}
	wg.Wait()
}

// classifyDeleteRace fails the test if any racer's error is anything other
// than nil, spi.ErrConflict or spi.ErrNotFound — in particular, it fails if
// one carries spi.ErrStoreRejected (entity_versions_pkey is a genuine,
// retryable race, never a deterministic rejection) or an unclassified raw
// error (the pre-fix defect: a 500 with no SPI meaning at all). Exactly one
// racer must win.
func classifyDeleteRace(t *testing.T, results []error) {
	t.Helper()
	wins := 0
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, spi.ErrConflict), errors.Is(err, spi.ErrNotFound):
			// expected loser outcomes
		default:
			t.Fatalf("racer %d: unexpected error (want nil, ErrConflict or ErrNotFound): %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("got %d winners, want exactly 1 (results: %v)", wins, results)
	}
}

// TestPostgres_DeleteConcurrent_NonTx_OneWinner is the isolated,
// single-backend reproduction of the entity_versions_pkey race: deleteOn used
// to read entities.version, then INSERT the computed next entity_versions
// row, and only THEN take the entities row lock (its own UPDATE) — so two
// concurrent non-transactional Deletes of the same entity could both compute
// the same next version and race each other into entity_versions' primary
// key. Before this task's fix that 23505 reached the caller unclassified;
// after BP-5's SQLSTATE-class marking (with no exception for it) it would
// have been mislabelled spi.ErrStoreRejected instead — a deterministic
// rejection latches the scheduler's owning node, which is wrong for a race
// any retry clears. Neither may happen: every loser gets spi.ErrConflict or
// spi.ErrNotFound, and exactly one Delete wins.
func TestPostgres_DeleteConcurrent_NonTx_OneWinner(t *testing.T) {
	// A pool sized to deleteRacers, warmed below, so every racer's own
	// internal transaction acquires an already-open connection instead of
	// paying a cold TCP/auth handshake — see warmPool.
	pool := newTestPoolSized(t, deleteRacers+2)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	factory := postgres.NewStoreFactory(pool)
	const tenant spi.TenantID = "tenant-delete-race"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-delete-race", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const id = "e-delete-race-nontx"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: ref},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	warmPool(t, postgres.PoolForTest(factory), deleteRacers)

	var wg sync.WaitGroup
	results := make([]error, deleteRacers)
	start := make(chan struct{})
	for i := range deleteRacers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = store.Delete(ctx, id)
		}()
	}
	close(start)
	wg.Wait()

	classifyDeleteRace(t, results)
}

// TestPostgres_DeleteConcurrent_TxJoined_OneWinner is
// TestPostgres_DeleteConcurrent_NonTx_OneWinner's "with a transaction" half:
// each racer runs its Delete under its OWN ambient (REPEATABLE READ)
// transaction, the shape workflow-engine callers actually use. deleteOn is
// the same function either way — the fix must protect both call sites.
func TestPostgres_DeleteConcurrent_TxJoined_OneWinner(t *testing.T) {
	// Every racer's transaction is Begun up front and held open for the whole
	// test (see below) — setupEntityTestWithTM's pool (newTestPool, 5 conns)
	// is too small to hold deleteRacers transactions at once, which would
	// deadlock the Begin loop against itself (nothing can free a connection
	// until a later Begin that cannot yet acquire one runs).
	pool := newTestPoolSized(t, deleteRacers+2)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	tm := postgres.NewTransactionManager(pool, newTestUUIDGenerator())
	factory := postgres.NewStoreFactoryWithTMForTest(pool, tm)
	const tenant spi.TenantID = "tenant-delete-race-tx"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-delete-race-tx", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const id = "e-delete-race-tx"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: ref},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// Begin every racer's own transaction from the main goroutine — t.Fatalf
	// and t.Cleanup (which BeginGuardedForTest / beginGuarded would reach for
	// on a Begin failure) may only be called from the goroutine running the
	// test, never from a spawned one — so Begin happens here, up front, and
	// only Delete/Commit/Rollback run concurrently below.
	txIDs := make([]string, deleteRacers)
	txCtxs := make([]context.Context, deleteRacers)
	for i := range deleteRacers {
		txID, txCtx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin racer %d: %v", i, err)
		}
		txIDs[i], txCtxs[i] = txID, txCtx
	}
	t.Cleanup(func() {
		for i, txID := range txIDs {
			_ = tm.Rollback(txCtxs[i], txID) // no-op once committed or already rolled back
		}
	})

	var wg sync.WaitGroup
	results := make([]error, deleteRacers)
	start := make(chan struct{})
	for i := range deleteRacers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			delErr := store.Delete(txCtxs[i], id)
			if delErr == nil {
				delErr = tm.Commit(txCtxs[i], txIDs[i])
			} else {
				_ = tm.Rollback(txCtxs[i], txIDs[i])
			}
			results[i] = delErr
		}()
	}
	close(start)
	wg.Wait()

	classifyDeleteRace(t, results)
}
