package postgres

// migrate_concurrency_test.go — the migration connection's own settings, and
// the startup schema sequence two nodes booting at once share.
//
// A migration is the inverse workload to a request: its DDL may legitimately run
// for a long time, so the ceilings that bound a request would kill it. What a
// migration must still not do is WAIT without bound, which is what lock_timeout
// covers — both for its own DDL locks and for golang-migrate's advisory lock.
//
// The ensureSchema tests below cover the other half: what a node sees when it
// boots alongside a peer that is mid-migration.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
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

// backendPID identifies the server-side session behind a connection, which is
// how a test tells a genuinely new connection from a pooled one handed back.
func backendPID(t *testing.T, conn *pgxpool.Conn) int {
	t.Helper()
	var pid int
	if err := conn.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

// TestOpenDB_DoesNotLeakIntoTheAppPool: pool.Config() deep-copies RuntimeParams
// (pgxpool/pool.go:202 -> pgx/conn.go:58 -> pgconn/config.go:156), so the
// migration overrides cannot travel back into the pool serving requests.
// Asserted rather than assumed — openDB mutates a map reached through the pool,
// and if any link in that chain shared it instead of copying it, every app
// connection opened AFTERWARDS would silently lose its ceilings.
//
// "Afterwards" is the whole difficulty, and the reason this test holds a
// connection open. RuntimeParams shape a connection's STARTUP packet and are
// never re-applied to a live session, and newPool Ping()s during construction,
// so the pool already holds an idle connection older than any call to openDB. A
// plain Acquire() here would be handed that one back and would report the app
// ceiling whether or not the pool's config had been corrupted — passing
// identically in both worlds and catching nothing. So the pre-existing
// connection is checked out and HELD, forcing the pool (MaxConns 2) to open a
// second one whose startup packet is built from the config as it stands after
// openDB ran. The backend PIDs are compared to prove that is what happened.
func TestOpenDB_DoesNotLeakIntoTheAppPool(t *testing.T) {
	pool := newPoolWithCeilings(t, 5*time.Minute, 5*time.Minute)
	ctx := context.Background()

	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the pool's existing connection: %v", err)
	}
	defer held.Release()
	heldPID := backendPID(t, held)

	_ = openDB(pool, 5*time.Minute)

	fresh, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection opened after openDB: %v", err)
	}
	defer fresh.Release()
	freshPID := backendPID(t, fresh)
	if freshPID == heldPID {
		t.Fatalf("the pool handed back backend %d again; this session predates openDB, "+
			"so its settings say nothing about whether openDB corrupted the pool config", freshPID)
	}
	t.Logf("held backend %d, opened backend %d after openDB", heldPID, freshPID)

	var got string
	if err := fresh.QueryRow(ctx, "SHOW statement_timeout").Scan(&got); err != nil {
		t.Fatalf("SHOW: %v", err)
	}
	if normalizePgTime(got) == "0" {
		t.Fatalf("migration override leaked into the app pool: a connection opened after openDB "+
			"reports statement_timeout = %q; every later app connection has lost its ceiling", got)
	}
}

// --- the `cyoda migrate` subcommand's own pool --------------------------------
//
// RunMigrateWithDSN builds a pool from the DSN alone, so it inherits nothing
// from the app pool — but pgxpool.ParseConfig folds unrecognised DSN keys into
// RuntimeParams, so a ceiling the operator put in CYODA_POSTGRES_URL arrives on
// that pool anyway and would bound the migration's DDL just as surely.

