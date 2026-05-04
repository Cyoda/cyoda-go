# Cyoda-Go

A Go [EDBMS](https://medium.com/@paul_42036/whats-an-entity-database-11f8538b631a) (Entity Database Management System) — a database engine where the first-class abstraction is not a row or document but a *stateful entity* with schema, lifecycle, temporal history, and transactional integrity. Cyoda-Go replicates the functional behavior of the [Cyoda](https://cyoda.com) platform's APIs, gRPC integrations, entity lifecycle management, and workflow engine.

Cyoda-Go operates in three modes:

- **In-Memory mode** — a single-process, zero-dependency instance. Sub-millisecond latencies. Data is lost on restart.
  - **Local development target** — build and test Cyoda applications without a full distributed deployment
  - **Functional test harness** — verify API inputs/outputs and gRPC integration contracts
  - **Digital Twin Universe component** — validate at volumes and rates far exceeding production limits, run thousands of scenarios per hour without rate limits or API costs
- **SQLite mode** — persistent, zero-ops embedded storage. Data survives restarts in a single file. No external server required.
  - **Edge and IoT deployments** — single-binary, single-file persistence for resource-constrained environments
  - **Small team self-hosting** — durable storage without the operational overhead of a database server
  - **Local development with persistence** — keep data across restarts while retaining the simplicity of `go run`
- **PostgreSQL mode** — durable storage with `SERIALIZABLE` isolation for production workloads.
  - **Zero-compromise transactional safety** — full ACID with PostgreSQL-native SSI; no eventual consistency, no conflict windows, no split-brain
  - **Active-active high availability** — 3-10 stateless Go nodes behind a load balancer, any node serves any request, no leader election
  - **Operational simplicity** — PostgreSQL is the only infrastructure dependency; no ZooKeeper, no etcd, no Kafka

## Target Applications

High-complexity, high-consistency enterprise domains where correctness is non-negotiable:

- **Financial ledgers** — double-entry bookkeeping with strict state machine enforcement on journal entries
- **Order management** — multi-stage order lifecycles with automated and manual state transitions, external processor callouts for validation/enrichment
- **Regulatory compliance** — auditable entity histories with point-in-time retrieval for regulatory reporting windows
- **Digital twin orchestration** — behavioral clones of production systems for scenario testing at volumes exceeding production limits

## EDBMS Features

- **Entity Management** — store, retrieve, update, and delete JSON entities with full version history and the ability to query any past state
- **Entity Models** — discover schemas automatically from sample data, evolve them over time, and validate incoming entities against them
- **Workflow Engine** — define how entities move through states with rules that fire automatically or on demand, including callouts to external processors
- **Search** — find entities by field values, metadata, or compound conditions, with both immediate and background query modes
- **Externalized Processing** — connect external computation nodes over gRPC that receive work, transform entities, and evaluate conditions
- **Transactions** — all-or-nothing operations with conflict detection so concurrent writers never silently corrupt data
- **Multi-Tenancy** — each tenant's data is fully isolated; no API path can cross tenant boundaries
- **Authentication** — OAuth 2.0 token issuance, machine-to-machine credentials, on-behalf-of token exchange, and external key trust
- **Temporal Queries** — retrieve entities and search results as they existed at any point in the past
- **Edge Messaging** — store and retrieve messages with headers, metadata, and arbitrarily large payloads
- **Audit Trail** — every entity change and workflow decision is recorded and queryable
- **Pluggable Persistence** — run entirely in memory for speed, or switch to PostgreSQL for durability, or mix both per data type

See [OVERVIEW.md](OVERVIEW.md) for the full architecture and feature details.

## Documentation

Authoritative reference for flags, env vars, endpoints, and error codes ships in the binary itself:

    cyoda help                  # topic index
    cyoda help cli              # CLI reference
    cyoda help config database  # database config vars
    cyoda help errors MODEL_NOT_FOUND

A running server exposes the same tree over HTTP at `{ContextPath}/help` (default `/api/help`). Release assets include `cyoda_help_<version>.{tar.gz,json}` for offline / tooling consumption.

## Requirements

- **Go 1.26+**
- **Docker** (optional, for PostgreSQL backend and container builds)
- **PostgreSQL 17+** (required for PostgreSQL mode)

## Versioning

Cyoda-Go is **pre-1.0**: minor version bumps (e.g. `0.6.x` → `0.7.0`) may include breaking changes to the wire format, configuration, or operational surface. Patch bumps (`0.x.y` → `0.x.y+1`) are non-breaking.

Each release lists breaking changes in [`CHANGELOG.md`](./CHANGELOG.md). Active release line: see the latest tag at [Releases](https://github.com/Cyoda-platform/cyoda-go/releases).

**Older release lines (e.g. `v0.6.x`) are not maintained.** No back-port branches exist. If a real consumer needs a fix on an older line, branch from the relevant tag (e.g. `git checkout -b release/v0.6.x v0.6.3`) and open an issue describing the constraint — we'll consider creating an official maintenance branch if the need is concrete.

## Install

### macOS or Linux via Homebrew

```bash
brew install cyoda-platform/cyoda-go/cyoda
```

Or, if you tap first:

```bash
brew tap cyoda-platform/cyoda-go
brew install cyoda
```

After install, run `cyoda init` to write the default sqlite config to
`~/.config/cyoda/cyoda.env` (data file at `~/.local/share/cyoda/cyoda.db`),
or set `CYODA_STORAGE_BACKEND` and related env vars in your shell to run
without an on-disk config. Homebrew does not auto-init because its
`post_install` sandbox denies writes to `$HOME` (see issue #96).

### Any Unix via curl

```bash
curl -fsSL https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh | sh
```

Installs to `~/.local/bin/cyoda` and runs `cyoda init`. Override the
install directory with `CYODA_INSTALL_DIR=~/bin curl ... | sh`. Pin a
specific version with `CYODA_VERSION=v0.2.0 curl ... | sh`.

### Debian or Ubuntu

```bash
wget https://github.com/cyoda-platform/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb
```

Drops `/usr/bin/cyoda` and `/etc/cyoda/cyoda.env` (sqlite as the
system-wide default; preserved across upgrades). Replace `amd64` with
`arm64` for ARM hosts.

To pin a specific version:

```bash
wget https://github.com/cyoda-platform/cyoda-go/releases/download/v0.2.0/cyoda_linux_amd64.deb
```

### Fedora or RHEL

```bash
wget https://github.com/cyoda-platform/cyoda-go/releases/latest/download/cyoda_linux_amd64.rpm
sudo rpm -i cyoda_linux_amd64.rpm
```

### From source

```bash
go install github.com/cyoda-platform/cyoda-go/cmd/cyoda@latest
```

Uses the binary's compiled-in `memory` default. Set
`CYODA_STORAGE_BACKEND=sqlite` or run `cyoda init` for persistence.

## Quick Start

### Local Development (recommended)

```bash
./scripts/dev/run-local.sh
```

This runs Cyoda-Go with the `local` profile (`.env.local`): in-memory storage, JWT auth, debug logging on port **8123** (HTTP) and **9123** (gRPC). Copy `.env.local.example` to `.env.local` to customize.

```bash
# Get a token (set CYODA_BOOTSTRAP_CLIENT_ID and CYODA_BOOTSTRAP_CLIENT_SECRET in .env.local):
TOKEN=$(curl -s -X POST http://localhost:8123/api/oauth/token \
  -u "$CYODA_BOOTSTRAP_CLIENT_ID:$CYODA_BOOTSTRAP_CLIENT_SECRET" \
  -d "grant_type=client_credentials" | jq -r .access_token)

# Use it:
curl -H "Authorization: Bearer $TOKEN" http://localhost:8123/api/health
# {"status":"UP"}
```

### In-Memory Mode (no dependencies, no auth)

```bash
go run ./cmd/cyoda
```

Starts on port **8080** (HTTP) and **9090** (gRPC) with mock auth (no tokens needed). All data lives in memory and is lost on restart. This is the simplest way to get started, but doesn't reflect production auth behavior.

```bash
curl http://localhost:8080/api/health
# {"status":"UP"}
```

### Docker with PostgreSQL (single node)

> **Temporarily unavailable.** This path depends on `deploy/docker/Dockerfile` and
> `deploy/docker/compose.yaml`, which are produced by the Docker per-target plan
> (follow-up work). Until those artifacts land, use `./scripts/dev/run-local.sh`
> (in-memory, no deps) or `examples/compose-with-observability/compose.yaml`
> (PostgreSQL + Grafana stack) instead.

```bash
./scripts/dev/run-docker-dev.sh
```

This generates a `.env.docker` with a fresh JWT signing key and starts both Cyoda-Go and PostgreSQL via `docker compose`. Data is persisted to a Docker volume.

```bash
# Get a token (set CYODA_BOOTSTRAP_CLIENT_ID and CYODA_BOOTSTRAP_CLIENT_SECRET before starting):
TOKEN=$(curl -s -X POST http://localhost:8123/api/oauth/token \
  -u "$CYODA_BOOTSTRAP_CLIENT_ID:$CYODA_BOOTSTRAP_CLIENT_SECRET" -d "grant_type=client_credentials" | jq -r .access_token)

# Use it:
curl -H "Authorization: Bearer $TOKEN" http://localhost:8123/api/health
```

### Multi-Node Cluster

```
┌─────────┐
│  nginx   │ ← Load balancer (port 8123)
│  (LB)   │
├─────────┤
│ Node 1  │ ← HTTP + gRPC + gossip
│ Node 2  │
│ Node 3  │
├─────────┤
│PostgreSQL│ ← Shared, SERIALIZABLE isolation
└─────────┘
```

Provisioned via `start-cluster.sh` with configurable `--nodes` flag. All nodes are stateless and identical — no leader election, no shard ownership. PostgreSQL is the single coordination layer. Nodes discover each other using gossip (SWIM protocol) with no external service discovery infrastructure.

When a node begins a PostgreSQL transaction, it generates a signed routing token encoding which node owns the `pgx.Tx` handle. All subsequent requests for that transaction are routed to the owning node. If the owning node dies, PostgreSQL auto-rolls back the connection and the client retries from scratch.

## Storage backends

Cyoda-Go's storage layer is a plugin system defined by the stable [`cyoda-go-spi`](https://github.com/Cyoda-platform/cyoda-go-spi) module. Exactly one plugin is active at a time, selected at startup via `CYODA_STORAGE_BACKEND`:

| Backend | Default | Notes |
|---------|---------|-------|
| `memory` | ✓ | Zero configuration. In-process, ephemeral. Single-node only. The default so `go build && ./cyoda` just runs. |
| `sqlite` |   | Persistent, zero-ops embedded storage. Single-node, single-process. No external dependencies. Configure via `CYODA_SQLITE_*`. |
| `postgres` |   | Durable, `SERIALIZABLE` isolation. Configure via `CYODA_POSTGRES_*`. Supports multi-node clusters (see cluster deployment guide). |

The stock binary contains all three. A proprietary `cassandra` plugin ships in the separate `cyoda-go-cassandra` binary for deployments that need horizontal write scalability.

### SQLite quick configuration

```bash
CYODA_STORAGE_BACKEND=sqlite
# Optional — defaults to $XDG_DATA_HOME/cyoda/cyoda.db
CYODA_SQLITE_PATH=/var/lib/cyoda/cyoda.db
```

Data persists across restarts in a single file. WAL mode is enabled automatically. No external server required. The process acquires an exclusive file lock on startup — only one instance can use a given database file.

### PostgreSQL quick configuration

```bash
CYODA_STORAGE_BACKEND=postgres
CYODA_POSTGRES_URL=postgres://user:pass@localhost:5432/minicyoda?sslmode=disable
CYODA_POSTGRES_AUTO_MIGRATE=true
```

### Writing a third-party plugin

Plugin authors depend only on `github.com/cyoda-platform/cyoda-go-spi` (stdlib only). The stock plugins in [`plugins/memory/`](plugins/memory/) and [`plugins/postgres/`](plugins/postgres/) are the reference implementations. Key patterns:

- Register with `spi.Register` from `init()`.
- Implement `spi.Plugin.NewFactory(ctx, getenv, opts...)` — use the injected `getenv` for config, `ctx` for cancellable blocking setup.
- Implement `spi.DescribablePlugin.ConfigVars()` so `--help` renders your env vars.
- Own your `TransactionManager` — expose it via `StoreFactory.TransactionManager(ctx)`. The postgres plugin illustrates the txID-to-physical-handle bridge pattern.
- Implement `spi.Startable.Start(ctx)` if you spawn background goroutines; tear them down in `StoreFactory.Close()`.

To build a custom binary with your plugin, blank-import it alongside the stock plugins in `main.go`:

```go
import (
    _ "github.com/cyoda-platform/cyoda-go/plugins/memory"
    _ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
    _ "example.com/my-custom-plugin"
)
```

See the [`cyoda-go-spi` package documentation](https://pkg.go.dev/github.com/cyoda-platform/cyoda-go-spi) for the full contract and a worked example.

## Scale Profile

| Dimension | Sweet Spot | Upper Bound |
|-----------|-----------|-------------|
| Cluster size | 3-5 nodes | 10-20 nodes |
| Concurrent transactions | 50-250 | ~750 (3 nodes x 25 PG connections) |
| Entity volume | Up to millions per model | Bounded by PG storage |
| Write throughput | 50-200 entity creates/s per node | Bounded by PG SERIALIZABLE |

Cyoda-Go excels at transactional correctness and operational simplicity for small-to-medium data volumes (terabytes, not petabytes). It trades away horizontal write scalability — all writes go through a single PostgreSQL instance.

## Configuration

All configuration is via environment variables with the `CYODA_` prefix. Run `cyoda --help` for the complete reference.

### Config sources

cyoda reads configuration from these sources, in increasing order of
precedence (later overrides earlier):

1. Compiled-in defaults (memory backend, port 8080, mock auth).
2. System config file (Linux only): `/etc/cyoda/cyoda.env`. Dropped by
   the `.deb`/`.rpm` package; survives upgrades. macOS has no system
   config path; Homebrew users get user config instead.
3. User config file (per-OS):
   - Linux and macOS: `$XDG_CONFIG_HOME/cyoda/cyoda.env` → fallback
     `~/.config/cyoda/cyoda.env`.
   - Windows: `%AppData%\cyoda\cyoda.env`.

   Written by `cyoda init`.
4. `.env` and `.env.<profile>` in the current working directory
   (profiles via `CYODA_PROFILES`). See [`.env.sqlite.example`](.env.sqlite.example),
   [`.env.postgres.example`](.env.postgres.example),
   [`.env.local.example`](.env.local.example),
   [`.env.jwt.example`](.env.jwt.example).
5. Shell environment variables (always win).

### Subcommands

- `cyoda init` — write a sqlite user config file (desktop use)
- `cyoda health` — probe `/readyz` and exit 0 ready / 1 otherwise (Docker HEALTHCHECK)
- `cyoda migrate` — run schema migrations for the configured backend and exit

Run `cyoda init` to write a starter user config with sqlite enabled.
Run `cyoda --help` for the full env-var reference.

### Profiles

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_PROFILES` | *(none)* | Comma-separated profile names. Loads `.env` then `.env.{profile}` for each profile. Shell env vars always win. |

Profiles load `.env` files in order — later profiles override earlier ones. Example files: `.env.local`, `.env.postgres`, `.env.otel`. Copy the corresponding `.example` file to get started.

```bash
# Combine profiles:
CYODA_PROFILES=postgres,otel go run ./cmd/cyoda
```

The `./scripts/dev/run-local.sh` script is a convenience wrapper that sets `CYODA_PROFILES=local` by default.

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_HTTP_PORT` | `8080` | HTTP listen port |
| `CYODA_GRPC_PORT` | `9090` | gRPC listen port |
| `CYODA_ADMIN_PORT` | `9091` | Admin listener port (`/livez`, `/readyz`, `/metrics`) |
| `CYODA_ADMIN_BIND_ADDRESS` | `127.0.0.1` | Admin listener bind address (loopback by default; set to `0.0.0.0` in containers) |
| `CYODA_CONTEXT_PATH` | `/api` | URL prefix for all routes |
| `CYODA_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `CYODA_SUPPRESS_BANNER` | `false` | Set to `true` to silence the startup banner and any mock-auth warnings. Intended for CI/test harnesses; never set in production so operators see security-relevant warnings. |
| `CYODA_ERROR_RESPONSE_MODE` | `sanitized` | Error detail: `sanitized` (production) or `verbose` (development) |
| `CYODA_CORS_ENABLED` | `true` | Master switch for CORS. Set to `false` to disable and handle CORS at an ingress/proxy. |
| `CYODA_CORS_ALLOWED_ORIGINS` | *(unset)* | Comma-separated allowlist or `*` for wildcard. Unset = loopback mode (only `localhost`/`127.0.0.1`/`[::1]` permitted). See `cyoda help config cors`. |

### Authentication

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_IAM_MODE` | `mock` | `mock` (no auth) or `jwt` (OAuth 2.0 with JWT) |
| `CYODA_REQUIRE_JWT` | `false` | Set to `true` to make the binary refuse to start unless `CYODA_IAM_MODE=jwt` and `CYODA_JWT_SIGNING_KEY` are both set. The canonical Helm chart enables this by default; desktop and Docker leave it off so the mock-auth fallback still applies to evaluators. |
| `CYODA_IAM_MOCK_ROLES` | `ROLE_ADMIN,ROLE_M2M` | Comma-separated roles granted to the default mock user. `ROLE_M2M` is required for the gRPC streaming endpoint; `ROLE_ADMIN` for admin HTTP endpoints. |
| `CYODA_JWT_SIGNING_KEY` | — | RSA private key in PEM format. Required for `jwt` mode. |
| `CYODA_JWT_ISSUER` | `cyoda` | JWT issuer claim |
| `CYODA_JWT_AUDIENCE` | — | Expected `aud` claim on inbound JWTs. When empty, the audience check is skipped (pre-hardening behaviour); set to your deployment's audience to reject tokens minted for other relying parties. |
| `CYODA_JWT_EXPIRY_SECONDS` | `3600` | Token lifetime |

### Credential env vars: `_FILE` suffix support

The five credential env vars — `CYODA_POSTGRES_URL`, `CYODA_JWT_SIGNING_KEY`,
`CYODA_HMAC_SECRET`, `CYODA_BOOTSTRAP_CLIENT_SECRET`, `CYODA_METRICS_BEARER` — accept a `_FILE`
variant that reads the value from the file at the given path:

```bash
# Equivalent:
export CYODA_JWT_SIGNING_KEY="$(cat /path/to/key.pem)"
export CYODA_JWT_SIGNING_KEY_FILE=/path/to/key.pem
```

`_FILE` takes precedence when both are set. Trailing whitespace is stripped
from file contents — safe for both DSN strings and multi-line PEM keys.

This is the canonical Docker/Kubernetes pattern (postgres, mysql, redis,
keycloak all use it) and is how the Helm chart wires credentials from
Secrets to the pod without exposing them in `env` output.

### Bootstrap (jwt mode)

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_BOOTSTRAP_CLIENT_ID` | `""` | Bootstrap M2M client ID. Coupled with `CYODA_BOOTSTRAP_CLIENT_SECRET` in jwt mode: both set (bootstrap client created at startup) or both empty (no bootstrap client). Half-configured states are rejected at startup. Ignored in mock mode. |
| `CYODA_BOOTSTRAP_CLIENT_SECRET` | `""` | Bootstrap M2M client secret. See `CYODA_BOOTSTRAP_CLIENT_ID` for the coupling rule. Ignored in mock mode. |
| `CYODA_BOOTSTRAP_TENANT_ID` | `default-tenant` | Tenant for the bootstrap client |
| `CYODA_BOOTSTRAP_ROLES` | `ROLE_ADMIN,ROLE_M2M` | Comma-separated roles |

### Schema extension log

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_SCHEMA_SAVEPOINT_INTERVAL` | `64` | Number of extensions between savepoint rows. Honored by: postgres, sqlite, cassandra. Ignored by memory (no log). |
| `CYODA_SCHEMA_EXTEND_MAX_RETRIES` | `8` | Plugin-layer retry budget for ExtendSchema. Honored by: sqlite (SQLITE_BUSY), cassandra (LWT). Ignored by memory, postgres (no conflict surface on schema writes). |

### SQLite

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_SQLITE_PATH` | `$XDG_DATA_HOME/cyoda/cyoda.db` | Database file path |
| `CYODA_SQLITE_AUTO_MIGRATE` | `true` | Run embedded SQL migrations on startup |
| `CYODA_SQLITE_BUSY_TIMEOUT` | `5s` | Wait time for SQLite write lock |
| `CYODA_SQLITE_CACHE_SIZE` | `64000` | Page cache in KiB |
| `CYODA_SQLITE_SEARCH_SCAN_LIMIT` | `100000` | Max rows examined per search with residual filter |

### PostgreSQL

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_POSTGRES_URL` | — | Connection string. Required when any store uses `postgres`. |
| `CYODA_POSTGRES_MAX_CONNS` | `25` | Connection pool maximum |
| `CYODA_POSTGRES_MIN_CONNS` | `5` | Connection pool minimum |
| `CYODA_POSTGRES_AUTO_MIGRATE` | `true` | Run schema migrations on startup |

### gRPC

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_KEEPALIVE_INTERVAL` | `10` | Keep-alive send interval (seconds) |
| `CYODA_KEEPALIVE_TIMEOUT` | `30` | Keep-alive timeout before disconnect (seconds) |

### Observability

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_OTEL_ENABLED` | `false` | Enable OpenTelemetry tracing and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4318` | OTLP endpoint (standard OTel env var) |

Enable via the `otel` profile: `CYODA_PROFILES=local,otel go run ./cmd/cyoda`. Copy `.env.otel.example` to `.env.otel` to customize.

### Admin / Observability Listener

The admin listener binds to `CYODA_ADMIN_BIND_ADDRESS:CYODA_ADMIN_PORT` (default `127.0.0.1:9091`) and exposes the following endpoints:

| Endpoint | Description |
|----------|-------------|
| `/livez` | Liveness — process responsiveness only; does not check storage. Use this for Kubernetes `livenessProbe`. Always unauthenticated. |
| `/readyz` | Readiness — storage reachable, migrations applied, bootstrap complete. Use for `readinessProbe` and load-balancer health gates. Always unauthenticated. |
| `/metrics` | Prometheus pull endpoint. **Optionally requires a Bearer token** — set `CYODA_METRICS_BEARER` (or `_FILE`) to enable; see below. |

**Probe endpoints (`/livez`, `/readyz`) are unauthenticated by design** — kubelet probes carry no bearer. `/metrics` is also unauthenticated by default so desktop and Docker workflows work without friction, but operators deploying to shared clusters should enable Bearer-token auth on it:

- `CYODA_METRICS_BEARER` (or `_FILE`) — static token; when non-empty, `GET /metrics` requires `Authorization: Bearer <token>`. Constant-time compared.
- `CYODA_METRICS_REQUIRE_AUTH=true` — coupled predicate; refuses to start if set while the bearer is empty. Protects against "I thought I turned it on" misconfigurations.
- The canonical Helm chart enables this end-to-end: a chart-managed Secret holds the token, the StatefulSet mounts it via the `_FILE` pattern, and the `ServiceMonitor` references it via `bearerTokenSecret` so Prometheus scrapes authenticate automatically.

Bind-address remains the outer boundary:
- Desktop: leave `CYODA_ADMIN_BIND_ADDRESS` at its default `127.0.0.1` (loopback only); no bearer needed.
- Kubernetes: bind to `0.0.0.0` (kubelet + Prometheus reach the pod-facing interface) and enable the bearer; the Helm chart does both.
- Docker Compose: map the port as `127.0.0.1:9091:9091` so it is only reachable from the host; no bearer needed.

The existing `/api/health` on the main API listener is retained for backwards compatibility.

### Security

**Production safety floor — `CYODA_REQUIRE_JWT=true`**

Set `CYODA_REQUIRE_JWT=true` to make the binary refuse to start unless `CYODA_IAM_MODE=jwt` and `CYODA_JWT_SIGNING_KEY` are both set. This prevents accidentally deploying with mock auth enabled. The canonical Helm chart enables this by default. Desktop and Docker leave it off so the mock-auth fallback still applies to evaluators.

**Mock-auth warning banner**

When running in `CYODA_IAM_MODE=mock`, the binary emits a prominent warning banner at startup to remind operators that all requests are unauthenticated. `CYODA_SUPPRESS_BANNER=true` silences this banner — it is intended only for CI/test harnesses where the warning is noise. Never set `CYODA_SUPPRESS_BANNER=true` in production: operators need to see the banner to know the security posture of the running instance.

### Admin Endpoints

#### Log Level (`/api/admin/log-level`)

Runtime-switchable log level. Requires `ROLE_ADMIN` on the JWT.

**GET `/api/admin/log-level`** — returns the current log level as JSON:

```json
{"level": "info"}
```

**POST `/api/admin/log-level`** — changes the log level atomically:

```bash
curl -X POST -H 'Authorization: Bearer $TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{"level":"debug"}' \
  http://localhost:8080/api/admin/log-level
```

#### Trace Sampler (`/api/admin/trace-sampler`)

Runtime-switchable OTel trace sampler. Requires `ROLE_ADMIN` on the
JWT. Matches the `/api/admin/log-level` pattern.

**GET `/api/admin/trace-sampler`** — returns the current sampler
configuration as JSON:

```json
{"sampler": "ratio", "ratio": 0.1, "parent_based": true}
```

**POST `/api/admin/trace-sampler`** — changes the sampler
configuration atomically. Body shape is symmetric with the GET
response:

```bash
# Sample every trace (default)
curl -X POST -H 'Authorization: Bearer $TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{"sampler":"always"}' \
  http://localhost:8080/api/admin/trace-sampler

# Sample 10% of traces
curl -X POST -H 'Authorization: Bearer $TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{"sampler":"ratio","ratio":0.1}' \
  http://localhost:8080/api/admin/trace-sampler

# Disable tracing entirely
curl -X POST -H 'Authorization: Bearer $TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{"sampler":"never"}' \
  http://localhost:8080/api/admin/trace-sampler
```

Valid `sampler` values: `always`, `never`, `ratio`. When `sampler` is
`ratio`, `ratio` must be a float in `[0, 1]`.

**`parent_based` interaction with upstream sampling decisions.** When
`parent_based` is `true` (the default), the sampler respects the
upstream trace's sampling decision from the `traceparent` header. If
an upstream service or load balancer decided "do not sample", this
node honors that decision, even with `sampler: always`. This is
standard OTel `ParentBased` behavior and is usually what operators
want for distributed-trace correctness.

To force 100% sampling on this node regardless of upstream, set
`parent_based: false`:

```json
{"sampler": "always", "parent_based": false}
```

**Initial sampler.** At startup, the sampler is seeded from the
standard OTel env vars `OTEL_TRACES_SAMPLER` and
`OTEL_TRACES_SAMPLER_ARG`. Supported values are the six standard
combinations from the OTel spec (`always_on`, `always_off`,
`traceidratio`, and their `parentbased_` variants). The admin endpoint
is a runtime override, not a replacement.

**Process-local.** Each node has its own sampler; multi-node
deployments need to hit each node's admin endpoint separately, same
as `/admin/log-level`.
