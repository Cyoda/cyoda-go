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

`cyoda [<subcommand> [<arguments>]]`

## DESCRIPTION

cyoda is a Go binary that embeds the full platform: API server, schema engine, workflow runner, and storage plugins. Invoked with no subcommand, or with `serve`, it starts the server. The server takes no flags: its configuration comes from environment variables only (see `cyoda help config`). Subcommands provide operational affordances — `init` for first-run bootstrap, `health` for readiness probes, `migrate` for schema migrations, `token` for signing an admin token offline.

Global flags `--help` (or `-h`) and `--version` (or `-v`) are recognized in place of a subcommand. `-h` or `--help` after a subcommand prints that subcommand's help and exits `0`: `init`, `migrate` and `token` print their flag usage; `serve`, `health` and `help` print their help topic when `-h` or `--help` is the only argument after them.

An argument cyoda does not recognise is an error: an unknown subcommand or flag (the server has no flags), or an argument given to a subcommand or global flag that takes none. cyoda then prints `cyoda: unknown command "<arg>"` (or the matching message for a flag or an extra argument) and the usage summary to stderr, and exits `2` without loading any configuration or starting a server.

## SUBCOMMANDS

- `cyoda` or `cyoda serve` — start the API server. See `cyoda help cli serve`. Exit codes: `0` clean shutdown after SIGINT/SIGTERM; `1` startup failure (configuration validation, OTel init, port bind, backend connect); `2` an argument after `serve`, or a hard exit forced by a second signal.
- `cyoda init [--force]` — Write a starter user config enabling sqlite. See `cyoda help cli init`. Exit codes: `0` success or idempotent no-op; `1` I/O error; `2` bad flags or arguments.
- `cyoda health` — Probe `/readyz` on the admin listener. See `cyoda help cli health`. Exit codes: `0` readyz returned 200; `1` connection error or non-200 status; `2` any argument (it takes none).
- `cyoda migrate [--timeout <duration>]` — Run schema migrations for the configured backend and exit. See `cyoda help cli migrate`. Exit codes: `0` success or no-op (memory/sqlite); `1` runtime error (bad config, DB unreachable, migration failure, timeout); `2` bad flags or arguments.
- `cyoda token --tenant <tenantId> [--user <userId>] [--roles <r1,r2>] [--ttl <duration>]` — Sign a short-lived admin token with the signing key and print it. See `cyoda help cli token`. Exit codes: `0` token printed; `1` key or configuration error; `2` flag error or unexpected argument.
- `cyoda help [<topic>...] [--format=<fmt>]` — Browse the help topic tree. See `cyoda help cli help`. Exit codes: `0` topic found, or a lone `-h` / `--help`; `1` render failure; `2` unknown topic or action, bad `--format`, or `-h` / `--help` followed by more arguments.

## OPTIONS

- `--help`, `-h` — Print top-level help summary. Exit code: `0`; `2` if any argument follows it.
- `--version`, `-v` — Print the binary's ldflag-injected version, commit SHA, and build date. Exit code: `0`; `2` if any argument follows it.

## CONFIGURATION

All server configuration is via environment variables with the `CYODA_` prefix. Variables can be placed in `.env` files and loaded automatically using profiles. See `cyoda help config` for the full reference.

## EXAMPLES

```
# Start the server with defaults: mock auth, and sqlite storage once
# `cyoda init` has written its config (in-memory storage before that)
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