// TestE2E_MigratePoolConfig_OverridesACeilingFromTheDSN is the deterministic
// half: the exact config RunMigrateWithDSN builds, materialised into a live
// connection, with a hostile ceiling in the DSN.
func TestE2E_MigratePoolConfig_OverridesACeilingFromTheDSN(t *testing.T) {
	dsn := dsnWithParam(t, skipIfNoLiveDB(t), "statement_timeout", "1s")
	poolCfg, err := migratePoolConfig(dsn, 5*time.Minute)
	if err != nil {
		t.Fatalf("migratePoolConfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open the migrate subcommand's pool: %v", err)
	}
	t.Cleanup(pool.Close)

	for setting, want := range map[string]string{
		"statement_timeout":                   "0",
		"idle_in_transaction_session_timeout": "0",
		"lock_timeout":                        "300000",
	} {
		var got string
		if err := pool.QueryRow(ctx, "SHOW "+setting).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", setting, err)
		}
		t.Logf("migrate subcommand connection: SHOW %s = %q", setting, got)
		if normalizePgTime(got) != want {
			t.Errorf("%s = %q, want %q — the DSN's value survived onto the migration connection", setting, got, want)
		}
	}
}

// TestE2E_RunMigrateWithDSN_CompletesWithACeilingInTheDSN drives the whole
// subcommand end to end against a live server — the first coverage it has had
// at all — with a 1ms statement ceiling in the DSN.
//
// It is a smoke test, NOT the proof that the DSN's ceiling is overridden, and
// the distinction was established by mutation rather than assumed: stripping the
// explicit RuntimeParams out of migratePoolConfig leaves this test passing. The
// reason is that openDB builds the migration handle from pool.Config() and
// overlays the same settings itself, so the DDL path stays protected either way.
// What migratePoolConfig governs is the statements the POOL issues directly —
// today just pool.Ping, which clears 1ms comfortably. That is defence in depth
// against a future statement being added on the pool, and the assertion carrying
// the discriminating power is TestE2E_MigratePoolConfig_OverridesACeilingFromTheDSN
// above, which does fail under that mutation.
func TestE2E_RunMigrateWithDSN_CompletesWithACeilingInTheDSN(t *testing.T) {
	plain := skipIfNoLiveDB(t)
	dsn := dsnWithParam(t, plain, "statement_timeout", "1ms")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// A clean slate, so the run below really applies every migration rather than
	// short-circuiting on ErrNoChange and proving nothing.
	resetPool, err := pgxpool.New(ctx, plain)
	if err != nil {
		t.Fatalf("open reset pool: %v", err)
	}
	t.Cleanup(resetPool.Close)
	if err := dropSchema(resetPool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	if err := RunMigrateWithDSN(ctx, dsn); err != nil {
		t.Fatalf("RunMigrateWithDSN under a 1ms statement ceiling in the DSN: %v", err)
	}

	// The schema is now at this binary's max version: a compatibility check that
	// refuses to auto-migrate passes only when nothing is pending.
	db := openDB(resetPool, defaultMigrateLockTimeout)
	defer db.Close()
	if err := checkSchemaCompat(ctx, db, false); err != nil {
		t.Fatalf("after RunMigrateWithDSN the schema is not at max version: %v", err)
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

// --- the startup schema sequence --------------------------------------------
//
// These exercise ensureSchema, which both the plugin factory and the
// `cyoda migrate` subcommand run. Each takes a database of its own rather than
// the shared test schema: they stamp schema_migrations into states — dirty,
// ahead of this binary — that any other test running against the same database
// would trip over.

// freshDatabase creates an empty database on the test server and returns its
// DSN. A database rather than a schema because golang-migrate derives its
// advisory lock id from the database name, so two of these are genuinely
// independent migration domains.
func freshDatabase(t *testing.T) string {
	t.Helper()
	base := skipIfNoLiveDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "cyoda_boot_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse CYODA_TEST_DB_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// dsnDatabase returns just the database name from dsn, for failure messages.
//
// A DSN must never reach test output whole: url.URL.String() reproduces the
// `user:password@` it was parsed from, so a CYODA_TEST_DB_URL pointing at a
// shared server would put its credentials in a CI log. The database name is the
// only part a failure message needs — which of the throwaway databases these
// tests create was involved — and it carries nothing secret.
func dsnDatabase(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable dsn>"
	}
	return strings.TrimPrefix(u.Path, "/")
}

// TestDSNDatabase_KeepsCredentialsOutOfTestOutput pins the one property the
// helper exists for. It is cheap to reintroduce the leak by "helpfully" printing
// the whole DSN in a failure message, and CI logs are where that would surface.
func TestDSNDatabase_KeepsCredentialsOutOfTestOutput(t *testing.T) {
	const dsn = "postgres://cyoda:sup3rs3cret@db.internal:5432/cyoda_boot_abc?sslmode=disable"
	got := dsnDatabase(dsn)
	if got != "cyoda_boot_abc" {
		t.Errorf("dsnDatabase = %q, want the bare database name", got)
	}
	for _, leak := range []string{"sup3rs3cret", "cyoda:", "db.internal", "@"} {
		if strings.Contains(got, leak) {
			t.Errorf("dsnDatabase leaks %q: %s", leak, got)
		}
	}
	if got := dsnDatabase("://not a url"); strings.Contains(got, "not a url") {
		t.Errorf("an unparseable DSN was echoed back verbatim: %s", got)
	}
}

// openPool opens a pool on dsn sized for a boot sequence — ensureSchema opens
// two independent *sql.DB handles off it — with the background health check
// disabled so Close cannot race it.
func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = 6, 0
	cfg.HealthCheckPeriod = 24 * time.Hour
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool on %s: %v", dsnDatabase(dsn), err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// openStdlibDB opens a database/sql handle on dsn — what golang-migrate's
// WithInstance takes. Opening is lazy, so this connects nothing yet and is safe
// to call before handing the handle to another goroutine.
func openStdlibDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return stdlib.OpenDB(*cfg.ConnConfig)
}

// headVersion is the highest migration this binary embeds.
func headVersion(t *testing.T) int {
	t.Helper()
	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("open embedded migrations: %v", err)
	}
	v, err := maxMigrationVersion(src)
	if err != nil {
		t.Fatalf("scan embedded migrations: %v", err)
	}
	return int(v)
}

// migrateToHead brings dsn's database up to date, so a test starts from the
// state a running cluster is normally in.
func migrateToHead(t *testing.T, dsn string) {
	t.Helper()
	if err := runMigrations(context.Background(), openPool(t, dsn), defaultMigrateLockTimeout); err != nil {
		t.Fatalf("migrate %s to head: %v", dsnDatabase(dsn), err)
	}
}

// stampVersion overwrites schema_migrations, which is how a test reaches a
// state — dirty, or ahead of this binary — without a migration that produces it.
func stampVersion(t *testing.T, dsn string, version int, dirty bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, dsn)
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`); err != nil {
		t.Fatalf("ensure schema_migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`, version, dirty); err != nil {
		t.Fatalf("stamp version %d dirty=%v: %v", version, dirty, err)
	}
}

