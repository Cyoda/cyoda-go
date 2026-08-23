---
topic: config
title: "cyoda configuration reference"
stability: stable
see_also:
  - cli
  - run
  - config.auth
  - config.cors
  - config.database
  - config.grpc
  - config.schema
  - config.cluster
  - config.scheduler
---

# config

## NAME

config — environment-driven configuration for cyoda.

## SYNOPSIS

All configuration is environment variables prefixed with `CYODA_`. Topics group related variables:

- `config.auth` — IAM mode, JWT issuer, admin controls
- `config.cors` — CORS middleware mode and allowed origins
- `config.database` — storage backend selection, per-backend connection settings
- `config.grpc` — gRPC listener and compute-node credentials
- `config.schema` — schema-extension log tuning
- `config.cluster` — multi-node clustering, gossip, cross-node dispatch
- `config.scheduler` — scheduled-transition scan-loop cadence, distribution, and expiry grace
- `config all` — flat listing of every variable (append `--format=json` for the docs-site JSON)

## DESCRIPTION

### Precedence

Environment variables beat default values. The `_FILE` suffix variant takes precedence over the plain variable when both are set — for example, `CYODA_POSTGRES_URL_FILE=/etc/secrets/db-url` wins over `CYODA_POSTGRES_URL`. There are no command-line flags for configuration values; env vars are the sole configuration surface.

### _FILE suffix support

The following variables support the `_FILE` suffix. Setting `CYODA_FOO_FILE=<path>` causes the binary to read the value from the file at `<path>`, trimming trailing whitespace. The `_FILE` variant takes precedence over `CYODA_FOO` when both are set. A set but unreadable `_FILE` path causes immediate startup failure.

- `CYODA_JWT_SIGNING_KEY` / `CYODA_JWT_SIGNING_KEY_FILE`
- `CYODA_HMAC_SECRET` / `CYODA_HMAC_SECRET_FILE`
- `CYODA_BOOTSTRAP_CLIENT_SECRET` / `CYODA_BOOTSTRAP_CLIENT_SECRET_FILE`
- `CYODA_METRICS_BEARER` / `CYODA_METRICS_BEARER_FILE`

### Profile loader

`CYODA_PROFILES` is a comma-separated list of profile names. For each name `N`, a file
`cyoda.N.env` is loaded from the working directory before the process's own environment is
consulted. This supports local development without exporting many variables.

**Example:**

```
CYODA_PROFILES=postgres,otel go run ./cmd/cyoda
```

loads `cyoda.postgres.env` and `cyoda.otel.env` from the working directory.

### Server options

- `CYODA_HTTP_PORT` (int, default: `8080`) — HTTP listen port.
- `CYODA_CONTEXT_PATH` (string, default: `/api`) — URL prefix for all routes.
- `CYODA_ERROR_RESPONSE_MODE` (string, default: `sanitized`) — error detail level: `sanitized` (generic message + ticket UUID for 5xx) or `verbose` (internal error detail included in responses; development use only).
- `CYODA_LOG_LEVEL` (string, default: `info`) — accepted: `debug|info|warn|error`.
- `CYODA_SUPPRESS_BANNER` (bool, default: `false`) — silence startup and mock-auth banners.
- `CYODA_STARTUP_TIMEOUT` (duration, default: `30s`) — deadline for plugin init, TM init, and (cluster mode) the gossip seed-join retry loop.
- `CYODA_DEBUG` — reserved; not currently read by the server.
- `CYODA_MAX_STATE_VISITS` (int, default: `10`) — max visits per state in workflow cascade.
- `CYODA_MODEL_CACHE_LEASE` (duration, default: `5m`) — model cache lease duration; actual expiry is jittered ±10%.
- `CYODA_STATS_GROUP_MAX` (int, default: `10000`) — cardinality ceiling for `POST /api/entity/stats/{entityName}/{modelVersion}/query`. When the grouped-stats result produces more distinct `groupKey` combinations than this value, the request fails with 422 `GROUP_CARDINALITY_EXCEEDED`. Also caps the request `limit` parameter (`limit > max` rejects with 400 `INVALID_LIMIT`). Values `<= 0` are silently clamped to the default (`10000`) — a non-positive cap would disable the ceiling entirely (plugins treat `<= 0` as "unbounded"), defeating the safety net.

### Admin and metrics

- `CYODA_ADMIN_PORT` (int, default: `9091`) — admin port for health and metrics.
- `CYODA_ADMIN_BIND_ADDRESS` (string, default: `127.0.0.1`) — admin listener bind address.
- `CYODA_METRICS_REQUIRE_AUTH` (bool, default: `false`) — require Bearer auth on `/metrics`; startup fails if `true` and `CYODA_METRICS_BEARER` is empty.
- `CYODA_METRICS_BEARER` (string, default: unset) — static Bearer token for `GET /metrics`. Supports `_FILE` suffix.
- `CYODA_OTEL_ENABLED` (bool, default: `false`) — enable OpenTelemetry tracing and metrics.

### Search internals

- `CYODA_SEARCH_SNAPSHOT_TTL` (duration, default: `1h`) — search snapshot TTL.
- `CYODA_SEARCH_REAP_INTERVAL` (duration, default: `5m`) — search snapshot reap interval.
- `CYODA_SEARCH_MAX_SORT_KEYS` (int, default: `16`) — maximum number of `sort` keys per search request. Requests exceeding this cap are rejected with `400 INVALID_FIELD_PATH`. Values `<= 0` are clamped to the default.
- `CYODA_SEARCH_ASYNC_WORKERS` (int, default: `8`) — async-search worker pool size. Config is a QA'd artefact: values `< 1` fail startup rather than being clamped.
- `CYODA_SEARCH_ASYNC_QUEUE` (int, default: `256`) — async-search submit queue capacity beyond the running workers. Once both are exhausted, submission fails with `503 SEARCH_QUEUE_FULL` (retryable). Values `< 0` fail startup.
- `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` (duration, default: `15s`) — how often a running async-search executor stamps job liveness and polls for a cross-node cancel/terminal status, starting at submit time (queued or scanning). Config is a QA'd artefact: values `<= 0` fail startup rather than being clamped.

### Cluster and dispatch

See `config.cluster` for multi-node clustering, gossip, and cross-node dispatch variables.

## SEE ALSO

- cli
- run
- config.auth
- config.cors
- config.database
- config.grpc
- config.schema
- config.cluster
