package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// unjoinedQuerier runs a statement that must NOT join the caller's transaction,
// bounding the connection acquire on the one path where an unbounded wait is a
// deadlock: inside a transaction.
//
// Two callers, one hazard. A point-in-time read is committed-only, so it must
// run off the caller's transaction (committedQuerier, search_base.go). An async
// search job record must outlive the request that submitted it, so it must not
// be bound to that request's transaction (poolQuerier, store_factory.go). Both
// requirements are non-negotiable, and both have the same consequence: in a
// transaction the request holds two connections at once — the transaction's,
// which it keeps until commit, and this one. That is hold-and-wait, and
// pgxpool's Query/Exec/QueryRow carry no acquire deadline (deliberately — see
// resolveRaw), so on a saturated pool the statement does not fail, it BLOCKS,
// until the caller's own context expires. A point-in-time Iterate is the worst
// of it: the connection is held for the whole scan, not one statement, so a
// handful of concurrent in-transaction snapshot iterations can wedge every
// remaining connection against each other. The job-record statements hold theirs
// for a single round trip, but they are reachable from every async-search
// endpoint — submit, status, results, cancel — because the TxJoin middleware
// wraps the whole API mux and the gRPC tx-route interceptor covers the snapshot
// RPCs.
//
// The bound is on GETTING the connection, never on USING it. pool.Acquire takes
// its own short-lived deadline context; the statement then runs on the CALLER's
// context on the acquired connection. A deadline that covered the query would
// cap a streaming Iterate at the acquire window — cancelling a scan that already
// holds everything it needs, which is the opposite of the point. This is the
// same split newAcquireContext draws for the three sites that open a transaction
// on the pool, and the deadline is the same operator knob they use
// (CYODA_POSTGRES_ACQUIRE_TIMEOUT): one setting for "how long may this plugin
// wait for a pooled connection", not a second one per call site. Zero disables
// it, per that knob's documented convention.
//
// Failure is closed and named: classifyAcquireErr mints acquireTimeoutError,
// which carries the StorageUnavailable marker the domain turns into a retryable
// 503 — pool exhaustion is transient contention, and the next attempt may well
// find a free connection. It is NOT spi.ErrConflict: nothing here lost a
// first-committer race, and a 409 would tell the caller to rebuild its write on
// a fresh snapshot for a condition that has nothing to do with its data. The
// same call also keeps a caller who gave up first off the retryable path.
//
// Outside a transaction the statement goes straight to the pool, unbounded, as
// before. It holds no other connection, so it is not hold-and-wait and there is
// no deadlock to break; bounding it would convert ordinary contention on a busy
// server into spurious failures — and the job store's own background traffic
// (the reaper, the heartbeat, the job goroutine's terminal write) is all on that
// path. The gate is the caller's context alone (spi.GetTransaction) rather than
// whether the transaction is still live: a transaction the manager has already
// lost holds no connection, but bounding that case too costs nothing and keeps
// the rule statable in one line.
//
// Classification is the plain funnel (classifyError) rather than ctxQuerier's
// transaction-scoped one, for the same reason the statement does not join: it
// does not belong to the caller's transaction, so an error it raises must not
// reclaim that transaction's bookkeeping.
type unjoinedQuerier struct {
	pool           *pgxpool.Pool
	acquireTimeout time.Duration

	// what names the statement family in an acquire-failure message, so an
	// operator log says which door ran out of connections. classifyAcquireErr
	// renders it as "<what> query: could not acquire a database connection …".
	what string
}

// pooled is the unbounded path — the pool's own acquire, classified.
func (c unjoinedQuerier) pooled() classifiedQuerier {
	return classifiedQuerier{inner: c.pool}
}

// acquire takes a connection under the acquire-only deadline. The deadline
// context is cancelled before returning: it has done its job once Acquire has
// answered, and the connection it hands back is not bound to it.
func (c unjoinedQuerier) acquire(ctx context.Context, verb string) (*pgxpool.Conn, error) {
	acquireCtx, cancelAcquire := newAcquireContext(ctx, c.acquireTimeout)
	defer cancelAcquire()
	conn, err := c.pool.Acquire(acquireCtx)
	if err != nil {
		return nil, classifyAcquireErr(ctx, acquireCtx, c.what+" "+verb, err)
	}
	return conn, nil
}

func (c unjoinedQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if spi.GetTransaction(ctx) == nil {
		return c.pooled().Exec(ctx, sql, args...)
	}
	conn, err := c.acquire(ctx, "exec")
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	tag, err := conn.Exec(ctx, sql, args...)
	return tag, classifyError(err)
}

func (c unjoinedQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if spi.GetTransaction(ctx) == nil {
		return c.pooled().Query(ctx, sql, args...)
	}
	conn, err := c.acquire(ctx, "query")
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		conn.Release()
		return nil, classifyError(err)
	}
	// releasingRows innermost so the Close() promoted through classifyingRows
	// reaches it, and the connection goes back when the caller is done reading.
	return wrapRows(&releasingRows{Rows: rows, release: conn.Release}, classifyError), nil
}

func (c unjoinedQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if spi.GetTransaction(ctx) == nil {
		return c.pooled().QueryRow(ctx, sql, args...)
	}
	conn, err := c.acquire(ctx, "row read")
	if err != nil {
		// Already classified; returned as-is rather than through
		// classifyingRow, which would funnel it a second time.
		return acquireFailedRow{err: err}
	}
	return &classifyingRow{
		inner:    &releasingRow{inner: conn.QueryRow(ctx, sql, args...), release: conn.Release},
		classify: classifyError,
	}
}

// releasingRows hands the connection back when the result set is finished with.
//
// It mirrors what pgxpool's own poolRows does for pool.Query, and must: pgx
// closes the underlying rows internally when iteration exhausts them, and that
// close never passes through this wrapper — so without the Next() override a
// caller that iterates to the end and (legitimately) relies on the auto-close
// would leak the connection. Release is idempotent, so the explicit Close() every
// call site also issues is free.
type releasingRows struct {
	pgx.Rows
	release func()
}

func (r *releasingRows) Close() {
	r.Rows.Close()
	r.release()
}

func (r *releasingRows) Next() bool {
	next := r.Rows.Next()
	if !next {
		r.Close()
	}
	return next
}

// releasingRow hands the connection back after Scan, which is the only thing a
// pgx.Row can be asked to do and the point at which pgx closes the result set
// underneath. The deferred release covers a panicking Scan destination.
//
// A row that is never scanned never releases — the same property pgxpool's own
// poolRow has, and the reason pgx.Row's contract is "call Scan exactly once".
// Every call site on this path (GetAsAt; the job store's GetJob, probeFenced and
// Cancel) does.
type releasingRow struct {
	inner   pgx.Row
	release func()
}

func (r *releasingRow) Scan(dest ...any) error {
	defer r.release()
	return r.inner.Scan(dest...)
}

// acquireFailedRow reports an acquire that never got a connection, through the
// pgx.Row shape QueryRow's caller is holding.
type acquireFailedRow struct{ err error }

func (r acquireFailedRow) Scan(...any) error { return r.err }
