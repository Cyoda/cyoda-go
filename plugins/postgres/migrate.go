package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationRuntimeParams returns the connection settings a migration needs —
// the inverse of the app pool's.
//
// Doing work for a long time is fine: a migration's DDL may legitimately run for
// minutes, so the statement and idle-in-transaction ceilings are disabled
// outright. WAITING is what must be bounded. lock_timeout caps both a DDL lock
// wait and golang-migrate's pg_advisory_lock wait, which is otherwise unbounded
// at the Go level because its Lock() uses context.Background().
//
// 5m rather than something tighter: a migration's own DDL lock waits are
// legitimate during a rolling upgrade, and an old node's in-flight write
// transaction is itself bounded by the app-pool ceilings — so a bounded wait
// succeeds where a 30s one would abort a healthy upgrade.
//
// 0 is PostgreSQL's own convention for "no limit", and the values are rendered
// as a bare integer count of milliseconds — never a Go duration string, which
// PostgreSQL cannot parse. See pgDurationMillis.
func migrationRuntimeParams(lockTimeout time.Duration) map[string]string {
	return map[string]string{
		"lock_timeout":                        pgDurationMillis(lockTimeout),
		"statement_timeout":                   "0",
		"idle_in_transaction_session_timeout": "0",
	}
}

// openDB creates an independent *sql.DB from the pool's config, with the
// migration settings applied.
//
// pool.Config() deep-copies RuntimeParams, so these overrides cannot leak back
// into the app pool — asserted by TestOpenDB_DoesNotLeakIntoTheAppPool rather
// than assumed.
func openDB(pool *pgxpool.Pool, lockTimeout time.Duration) *sql.DB {
	connCfg := *pool.Config().ConnConfig
	if connCfg.RuntimeParams == nil {
		connCfg.RuntimeParams = map[string]string{}
	}
	for k, v := range migrationRuntimeParams(lockTimeout) {
		connCfg.RuntimeParams[k] = v
	}
	return stdlib.OpenDB(connCfg)
}

// ensureSchema is the single startup schema sequence, shared by the plugin
// factory and the `cyoda migrate` subcommand so the ordering guarantee holds for
// both by construction.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration) error {
	return ensureSchemaWith(ctx, pool, autoMigrate, lockTimeout, nil)
}

// ensureSchemaWith is ensureSchema with a test seam. afterMigratorBuilt, when
// non-nil, runs at the analogous point in each phase: right after a migrator has
// been constructed (which itself takes and releases golang-migrate's advisory
// lock inside ensureVersionTable) and before that phase's first unlocked
// observation of the schema. That gap is the entire concurrent-boot race window
// and it cannot be reached from outside these functions. Production passes nil.
func ensureSchemaWith(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration, afterMigratorBuilt func()) error {
	// Migrations FIRST when this binary is the one migrating.
	//
	// m.Up() takes golang-migrate's advisory lock before reading the dirty flag,
	// so a node booting alongside a peer that is mid-migration blocks until that
	// peer has finished and cleared it, then applies nothing and gets
	// ErrNoChange. Once m.Up() returns, the schema is settled: any peer that
	// would stamp dirty must first acquire the same lock, and can only do so
	// before or after this whole call — and if after, it finds nothing to apply.
	//
	// Reading the flag first, as the sequence used to, reads it outside any lock
	// and reports a peer's in-progress migration as a fatal dirty schema,
	// inviting an operator to hand-edit schema_migrations mid-migration.
	//
	// With autoMigrate=false the ordering is moot — nothing here migrates — and a
	// dirty read is accurate information about a migration running under someone
	// else's control, which this node must not start against.
	if autoMigrate {
		if err := runMigrationsWith(ctx, pool, lockTimeout, afterMigratorBuilt); err != nil {
			return fmt.Errorf("postgres migrate: %w", migrationLockWaitError(err, lockTimeout))
		}
	}

	// The compat check now runs on a settled schema. dirty == true here
	// unambiguously means a migration genuinely died, and stays fatal.
	//
	// Running migrations first does not weaken the newer-than-code guard:
	// golang-migrate refuses to plan from a version its own source has no
	// migration for, which runMigrationsWith restates as the same refusal this
	// check produces.
	db := openDB(pool, lockTimeout)
	defer db.Close()
	return migrationLockWaitError(checkSchemaCompatWith(ctx, db, autoMigrate, afterMigratorBuilt), lockTimeout)
}

