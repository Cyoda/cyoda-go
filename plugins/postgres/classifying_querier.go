package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ctxQuerier resolves the underlying Querier lazily on every call using the
// context passed to each method. This matters because stores are constructed
// at the start of a handler (before Begin), but the active transaction is
// discovered from the context passed to the store method. Caching a single
// Querier at store construction would freeze the choice to whatever was in
// the construction-time ctx — typically the pool — and subsequent calls with
// a tx-carrying ctx would still go through the pool, deadlocking when pool
// conns are saturated by in-flight txs.
//
// Errors returned from Exec/Query/QueryRow flow through the factory's
// classifier so concurrent-update aborts (40001 serialization_failure under
// REPEATABLE READ, 40P01 deadlock_detected) surface as spi.ErrConflict for the
// handler's errors.Is check, and a transaction the server has reclaimed takes
// its bookkeeping with it. This is the funnel every store statement passes
// through — classification belongs here rather than at the call sites, which is
// why the stores do not re-classify what they get back.
type ctxQuerier struct {
	factory *StoreFactory
}

// resolveInner returns the concrete pgx querier for the given context —
// the active pgx.Tx when one is in context, otherwise the pool.
func (c *ctxQuerier) resolveInner(ctx context.Context) Querier {
	return c.factory.resolveRaw(ctx)
}

func (c *ctxQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := c.resolveInner(ctx).Exec(ctx, sql, args...)
	return tag, c.factory.classifyFor(ctx, err)
}

func (c *ctxQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := c.resolveInner(ctx).Query(ctx, sql, args...)
	return rows, c.factory.classifyFor(ctx, err)
}

func (c *ctxQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &classifyingRow{
		inner:    c.resolveInner(ctx).QueryRow(ctx, sql, args...),
		classify: func(err error) error { return c.factory.classifyFor(ctx, err) },
	}
}

// classifyingRow carries its call-time classifier because pgx.Row.Scan takes no
// context, and the transaction the statement ran against is only knowable from
// the one QueryRow was handed.
type classifyingRow struct {
	inner    pgx.Row
	classify func(error) error
}

func (r *classifyingRow) Scan(dest ...any) error {
	return r.classify(r.inner.Scan(dest...))
}

// deadTxQuerier is what resolveRaw hands a store whose context names a
// transaction the manager no longer holds. Every statement fails, and fails with
// the storage-unavailable marker: the transaction is gone, so the work cannot be
// completed correctly, and a retry on a fresh one may well succeed.
type deadTxQuerier struct{ txID string }

func (d deadTxQuerier) err() error {
	return &idleInTxAbortError{cause: fmt.Errorf("transaction %s is no longer active", d.txID)}
}

func (d deadTxQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.err()
}

func (d deadTxQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, d.err()
}

func (d deadTxQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return deadTxRow{d} }

type deadTxRow struct{ q deadTxQuerier }

func (r deadTxRow) Scan(...any) error { return r.q.err() }
