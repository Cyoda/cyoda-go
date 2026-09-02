# `postgres` storage plugin

## Capabilities

Durable multi-node storage backed by PostgreSQL. Each transaction
holds a `pgx.Tx` handle in one cyoda node's process memory — cyoda's
multi-node architecture pins each transaction to its owning node via
`txID → pgx.Tx` affinity, giving active-active HA without
distributed-transaction overhead.

**Works against any managed PostgreSQL 14+ platform:** AWS RDS, Google
Cloud SQL, Azure Database for PostgreSQL, Supabase, Neon, Aiven,
Crunchy Bridge, Render, Fly.io Postgres, DigitalOcean Managed
Databases, and self-hosted.

## Concurrency model

The postgres plugin runs every transaction under PostgreSQL's
`REPEATABLE READ` isolation (snapshot isolation) and layers
**application-level, row-granular first-committer-wins** validation on
top at commit time. SERIALIZABLE is not used: the plugin's
`TransactionManager` calls `pool.BeginTx(ctx, pgx.TxOptions{IsoLevel:
pgx.RepeatableRead})` and relies on a per-transaction `readSet` /
`writeSet` to detect conflicts that snapshot isolation alone would
miss.

Before `pgxTx.Commit(ctx)`, the TM re-reads the current committed
versions of every entity the transaction read, compares them against
the snapshot captured at read time, and aborts with
`spi.ErrConflict` on any mismatch. Write-write conflicts are handled
by PostgreSQL's own tuple-level locks raised from
`INSERT`/`UPDATE`/`DELETE` statements — those surface as SQLSTATE
`40001` at DML time or commit time.

**Error-code handling (`classifyError`):** two PostgreSQL error
classes mean "the database rolled this transaction back cleanly, a
retry on a fresh snapshot is safe" — `40001`
(`serialization_failure`) and `40P01` (`deadlock_detected`). Both are
wrapped into `spi.ErrConflict` so callers can retry uniformly; the
original `*pgconn.PgError` stays in the error chain for observability.

Every transaction sets `app.current_tenant` via
`SELECT set_config('app.current_tenant', $1, true)` immediately after
`BEGIN`, which RLS policies on every table consult to enforce tenant
isolation at the row level.

## Transaction manager

Full transaction-lifecycle implementation
(`plugins/postgres/transaction_manager.go`, ~366 lines) covering:

- **Lifecycle:** `Begin` / `Commit` / `Rollback` / `Join` /
  `GetSubmitTime`. `Begin` allocates a time-ordered UUID, starts a
  `REPEATABLE READ` `pgx.Tx`, sets the RLS tenant, and registers the
  transaction in the in-process `txRegistry`.
- **Savepoints:** full `Savepoint` / `RollbackToSavepoint` /
  `ReleaseSavepoint` support, backed by PostgreSQL's native
  `SAVEPOINT` / `ROLLBACK TO` / `RELEASE SAVEPOINT` plus a per-txState
  savepoint stack that snapshots and restores the application
  readSet/writeSet in lockstep with the database.
- **Row-granular validation:** commit-time re-read of the readSet
  (`validateInChunks`) drives the first-committer-wins check.
- **Transaction registry:** the `txRegistry` is a mutex-guarded
  `txID → pgx.Tx` map — the single source of truth for active
  transactions on a node.
- **Submit-time bookkeeping:** each committed transaction captures
  `SELECT CURRENT_TIMESTAMP` before `COMMIT` and records it with a
  1-hour TTL, surfaced via `GetSubmitTime`.

- **Eager deletes:** an in-transaction `Delete` writes its tombstone
  version row immediately on the transaction's connection. A re-create of
  the same entity later in that transaction therefore leaves `DELETED` then
  the re-create in the version history, where memory and sqlite (which
  buffer and cancel the delete) record only the re-create. Documented
  difference; see `docs/CONSISTENCY.md` §6.

The real serialization guarantee is the combination of PostgreSQL's
`REPEATABLE READ` snapshot + tuple locks + the TM's first-committer
validation — not `SERIALIZABLE` alone.

### `pgx.Tx` single-owner property

A `pgx.Tx` is held by exactly one goroutine on exactly one node.
There is no mechanism for two nodes to share a PostgreSQL transaction
handle: the handle is a pointer into a `pgxpool.Conn` that only exists
in the process that acquired it.

Consequences:

- No distributed locking is needed for transaction access.
- No fencing tokens are needed to prevent stale writes from a revoked
  owner — if the owning node dies, PostgreSQL rolls back the
  transaction on connection loss / idle timeout.
- cyoda's multi-node dispatch routes every subsequent operation on a
  `txID` back to the node that began it. The gossip-backed cluster
  registry advertises which node owns which transaction; any peer
  that receives a request for someone else's txID proxies the
  request rather than trying to rehydrate the handle locally.
- The `txRegistry` (`sync.RWMutex`-protected `map[string]pgx.Tx`) is
  the single source of truth for active transactions on a node.

## Data model and schema