// migrationLockWaitError restates a migration-lock wait that lock_timeout
// aborted, and returns every other error untouched.
//
// Both phases take golang-migrate's advisory lock, so a peer whose migration
// outruns the bound aborts this node's wait and it refuses to start — the
// intended trade, since a node that cannot establish a settled schema must not
// serve against it. golang-migrate reports that as a bare cancelled
// `pg_advisory_lock`, which names neither the cause nor the remedy.
//
// Matched on the SQLSTATE rather than with errors.As because golang-migrate's
// database.Error keeps its cause in a plain field and implements no Unwrap, so
// the typed *pgconn.PgError is unreachable. The SQLSTATE is a wire constant.
func migrationLockWaitError(err error, lockTimeout time.Duration) error {
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE "+pgerrcode.LockNotAvailable) {
		return err
	}
	return fmt.Errorf(
		"timed out after %s waiting for the schema-migration lock: another node or `cyoda migrate` "+
			"is migrating this database. This node will not start against an unsettled schema — retry, "+
			"or raise CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT: %w", lockTimeout, err)
}

// dirtySchemaError is the operator-facing message for a schema left mid-
// migration. Both routes into that state — golang-migrate's own pre-flight check
// inside m.Up(), and the compat check's version read — produce this exact text,
// so the recovery procedure is reachable from either. Without the translation,
// running migrations first would leave an operator with golang-migrate's bare
// "Dirty database version N. Fix and force version.", which says nothing about
// the INVALID index a failed CREATE INDEX CONCURRENTLY leaves behind.
func dirtySchemaError(version int) error {
	return fmt.Errorf(
		"database migration state is dirty at version %d — manual intervention required; "+
			"run `cyoda help cli migrate` for the recovery procedure", version)
}

// schemaNewerThanBinaryError is the operator-facing refusal for a database
// migrated past what this binary knows how to run. Shared for the same reason
// dirtySchemaError is: the compat check and the migrator each detect the
// condition on their own, and an operator must meet one message, not two.
func schemaNewerThanBinaryError(dbVersion, maxVersion uint) error {
	return fmt.Errorf(
		"database schema version %d is newer than this binary's max migration version %d — "+
			"refusing to start to avoid data corruption", dbVersion, maxVersion)
}

// newerThanBinaryError returns the refusal when m's database is recorded at a
// version past the highest migration src embeds, and nil in every other case —
// including when the version cannot be established, where the caller's original
// error is the more honest thing to report.
func newerThanBinaryError(m *migrate.Migrate, src source.Driver) error {
	maxVersion, err := maxMigrationVersion(src)
	if err != nil {
		return nil
	}
	dbVersion, _, err := m.Version()
	if err != nil || dbVersion <= maxVersion {
		return nil
	}
	return schemaNewerThanBinaryError(dbVersion, maxVersion)
}

// runMigrations applies pending migrations. Uses m.GracefulStop to honor
// context cancellation at migration-step boundaries (golang-migrate's
// m.Up() itself takes no context).
func runMigrations(ctx context.Context, pool *pgxpool.Pool, lockTimeout time.Duration) error {
	return runMigrationsWith(ctx, pool, lockTimeout, nil)
}

