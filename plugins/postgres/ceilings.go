package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgDurationMillis renders a Go duration in the form PostgreSQL's
// statement_timeout / idle_in_transaction_session_timeout / lock_timeout GUCs
// accept in the startup packet: a bare integer count of milliseconds, which is
// their default unit.
//
// Nothing may pass a Go duration through verbatim. PostgreSQL's units are
// us/ms/s/min/h/d — "m" is not among them — and Go renders five minutes as
// "5m0s", which is invalid twice over. A malformed value here fails pool.Ping at
// boot for every deployment, so a test asserts the rendered form.
func pgDurationMillis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// envCeiling parses a PostgreSQL-ceiling env var. It reports whether the
// operator set it explicitly, which applyCeiling needs in order to decide
// against a value supplied in the DSN.
//
// Unlike the envInt/envDuration/envBool helpers next door, a malformed value is
// an error rather than a silent fall back to the default: a silently-defaulted
// ceiling is a silently-removed safety limit.
func envCeiling(getenv func(string) string, key string, dflt time.Duration) (time.Duration, bool, error) {
	v := getenv(key)
	if v == "" {
		return dflt, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s=%q is not a valid duration: %w", key, v, err)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("%s=%q must not be negative", key, v)
	}
	if d > 0 && d < time.Millisecond {
		// Rejected rather than accepted, and the wording covers every caller:
		// the GUC ceilings are rendered as integer milliseconds, so a
		// sub-millisecond value truncates to "0" and tells PostgreSQL "no
		// limit" — the exact inversion of the operator's intent; and the
		// Go-side acquire deadline, which has no PostgreSQL setting behind it,
		// would be too short for any acquire to complete. Both outcomes are
		// worse than saying no.
		return 0, false, fmt.Errorf(
			"%s=%q is below the 1ms resolution these limits are expressed in; "+
				"use 0 to disable the limit explicitly, or a value of at least 1ms", key, v)
	}
	return d, true, nil
}

// applyCeiling writes a ceiling into the pool's startup RuntimeParams.
//
// pgxpool.ParseConfig folds unrecognised DSN keys into RuntimeParams, so a value
// the operator set in CYODA_POSTGRES_URL is already present here. Since these
// settings now have non-zero defaults, writing unconditionally would let a
// default nobody set override a value somebody did. So: an explicitly set env
// var always wins (and says so when it is overriding), a DSN-only value is left
// alone, and the default applies when neither is present.
//
// The warning names the setting and the two rendered values only — never the
// connection string, which carries credentials.
func applyCeiling(params map[string]string, name string, d time.Duration, explicit bool) {
	dsnValue, inDSN := params[name]
	if inDSN && !explicit {
		return
	}
	if inDSN && explicit {
		slog.Warn("overriding a PostgreSQL setting supplied in CYODA_POSTGRES_URL with the environment variable",
			"pkg", "postgres", "setting", name, "dsnValue", dsnValue, "envValue", pgDurationMillis(d))
	}
	params[name] = pgDurationMillis(d)
}

// newAcquireContext bounds a connection acquire — and ONLY the acquire.
//
// pgxpool.Config has no AcquireTimeout field, so the wait for a free pooled
// connection is bounded Go-side. The caller must cancel the returned context as
// soon as the acquiring call has returned: the context a transaction then lives
// under is derived from the CALLER's context, never from this one. A deadline
// that reached the transaction handle would cancel it the moment the acquire
// window closed, killing every later operation on a perfectly healthy
// transaction.
//
// A zero timeout disables the deadline, matching the convention the GUC ceilings
// use.
func newAcquireContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// acquireTimeoutError marks a failure caused by this plugin's own
// connection-acquire deadline expiring: the pool could not supply a connection
// in time. It carries the StorageUnavailable marker the application layer
// matches with errors.As — an interface, not a concrete type, so no
// cyoda-go-spi change and therefore no coordinated cross-repo release. Another
// backend can opt in by returning the same shape.
type acquireTimeoutError struct{ cause error }

func (e *acquireTimeoutError) Error() string {
	return "could not acquire a database connection within the configured timeout: " + e.cause.Error()
}
func (e *acquireTimeoutError) Unwrap() error            { return e.cause }
func (e *acquireTimeoutError) StorageUnavailable() bool { return true }

// classifyAcquireErr distinguishes "our acquire deadline expired" from "the
// caller's request context expired". The pool surfaces a context error for both,
// and reporting a client timeout as a retryable server 503 would be wrong — so
// the caller's context is checked first, and only this plugin's own deadline
// produces the storage-unavailable marker.
//
// It is package-level, and every acquire in this plugin goes through it, because
// the three sites that open a transaction on the pool — TransactionManager.Begin,
// ExtendSchema's self-wrap and the async-search scan's own-ceiling transaction —
// contend for the same connections and must therefore say the same thing about
// running out of them. When only Begin classified, a saturated pool answered a
// retryable 503 through the write doors and a ticketed 500 through the
// schema-extension path, for one condition.
// An acquire fails for two transient reasons, not one, so the non-deadline
// branch is not a bare wrap: it runs classifyError, the same classifier every
// other statement in this plugin runs. A socket torn out from under pgx while it
// opens the transaction carries no SQLSTATE — the session was gone before the
// server could send one — and would otherwise degrade to a ticketed 500 for a
// condition a retry clears. classifyError leaves context errors alone
// (isConnectionTorn excludes them), so a caller that cancelled still never reads
// as a server-side outage.
func classifyAcquireErr(callerCtx, acquireCtx context.Context, what string, err error) error {
	if callerCtx.Err() == nil && errors.Is(acquireCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", what, &acquireTimeoutError{cause: err})
	}
	return fmt.Errorf("%s: %w", what, classifyError(err))
}

