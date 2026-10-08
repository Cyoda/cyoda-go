[![CI](https://github.com/cyoda/cyoda-go/actions/workflows/ci.yml/badge.svg)](https://github.com/cyoda/cyoda-go/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/cyoda/cyoda-go)](https://github.com/cyoda/cyoda-go/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/cyoda-platform/cyoda-go.svg)](https://pkg.go.dev/github.com/cyoda-platform/cyoda-go)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

# cyoda-go

**One transactional runtime for the entity lifecycle.**

cyoda-go is an EDBMS (Entity Database Management System) — state machine, processors, and full revision history live inside the record, committed atomically. Minimizes the need for sagas, CDC pipelines, and external orchestration.

**Correctness over availability.** When the two conflict, cyoda-go fails closed: an operation that cannot be completed correctly is rejected, never committed partially or with a substituted value. A required dependency being unavailable fails the operation rather than degrading it.

## Four storage engines, one application contract

Same application code, four operational shapes:

| Engine     | Where it fits                                 | Availability       |
|------------|-----------------------------------------------|--------------------|
| memory     | Local dev, unit tests, digital-twin scenarios | open source        |
| sqlite     | Edge, single-node self-host, persistent dev   | open source        |
| postgres   | Production transactional workloads, HA        | open source        |
| cassandra  | Distributed scale, high write throughput      | commercial (Cyoda) |

Switch by setting `CYODA_STORAGE_BACKEND` — no code changes. The cassandra engine is offered as a commercial backend by Cyoda for workloads that outgrow a single PostgreSQL primary; contact information is on the [cyoda.com](https://cyoda.com) website.

## Try it in 30 seconds

```bash
brew install cyoda/cyoda-go/cyoda
cyoda init && cyoda &
curl http://localhost:8080/api/health
# {"status":"UP"}
```

`cyoda init` writes a sqlite-backed user config (default path `~/.local/share/cyoda/cyoda.db`); `cyoda` then starts the server with that config and mock auth. See **Install** for non-Homebrew options and **First real call** for jwt + a real authenticated request.

## Install

### Homebrew (macOS / Linux)

```bash
brew install cyoda/cyoda-go/cyoda
```

### curl (any Unix)

```bash
curl -fsSL https://github.com/cyoda/cyoda-go/releases/latest/download/install.sh | sh
```

Installs to `~/.local/bin/cyoda` and runs `cyoda init`. Pin a version with `CYODA_VERSION=v0.7.1 curl ... | sh`. The installer SHA256-verifies the archive and, if [`cosign`](https://docs.sigstore.dev/cosign/installation/) is on `PATH`, also verifies a Sigstore keyless signature from the cyoda-go release workflow.

### Debian / Ubuntu / Fedora / RHEL

```bash
# Debian / Ubuntu
wget https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb

# Fedora / RHEL
wget https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.rpm
sudo rpm -i cyoda_linux_amd64.rpm
```

Replace `amd64` with `arm64` for ARM hosts. Both packages drop `/usr/bin/cyoda` and `/etc/cyoda/cyoda.env` (sqlite as the system-wide default, preserved across upgrades).

### From source

Requires **Go 1.26+**.

```bash
go install github.com/cyoda-platform/cyoda-go/cmd/cyoda@latest
```

This binary uses the in-memory backend by default. Run `cyoda init` for sqlite persistence, or set `CYODA_STORAGE_BACKEND` directly.

## First real call

The 30-second example uses mock auth. To exercise the real auth chain end-to-end with sqlite + jwt, use the project's profile pattern. The only credential is the JWT signing key; `cyoda token` signs the first admin token with it, offline:

```bash
# Generate a JWT signing key (openssl writes it 0600 by default; make it explicit)
openssl genrsa -out /tmp/jwt.key 2048
chmod 600 /tmp/jwt.key

# Write a local profile with sqlite + jwt. .env.local is gitignored.
cat > .env.local <<'EOF'
CYODA_STORAGE_BACKEND=sqlite
CYODA_IAM_MODE=jwt
CYODA_JWT_SIGNING_KEY_FILE=/tmp/jwt.key
EOF

# Start cyoda with the local profile (loads .env.local automatically)
CYODA_PROFILES=local cyoda &

# Sign a short-lived admin token for tenant "demo" with the same key
TOKEN=$(CYODA_PROFILES=local cyoda token --tenant demo)

# Make an authenticated call
curl -H @- <<<"Authorization: Bearer $TOKEN" http://localhost:8080/api/account
```

The `/api/account` response confirms the token's tenant and roles. With that token, create the M2M clients that applications and compute nodes use (`POST /api/clients`; see `cyoda help auth clients` and `cyoda help cli token`). Every data operation — models, entities, search, messages — requires `ROLE_M2M`: an M2M client's tokens carry it, and a `cyoda token` carries it only when signed with `--roles ROLE_ADMIN,ROLE_M2M` (see `cyoda help auth`). From here, follow the **Build an app** link below to register an entity model and start creating entities.

**Optional IAM settings:**

| Env var | Default | Effect |
|---------|---------|--------|
| `CYODA_JWT_ISSUER` | `cyoda` | The `iss` of every token cyoda-go issues, and the value every user assertion's `aud` must contain. It names this deployment, not an identity provider. An empty value refuses to start. |
| `CYODA_JWT_EXPIRY_SECONDS` | `300` | Maximum lifetime of a token cyoda-go issues, in seconds (`cyoda token --ttl` and an on-behalf-of token can be shorter), and the upper bound of `cyoda token --ttl`. Must be an integer from 1 to 3600; any other value refuses to start. A token exchange's token also ends no later than its assertion's `exp`. |
| `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` | `false` | When `true`, enables the 5 `/oauth/keys/trusted/*` admin endpoints. When `false`, those endpoints return `404 FEATURE_DISABLED`. |
| `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` | `10` | Per-tenant cap on trusted keys that can verify (active, `validTo` not passed); registering or reactivating past it returns `400 TRUSTED_KEY_CAP_REACHED`. `0` means unbounded. |
| `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` | `365` | Validity, in days from `validFrom`, of a trusted key registered without `validTo` (not a cap on a `validTo` you send). Rotate keys before it ends. |
| `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` | `false` | When `true`, `POST /clients?withAdminRole=true` may grant `ROLE_ADMIN` to created M2M clients. When `false` (default), that request shape returns `404 FEATURE_DISABLED`. |
| `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` | `100` | Per-tenant cap on M2M clients; `POST /clients` at the cap returns `400 M2M_CLIENT_CAP_REACHED`. `0` means unbounded; a negative value refuses to start. |
| `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` | `600` | Token requests each M2M client may make per minute on one node, across both grants; over it `POST /tenants/{tenant}/oauth/token` returns `429 slow_down` with `Retry-After`. `0` means unlimited; a negative value refuses to start. |
| `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS` | the number of CPUs the process may use (GOMAXPROCS) | Client-secret (bcrypt) operations that run at once on one node: `POST /tenants/{tenant}/oauth/token` checks and the secret hashing of `POST /clients` and the secret reset. An operation that gets no slot within 1 s returns `503` with `Retry-After` (`temporarily_unavailable` on the token endpoint, `SERVER_BUSY` on `/clients`). Must be at least `1`. The bound protects the node, not the endpoint: put a per-source rate limit in front of `/api/tenants/{tenant}/oauth/token` at the ingress. |

In mock mode, `CYODA_IAM_MOCK_KIND` (default `service`) sets the principal kind
(`user`/`service`/`system`) of the fixed mock principal every request runs as. The default makes
every mock-mode caller a client, as in `jwt` mode; `user` or `system` lets
local/CI setups exercise user- or system-attributed code paths without real
JWT auth. Any other value, in either mode, refuses to start.

## Access

Only M2M clients connect to cyoda-go; users never call it directly. An
application signs its own users in, decides what each user may do, and calls
cyoda-go for them. cyoda-go has no per-user permissions: it records what the
client states about the user and does not verify the user. Like any database,
it cannot protect data from an application that is itself compromised.

- **M2M clients** (`POST /clients`) belong to one tenant and get tokens with
  `client_credentials`. A plain client holds `ROLE_M2M`, which every data
  operation requires; an admin client also holds `ROLE_ADMIN`. A service or a
  compute node uses a client of its own, and its changes are recorded as the
  client's.
- **On-behalf-of clients** (`POST /clients?onBehalfOf=true`) act for the
  application's users. They use only the token exchange (RFC 8693): the
  client presents a short user assertion the application signed, and gets a
  token for that user carrying the client's roles. Every change made with it
  is recorded for the user, with the client as its executor, and both reach
  compute nodes in each callout. An on-behalf-of client never holds
  `ROLE_ADMIN` and never exists in the `PLATFORM` tenant.
- **Trusted keys** (`/oauth/keys/trusted*`) are the public keys a tenant admin
  registers for the application's user assertions. A trusted key verifies
  only an assertion presented to the token exchange, in its own tenant; it is
  never accepted as a bearer token.

The platform operator's first admin token comes from `cyoda token` (see
*First real call*).

**Integrating an application?** `cyoda help auth integration` is the
step-by-step guide: which clients to create, how to register a trusted key,
how to sign and exchange user assertions, how compute nodes connect and read
the user and executor of each callout, every token-endpoint error with its
retry rule, rotation, incidents, a local end-to-end recipe, and the move from
forwarded identity-provider tokens.
[`docs/access-to-the-cyoda-api.html`](docs/access-to-the-cyoda-api.html) is
the same guide with scenario diagrams; `cyoda help auth` is the reference.

### Auth cache reconciliation

The signing-key cache pushes updates to peers on write and falls back to a
periodic KV-reconcile if a broadcast is missed. No node keeps a copy of a
trusted key or an M2M client: every token request and every client or
trusted-key call reads the store, so a delete, a secret reset or a key
invalidation stops new tokens at once on every node. Tokens already issued
keep verifying until their `exp` (a compute-node stream closes within a
minute instead); to cut them off, see *A leaked platform admin-client secret*
in `cyoda help config auth`.

| Env var | Default | Effect |
|---------|---------|--------|
| `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` | `60s` | Reconcile interval for the signing-key cache; verification fails closed after 10× this without a successful KV reconcile. |

## Composite unique keys

An entity model can declare one or more **composite unique keys** — each key is a set of scalar field paths that must be unique across all live entities of that model within a tenant.

**Declaring keys** requires the model to be `UNLOCKED`:

```
PUT /api/model/{entityName}/{modelVersion}/unique-keys
```

Request body:

```json
{
  "uniqueKeys": [
    { "id": "by-email", "fields": ["$.email"] },
    { "id": "by-org-and-handle", "fields": ["$.org", "$.handle"] }
  ]
}
```

This call is idempotent — it replaces the model's entire key list. The keys are validated immediately (field paths must be known scalar leaves in the inferred schema). After locking the model, no entity create or update can produce a duplicate value-set for any declared key.

**Key semantics:**

- **Scope:** per `(tenant, model name, model version)`, live entities only. Soft-deleting an entity frees its key value-set.
- **Null rule (all-or-nothing):** if all fields in a key are absent or null, the entity is exempt. If some but not all fields are present, the write is rejected with `422 INVALID_UNIQUE_KEY`. If all fields are present, uniqueness is enforced.
- **String comparison is byte-exact:** case-sensitive, no Unicode normalization, no whitespace trimming — the bytes the application wrote are what is compared. Applications that want case-insensitive matching must normalize before writing.
- **Enforced on create and update.** Moving a key value to a free slot is allowed; moving it to a slot already taken by another entity returns `409 UNIQUE_VIOLATION`.
- **Supported backends:** memory, sqlite, postgres. The commercial backend returns `422 COMPOSITE_KEY_UNSUPPORTED` until its own support lands.

**Multi-node note:** see the `cluster` help topic — *Composite unique key staleness* — for a bounded operational limitation when changing a key on a live multi-node postgres deployment.

## Search result sorting

Search endpoints accept one or more `sort` query parameters to order results by scalar data or meta fields:

```
POST /api/search/direct/{entityName}/{modelVersion}?sort=price:asc&sort=@creationDate:desc
```

Grammar: `[@]path[:asc|desc]` — a bare dotted path sorts by a scalar entity-data field; the `@` prefix sorts by a meta field. Direction defaults to `asc`. Repetition order is sort precedence; `entity_id` is always the final tiebreaker. Absent/null values sort last.

**Sortable meta fields:** `state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`.

**Error:** unsortable, unknown, or non-scalar paths return `400 INVALID_FIELD_PATH`.

**Key cap:** `CYODA_SEARCH_MAX_SORT_KEYS` (default `16`) — see `cyoda help config` (Search and transaction internals).

## Async search backpressure

`POST /api/search/async/{entityName}/{modelVersion}` runs on a bounded worker pool rather than one goroutine per submission. `CYODA_SEARCH_ASYNC_WORKERS` (default `8`) sizes the pool; `CYODA_SEARCH_ASYNC_QUEUE` (default `256`) sizes its submit queue. Once both are exhausted, submission fails fast with a retryable `503 SEARCH_QUEUE_FULL` instead of queuing indefinitely or spawning unbounded goroutines. The pool is shared by every tenant, so `CYODA_SEARCH_ASYNC_MAX_PER_TENANT` (default: the worker count; `0` disables) caps how many jobs one tenant may have in flight on a node — queued and running are counted together, so one tenant's burst can hold at most that many of the shared queue's slots and the rest stay available to other tenants, whose submissions are still accepted and served as workers free up. The cap applies to a single-tenant deployment too: with the defaults its accepted-in-flight ceiling is 8, not `workers + queue`. Results stream to storage incrementally as the scan runs. A running job stamps liveness every `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` (default `15s`), starting at submit time (queued or scanning), and the same poll observes a cross-node cancel or externally-recorded terminal status. A background reaper claims and re-executes any job whose heartbeat has gone silent for `CYODA_SEARCH_JOB_STALE_AFTER` (default `5m`, must be >= 4x the heartbeat interval) — e.g. its owning node crashed — on a live node, on the heartbeat-interval ticker plus a startup sweep; a job is failed only after `CYODA_SEARCH_JOB_MAX_ATTEMPTS` (default `3`) executor losses. A graceful node shutdown or restart releases its in-flight jobs immediately, so they hand off to a peer (or the restarted node's own startup sweep) within one heartbeat interval rather than waiting to go stale. See `cyoda help config` (Search internals) and `cyoda help errors SEARCH_QUEUE_FULL`.

## Scheduled transitions

A workflow transition with a `schedule` fires automatically after a delay. The delay can be a static `delayMs`, or a `function` callout computing the firing time (and optional expiry) per entity at arm time — mutually exclusive with `delayMs`. Every node claims due scheduled tasks and runs them itself. See `cyoda help config scheduler` for the full topic.

| Env var | Default | Effect |
|---------|---------|--------|
| `CYODA_SCHEDULER_ENABLED` | `true` | Kill switch: a node with `false` claims no scheduled task. |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | `1s` | How often a node claims due tasks. |
| `CYODA_SCHEDULER_MAX_RUNS` | `8` | Most scheduled runs one node holds at once. |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | `4` | Most runs of one tenant on one node (per node, not per cluster); at most `CYODA_SCHEDULER_MAX_RUNS`. |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | `15s` | How often a node records that it is alive. |
| `CYODA_SCHEDULER_STALE_AFTER` | `2m` | Time without a heartbeat before another node takes over a node's runs; at least `50s + 3 × heartbeat`, the same on every node. |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | `3` | A task whose node is lost this many times ends FAILED. |
| `CYODA_SCHEDULER_RETRY_DELAY` | `30s` | Delay before the first retry of a safe failure; doubles on each further one. |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | `15m` | The retry delay never grows past this. |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | `20s` | On shutdown, how long a node waits for its runs before it cancels them. |

## Compute-node callouts

A processor, criterion or function request to a compute member is a *callout*.

| Variable | Default | Description |
|---|---|---|
| `CYODA_RETRY_FIXED_NUM_RETRIES` | `3` | Retries after the first try when `retryPolicy` is `FIXED` or unset. |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` | `30000` | Answer limit when the callout sets no `responseTimeoutMs`. |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` | `60000` | Upper bound on `responseTimeoutMs`; workflow import refuses more. |
| `CYODA_DISPATCH_WAIT_TIMEOUT` | `5s` | The patience: how long a callout waits, in total, for a compute member to exist. `0` disables waiting. |
| `CYODA_DISPATCH_CONNECT_TIMEOUT` | `2s` | Time allowed to open the connection when a callout is handed over to another node. |
| `CYODA_CALLOUT_HANDOVER_ALLOWANCE` | `30s` | What the owning node allows a hand-over on top of `tries left × answer limit`. |
| `CYODA_CALLOUT_PASS_ALLOWANCE` | `30s` | How long a compute member's transaction token outlives its try's answer limit. |
| `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES` | `10485760` | Ceiling on the answer to a compute member's callback, held in memory while the transaction is. A larger answer fails with `413 JOINED_RESPONSE_TOO_LARGE`. |
| `CYODA_CALLOUT_JOINED_MAX_WAITERS` | `128` | How many of a compute member's callbacks may queue for one transaction. Past the cap: `503 TOO_MANY_JOINED_REQUESTS`, retryable. |

See `cyoda help config grpc` and `cyoda help config cluster`.

## Where to go next

Online docs at [docs.cyoda.net](https://docs.cyoda.net) mirror the `cyoda help` topic tree — the same content is available offline via `cyoda help <topic>`.

Run `cyoda help config all` for the complete env-var reference (add `--format=json` for machine-readable output); `cyoda help config cluster` covers multi-node/dispatch vars.

| Goal                          | Link                                              |
|-------------------------------|---------------------------------------------------|
| Build an app fast (Claude Code) | [github.com/cyoda/cyoda-skills](https://github.com/cyoda/cyoda-skills) — install the cyoda-skills plugin and use `/cyoda:app` to scaffold |
| Build an app                  | [docs.cyoda.net/help/quickstart](https://docs.cyoda.net/help/quickstart) |
| Configure                     | [docs.cyoda.net/help/config](https://docs.cyoda.net/help/config)       |
| Error reference               | [docs.cyoda.net/help/errors](https://docs.cyoda.net/help/errors)       |
| Deploy with Helm              | [docs.cyoda.net/help/helm](https://docs.cyoda.net/help/helm)           |
| Deploy with Docker Compose    | [examples/compose-with-observability/](examples/compose-with-observability/) |
| Architecture                  | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)      |
| Transactions and consistency  | [docs/CONSISTENCY.md](docs/CONSISTENCY.md); timing per storage backend: [docs/submit-times-snapshots-consistency-time.html](docs/submit-times-snapshots-consistency-time.html) |
| Application examples          | [docs/PRD.md#target-applications](docs/PRD.md#target-applications) |
| Product overview              | [docs/PRD.md](docs/PRD.md)                        |
| Feature & API inventory       | [docs/FEATURES.md](docs/FEATURES.md)              |
| Multi-node cluster            | [docs.cyoda.net/help/cluster](https://docs.cyoda.net/help/cluster) |
| Admin endpoints (log/trace)   | [docs.cyoda.net/help/admin](https://docs.cyoda.net/help/admin)         |
| Write a storage plugin        | [docs/plugins.md](docs/plugins.md)                |
| Contribute                    | [CONTRIBUTING.md](CONTRIBUTING.md)                |
| Security disclosures          | [SECURITY.md](SECURITY.md)                        |

## Related projects

Sibling repositories under [github.com/cyoda](https://github.com/cyoda) that complement cyoda-go:

- **[cyoda-skills](https://github.com/cyoda/cyoda-skills)** — Claude Code skills (`/cyoda:app`, `/cyoda:design`, `/cyoda:build`, `/cyoda:test`, ...) for AI-assisted Cyoda app development against a local cyoda-go or Cyoda Cloud instance.
- **[cyoda-cloud-cli](https://github.com/cyoda/cyoda-cloud-cli)** — Command-line client for Cyoda Cloud with OAuth 2.0 authentication and Cloud-side API operations.
- **[cyoda-docs](https://github.com/cyoda/cyoda-docs)** — Source for [docs.cyoda.net](https://docs.cyoda.net) — developer guides, onboarding, and the rendered `cyoda help` topic tree.
- **[cyoda-workflow-editor](https://github.com/cyoda/cyoda-workflow-editor)** — TypeScript components for parsing, rendering, and editing Cyoda workflow JSON definitions.

## Versioning

The Cyoda-Go ecosystem follows Semantic Versioning with a leading `v`, under the pre-1.0 convention where **the minor component is the breaking-change signal**:

- **`0.MINOR.0`** — a backward-*incompatible* change to the `cyoda-go` binary's public contract: the HTTP/wire API.
- **`0.x.PATCH`** — any backward-*compatible* change, **including new features**: additive API parameters, new endpoints, new optional SPI fields, and bug fixes all ship as patches.

This is the "leftmost non-zero component is the de-facto major" convention (as used by Cargo and npm's `^0.x` ranges). It keeps the minor counter meaningful — a minor bump means "something under you may have broken" — rather than a feature odometer. The discipline it rests on: **a breaking change never ships in a patch.**

**Each module versions on its own axis.** `cyoda-go-spi`, the `cyoda-go` binary, the in-tree plugins, and the Helm chart are **not** required to share a version number. The compatible combinations are recorded in [`COMPATIBILITY.md`](COMPATIBILITY.md); that matrix, not a shared digit, is the source of truth for what works with what.

**`cyoda-go-spi` is the exception**, because it is effectively an internal library: its only consumers are the in-tree plugins and the commercial Cassandra backend, all released in lock-step with the binary. It therefore ships breaking interface changes in patch releases rather than burning a minor for an audience of two. Consumers must read the SPI's own `### Breaking` changelog section on every bump — for that module the version component is not the breakage signal.

See [`CHANGELOG.md`](CHANGELOG.md) for breaking changes and [`MAINTAINING.md`](MAINTAINING.md#maintenance-of-older-release-lines) for the policy on older release lines.

## License

Apache-2.0 — see [LICENSE](LICENSE).
