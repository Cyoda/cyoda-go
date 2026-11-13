package postgres

import (
	"context"
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
	pool, err := pgxpool.NewWithConfig(ctx, modelPoolConfig(base, conns))
	if err != nil {
		return nil, fmt.Errorf("failed to create the model pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to connect the model pool: %w", err)
	}
	return &modelPool{
		pool:           pool,
		extSlots:       make(chan struct{}, max(1, conns/2)),
		acquireTimeout: acquireTimeout,
	}, nil
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
