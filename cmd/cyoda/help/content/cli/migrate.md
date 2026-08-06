---
topic: cli.migrate
title: "cyoda migrate — run storage migrations"
stability: stable
see_also:
  - config
  - cli.init
---

# cli.migrate

## NAME

cli.migrate — run schema migrations for the configured storage backend and exit.

## SYNOPSIS

`cyoda migrate [--timeout <duration>]`

## DESCRIPTION

`cyoda migrate` is a short-lived process that applies pending schema migrations for the configured storage backend, then exits cleanly — no admin listener, no background loops, no lingering goroutines.

It loads the same configuration the server does via `app.DefaultConfig`, honoring all `CYODA_*` environment variables and `_FILE` suffix resolution identically to the main server process.

Dispatch is on `CYODA_STORAGE_BACKEND`:

- **memory** — No-op; exits 0. The memory backend has no schema to migrate.
- **sqlite** — No-op; exits 0. SQLite applies migrations lazily on first open; the migrate subcommand is not needed.
- **postgres** — Runs the postgres plugin's embedded migration logic against `CYODA_POSTGRES_URL`. Exits 0 on success, 1 on error.
- **other** — Exits 1 with "unknown storage backend".

The migrate subcommand respects the schema-compatibility contract: it refuses to run if the database schema is newer than the code's embedded maximum version. This prevents a rollback from accidentally downgrading a schema.

The primary consumer is the Helm chart's pre-install and pre-upgrade Job, which runs `cyoda migrate` before starting the server to ensure the schema is up to date.

## OPTIONS

- `--timeout <duration>` — Maximum duration for the migration run (default: 5 minutes). Accepts Go duration strings: `30s`, `2m`, `10m`, etc.

## ENVIRONMENT VARIABLES

- `CYODA_STORAGE_BACKEND` — Selects the backend to migrate (default: `memory`).
- `CYODA_POSTGRES_URL` — PostgreSQL DSN, required when backend is `postgres`. Accepts `CYODA_POSTGRES_URL_FILE` variant.

## EXIT CODES

- `0` — Migration succeeded (or was a no-op for memory/sqlite).
- `1` — Runtime error: bad config, database unreachable, migration failure, or timeout.
- `2` — Flag-parse error.

## EXAMPLES

```
# Migrate postgres schema (reads CYODA_POSTGRES_URL from environment)
CYODA_STORAGE_BACKEND=postgres \
  CYODA_POSTGRES_URL="postgres://user:pass@localhost/cyoda" \
  cyoda migrate

# Migrate with a custom timeout
CYODA_STORAGE_BACKEND=postgres \
  CYODA_POSTGRES_URL="postgres://user:pass@localhost/cyoda" \
  cyoda migrate --timeout 2m

# No-op — memory backend
cyoda migrate
```

## ADDING AN INDEX MIGRATION

An index on a table that already holds data must be built with `CREATE INDEX CONCURRENTLY`, alone in its own migration file. A plain `CREATE INDEX` takes a lock that conflicts with every insert, update and delete on that table for the whole build.

`CONCURRENTLY` must be the file's only statement. The migration driver sends a file as a single simple query, and PostgreSQL wraps a multi-statement simple query in an implicit transaction — inside which `CONCURRENTLY` cannot run.

An index created in the same migration as its own table needs no `CONCURRENTLY`: that table is empty and no writer can reach it yet.

## RECOVERING FROM A DIRTY MIGRATION STATE

A migration that dies partway leaves the schema half-applied and the migration state marked dirty. Startup then refuses with `database migration state is dirty at version N`, on both PostgreSQL and SQLite.

Recovery is always the same three steps: undo what the failed migration left behind, point the recorded version back at the last good migration, re-run.

**1. Undo the partial work.** On PostgreSQL the usual cause is a `CREATE INDEX CONCURRENTLY` that failed, which leaves an INVALID index behind. Find it and drop it — concurrently, so live traffic is unaffected:

```sql
SELECT indexrelid::regclass AS index_name FROM pg_index WHERE NOT indisvalid;
DROP INDEX CONCURRENTLY <index_name>;
```

Anything else — a partly-created table, a column added but not backfilled — has to be reversed by hand against the failed migration's SQL.

**2. Reset the recorded version.** There is no `cyoda migrate` subcommand for this. Do it in SQL, with no server and no `cyoda migrate` running against the database:

```sql
UPDATE schema_migrations SET version = <N-1>, dirty = false;
```

If N is 1, delete the row instead — nothing has been applied:

```sql
DELETE FROM schema_migrations;
```

**3. Re-run.** `cyoda migrate` on PostgreSQL; on SQLite, restart the node, which applies migrations at open.

Never edit `schema_migrations` while a node may be migrating. If the flag appeared during a rolling upgrade, confirm no peer is mid-migration first — a node that finishes migrating clears the flag itself.

## SEE ALSO

- config
- cli.init
