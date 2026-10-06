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
- **Commit-instant stamping:** immediately before `COMMIT` — after
  read-set validation — the TM takes one instant from `cyoda_stamp` (see
  "Consistency time and commit stamping") and applies it to every row the
  transaction wrote, by narrow-column
  `UPDATE`s over `entity_versions`, `entities` and the audit events
  labelled with the transaction. The column default `CURRENT_TIMESTAMP` is
  fixed at transaction *start*; the commit-phase stamp is what dates a write.
  See "Bi-temporal versioning" below for which values move.
- **Submit-time bookkeeping:** the same instant is the transaction's
  submit time. It is recorded both in an in-process map (the fast path)
  and durably in the `submit_times` table, inside the committing
  transaction, so `GetSubmitTime` answers on any node and after a restart
  rather than only on the node that committed. Both copies carry a 1-hour
  TTL; the table's pruning runs after commit, on the pool, rate-limited —
  never inside the committing transaction, where an unscoped housekeeping
  `DELETE` could make two unrelated tenants' commits abort each other.

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

### Consistency time and commit stamping

Migration `000016_consistency_time` adds a sequence, a tenant-key table and
two functions. The contract they implement is `docs/CONSISTENCY.md` §1a.

- `cyoda_stamp_floor` — a `bigint` sequence holding the highest stamp or
  consistency time issued, in microseconds since the epoch. The migration
  sets it to the highest stamp already stored in `entity_versions` and
  `submit_times`. `search_jobs.point_in_time` is not a source: it holds a
  caller-chosen instant, which must not move the floor.
- `consistency_tenant_keys` — one row per tenant: `tenant_id` and
  `tenant_key`, an `int4` drawn from `consistency_tenant_key_seq` (starts at
  1; a `CHECK` keeps it above 0). Keys are unique, so two tenants never share
  commit markers. Rows are never deleted. Each store factory, with its
  transaction manager, keeps one cache of tenant keys; it looks a tenant's key
  up the first time it needs it, in a short `READ COMMITTED` transaction of
  its own, before any commit phase (at
  `Begin`, before a non-transactional write opens its own transaction, and at
  the start of a consistency-time call); a commit's own transaction never
  reads the table. A failed lookup fails the operation. The table carries the
  same tenant row-level security policy as the other tenant-scoped tables.
- `cyoda_stamp(tenant_key)` — called once per commit, at the top level of the
  commit transaction, by `stampCommitInstant` and `stampOwnCommitInstant` in
  place of `clock_timestamp()`. It returns `max(clock, floor + 1)` and moves
  the floor there, so no two commits share a stamp and stamps never go back.
  It also takes a transaction-level advisory lock `(tenant_key, xact_key)` —
  the commit's in-flight marker — held until the transaction ends, after its
  rows are visible.
- `cyoda_consistency_time(tenant_key, wait_budget_ms)` — raises the floor to
  `C = max(clock, floor)` so every later stamp is above `C`, then takes and
  releases a shared lock on each of the tenant's in-flight markers, waiting
  for those commits to end. The call runs on its own pool connection in
  autocommit, never on a transaction's connection. The wait budget is 10 s,
  or `CYODA_POSTGRES_STATEMENT_TIMEOUT` when that is above 0 and lower; a
  budget that runs out (`55P03` or `57014`) is `ErrConsistencyTimeUnavailable`.

Advisory keys use the two-int form (`objsubid = 2`), which nothing else in the
plugin uses (the scheduler and the migrator use the one-bigint form):
`(0, 0)` is the floor mutex, held for microseconds; `(tenant_key, n)` with
`tenant_key` from `consistency_tenant_keys` (never 0) and `n` in `1..2^31-1`
is a commit marker.

**Load.** Each consistency-time computation reads `pg_locks`, a snapshot of
the instance-wide lock table. The engine bounds this to two concurrent calls
per tenant per node. Operators who expose the API to untrusted callers at a
high request rate should rate-limit at ingress.

