# `sqlite` storage plugin

## Capabilities

Persistent, zero-ops single-node storage. Embedded in-process via a
pure-Go (WASM) SQLite driver — no CGO, clean cross-compilation,
future [sqlite-vec](https://github.com/asg017/sqlite-vec) support. The
ideal backend for desktop binary users, edge deployments, and
containerised single-node production.

Search predicate pushdown to SQL — the majority of entity search
predicates resolve in the SQL engine rather than post-filter in Go,
matching the PostgreSQL plugin's search shape.

## Concurrency model

Application-layer Snapshot Isolation with First-Committer-Wins
(SI+FCW). SQLite provides only database-level write locking (zero
write concurrency); cyoda's SI+FCW layer gives entity-level conflict
detection on top.

An exclusive `flock` on the database file is acquired at startup and
held for the process lifetime. A second cyoda process against the
same file fails fast with a clear error. The flock is required
because the SI+FCW state (committed-log, active-transaction set) is
per-process — two processes sharing a file would have independent
conflict-detection state and silently corrupt each other.

`flock` does not work on NFS, but SQLite itself is unreliable on NFS,
so the restriction is implicit in choosing SQLite at all.

Reads run on a second connection pool, separate from the writer. The
writer holds a single connection (SQLite is single-writer); the reader
pool defaults to `GOMAXPROCS` clamped to 4..8 and is tunable via
`CYODA_SQLITE_READER_POOL_SIZE` — see the operational notes below for
why a memory-constrained container should lower it. WAL
journal mode is what makes this safe — readers run concurrently with
each other and with the writer. Without the split, one long streaming
scan holding its cursor open would block every concurrent write and
every interactive read behind it. WAL is therefore a hard requirement,
not a tuning choice.

## Transaction manager

Application-layer SI+FCW `TransactionManager`; SQLite is the
durability layer, not the concurrency controller. Reference:
`plugins/sqlite/txmanager.go`.

## Data model and schema

Mirrors the PostgreSQL logical schema with SQLite optimisations:

- JSONB columns stored as `BLOB` with `jsonb()` / `json()` functions —
  2-5× faster `json_extract()` than TEXT JSON. Plugin asserts
  `sqlite_version() >= 3.45.0` at startup.
- `STRICT` tables + `WITHOUT ROWID` on append-only tables (e.g.
  `entity_versions`). The `entities` table keeps its rowid because it
  is UPSERT-heavy and `WITHOUT ROWID` would rewrite the clustered row
  on every update.
- INTEGER timestamps (Unix nanoseconds) — 15-25% smaller, 15-30% faster
  point lookups than TEXT timestamps.

Migrations via `golang-migrate` with embedded SQL files — same pattern
as the postgres plugin. Runs automatically on startup when
`CYODA_SQLITE_AUTO_MIGRATE=true` (the default).

## Canonical entity-ID order

`GetPage` (paged entity listing), the entity-ID tie-break under a
user-field `OrderBy`, and an explicit entity-ID `OrderBy` all order by the
sqlite plugin's canonical entity-ID order: **byte-wise ascending** — SQLite's
default `BINARY` collation, which agrees with Go's native `<` string
comparison. This order is stable and deterministic but is **not**
guaranteed identical to another storage engine's canonical order — each
in-house backend documents byte-wise ascending as its native behaviour, but
a client that depends on cross-backend identical list order is relying on
an accident, not a contract. See `docs/cloud-parity/` for the
public-API-facing statement of this rule.

## Configuration (env vars)

| Var | Default | Purpose |
|---|---|---|
| `CYODA_SQLITE_PATH` | `$XDG_DATA_HOME/cyoda/cyoda.db` on Linux/macOS (fallback `~/.local/share/cyoda/cyoda.db`); `%LocalAppData%\cyoda\cyoda.db` on Windows | Database file path |
| `CYODA_SQLITE_AUTO_MIGRATE` | `true` | Run embedded SQL migrations on startup |
| `CYODA_SQLITE_BUSY_TIMEOUT` | `5s` | Wait time for write lock before returning `SQLITE_BUSY` |
| `CYODA_SQLITE_CACHE_SIZE` | `64000` (KiB) | Page cache size in KiB, **per connection** (one writer plus the reader pool) |
| `CYODA_SQLITE_READER_POOL_SIZE` | `GOMAXPROCS` clamped to `4`..`8` | Max concurrent reader connections. Minimum 1; a value below it falls back to the default |
| `CYODA_SQLITE_SEARCH_SCAN_LIMIT` | `100000` | Max rows examined per search when a residual filter applies |

## Operational notes and limits

- **No CGO.** Uses [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3)
  (WASM-based); ~2-3× slower on micro-benchmarks than native C SQLite.
  Accepted for clean cross-compile and the sqlite-vec roadmap.
- **Tenant isolation is application-layer only.** No RLS (SQLite has no
  native row-level security). Same trust model as the memory plugin.
- **Single-process, single-node.** See concurrency model for the flock
  requirement.
- **NFS unsupported.**
- **Reader connections cost memory and file handles.** `CYODA_SQLITE_CACHE_SIZE`
  is a *per-connection* page cache, so peak page-cache use is
  `(CYODA_SQLITE_READER_POOL_SIZE + 1) × CYODA_SQLITE_CACHE_SIZE` — the writer
  holds one too. With the defaults on an 8-CPU host that is ≈ 562 MiB. The pool
  size derives from `GOMAXPROCS`, which follows the CPU quota and is blind to the
  memory limit, so a container generous on cores and tight on memory should lower
  `CYODA_SQLITE_READER_POOL_SIZE` — not `CYODA_SQLITE_CACHE_SIZE`, which shrinks
  the writer's cache along with the readers'. Idle reader connections are closed
  after 5 minutes of inactivity, and the pool is read-only (`PRAGMA query_only`).

## When to use / when not to use

**Use:** desktop binary users, containerised single-node production,
embedded deployments, edge devices, any scenario where "memory plugin
but must survive restart" is the requirement.

**Don't use:** multi-node deployments, multi-process deployments,
NFS-mounted storage, workloads that need horizontal write scale (go to
postgres or the commercial cassandra plugin).
