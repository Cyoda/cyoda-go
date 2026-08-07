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
// Errors returned from Exec/Query/QueryRow flow through the factory's
// classifier so concurrent-update aborts (40001 serialization_failure under
// REPEATABLE READ, 40P01 deadlock_detected) surface as spi.ErrConflict for the
// handler's errors.Is check, and a transaction the server has reclaimed takes
// its bookkeeping with it. Classification belongs here rather than at the call
// sites, which is why the stores do not re-classify what they get back.
//
// It is one of two queriers in this package, not the only one: a store whose
// statements must not join an ambient transaction takes poolQuerier instead.
// Both classify identically; they differ only in what they resolve.
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
	classify := func(err error) error { return c.factory.classifyFor(ctx, err) }
	return wrapRows(rows, classify), classify(err)
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

// classifyingRows extends the funnel over a result set.
//
// Query returns before a single row has been read, so a connection that dies
// while the caller is iterating reports through Scan and Err — neither of which
// passes back through Query. Without this, the one failure the funnel most needs
// to catch on a read path escapes it: a torn socket surfaces as a bare
// "row iteration error: unexpected EOF", unmarked, and a retryable outage is
// reported to the caller as an opaque 500.
type classifyingRows struct {
	pgx.Rows
	classify func(error) error
}

func (r *classifyingRows) Scan(dest ...any) error { return r.classify(r.Rows.Scan(dest...)) }

func (r *classifyingRows) Err() error { return r.classify(r.Rows.Err()) }

// wrapRows applies the funnel to a result set, passing a nil one through
// untouched — pgx returns nil rows on some failures, and a wrapper around nil
// would turn the caller's `defer rows.Close()` into a panic.
func wrapRows(rows pgx.Rows, classify func(error) error) pgx.Rows {
	if rows == nil {
		return nil
	}
	return &classifyingRows{Rows: rows, classify: classify}
}

// poolQuerier is the funnel for a store whose statements must NOT join an
// ambient transaction: it resolves the pool unconditionally, and classifies with
// the same plain classification classifyFor applies to a non-transactional
// statement.
//
// One store needs this. An async-search job record outlives the request that
// submitted it — the goroutine that fills it in runs on a context of its own —
// and the submit path's context can carry a joined transaction, because the
// TxJoin middleware wraps the whole API mux and the gRPC tx-route interceptor
// covers the snapshot RPCs. Resolving that transaction would bind the job record
// to it: invisible to the goroutine that must update it, and gone if the caller
// rolls back. Pinning to the pool keeps the record independent while still
// classifying what the pool reports.
type poolQuerier struct{ pool Querier }

func (p poolQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := p.pool.Exec(ctx, sql, args...)
	return tag, classifyError(err)
}

func (p poolQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	return wrapRows(rows, classifyError), classifyError(err)
}

func (p poolQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &classifyingRow{inner: p.pool.QueryRow(ctx, sql, args...), classify: classifyError}
}

// deadTxError marks a statement issued against a transaction the manager no
// longer holds.
//
// It carries the same StorageUnavailable marker — the transaction is gone
// either way, so the work cannot complete and a retry on a fresh one may well
// succeed — but it is a distinct type from idleInTxAbortError because the CAUSE
// is unknown here. A registry miss follows a reclaimed session, a failed commit,
// or a transaction that simply finished, and the operator log must not name one
// of them.
type deadTxError struct{ txID string }

func (e *deadTxError) Error() string {
	return "transaction " + e.txID + " is no longer active"
}
func (e *deadTxError) StorageUnavailable() bool { return true }

// deadTxQuerier is what resolveRaw hands a store whose context names a
// transaction the manager no longer holds. Every statement fails: falling back
// to the pool would run it outside the transaction the caller believes it is in.
type deadTxQuerier struct{ txID string }

func (d deadTxQuerier) err() error { return &deadTxError{txID: d.txID} }

func (d deadTxQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.err()
}

func (d deadTxQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, d.err()
}

func (d deadTxQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return deadTxRow{d} }

type deadTxRow struct{ q deadTxQuerier }

func (r deadTxRow) Scan(...any) error { return r.q.err() }