**Commit-phase limits.** Inside `cyoda_stamp`, `lock_timeout` is 2 s, so a
commit that cannot get the floor mutex fails with `55P03`, rolls back, and is
a retryable `503 STORAGE_UNAVAILABLE`. The function also lowers
`idle_in_transaction_session_timeout` to at most 5 s for the rest of the
transaction: a pause of more than 5 s between the stamp and `COMMIT` aborts
the commit. After the stamp, the commit touches only rows it wrote itself, so
it never waits on another transaction's lock, and a fenced read made while the
caller holds a transaction cannot deadlock with the commits it waits for.

**Roles.** The plugin connects as the owner of these objects. A non-owner role
needs `USAGE` on the schema, `SELECT, INSERT` on `consistency_tenant_keys`,
`USAGE` on `consistency_tenant_key_seq`, and `EXECUTE` on both functions
(granted to `PUBLIC` by default). Both functions are `SECURITY DEFINER`: they
run as their owner, the role that ran the migration, so the runtime role needs
nothing on `cyoda_stamp_floor` and cannot set or advance it itself. `EXECUTE`
stays with `PUBLIC`, because neither function can move the floor anywhere but
along the clock (`cyoda_stamp` to `max(clock, floor + 1)`,
`cyoda_consistency_time` to `max(clock, floor)`). They run with a fixed
`search_path` of exactly `pg_catalog, pg_temp`, and name the floor sequence
with the schema the migration ran in, so no object another role creates — in
a writable schema such as `public` on PostgreSQL 14, or in its temporary
schema — can be called in their place with their owner's privileges. The
plugin's own calls of the two functions give exact argument types, so an
overload with other types cannot be chosen in their place either.

**Schemas on the search path.** Beyond these two functions the plugin names
its tables, functions and operators without a schema, and PostgreSQL chooses
a function or operator by the best argument match across every schema on the
`search_path`. A role that may create objects in any of those schemas could
plant one that the plugin's statements, or its migrations, then run with the
plugin's or the migration role's privileges. So before it migrates, and on
every start whether or not `CYODA_POSTGRES_AUTO_MIGRATE` is set, the plugin
reads the ACL of each schema on its connection's effective `search_path`
(`pg_catalog` included) and refuses to continue when one grants `CREATE` to a
role other than the schema's owner. The check covers the server, the
`cyoda migrate` subcommand and so the Helm chart's migrate Job. It excuses a
grant that gives its grantee nothing new: to a superuser, or to a role that
inherits the owner's privileges. It does not excuse the connecting role
itself. A grant to `PUBLIC` — PostgreSQL 14's default on `public` — is
refused with:

```
postgres: refusing to migrate or start: on this connection's search_path, schema public grants CREATE to PUBLIC. A role that may create objects in a schema on the search_path can make this node's SQL run its code with this node's privileges. Revoke each grant, then restart: REVOKE CREATE ON SCHEMA public FROM PUBLIC;
```

The message lists every offending schema and grantee, each with its
`REVOKE`. Run them as the schema's owner and restart. PostgreSQL 15 and later
do not grant `CREATE` on `public` to `PUBLIC`. The check runs once, at start:
a grant made while a node runs is not detected until that node restarts. Nor
does it see a schema that does not exist yet: a role with `CREATE` on the
database can create a schema named after the plugin's role, which `$user`
then puts first on the default path. Do not give untrusted roles `CREATE` on
the database.

**Replicas.** With asynchronous replicas, a failover to a host whose clock is
behind can stamp below a consistency time already returned — the same
exposure as losing commits on an asynchronous failover. Each
`cyoda_consistency_time` call commits its floor update durably.

## Scheduled tasks and the scheduler pool