The postgres plugin uses a normalized relational schema with JSONB
columns for flexible document storage and GIN indexes on the JSONB
columns where search requires it.

**Bi-temporal versioning:** `entity_versions` is the append-only
history table:

```sql
CREATE TABLE entity_versions (
    tenant_id        TEXT        NOT NULL,
    entity_id        TEXT        NOT NULL,
    model_name       TEXT        NOT NULL,
    model_version    TEXT        NOT NULL,
    version          BIGINT      NOT NULL,
    valid_time       TIMESTAMPTZ NOT NULL,
    transaction_time TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    wall_clock_time  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    doc              JSONB       NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, version)
);
```

- `valid_time` — application-supplied timestamp (entity's logical
  time).
- `transaction_time` — database `CURRENT_TIMESTAMP` (when PG recorded
  the row).
- `wall_clock_time` — `clock_timestamp()` (actual wall-clock,
  independent of transaction).

As-at queries filter by `valid_time`:

```sql
SELECT doc FROM entity_versions
WHERE tenant_id = $1 AND entity_id = $2 AND valid_time <= $3
ORDER BY valid_time DESC, transaction_time DESC
LIMIT 1;
```

**Row-level security (RLS):** every table has RLS enabled with a
policy that compares `tenant_id` against the session variable
`app.current_tenant`, set via `set_config(..., true)` at transaction
start:

```sql
ALTER TABLE entities ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_entities ON entities
    USING (tenant_id = current_setting('app.current_tenant', true));
```

This is defense-in-depth: even a tenant-scoping bug in application
code cannot leak data, because PostgreSQL enforces the isolation at
the row level.

**Schema (all tables):**

| Table | Purpose | Primary key |
|-------|---------|-------------|
| `entities` | Current entity state (one row per entity) | `(tenant_id, entity_id)` |
| `entity_versions` | Append-only bi-temporal history | `(tenant_id, entity_id, version)` |
| `models` | Model descriptors (JSON) | `(tenant_id, model_name, model_version)` |
| `kv_store` | Generic key-value (workflows, configs) | `(tenant_id, namespace, key)` |
| `messages` | Edge messages with binary payload | `(tenant_id, message_id)` |
| `sm_audit_events` | State-machine audit trail | `(tenant_id, entity_id, event_id)` |
| `search_jobs` | Async search job metadata | `id` (with `tenant_id` indexed) |
| `search_job_results` | Entity ID results per job | `(job_id, seq)`, FK to `search_jobs` |

Workflows live in `kv_store` under a dedicated namespace.

**Migrations:** SQL migrations ship embedded in the binary via
`//go:embed migrations/*.sql` and are applied on startup by
`golang-migrate` when `CYODA_POSTGRES_AUTO_MIGRATE=true` (the
default). Migrations run **first**; the schema-compatibility check then
runs against a settled schema (`ensureSchemaWith`). A node booting
alongside a peer's in-flight migration therefore waits for it rather
than reading the dirty flag outside any lock and exiting. A database
newer than the binary's embedded migrations is still refused — that
check reads the version under golang-migrate's own advisory lock — and
a schema left genuinely dirty by a failed migration is still a fatal
error requiring manual intervention. With
`CYODA_POSTGRES_AUTO_MIGRATE=false` the compatibility check is the only
phase. A dedicated `cyoda migrate` subcommand (`RunMigrateWithDSN`) is
available for operators who prefer to apply migrations out-of-band.

## Canonical entity-ID order

