// Package postgres is the durable PostgreSQL storage plugin for cyoda-go.
//
// It ships in the stock binary alongside the memory plugin and serves
// as the reference example for the DescribablePlugin pattern
// (ConfigVars() drives --help output) and for the txID-to-physical-handle
// bridge (pgx.Tx lookup via the internal txRegistry).
//
// # Configuration
//
// Plugin-namespaced env vars, all read via the injected getenv:
//
//	CYODA_POSTGRES_URL                (required) PostgreSQL connection string
//	CYODA_POSTGRES_MAX_CONNS          default 25
//	CYODA_POSTGRES_MIN_CONNS          default 5
//	CYODA_POSTGRES_MAX_CONN_IDLE_TIME default 5m
//	CYODA_POSTGRES_AUTO_MIGRATE       default true  (runs embedded SQL migrations at startup)
//
// Ceilings, each applied server-side and each accepting 0 to disable:
//
//	CYODA_POSTGRES_STATEMENT_TIMEOUT        default 5m   max run time for one statement
//	CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT       default 5m   max idle time inside an open transaction
//	CYODA_POSTGRES_ACQUIRE_TIMEOUT          default 10s  max wait for a free pooled connection
//	CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT default 30m  statement ceiling for async search scans
//	CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT     default 5m   max lock wait during schema migration
//
// Exceeding the acquire or idle-in-transaction ceiling is transient
// contention and surfaces as a retryable 503 STORAGE_UNAVAILABLE.
// Exceeding a statement ceiling is not: re-running the same work would
// exceed it again, so it surfaces as a 500 with a ticket, and the server
// log names the setting that fired.
//
// Not plugin-namespaced, but read here and advertised via ConfigVars():
//
//	CYODA_SCHEMA_SAVEPOINT_INTERVAL   default 64  rows per savepoint when folding schema extensions
//
// # Migrations and context cancellation
//
// NewFactory receives a startup context with a deadline. The embedded
// SQL migrations run via golang-migrate/migrate/v4, whose m.Up() method
// does not accept a context. To honor the deadline, runMigrations runs
// m.Up() in a goroutine and signals m.GracefulStop on ctx.Done() to
// interrupt at the next migration-step boundary.
//
// # TransactionManager and RLS
//
// The plugin's TM is a lifecycle tracker over a thread-safe txRegistry
// mapping txID → pgx.Tx. TM.Begin starts a REPEATABLE READ transaction
// (snapshot isolation; first-committer-wins is layered on top in the TM),
// runs SELECT set_config('app.current_tenant', $1, true) for row-level
// security (the set_config function accepts bound parameters where
// SET LOCAL does not under pgx's extended-query protocol), and records
// the handle in the registry. Stores hold a ctxQuerier that re-resolves
// the underlying pgx.Tx on every call from the passed-in context — so
// the active tx, discovered via spi.GetTransaction(ctx), is always used
// when one is present, and the pool is used otherwise.
//
// Registration:
//
//	import _ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
package postgres
