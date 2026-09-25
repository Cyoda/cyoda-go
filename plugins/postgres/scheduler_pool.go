package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The scheduler pool's session ceilings. They are fixed, not operator
// settings: the scheduler's timing rules (the STALE_AFTER validation, the
// watchdog margin) are derived from them.
const (
	schedulerStatementTimeout = 30 * time.Second
	schedulerIdleInTxTimeout  = 10 * time.Second
	schedulerLockTimeout      = 2 * time.Second
	schedulerAcquireTimeout   = 5 * time.Second
)

// schedulerPools are the scheduler's own connections, kept apart from the main
// pool so that entity transactions, however many are open, cannot starve a
// claim, a heartbeat or a run's bookkeeping (C4).
//
// work carries every never-joining ScheduledTaskStore method except Query and
// Heartbeat, and the async-search heartbeat and claim. heartbeat is one
// connection used only by ScheduledTaskStore.Heartbeat, so a busy work pool
// cannot delay the liveness record either.
//
// Both are derived from the main pool's configuration and opened on first use,
// so a test factory that never schedules anything opens nothing, and a factory
// built without a pool fails only when asked for one. Plugin.NewFactory opens
// them eagerly, so a deployment that cannot connect them fails at startup.
type schedulerPools struct {
	mu        sync.Mutex
	work      *pgxpool.Pool
	heartbeat *pgxpool.Pool
}

// schedulerPoolConfig derives a scheduler pool from the main pool's config.
// The ceilings overwrite any value from the DSN: they are the scheduler's
// contract, not the operator's.
func schedulerPoolConfig(base *pgxpool.Config, maxConns int32) *pgxpool.Config {
	c := base.Copy()
	c.MaxConns = maxConns
	c.MinConns = 0
	c.MinIdleConns = 0
	if c.ConnConfig.RuntimeParams == nil {
		c.ConnConfig.RuntimeParams = map[string]string{}
	}
	p := c.ConnConfig.RuntimeParams
	p["statement_timeout"] = pgDurationMillis(schedulerStatementTimeout)
	p["idle_in_transaction_session_timeout"] = pgDurationMillis(schedulerIdleInTxTimeout)
	p["lock_timeout"] = pgDurationMillis(schedulerLockTimeout)
	p["default_transaction_isolation"] = "read committed"
	return c
}

// schedulerPools returns the two scheduler pools, creating them on first use.
func (f *StoreFactory) schedulerPools() (work, heartbeat *pgxpool.Pool, err error) {
	f.sched.mu.Lock()
	defer f.sched.mu.Unlock()
	if f.sched.work != nil {
		return f.sched.work, f.sched.heartbeat, nil
	}
	if f.pool == nil {
		return nil, nil, errors.New("scheduler pool: the store factory has no connection pool")
	}
	base := f.pool.Config()
	work, err = pgxpool.NewWithConfig(context.Background(), schedulerPoolConfig(base, f.cfg.SchedulerConns))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the scheduler pool: %w", err)
	}
	heartbeat, err = pgxpool.NewWithConfig(context.Background(), schedulerPoolConfig(base, 1))
	if err != nil {
		work.Close()
		return nil, nil, fmt.Errorf("failed to create the scheduler heartbeat pool: %w", err)
	}
	f.sched.work, f.sched.heartbeat = work, heartbeat
	return work, heartbeat, nil
}

// openSchedulerPools creates both pools and connects one session in each.
func (f *StoreFactory) openSchedulerPools(ctx context.Context) error {
	work, heartbeat, err := f.schedulerPools()
	if err != nil {
		return err
	}
	if err := work.Ping(ctx); err != nil {
		f.closeSchedulerPools()
		return fmt.Errorf("failed to connect the scheduler pool: %w", err)
	}
	if err := heartbeat.Ping(ctx); err != nil {
		f.closeSchedulerPools()
		return fmt.Errorf("failed to connect the scheduler heartbeat pool: %w", err)
	}
	return nil
}

// closeSchedulerPools closes whichever scheduler pools are open.
func (f *StoreFactory) closeSchedulerPools() {
	f.sched.mu.Lock()
	defer f.sched.mu.Unlock()
	if f.sched.work != nil {
		f.sched.work.Close()
		f.sched.work = nil
	}
	if f.sched.heartbeat != nil {
		f.sched.heartbeat.Close()
		f.sched.heartbeat = nil
	}
}

// snapshot returns the scheduler pools that are open now; nil for one that is
// not. The metrics callback reads them at each scrape.
func (p *schedulerPools) snapshot() (work, heartbeat *pgxpool.Pool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.work, p.heartbeat
}

// schedulerQuerier runs a statement on a scheduler pool. It never joins a
// transaction on ctx. Unlike unjoinedQuerier it bounds every acquire, inside a
// transaction or not: the scheduler retries a failed write with a backoff, and
// an unbounded wait would stall that loop instead.
//
// Errors pass through the plain funnel (classifyError), as for unjoinedQuerier:
// the statement is not part of the caller's transaction.
type schedulerQuerier struct {
	factory   *StoreFactory
	heartbeat bool
	what      string
}

func (f *StoreFactory) schedulerQuerier(what string) schedulerQuerier {
	return schedulerQuerier{factory: f, what: what}
}

func (f *StoreFactory) heartbeatQuerier() schedulerQuerier {
	return schedulerQuerier{factory: f, heartbeat: true, what: "scheduler heartbeat"}
}

func (q schedulerQuerier) pool() (*pgxpool.Pool, error) {
	work, heartbeat, err := q.factory.schedulerPools()
	if err != nil {
		return nil, err
	}
	if q.heartbeat {
		return heartbeat, nil
	}
	return work, nil
}

func (q schedulerQuerier) acquire(ctx context.Context, verb string) (*pgxpool.Conn, error) {
	p, err := q.pool()
	if err != nil {
		return nil, err
	}
	acquireCtx, cancel := newAcquireContext(ctx, schedulerAcquireTimeout)
	defer cancel()
	conn, err := p.Acquire(acquireCtx)
	if err != nil {
		return nil, classifyAcquireErr(ctx, acquireCtx, q.what+" "+verb, err)
	}
	return conn, nil
}

// begin opens a READ COMMITTED transaction on the pool. The acquire deadline
// never reaches the returned transaction (see newAcquireContext).
func (q schedulerQuerier) begin(ctx context.Context) (pgx.Tx, error) {
	p, err := q.pool()
	if err != nil {
		return nil, err
	}
	acquireCtx, cancel := newAcquireContext(ctx, schedulerAcquireTimeout)
	defer cancel()
	tx, err := p.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, classifyAcquireErr(ctx, acquireCtx, q.what+" begin", err)
	}
	return tx, nil
}

func (q schedulerQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	conn, err := q.acquire(ctx, "exec")
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	tag, err := conn.Exec(ctx, sql, args...)
	return tag, classifyError(err)
}

func (q schedulerQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	conn, err := q.acquire(ctx, "query")
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		conn.Release()
		return nil, classifyError(err)
	}
	return wrapRows(&releasingRows{Rows: rows, release: conn.Release}, classifyError), nil
}

func (q schedulerQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	conn, err := q.acquire(ctx, "row read")
	if err != nil {
		return acquireFailedRow{err: err}
	}
	return &classifyingRow{
		inner:    &releasingRow{inner: conn.QueryRow(ctx, sql, args...), release: conn.Release},
		classify: classifyError,
	}
}
