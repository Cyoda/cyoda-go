package postgres

import (
	"context"

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
// Errors returned from Exec/Query/QueryRow flow through classifyError so
// concurrent-update aborts (40001 serialization_failure under REPEATABLE
// READ, 40P01 deadlock_detected) surface as spi.ErrConflict for the
// handler's errors.Is check.
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
	return tag, classifyError(err)
}

func (c *ctxQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := c.resolveInner(ctx).Query(ctx, sql, args...)
	return rows, classifyError(err)
}

func (c *ctxQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &classifyingRow{inner: c.resolveInner(ctx).QueryRow(ctx, sql, args...)}
}

type classifyingRow struct {
	inner pgx.Row
}

func (r *classifyingRow) Scan(dest ...any) error {
	return classifyError(r.inner.Scan(dest...))
}