// idleInTxAbortError marks an operation that found its transaction already gone
// — the shape idle_in_transaction_session_timeout produces when it reclaims a
// transaction that sat idle past the ceiling. Transient contention, like pool
// exhaustion: the transaction is lost, but the same request on a fresh one may
// well succeed, so it carries the same StorageUnavailable marker.
//
// The message does not name the ceiling, because one of the two shapes below is
// a bare torn socket that a network fault produces identically. Whenever the
// server did say why, the wrapped cause carries its SQLSTATE, and classifyError
// logs the setting by name.
type idleInTxAbortError struct{ cause error }

func (e *idleInTxAbortError) Error() string {
	return "the transaction was reclaimed before the operation completed: " + e.cause.Error()
}
func (e *idleInTxAbortError) Unwrap() error            { return e.cause }
func (e *idleInTxAbortError) StorageUnavailable() bool { return true }

// isIdleInTxAbort reports whether err is PostgreSQL reclaiming a transaction
// that sat idle past the ceiling.
//
// Two shapes, because 25P03 terminates the SESSION rather than merely aborting
// the transaction: pgx may read the buffered ErrorResponse and surface a
// PgError, or notice the connection is already gone and surface a transport
// error. Both are checked — the torn-socket test comes second but is not
// subordinate, since a chain can carry an unrelated PgError above a torn socket.
// It also recognises the marker classifyError mints, so the predicate and that
// marker are each other's inverse: classifying an already-classified error must
// not change the answer.
func isIdleInTxAbort(err error) bool {
	var already *idleInTxAbortError
	if errors.As(err, &already) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.IdleInTransactionSessionTimeout {
		return true
	}
	return isConnectionTorn(err)
}

// isStatementTimeout reports whether err is PostgreSQL cancelling a statement
// that exceeded statement_timeout. Reachable on any statement and any endpoint,
// including reads.
func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.QueryCanceled
}

// searchScanCeilingKey marks a context as belonging to the async-search scan.
// The value is the ceiling that scan runs under, minted by the AsyncSearchStore
// — the component that owns both the setting and the workload.
//
// A context value rather than a SearchOptions field: the scan reaches the store
// through the ordinary domain Search path, which has no async-specific
// parameter, and adding one to spi.SearchOptions would be a cross-repo SPI
// change for a detail exactly one backend acts on.
type searchScanCeilingKey struct{}

// withSearchScanCeiling marks ctx as the async-search scan's, running under d.
func withSearchScanCeiling(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, searchScanCeilingKey{}, d)
}

// searchScanCeiling reports the ceiling this context's scan runs under, and
// whether the context is an async-search scan's at all. A zero duration is a
// real answer, not an absent one — it is PostgreSQL's own convention for "no
// limit", so an operator who disables the search ceiling gets an unbounded scan
// rather than silently inheriting the interactive one.
func searchScanCeiling(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(searchScanCeilingKey{}).(time.Duration)
	return d, ok
}

// searchCeilingError marks a scan cancelled by the async-search path's own
// statement ceiling.
//
// It carries a marker method rather than a sentinel value so the domain can
// recognise the condition with errors.As on a locally-declared interface — the
// same shape as StorageUnavailable, and for the same reason: no cyoda-go-spi
// change, so no coordinated cross-repo release, and any backend that bounds its
// async scan can opt in by returning the same shape.
//
// Deliberately NOT the storage-unavailable marker. Re-running a scan that just
// exceeded its ceiling will exceed it again, so nothing here may advertise a
// retry.
type searchCeilingError struct{ cause error }

func (e *searchCeilingError) Error() string {
	return "the async search exceeded the search statement ceiling: " + e.cause.Error()
}
func (e *searchCeilingError) Unwrap() error               { return e.cause }
func (e *searchCeilingError) SearchCeilingExceeded() bool { return true }

// isConnectionTorn reports whether the session behind err went away underneath
// the operation.
//
// The membership of this set was settled by observation against a live server,
// not by reading the driver. With the ceiling at 300ms and a 2s idle gap, the
// FIRST operation after the ceiling fires surfaces
//
//	*pgconn.PgError — "FATAL: terminating connection due to idle-in-transaction
//	timeout (SQLSTATE 25P03)"
//
// and every operation after that surfaces
//
//	"failed to deallocate cached statement(s): conn closed"
//
// — pgx's own wrapping of pgconn.ErrConnClosed, because the driver has by then
// closed the connection it read that fatal response on. Both shapes therefore
// occur in one request: the entity write path issues several statements, so
// which one reaches the handler depends on how far into the request the session
// died. The remaining members are the transport faults that produce the same
// condition without a server response to read, kept because a session
// terminated mid-write can surface as any of them.
//
// Two exclusions, both deliberate:
//
//   - A caller who went away. pgx reports a cancelled or expired request context
//     through the same call, and the server is not the reason such a request
//     failed — the distinction classifyAcquireErr draws for the acquire deadline.
//   - *pgconn.ConnectError, which means a connection could not be ESTABLISHED,
//     not that a live session went away. Its causes include ones no retry clears
//     (a rejected password, a pg_hba rule), and treating it as a torn session
//     would have discardTx roll back and de-register a transaction that is
//     perfectly alive. A failed acquire is acquireTimeoutError's territory.
func isConnectionTorn(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return false
	}
	return errors.Is(err, pgconn.ErrConnClosed) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}