// runMigrationsWith is runMigrations carrying ensureSchemaWith's test seam. See
// ensureSchemaWith for what afterMigratorBuilt is for; production passes nil.
func runMigrationsWith(ctx context.Context, pool *pgxpool.Pool, lockTimeout time.Duration, afterMigratorBuilt func()) error {
	db := openDB(pool, lockTimeout)
	defer db.Close()

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}

	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	if afterMigratorBuilt != nil {
		afterMigratorBuilt()
	}

	done := make(chan error, 1)
	go func() {
		err := m.Up()
		var dirty migrate.ErrDirty
		switch {
		case errors.Is(err, migrate.ErrNoChange):
			err = nil
		case errors.As(err, &dirty):
			err = dirtySchemaError(dirty.Version)
		case errors.Is(err, fs.ErrNotExist):
			// golang-migrate refuses to plan from a version its own source has
			// no migration for, and says only "no migration found for version
			// N". That is what an older binary meets when it boots against a
			// schema a newer one has already migrated, so restate it as the
			// compatibility refusal an operator can act on.
			//
			// Only when the database really is ahead: a version BELOW this
			// binary's max means the embedded source has a gap, which is a
			// packaging fault that must keep its own error rather than be
			// excused into a start.
			if newer := newerThanBinaryError(m, src); newer != nil {
				err = newer
			}
		}
		done <- err
	}()

	select {
	case <-ctx.Done():
		select {
		case m.GracefulStop <- true:
		default:
		}
		<-done // wait for the goroutine to exit so we don't leak it
		return fmt.Errorf("postgres migrate: %w", ctx.Err())
	case err := <-done:
		return err
	}
}

// Migrate preserves the existing exported API for test fixtures. It applies the
// shipped lock-timeout default: a fixture migrates a database nothing else is
// touching, so its lock waits are uncontended and there is no second source of
// truth to keep in step.
func Migrate(pool *pgxpool.Pool) error {
	return runMigrations(context.Background(), pool, defaultMigrateLockTimeout)
}

// migratePoolConfig builds the pool config the `cyoda migrate` subcommand runs
// on: minimal, because the process is short-lived, and carrying the migration
// settings explicitly.
//
// Explicitly, because this pool inherits nothing from the app pool but DOES
// inherit RuntimeParams embedded in the DSN — pgxpool.ParseConfig folds
// unrecognised keys there. Relying on openDB alone would leave a
// statement_timeout an operator put in CYODA_POSTGRES_URL in force on
// pool.Ping and on the pool's own statements, bounding the migration's DDL by
// the back door.
//
// A separate function so a test can assert the settings on a live connection
// without driving the whole subcommand.
func migratePoolConfig(dsn string, lockTimeout time.Duration) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	// Minimal pool for a short-lived migrate process: no idle connections,
	// no background health-check goroutines.
	poolCfg.MaxConns = 2
	poolCfg.MinConns = 0

	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	for k, v := range migrationRuntimeParams(lockTimeout) {
		poolCfg.ConnConfig.RuntimeParams[k] = v
	}
	return poolCfg, nil
}

