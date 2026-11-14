package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// modelPool is the model store's own connection pool, kept apart from the main
// pool so that entity transactions, however many are open, cannot starve a
// model read, an extension or a model-administration call.
//
// Schema extensions are the long holders of its connections (each one waits on
// the model's row lock), so they take a slot first and the slots are capped at
// half the pool: reads always have connections left over.
//
// Every connection acquire on this pool is bounded by the acquire timeout
// (CYODA_POSTGRES_ACQUIRE_TIMEOUT), inside a transaction or not: an
// extension's after it has its slot (acquireExtension), a transaction's
// (modelStore.inModelTx) and a single statement's (modelStore's querier sets
// unjoinedQuerier.alwaysBound). A saturated model pool then answers a
// retryable storage-unavailable error, the same through every door, rather
// than a wait bounded only by the request: up to half its connections can be
// held by extensions waiting on a model's row lock, and its other callers are
// requests, with no background traffic that an acquire timeout would fail
// spuriously.
//
// The pool's lock_timeout is 0 (wait indefinitely): a model write that must
// queue behind another on the same model's row lock is bounded by the caller's
// context and the statement and idle-in-transaction ceilings inherited from the
// main pool, not by a lock ceiling that would turn queueing into an error.
type modelPool struct {
	pool           *pgxpool.Pool
	extSlots       chan struct{}
	acquireTimeout time.Duration
}

// modelPoolConfig derives the model pool from the main pool's config. The
// statement and idle-in-transaction ceilings are the base's; only the size and
// lock_timeout are the model pool's own.
func modelPoolConfig(base *pgxpool.Config, conns int32) *pgxpool.Config {
	c := base.Copy()
	c.MaxConns = conns
	c.MinConns = 0
	c.MinIdleConns = 0
	if c.ConnConfig.RuntimeParams == nil {
		c.ConnConfig.RuntimeParams = map[string]string{}
	}
	c.ConnConfig.RuntimeParams["lock_timeout"] = "0"
	return c
}

// newModelPool opens the pool from the main pool's configuration and connects
// one session, so a deployment that cannot reach the database fails at startup.
func newModelPool(ctx context.Context, base *pgxpool.Config, conns int32, acquireTimeout time.Duration) (*modelPool, error) {
	m, err := openModelPool(ctx, base, conns, acquireTimeout)
	if err != nil {
		return nil, err
	}
	if err := m.pool.Ping(ctx); err != nil {
		m.close()
		return nil, fmt.Errorf("failed to connect the model pool: %w", err)
	}
	return m, nil
}

// openModelPool creates the pool without connecting a session.
func openModelPool(ctx context.Context, base *pgxpool.Config, conns int32, acquireTimeout time.Duration) (*modelPool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, modelPoolConfig(base, conns))
	if err != nil {
		return nil, fmt.Errorf("failed to create the model pool: %w", err)
	}
	return &modelPool{
		pool:           pool,
		extSlots:       make(chan struct{}, max(1, conns/2)),
		acquireTimeout: acquireTimeout,
	}, nil
}

// modelPool returns the factory's model pool. Plugin.NewFactory opens it at
// startup; a factory built by another constructor opens it on first use, as
// the scheduler pools are. Once closeModelPool has run it is never reopened.
func (f *StoreFactory) modelPool() (*modelPool, error) {
	f.modelsMu.Lock()
	defer f.modelsMu.Unlock()
	if f.modelsClosed {
		return nil, errors.New("failed to open the model pool: already closed")
	}
	if f.models != nil {
		return f.models, nil
	}
	if f.pool == nil {
		return nil, errors.New("failed to open the model pool: the store factory has no connection pool")
	}
	m, err := openModelPool(context.Background(), f.pool.Config(), f.cfg.ModelConns, f.cfg.AcquireTimeout)
	if err != nil {
		return nil, err
	}
	f.models = m
	return m, nil
}

// openModelPoolOrNil returns the model pool's connections if the pool is open,
// and nil otherwise. It never opens the pool: the pool metrics read it on
// every scrape.
func (f *StoreFactory) openModelPoolOrNil() *pgxpool.Pool {
	f.modelsMu.Lock()
	defer f.modelsMu.Unlock()
	if f.models == nil {
		return nil
	}
	return f.models.pool
}

// closeModelPool closes the model pool if it is open and marks it closed for
// good.
func (f *StoreFactory) closeModelPool() {
	f.modelsMu.Lock()
	defer f.modelsMu.Unlock()
	f.modelsClosed = true
	f.models.close()
	f.models = nil
}

// acquireExtension takes an extension slot, waiting until ctx ends, then a
// connection bounded by the acquire timeout. The release frees the connection
// and then the slot. On error nothing is held.
func (m *modelPool) acquireExtension(ctx context.Context) (*pgxpool.Conn, func(), error) {
	select {
	case m.extSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	acquireCtx, cancel := newAcquireContext(ctx, m.acquireTimeout)
	defer cancel()
	conn, err := m.pool.Acquire(acquireCtx)
	if err != nil {
		<-m.extSlots
		return nil, nil, classifyAcquireErr(ctx, acquireCtx, "model extension acquire", err)
	}
	return conn, func() {
		conn.Release()
		<-m.extSlots
	}, nil
}

func (m *modelPool) close() {
	if m != nil {
		m.pool.Close()
	}
}
