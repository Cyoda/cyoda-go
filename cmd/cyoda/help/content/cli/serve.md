---
topic: cli.serve
title: "cyoda serve — start the API server"
stability: stable
see_also:
  - config
  - run
  - quickstart
---

# cli.serve

## NAME

cli.serve — start the cyoda API server.

## SYNOPSIS

`cyoda` (no subcommand; serving is the default mode)

## DESCRIPTION

Starting with no subcommand loads configuration from environment variables, validates the IAM mode, and binds the REST, gRPC, and admin listeners. The server is single-process, multi-tenant, and stateful — storage is provided by one of the pluggable backends (`memory`, `sqlite`, or `postgres`); see `cyoda help config` for backend selection.

On startup, the binary prints an ASCII banner with version, commit, build date, HTTP port, gRPC port, IAM mode, context path, and active storage profiles. The banner is suppressed by `CYODA_SUPPRESS_BANNER=true`.

The server handles graceful shutdown on `SIGINT` (Ctrl+C) or `SIGTERM`: the HTTP, admin and gRPC servers drain in-flight requests concurrently, each within a hardcoded 10-second deadline, then the storage backend is closed and the process exits. See `run` for the full shutdown timing.

## LISTENERS

Three TCP listeners are bound before any of them is served — a port that cannot be bound stops the process before a request is accepted — and are then served concurrently:

- **REST API** — `CYODA_HTTP_PORT` (default: 8080). All entity, schema, workflow, and auth endpoints, plus `GET /health` — a health summary for humans and simple scripts, not the deployment probe: `200 {"status":"UP"}` while healthy, `503 {"status":"DOWN"}` after a panic recovered in engine or store work, latched until the node is replaced. Context path prefix: `CYODA_CONTEXT_PATH` (default: `/api`).
- **gRPC** — `CYODA_GRPC_PORT` (default: 9090). Externalized-processor streaming.
- **Admin** — `CYODA_ADMIN_BIND_ADDRESS:CYODA_ADMIN_PORT` (default: `127.0.0.1:9091`). `/livez`, `/readyz`, and `/metrics` endpoints — `/livez` (unconditional) and `/readyz` (mirrors the same flag as `/health`) are the deployment probes. Admin port is bound to localhost by default; the Helm chart overrides `CYODA_ADMIN_BIND_ADDRESS` so the kubelet can reach `/readyz` without traversing the service mesh.

## ENVIRONMENT VARIABLES

All configuration is via environment variables. The subtopics below enumerate the complete per-subsystem variable sets:

- `config` — all top-level server options (HTTP port, log level, OTel, etc.)
- `config.database` — storage backend selection and per-backend connection settings
- `config.auth` — IAM mode, JWT issuer, signing key
- `config.grpc` — gRPC listener and compute-node credentials
- `config.schema` — schema-extension log tuning

Variables read specifically during server boot (not covered by the config subtopics above):

- `CYODA_HTTP_PORT` (int, default: `8080`) — HTTP API listen port.
- `CYODA_GRPC_PORT` (int, default: `9090`) — gRPC listen port.
- `CYODA_ADMIN_PORT` (int, default: `9091`) — admin listener port for `/livez`, `/readyz`, `/metrics`.
- `CYODA_ADMIN_BIND_ADDRESS` (string, default: `127.0.0.1`) — admin listener bind address: a bare host, IPv4 or IPv6, without brackets (`::1`, not `[::1]`).
- `CYODA_OTEL_ENABLED` (bool, default: `false`) — initialize the OpenTelemetry SDK at startup.
- `CYODA_LOG_LEVEL` (string, default: `info`) — accepted: `debug|info|warn|error`.
- `CYODA_SUPPRESS_BANNER` (bool, default: `false`) — suppress the ASCII startup banner and mock-auth warning.

## EXIT CODES

- `0` — clean shutdown after SIGINT or SIGTERM.
- `1` — startup failure, or a server that failed while running (`server group exited with error` in the log). Startup failures include: IAM validation failed (`CYODA_REQUIRE_JWT` contract not met), OTel SDK initialization error, a gRPC, HTTP or admin port that cannot be bound, or backend connection failure during `app.New`.
- `2` — hard exit forced by a second SIGINT or SIGTERM delivered while the graceful drain was still running. Nothing is drained or flushed on this path.

## EXAMPLES

```
# Run with defaults (in-memory storage, mock auth)
cyoda

# SQLite backend with debug logging
CYODA_STORAGE_BACKEND=sqlite CYODA_LOG_LEVEL=debug cyoda

# PostgreSQL backend, JWT auth, loaded via profiles
CYODA_PROFILES=postgres,jwt \
  CYODA_JWT_SIGNING_KEY="$(cat signing.pem)" \
  cyoda

# Suppress startup banner (useful in CI)
CYODA_SUPPRESS_BANNER=true cyoda
```

## SEE ALSO

- config
- run
- quickstart
