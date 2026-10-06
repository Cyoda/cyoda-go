package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// consistencyWaitBudget caps how long one ConsistencyTime call waits for the
// tenant's commits in their commit phase.
const consistencyWaitBudget = 10 * time.Second

// tenantKeys caches each tenant's marker key: the int4 its row in
// consistency_tenant_keys (migration 000016) assigns it, which cyoda_stamp and
// cyoda_consistency_time take as the first half of the tenant's in-flight
// markers. The key is allocated from a sequence, so two tenants never share
// one and cannot delay each other or see each other's commit timing. A key
// never changes once allocated, so a cached key never goes stale. The map is
// keyed by the exact tenant id, never a normalised form. A store factory owns
// one cache from construction and hands it to the transaction manager it
// builds (withTenantKeys), so the two share it.
//
// The key is resolved before a commit phase starts — at Begin, before a
// non-transactional write opens its own transaction, and at the start of
// ConsistencyTime — in a lookup transaction of its own, never on a caller's
// or a committing transaction's connection. A stamping transaction therefore
// never touches the table, and the design rule that nothing after the stamp
// waits on a lock holds. A failed lookup fails the operation.
type tenantKeys struct {
	m sync.Map // spi.TenantID -> int32
}

func newTenantKeys() *tenantKeys { return &tenantKeys{} }

// get returns tenant's marker key. On a miss it resolves the key on a pool
// connection of its own, with the acquire bounded as Begin's is.
func (k *tenantKeys) get(ctx context.Context, pool *pgxpool.Pool, acquireTimeout time.Duration, tenant spi.TenantID) (int32, error) {
	if key, ok := k.m.Load(tenant); ok {
		return key.(int32), nil
	}
	acquireCtx, cancelAcquire := newAcquireContext(ctx, acquireTimeout)
	conn, err := pool.Acquire(acquireCtx)
	cancelAcquire()
	if err != nil {
		return 0, classifyAcquireErr(ctx, acquireCtx, "resolve tenant marker key", err)
	}
	defer conn.Release()
	return k.getOn(ctx, conn, tenant)
}

// getOn is get on a connection the caller already holds, with no transaction
// open on it.
func (k *tenantKeys) getOn(ctx context.Context, conn *pgxpool.Conn, tenant spi.TenantID) (int32, error) {
	if key, ok := k.m.Load(tenant); ok {
		return key.(int32), nil
	}
	key, err := resolveTenantKey(ctx, conn, tenant)
	if err != nil {
		return 0, err
	}
	k.m.Store(tenant, key)
	return key, nil
}

