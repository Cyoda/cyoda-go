---
topic: config.database
title: "database configuration"
stability: stable
see_also:
  - config
  - run
  - errors.STORAGE_UNAVAILABLE
---

# config.database

## NAME

config.database — storage backend selection and per-backend connection settings.

## SYNOPSIS

Select the backend with `CYODA_STORAGE_BACKEND`. Configure the chosen backend via its
per-backend variables (`CYODA_SQLITE_*` or `CYODA_POSTGRES_*`). The `memory` backend
requires no additional configuration.

## OPTIONS

### Backend selection

- `CYODA_STORAGE_BACKEND` — storage backend to use: `sqlite`, `postgres`, or `memory`
  (bare default: `memory`; `cyoda init` writes `CYODA_STORAGE_BACKEND=sqlite` to
  `~/.config/cyoda/cyoda.env`, so the effective default after `cyoda init` is `sqlite`)

### SQLite backend (`CYODA_SQLITE_*`)

Used when `CYODA_STORAGE_BACKEND=sqlite`.

- `CYODA_SQLITE_PATH` — path to the SQLite database file (default: `~/.local/share/cyoda/cyoda.db` on Linux/macOS XDG; `%LocalAppData%\cyoda\cyoda.db` on Windows)
- `CYODA_SQLITE_AUTO_MIGRATE` — run embedded SQL migrations on startup (default: `true`)
- `CYODA_SQLITE_BUSY_TIMEOUT` — busy timeout for lock contention (default: `5s`)
- `CYODA_SQLITE_CACHE_SIZE` — SQLite page cache size in KiB, **per connection** (default: `64000`)
- `CYODA_SQLITE_READER_POOL_SIZE` — max concurrent read connections (default: `GOMAXPROCS` clamped to `4`..`8`; minimum 1, and a value below it falls back to the default)

The prefix `CYODA_SQLITE_` is used to namespace all SQLite configuration variables.

**Reader pool and memory.** The page cache is per connection, so resident memory
scales with the pool: the ceiling is `(readers + 1) × CYODA_SQLITE_CACHE_SIZE`
(the writer holds one too) — with the defaults on an 8-CPU host that is
9 × 62.5 MiB ≈ 562 MiB. The pool size derives from `GOMAXPROCS`, which follows
the CPU quota and is blind to the memory limit, so a container that is generous
on cores and tight on memory must set `CYODA_SQLITE_READER_POOL_SIZE` down.
Lower it, not `CYODA_SQLITE_CACHE_SIZE` — that shrinks the writer's cache along
with the readers'. The floor of 4 exists so a small container still keeps an
interactive read off the back of a long streaming scan.

### PostgreSQL backend (`CYODA_POSTGRES_*`)

Used when `CYODA_STORAGE_BACKEND=postgres`.

- `CYODA_POSTGRES_URL` — PostgreSQL connection string, e.g. `postgres://user:pass@host/db`
  (required when using postgres backend)
- `CYODA_POSTGRES_URL_FILE` — file path for `CYODA_POSTGRES_URL` (takes precedence)
- `CYODA_POSTGRES_MAX_CONNS` — maximum pool connections (default: `25`)
- `CYODA_POSTGRES_MIN_CONNS` — minimum pool connections (default: `5`)
- `CYODA_POSTGRES_MAX_CONN_IDLE_TIME` — max idle time before closing a connection (default: `5m`)
- `CYODA_POSTGRES_AUTO_MIGRATE` — run embedded SQL migrations on startup (default: `true`)

The prefix `CYODA_POSTGRES_` is used to namespace all PostgreSQL configuration variables.

#### Ceilings

Five limits bound how long the storage layer waits or runs. Each accepts a Go duration
(`30s`, `5m`, `1h`); `0` disables that limit. A malformed value fails startup rather than
falling back to the default — a silently-defaulted ceiling is a silently removed safety
limit.

Two are carried in the connection startup packet, so every pooled connection is bounded
from its first statement:

- `CYODA_POSTGRES_STATEMENT_TIMEOUT` — maximum run time for a single SQL statement (default: `5m`)
- `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` — maximum time a connection may sit idle inside an open transaction (default: `5m`)

The other three each bound one path:

- `CYODA_POSTGRES_ACQUIRE_TIMEOUT` — deadline on the wait for a free pooled connection, after which the request fails with `503 STORAGE_UNAVAILABLE`. Applied by the pool, not the server (default: `10s`)
- `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` — maximum lock wait on the migration connection. That connection disables the two ceilings above, so a long index build is not cancelled mid-flight; what stays bounded is waiting (default: `5m`)
- `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` — statement ceiling applied per async search scan, which legitimately runs far longer than an interactive statement (default: `30m`; `0` disables it, leaving the scan unbounded)

A job that hits the async search ceiling fails with `search exceeded the backend's
async search ceiling — narrow the query, or have the operator raise or disable the
ceiling (see the config.database help topic)`. That ceiling is deliberate operator
configuration and the only bound the server puts on search work: no backend meters
rows or imposes a scan budget of its own. Bounding search **time** is the caller's —
`timeoutMillis` on direct search, job cancellation on async.

`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` reclaims a transaction nothing is driving any more.
It must clear the longest legitimate idle gap, which is a compute-node callout bounded by
`responseTimeoutMs` (default `30s`) — set it below that and healthy work is killed
mid-flight. It applies per gap, not per transaction: a cascade writes between callouts,
which restarts the clock, so a long cascade need only keep each gap under the ceiling.

`CYODA_POSTGRES_STATEMENT_TIMEOUT` and `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` can also be set
in `CYODA_POSTGRES_URL` (`?statement_timeout=...`). A value there is left alone unless the
environment variable is also set, in which case the environment variable wins and the
override is logged at WARN.

See `cyoda help errors STORAGE_UNAVAILABLE` for what a client sees when a ceiling fires.

### Memory backend

Used when `CYODA_STORAGE_BACKEND=memory`. No additional configuration needed. Data is
not persisted across restarts. Suitable for development and testing only.

## EXAMPLES

**SQLite (default after `cyoda init`):**

```
CYODA_STORAGE_BACKEND=sqlite
CYODA_SQLITE_PATH=/var/data/cyoda.db
CYODA_SQLITE_AUTO_MIGRATE=true
```

**PostgreSQL:**

```
CYODA_STORAGE_BACKEND=postgres
CYODA_POSTGRES_URL=postgres://cyoda:secret@localhost:5432/cyoda
CYODA_POSTGRES_MAX_CONNS=50
```

**In-memory (tests/dev):**

```
CYODA_STORAGE_BACKEND=memory
```

## SEE ALSO

- config
- run
- errors.STORAGE_UNAVAILABLE
