---
topic: cli
title: "cyoda CLI — subcommand reference"
stability: stable
see_also:
  - config
  - run
  - quickstart
---

# cli

## NAME

cli — the cyoda command-line interface.

## SYNOPSIS

`cyoda [<subcommand>] [<flags>]`

## DESCRIPTION

cyoda is a Go binary that embeds the full platform: API server, schema engine, workflow runner, and storage plugins. Invoked with no subcommand, it starts the server using environment-provided configuration. Subcommands provide operational affordances — `init` for first-run bootstrap, `health` for liveness probes, `migrate` for schema migrations, `token` for signing an admin token offline.

Global flags `--help` (or `-h`) and `--version` (or `-v`) are recognized before subcommand dispatch.

## SUBCOMMANDS

- `cyoda` (no subcommand) — start the API server. See `cli serve`. Exit codes: `0` clean shutdown after SIGINT/SIGTERM; `1` startup failure (IAM validation, OTel init, port bind, backend connect).
- `cyoda init [--force]` — Write a starter user config enabling sqlite. See `cyoda help cli init`. Exit codes: `0` success or idempotent no-op; `1` I/O error; `2` bad flags.
- `cyoda health` — Probe `/readyz` on the admin listener. See `cyoda help cli health`. Exit codes: `0` readyz returned 200; `1` connection error or non-200 status.
- `cyoda migrate [--timeout <duration>]` — Run schema migrations for the configured backend and exit. See `cyoda help cli migrate`. Exit codes: `0` success or no-op (memory/sqlite); `1` runtime error (bad config, DB unreachable, migration failure, timeout); `2` flag-parse error.
- `cyoda token --tenant <tenantId> [--user <userId>] [--roles <r1,r2>] [--ttl <duration>]` — Sign a short-lived admin token with the signing key and print it. See `cyoda help cli token`. Exit codes: `0` token printed; `1` key or configuration error; `2` flag error.
- `cyoda help [<topic>...] [--format=<fmt>]` — Browse the help topic tree. See `cyoda help cli help`. Exit codes: `0` topic found; `1` topic not found.

## OPTIONS

- `--help`, `-h` — Print top-level help summary. Exit code: `0`.
- `--version`, `-v` — Print the binary's ldflag-injected version, commit SHA, and build date. Exit code: `0`.

## CONFIGURATION

All server configuration is via environment variables with the `CYODA_` prefix. Variables can be placed in `.env` files and loaded automatically using profiles. See `cyoda help config` for the full reference.

## EXAMPLES

```
# Start the server with defaults (in-memory storage, mock auth)
cyoda

# First-run bootstrap then start
cyoda init && cyoda

# Check version of an installed binary
cyoda --version

# Run with profiles: postgres storage + observability
CYODA_PROFILES=postgres,otel cyoda

# Run via docker compose (dev helper)
./scripts/dev/run-docker-dev.sh
```

## SEE ALSO

- config
- run
- quickstart
