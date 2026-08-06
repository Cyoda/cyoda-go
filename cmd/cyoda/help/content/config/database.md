---
topic: config.database
title: "database configuration"
stability: stable
see_also:
  - config
  - run
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
- `CYODA_SQLITE_CACHE_SIZE` — SQLite page cache size in KiB (default: `64000`)
- `CYODA_SQLITE_SEARCH_SCAN_LIMIT` — max rows scanned per search query (default: `100000`)

The prefix `CYODA_SQLITE_` is used to namespace all SQLite configuration variables.

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

Every PostgreSQL connection is opened with these limits in place. Each accepts a Go
duration (`30s`, `5m`, `1h`); `0` disables that limit. A malformed value fails startup
rather than falling back to the default — a silently-defaulted ceiling is a silently
removed safety limit.

- `CYODA_POSTGRES_STATEMENT_TIMEOUT` — maximum run time for a single SQL statement (default: `5m`)
- `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` — maximum time a connection may sit idle inside an open transaction (default: `5m`)
- `CYODA_POSTGRES_ACQUIRE_TIMEOUT` — maximum wait for a free pooled connection before the request fails with `503 STORAGE_UNAVAILABLE` (default: `10s`)
- `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` — maximum lock wait during schema migration (default: `5m`)
- `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` — statement ceiling for async search scans, which legitimately run far longer than an interactive statement (default: `30m`)

`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` reclaims a transaction nothing is driving any more.
It must clear the longest legitimate idle gap, which is a compute-node callout bounded by
`responseTimeoutMs` (default `30s`) — set it below that and healthy work is killed
mid-flight. It applies per gap, not per transaction: a cascade writes between callouts,
which restarts the clock, so a long cascade need only keep each gap under the ceiling.

The first two can also be set in `CYODA_POSTGRES_URL` (`?statement_timeout=...`). A value
there is left alone unless the environment variable is also set, in which case the
environment variable wins and the override is logged at WARN.

See `cyoda help errors.STORAGE_UNAVAILABLE` for what a client sees when a ceiling fires.

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
