package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newSchedulerTestFactory builds a factory over a small main pool and closes
// its scheduler pools when the test ends.
func newSchedulerTestFactory(t *testing.T, getenv func(string) string) *StoreFactory {
	t.Helper()
	f := newStoreFactoryWithConfig(openCeilingPool(t, getenv), defaultStoreConfig())
	t.Cleanup(f.closeSchedulerPools)
	return f
}

// The scheduler's ceilings are fixed. A statement_timeout in the DSN reaches
// the main pool (applyCeiling) but never the scheduler's pools.
func TestSchedulerPools_SessionCeilings(t *testing.T) {
	dsn := dsnWithParam(t, testDBURL(t), "statement_timeout", "7000")
	f := newSchedulerTestFactory(t, ceilingEnv(dsn, nil))
	work, heartbeat, err := f.schedulerPools()
	if err != nil {
		t.Fatalf("schedulerPools: %v", err)
	}
	if got := work.Config().MaxConns; got != 10 {
		t.Errorf("work pool MaxConns = %d, want 10 (CYODA_POSTGRES_SCHEDULER_CONNS default)", got)
	}
	if got := heartbeat.Config().MaxConns; got != 1 {
		t.Errorf("heartbeat pool MaxConns = %d, want 1", got)
	}
	for name, p := range map[string]*pgxpool.Pool{"work": work, "heartbeat": heartbeat} {
		t.Run(name, func(t *testing.T) {
			if got := gucMillis(t, p, "statement_timeout"); got != 30000 {
				t.Errorf("statement_timeout = %d ms, want 30000", got)
			}
			if got := gucMillis(t, p, "idle_in_transaction_session_timeout"); got != 10000 {
				t.Errorf("idle_in_transaction_session_timeout = %d ms, want 10000", got)
			}
			if got := gucMillis(t, p, "lock_timeout"); got != 2000 {
				t.Errorf("lock_timeout = %d ms, want 2000", got)
			}
			var iso string
			if err := p.QueryRow(context.Background(),
				`SELECT current_setting('default_transaction_isolation')`).Scan(&iso); err != nil {
				t.Fatalf("read default_transaction_isolation: %v", err)
			}
			if iso != "read committed" {
				t.Errorf("default_transaction_isolation = %q, want %q", iso, "read committed")
			}
		})
	}
}

// A statement on the scheduler pool that waits on a row lock gives up after
// lock_timeout with 55P03, which callers retry.
func TestSchedulerPools_LockWaitEndsAtLockTimeout(t *testing.T) {
	f := newSchedulerTestFactory(t, ceilingEnv(testDBURL(t), nil))
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS sched_lock_probe (id int PRIMARY KEY);
		 INSERT INTO sched_lock_probe VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DROP TABLE IF EXISTS sched_lock_probe`) })

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, `UPDATE sched_lock_probe SET id = id WHERE id = 1`); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	start := time.Now()
	_, err = f.schedulerQuerier("lock probe").Exec(ctx, `UPDATE sched_lock_probe SET id = id WHERE id = 1`)
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
		t.Fatalf("err = %v, want SQLSTATE 55P03 lock_not_available", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("gave up after %s, want about the 2s lock_timeout", elapsed)
	}
}

// The acquire is bounded at 5s even outside any transaction, and a timeout
// carries the storage-unavailable marker.
func TestSchedulerPools_AcquireIsBounded(t *testing.T) {
	f := newSchedulerTestFactory(t, ceilingEnv(testDBURL(t), nil))
	work, _, err := f.schedulerPools()
	if err != nil {
		t.Fatalf("schedulerPools: %v", err)
	}
	ctx := context.Background()
	for i := int32(0); i < work.Config().MaxConns; i++ {
		c, err := work.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold connection %d: %v", i, err)
		}
		t.Cleanup(c.Release) // runs before closeSchedulerPools (LIFO)
	}

	start := time.Now()
	_, err = f.schedulerQuerier("acquire probe").Exec(ctx, `SELECT 1`)
	elapsed := time.Since(start)
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("err = %v, want the storage-unavailable acquire-timeout marker", err)
	}
	if elapsed < 4*time.Second || elapsed > 9*time.Second {
		t.Errorf("acquire gave up after %s, want about 5s", elapsed)
	}
}

// Plugin.NewFactory opens and pings both scheduler pools; Close closes them.
func TestNewFactory_OpensTheSchedulerPools(t *testing.T) {
	dsn := testDBURL(t)
	reset := openCeilingPool(t, ceilingEnv(dsn, nil))
	if err := dropSchema(reset); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(func() { _ = dropSchema(reset) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, err := (&plugin{}).NewFactory(ctx, ceilingEnv(dsn, map[string]string{"CYODA_POSTGRES_MIN_CONNS": "0"}))
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	sf := f.(*StoreFactory)
	if sf.sched.work == nil || sf.sched.heartbeat == nil {
		t.Fatal("NewFactory did not open the scheduler pools")
	}
	if sf.sched.work.Stat().TotalConns() < 1 || sf.sched.heartbeat.Stat().TotalConns() < 1 {
		t.Error("NewFactory opened the scheduler pools without connecting them")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sf.sched.work != nil || sf.sched.heartbeat != nil {
		t.Error("Close left the scheduler pools open")
	}
}
