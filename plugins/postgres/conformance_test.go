package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	spitest "github.com/cyoda-platform/cyoda-go-spi/spitest"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// noCloseFactory wraps a StoreFactory so that the harness's factory.Close()
// call is a no-op. This lets the test's own t.Cleanup control when the pool
// is actually closed — AFTER all subtest goroutines have finished and all
// connections returned.
type noCloseFactory struct {
	spi.StoreFactory
}

func (f *noCloseFactory) Close() error { return nil }

// newConformancePool creates a fully-migrated StoreFactory with enough
// connections for concurrent conformance subtests (e.g.
// Entity/Concurrent/DifferentEntities spawns 8 goroutines).
//
// Migrations are applied via a separate dedicated pool so there is no
// race between MigrateDown and pool.Close at cleanup time.
//
// The main pool disables the pgxpool health check (HealthCheckPeriod=24h)
// to avoid a deadlock in pgxpool/puddle where the health-check goroutine
// destroys a connection whose context-watcher goroutine has already exited.
// The factory is wrapped in noCloseFactory so the harness's Close() call
// is a no-op; the actual pool.Close() runs in t.Cleanup after all subtests
// complete and goroutines have returned their connections.
// It also returns the underlying pool, so callers can read the DATABASE clock
// (the clock this plugin stamps versions from) rather than the test process's.
func newConformancePool(t *testing.T) (spi.StoreFactory, *pgxpool.Pool) {
	t.Helper()
	dbURL := testDBURL(t)

	// Migration pool: minimal, used only for Migrate/MigrateDown.
	migPool, err := postgres.NewPool(context.Background(), postgres.DBConfig{
		URL:             dbURL,
		MaxConns:        2,
		MinConns:        0,
		MaxConnIdleTime: "30s",
	})
	if err != nil {
		t.Fatalf("failed to create migration pool: %v", err)
	}

	// Reset the schema unconditionally before migrating up. DropSchema is
	// used instead of MigrateDown because MigrateDown can fail when test data
	// from a previous run violates DOWN-migration constraints (e.g. duplicate
	// job IDs across tenants when reverting a composite-PK migration).
	if err := postgres.DropSchemaForTest(migPool); err != nil {
		migPool.Close()
		t.Fatalf("failed to reset schema: %v", err)
	}
	if err := postgres.Migrate(migPool); err != nil {
		migPool.Close()
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() {
		// DropSchema is robust even when test data is present.
		_ = postgres.DropSchemaForTest(migPool)
		migPool.Close()
	})

	// Main pool: high connection limit for concurrent subtests.
	// HealthCheckPeriod is set to 24h (effectively disabled) to prevent the
	// pgxpool health-check goroutine from racing with pool.Close() and
	// deadlocking on the context-watcher unwatch path.
	mainPoolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("failed to parse conformance pool config: %v", err)
	}
	mainPoolCfg.MaxConns = 20
	mainPoolCfg.MinConns = 0
	mainPoolCfg.MaxConnIdleTime = 30 * time.Second
	mainPoolCfg.HealthCheckPeriod = 24 * time.Hour

	pool, err := pgxpool.NewWithConfig(context.Background(), mainPoolCfg)
	if err != nil {
		t.Fatalf("failed to create conformance pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("failed to ping conformance pool: %v", err)
	}

	factory := postgres.NewStoreFactory(pool)
	factory.InitTransactionManager(newTestUUIDGenerator())

	// Close the pool in t.Cleanup (LIFO: runs before migration cleanup because
	// it is registered later). A brief sleep gives any in-flight goroutines
	// (e.g. from Entity/Concurrent subtests) time to return their connections
	// before pool.Close() is called. pool.Close() runs in a goroutine with a
	// deadline so a stuck cleanup never blocks the test suite indefinitely.
	t.Cleanup(func() {
		time.Sleep(200 * time.Millisecond)
		done := make(chan struct{})
		go func() {
			pool.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// Pool close timed out — leak the pool. This is acceptable in tests:
			// connections will be cleaned up when the process exits.
		}
	})

	// Wrap factory so the harness's factory.Close() is a no-op; actual pool
	// close is handled by the t.Cleanup above.
	return &noCloseFactory{factory}, pool
}