Scheduled tasks live in `scheduled_tasks`, keyed by `(tenant_id, id)`. Each
task has a life (its arm token, drawn on every arm), a status (`WAITING`,
`RUNNING`, `FAILED`) and, while `RUNNING`, a claim (claim token and owner
incarnation). Two more tables serve the scheduler: `scheduled_task_marks`,
keyed by `(tenant_id, task_id, arm_token)`, records that an owner was about
to hand a life off to a processor that is not safe to repeat; `scheduler_owners`
holds one liveness record per pnode incarnation, stamped by the database
clock. CHECK constraints enforce the row's own consistency:
`scheduled_tasks_status_chk` limits `status` to the three values,
`scheduled_tasks_claim_chk` requires a claim exactly while `RUNNING`,
`scheduled_tasks_claim_pair_chk` requires `claim_token` and `claim_owner` to
be both set or both `NULL`, `scheduled_tasks_failed_chk` requires a failure
reason exactly while `FAILED`, and `scheduled_tasks_last_error_len_chk` caps
`last_error` at 1024 bytes — the store rejects a longer one before any
statement runs. A partial unique index, `scheduled_tasks_one_running_per_entity_uq`
on `(tenant_id, entity_id) WHERE status = 'RUNNING'`, is the one place the
database itself enforces "at most one running task per entity".

**Writes in the entity transaction.** Arming, cancelling, removing a fired
task, the segment stamp and `Fail` write task rows straight into the open
entity transaction (`REPEATABLE READ`). A task row that another transaction
changed after the snapshot raises `40001`, mapped to `spi.ErrConflict`;
`40P01` maps the same way. A row the transaction wrote stays locked until it
ends.

**The scheduler pool.** Claims, marks, attempt records, give-backs, owner
records and sweeps never join the caller's transaction. They run on a pool of
their own, sized by `CYODA_POSTGRES_SCHEDULER_CONNS` connections (default
`10`, floor `2`), so entity transactions cannot starve them. The async-search
heartbeat and claim run there too. The scheduler heartbeat has one more
connection of its own, used only for `ScheduledTaskStore.Heartbeat`. Every
scheduler connection uses `READ COMMITTED`, `statement_timeout` 30s,
`idle_in_transaction_session_timeout` 10s and `lock_timeout` 2s — fixed
ceilings that overwrite whatever the DSN says — and every acquire is bounded
at 5s. `GET /scheduled-tasks` (`Query`) and every `Get` outside an open
transaction of the task's own tenant read on the main pool. Both scheduler
pools report through the existing `cyoda.storage.pool.connections` gauge,
under `pool="scheduler"` and `pool="heartbeat"` (`pool="main"` is the main
pool).

**`ClaimDue`** is one transaction on the scheduler pool: rank the due tasks
(one per entity, each tenant within its limit, tenants taking turns), lock
them with `FOR UPDATE SKIP LOCKED`, claim them with a conditional `UPDATE`,
and read their marks while the locks are held. The ranking never reads a
tenant's backlog. The claim first lists its tenants once: a loose index scan
of `scheduled_tasks_waiting_due_idx` reads each tenant's earliest `WAITING`
task (one probe per tenant with a `WAITING` task) and keeps the tenants whose
earliest task is due, plus, for a lost-owner claim, the tenants with a
`RUNNING` task. Per listed tenant, the ranking reads in
`(next_attempt_time, id)` order only the tasks that are their entity's
earliest claimable task, up to the most turns the claim can give that tenant,
plus as many lost-owner `RUNNING` tasks. Each walked row is checked with
per-row index probes by `(tenant, entity)` — `scheduled_tasks_waiting_entity_idx`
and `scheduled_tasks_one_running_per_entity_uq` — written as `LIMIT 1` scalar
subqueries so no plan turns them into a scan of the tenant's rows, and against
the claim's exclusions through a hashed `= ANY`. The cut comes after the
one-per-entity choice, so the result is the one a ranking of every due task
would give. A ranking round costs, per listed tenant, the rows it passes
before that tenant's last turn: its candidates, the busy or excluded rows,
the tasks of entities with a `RUNNING` task, and the later due tasks of the
entities already met — at most *n* times the tasks one entity can hold, which
is one per scheduled transition of its current state, never growing with the
number of the tenant's due entities. A claim ranks again after
each round that finds busy rows or held entities, about one round per
per-tenant limit of them. The claim transaction turns JIT off and forces
custom plans (`set_config(..., true)`). A per-entity, transaction-scoped
PostgreSQL advisory lock serialises claimers: a claimer takes an entity's lock
for the rest of its transaction, and a rival claimer of the same entity can
only take that lock once this one has committed or rolled back — PostgreSQL
makes a commit visible before it releases the lock. So a claim's own `UPDATE`
sees, for every entity it holds, either no sibling `RUNNING` or a sibling whose
commit is already visible; its `NOT EXISTS` check passes over that entity
either way, and a claim never meets a sibling claim at
`scheduled_tasks_one_running_per_entity_uq`. A row an open entity transaction
holds, or that another claimer's advisory lock already covers, is passed over
rather than waited for, and its turn goes to the next claimable task, a
sibling on the same entity included. Any claim error — including a unique
violation at that index, which would mean the invariant above was somehow
broken — is returned and the claim rolls back: a lock wait is
`spi.ErrTaskBusy`, a deadlock `spi.ErrConflict`, nothing is swallowed.