`GetPage` (paged entity listing), the entity-ID tie-break under a
user-field `OrderBy`, and an explicit entity-ID `OrderBy` all order by the
postgres plugin's canonical entity-ID order: **byte-wise ascending**,
enforced with `COLLATE "C"` on every `entity_id ORDER BY` — the database's
configured default collation may not be `"C"` and can otherwise reorder
entity IDs differently from Go's byte-wise string comparison, so `COLLATE
"C"` is pinned explicitly rather than relied on as a server default. The
supporting index is `idx_entities_model_entity_id` (migration `000008`; see
that migration's operator note below). This order is stable and
deterministic but is **not** guaranteed identical to another storage
engine's canonical order — each in-house backend documents byte-wise
ascending as its native behaviour, but a client that depends on
cross-backend identical list order is relying on an accident, not a
contract. See `docs/cloud-parity/` for the public-API-facing statement of
this rule.

## Configuration (env vars)

The plugin advertises its env vars via
`DescribablePlugin.ConfigVars()` (`plugins/postgres/plugin.go`); they
are rendered in the binary's `--help`.

| Var | Default | Purpose |
|---|---|---|
| `CYODA_POSTGRES_URL` (or `CYODA_POSTGRES_URL_FILE`) | *(required)* | PostgreSQL connection string. The `_FILE` variant reads the value from a file path and takes precedence if both are set (trailing whitespace trimmed). Implemented in `resolveSecretWith`. |
| `CYODA_POSTGRES_MAX_CONNS` | `25` | `pgxpool.Pool` maximum connections. |
| `CYODA_POSTGRES_MIN_CONNS` | `5` | `pgxpool.Pool` minimum (warm) connections. |
| `CYODA_POSTGRES_MAX_CONN_IDLE_TIME` | `5m` | Idle connection reap threshold (Go duration syntax). |
| `CYODA_POSTGRES_AUTO_MIGRATE` | `true` | Run embedded SQL migrations on startup. When `false`, the binary refuses to start if the database schema is older than the code. |
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | Maximum run time for a single SQL statement. Server-side, carried in the connection startup packet. |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | Maximum time a connection may sit idle inside an open transaction. Server-side, carried in the connection startup packet. Must clear the longest legitimate idle gap — a compute-node callout bounded by `responseTimeoutMs` (default `30s`). |
| `CYODA_POSTGRES_ACQUIRE_TIMEOUT` | `10s` | Deadline on the wait for a free pooled connection, after which the request fails with `503 STORAGE_UNAVAILABLE`. Applied by the pool, not the server — `pgxpool.Config` has no acquire-timeout field. |
| `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` | `30m` | Statement ceiling for async search scans, which legitimately run far longer than an interactive statement. Applied server-side as `SET LOCAL` in the scan's own transaction. |
| `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` | `5m` | Maximum lock wait on the migration connection. That connection disables the two statement ceilings above, so a long index build is not cancelled mid-flight; what stays bounded is waiting. |

The five ceilings each take a Go duration (`30s`, `5m`, `1h`); `0`
disables that limit. They are the only vars here that reject a
malformed value instead of falling back to the default — a
silently-defaulted ceiling is a silently removed safety limit.
`CYODA_POSTGRES_STATEMENT_TIMEOUT` and `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`
may also be set in `CYODA_POSTGRES_URL`; a value there is left alone
unless the environment variable is also set, in which case the
environment variable wins and the override is logged at WARN. See
`cyoda help config database` and `cyoda help errors STORAGE_UNAVAILABLE`.

### Managed-platform notes

Platforms that front PostgreSQL with **PgBouncer in transaction
pooling mode** (Supabase port 6543, Neon pooled endpoint) strip
prepared-statement caching mid-session. `pgx`'s default extended-query
protocol uses prepared statements.

Options:

- Use the platform's **direct-connection endpoint** (Supabase 5432,
  Neon direct) — recommended for cyoda.
- Set `default_query_exec_mode=exec` on the `pgx` pool to force
  simple-query mode — accepts a small per-query overhead in exchange
  for pooler compatibility.

cyoda uses transaction-scoped `set_config(..., true)` for RLS
(tenant isolation) only — no session-level state fights PgBouncer
transaction mode beyond the prepared-statement cache.

## Operational notes and limits

- Requires PostgreSQL 14+.
- Recommended HA mode: primary + streaming replica with automatic
  failover.
- Cluster-mode cyoda uses Postgres for durable storage and cyoda's
  own gossip registry for node discovery and transaction-owner
  routing — the two are orthogonal.
- Scale-out is bounded by the PostgreSQL primary's write capacity.
  Read-replicas are not yet wired in to cyoda.
- Schema-compatibility contract: the binary refuses to start if the
  database schema is newer than the code, and (with
  `CYODA_POSTGRES_AUTO_MIGRATE=false`) if it is older. Dirty
  migration state is fatal.
- **Migration `000008` (adds `idx_entities_model_entity_id`) blocks writers
  to `entities` for the duration of its index build**, on an upgrade of a
  populated deployment. It uses a plain `CREATE INDEX`, not `CREATE INDEX
  CONCURRENTLY` — the usual rule for an index added on a table that already
  holds data (see `cyoda help cli.migrate`, ADDING AN INDEX MIGRATION).
  `CONCURRENTLY` is deliberately not used here because it provably
  deadlocks this project's concurrent multi-node boot path: golang-migrate
  holds one session-level advisory lock for a migrator's entire run, and
  `CONCURRENTLY`'s own multi-phase build waits on every other backend's
  in-flight statement — including a second node's migrator merely blocked
  trying to acquire that same advisory lock, which still holds an active
  snapshot from PostgreSQL's perspective. That is a genuine lock cycle
  (`SQLSTATE 40P01`), reproduced empirically, not a theoretical concern. A
  plain `CREATE INDEX` avoids the deadlock at the cost of a brief
  writer-blocking window during the build — size the maintenance window to
  the `entities` table's row count before upgrading a populated instance.
  **Structural gap:** the migration runner has no retry tolerance for a
  deadlock-killed advisory-lock acquisition, so any future migration that
  adds an index to an already-populated table hits the same choice between
  `CONCURRENTLY` (deadlocks concurrent multi-node boot) and a plain
  `CREATE INDEX` (blocks writers) until the runner grows that tolerance.

## When to use / when not to use

**Use:** clustered production, high consistency requirements,
audit/compliance workloads, any deployment where a managed PostgreSQL
platform is the infrastructure baseline.

**Don't use:** single-process desktop deployments (use `sqlite`),
workloads whose write volume exceeds what a single Postgres primary
can sustain (consider the commercial `cassandra` plugin).