// RunMigrateWithDSN is the entry point for the `cyoda migrate` subcommand.
// It opens a connection pool from dsn, runs the same ensureSchema sequence a
// booting node runs, and closes the pool before returning. The caller supplies
// a context for timeout/cancellation control.
//
// Returns a descriptive error when:
//   - dsn is empty
//   - the pool cannot be opened or pinged
//   - the schema is newer than this binary's embedded migrations
//   - the migration state is dirty (manual intervention required)
//   - the wait for the migration lock exceeds CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT
//   - any migration step fails
func RunMigrateWithDSN(ctx context.Context, dsn string) error {
	if dsn == "" {
		return fmt.Errorf("CYODA_POSTGRES_URL required for postgres migrations")
	}

	// Read the same way the plugin does, so the `cyoda migrate` subcommand and
	// the server cannot disagree about the bound.
	lockTimeout, _, err := envCeiling(os.Getenv, "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT", defaultMigrateLockTimeout)
	if err != nil {
		return fmt.Errorf("postgres migrate: %w", err)
	}

	poolCfg, err := migratePoolConfig(dsn, lockTimeout)
	if err != nil {
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("open postgres pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}

	// The same sequence the plugin factory runs, so the `cyoda migrate`
	// subcommand and a booting server cannot disagree about it.
	// autoMigrate=true here: we are the migration process.
	return ensureSchema(ctx, pool, true, lockTimeout)
}

// dropSchema drops all application tables and the migration tracking table by
// running DROP SCHEMA CASCADE + CREATE SCHEMA. This is faster and more robust
// than MigrateDown for test cleanup because MigrateDown can fail when test
// data violates constraints introduced by a DOWN migration (e.g. duplicate
// primary-key values across tenants when reverting a composite-PK migration).
//
// dropSchema is intentionally unexported. Test code accesses it through
// DropSchemaForTest declared in export_test.go. This prevents production
// binaries from ever being able to call it, even by mistake (e.g. a
// misconfigured CYODA_TEST_DB_URL pointing at a production database).
//
// Before dropping the schema, dropSchema terminates all OTHER backends
// connected to the same database. This is necessary because DROP SCHEMA
// CASCADE requires an AccessExclusive lock on every object it drops, and idle
// connections from the main conformance pool can hold those locks open
// (preventing the DROP from proceeding) even after the pool's Close() has
// been called but before puddle has fully drained its wait-group.
func dropSchema(pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		return fmt.Errorf("acquire connection for dropSchema: %w", err)
	}
	defer conn.Release()

	// Terminate other backends so DROP SCHEMA can acquire its exclusive locks
	// without waiting. This is safe in test environments.
	_, _ = conn.Exec(context.Background(),
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND datname = current_database()")

	if _, err := conn.Exec(context.Background(),
		"DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public"); err != nil {
		return fmt.Errorf("dropSchema: %w", err)
	}
	return nil
}

// checkSchemaCompat enforces the schema-compatibility contract on startup:
//   - schema newer than code → fatal, regardless of autoMigrate
//   - schema older than code, autoMigrate=false → fatal
//   - schema older than code, autoMigrate=true → proceed
//   - schema matches → proceed
//   - dirty state → fatal, manual intervention required
//
// On the ensureSchema path this runs AFTER migrations, so the older-than-code
// and dirty branches are reached only when a migration silently did nothing —
// they are the backstop, not the normal route.
func checkSchemaCompat(ctx context.Context, db *sql.DB, autoMigrate bool) error {
	return checkSchemaCompatWith(ctx, db, autoMigrate, nil)
}

// checkSchemaCompatWith is checkSchemaCompat carrying ensureSchemaWith's test
// seam. See ensureSchemaWith for what afterMigratorBuilt is for; production
// passes nil.
func checkSchemaCompatWith(ctx context.Context, db *sql.DB, autoMigrate bool, afterMigratorBuilt func()) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("schema compat: context cancelled: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("schema compat: create driver: %w", err)
	}
	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("schema compat: open migration source: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("schema compat: create migrator: %w", err)
	}
	if afterMigratorBuilt != nil {
		afterMigratorBuilt()
	}

	maxVersion, err := maxMigrationVersion(src)
	if err != nil {
		return fmt.Errorf("schema compat: scan embedded migrations: %w", err)
	}

	dbVersion, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		dbVersion = 0 // fresh database — treat as older-than-code
	case err != nil:
		return fmt.Errorf("schema compat: read DB version: %w", err)
	}
	if dirty {
		return fmt.Errorf("schema compat: %w", dirtySchemaError(int(dbVersion)))
	}

	switch {
	case dbVersion > maxVersion:
		return fmt.Errorf("schema compat: %w", schemaNewerThanBinaryError(dbVersion, maxVersion))
	case dbVersion < maxVersion && !autoMigrate:
		return fmt.Errorf("schema compat: database schema version %d is older than code (%d) and CYODA_POSTGRES_AUTO_MIGRATE=false — set CYODA_POSTGRES_AUTO_MIGRATE=true and restart, or apply migrations out-of-band", dbVersion, maxVersion)
	}
	return nil
}

// maxMigrationVersion walks the embedded migration source and returns
// the highest version present.
func maxMigrationVersion(src source.Driver) (uint, error) {
	v, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("first migration: %w", err)
	}
	max := v
	for {
		next, err := src.Next(max)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("next migration after %d: %w", max, err)
		}
		max = next
	}
	return max, nil
}

// migrateDown rolls back all applied migrations. Intentionally unexported —
// exposed to test code only through MigrateDownForTest in export_test.go.
func migrateDown(pool *pgxpool.Pool, lockTimeout time.Duration) error {
	db := openDB(pool, lockTimeout)
	defer db.Close()

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}

	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("down: %w", err)
	}
	return nil
}
