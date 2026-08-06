package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"
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