**`MarkUnsafe`** share-locks the task row with `NOWAIT` and inserts the mark.
A row held by another transaction answers `spi.ErrTaskBusy` at once.

**Errors.** SQLSTATE classes `22`, `23` and `42` carry `spi.ErrStoreRejected`
in every store of the plugin — the database will refuse the same statement
again. A lock wait past `lock_timeout` (`55P03`) answers `spi.ErrTaskBusy`
for `MarkUnsafe`, `RecordAttempt`, and the async-search `Heartbeat` (it
stamps `search_jobs` and runs on this same scheduler pool); a busy heartbeat
tick is treated as transient and missed, not a lost claim, and is retried on
the next tick. `ScheduledTaskStore.Heartbeat` itself is a plain upsert into
`scheduler_owners` with no row contention to answer busy for.

**Tenant isolation.** Every tenant-facing statement filters on `tenant_id`.
`ClaimDue`, `GiveBackIdle`, the owner methods and the sweeps are cross-tenant
and no API reaches them, so none of the three tables is under row-level
security. The comment in migration `000004` claims every write carried a
tenant predicate; that was not so, and `000014` records the correction.

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
    transaction_id   TEXT        NOT NULL DEFAULT '',            -- 000012
    creation_date    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP, -- 000012
    doc              JSONB       NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, version),
    FOREIGN KEY (tenant_id, entity_id)
        REFERENCES entities (tenant_id, entity_id) ON DELETE RESTRICT  -- 000012
);
```

- `valid_time` — **the commit instant of the transaction that wrote this
  revision.** Nothing supplies it: no API accepts a caller-chosen
  effective time, so "application-supplied" was never true of this
  column. It is defined as the instant a revision became effective, and
  it equals `transaction_time` for every write the system can make today;
  the two are kept separate because a backdated write — which would carry
  an earlier `valid_time` against a current `transaction_time` — is the
  case the bi-temporal model reserves them for, and that is **not
  implemented**.
- `transaction_time` — the commit instant of the same transaction: when
  the revision became visible. The `DEFAULT CURRENT_TIMESTAMP` is a
  provisional value only, holding the column non-null between the insert
  and the commit-phase stamp that overwrites it; `CURRENT_TIMESTAMP` is
  fixed at transaction start and is exactly the value the stamp exists to
  replace. This is the value read back as `lastUpdateTime`.
- `wall_clock_time` — `clock_timestamp()` at the inserting statement: the
  physical moment the row was written, independent of the transaction and
  never restamped. It is the one column that answers "when was this row
  actually written", which a commit-phase stamp cannot.
- `transaction_id` — the transaction that committed the row, stamped by
  the store and never taken from the caller; the empty string for a
  non-transactional write. The commit phase finds its own rows by it
  (`idx_ev_transaction`). `GetVersionByTransaction` deliberately does
  **not** read this column — it keeps probing the document's
  `_meta.transaction_id`, because that is where a caller-supplied id
  lives and the other backends answer from it.
- `creation_date` — the commit instant of the transaction that *created*
  the entity, carried onto every later revision. A later transaction
  never restamps it.

`entities` carries the same pair of columns for the current row
(`creation_date`, `last_modified`, both migration `000012`).

**Temporal values live in columns, not in the document.** The documents no
longer carry `creation_date` / `last_modified_date` copies, and every read
path projects the columns alongside `doc`. (`_meta.transaction_id` stays in
the document — the column beside it answers a different question, as above.)
This is
what makes the commit-phase stamp a narrow-column `UPDATE` rather than a
rewrite of every JSONB document the transaction touched, which would
roughly double a transaction's write volume and end `entity_versions`'
append-only property. Migration `000012` backfills the columns from the
documents before writes stop populating them.

A single-entity as-at read probes the version chain:

```sql
SELECT doc, creation_date, transaction_time FROM entity_versions
WHERE tenant_id = $1 AND entity_id = $2
  AND valid_time <= $3