// stampDirty leaves the schema looking like a migration died part-way.
func stampDirty(t *testing.T, dsn string, version int) {
	t.Helper()
	stampVersion(t, dsn, version, true)
}

// schemaState reads back what schema_migrations records.
func schemaState(t *testing.T, dsn string) (int, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var version int
	var dirty bool
	if err := openPool(t, dsn).QueryRow(ctx,
		`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return version, dirty
}

// transientMigratingPeer models the peer whose in-flight migration the booting
// node used to false-alarm on: it takes golang-migrate's advisory lock and
// stamps dirty exactly as golang-migrate does before a step, holds both for
// `hold`, then clears and releases.
//
// It uses the driver's own Lock/SetVersion rather than recomputing the lock id,
// which would couple the test to a 32-bit CRC over an internally-derived name.
func transientMigratingPeer(t *testing.T, dsn string, version int, hold time.Duration) (started <-chan struct{}, done <-chan struct{}) {
	t.Helper()
	db := openStdlibDB(t, dsn)
	ready, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { _ = db.Close() }()
		drv, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
		if err != nil {
			t.Error(err)
			close(ready)
			return
		}
		defer func() { _ = drv.Close() }()
		if err := drv.Lock(); err != nil {
			t.Error(err)
			close(ready)
			return
		}
		_ = drv.SetVersion(version, true)
		close(ready)
		time.Sleep(hold)
		_ = drv.SetVersion(version, false)
		_ = drv.Unlock()
	}()
	return ready, finished
}

// TestEnsureSchema_ConcurrentMigratorIsNotReportedAsDirty is the race itself.
//
// The seam fires after this node has built its migrator (advisory lock taken and
// released inside ensureVersionTable) and before its first unlocked look at the
// schema — the exact window the race lives in. "Boot two nodes concurrently"
// without the seam passes whatever the ordering is, because WithInstance's own
// lock serialises them, so it would prove nothing.
func TestEnsureSchema_ConcurrentMigratorIsNotReportedAsDirty(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)

	var once sync.Once
	var peerDone <-chan struct{}
	seam := func() {
		once.Do(func() {
			var ready <-chan struct{}
			ready, peerDone = transientMigratingPeer(t, dsn, headVersion(t), 500*time.Millisecond)
			<-ready // the peer now holds the lock and has stamped dirty
		})
	}

	err := ensureSchemaWith(context.Background(), openPool(t, dsn), true, 5*time.Minute, seam)
	if peerDone != nil {
		<-peerDone
	}
	if err != nil {
		t.Fatalf("a peer's in-flight migration was reported as a fatal dirty schema: %v", err)
	}
}

// TestEnsureSchema_GenuinelyDirtySchemaStillFailsFast — no concurrent migrator:
// the flag means a migration really died.
func TestEnsureSchema_GenuinelyDirtySchemaStillFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)
	stampDirty(t, dsn, headVersion(t))

	err := ensureSchema(context.Background(), openPool(t, dsn), true, 5*time.Minute)
	if err == nil {
		t.Fatal("a genuinely dirty schema was allowed to start")
	}
	t.Logf("dirty schema refused with: %v", err)
	// The actionable message, not golang-migrate's bare "Dirty database version
	// N. Fix and force version." With migrations first, m.Up() reports dirty
	// before applying anything, so this is where an operator now meets it — and
	// the pointer to the recovery procedure must survive the reorder.
	if !strings.Contains(err.Error(), "manual intervention required") ||
		!strings.Contains(err.Error(), "cyoda help cli migrate") {
		t.Fatalf("message lost its guidance: %v", err)
	}
	// And it arrived by the migration route, not the compat check. That is the
	// whole point of translating ErrDirty: with migrations first the compat
	// check never runs here, so without the translation this guidance would be
	// unreachable on the auto-migrate path.
	if !strings.HasPrefix(err.Error(), "postgres migrate: ") {
		t.Fatalf("expected m.Up()'s own dirty verdict, translated; got %v", err)
	}
}

// TestEnsureSchema_DatabaseNewerThanBinaryStillRefuses — running migrations
// first must not weaken the newer-than-code guard.
func TestEnsureSchema_DatabaseNewerThanBinaryStillRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)
	stampVersion(t, dsn, headVersion(t)+5, false)

	err := ensureSchema(context.Background(), openPool(t, dsn), true, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("started against a database newer than the binary: %v", err)
	}
}

// TestEnsureSchema_SingleNodeMigratesItself — the uncontended case must stay
// boring.
func TestEnsureSchema_SingleNodeMigratesItself(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	if err := ensureSchema(context.Background(), openPool(t, dsn), true, 5*time.Minute); err != nil {
		t.Fatalf("single-node install failed to migrate itself: %v", err)
	}
	if v, dirty := schemaState(t, dsn); v != headVersion(t) || dirty {
		t.Fatalf("schema state = (%d, dirty=%v)", v, dirty)
	}
}

// TestRunMigrateWithDSN_ConcurrentWithNodeBoot — both entry points go through
// ensureSchema, so the CLI inherits the same ordering. This asserts that
// behaviourally rather than by inspection.
func TestRunMigrateWithDSN_ConcurrentWithNodeBoot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	pool := openPool(t, dsn)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = RunMigrateWithDSN(context.Background(), dsn) }()
	go func() {
		defer wg.Done()
		errs[1] = ensureSchema(context.Background(), pool, true, 5*time.Minute)
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("participant %d failed: %v", i, err)
		}
	}
}

// TestEnsureSchema_BoundedLockWaitSaysWhatHappened — lock_timeout bounds the
// wait a booting node does behind a peer's migration, so a migration that
// legitimately outruns it turns a slow-but-successful concurrent boot into a
// startup failure. That is the intended trade: a bounded, logged failure a
// supervisor retries beats an unbounded stall, and a node that cannot establish
// a settled schema must not start. What it must not be is inscrutable —
// golang-migrate reports it as a bare cancelled `pg_advisory_lock`.
func TestEnsureSchema_BoundedLockWaitSaysWhatHappened(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)

	started, peerDone := transientMigratingPeer(t, dsn, headVersion(t), 3*time.Second)
	<-started // the peer holds the migration lock

	err := ensureSchema(context.Background(), openPool(t, dsn), true, 300*time.Millisecond)
	<-peerDone
	if err == nil {
		t.Fatal("started while a peer held the migration lock")
	}
	t.Logf("bounded lock wait refused with: %v", err)
	for _, want := range []string{
		"waiting for the schema-migration lock",
		"CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}
}

// seamOnCall returns a seam that fires fn on the nth migrator ensureSchemaWith
// builds — 1 is the migration phase's, 2 the compatibility check's. The phases
// take the advisory lock independently, so each has a window of its own and a
// test has to say which one it is aiming at.
func seamOnCall(n int, fn func()) func() {
	var calls int
	return func() {
		calls++
		if calls == n {
			fn()
		}
	}
}

// TestEnsureSchema_NewerPeerMigratingIsNotReportedAsDirty is the compatibility
// phase's half of the same race.
//
// Running migrations first settles the schema against every peer running THIS
// binary — after m.Up() returns, a same-version peer has nothing left to apply.
// A NEWER binary does: it carries a migration this one does not embed. So it can
// still take the lock and stamp dirty inside the compat phase's own unlocked
// window, and an old node restarting during a mixed-binary rolling upgrade meets
// it. The verdict must be the accurate refusal — the schema is ahead of this
// binary — and never the dirty alarm that tells an operator to go and repair a
// migration which is at that moment running normally on another node.
func TestEnsureSchema_NewerPeerMigratingIsNotReportedAsDirty(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)

	var peerDone <-chan struct{}
	seam := seamOnCall(2, func() {
		var ready <-chan struct{}
		ready, peerDone = transientMigratingPeer(t, dsn, headVersion(t)+1, 500*time.Millisecond)
		<-ready // the peer holds the lock and has stamped dirty at head+1
	})

	err := ensureSchemaWith(context.Background(), openPool(t, dsn), true, 5*time.Minute, seam)
	if peerDone != nil {
		<-peerDone
	}
	if err == nil {
		t.Fatal("started against a schema a newer binary had migrated past")
	}
	t.Logf("newer peer mid-migration refused with: %v", err)
	if strings.Contains(err.Error(), "manual intervention required") {
		t.Fatalf("a newer peer's in-flight migration was reported as a schema needing manual repair: %v", err)
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("want the newer-than-binary refusal, got: %v", err)
	}
}

// TestEnsureSchema_CompatCheckWaitsOutAPeersMigration is the autoMigrate=false
// path, where the compatibility check is the only phase and therefore the only
// thing holding the line.
//
// The ordering swap does nothing here — nothing in this binary migrates — so
// this window is closed only by reading the version under the same lock a
// migrator takes. With the read locked, a peer's in-flight migration is waited
// out and the settled schema it leaves behind is accepted.
func TestEnsureSchema_CompatCheckWaitsOutAPeersMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)

	var peerDone <-chan struct{}
	seam := seamOnCall(1, func() {
		var ready <-chan struct{}
		ready, peerDone = transientMigratingPeer(t, dsn, headVersion(t), 500*time.Millisecond)
		<-ready
	})

	err := ensureSchemaWith(context.Background(), openPool(t, dsn), false, 5*time.Minute, seam)
	if peerDone != nil {
		<-peerDone
	}
	if err != nil {
		t.Fatalf("a peer's in-flight migration was reported as a fatal dirty schema: %v", err)
	}
}

// lockTimeoutFrom builds the error golang-migrate produces when lock_timeout
// aborts the given statement — a database.Error carrying the query it was
// running and the server's 55P03 underneath, exactly as the pgx driver wraps it.
// Built from the real types so the classifier is pinned against golang-migrate's
// actual rendering rather than a guess at it.
func lockTimeoutFrom(what, query string) error {
	return &database.Error{
		Err:   what,
		Query: []byte(query),
		OrigErr: &pgconn.PgError{
			Severity: "ERROR",
			Code:     pgerrcode.LockNotAvailable,
			Message:  "canceling statement due to lock timeout",
		},
	}
}

// TestMigrationLockWaitError_NamesWhichLockTimedOut — both waits abort with
// SQLSTATE 55P03, so the SQLSTATE alone cannot tell them apart. One is a peer
// holding the migration lock; the other is an ordinary transaction holding a
// table this migration's DDL needs. Reporting the second as the first tells an
// operator to go and look for a migrating node that does not exist.
func TestMigrationLockWaitError_NamesWhichLockTimedOut(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name:       "advisory lock held by a peer migrator",
			err:        lockTimeoutFrom("try lock failed", "SELECT pg_advisory_lock($1)"),
			wantSubstr: []string{"waiting for the schema-migration lock", "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT"},
		},
		{
			name:       "table lock held by a long reader",
			err:        lockTimeoutFrom("migration failed", "CREATE INDEX CONCURRENTLY idx ON entities (tenant_id)"),
			wantSubstr: []string{"waiting for a table lock", "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT"},
			notSubstr:  []string{"schema-migration lock", "is migrating this database"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := migrationLockWaitError(tc.err, 5*time.Minute)
			for _, want := range tc.wantSubstr {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("message does not mention %q: %v", want, got)
				}
			}
			for _, unwanted := range tc.notSubstr {
				if strings.Contains(got.Error(), unwanted) {
					t.Errorf("message wrongly claims %q: %v", unwanted, got)
				}
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("translation dropped the cause: %v", got)
			}
		})
	}
}

// TestMigrationLockWaitError_LeavesEverythingElseAlone — only a lock wait is
// restated; every other failure keeps its own error.
func TestMigrationLockWaitError_LeavesEverythingElseAlone(t *testing.T) {
	if got := migrationLockWaitError(nil, time.Minute); got != nil {
		t.Errorf("nil became %v", got)
	}
	other := errors.New("relation \"entities\" does not exist (SQLSTATE 42P01)")
	if got := migrationLockWaitError(other, time.Minute); got != other {
		t.Errorf("unrelated error was rewritten to %v", got)
	}
}

// TestEnsureSchema_CancelledContextSaysItOnce — runMigrations and ensureSchema
// each used to prepend "postgres migrate: ", so a cancelled boot reported
// "postgres migrate: postgres migrate: context canceled". The inner wrap is the
// one with something to say; the outer one names the phase.
func TestEnsureSchema_CancelledContextSaysItOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ensureSchema(ctx, openPool(t, dsn), true, 5*time.Minute)
	if err == nil {
		t.Fatal("a cancelled context still completed the boot sequence")
	}
	t.Logf("cancelled boot reported: %v", err)
	if n := strings.Count(err.Error(), "postgres migrate: "); n != 1 {
		t.Errorf("phase named %d times, want once: %v", n, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation is not recoverable from the error: %v", err)
	}
}
