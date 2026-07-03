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

### Search and transaction internals

- `CYODA_SEARCH_SNAPSHOT_TTL` (duration, default: `1h`) — search snapshot TTL.
- `CYODA_SEARCH_REAP_INTERVAL` (duration, default: `5m`) — search snapshot reap interval.
- `CYODA_SEARCH_MAX_SORT_KEYS` (int, default: `16`) — maximum number of `sort` keys per search request. Requests exceeding this cap are rejected with `400 INVALID_FIELD_PATH`. Values `<= 0` are clamped to the default.
- `CYODA_TX_TTL` (duration, default: `60s`) — transaction TTL.
- `CYODA_TX_REAP_INTERVAL` (duration, default: `10s`) — transaction reap interval.
- `CYODA_TX_OUTCOME_TTL` (duration, default: `5m`) — transaction outcome TTL.

### Cluster and dispatch

- `CYODA_CLUSTER_ENABLED` (bool, default: `false`) — enable multi-node clustering.
- `CYODA_NODE_ID` (string, default: unset) — unique node identifier; required when `CYODA_CLUSTER_ENABLED=true`; any non-empty string is accepted.
- `CYODA_NODE_ADDR` (string, default: `http://localhost:8080`) — this node's HTTP base URL; must include scheme (`http://` or `https://`).
- `CYODA_GRPC_NODE_ADDR` (string, default: unset) — this node's gRPC endpoint advertised to peers (`host:port`, no scheme). When set, peers dial this address for cross-node gRPC callback forwarding. When unset, peers derive the gRPC address from this node's HTTP host plus their own `CYODA_GRPC_PORT` (uniform-deployment default).
- `CYODA_GOSSIP_ADDR` (string, default: `:7946`) — gossip protocol listen address; format `[host]:port` — parsed via `net.SplitHostPort`; invalid format causes startup failure.
- `CYODA_GOSSIP_STABILITY_WINDOW` (duration, default: `2s`) — gossip stability window.
- `CYODA_SEED_NODES` (string, default: empty) — comma-separated list of seed node addresses (e.g., `node1.example.com:7946,node2.example.com:7946`); empty means single-node or seed-discovery handled externally.
- `CYODA_HMAC_SECRET` (string, default: unset) — hex-encoded HMAC secret for inter-node dispatch authentication; required when `CYODA_CLUSTER_ENABLED=true`. Supports `_FILE` suffix.
- `CYODA_PROXY_TIMEOUT` (duration, default: `30s`) — request proxy timeout.
- `CYODA_DISPATCH_WAIT_TIMEOUT` (duration, default: `5s`) — how long the dispatcher polls gossip for a compute member with matching tags.
- `CYODA_DISPATCH_FORWARD_TIMEOUT` (duration, default: `30s`) — HTTP timeout for the cross-node forwarding call.
- `CYODA_TX_TOKEN_TTL` (duration, default: `90s`) — TTL of the signed transaction routing token minted on processor/criteria dispatch; must be ≥ `CYODA_DISPATCH_FORWARD_TIMEOUT` so the token remains valid through the full round-trip and callback verification, including the forwarded-chain case where two budgets stack.
- `CYODA_KEEPALIVE_INTERVAL` (int, default: `10`) — keep-alive send interval in seconds.
- `CYODA_KEEPALIVE_TIMEOUT` (int, default: `30`) — keep-alive timeout in seconds.

## SEE ALSO

- cli
- run
- config.auth
- config.database
- config.grpc
- config.schema