ORDER BY valid_time DESC, transaction_time DESC, version DESC
LIMIT 1;
```

`version DESC` is a required tiebreak, not a defensive one: every row a
transaction writes shares one `valid_time` and one `transaction_time`, so
a delete-then-recreate inside one transaction ties on both sort keys and
the winner would otherwise be whatever the plan happened to produce.

**A point-in-time read over a model follows entities, not revisions.** The
base query for `Search`, `Iterate` / grouped statistics and
`GetPage(asAt)` enumerates the model's rows in `entities` and probes each
one's revision at the instant through a `CROSS JOIN LATERAL` into
`idx_ev_bitemporal`, with the same ordering and tiebreak as above. It
therefore costs one index probe per *entity*, not one per revision, so the
read cost follows the size of the model rather than the length of its
history. The result set rests on three properties:
`entities` keeps a row for every entity that has ever existed (delete is
a soft delete; nothing removes the row, and the foreign key above makes
that an enforced invariant rather than a habit), an entity's model
reference never changes (see below), and an entity created after the
instant yields no lateral row and drops out.

**Row-level security (RLS):** every table has RLS enabled with a
policy that compares `tenant_id` against the session variable
`app.current_tenant`, set via `set_config(..., true)` at transaction
start:

```sql
ALTER TABLE entities ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_entities ON entities
    USING (tenant_id = current_setting('app.current_tenant', true));