// resolveTenantKey reads tenant's key, allocating it first when the tenant has
// none, in a READ COMMITTED transaction that sets app.current_tenant for the
// table's row-level security policy, as every tenant-scoped transaction this
// plugin opens does. READ COMMITTED is what makes the allocation race-free: when two
// nodes allocate the same tenant at once, the loser's INSERT waits for the
// winner and does nothing, and the SELECT after it takes a new snapshot that
// sees the winner's row. The first SELECT spares a known tenant the INSERT,
// which draws a sequence value even when it conflicts.
func resolveTenantKey(ctx context.Context, conn *pgxpool.Conn, tenant spi.TenantID) (int32, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin tenant marker key lookup: %w", classifyError(err))
	}
	// A no-op after Commit; on a context derived WithoutCancel so a rollback
	// after the caller gave up still reaches the server.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", string(tenant)); err != nil {
		return 0, fmt.Errorf("set tenant for marker key lookup: %w", classifyError(err))
	}
	const read = `SELECT tenant_key FROM consistency_tenant_keys WHERE tenant_id = $1`
	var key int32
	err = tx.QueryRow(ctx, read, string(tenant)).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx,
			`INSERT INTO consistency_tenant_keys (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
			string(tenant)); err != nil {
			return 0, fmt.Errorf("allocate tenant marker key: %w", classifyError(err))
		}
		err = tx.QueryRow(ctx, read, string(tenant)).Scan(&key)
	}
	if err != nil {
		return 0, fmt.Errorf("read tenant marker key: %w", classifyError(err))
	}
	// classifyError, not classifyCommitOutcome: the lookup is idempotent (a
	// retry re-reads the row or re-runs the no-op INSERT), so a COMMIT whose
	// outcome is in doubt is safe to retry and reads as the retryable
	// storage-unavailable failure it is.
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tenant marker key lookup: %w", classifyError(err))
	}
	return key, nil
}

// stampLockTimeoutError marks a commit whose stamp (cyoda_stamp, migration
// 000016) could not take the floor mutex within its lock_timeout. The
// transaction rolls back and nothing was applied, so a retry may succeed: it
// carries the storage-unavailable marker, like an acquire timeout.
type stampLockTimeoutError struct{ cause error }

func (e *stampLockTimeoutError) Error() string {
	return "commit stamp: lock wait exceeded: " + e.cause.Error()
}
func (e *stampLockTimeoutError) Unwrap() error            { return e.cause }
func (e *stampLockTimeoutError) StorageUnavailable() bool { return true }

// stampError marks any failure of cyoda_stamp. The function can fail with the
// session-level floor mutex in doubt, so whoever holds the transaction closes
// its connection instead of returning it to the pool (closeIfStampFailed). It
// is transparent otherwise: no marker of its own, and Error is the cause's.
type stampError struct{ cause error }

func (e *stampError) Error() string { return e.cause.Error() }
func (e *stampError) Unwrap() error { return e.cause }

// classifyStampError marks an error from cyoda_stamp as a stampError, and a
// 55P03 inside it as a stampLockTimeoutError, which carries the
// storage-unavailable marker.
func classifyStampError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable {
		err = &stampLockTimeoutError{cause: err}
	}
	return &stampError{cause: err}
}

// closeIfStampFailed closes tx's connection when err came from cyoda_stamp, so
// the pool cannot hand out a session that may still hold the floor mutex. The
// caller's Rollback afterwards releases the closed connection, and pgxpool
// destroys it rather than reusing it.
func closeIfStampFailed(ctx context.Context, tx pgx.Tx, err error) {
	var se *stampError
	if !errors.As(err, &se) {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = tx.Conn().Close(closeCtx)
}

// waitBudgetMillis is 10 s, or the configured statement timeout when that is
// above 0 and lower (0 means no limit). A statement timeout set only in the
// DSN is not seen here; the server then ends the wait with 57014, which
// ConsistencyTime reports the same way as an exhausted budget.
func (tm *TransactionManager) waitBudgetMillis() int64 {
	b := consistencyWaitBudget
	if st := tm.statementTimeout; st > 0 && st < b {
		b = st
	}
	return b.Milliseconds()
}

// ConsistencyTime implements spi.TransactionManager by "reserve, then wait"
// (cyoda_consistency_time, migration 000016): it raises the stamp floor to
// C = max(DB clock, highest stamp issued), so every later stamp is above C,
// then waits for each of the tenant's in-flight markers — commits that hold a
// stamp at or below C and have not yet ended. The markers are found by the
// tenant's key (tenantKeys), resolved on the same connection first.
//
// It runs on a pool connection of its own, in autocommit, never on the
// transaction in ctx, so it is safe to call from inside one. The acquire is
// bounded as Begin's is: inside a transaction this is a second connection, and
// an unbounded wait for it would be hold-and-wait. A commit waited on never
// waits on a lock after its stamp (see stampCommitInstant), so the caller's
// transaction cannot be what holds it up.
//
// On an error from cyoda_consistency_time the connection is closed instead of
// being returned to the pool, so no session-level lock can outlive the error
// (the key lookup before it ends its own transaction and holds no lock, and
// its connection is released as usual). After a client-side
// cancel the server statement can keep waiting until its budget ends; until
// then it holds a server connection the pool no longer counts.
func (tm *TransactionManager) ConsistencyTime(ctx context.Context) (time.Time, error) {
	tenantID, err := resolveTenant(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", err)
	}

	acquireCtx, cancelAcquire := tm.acquireContext(ctx)
	conn, err := tm.pool.Acquire(acquireCtx)
	cancelAcquire()
	if err != nil {
		return time.Time{}, classifyAcquireErr(ctx, acquireCtx, "ConsistencyTime", err)
	}

	key, err := tm.keys.getOn(ctx, conn, tenantID)
	if err != nil {
		conn.Release()
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", err)
	}

	budget := tm.waitBudgetMillis()
	var c time.Time
	qerr := conn.QueryRow(ctx, `SELECT cyoda_consistency_time($1, $2)`, key, budget).Scan(&c)
	if qerr == nil {
		conn.Release()
		return c, nil
	}

	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	_ = conn.Hijack().Close(closeCtx)
	cancelClose()

	// A caller who gave up first gets its own context error: a client cancel
	// also reaches the server as 57014, which is not the store's failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", ctxErr)
	}
	var pgErr *pgconn.PgError
	if errors.As(qerr, &pgErr) && (pgErr.Code == pgerrcode.LockNotAvailable || pgErr.Code == pgerrcode.QueryCanceled) {
		slog.Warn("consistency time not certified within the wait budget",
			"pkg", "postgres", "budgetMs", budget, "sqlstate", pgErr.Code)
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w: %w", spi.ErrConsistencyTimeUnavailable, qerr)
	}
	return time.Time{}, fmt.Errorf("ConsistencyTime: %w", classifyError(qerr))
}
