package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// consistencyWaitBudget caps how long one ConsistencyTime call waits for the
// tenant's commits in their commit phase.
const consistencyWaitBudget = 10 * time.Second

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

// classifyStampError marks a 55P03 from cyoda_stamp. Any other error is
// returned unchanged for the caller's usual classification.
func classifyStampError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable {
		return &stampLockTimeoutError{cause: err}
	}
	return err
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
// stamp at or below C and have not yet ended.
//
// It runs on a pool connection of its own, in autocommit, never on the
// transaction in ctx, so it is safe to call from inside one. The acquire is
// bounded as Begin's is: inside a transaction this is a second connection, and
// an unbounded wait for it would be hold-and-wait. A commit waited on never
// waits on a lock after its stamp (see stampCommitInstant), so the caller's
// transaction cannot be what holds it up.
//
// On an error the connection is closed instead of being returned to the
// pool, so no session-level lock can outlive the error.
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

	budget := tm.waitBudgetMillis()
	var c time.Time
	qerr := conn.QueryRow(ctx, `SELECT cyoda_consistency_time($1, $2)`, string(tenantID), budget).Scan(&c)
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