```

**These policies are inert in the posture cyoda-go supports today.** The
application connects as the table owner, and RLS is `ENABLE`d but not
`FORCE`d — an owner bypasses every policy. The live mechanism is the explicit
`WHERE tenant_id = $1` predicate every statement carries; the policies are
staged for a future hardening step, not a second line of defence you can rely
on now. A tenant-scoping bug in application code **would** leak data. The
scheduler's three tables are not under RLS at all; see "Scheduled tasks
and the scheduler pool".

Making them load-bearing needs three things together, not just one: `FORCE ROW
LEVEL SECURITY`, a non-owner role, and `app.current_tenant` set on the pool
path for the whole plugin (a `pgxpool` `AfterConnect`/`BeforeAcquire` hook).
The GUC is set with `set_config(..., true)`, which is transaction-local, so no
pool-routed statement carries it today — under a non-owner role those reads
would match no row and answer a confident, wrong "not found".

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
| `submit_times` | Durable transaction submit instants (1-hour TTL) | `(tenant_id, tx_id)` |
| `consistency_tenant_keys` | Per-tenant commit-marker key (consistency time) | `tenant_id`; `tenant_key` unique |
| `scheduled_tasks` | Scheduled tasks: life, status, claim, attempt record | `(tenant_id, id)` |
| `scheduled_task_marks` | Unsafe-dispatch marks, one per life | `(tenant_id, task_id, arm_token)` |
| `scheduler_owners` | Scheduler liveness, one row per pnode incarnation | `owner` |

Workflows live in `kv_store` under a dedicated namespace.

**An entity's model reference is immutable.** `model_name` /
`model_version` are fixed when the entity row is created; the `entities`
upsert refuses to change them on an existing row and `Save` returns
`spi.ErrEntityModelMismatch` (surfaced as `400 ENTITY_MODEL_MISMATCH`)
instead of rewriting them. All three in-tree backends enforce this, and
the SPI conformance suite requires it of every backend. Rewriting the
reference would strand the entity's earlier-model history: a point-in-time
read issued under the original model would lose the entity entirely, with
no error.

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
supporting index is `idx_entities_model_entity_id` (migration `000008`,
rebuilt by `000011`; see those migrations' operator note below). Since
`000011` the index covers every entity of a model rather than only the
live ones: a point-in-time read must see an entity deleted *since* the
instant, whose current row carries `deleted = true`, so the index can no
longer carry a `WHERE NOT deleted` predicate. Current-state reads keep
that predicate in the query and filter after the index lookup. This order is stable and
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
| `CYODA_POSTGRES_SCHEDULER_CONNS` | `10` | Size of the scheduler pool (see "Scheduled tasks and the scheduler pool"). At least `2`; an invalid value fails startup. The scheduler heartbeat has one more connection of its own. |
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
(tenant isolation) and for the commit-phase limits in `cyoda_stamp` — no
session-level state fights PgBouncer transaction mode beyond the
prepared-statement cache. The one session-level lock is the floor mutex
`(0, 0)` taken inside `cyoda_stamp` and `cyoda_consistency_time`, for
microseconds. It cannot leak: it is taken in a guarded block that releases it
on every exit, including a cancel, and a connection on which either function
returned an error is closed instead of going back to the pool.

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
- **Migrations `000011`, `000012` and `000013` each block writers on an
  upgrade of a populated deployment**, for the same structural reason and
  with the same plain-`CREATE INDEX` choice. Size one maintenance window
  to cover all three.
  - `000011` rebuilds `idx_entities_model_entity_id` without its partial
    predicate. It builds the replacement under a temporary name first and
    only then drops and renames, so **readers are blocked for a moment
    rather than for the build** — the naive drop-then-create holds
    `AccessExclusiveLock` across the whole build, because the file runs as
    one implicit transaction, and blocks every `SELECT` against `entities`
    cluster-wide for its duration.
  - `000012` adds the four temporal columns, backfills them from the
    documents, adds `idx_ev_transaction`, adds the `entity_versions →
    entities` foreign key, and creates `submit_times`. The foreign key is
    added `NOT VALID` and validated by a following statement, but both run
    inside one implicit transaction — the whole file does — so the
    `SHARE ROW EXCLUSIVE` the `ADD CONSTRAINT` takes is held until the file
    commits, across the historical validation scan of `entity_versions`.
    **Writers to `entities` and `entity_versions` are blocked for that whole
    span**; size the window for it. Readers are unaffected.
    The backfill itself is a full pass over `entity_versions` and
    `entities` — its duration scales with history, not with live entities.
  - `000013` adds `idx_sm_events_tenant_tx`, the index the commit-phase
    audit stamp filters on. A plain `CREATE INDEX` takes `SHARE` on
    `sm_audit_events`, so audit writers — which now includes every
    committing transaction — block for the build; readers never do.
- **Migration `000014` blocks readers and writers of `scheduled_tasks`** while
  it runs. It alters the table (a rewrite, for the per-row arm token), backfills
  it and builds five indexes in one implicit transaction under
  `ACCESS EXCLUSIVE`. No other table is locked. The table holds one row per
  armed timer.

## When to use / when not to use

**Use:** clustered production, high consistency requirements,
audit/compliance workloads, any deployment where a managed PostgreSQL
platform is the infrastructure baseline.

**Don't use:** single-process desktop deployments (use `sqlite`),
workloads whose write volume exceeds what a single Postgres primary
can sustain (consider the commercial `cassandra` plugin).
