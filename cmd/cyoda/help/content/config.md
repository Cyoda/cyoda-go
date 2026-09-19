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
- `CYODA_HTTP_READ_HEADER_TIMEOUT` (duration, default: `10s`) — time allowed to receive a request's headers on the API and admin servers. 0 falls back to `CYODA_HTTP_READ_TIMEOUT`.
- `CYODA_HTTP_READ_TIMEOUT` (duration, default: `5m`) — time allowed to receive a whole request, body included. Does not limit handler execution. 0 disables.
- `CYODA_HTTP_WRITE_TIMEOUT` (duration, default: `0s`) — time from the end of the request headers to the end of the response. Limits handler execution, so it ships disabled; set only if you want the server to cut off long-running requests.
- `CYODA_HTTP_IDLE_TIMEOUT` (duration, default: `2m`) — how long an idle keep-alive connection is held open between requests. 0 falls back to `CYODA_HTTP_READ_TIMEOUT`.
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
- `CYODA_ADMIN_BIND_ADDRESS` (string, default: `127.0.0.1`) — admin listener bind address: a bare host, IPv4 or IPv6, without brackets (`::1`, not `[::1]`).
- `CYODA_METRICS_REQUIRE_AUTH` (bool, default: `false`) — require Bearer auth on `/metrics`; startup fails if `true` and `CYODA_METRICS_BEARER` is empty.
- `CYODA_METRICS_BEARER` (string, default: unset) — static Bearer token for `GET /metrics`. Supports `_FILE` suffix.
- `CYODA_OTEL_ENABLED` (bool, default: `false`) — enable OpenTelemetry tracing and metrics.

### Search internals

- `CYODA_SEARCH_SNAPSHOT_TTL` (duration, default: `1h`) — search snapshot TTL.
- `CYODA_SEARCH_REAP_INTERVAL` (duration, default: `5m`) — search snapshot reap interval: how often terminal jobs older than `CYODA_SEARCH_SNAPSHOT_TTL` are deleted. It no longer drives the stale/reclaim sweep — that runs on `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`'s ticker instead (see `CYODA_SEARCH_JOB_STALE_AFTER`). This is purely the snapshot-cleanup cadence.
- `CYODA_SEARCH_MAX_SORT_KEYS` (int, default: `16`) — maximum number of `sort` keys per search request. Requests exceeding this cap are rejected with `400 INVALID_FIELD_PATH`. Values `<= 0` are clamped to the default.
- `CYODA_SEARCH_ASYNC_WORKERS` (int, default: `8`) — async-search worker pool size. Config is a QA'd artefact: values `< 1` fail startup rather than being clamped.
- `CYODA_SEARCH_ASYNC_QUEUE` (int, default: `256`) — async-search submit queue capacity beyond the running workers. Once both are exhausted, submission fails with `503 SEARCH_QUEUE_FULL` (retryable). Values `< 0` fail startup.
- `CYODA_SEARCH_ASYNC_MAX_PER_TENANT` (int, default: `8` — tracks `CYODA_SEARCH_ASYNC_WORKERS`) — maximum async-search jobs one tenant may have in flight (queued or running) on a node. Over-cap submissions get the same retryable `503 SEARCH_QUEUE_FULL`, so one tenant's burst cannot fill the shared queue and lock every other tenant out. A tenant may still occupy every worker; it just cannot hold more than this many queue slots. `0` disables the cap. Values `< 0` fail startup.
- `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` (duration, default: `15s`) — how often a running async-search executor stamps job liveness and polls for a cross-node cancel/terminal status, starting at submit time (queued or scanning). Config is a QA'd artefact: values `<= 0` fail startup rather than being clamped.
- `CYODA_SEARCH_JOB_STALE_AFTER` (duration, default: `5m`) — how long a `RUNNING` async-search job may go without a heartbeat before the reaper claims it for reclaim (its owning executor most likely crashed or was killed). Config is a QA'd artefact: values below `4 x CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` fail startup rather than being clamped. The stale/reclaim sweep runs on `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`'s ticker (default `15s`) plus once at startup — not on `CYODA_SEARCH_REAP_INTERVAL`, which now drives only the snapshot-TTL reap. A crash therefore hands off within `CYODA_SEARCH_JOB_STALE_AFTER` + one heartbeat interval (~5m15s at defaults); a graceful shutdown or restart releases in-flight jobs immediately, so those hand off within one heartbeat interval — or immediately on the restarted node's own startup sweep. Operational note: on postgres, a node that dies mid-save (inside the `SaveResults` chunk transaction) is reaped by the server's transaction-local idle timeout rather than by this sweep directly, so a mid-save crash's handoff still matches any other crash once that timeout releases the row.
- `CYODA_SEARCH_JOB_MAX_ATTEMPTS` (int, default: `3`) — executions an async-search job may consume before it is failed: the initial run plus one per executor lost without a graceful release. A graceful handoff (release then reclaim) does not count. Config is a QA'd artefact: values `< 1` fail startup rather than being clamped. `1` disables re-execution — a job is failed the first time its executor is lost.

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