// newTestFactory is kept for per-store test helpers that do not use the
// full conformance harness. It reuses the shared test pool and does NOT call
// factory.Close() (the pool is closed by newTestPool's own t.Cleanup).
func newTestFactory(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("failed to reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	factory := postgres.NewStoreFactory(pool)
	factory.InitTransactionManager(newTestUUIDGenerator())
	return factory
}

// TestConformance runs the SPI conformance harness against the postgres plugin.
//
// AdvanceClock is time.Sleep(d) — postgres uses CURRENT_TIMESTAMP and
// clock_timestamp() DB-side for version and transaction timestamps. These
// are load-bearing for correctness (tx-stable timestamps) so we do not
// refactor plugin timekeeping. The DB's monotonic wall clock satisfies the
// harness contract under wall-clock advance at 1ms granularity.
//
// Now is overridden to read the DATABASE clock. It must NOT be left at the
// default (time.Now): the harness uses Now to capture as-at markers that are
// compared against plugin-stamped version times, and this plugin stamps those
// from postgres. Against a testcontainer that is the Docker VM's clock, which
// was measured lagging the macOS host by 10–13 ms under CPU load — far more
// than the 5 ms AdvanceClock floor below, so a host-clock marker would resolve
// temporal subtests to the wrong version. See issue #460.
//
// Total wall-clock sleep overhead is ~30–50 ms across all temporal subtests.
func TestConformance(t *testing.T) {
	factory, pool := newConformancePool(t)
	spitest.StoreFactoryConformance(t, spitest.Harness{
		Factory: factory,
		Now: func() time.Time {
			var now time.Time
			if err := pool.QueryRow(context.Background(), `SELECT CURRENT_TIMESTAMP`).Scan(&now); err != nil {
				// The harness calls this from its subtests' goroutines, so the
				// outer t's FailNow would be invalid here — panic instead, which
				// surfaces against whichever subtest is running.
				panic(fmt.Sprintf("Harness.Now: read DB clock: %v", err))
			}
			return now
		},
		AdvanceClock: func(d time.Duration) {
			// Postgres timestamp resolution on macOS can be coarser than 1 µs.
			// Enforce a 5 ms floor so that DB clock_timestamp() calls separated
			// by AdvanceClock(1ms) are reliably distinct.
			if d < 5*time.Millisecond {
				d = 5 * time.Millisecond
			}
			// Cap the ceiling too. Some subtests advance by an arbitrarily large
			// duration on purpose — e.g. AsyncSearch's Claim/TerminalNeverClaimed
			// advances 365 days "to rule out any staleAfter/limit edge case
			// masking a store that claims terminal jobs" (spitest/asyncsearch.go)
			// — not because the assertion needs real time to actually pass that
			// far. Postgres has no virtual clock to fast-forward: it can only
			// honour AdvanceClock by sleeping, and the harness's own contract
			// (spitest.Harness.AdvanceClock godoc) only requires every subsequent
			// DB timestamp to strictly dominate every earlier one — a requirement
			// any positive sleep already satisfies. Sleeping the literal 365 days
			// would hang the suite for a year for no correctness benefit.
			if d > 100*time.Millisecond {
				d = 100 * time.Millisecond
			}
			time.Sleep(d)
		},
		Skip: map[string]string{
			// pgx.Tx aborts surface as ErrConflict via SQLSTATE 25P02
			// (in_failed_sql_transaction), not as ErrTxRolledBack. This is
			// the documented postgres-engine behaviour per the ErrTxTerminated
			// SPI godoc — backends that don't own their own in-process
			// tx-state buffer surface mid-op rollback through the engine's
			// failure code.
			"Transaction/TxStateErrors/OpAfterRollback": "postgres: pgx.Tx aborts surface as ErrConflict via SQLSTATE 25P02, not as ErrTxRolledBack",
		},
	})
}
