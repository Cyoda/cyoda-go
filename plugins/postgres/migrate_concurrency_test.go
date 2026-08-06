package postgres

// migrate_concurrency_test.go — the migration connection's own settings.
//
// A migration is the inverse workload to a request: its DDL may legitimately run
// for a long time, so the ceilings that bound a request would kill it. What a
// migration must still not do is WAIT without bound, which is what lock_timeout
// covers — both for its own DDL locks and for golang-migrate's advisory lock.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newPoolWithCeilings opens a pool carrying the given app-pool ceilings — the
// ones a migration connection must NOT inherit.
func newPoolWithCeilings(t *testing.T, statement, idle time.Duration) *pgxpool.Pool {
	t.Helper()
	return openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":  statement.String(),
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT": idle.String(),
	}))
}

// normalizePgTime converts SHOW's rendering of a millisecond-unit GUC into the
// bare integer millisecond count these settings are configured in.
//
// SHOW picks whichever unit renders the value most compactly — 300000 comes back
// as "5min" — so comparing its output verbatim would assert PostgreSQL's
// formatting choice rather than the value. An unrecognised string is returned
// unchanged so a mismatch reports what the server actually said.
func normalizePgTime(v string) string {
	v = strings.TrimSpace(v)
	// "us" and "ms" before "s", which is a suffix of both.
	for _, u := range []struct {
		suffix string
		unit   time.Duration
	}{
		{"us", time.Microsecond},
		{"ms", time.Millisecond},
		{"min", time.Minute},
		{"h", time.Hour},
		{"d", 24 * time.Hour},
		{"s", time.Second},
	} {
		num, ok := strings.CutSuffix(v, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
		if err != nil {
			return v
		}
		return strconv.FormatInt((time.Duration(n) * u.unit).Milliseconds(), 10)
	}
	return v
}

// acquireConn checks out a connection from a pool of its own, so two callers get
// two distinct sessions — which is what an advisory-lock contention scenario
// needs, a lock being held for the lifetime of the session that took it.
func acquireConn(t *testing.T) *pgxpool.Conn {
	t.Helper()
	pool := newPoolWithCeilings(t, defaultStatementTimeout, defaultIdleInTxTimeout)
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Registered after the pool's own Close cleanup, so LIFO releases the
	// connection before the pool it came from is torn down.
	t.Cleanup(conn.Release)
	return conn
}

func mustExec(t *testing.T, conn *pgxpool.Conn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// TestMigrationRuntimeParams_DoNotInheritAppCeilings — a migration's DDL may
// legitimately run for a long time, so inheriting the app pool's
// statement_timeout would kill a long index build. Four of the embedded
// migrations create indexes, so this is not hypothetical.
func TestMigrationRuntimeParams_DoNotInheritAppCeilings(t *testing.T) {
	pool := newPoolWithCeilings(t, 5*time.Minute, 5*time.Minute)
	db := openDB(pool, 5*time.Minute)
	defer db.Close()

	for setting, want := range map[string]string{
		"statement_timeout":                   "0",
		"idle_in_transaction_session_timeout": "0",
		"lock_timeout":                        "300000",
	} {
		var got string
		if err := db.QueryRow("SHOW " + setting).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", setting, err)
		}
		t.Logf("migration connection: SHOW %s = %q", setting, got)
		if normalizePgTime(got) != want {
			t.Errorf("%s = %q, want %q", setting, got, want)
		}
	}
}

// TestOpenDB_DoesNotLeakIntoTheAppPool: pool.Config() deep-copies RuntimeParams
// (pgxpool/pool.go:202 -> pgx/conn.go:58 -> pgconn/config.go:156), so the
// migration overrides cannot travel back into the pool serving requests.
// Asserted rather than assumed — openDB mutates a map reached through the pool,
// and if any link in that chain shared it instead of copying it, every app
// connection opened afterwards would silently lose its ceilings.
func TestOpenDB_DoesNotLeakIntoTheAppPool(t *testing.T) {
	pool := newPoolWithCeilings(t, 5*time.Minute, 5*time.Minute)
	_ = openDB(pool, 5*time.Minute)

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var got string
	if err := conn.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&got); err != nil {
		t.Fatalf("SHOW: %v", err)
	}
	if normalizePgTime(got) == "0" {
		t.Fatal("migration override leaked into the app pool; the app ceiling is gone")
	}
}

// TestLockTimeout_AbortsAnAdvisoryLockWait characterises PostgreSQL, not this
// plugin, so it passes before any of this task's code exists — and it has to,
// because the single-migrator bound rests entirely on it.
//
// golang-migrate's Lock() issues `SELECT pg_advisory_lock($1)` under
// context.Background() (golang-migrate/v4@v4.19.1 database/pgx/v5/pgx.go:229),
// so nothing on the Go side can end that wait. The claim being proven here is
// that advisory locks go through PostgreSQL's regular lock manager and are
// therefore subject to lock_timeout — asserted against a live server rather than
// taken from documentation.
func TestLockTimeout_AbortsAnAdvisoryLockWait(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	const lockID = 424242

	holder := acquireConn(t) // session A
	mustExec(t, holder, "SELECT pg_advisory_lock($1)", lockID)
	defer mustExec(t, holder, "SELECT pg_advisory_unlock($1)", lockID)

	waiter := acquireConn(t) // session B, with a short lock_timeout
	mustExec(t, waiter, "SET lock_timeout = '300ms'")
	// A backstop, 30x the lock_timeout: if the claim above were false the wait
	// would otherwise run to the pool's 5m statement ceiling before this test
	// could report it. It cannot mask the result — only lock_timeout produces
	// the 55P03 asserted below, and a statement cancellation is 57014.
	mustExec(t, waiter, "SET statement_timeout = '10s'")

	// context.Background() deliberately, mirroring golang-migrate's Lock(): the
	// point is that the Go side contributes no bound at all.
	start := time.Now()
	_, err := waiter.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockID)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("advisory lock wait was NOT aborted by lock_timeout; the single-migrator bound does not exist")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
		t.Fatalf("aborted with %v after %v, want SQLSTATE 55P03 (lock_not_available)", err, elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("waited %v; the timeout did not apply", elapsed)
	}
	t.Logf("advisory lock wait aborted after %v with SQLSTATE %s (%s)", elapsed, pgErr.Code, pgErr.Message)
}
