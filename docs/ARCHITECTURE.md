# Cyoda-Go Architecture

**Version:** 2.2
**Date:** 2026-08-05

Technical architecture reference for Cyoda-Go, a Go implementation of the Cyoda platform with a pluggable storage layer. This document targets system architects familiar with distributed systems concepts (CAP theorem, Snapshot Isolation, SWIM gossip protocols, first-committer-wins validation).

For product-level context, see the [PRD](PRD.md).

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Storage Architecture](#2-storage-architecture)
3. [Transaction Model](#3-transaction-model)
4. [Multi-Node Routing Architecture](#4-multi-node-routing-architecture)
5. [Workflow Engine](#5-workflow-engine)
6. [gRPC & Externalized Processing](#6-grpc--externalized-processing)
7. [Authentication & Authorization](#7-authentication--authorization)
   - 7.1 [Mock Mode](#71-mock-mode-default)
   - 7.2 [JWT Mode](#72-jwt-mode)
   - 7.3 [OIDC Provider Registry](#73-oidc-provider-registry)
   - 7.4 [Authorization](#74-authorization)
   - 7.5 [Admin listener authentication](#75-admin-listener-authentication)
8. [Error Model](#8-error-model)
9. [Configuration Reference](#9-configuration-reference)
10. [Deployment Architecture](#10-deployment-architecture)
11. [Observability](#11-observability)
12. [Known Gaps](#12-known-gaps)
13. [Design Decisions Log](#13-design-decisions-log)
14. [Non-Functional Limits and Design Boundaries](#14-non-functional-limits-and-design-boundaries)

---

## 1. System Overview

Cyoda-Go is a **modular monolith with a ports-and-adapters architecture**. The stable external port is `cyoda-go-spi`, a small stdlib-only Go module that defines the storage contract. Adapters are storage plugins in separately versioned Go modules — stock plugins (`plugins/memory`, `plugins/sqlite`, `plugins/postgres`) under this repository, proprietary and third-party plugins in their own repositories. The `cyoda-go` binary resolves its active plugin at startup via `spi.GetPlugin(cfg.StorageBackend)`; a custom binary including a third-party plugin is a one-file edit (blank import) of the `main` package.

Non-storage cross-cutting concerns (authentication, audit, processing dispatch, cluster registry) are defined as internal-to-cyoda-go Go interfaces in `internal/contract/`. These are consumer-side ports between cyoda-go's own layers — not plugin concerns.

Domain concepts are grouped under `internal/domain/` by responsibility (`entity`, `workflow`, `model`, `search`, `messaging`, `audit`, `account`). Each follows a consistent handler/service layering over the storage port.

### Repositories

| Module | Path | Purpose | License |
|--------|------|---------|---------|
| `cyoda-go` | github.com/cyoda-platform/cyoda-go | Application core + stock plugins | Apache 2.0 |
| `cyoda-go-spi` | github.com/cyoda-platform/cyoda-go-spi | Storage-plugin contract (stdlib only) | Apache 2.0 |

### Package Layout (`cyoda-go`)

```
cmd/
  cyoda/main.go           Entrypoint; blank-imports stock plugins
  compute-test-client/    Local compute harness for parity tests
  release-preflight/      Release-gate checks
app/                      Application wiring, Config, startup; resolves plugin via spi.GetPlugin
go.mod                    module github.com/cyoda-platform/cyoda-go
go.work                   Lists ., plugins/memory, plugins/postgres, plugins/sqlite

plugins/                  Each plugin is its own Go module with its own go.mod
  memory/                 plugin.go (init() → spi.Register), store_factory.go,
                          txmanager.go (in-process SI+FCW), per-store files, doc.go
  sqlite/                 plugin.go (+ ConfigVars()), store_factory.go,
                          txmanager.go (application-layer SI+FCW), per-store files,
                          query_planner.go / searcher.go / post_filter.go (predicate
                          pushdown to SQL), migrate.go, migrations/
  postgres/               plugin.go (+ ConfigVars()), store_factory.go,
                          transaction_manager.go + txstate.go + tx_registry.go
                          (savepoint-capable TM), commit_validator.go (commit-time
                          read-set validation), config.go (pgx pool setup; reads
                          CYODA_POSTGRES_*), ceilings.go (statement / idle-in-tx
                          bounds), per-store files, migrate.go, querier.go,
                          migrations/ (golang-migrate), doc.go

internal/
  admin/                  Admin listener (/livez, /readyz, /metrics)
  common/                 AppError formatting, error codes, diagnostics, tags, concrete UUIDGenerator
  contract/               Consumer-side interfaces internal to cyoda-go:
                          AuthenticationService, AuthorizationService, AuditService,
                          ExternalProcessingService, ClusterService, NodeRegistry
  match/                  gjson-based predicate match engine (consumed by memory plugin;
                          operates on the predicate.Condition AST)
  logging/                slog wrappers
  observability/          OpenTelemetry SDK init, tracing decorators
  auth/                   JWT (RS256, JWKS, M2M, OBO), key management; auth/oidc/ provider registry
  iam/mock/               Mock authentication for development
  httpmw/                 Transaction-join middleware
  txgate/                 Per-transaction lock; held by every joined request for
                          its whole handling
  fence/                  Which compute member holds a callout's work; refuses
                          the rest
  scheduler/              Scheduled-transition dispatch loop
  domain/
    entity/               Entity CRUD, state machine integration, transaction scope
    model/                Model descriptors, import/export, locking
    workflow/             FSM engine, cascade logic, criteria/processor dispatch
    search/               Sync + async search, predicate evaluation
    account/              Account management
    messaging/            Edge message store
    audit/                Audit trail
    pagination/           Cursor paging
    txjoin/               Callback transaction-join: the pass, the tenant, the
                          fence, and the transaction's lock for the whole request
  callout/                The owner's loop: tries, hand-overs, patience, the
                          callout deadline
  grpc/                   CloudEventsService, streaming, the local procedure of
                          a callout
  api/                    HTTP handlers (generated OpenAPI types); middleware/
  cluster/
    token/                HMAC-signed transaction routing tokens
    proxy/                HTTP reverse proxy + gRPC routing helpers
    registry/             Gossip (memberlist) and local node registries; the Gossip
                          registry implements spi.ClusterBroadcaster and is passed to
                          plugins via spi.WithClusterBroadcaster; tag lists travel
                          beside the metadata (tags.go, tags_worker.go)
    modelcache/           Model-store caching decorator with gossip invalidation
    dispatch/             Cross-node compute dispatch (dispatcher, selector, forwarder)
    peeraddr/             Peer-address SSRF validation
  testing/localproc/      In-process processor for E2E tests
  e2e/                    Full-HTTP-stack E2E suite

api/                      Generated OpenAPI types, gRPC protobuf stubs
proto/                    Protobuf definitions
e2e/parity/               Backend-agnostic parity scenarios (importable by plugin authors)
deploy/                   Dockerfile, compose files, Helm chart
scripts/                  Dev and multi-node cluster scripts
```

### The `cyoda-go-spi` Module

`cyoda-go-spi` is the stable contract module, kept to a minimal dependency set (`google/uuid`, `tidwall/gjson`) so plugin authors do not inherit transitive dependencies beyond what they add themselves.

Three importable packages:

- **`spi`** (the module root) — storage-plugin interfaces and value types:
  - Store interfaces: `StoreFactory`, `EntityStore`, `ModelStore`, `KeyValueStore`, `MessageStore`, `WorkflowStore`, `StateMachineAuditStore`, `ScheduledTaskStore`, `AsyncSearchStore`, `SelfExecutingSearchStore`
  - `EntityStore` includes `Search` (bounded-or-fail predicate pushdown) and `Iterate` (streamed predicate pushdown, yielding an `Iterator`); there is no whole-model read
  - Optional capability interfaces a store may also implement: `GroupedAggregator`, `CompositeUniqueKeyCapable`
  - `TransactionManager` interface (Begin/Commit/Rollback/Join/GetSubmitTime/Savepoint/RollbackToSavepoint/ReleaseSavepoint)
  - Value types: `Entity`, `EntityMeta`, `EntityVersion`, `ModelRef`, `ModelDescriptor`, `WorkflowDefinition`, `StateDefinition`, `TransitionDefinition`, `TransitionSchedule`, `ScheduleFunction`, `ScheduledTask`, `StateMachineEvent`, `TransactionState`, `MessageHeader`, `MessageMetaData`, `ProcessorDefinition`, `SearchJob`, `Principal`, `WriteAttribution`
  - Context: `UserContext`, `Tenant`, `TenantID`, `WithUserContext`/`GetUserContext`, `WithTransaction`/`GetTransaction`
  - Sentinel errors, including `ErrNotFound`, `ErrConflict`, `ErrEpochMismatch`, the transaction-state family (`ErrTxNotFound`, `ErrTxRolledBack`, `ErrTxAlreadyCommitted`, `ErrTxTenantMismatch`, …) and `ErrSearchResultLimitExceeded`
  - `UUIDGenerator` interface — returns `[16]byte` so plugins are not bound to a particular UUID package (callers use the zero-cost `uuid.UUID(x)` conversion if they want the google/uuid type)
  - `ClusterBroadcaster` interface — fire-and-forget, best-effort topic broadcast
  - Plugin machinery: `Plugin`, `DescribablePlugin`, `Startable`, `ConfigVar`, `FactoryOption`, `FactoryConfig`, `WithClusterBroadcaster`, `ApplyFactoryOptions`, `Register`, `GetPlugin`, `RegisteredPlugins`
  - Helper: `DefaultSaveAll` (sequential fallback for `EntityStore.SaveAll`, over an `iter.Seq[*Entity]`)
- **`predicate`** — search AST types and JSON parse/marshal:
  - `Condition` (interface), `GroupCondition`, `SimpleCondition`, `ArrayCondition`, `LifecycleCondition`, `FunctionCondition` + operator constants
  - `ParseCondition(body []byte) (Condition, error)` + marshalers
- **`spitest`** — the behavioural conformance harness. A plugin runs it against its own `StoreFactory` to prove it satisfies the contract; all three stock plugins do.

The `predicate` package imports only the standard library. A plugin that translates predicates to its own query dialect (SQL, CQL) can import it without pulling in a match engine. The in-process tree adapter over the SPI predicate kernel — used for workflow criteria, the conditional-delete residual and the grouped-stats residual — lives in `cyoda-go/internal/match/`.

### Plugin Contract (summary)

```go
// In github.com/cyoda-platform/cyoda-go-spi

type Plugin interface {
    Name() string
    NewFactory(ctx context.Context, getenv func(string) string, opts ...FactoryOption) (StoreFactory, error)
}

type DescribablePlugin interface {   // optional — for --help rendering
    Plugin
    ConfigVars() []ConfigVar
}

type Startable interface {            // optional — for plugins with background work
    Start(ctx context.Context) error
}

type ConfigVar struct {
    Name, Description, Default string
    Required                   bool
}

type FactoryOption func(*factoryConfig)

func WithClusterBroadcaster(b ClusterBroadcaster) FactoryOption
func ApplyFactoryOptions(opts []FactoryOption) FactoryConfig

func Register(p Plugin)               // panics on duplicate Name() — init-time error
func GetPlugin(name string) (Plugin, bool)
func RegisteredPlugins() []string
```

A plugin registers itself from `init()`. The `cyoda-go/main.go` blank-imports the plugins it ships with:

```go
import (
    _ "github.com/cyoda-platform/cyoda-go/plugins/memory"
    _ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
    _ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)
```

A third-party plugin is added to a custom binary by a one-line blank import. No code changes to cyoda-go are required.

### Non-Storage Internal Contracts (`internal/contract/`)

Interfaces between cyoda-go's own layers — HTTP middleware, services, cluster:

```go
// Auth — consumed by internal/api/middleware, implemented by internal/auth and iam/mock
type AuthenticationService interface { ... }
type AuthorizationService interface { ... }

// Audit — consumed by domain services, implemented by internal/domain/audit
type AuditService interface { ... }

// Processing dispatch — consumed by workflow engine, implemented by cluster/dispatch and grpc
type ExternalProcessingService interface {
    DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID) (*spi.Entity, error)
    DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID) (matches bool, reason string, err error)
    DispatchFunction(ctx, entity, fn, workflowName, transitionName, txID) (FunctionResult, error)
}

// Cluster — consumed by HTTP admin API, implemented by cluster/registry
type ClusterService interface { ... }
type NodeRegistry interface { ... }
```

Plugin authors never implement these — they are internal to the cyoda-go application.

Multi-tenancy is intrinsic. Every request context carries a resolved `UserContext` with `TenantID`. All stores, across all plugins, partition by tenant.

A tenant identifier matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$` — 1 to 100 bytes, the first an ASCII letter or digit, case preserved and significant. `common.ValidateTenantID` is the one definition, and it is applied at the only two places a tenant identifier enters the binary from outside it: the `caas_org_id` claim on an inbound JWT (§7.2), which covers every authenticated HTTP request and every authenticated gRPC method, and `CYODA_BOOTSTRAP_TENANT_ID` at startup (§9). Everything downstream — peer dispatch bodies, scheduler payloads, gossip envelopes, scheduled-task and search-job rows, OIDC provider records, the M2M client table — carries a value already admitted at one of those two doors and does not re-check it. The rule is a Cloud-facing contract: see `docs/cloud-parity/tenant-id-grammar.md`.

A user identifier is valid UTF-8, 1 to 255 characters, with no control character (U+0000–U+001F, U+007F–U+009F), no noncharacter and no U+FFFD; nothing is normalised. `common.ValidateUserID` is the one definition, applied at every place a principal's user id enters from outside: the first-party `caas_user_id` claim (or `sub` when `caas_user_id` is absent), the OIDC `sub`, the token-exchange subject `sub`, and `CYODA_BOOTSTRAP_USER_ID` at startup. The excluded characters are those the CloudEvents spec forbids in a string attribute, since the user id is sent to compute nodes as `authid`, plus U+FFFD, so that invalid input cannot alias a real id. The OIDC path builds its user ids as `oidc:<providerId>:<sub>`, so `oidc:` is a reserved word: `common.ValidateFirstPartyUserID` rejects it, in any case, at every other door. See `docs/cloud-parity/user-id-rule.md`.

---

## 2. Storage Architecture

A running cyoda-go binary hosts exactly one active storage plugin, resolved at startup:

```go
plugin, ok := spi.GetPlugin(cfg.StorageBackend)   // default: "memory"
if !ok {
    slog.Error("unknown storage backend", "backend", cfg.StorageBackend,
        "available", spi.RegisteredPlugins())
    os.Exit(1)
}

var opts []spi.FactoryOption
if gossipReg != nil {   // non-nil only when cluster mode is enabled
    opts = append(opts, spi.WithClusterBroadcaster(gossipReg))
}

factory, err := plugin.NewFactory(startupCtx, os.Getenv, opts...)

// Start runs BEFORE TransactionManager: plugins whose TM depends on
// Start's side effects would otherwise init a half-ready TM. Plugins
// with no background lifecycle don't implement Startable and this is
// a no-op for them.
if s, ok := factory.(spi.Startable); ok {
    s.Start(startupCtx)
}

txMgr, _ := factory.TransactionManager(startupCtx)
```

`startupCtx` carries `CYODA_STARTUP_TIMEOUT`, so plugin init, migrations and cluster join share one deadline. Between `NewFactory` and `Start` the factory is wrapped in the model-cache decorator (§4.1) and given the schema-replay apply function.

No per-store routing. No swap logic for transaction managers. Every store in the binary comes from the same plugin, and the plugin supplies its own `TransactionManager` whose semantics match its storage engine.

### 2.1 The `memory` plugin (`plugins/memory/`)

Ephemeral, in-process state with microsecond-latency SI+FCW concurrency
control. Default for tests, local development, and high-throughput
digital-twin workloads where durability is delegated elsewhere. Full
detail in [docs/plugins/IN_MEMORY.md](plugins/IN_MEMORY.md).

### 2.2 The `sqlite` plugin (`plugins/sqlite/`)

Persistent, zero-ops single-node storage. Embedded in-process via a
pure-Go (WASM) SQLite driver, exclusive file lock, application-layer
SI+FCW concurrency control, search predicate pushdown to SQL. Default
for desktop binary, edge deployments, and containerised single-node
production. Full detail in [docs/plugins/SQLITE.md](plugins/SQLITE.md).

### 2.3 The `postgres` plugin (`plugins/postgres/`)

Durable multi-node storage. PostgreSQL `REPEATABLE READ` provides
snapshot isolation; an application-layer read-set validation at commit
time provides first-committer-wins on entity-level conflicts. Works
against any managed PostgreSQL 14+ platform (RDS, Cloud SQL, Azure,
Supabase, Neon, Aiven, Crunchy Bridge, self-hosted, etc.). Full detail
in [docs/plugins/POSTGRES.md](plugins/POSTGRES.md).

Model storage splits into two tables: `models` carries stable
metadata (state, ChangeLevel, base schema) and `model_schema_extensions`
is an append-only log of typed-op deltas produced by
`ExtendSchema`. Appending rather than updating keeps concurrent entity
writes with `ChangeLevel != ""` off a single hot row. Plugin-internal
savepoints every `CYODA_SCHEMA_SAVEPOINT_INTERVAL` rows (default 64)
bound the fold cost on read; the `sqlite` plugin uses the same split and
the same knob. See
[docs/CONSISTENCY.md §3a](CONSISTENCY.md#3a-model--data-contract).

### 2.4 The `cassandra` plugin (commercial)

A Cassandra-backed storage plugin is available as a commercial offering
from Cyoda. It slots into cyoda-go through the same `spi.Plugin` contract
as the open-source plugins — operators select it at runtime via
`CYODA_STORAGE_BACKEND=cassandra`.

**Capability envelope:**

- Horizontal write scalability across a Cassandra cluster
- Snapshot isolation with first-committer-wins semantics (same
  published contract as the open-source plugins — see
  [docs/CONSISTENCY.md](CONSISTENCY.md))
- Append-only point-in-time storage with full historical reads
- No single points of failure
- Multi-node consistency
- **Cluster-coordinated transactions** — transactions are not pinned
  to a single owning cyoda-go node. A transaction survives the
  unavailability of individual cluster nodes mid-flight, eliminating
  the `TRANSACTION_NODE_UNAVAILABLE` failure mode that the postgres
  plugin exposes under node affinity (see §4 Multi-Node Routing and
  PRD §4 Multi-Node Transaction Affinity)

**When it fits:** workloads whose write volume or availability
requirements outgrow a single-primary PostgreSQL deployment — while
keeping the same EDBMS semantics (entities, workflows, temporal
history, uniform isolation contract) that the open-source binary
provides on top of the in-memory / sqlite / postgres plugins.

**Interested?** Get in touch with Cyoda at
[cyoda.com](https://www.cyoda.com) and use its contact page.

---

## 3. Transaction Model

### 3.1 TransactionManager SPI

```go
type TransactionManager interface {
    Begin(ctx context.Context) (txID string, txCtx context.Context, err error)
    Commit(ctx context.Context, txID string) error
    Rollback(ctx context.Context, txID string) error
    Join(ctx context.Context, txID string) (txCtx context.Context, err error)
    GetSubmitTime(ctx context.Context, txID string) (time.Time, error)
    Savepoint(ctx context.Context, txID string) (savepointID string, err error)
    RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error
    ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error
}
```

- `Begin`: Resolves tenant from context, generates a UUID txID, creates a transaction, returns a new context carrying the `TransactionState`.
- `Join`: Attaches to an existing active transaction by txID. Used when a proxied CRUD request arrives at the transaction-owning node. Verifies tenant match.
- `Commit`: Validates, flushes, records. Returns `common.ErrConflict` on serialization failure (Snapshot Isolation with first-committer-wins (SI+FCW); see [docs/CONSISTENCY.md](CONSISTENCY.md) for the full contract and per-plugin implementation).
- `Rollback`: Marks transaction rolled back, clears from active map. Waits for in-flight operations via `OpMu`.
- `GetSubmitTime`: Returns the database timestamp captured at commit. Used for temporal ordering.
- `Savepoint` / `RollbackToSavepoint` / `ReleaseSavepoint`: nested-savepoint support used by the workflow engine's `ASYNC_NEW_TX` execution mode. The plugin returns a savepoint ID that the caller passes back for rollback or release. Plugins that don't support savepoints may return `common.ErrUnsupported`.

**TX boundary ownership.** For most cascades the request handler in `internal/domain/entity/service.go` opens the transaction, calls the engine, and commits when the engine returns — a single `Begin`/`Commit` pair, producing a single `Save`, a single `Commit` and a single `EntityVersion` row. When a transition carries a `COMMIT_BEFORE_DISPATCH` processor (see §5.4), the workflow engine — not the handler — owns the transaction boundaries: the engine flushes the pre-callout entity state via `EntityStore.Save`, commits `TX_pre`, dispatches the processor outside any transaction, opens `TX_post` on the same node, applies the result via `CompareAndSave` (CAS expected = the txID stamped at `TX_pre`'s commit), and commits. Per-segment SPI writes are issued by the engine; the handler hands `txMgr` and the `If-Match` precondition to the engine and lets it own boundaries.

**Mid-cascade home-node crash with `COMMIT_BEFORE_DISPATCH`.** If the home node crashes after `TX_pre` commits and before `TX_post` opens (or before `TX_post` commits), the entity is durable in the pre-callout state but the in-flight orchestration is lost — there is no engine-side reaper for the stranded cascade. The client retries the original API call, which restarts the cascade from the beginning; the dispatched processor must be idempotent or detect prior completion via an external resource identifier. Recovery is the application's concern; the engine does not automatically resume mid-cascade. See [docs/CONSISTENCY.md](CONSISTENCY.md) §10 and `cmd/cyoda/help/content/workflows.md` for the workflow-author idempotency requirements.

### 3.2 In-Memory SI+FCW Conflict Detection

Extracted to [docs/plugins/IN_MEMORY.md](plugins/IN_MEMORY.md).
See also [docs/CONSISTENCY.md](CONSISTENCY.md) for the cross-plugin
contract.

### 3.3 Postgres SI+FCW via `REPEATABLE READ` + commit-time validation

Extracted to [docs/plugins/POSTGRES.md](plugins/POSTGRES.md).
See also [docs/CONSISTENCY.md](CONSISTENCY.md) for the cross-plugin
contract.

### 3.4 What Bounds a Transaction

**Release on every exit path.** An entity write flow opens its transaction through a deferred scope (`txScope`, `internal/domain/entity/txscope.go`) that rolls back the segment currently open unless the flow committed it. One deferred `Release` covers every return, every error branch, and a panic unwinding the stack, so a transaction is never abandoned open with its pooled connection unreturned. A joined callback never rolls back its owner's transaction; a segment the engine opened during the call is released regardless of ownership. The workflow engine carries the same guard for the segments it opens itself, since those are its own until handed back.

**Panic containment.** Five recovery sites wrap code that runs the engine or the store on the application's behalf and latch the node unhealthy: the HTTP `Recovery` middleware (outermost on the API server), the gRPC server (unary and stream interceptors), the async-search executor, the search reaper (the snapshot-TTL sweep on `SearchReapInterval` and the stale-job reclaim sweep on the finer `SearchJobHeartbeatInterval` run on two separate tickers, both independently panic-latching), and the scheduler's dispatch goroutine. All five log the value and stack, record a sanitized outcome (a ticket-carrying error on the request doors, a `FAILED` job for async search, a log line for the reaper and for a scheduled fire, the latter with no caller to answer), and mark the node unhealthy. The criterion is what the recovered code was doing, not where it entered from: a panic inside engine or store code leaves state nothing has verified. That is why the scheduler site latches too — `ClusterExecutor.Execute` fires in-process whenever distribution picks this node, so otherwise an identical panicking fire would withdraw the node only when the pick happened to be a peer.

Further recovery sites deliberately do **not** latch, because they wrap probes, notification callbacks or per-connection framing rather than domain work: the admin listener, which runs the same `Recovery` middleware with no health flag (`cmd/cyoda/adminserver.go`) so a panic in `/livez`, `/readyz` or a `/metrics` scrape still answers a ticket-carrying 500 without withdrawing the node; the member-registry `onChange` fan-out (`internal/grpc/members.go`); the OIDC broadcast handler with its dispatch goroutines (`internal/auth/oidc/broadcast.go`, which counts panics on its own metric); and each per-member gRPC stream's three per-connection goroutines — the writer (`Member.writeLoop`), the receive goroutine (`receiveLoop`) and the keep-alive loop (`keepAliveLoop`), all in `internal/grpc/streaming.go` and `members.go` — which recover with a ticket-carrying status and evict just that member rather than latching the node, since each wraps only that member's own framing or liveness bookkeeping, not domain work. None of these sites holds a transaction, and all self-heal: the admin surface on the next probe, the fan-out and broadcast handler on the next event, the three per-member goroutines by the client reconnecting as a fresh member.

Nothing resets the flag: it latches on the first panic recovered in engine or store work, and `GET /health` on the API listener mirrors it directly — `200 {"status":"UP"}` while healthy, `503 {"status":"DOWN"}` once latched — while the admin listener's `/readyz` (§7.5) reads the same flag and reports `503` for the same reason. A node that has panicked has unverified state, so taking it out of service is the correct response rather than continuing to serve from a state nothing has checked. Read the ticket in the log, then replace the node — nothing re-arms the flag. `/health` and `/readyz` read the same flag but serve different audiences: `/readyz` (with `/livez`, unconditional) is the deployment probe on the admin listener; `/health` is a plain summary for humans and simple scripts.

What the flag actually stops, and what it does not:

- **Stops:** new client connections arriving through the Kubernetes Service. The chart's readiness probe (5s period, 3 failures) drops the pod from the Service endpoints in ~10-15s, and both the Gateway `HTTPRoute` and the `Ingress` route through that Service.
- **Does not stop:** peer-forwarded work. The chart always enables cluster mode, and peers address each other through the gossip registry, not the Service — tx-affinity proxying, cluster dispatch and the peer scheduler RPC all keep reaching the node, and the scheduler's round-robin distribution does not read node liveness, so it retains its share of every scan. Established connections — a compute node holding a gRPC stream, for instance — are not closed either.
- **Does not restart it.** `/livez` is unconditional and does not read the flag, deliberately: a deterministic panic (a poisoned entity, a bad workflow definition) would otherwise recur on the next request and turn a restart into a loop. Replacing a drained node is an operator action.

`/readyz` fails for two independent reasons — storage not initialised, or a recovered panic — and reports which in the server-side log while answering the probe generically.

**Storage ceilings** (postgres plugin, §9):

| Ceiling | Default | Bounds |
|---|---|---|
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | Any single SQL statement |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | A connection sitting idle inside an open transaction |
| `CYODA_POSTGRES_ACQUIRE_TIMEOUT` | `10s` | The wait for a free pooled connection |
| `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` | `30m` | Async search scans, which get their own higher ceiling |
| `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` | `5m` | The lock wait during schema migration |

Four of the five are set on the server side, so PostgreSQL enforces them whether or not the application is still watching. `CYODA_POSTGRES_ACQUIRE_TIMEOUT` is the exception: `pgxpool.Config` has no acquire-timeout field, so that deadline is applied Go-side by the pool.

How an abort surfaces depends on whether retrying could plausibly work:

- **Transient contention → `503 STORAGE_UNAVAILABLE`, retryable.** An operation that cannot get a connection within `CYODA_POSTGRES_ACQUIRE_TIMEOUT` — a write, or a read needing a second connection while the caller's transaction holds one — and an operation whose transaction PostgreSQL already aborted for exceeding `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`. A second attempt may well succeed.
- **Statement ceiling exceeded → `500` with a ticket, not retryable.** A statement cancelled by `CYODA_POSTGRES_STATEMENT_TIMEOUT`. Re-running work that just exceeded its ceiling will exceed it again, so advertising a retry would be a lie.
- **Async scan ceiling exceeded → recorded on the job, never an HTTP status.** A scan cancelled by `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` fails the job it belongs to: the job goes `FAILED` with a fixed message, and `GetJob` serves that back verbatim. No ticket is minted, because there is no response to attach one to.

In all three cases the server log names the setting that fired, which is what turns an otherwise unexplained failure into a diagnosable one. See `cyoda help errors STORAGE_UNAVAILABLE` for the caller-facing statement of the retryable/non-retryable split.

**Callout timeouts must fit under the idle ceiling.** A `SYNC` or `ASYNC_SAME_TX` callout holds its transaction's connection idle for its whole duration — and that duration is the callout's deadline, not one try's answer limit: the owner may try several compute members, and several nodes, under one deadline (§4.3). So the arithmetic that has to fit under `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` is `tries × answer limit + patience + hand-over allowance`, which is 155s at the defaults and 275s with the answer limit at its configured upper bound. `responseTimeoutMs` is bounded at import by `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` but not against the idle ceiling, and the two are set independently: a deployment that raises the retry count or the allowances past the ceiling gets a transaction PostgreSQL aborts mid-callout, and the caller sees `503 STORAGE_UNAVAILABLE`. `COMMIT_BEFORE_DISPATCH` (§5.4) removes the constraint for a given processor by committing before the callout and holding no connection across it.

### 3.5 `pgx.Tx` Single-Owner Property

Extracted to [docs/plugins/POSTGRES.md](plugins/POSTGRES.md).

The property remains load-bearing for the cluster design: see DD-2 in [Section 13](#13-design-decisions-log) — fencing a transaction's *ownership* is not required, because no two nodes can share a PostgreSQL transaction. It is also why a transaction admits one user at a time rather than relying on the store to arbitrate (§3.8).

### 3.6 Plugin-Specific Transaction Managers

Each plugin provides its own `TransactionManager` whose semantics match its storage engine — all delivering the same published Snapshot Isolation + first-committer-wins contract (see §3.7 and [docs/CONSISTENCY.md](CONSISTENCY.md)):

- **memory plugin** — in-process SI+FCW with entity-level read/write sets and a committed-transaction log ([docs/plugins/IN_MEMORY.md](plugins/IN_MEMORY.md)).
- **sqlite plugin** — application-layer SI+FCW over a SQLite file with an exclusive file lock ([docs/plugins/SQLITE.md](plugins/SQLITE.md)).
- **postgres plugin** — PostgreSQL `REPEATABLE READ` for the engine-level snapshot, plus application-layer read-set validation at commit time ([docs/plugins/POSTGRES.md](plugins/POSTGRES.md)). The TM assigns IDs, tracks active/committed sets with timestamps, and supports savepoints as a local stack.
- **Commercial plugins** (e.g. the Cassandra plugin from Cyoda)
  implement their own `TransactionManager` against their underlying
  store's primitives. See §2.4 for the capability envelope of the
  commercial Cassandra plugin.

The core `cyoda-go` never picks a TM. It asks the plugin via `factory.TransactionManager(ctx)` and wraps the result with its tracing decorator when OTel is enabled.

### 3.7 Cross-plugin isolation contract

All four storage plugins deliver the same semantic guarantee:
**Snapshot Isolation with First-Committer-Wins on entity-level
conflicts.** The implementation mechanism differs by plugin — the
guarantee does not.

| Plugin | Engine-level mechanism | Application-layer validation | Effective guarantee | Conflict granularity |
|---|---|---|---|---|
| `memory` | n/a — all in-process Go | committed-log + read/write-set tracking | SI+FCW | per-entity |
| `sqlite` | DB-level write lock | application-layer SI+FCW | SI+FCW | per-entity |
| `postgres` | `REPEATABLE READ` + tuple locks | entity-keyed read-set validation at commit; `40001`/`40P01` retry | SI+FCW | per-entity |
| `cassandra` (commercial) | *(proprietary)* | *(plugin-internal)* | SI+FCW | per-entity |

This contract catches dirty read, non-repeatable read, lost update,
and entity-level write-write / write-after-read conflicts. It does
NOT prevent predicate-based phantom anomalies. Workflow authors
observe an operational rule: do not branch on
`search(predicate).count()` inside a transactional workflow step.
See [docs/CONSISTENCY.md](CONSISTENCY.md) for the full contract,
worked scenarios, the operational rule with three robust
alternatives, and the isolation-level taxonomy.

For the in-process **concurrency model** — what locks gate access to
per-tx state, what's per-node-process vs durable, what cluster routing
covers and what it does not — see
[docs/CONCURRENCY.md](CONCURRENCY.md). It complements CONSISTENCY.md
(which covers the cross-plugin isolation contract) with the
in-process and per-node mechanics.

### 3.8 One User of a Transaction at a Time

An open transaction has more than one candidate user. The request chain that
opened it — the **owner** — runs the workflow engine; while a callout is out,
the compute member handling it makes callbacks that carry the transaction's
pass and join the same transaction. The storage contract leaves serialising
concurrent operations on one transaction to the application (`cyoda-go-spi`
`transaction.go`), and two users at once is not a degradation but a process-wide
failure: on memory and sqlite every in-transaction operation mutates plain maps
— a `Get` writes the transaction's read-set — so two users end in the Go
runtime's unrecoverable `concurrent map writes`; on postgres a joined request
runs on the operation's own `pgx.Tx`, which refuses a second concurrent user
with `conn busy` and synchronises neither its status field nor its statement
cache. This applies in single-node mode exactly as in a cluster.

Two mechanisms sit over it: a lock that admits one user at a time, and a fence
that decides *which* compute member is the current user.

**The transaction's lock.** `txgate.Registry` hands out one exclusive lock per
txID. Every joined request takes it in one place — `txjoin.Joiner.Run`, which
both callback doors go through — from before its first store operation until its
handler returns. A read takes it as much as a write, an entity request as much
as a search, a model load or a message save: the lock sits where they all pass
rather than in each of them. The owner's own chain takes the same lock around its
final `Save` + `Commit` (`internal/domain/entity/service.go`) and around a
rollback (`internal/domain/entity/txscope.go`), and never holds it across
`engine.Execute`. Three things follow from holding it for a whole request:

- **What the lock waits on is never the compute member.** The join layer has
  the whole request in memory before it takes the lock: the HTTP middleware
  verifies the pass, then reads the body under a 10 MB ceiling and hands the
  handler a reader over the bytes (a body past the ceiling is `413`; a pass that
  fails verification is refused before a byte is read), and the gRPC stream
  interceptor receives the one request message — after the same verification,
  which the routing decision has already made — and hands the handler a stream that replays
  it; a gRPC unary message is already complete when the interceptor runs. On the
  way out the handler writes into a buffering response writer (HTTP) or a stream
  whose frames are held (gRPC server-streaming), and the response is sent once
  the lock is released. What is held on the way out has the same 10 MB ceiling as
  the body on the way in — the owner's next move waits behind those bytes — and
  an answer past it fails the request with a ticketed 5xx rather than being sent
  in part. A joined chunked collection that fails at frame *n*
  still delivers frames 1…*n*-1 and then the error. A compute member that
  sends its headers and stalls, or never reads its response, holds nothing.
- **A joined request is not interrupted by its compute member going away.** From
  the moment it holds the lock it runs under `context.WithoutCancel`: a member
  that disconnects in the middle of a statement would otherwise cancel the
  context its statements run on, and pgx closes the operation's connection when
  that happens — the owner's next statement, or its commit, would then fail. A
  joined request ends by finishing or by being refused at a check. The one part
  that its client can still call off is the wait for the lock itself: a request
  still queued has touched nothing, so a member that goes away there takes
  nothing with it, its handler never runs and the handler goroutine returns at
  once rather than parking for the life of the callout. What this gives up is deliberate: a deadline
  the member sets on its own call, the engine's cascade check of the request
  context, the per-item check of a gRPC collection, and the context checks that
  end a scan early on memory and sqlite — a joined search by a member that has
  gone away runs to the end of its data, which is finite, while postgres
  statements stay bounded by the ceilings of §3.4. A **proxied** callback that
  runs a callout of its own for longer than `CYODA_PROXY_TIMEOUT` is answered
  `503 TRANSACTION_NODE_UNAVAILABLE` by the node it arrived at while it runs to
  completion on the owner; one still *queued* for the lock when that timeout
  closes the proxy's connection is dropped on the owner, having touched nothing.
  For a compute member either way, a 503 or a dropped connection on a joined
  write means the outcome is unknown.
- **The lock is not held across a callout.** A callback that reaches a processor
  or a `function` criterion of its own gives the lock up for the length of that
  callout (`txgate.Suspend`, installed by the join layer) and re-takes it
  afterwards, so the lock is never held across anything unbounded. The engine
  suspends it at every dispatch site, which is also what keeps the owner's loop
  free of self-deadlock: `fence.Advance` and the end of a callout take this same
  lock, so a chain that ran a callout while holding it would wait for itself.
  One case keeps the lock: a `COMMIT_BEFORE_DISPATCH` processor reached inside a
  callback dispatches without suspending it — its own callout runs on a
  different transaction, or on none, so nothing deadlocks, but the fence's wait
  on the enclosing transaction can then wait on a compute member for the length
  of that callout.

**The fence.** Giving up on a compute member does not stop it. Its late
*answer* is discarded by request correlation (§6.4), but while it works it makes
callbacks — ordinary API requests carrying a pass. A pass that named only the
transaction would be accepted for as long as the transaction is open, so a slow
member's write could land after its replacement has answered and a later
processor of the same transition has run. `internal/fence` is the arbiter, and
what the pass names is a callout at a number: one `Fence` per process,
which decides in memory, for as long as a callout lasts, which passes are
current. Every callback is already routed to the node holding the transaction
(§4.2), so that node's fence is the only judge, and no database step is needed.

- **The number.** Each callout has a fencing number, the pair `(major, minor)`,
  ordered lexicographically. `Fence.Begin` registers a callout on a transaction
  and returns the context the callout runs under and the func that ends it; the
  owner's loop defers that func, so a callout is ended on every exit path, a
  panic included. No pass is current until the first `Advance`. `Fence.Advance`
  raises the number to `(major, 0)`, which shuts out every pass issued under a
  lower one. The owner raises it **before every try** — before each of its own
  and before each hand-over, not only when the work moves to another member — so
  the pass of each try is minted under a number of its own. A node that received
  a hand-over numbers the tries it makes `minor = 1, 2, …` under the major it
  was given and touches no fence: the arbiter is the owner's.
- **Admission.** `Fence.Admit` is the check on entry, made under one lock over
  the pass's own pair and the pair of every enclosing callout. A pair is current
  when its callout is registered, its `major` equals the registered one, and its
  `minor` is not lower than the highest seen under that `major`. A **higher
  minor is admitted and absorbed** — it becomes the current one, and passes
  carrying a lower one are refused from then on. That is how the owner learns,
  with no message between the two nodes, that the node it handed the callout to
  has moved on to its next try: from the first callback carrying the new number.
  `Advance` resets the highest minor to zero, so the first try of a second
  hand-over is not measured against the last try of the first, and a higher
  minor is absorbed only after every pair on the pass has been verified, so a
  pass refused for an enclosing callout changes nothing. `minor` is covered by
  the pass's HMAC like every other claim. Until a callback with the higher minor
  arrives, or the callout ends, the earlier member of a hand-over is still
  admitted — which is why the `idempotent` declaration a workflow author makes
  says "possibly at the same time on two compute members".
- **`fence.Check`** reports the refusal for a context that was admitted and one
  of whose pairs is no longer current; it absorbs nothing. A context that was
  never admitted — the owner's own chain, an ordinary request — always passes,
  so the owner is never subject to a check.
- **The fence cancels no callback's context.** Cancelling one in the middle of a
  statement makes pgx close the operation's connection, so a successful failover
  would turn into a failed operation because the member being replaced happened
  to be reading; on memory an in-transaction write consults no context at all,
  and on sqlite a cancelled context can only fail a read — the model-reference
  lookup on an entity's first save in the transaction, or a `Get` that misses
  the buffer — never the buffered write and never the transaction. The
  fence therefore works by **checks** and by **the wait**, which hold on every
  backend alike. The only context it cancels is the one `Begin` returns — a
  callout's own, under which no statement of the shared transaction runs.
- **The wait.** After shutting the earlier pass out — in `Advance`, and at the
  end of a callout — the fence takes the transaction's lock once and releases
  it, outside its own mutex. A joined request either made its check under the
  lock *before* the number rose, in which case it still holds the lock and the
  fence waits for it to finish; or it takes the lock afterwards, and its check
  refuses it. There is no third case. So when the work is given to the next
  compute member, nothing of the earlier one is in progress on the transaction —
  no write, no read — and nothing can start; and when a callout has ended,
  answered or failed, the same holds before the engine does anything else.

**Where the fence is enforced.**

1. *On entry.* `txjoin.Joiner` is the one door every callback passes on the
   owner — both transports, and a callback that arrived at another node and was
   proxied. Its order is: verify the pass → `txMgr.Join`, which checks the tenant
   → `Fence.Admit`. The pass is verified on its own first (`Joiner.Verify`), so a
   forged or expired one is refused before the request is read into memory. The tenant check comes first, so a stolen pass tells another
   tenant nothing about which callouts exist, and a callback that arrives after
   the *transaction* has ended is answered `404 TRANSACTION_NOT_FOUND`;
   `410 CALLOUT_SUPERSEDED` is the answer while the transaction is still open. A
   pass naming no callout and no number is `401 UNAUTHORIZED`, as any malformed
   pass. A refused callback performs no store operation of any kind.
2. *Under the transaction's lock.* `txjoin.Joiner.Run` runs `fence.Check` after
   it has taken the lock and before it calls the handler. This is the check that
   gives the right to touch the transaction, and the fence's wait takes the same
   lock. The check is repeated after every `txgate` resume — after each processor
   callout, after a `function` criterion and after the scheduled-transition
   arming callout — and once more directly after each processor returns, before
   its audit event and before any mode decides what its result means; without
   that last one `ASYNC_NEW_TX`'s "log and continue" would carry a refused chain
   on to the next processor and past the last one to the final save. One guard in
   the engine's `recordEvent` keeps a refused chain from writing an audit row, so
   no further recording site can appear later. There is deliberately **no check
   before a joined request's final write**: a chain that reaches it has held the
   lock since its last check, so the fence is still waiting for it and its write
   lands before anything the owner does next. It is answered 200, because what it
   wrote is in the transaction.
3. *A callback waiting on a callout of its own* is released rather than checked:
   the inner callout runs under the context `Begin` returned, which is cancelled
   — with the fence's own cause — when an enclosing pair stops being current,
   through `Advance`, through the end of the enclosing callout, or through a
   higher minor absorbed by `Admit`. It returns `410 CALLOUT_SUPERSEDED` and ends
   the inner callout, which shuts the inner compute member out in turn. The pairs
   travel as a context value, so an inner callout is begun under them even where
   the engine has detached the context from cancellation.

**Savepoints under `ASYNC_NEW_TX`.** The processor runs inside a savepoint of
the parent transaction. Its own failure is non-fatal: the savepoint is undone
and the pipeline continues. A savepoint that cannot be **created, undone or
released** is not a processor failure — it says the transaction is unusable — so
it is marked with `workflow.ErrSavepointInfra` and **fails the operation** with a
ticketed 5xx (an unavailable store keeps its `503 STORAGE_UNAVAILABLE`, which is
classified first); it is never reported as the processor's own error. A chain the fence refuses after its
callout neither undoes nor releases its savepoint: by then the replacement
member may have written, and undoing a savepoint restores the whole buffer on
memory and sqlite and everything since on postgres. An abandoned savepoint is
harmless on every backend.

**What is not stopped, and why that is acceptable.** For tries made by another
node, the earlier compute member stays admitted until the first callback of the
later one arrives; its writes are repeats of an idempotent processor's own
writes, and the wait at the end of the callout still puts all of them before
anything the engine does next. A joined request already in progress when its
member is replaced — a read as much as a write — runs to completion, and the
fence waits for it. What a processor did outside cyoda is outside every one of
these mechanisms: a rollback does not undo it and a repeat does it again, which
is what the `idempotent` declaration is about.

---

## 4. Multi-Node Routing Architecture

Multi-node cluster mode is **opt-in** via `CYODA_CLUSTER_ENABLED` (default: `false`). The `false` default is an onboarding affordance — it does not make cluster/HA features secondary. Multi-node correctness (proxy routing, tx-affinity, cross-node callback join, peer failover) is a primary design target. See `.claude/rules/multi-node-primary.md`. The routing, gossip, and transaction forwarding described in this section are only active when cluster mode is enabled.

This is the most architecturally significant section. Cyoda-Go supports multi-node deployment where any node can receive any request, with transactions pinned to their originating node.

### 4.1 Cluster Discovery

**Protocol:** SWIM gossip via HashiCorp `memberlist` (pure Go, embedded, no external infrastructure).

**Topics:** In addition to cluster membership, the gossip layer
carries application-level invalidation topics. The model-cache
decorator (`internal/cluster/modelcache`) publishes on
`model.invalidate` whenever a local mutation changes a
`(tenantID, ref)` binding — every peer evicts the matching cache
entry. The TTL lease (±10% jitter) is the fallback when gossip
drops a message.

**Encryption:** AES-GCM encrypted gossip keyed by the shared secret `CYODA_HMAC_SECRET`. The same secret is used for gossip encryption and transaction token signing; the documented and chart-generated form is 32 hex-decoded bytes, which selects AES-256.

**Node metadata** (JSON, serialized in memberlist node meta) is identity plus a list version:

```go
type nodeMeta struct {
    ID       string      `json:"id"`                 // stable, operator-assigned
    Addr     string      `json:"addr"`               // HTTP address (e.g., "http://node-1:8123")
    GRPCAddr string      `json:"grpcAddr,omitempty"` // gRPC address, when advertised
    Tags     listVersion `json:"tv"`                 // {epoch: process start, unix nanos; seq: change counter}
}
```

Its size depends only on operator settings. `NewGossip` measures it with the longest version there can be and refuses to start past memberlist's `MetaMaxSize` (512 bytes).

**Tag lists** (tenant → compute tags) do not ride in the metadata. Each node holds one list per peer. The version a node announces in its own metadata is the authority for which of its lists is current; versions are compared for equality and ordered only within one epoch, so a node restarted under the same id — with a clock that stepped backwards, even — is never taken for an older self. On a change a node bumps `seq`, re-advertises its metadata and sends `{nodeID, version, tags}` to every alive peer with `memberlist.SendReliable` (topic `cluster.tags`). A peer stores a list whose version equals the announced one, or is a later `seq` of the announced epoch. A peer that holds no list for a node, or another version than the announced one, sends `cluster.tags.request` and the node answers with its list: on `NotifyJoin`, on `NotifyUpdate`, and from a periodic scan of all members that is the floor under both. `NotifyLeave` drops the node's list. memberlist's event callbacks run under its node lock and are the only place a member can be read safely, so they do nothing but copy: the member (name, address, metadata) into the registry's own directory of alive members, and a nudge into a bounded queue. Everything else — `List`, `Lookup`, the fan-out, the scan — reads that directory and never `memberlist.Members()`, whose nodes memberlist rewrites under a lock no caller can take. One worker goroutine stores lists, and every send runs on a goroutine of its own. `NodeRegistry.Changed()` is closed when a list arrives with different tags, when a peer joins and when one leaves.

**Bootstrap algorithm:**

```
1. Filter self-address from seed list
2. Attempt list.Join(seeds) with exponential backoff:
     initial = 500ms, max = 10s, deadline = CYODA_STARTUP_TIMEOUT
3. After successful join, poll member count every 200ms
4. Block until member count is stable for StabilityWindow (default 2s)
5. Only then: mark node ready, open gRPC server
```

This handles simultaneous startup of all nodes. Memberlist is self-healing and merges transient split clusters before the stability window elapses.

In Kubernetes deployments, `BindAddr` is `0.0.0.0` (all interfaces)
while seeds are pod DNS names (e.g.
`cyoda-0.cyoda-headless.cyoda.svc.cluster.local:7946`). Because the
string-level comparison `0.0.0.0:7946 != <dns-name>:7946` never
matches, no pod filters itself out. This is intentional: the Helm
chart's ConfigMap emits every pod's DNS name in the seed list so
every pod has real peers (at least N-1 non-self) to join. At
`replicas=1`, the single pod's seed list effectively reduces to
itself and it proceeds as a cluster of one.

**Failure detection:** Automatic via SWIM protocol. Dead nodes are evicted from the membership list within seconds. No manual intervention required.

**Graceful leave:** `Deregister()` calls `list.Leave(5s)` then `list.Shutdown()`, giving peers time to update their membership views.

### 4.2 Transaction Routing

**Token structure (HMAC-SHA256 signed, base64url-encoded):**

```go
type Claims struct {
    NodeID    string `json:"n"`           // the owner: the node holding the transaction
    TxRef     string `json:"t"`           // UUID, key into the owner's local tx map
    ExpiresAt int64  `json:"e"`           // Unix seconds: the try's answer limit + CYODA_CALLOUT_PASS_ALLOWANCE
    Callout   string `json:"c"`           // the callout's request id
    Major     uint32 `json:"j"`           // raised by the owner before every try
    Minor     uint32 `json:"i,omitempty"` // counted by a node that received a hand-over; 0 on the owner's own tries
    Outer     []Pair `json:"o,omitempty"` // the (callout, major, minor) of every enclosing callout
}
```

Token format: `base64url(json_payload).base64url(hmac_sha256(json_payload, secret))`

`CYODA_HMAC_SECRET` is hex-encoded bytes by convention (the Helm
chart's generated secret is a 64-char hex string decoded to 32 raw
bytes); the binary's `envHexFromSecret` decodes hex if valid,
falling back to raw bytes otherwise. Transaction tokens use
base64url for both the JSON payload and the HMAC signature:
`base64url(json_payload).base64url(hmac_sha256(json_payload, secret))`.

Inter-node dispatch authentication uses AEAD (AES-256-GCM) over an
HKDF-SHA256-derived key (info string `"cyoda-dispatch-v1"`), which separates
the dispatch key from the raw gossip-encryption secret despite both being
derived from the same `CYODA_HMAC_SECRET`. Wire format is `[nonce(12) ||
ciphertext||tag]` with Content-Type `application/cyoda-dispatch-v1`, in both
directions. A request's associated data is the label `request`, the HTTP
method, the path, `X-Dispatch-Timestamp` and the node id the request is sealed
**for**; an answer's is the label `response`, the path, the timestamp, that same
recipient and the nonce of the request it answers, under a fresh nonce of its
own. That prevents cross-endpoint replay, reflection, an answer being moved onto
another request, and — the reason the recipient is bound in both — a captured
request being delivered to another node, which holds the same cluster key and
would otherwise accept it and seal an answer the owner could not tell from the
genuine one. The recipient is never on the wire: the sender names the node whose
address it looked up, the receiver names itself, and the envelope opens only
where the two agree. A bounded, TTL-evicted nonce cache rejects replayed
requests within the 30s skew window; answers do not enter it, being bound to a
request nonce their receiver chose. The scheduler's peer RPC signs its requests
and answers the same way. Both bodies are JSON encoded with HTML escaping off,
and the entity's payload travels base64 so that it is neither compacted nor
rewritten (§the internal dispatch endpoint).

What the seal binds is a request to one recipient, one endpoint, one timestamp
and one nonce, and an answer to one request. What it does not bind is a node's
lifetime: the replay cache is in memory, so a node restarted inside the
30-second skew window accepts a replay of a request its previous life ran. That
is accepted — binding the recipient's list epoch instead would fail every
hand-over to a peer whose epoch has not yet gossiped, turning a rare
attacker-dependent hole into a routine failure on every peer restart.

The token is a **pass**: it is minted per try, by the node that makes the
hand-off, and `NodeID` is always the owner's — so a callback is routed to the
node holding the transaction whichever node handed the work out. Which passes
are current is the fence's decision (§3.8).

The token is opaque to the client. The router decodes it to extract `nodeID` without any network call -- the address is a local lookup in the registry's own directory of alive members (§4.1).

**HTTP reverse proxy middleware (`proxy.HTTPRouting`):**

```
1. Extract X-Tx-Token header from request
2. If absent → serve locally (next handler)
3. Verify HMAC signature
4. If claims.NodeID == self → serve locally
5. If claims.NodeID != self → lookup address in gossip registry
6. If node alive and its address passes SSRF validation
     → httputil.ReverseProxy to target node
7. If node dead/unknown/address rejected → 503 TRANSACTION_NODE_UNAVAILABLE
```

The proxy is near-transparent: the target node receives the original request including the `X-Tx-Token`, with `Origin` and `Access-Control-Request-*` stripped so CORS is decided once, at the edge. Transport is a shared `http.Transport` with connection pooling (100 max idle, 10 per host, 90s idle timeout) and a response-header timeout of `CYODA_PROXY_TIMEOUT`.

**Token error handling (HTTP):**

| Error | HTTP Status | Code |
|-------|-------------|------|
| Token expired | 410 | `TRANSACTION_EXPIRED` |
| HMAC mismatch / invalid format | 401 | `UNAUTHORIZED` |
| Target node dead | 503 | `TRANSACTION_NODE_UNAVAILABLE` |
| Target node unreachable | 503 | `TRANSACTION_NODE_UNAVAILABLE` |

A pass that names no callout and no fencing number is invalid format, so `401 UNAUTHORIZED` covers it. Routing is only the first half of what a callback passes: once the request is on the owner, joining the transaction can still answer `404 TRANSACTION_NOT_FOUND`, `403 FORBIDDEN` for another tenant's transaction, or `410 CALLOUT_SUPERSEDED` for a pass the fence no longer holds current (§3.8).

gRPC applies the same mapping: `classifyRouteErr` in `internal/grpc/txroute_interceptor.go` yields `410 TRANSACTION_EXPIRED`, `401 UNAUTHORIZED` and `503 TRANSACTION_NODE_UNAVAILABLE` for the same conditions, rendered into the RPC's error envelope.

**gRPC routing:**

```go
// ExtractGRPCToken reads tx-token from gRPC incoming metadata.
func ExtractGRPCToken(ctx context.Context) string

// ResolveNodeInfo determines whether a request should be proxied, and to whom.
func ResolveNodeInfo(ctx, signer, registry, selfNodeID, tok) (contract.NodeInfo, bool, error)
```

gRPC routing forwards rather than redirecting. A `txRouteInterceptor` — unary and stream — extracts the token, resolves the owning node, and either joins the transaction locally or re-issues the call to that node over a pooled gRPC connection. For unary RPCs the peer's response is returned to the client as the interceptor's own; for server-streaming RPCs `proxyStream` copies every response frame back onto the inbound stream verbatim. The client is not told to retry elsewhere and does not learn which node served it.

**`COMMIT_BEFORE_DISPATCH` segment pinning.** A `COMMIT_BEFORE_DISPATCH` cascade pins **all segments to the home node** that opened `TX_pre`. `TX_post` is required to begin on the same node — this is enforced via the cluster's TX-token registry. Cross-node continuation is out of scope: a home-node crash mid-cascade leaves the entity durable in the pre-callout state and the in-flight orchestration lost (see §3.1); the client restarts with a fresh `Begin()` on a surviving node, which re-fires the cascade from the beginning.

**Response txID is the cascade-entry txID.** When a cascade is segmented by `COMMIT_BEFORE_DISPATCH`, the API response carries the txID that `Begin()` returned at cascade entry, **not** the txID that committed `TX_post` (the durable apply-result). This is the audit-correlation txID — `/audit/entity/{id}/workflow/{txId}/finished` looks up cascades by this entry txID. Implementation: `internal/domain/entity/service.go` returns `txID` (cascade-entry) regardless of how many segments the engine internally opened.

### 4.3 Compute Dispatch Routing

A **callout** is a call out to a compute member: an externalized processor, a
`function` criterion, or the `function` that times a scheduled transition. The
node that holds the operation's transaction — the owner — runs every callout, in
single-node and cluster mode alike. Six parts:

| Component | Type | Purpose |
|-----------|------|---------|
| Owner's loop | `callout.Coordinator`, implementing `contract.ExternalProcessingService` | Tries, hand-overs, waiting, the callout deadline. Built in both modes; its peer router is nil on a single node |
| Local procedure | `grpc.ProcessorDispatcher.RunLocal` | Tries a callout on this node's own matching compute members |
| Member selection | `grpc.MemberSelector` → `RoundRobinSelector` | Picks among the candidates not yet tried — never an evicted member (§6.3) |
| Peers | `dispatch.PeerRouter` | The alive peers advertising the tag, and the hand-over to one of them |
| Peer selection | `PeerSelector` → `RandomSelector` | The order in which peers are asked |
| Fencing | `fence.Fence` | Which passes are current (§3.8) |

**A try** is one attempt to hand a callout's work to one compute member. **The
hand-off** is `Member.Send` returning nil: before it the work provably never
left this node; after it the member may be working on it. Every failed try
carries a kind (`contract.CalloutFailureKind`), and the kind — together with
whether the callout is repeat-safe, which a criterion and a function are by rule
and a processor is only when its author declared it `idempotent` — decides
whether another compute member may be tried:

| Kind | What happened | Repeat-safe callout | Processor, `idempotent` false |
|---|---|---|---|
| `NoHandOff` | The member was gone, or not draining, before the hand-off — or there was no matching member to try at all, in which case no try is used | another member is tried | another member is tried |
| `NoAnswer` | Hand-off made, then no answer within the answer limit, or the stream dropped | another member is tried | **stop** |
| `MemberFailed` | The member answered `success: false` | stop | stop |
| `Terminal` | This node could not build the request, mint the try's pass, or read the answer | stop | stop |

**The owner's loop (`internal/callout`).**

```
tries        = 1 for retryPolicy NONE, else 1 + CYODA_RETRY_FIXED_NUM_RETRIES
answer limit = the callout's responseTimeoutMs, else CYODA_CALLOUT_RESPONSE_TIMEOUT_MS
deadline     = now + tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT
                   + CYODA_CALLOUT_HANDOVER_ALLOWANCE
one request id for the whole callout; fence.Begin, ended on every exit path
loop (one pass):
  take MemberRegistry.Changed() and NodeRegistry.Changed()  # before looking, so no signal is lost
  RunLocal(call, tries left): the node's own matching members, one after
    another, never the same one twice in a pass, chosen round robin. The
    fencing number rises before each try.
      answered → done
      a kind that forbids another member → stop
      tries used up → the attempts are reported
  with tries left, for each alive peer advertising the tag for the tenant,
  in peer-selector order, each at most once per pass:
      the fencing number rises; PeerRouter.HandOver(peer, call, tries left)
      could not connect, or the peer gave the work to nobody → no try used, next peer
      answered → done · a kind that forbids another member → stop
  nothing anywhere took the work → wait on either channel within the patience,
  then start a new pass
```

**The patience** is `CYODA_DISPATCH_WAIT_TIMEOUT`: one allowance for the whole
callout, counted in elapsed waiting time, `0` disables it. The wait is on the
two change channels — a compute member attaching or detaching here, a peer
joining or leaving or its tag list arriving — never a timer loop, so it costs
nothing while nothing changes and ends the moment something does. A pass that
made tries may still wait: a member that dropped and is coming back is the case
the patience exists for. A wait a change signal ends starts a new pass, in which
every peer may be asked again; a wait the patience ends does not, because
nothing changed and the same members would only be tried again. Waiting is not
a try.

**The deadline is the hard limit, the number of tries is not.** A hand-over
whose answer is lost counts as one try although the peer may have made more, so
the total can exceed the setting; the time cannot. The deadline is fixed when
the callout starts, no try or hand-over begins after it, and one in progress is
cut off at it. It is a context derived from the caller's with a cause of its own
(`context.WithDeadlineCause`, `contract.ErrCalloutDeadline`), which is how one
context tells the two apart: a try the callout's own deadline cuts off is
classified (`NoAnswer` after the hand-off, `NoHandOff` before it), while the
caller's own context ending — the client went away, or its
`transactionTimeoutMillis` fired — is returned unchanged. At the defaults the
limit is 4 × 30 s + 5 s + 30 s = 155 s; with the answer limit at its configured
upper bound, 275 s. Both sit inside PostgreSQL's idle-in-transaction ceiling
(§3.4).

**Fencing.** The callout's number rises before every try, which refuses the
earlier compute member's callbacks, and the fence then waits for a joined
request of that member still in progress. The full account — the number, the
pass, admission, the wait, and the join layer the checks sit in — is §3.8.

**The hand-over** is a POST to `http://peer/internal/dispatch/callout` under the
AES-256-GCM AEAD envelope of §4.2; the peer verifies the envelope, decrypts it,
runs its own local procedure and seals the answer for that one request. The peer
runs with the tries and the answer limit the owner sent, and never hands the
callout on.

Every hand-over opens its own connection (`DisableKeepAlives`), so that a node
that cannot be connected to is told apart from one that took the work and then
died. Opening the connection is bounded by `CYODA_DISPATCH_CONNECT_TIMEOUT`
(TCP connect and TLS handshake); the wait for the answer is a deadline the owner
puts on the request's context — tries left × answer limit +
`CYODA_CALLOUT_HANDOVER_ALLOWANCE`, never past the callout's deadline — and the
transport has no timeout of its own beyond the connect. Opening the connection
includes resolving a node address that is a hostname, so the name lookup the
SSRF guard makes runs under the connect timeout too, inside the hand-over's own
context.

No client that talks to another node uses a proxy or follows a redirect — the
hand-over transport, the scheduler's peer RPC, the HTTP reverse proxy and the
pooled gRPC client alike. Both would send a request to an address the peer
address guard never validated, which is the pivot that guard exists to close.
`CYODA_DISPATCH_FORWARD_TIMEOUT` bounds the scheduler's peer RPC only.

**How the owner reads an answer.** Only a decoded, authenticated answer whose
outcome is `no_handoff` with no try used means nothing reached a compute member;
so does a connection that could not be opened, a peer address that fails
SSRF validation, and a hand-over whose time was spent before anything was
written to a connection. Everything else that is not `ok`, `member_failed` or `terminal`
is read as a lost answer: a transport error after the connection opened, any
non-2xx status, a truncated or unauthenticated body, an outcome this version
cannot read, an `ok` missing the result it promises, a `triesUsed` outside
`0…triesLeft`. So is an `ok`, `member_failed` or `no_answer` that claims *no*
try was used — only a `no_handoff` and a `terminal` may say that truthfully. A
lost answer counts as **one** try — never zero, so the loop
always makes progress — and is recorded as an attempt with the member id `-`. A
peer that answers `no_handoff` having tried members costs the tries it made, and
the loop goes on to the next peer. A hand-over this node cannot build, marshal
or sign — or that does not fit the envelope — is `Terminal`: it would fail
identically for every peer, so the loop stops rather than trying the next one.

**Internal dispatch endpoint:**

```
POST /internal/dispatch/callout
```

- Single route for every callout kind (processor, criteria, function); `Kind` in
  the request body discriminates
- Authenticated and encrypted with the AES-256-GCM AEAD envelope described in §4.2 — the answer too
- Max envelope, on both legs: `base64(10 MB) + 512 KB` ≈ 13.8 MB — derived from
  the 10 MB an entity write may carry, so that an entity the API stores can
  always be handed over. The payload travels base64 (see below), the headroom
  covers the meta, the definition, the roles, the tags and the envelope's 28
  bytes, and the JSON is encoded with HTML escaping off so that a payload holding
  HTML or XML text is not multiplied sixfold. An answer carries one compute
  member's entity, which arrived over gRPC under that server's receive limit
  (grpc-go's 4 MB default — nothing here sets `MaxRecvMsgSize`), so it is well
  under the ceiling. An envelope above the ceiling is refused by whichever end
  builds it: a hand-over that cannot fit is `Terminal` and uses no try (it would
  fail identically on every peer), and an answer that cannot fit is not written,
  so the owner reads the lost answer it is
- The entity's payload travels **base64, byte for byte**, in both directions. It
  is what the store holds and what the compute member is handed, and both must be
  the same bytes: inside a JSON envelope a raw payload would be compacted (its
  insignificant whitespace dropped) and, with escaping on, rewritten character by
  character. A processor that changes nothing answers with the entity it was
  given and the owner persists that answer, so a rewrite on either leg would
  rewrite a tenant's stored data
- Reconstruct `UserContext` from request fields (tenantID, userID, roles, principal kind)
- A request carries two tenants — its own `TenantID`, which the reconstructed `UserContext` runs as, and `EntityMeta.TenantID`, which is handed to the local dispatcher as the entity's own. They must agree, or the callout would run as one tenant over another's entity; a mismatch is answered, under seal, as a `terminal` refusal with no try made — as is any authenticated request that cannot be run. The equality is unconditional and covers an absent `EntityMeta.TenantID`: every callout kind is built from a live stored entity whose `Meta.TenantID` is always set, so an empty one can only come from a hand-crafted peer body. The response names neither value — both are peer-supplied.
- Runs the local procedure (`RunLocal`) with the tries the owner allows, and never hands the callout on
- A request that does not authenticate is a bare `403`, and so is a replayed one; a request that authenticates but meets a full replay cache is answered, under seal, `no_handoff` with no try used, so that a saturated cache does not fail a callout that is not repeat-safe. Such a refusal records no nonce, so the node raises a "refuse at or before" watermark to the refused request's timestamp and answers every later request at or before it, whose nonce it does not hold, the same way — without it, the refused envelope is accepted as soon as the cache has room

**Dispatch request/response types** (`internal/cluster/dispatch/types.go`): the
request carries the entity payload and meta, the workflow/transition names,
the callout's txID, the caller's tenant/user/roles/principal, a request id the
owner mints and uses for every try, how many tries and how long an answer may
take, the owner's node id and the fencing number the peer's tries are numbered
under, the enclosing callouts (empty unless the callout was made from inside a
callback), whether the work may be given to a second compute member after a
hand-off, and one kind-specific member (`Processor`, `Criterion` + `Target` +
`ProcessorName`, or `Function`). The response carries the outcome (`ok`, `no_handoff`,
`no_answer`, `member_failed`, `terminal`), the tries used, one entry per failed
try, the kind-specific result (`EntityData` for a processor, `Matches` +
`Reason` for a criterion, `Result` + `ResultKind` for a function),
accumulated warnings and diagnostics, and — on failure — either the compute
member's own message and verdict (`member_failed`) or the peer's classified
error code, HTTP status and retryable flag so the owner re-mints the same
`AppError` the peer would have returned. Text a peer wrote — warnings, errors,
attempt causes, the member's own message — is bounded in count and length before
it is relayed, and an error code this build does not define is dropped rather
than minted.

**What the client sees:**

| Scenario | Behavior | Status, error code |
|----------|----------|------------|
| No matching member anywhere within the patience, no try made | Fail once the patience is spent | 503 `NO_COMPUTE_MEMBER_FOR_TAG` |
| Exactly one attempt on record | That attempt's own error, not wrapped | 503 `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED` or `DISPATCH_FORWARD_FAILED` |
| More than one attempt on record — tries used up, or the patience or the deadline spent with tries left | The attempts are listed: `… got N failures: [member<id>: cause], …`, identical entries collapsed | 503 `CALLOUT_FAILED` |
| No answer after the hand-off, processor not `idempotent` | Stop after that try | 503, the try's own code |
| A member answered `success: false` | Stop; its message reaches the client, retryable exactly when the member said so | 400 `WORKFLOW_FAILED` |
| A peer cannot be connected to, or gave the work to nobody | No try used; the next peer is asked | — |
| A hand-over's answer is lost | One try used, recorded as member `-`; the next peer only for a repeat-safe callout | 503 `DISPATCH_FORWARD_FAILED` when it is the only attempt |
| A callout's `responseTimeoutMs` exceeds `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` | `Terminal`, naming the setting; the limit is never silently clamped | 400 `WORKFLOW_FAILED` |
| The client's `transactionTimeoutMillis` fires, or the client goes away | The callout ends at once, from a wait as from a try | `TRANSACTION_TIMEOUT` (408) / none |
| A callback from a compute member that was replaced, or whose callout has ended, while the transaction is open | Refused; no store operation (§3.8) | 410 `CALLOUT_SUPERSEDED` |

Member ids appear in client-visible text: they identify the tenant's own
connections and are already given to the compute member in its greet. Node ids
and peer addresses never do.

### 4.4 Transaction Flow -- Complete Swimlane

Participants:

| Participant | Role | Holds |
|-------------|------|-------|
| Client | External caller (REST API) | HTTP connection |
| Node A | Receives primary event, owns transaction | `pgx.Tx` for tx-123, flow chain state |
| Compute | External processor (gRPC member) | Business logic, stateless |
| Node B | Receives callback CRUD from compute | Nothing -- proxies to tx owner |
| PostgreSQL | Source of truth | Transaction tx-123, all data |

**Happy path:**

```
t0   Client --> POST /entity create --> Node A
t1   Node A: BEGIN tx-123 --> PG: BEGIN REPEATABLE READ
t2   Node A: Save entity --> PG: INSERT entity (in tx-123)
t3   Node A: SM engine dispatches to processor
t3a  Node A: Coordinator begins the callout; RunLocal finds no local member
t3b  Node A: PeerRouter.Peers --> Node B advertises the tag for this tenant
t3c  Node A: raises the callout's fencing number; hands the callout over to
             Node B with the tries left
t3d  Node B: RunLocal picks a member, mints that try's pass
             {NodeID=A, TxRef=tx-123, callout, number}
t3e  Node B: sends the request on the member's gRPC stream (the hand-off)
t4   Compute: Receives CloudEvent with cyodatxtoken (signed tx-token referencing Node A / tx-123)
t5   Compute: Executes business logic
t6   Compute: CRUD callback; echoes cyodatxtoken as X-Tx-Token (HTTP) or tx-token (gRPC metadata)
              --> Node B receives request with X-Tx-Token header
t7   Node B: Decode txToken --> extract Node A from claims --> proxy to Node A
t8   Node A: Receive proxied CRUD, Join tx-123, admit the pass, take the
             transaction's lock --> PG: INSERT/UPDATE (in tx-123)
t9   Node A: release the lock, CRUD OK --> respond to Node B --> Node B forwards to Compute
t10  Compute: Receive CRUD OK --> finish logic
t11  Compute: Respond OK on the stream to Node B, which seals the answer for Node A
t12  Node A: SM complete, all processors finished
t13  Node A: COMMIT tx-123 --> PG: COMMIT
t14  Node A: 200 OK {entityId, transactionId} --> Client
```

Key observations:
- Node A is the single transaction owner throughout. All writes go through Node A.
- The node that makes the hand-off mints the pass — here Node B — naming Node A as owner, and embeds it in the CloudEvent (`cyodatxtoken`). The compute member echoes it on callbacks — `X-Tx-Token` header (HTTP) or `tx-token` metadata (gRPC). Without the echo the callback runs in a standalone transaction rather than joining T.
- Node A admits the callback at t8 only under the callout's current fencing number, and holds the transaction's lock for the whole of it (§3.8). Its response leaves after the lock is released, so the lock never waits on the compute member.
- Node B acts as a transparent proxy for CRUD callbacks (via `X-Tx-Token`) and as a local dispatch host for its compute members.
- Callback acks are provisional: writes are not durable until Node A commits T at t13.
- The compute member is stateless -- it receives entity data via CloudEvent payload and returns modified data the same way.
- The dispatch forward (t3e) and the CRUD proxy (t7) are distinct network paths that can fail independently.

**Variant: `COMMIT_BEFORE_DISPATCH` (segment boundary at the dispatch).**

When the dispatched processor's `executionMode` is `COMMIT_BEFORE_DISPATCH`, the engine splits the swimlane at the dispatch boundary into two transactions on the same home node:

```
t0   Client --> POST /entity create --> Node A
t1   Node A: BEGIN tx-123 (TX_pre), generate txToken --> PG: BEGIN REPEATABLE READ
t2   Node A: engine flushes pre-callout entity state --> PG: INSERT/UPDATE in TX_pre
     (audit: SMEventProcessingPaused recorded in TX_pre)
t3   Node A: COMMIT TX_pre --> PG: COMMIT  ◀── segment boundary; entity durable in pre-callout state
                                              ◀── connection released for the dispatch wait
t4   Node A: SM engine dispatches to processor (outside any transaction)
t4a  Node A: dispatch routing (local member or peer-forward as in the happy path)
t5   Compute: receives CloudEvent w/ tx-123 (no transactional CRUD if startNewTxOnDispatch=false)
t6   Compute: executes business logic, makes external side effects
t7   Compute: responds to Node A (dispatch return)
t8   Node A: BEGIN tx-456 (TX_post) on the **same node** as TX_pre
              --> PG: BEGIN REPEATABLE READ
t9   Node A: CompareAndSave (expected = txID from TX_pre) applies the processor result
              and runs any subsequent SYNC processors and cascade transitions inline in TX_post
              (audit: SMEventStateProcessResult recorded in TX_post)
t10  Node A: COMMIT TX_post --> PG: COMMIT
t11  Node A: 200 OK {entityId, transactionId: tx-123 /* cascade-entry txID */} --> Client
```

Segment-boundary observations:
- `TX_pre.Commit` releases the storage connection for the dispatch wall-clock window. Pool pressure for slow processors drops by `dispatch_duration / total_cascade_duration`.
- The entity is **publicly observable** in the pre-callout state between `t3` and `t10`. Other transactions' `Get`/`Search` see it; criteria-driven cascades elsewhere can fire on it. See [docs/CONSISTENCY.md](CONSISTENCY.md) §10 for the visibility caveat.
- CAS at `t9` expects the txID stamped at `t3`'s commit. A concurrent committer between `t3` and `t9` invalidates that expectation — the engine surfaces `ErrConflict` → `409 retryable`. Entity remains durable in the pre-callout state. No engine-side retry; no automatic compensation.
- `TX_post` must open on the same node as `TX_pre` (§4.2 segment pinning). Cross-node continuation is out of scope.
- The response txID at `t11` is `tx-123` (cascade-entry), not `tx-456` (the durable apply-result). Audit lookups use the entry txID (§4.2).
- Home-node crash between `t3` and `t10` (§3.1) leaves the entity durable in the pre-callout state with no engine-side reaper. Recovery is application-driven retry — see §10 of CONSISTENCY.md and the workflows help topic for the idempotency requirement.

### 4.5 Network Partition Analysis

**Network links:**

| Link | Label | Protocol |
|------|-------|----------|
| L1 | Client <-> Node A | HTTP (REST) |
| L2 | Node A <-> Compute | gRPC bidirectional stream |
| L3 | Compute <-> Node B | gRPC / HTTP (CRUD callback) |
| L4 | Node B <-> Node A | Internal proxy (HTTP) |
| L5 | Node A <-> PostgreSQL | TCP (pgx connection) |
| L6 | Node B <-> PostgreSQL | TCP (pgx connection). Carries no part of this cascade: Node B holds none of tx-123, and it resolves the proxy target from its own local member directory (§4.1), not from PostgreSQL |
| L7 | Node A <-> Node B | HTTP POST /internal/dispatch/callout (callout hand-over) |

---

#### Phase 1: t0--t3 (Entity create, SM dispatches to processor)

**L1 partitions (Client <-> Node A):**

Client's HTTP request times out. Node A may have already begun tx-123 and dispatched to processor.

- *Before t1:* Request never arrived. Clean.
- *After t1:* Node A has an open transaction, flow chain running. Client is gone. Node A eventually completes or times out the flow chain. Transaction commits or rolls back without the client ever knowing.

ISSUE: Client retries create a duplicate entity. **Requires idempotency keys.**

**L5 partitions (Node A <-> PG):**

Node A cannot write to PG. The INSERT at t2 fails. Node A detects error, aborts flow chain, rolls back.

If the partition is brief and the pgx TCP connection survives (keepalive has not fired): Node A may not notice until the next PG operation fails.

SAFE: PG operation fails -> Node A rolls back -> client gets error.

---

#### Phase 2: t4--t5 (Compute executing processor logic)

**L2 partitions (Node A <-> Compute):**

The hand-off was made, so the try is classified `NoAnswer`: the stream breaking, or the answer limit passing with no answer, both read the same way. For a repeat-safe callout — a criterion, a function, or a processor declared `idempotent` — the owner's loop gives the work to the next compute member with the tries left. Otherwise the callout ends there and the transaction rolls back.

Meanwhile: Compute may still be executing business logic, unaware the stream is dead. Its callbacks are refused from the moment the fencing number rises or the callout ends (§3.8) — `410 CALLOUT_SUPERSEDED` while tx-123 is open, `404 TRANSACTION_NOT_FOUND` once it has rolled back — and a callback already in progress finishes before the work is given to anyone else.

SAFE: either another member completes the callout, or Node A rolls back with the earlier member fenced. Compute's work inside cyoda is discarded; what it did outside cyoda is the application's to reconcile.

**L5 partitions (Node A <-> PG):**

Node A is waiting for compute response. PG connection may drop. Two sub-cases:

1. *pgx connection killed by PG:* tx-123 is rolled back server-side. When compute responds and Node A tries to use the tx, pgx returns error. Node A detects and aborts.
2. *pgx connection survives (brief partition):* No PG operations happening during this phase. Transaction still alive. If partition heals before t8, everything proceeds normally.

SAFE: Either PG kills the tx, or partition heals and flow continues.

---

#### Phase 3: t6--t9 (CRUD callback through Node B, proxied to Node A)

**L3 partitions (Compute <-> Node B):**

Compute's CRUD callback cannot reach Node B. Compute gets connection error and answers `success: false` on the dispatch stream, which Node B relays to Node A under seal (if L2 is still up). That is `MemberFailed`: no other compute member is tried, whatever the callout's retry policy, and the member's own message and verdict go to the client. Node A rolls back tx-123.

SAFE: Clean failure propagation up the chain.

**L4 partitions (Node B <-> Node A):**

The critical proxy link. Node B receives CRUD request with tx-123. Extracts Node A's ID from token claims. Tries to proxy to Node A. Cannot reach it.

Node B returns error to compute. Compute reports failure to Node A (via L2, if up). Node A rolls back.

If L2 is *also* down: Node A is waiting for compute response. Compute cannot reach Node B, cannot complete its work, but also cannot report back to Node A. Node A's dispatch call eventually times out (gRPC keepalive/deadline). Node A rolls back.

SAFE: Multiple failure modes, but all lead to rollback. May be slow (timeout-dependent).

**L5 partitions during t8 (Node A <-> PG):**

Node B proxied the CRUD to Node A. Node A tries to INSERT/UPDATE in tx-123. PG connection is dead. pgx returns error. Node A aborts the CRUD operation, responds error to Node B -> Node B -> Compute -> Node A (processor failure). Node A rolls back.

SAFE: PG error propagates back through entire chain.

**L1 partitions during Phase 3 (Client <-> Node A):**

Client's HTTP connection drops. But Node A's flow chain is autonomous at this point -- it does not need the client connection to complete. Flow chain may still commit successfully. Client never gets the response.

ISSUE: Same as Phase 1 -- client retries create duplicates. **Requires idempotency keys.**

---

#### Phase 4: t12--t14 (SM complete, commit, respond)

**L5 partitions at COMMIT (Node A <-> PG):**

The most dangerous moment. Node A sends COMMIT to PG. Three outcomes:

1. **COMMIT succeeds, ACK lost:** PG committed. Node A does not know. pgx returns error. Node A assumes failure, tells client error. But data IS committed.
2. **COMMIT never reaches PG:** PG never committed. Transaction is rolled back by PG once `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` elapses (§3.4). Node A tells client error. Correct.
3. **Partition before COMMIT sent:** Node A detects dead connection, rolls back locally, tells client error. Correct.

ISSUE: Case 1 is the classic **commit ambiguity**. Node A cannot distinguish cases 1 and 2. **Requires a commit marker/confirmation mechanism** (see [§12](#12-known-gaps)).

**L1 partitions at response (Node A <-> Client):**

Transaction committed successfully. HTTP response cannot reach client. Client retries, may create duplicate.

ISSUE: Committed but client does not know. **Idempotency key** would detect the retry and return the original result.

---

#### L7 partition analysis (dispatch forward: Node A <-> Node B)

**The connection cannot be opened:** no try is used. Node A asks the next peer advertising the tag, or waits out the patience and fails with `NO_COMPUTE_MEMBER_FOR_TAG`.

**The hand-over was sent and the answer never arrives, or arrives unreadable:** one try is counted. For a repeat-safe callout Node A carries on with the tries left; otherwise the operation fails with `DISPATCH_FORWARD_FAILED` and the transaction rolls back. Node B's compute member may still be working, and Node B's own local procedure may still be making further tries: those members' callbacks are refused from the moment Node A raises the fencing number or ends the callout (§3.8), and the result Node B eventually produces is discarded — nobody is listening.

SAFE: every case ends in a rollback, or in a further try of a callout declared safe to repeat, with the earlier compute member fenced. No data corruption is possible because the answer must reach Node A before the entity is updated.

---

#### Phase 5: `COMMIT_BEFORE_DISPATCH` segment-boundary partition

This phase covers the partition windows the segmented cascade described in §4.4 (variant) opens. The boundary sits between `TX_pre.Commit` (entity durable in pre-callout state) and `TX_post.Begin` (engine resumes after dispatch returns).

**L5 partitions (Node A <-> PG) between segments:**

`TX_pre` already committed. The dispatch is in-flight outside any transaction; PG holds no resources for this cascade. If L5 partitions during the dispatch window, the engine simply cannot open `TX_post` when the processor returns: `Begin()` fails. Node A surfaces `5xx`, the cascade halts, and the entity is durable in the pre-callout state.

ISSUE: The processor may already have produced external side effects (created a TeamCity build, charged a payment, sent a notification). The engine does not roll those back — it cannot, the segment boundary is durable. Recovery is application-driven retry on a fresh `Begin()`, with the processor expected to be idempotent or detect prior completion via an external resource ID. **Stranded entity in pre-callout state with persistent external side effects.** Mitigation: workflow-author idempotency design (`docs/CONSISTENCY.md` §10, `cmd/cyoda/help/content/workflows.md`).

**Home-node crash between `TX_pre.Commit` and `TX_post.Commit`:**

PG already committed `TX_pre` (the entity is durable in pre-callout state). The home node crashes. PG drops the connection used for `TX_pre`. The in-flight orchestration (the dispatch wait, the segment-pinning to that node) is lost. Subsequent client requests with the original token receive `503 TRANSACTION_NODE_UNAVAILABLE` from the cluster proxy.

ISSUE: Same shape as the L5 case above — entity durable in pre-callout state, external side effects may have fired, no engine-side reaper. Client must restart with a fresh `Begin()` on a surviving node, which re-fires the cascade from the beginning. Same idempotency requirement.

**L1 partitions (Client <-> Node A) between segments:**

`TX_pre` committed. Node A is dispatching. Client connection drops; Node A's cascade is autonomous and continues — `TX_post` opens, applies the result, commits. Cascade may complete fully durable while the client never sees the response.

Same as Phase 1/3: client retries create duplicates without idempotency keys. Additionally, here the retry restarts the cascade from the beginning, so the dispatched processor fires twice. The processor must be idempotent.

**L2 partitions (Node A <-> Compute) between segments:**

The dispatch is in-flight outside any TX. gRPC stream breaks — Node A's dispatch call returns error or times out. `TX_post` is never opened. Cascade halts; entity durable in pre-callout state.

Same shape as L5 + home-node-crash above: stranded entity, possible external side effects, application-driven retry with idempotency.

**Summary:** segment-boundary partitions never violate atomicity within a single segment, but they break **cascade atomicity** — earlier segments are durable, later ones are not. This is the mode's defining trade-off and is documented as a property of `COMMIT_BEFORE_DISPATCH` (`docs/CONSISTENCY.md` §4 transactional umbrella; `docs/CONCURRENCY.md` §6 cluster routing).

---

#### Findings Summary

| Category | Finding | Needed Mechanism |
|----------|---------|-----------------|
| **Consistency** | All partition scenarios lead to rollback or clean commit. No split-brain possible because `pgx.Tx` is single-owner. PG `REPEATABLE READ` + commit-time read-set validation (SI+FCW, see §3.7) catches conflicting concurrent writes. | None (inherently safe) |
| **Duplicate operations** | Client <-> Node A partition at any point can cause the client to retry, creating a second transaction for the same intent. Both may commit without conflicting. | Idempotency keys |
| **Commit ambiguity** | L5 partition at COMMIT time: Node A cannot tell if PG committed or not. | Commit marker (write marker row before COMMIT; check on reconnect) |
| **Timeout / liveness** | One try to a dead compute member is bounded by the callout's answer limit, and the callout as a whole by its deadline — tries × answer limit + the patience + the hand-over allowance, fixed when it starts and 155 s at the defaults (§4.3). Behind that sits `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`: the connection is idle inside its transaction for the whole callout, not running a statement, so the statement ceiling does not apply (§3.4). What is not bounded is the inbound request itself — no deadline is derived from it and propagated downstream. | Deadline propagation via context |
| **Resource exhaustion** | A transaction holds one PG connection for its lifetime; the idle-in-transaction ceiling caps that lifetime and a saturated pool fails fast with `503 STORAGE_UNAVAILABLE` after `CYODA_POSTGRES_ACQUIRE_TIMEOUT` rather than queueing (§3.4). | Covered by the DB-side ceilings |
| **Observability** | No cluster-wide view of open transactions, their owners, or their age. Per-node transaction counts and durations are exported as `cyoda.tx.active` / `cyoda.tx.duration` when OTel is enabled (§11); PostgreSQL's `pg_stat_activity` is the cross-node view. | Cluster-wide transaction registry |

### 4.6 Async Search Execution

**`AsyncSearchStore` SPI** (`cyoda-go-spi/search_store.go`):

```go
type AsyncSearchStore interface {
    CreateJob(ctx, job *SearchJob) error
    GetJob(ctx, jobID string) (*SearchJob, error)
    UpdateJobStatus(ctx, jobID string, epoch int64, status string,
        resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error
    SaveResults(ctx, jobID string, epoch int64, entityIDs iter.Seq[string]) error
    GetResultIDs(ctx, jobID string, offset, limit int) (entityIDs []string, total int, err error)
    DeleteJob(ctx, jobID string) error
    Cancel(ctx, jobID string, finishTime time.Time) error
    ReapExpired(ctx, ttl time.Duration) (int, error)
    Heartbeat(ctx, jobID string, epoch int64) error
    ClaimStale(ctx, staleAfter time.Duration, limit int) ([]*SearchJob, error)
    ClearResults(ctx, jobID string) error
}
```

`SearchJob` additionally carries `HeartbeatTime *time.Time` (the last
liveness stamp; `nil` means never stamped, in which case staleness is
measured from `CreateTime`) and `Epoch int64` (the claim/attempt counter —
`CreateJob` always persists `1`; `ClaimStale` increments it on every
successful claim).

**Streamed result save.** `SaveResults` streams entity IDs into the job's
persisted result set as the scan runs, rather than materializing the whole
result set in memory and saving it once at the end. Save order is preserved
as `GetResultIDs` page order. Implementations batch internally as they see
fit (sqlite chunks into short write transactions so its single-writer lock
is never held for the job's lifetime; postgres uses chunked `CopyFrom`), but
the result sequence position increases strictly across chunks. A `nil`
return from `SaveResults` means everything yielded was durably stored — it
is not a statement about job success, which is recorded separately via
`UpdateJobStatus` after the engine consults the producer's error state.

**Terminal statuses are write-once.** `SUCCESSFUL`, `FAILED`, and
`CANCELLED` refuse any further write — including `Heartbeat` and
`SaveResults` — with the sentinel `spi.ErrAlreadyTerminal`. `Cancel` is the
sole idempotent-nil exception. This closes a zombie-executor race: without
it, an executor that stalls past the stale bound, gets reaped by another
node's claim, and then recovers could overwrite a `FAILED` job with
`SUCCESSFUL`.

**Orphan handling: heartbeat, claim, and epoch fencing.** The owning node
heartbeats a running job on `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`, starting
the moment it is submitted (including while queued, not only while
scanning) — on a dedicated per-job ticker goroutine, independent of scan
progress, so a long non-yielding scan stretch can never starve the
heartbeat and let the reaper seize a healthy job. A background reaper
(`internal/domain/search.SearchService.ReclaimStaleJobs`) claims any
`RUNNING` job that is either stale (heartbeat silent for
`CYODA_SEARCH_JOB_STALE_AFTER`) or `released` via `ClaimStale` — which
atomically bumps the job's `Epoch` so concurrent claimers obtain disjoint
jobs — clears the prior epoch's partial results (`ClearResults`), and
re-executes the job on this node at the claimed epoch, as-at its
originally stored `PointInTime`, so a crashed node's async job completes
`SUCCESSFUL` on a live node rather than staying `RUNNING` forever or being
failed outright. Every executor-side write (`Heartbeat`, `SaveResults`, the
terminal `UpdateJobStatus`) carries the epoch the executor was started or
claimed with; a store refuses a write whose epoch does not match the job's
current epoch with `spi.ErrStaleClaim`, so a deposed executor that later
recovers cannot corrupt a result set another node has since taken over.

Re-execution is bounded: `SearchJob.StaleClaims` counts only staleness
claims (never a graceful `Release`), and once it reaches
`CYODA_SEARCH_JOB_MAX_ATTEMPTS` (default 3 — the initial run plus two
retries) the reaper fails the job instead of reclaiming it again, with the
persisted message `search abandoned: executor lost repeatedly`. The status
(`FAILED`) is contractual; the message text is not. A repeatedly
crash-looping executor therefore cannot re-execute a job forever, and a
healthy handoff chain (release, not staleness) never advances a job toward
that cap. The reclaim sweep runs on `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`'s
ticker — finer than the snapshot-TTL cadence — plus once at process
startup, so a node that restarts picks up anything left `released` or gone
stale before its first ticker fire, rather than waiting a full interval.
`ClaimStale` and `ReapExpired` are cross-tenant, called with a tenant-less
context (precedent: `ScheduledTaskStore.ScanDue`); the reaper's follow-up
writes reconstruct a per-job tenant context from the claimed job's own
`TenantID`. A claim the node cannot honour — no free capacity in its worker
pool, or a `ClearResults` failure — releases the job again (uncounted
against the attempt cap) rather than enqueuing over unknown partial
residue, so a claim never silently drops the job on the floor.

**Bounded worker pool.** `POST /api/search/async/{entityName}/{modelVersion}`
submits to a fixed-size worker pool (`internal/domain/search.WorkerPool`)
instead of spawning one goroutine per request. `CYODA_SEARCH_ASYNC_WORKERS`
(default 8) sizes the pool; `CYODA_SEARCH_ASYNC_QUEUE` (default 256) sizes
its submit queue. The default worker count is a documented number, not a
computed one — the engine cannot read the postgres plugin's own connection
budget through the SPI — sized so that 8 workers, each holding a scan
connection for the run's duration plus a save connection per chunk, stay
within the default 25-connection pool. Once both the running workers and
the queue are exhausted, submission fails fast with a retryable
**503 `SEARCH_QUEUE_FULL`** instead of queuing indefinitely.

**Per-tenant share.** The pool is per node and shared by all tenants, so a
single tenant submitting long scans could otherwise occupy every worker and
fill the queue, denying async search to every other tenant on that node.
`CYODA_SEARCH_ASYNC_MAX_PER_TENANT` (default 8, matching the worker count)
caps how many jobs one tenant may hold in flight — queued and running
together — and rejects the excess with the same retryable
**503 `SEARCH_QUEUE_FULL`**. `0` disables the cap. At the default a single
tenant can still saturate the running set, but its queue occupancy is bounded,
so the remaining queue capacity always belongs to other tenants.

**Cancellation and shutdown.** A jobID→`CancelFunc` registry in the engine
lets `CancelAsync` cancel in-process work and dispatch the store's
`Cancel(ctx, jobID, finishTime)` for cross-node visibility; the executor's
heartbeat/poll loop re-checks job status on every tick and cancels its own
context on an externally-recorded `CANCELLED`. Node shutdown (`App.Shutdown`)
drains the worker pool up to a bounded budget, giving in-flight jobs a
chance to finish normally, then calls `ReleaseRegisteredJobs` for whatever
is still registered: each is cancelled in-process and `Release`d
(epoch-fenced) in the store — **released for reclaim, not failed**. A peer,
or this same node on its own restart-time startup sweep, claims and
re-executes it promptly rather than waiting for the stale-heartbeat
timeout, so a graceful shutdown or rolling restart hands work off within
one `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` — a `Release` never counts
against `SearchJob.StaleClaims`, so a clean rolling restart chain never
pushes a job toward the attempt cap. A shutdown neither leaves a job stuck
`RUNNING` forever nor terminates it as `FAILED`.

**TTL-based cleanup.** Independent of the stale-job reaper above, a
background reaper goroutine runs on `CYODA_SEARCH_REAP_INTERVAL` (default
5m) and deletes *terminal* jobs older than `CYODA_SEARCH_SNAPSHOT_TTL`
(default 1h) — `DD-12`. The PostgreSQL implementation uses `CASCADE` on the
foreign key from `search_job_results` to `search_jobs`.

**PostgreSQL schema.** Two tables in
`plugins/postgres/migrations/`: `search_jobs` holds the job record (status,
model ref, condition, point-in-time, search options, result count, timings,
`epoch`, `heartbeat_time`, `released`, `stale_claims`) and
`search_job_results` holds the ordered entity IDs, keyed `(job_id, seq)`.
`released` marks a job handed back for reclaim by a graceful shutdown
(cleared on the next claim); `stale_claims` is the attempt-cap counter —
it is bumped by a genuine staleness claim and left untouched by a
`released` claim, so `ClaimStale` can tell a crash-recovery attempt from a
routine handoff. Both are tenant-scoped — `search_jobs` has the composite
primary key `(tenant_id, id)` and `search_job_results` carries `tenant_id`
with a composite foreign key back to it, `ON DELETE CASCADE` — and both
carry RLS policies enforcing tenant isolation.

**Design principles (DD-10, DD-11, DD-12):** results tables store entity
IDs only, never entity data (re-fetched from the entity store on read, so
it can never go stale between search and read); `pointInTime` is always
populated on `SearchJob`, defaulting to `time.Now()`, so repeated reads at
the same point in time are deterministic; TTL-based cleanup is implemented
uniformly by every plugin.

### 4.7 Paged Entity Listing and History Reads

**Paged listing.** `GET /entity/{entityName}/{modelVersion}` is served by
`spi.EntityStore.GetPage(ctx, modelRef, limit, offset, asAt)`, which pages
at the store instead of loading the whole model into Go and slicing —
`pageNumber`/`pageSize` map directly to `offset`/`limit`. Order is the
engine's own canonical entity-ID order: one total, stable, deterministic
order each backend uses consistently everywhere it orders by ID (`GetPage`,
the tie-break under a user-field `OrderBy`, and an explicit entity-ID
`OrderBy`) — but **not** guaranteed identical across engines (memory,
sqlite, postgres all order byte-wise ascending as their native behaviour;
see each plugin's "Canonical entity-ID order" section in `docs/plugins/`).
Reachable inside a joined transaction via compute-node callbacks: with
`asAt` nil, the page overlays the transaction's own write-set on top of the
committed view and every returned entity is unconditionally recorded into
the transaction's read-set (the page, not the model). With `asAt` set,
the read is committed-only, ignoring any ambient transaction. Postgres
backs this with the `idx_entities_model_entity_id` index (migration
`000008`, rebuilt by `000011` to cover every entity of a model rather than
only the live ones, `COLLATE "C"` for byte-wise order); sqlite adds a
`(tenant_id, model_name, model_version, entity_id)` index.

**History reads.** Two purpose-built reads replace the old
full-history-with-payloads `GetVersionHistory` (removed, pre-1.0, no shim):

- `GetVersionByTransaction(ctx, entityID, txID)` returns the *earliest*
  version an entity acquired under a given transaction — backing
  `GET /entity/{entityId}?transactionId=`. A DELETED tombstone never
  matches (it carries no entity payload); an empty `txID` never matches a
  stored-empty transaction ID and returns `ErrNotFound`.
- `GetVersionMetadata(ctx, entityID, opts)` returns metadata only (no
  entity payload) for one entity's version history, newest first, tied
  broken by version number descending — backing
  `GET /entity/{entityId}/changes` and the audit-event search's window.
  `opts.Limit == 0` means "all", a deliberate divergence from `GetPage`'s
  `limit >= 1` requirement: this read is bounded by one entity's own
  history, never a model-wide scan. `Deleted` is canonical (derived from
  change type), replacing a backend-divergent "is the entity payload nil"
  probe — and it is what `HasEntity` in the wire response derives from
  (`!Deleted`), so a tombstone's `hasEntity` reads uniformly across
  backends.

Both reads push their filtering into the store where possible: postgres and
sqlite match `GetVersionByTransaction` in SQL over the entity's own
versions; memory maintains a per-entity transaction index.

---

## 5. Workflow Engine

The workflow engine (`internal/domain/workflow/Engine`) implements a finite state machine (FSM) model for entity lifecycle management.

### 5.1 FSM Model

A `WorkflowDefinition` contains:
- **States:** Named states (e.g., `NEW`, `PROCESSING`, `DONE`), each carrying its outgoing transitions.
- **Transitions:** Named edges between states. `Manual: bool` (true means
  operator-initiated only), `Disabled: bool` (true removes the edge) and
  `Schedule: *TransitionSchedule` (non-nil means the edge fires on a timer, not
  on state entry). A transition is *automatic* when all three are absent —
  `!Manual && !Disabled && Schedule == nil` — and automatic transitions fire on
  state entry when criteria match. Each transition also carries:
  - `criteria`: Optional conditions (predicate or function) that must be satisfied.
  - `processors`: Ordered list of processors executed when the transition fires.
- **Initial state:** The starting state for new entities.
- **Criterion:** Optional workflow-level criterion for workflow selection.

### 5.2 Execution Modes

Entry points into the engine, each taking a `context.Context` first. The first three return an `*EngineResult`:

1. **`Execute(ctx, entity, transitionName)`** -- Entity creation. Selects matching workflow, sets initial state, optionally fires a named transition, cascades automated transitions.
2. **`ManualTransition(ctx, entity, transitionName)`** -- Fires a named transition on an existing entity, then cascades. `ManualTransitionWithIfMatch` adds an optimistic-concurrency precondition.
3. **`Loopback(ctx, entity)`** -- Re-evaluates automated transitions from the current state without firing a specific transition. Used when entity data is updated by a processor callback and the workflow should re-check conditions. `LoopbackWithIfMatch` is the precondition-carrying form.
4. **`FireScheduledTransition(ctx, task)`** -- Fires a scheduled transition when its timer comes due, driven by the scheduler. Returns a `ScheduledOutcome` rather than an `*EngineResult`, since the caller is the scheduler and not a request handler.

`GetAvailableTransitions` / `GetAvailableTransitionsForEntity` are read-only queries over the same model.

### 5.3 Cascade Logic

After any transition fires, the engine cascades: it scans the automatic transitions from the new state and fires the first whose criteria match, then repeats from the resulting state. This continues until no automatic transition matches or a safety limit is hit.

**Loop protection:**

- `maxStateVisits` (default 10, configurable via `CYODA_MAX_STATE_VISITS`): Per-state visit counter. If the entity visits the same state more than `maxStateVisits` times during a single engine invocation, the cascade stops.
- `maxCascadeDepth` (absolute limit: 100): Total cascade steps across all states. Prevents runaway chains.

### 5.4 Processor Execution

Processors are dispatched via the `ExternalProcessingService` SPI, implemented by the owner's loop, `callout.Coordinator` (see Section 4.3). Four execution modes are defined in the Cyoda model:

| Mode | Behavior |
|------|----------|
| `SYNC` | Processor executes within the current transaction. Entity data is updated in-place before the next transition. |
| `ASYNC_SAME_TX` | Executes inline in the caller's transaction, exactly as `SYNC` does. CRUD callbacks are routed back to the transaction owner. The `ASYNC` label is preserved for Cyoda Cloud configuration compatibility; execution in cyoda-go is not asynchronous. |
| `ASYNC_NEW_TX` | Processor executes sequentially within a SAVEPOINT of the parent transaction. Fire-and-forget error semantics: the processor's own failure rolls back the SAVEPOINT only, the parent pipeline continues, and nothing reaches the client — not even the member's `retryable` verdict. A savepoint that cannot be created, undone or released is **not** a processor failure: the transaction is unusable, so the operation fails with a ticketed 5xx (§3.8). Entity mutations returned by the processor are discarded. Parent rollback discards all ASYNC_NEW_TX work. The `ASYNC` label is preserved for Cyoda Cloud configuration compatibility — execution is sequential in cyoda-go. |
| `COMMIT_BEFORE_DISPATCH` | Engine splits the cascade into two transactions around this processor. `TX_pre` flushes the pre-callout entity state and commits **before** the processor is dispatched, releasing the storage connection during the external compute window. The processor runs outside any transaction. When the processor returns, the engine opens `TX_post` on the same node, reapplies the result via `CompareAndSave` (CAS expects the txID stamped at `TX_pre`'s commit), runs subsequent SYNC processors and cascade transitions inline, then commits. CAS conflict at the boundary surfaces `ErrConflict` → `409 retryable`; entity remains durable in the pre-callout state, no engine-side retry, no automatic compensation. Companion field `startNewTxOnDispatch: bool` (default `false`, sibling on the same processor object, validator rejects `true` for any other mode) controls whether a fresh transaction context is supplied to the dispatched call for processor-side CRUD on entities other than the cascade-anchor. **Audit-trail placement**: `SMEventProcessingPaused` is recorded in `TX_pre` and durably committed at the segment boundary; `SMEventStateProcessResult` is recorded in `TX_post`. The mode has no event types of its own. See [docs/CONSISTENCY.md](CONSISTENCY.md) §10 for visibility caveats and idempotency requirements. |

### 5.5 Audit Trail

The engine records state machine events to `StateMachineAuditStore` throughout execution. 18 event types:

| Event Type | Constant | Meaning |
|------------|----------|---------|
| `STATE_MACHINE_START` | `SMEventStarted` | Engine invocation begins |
| `STATE_MACHINE_FINISH` | `SMEventFinished` | Engine invocation completes |
| `CANCEL` | `SMEventCancelled` | Engine cancelled |
| `FORCE_SUCCESS` | `SMEventForcedSuccess` | Forced successful completion |
| `WORKFLOW_FOUND` | `SMEventWorkflowFound` | Matching workflow selected |
| `WORKFLOW_NOT_FOUND` | `SMEventWorkflowNotFound` | No workflow matches |
| `WORKFLOW_SKIP` | `SMEventWorkflowSkipped` | Workflow criterion not matched |
| `TRANSITION_MAKE` | `SMEventTransitionMade` | Transition fired |
| `TRANSITION_NOT_FOUND` | `SMEventTransitionNotFound` | Named transition not in workflow |
| `TRANSITION_NOT_MATCH_CRITERION` | `SMEventTransitionCriterionNoMatch` | Transition criterion failed |
| `TRANSITION_ABORTED` | `SMEventTransitionAborted` | Transition abandoned after a conflict |
| `PROCESS_NOT_MATCH_CRITERION` | `SMEventProcessCriterionNoMatch` | Processor criterion failed |
| `PAUSE_FOR_PROCESSING` | `SMEventProcessingPaused` | Waiting for a dispatched processor |
| `STATE_PROCESS_RESULT` | `SMEventStateProcessResult` | Processor result received |
| `SCHEDULED_TRANSITION_ARM` | `SMEventScheduledTransitionArmed` | Scheduled transition armed on state entry |
| `SCHEDULED_TRANSITION_FIRE` | `SMEventScheduledTransitionFired` | Scheduled transition fired at its due time |
| `SCHEDULED_TRANSITION_EXPIRE` | `SMEventScheduledTransitionExpired` | Scheduled transition passed its expiry unfired |
| `SCHEDULED_TRANSITION_CANCEL` | `SMEventScheduledTransitionCancelled` | Scheduled transition cancelled before firing |

**Segment-boundary placement for `COMMIT_BEFORE_DISPATCH`** (§5.4): when the engine segments a cascade around a `COMMIT_BEFORE_DISPATCH` processor, `SMEventProcessingPaused` is recorded in `TX_pre` (and durably committed at the segment boundary, surviving an engine crash before the dispatch returns) and `SMEventStateProcessResult` is recorded in `TX_post`. **No event spans both transactions, and the mode has no event types of its own.** Audit consumers can detect a stranded mid-cascade entity by the presence of `SMEventProcessingPaused` without a matching `SMEventStateProcessResult` for the same dispatch.

---

## 6. gRPC & Externalized Processing

### 6.1 CloudEventsService

The gRPC service is defined in `proto/cyoda/cyoda-cloud-api.proto`
and exposes six RPCs — one bidirectional stream, three unary, and two
server-streaming — all carrying `io.cloudevents.v1.CloudEvent` payloads:

```protobuf
service CloudEventsService {
    rpc startStreaming(stream io.cloudevents.v1.CloudEvent) returns (stream io.cloudevents.v1.CloudEvent);
    rpc entityModelManage(io.cloudevents.v1.CloudEvent) returns (io.cloudevents.v1.CloudEvent);
    rpc entityManage(io.cloudevents.v1.CloudEvent) returns (io.cloudevents.v1.CloudEvent);
    rpc entityManageCollection(io.cloudevents.v1.CloudEvent) returns (stream io.cloudevents.v1.CloudEvent);
    rpc entitySearch(io.cloudevents.v1.CloudEvent) returns (io.cloudevents.v1.CloudEvent);
    rpc entitySearchCollection(io.cloudevents.v1.CloudEvent) returns (stream io.cloudevents.v1.CloudEvent);
}
```

`startStreaming` is the primary RPC — a bidirectional stream used for
the full calculation-member lifecycle (join, greet, keep-alive, dispatch,
response, leave). The unary and server-streaming RPCs carry the entity
management, model management, and search CloudEvent types enumerated in
§6.5.

### 6.2 Member Lifecycle

```
join --> greet --> keep-alive --> dispatch/response --> leave
```

1. **Join:** Client sends `CalculationMemberJoinEvent` as first message. Server registers member in `MemberRegistry`, extracts tags and tenant from payload. Returns `CalculationMemberGreetEvent` with assigned member ID.

2. **Keep-alive:** Server sends `CalculationMemberKeepAliveEvent` at configurable interval (default 10s). A processor response, criteria response, function response, `EventAckResponse`, or keep-alive echo all count as activity; if none is seen within the timeout (default 30s), or one outbound write stalls that long, the server evicts the member. The same interval/timeout also drive grpc-go's transport keepalive (HTTP/2 PING and ack deadline), which catches a peer whose TCP is alive but whose process is gone.

3. **Dispatch/Response:** Server sends `EntityProcessorCalculationRequest` or `EntityCriteriaCalculationRequest`. Client processes and returns the corresponding `Response` type. Correlation is by `requestID` field in the CloudEvent payload.

4. **Leave:** Stream closes (client disconnect or server eviction). `MemberRegistry.Unregister` drops the member and evicts it, which refuses new tracked requests and fails every pending one; the registry's change signal fires, waking any callout waiting for a member (§4.3).

### 6.3 Tag-Based Member Selection

`MemberRegistry.Candidates(tenantID, tagsCSV)` lists the tenant's members whose tags overlap the required tags (CSV comparison; every member of the tenant when `tagsCSV` is empty), ordered by `(ConnectedAt, ID)`. A `MemberSelector` picks one of them. `RoundRobinSelector` picks the member picked longest ago and stamps it from one counter on the registry; the stamp is a field on `Member`, so there is no per-tag state, and a member that has just attached goes first. A member that has been evicted is not a candidate either, even before its registration is removed, and the tag lists the node publishes to its peers skip it too.

### 6.4 Response Correlation

A callout gets one `requestID` (TimeUUID), minted by the owner and sent on **every** try of that callout — so a compute member that already holds the request can recognise a repeat by it, and it is the key an application uses to make an outside effect safe to repeat. The CloudEvent envelope's own id stays unique per event. For each try the local procedure:

1. Creates a buffered channel: `member.TrackRequest(requestID) -> chan *ProcessingResponse`
2. Sends the CloudEvent to the member's stream
3. Waits on the channel until the callout's answer limit passes

When the member responds, the streaming handler matches the response's `requestID` to the pending channel and delivers the result. Correlation is **per member**, so two tries of one callout in flight on different members cannot be confused; a try that ends without its answer abandons its tracking entry, so a late reply finds nothing and is discarded. If the member disconnects, `Member.Evict` fails every pending request with `Disconnected`, and the try is classified `NoAnswer` (`NoHandOff` if the member was already gone when the request was tracked). One deadline — the answer limit — bounds the enqueue and the wait together, so a member that is attached but not draining costs up to one answer limit before the try is classified.

### 6.5 CloudEvent Types

**Streaming/calculation:** `CalculationMemberJoinEvent`, `CalculationMemberGreetEvent`, `CalculationMemberKeepAliveEvent`, `EntityProcessorCalculationRequest/Response`, `EntityCriteriaCalculationRequest/Response`, `EntityFunctionCalculationRequest/Response`, `EventAckResponse`

**Entity management:** `EntityCreateRequest`, `EntityCreateCollectionRequest`, `EntityUpdateRequest`, `EntityUpdateCollectionRequest`, `EntityPatchRequest`, `EntityTransactionResponse`, `EntityDeleteRequest/Response`, `EntityDeleteAllRequest/Response`, `EntityTransitionRequest/Response`

**Model management:** `EntityModelImportRequest/Response`, `EntityModelExportRequest/Response`, `EntityModelTransitionRequest/Response`, `EntityModelDeleteRequest/Response`, `EntityModelGetAllRequest/Response`, `EntityModelSetUniqueKeysRequest/Response`

**Search/query:** `EntityGetRequest`, `EntityGetAllRequest`, `EntitySnapshotSearchRequest/Response`, `EntityResponse`, `EntitySearchRequest`, `EntityStatsGetRequest/EntityStatsResponse`, `EntityStatsByStateGetRequest/EntityStatsByStateResponse`, `EntityChangesMetadataGetRequest/EntityChangesMetadataResponse`

**Snapshot lifecycle (no dedicated response CloudEvent type):**
`SnapshotCancelRequest`, `SnapshotGetRequest`, `SnapshotGetStatusRequest`
are one-way request events; replies are carried on the generic
`EntityResponse` / `EventAckResponse` envelopes rather than dedicated
`*Response` types. See `internal/grpc/cloudevent_types.go`.

---

## 7. Authentication & Authorization

Two modes, selected via `CYODA_IAM_MODE`:

### 7.1 Mock Mode (default)

`mockiam.NewAuthenticationService(defaultUser)` -- returns a fixed `UserContext` for every request. Used for development and testing.

Default mock user: `mock-user-001`, tenant `mock-tenant`, roles `[ROLE_ADMIN, ROLE_M2M]` (override via `CYODA_IAM_MOCK_ROLES`). The defaults grant admin HTTP access and gRPC streaming (which requires `ROLE_M2M`).

### 7.2 JWT Mode

Full RS256 JWT authentication with JWKS discovery and M2M client support.

**Components:**

| Component | Purpose |
|-----------|---------|
| `AuthService` | Wires all auth components, exposes HTTP handlers |
| `InMemoryKeyStore` | Manages RSA key pairs (active signing key + rotated keys) |
| `TrustedKeyStore` | Interface for trusted external public keys (in-memory, or KV-backed over a per-node cache) |
| `InMemoryM2MClientStore` | Machine-to-machine client credentials |
| `JWKSHandler` | `GET /.well-known/jwks.json` -- standard JWKS endpoint |
| `NewTokenHandler` | `POST /oauth/token` -- issues JWTs (client_credentials, OBO exchange) |
| `JWKSValidator` | Validates JWTs against a `KeySource`: `NewLocalKeySource` in-process by default (no HTTP fetch), or `NewHTTPJWKSSource` (TLS 1.3 pinned, JSON content-type validated) for external-IdP wiring |
| `DelegatingAuthenticator` | Implements `contract.AuthenticationService`, delegates to validator |

`KVTrustedKeyStore` keeps a per-node cache over the KV store and converges across the cluster on three layers: mutations publish a payload-free change ping on the `auth.trustedkeys` gossip topic (receivers re-read KV, coalescing concurrent pings); a periodic reconcile loop (`CYODA_AUTH_CACHE_RECONCILE_INTERVAL`, default 60s, jittered ±10%) rebuilds the cache from KV as the backstop when a ping drops; and `Get` reads through to KV on a cache miss. If reconciliation has not succeeded for 10× the interval — KV unavailable that long means the platform is effectively down — the verification lookup (`GetForVerification`, which reads the cache only) fails closed (trusted-key-signed grants are rejected) while `Get` switches to KV read-through, keeping admin operations on ground truth. First-party JWT validation is unaffected (in-process key source, no KV dependency).

**Deterministic KID derivation:**

```go
pubDER, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
kidHash := sha256.Sum256(pubDER)
kid := hex.EncodeToString(kidHash[:16])  // first 16 bytes of SHA-256
```

This is critical for multi-node clusters: all nodes sharing the same RSA private key produce the same KID. Any node can validate tokens issued by any other node without key synchronization.

**OBO (On-Behalf-Of) exchange:** An M2M client presents, with its own credentials, a subject token signed by a trusted key registered in the client's own tenant (grant `urn:ietf:params:oauth:grant-type:token-exchange`); a key registered by another tenant is not found. A key's tenant is the tenant that registered it, never a claim in the token it signs. The subject's `caas_org_id` must equal the client's tenant, and its `sub` must pass the user-identifier rule (§1). The issued token carries the subject's `sub` as its user id, the subject's roles, and an `act` claim naming the client, so calls made with it are attributed to that user.

**Bootstrap M2M client:** Bootstrap M2M client creation is opt-in. In
`jwt` mode, `CYODA_BOOTSTRAP_CLIENT_ID` and
`CYODA_BOOTSTRAP_CLIENT_SECRET` must be set together (both present) or
both left empty. Half-configured states are rejected at startup with an
error naming the missing variable (see
`app/app.go:validateBootstrapConfig`). When set, the bootstrap M2M
client is created at startup and can be used to mint access tokens. In
`mock` mode, both variables are ignored. The Helm chart provisions the
secret via a chart-managed Kubernetes Secret with a GitOps-safety guard.

### 7.3 OIDC Provider Registry

When `CYODA_IAM_MODE=jwt` is active, tenants can register external Identity Providers (IdPs) that issue JWTs which cyoda-go should accept alongside its own locally-issued tokens. Each provider record is stored in the KV store under a single namespace (`oidc-providers`) with composite keys of the form `<tenantID>:<providerID>`, giving per-tenant isolation without a separate table.

The `<tenantID>` half is a **canonical lowercase UUID**, and the adapter — the single entry point to the OIDC service — requires the caller's tenant to already be spelled that way rather than normalising it. `uuid.Parse` accepts forms `uuid.UUID.String()` folds away, and folding them here would alias tenants that every other subsystem keys apart by raw text, letting one reach another's providers. A tenant that is not a canonically-spelled UUID has no key it may address and gets `400 OIDC_INVALID_TENANT` from every provider operation, including the list one; `POST /oauth/oidc/providers/reload` takes no tenant and is exempt.

**Chained multi-issuer validation.** The `DelegatingAuthenticator` from §7.2 is the outer shell; inside it the request's `iss` claim determines which validator handles the token:

1. **`JWKSValidator` (first)** — checks locally-issued tokens whose issuer matches `CYODA_JWT_ISSUER`.
2. **`OIDCValidator` (second)** — if the `JWKSValidator` rejects the issuer, the authenticator looks up a registered OIDC provider whose `issuers` list contains the token's `iss`. On a match it fetches the provider's JWKS (sourced from the discovery document at `<providerURL>/.well-known/openid-configuration`), validates the signature and standard claims, then maps the token's roles claim to cyoda roles. If no provider matches, the token is rejected as unauthorized.

**Per-provider configuration** (stored per-record, not global):

| Field | Purpose |
|-------|---------|
| `issuers` | Whitelist of accepted `iss` values from this IdP |
| `expectedAudiences` | Audience values the token must carry (`aud` claim) |
| `rolesClaim` | JWT claim name to extract roles from (overrides `CYODA_OIDC_ROLES_CLAIM` per-provider) |

**JWKS caching and cache eviction.** Each node caches the JWKS response for a provider. When a provider record is updated, deleted, or reloaded via the REST API, the owning node evicts its local cache entry and broadcasts on the `oidc.providers` topic via `spi.ClusterBroadcaster`; peers that receive it evict their copy. The broadcast is best-effort and fire-and-forget; behind it, the same reconcile backstop as the trusted-key cache (`CYODA_AUTH_CACHE_RECONCILE_INTERVAL`, default 60s, jittered ±10%) periodically rebuilds the provider map from KV, so a dropped message costs at most one interval of staleness rather than persisting until an explicit reload. Warm JWKS sources are carried over on reconcile — the backstop never causes IdP re-fetch traffic; key freshness stays governed by the per-source JWKS cache TTL. If reconciliation has not succeeded for 10× the interval, `ResolveKey` fails closed and OIDC-issued tokens are rejected with the uniform 401 until a reconcile succeeds. A provider whose JWKS URL is unreachable at validation time is treated as an auth failure, not a 5xx.

**REST API.** Seven endpoints under `/oauth/oidc/providers` implement the full lifecycle: register, list, update, invalidate (suspend without delete), reactivate, delete, and reload-cache. These endpoints require `ROLE_ADMIN` and are documented in the OpenAPI spec.

**Security controls.** The JWKS fetch URL is validated at registration time against SSRF rules: HTTPS is required by default (`CYODA_OIDC_REQUIRE_HTTPS`), and private/loopback/link-local network ranges are blocked by default (`CYODA_OIDC_ALLOW_PRIVATE_NETWORKS`). Violations surface as `400 OIDC_SSRF_BLOCKED`. See §9 for the six `CYODA_OIDC_*` env vars.

**Design rationale.** See [docs/adr/0002-federated-identity-provider-architecture.md](adr/0002-federated-identity-provider-architecture.md) for the full decision record including alternatives considered for storage layout, chaining order, and cache-eviction strategy.

### 7.4 Authorization

Currently `mockiam.NewAuthorizationService()` -- a permissive stub. The gRPC streaming endpoint enforces `ROLE_M2M` for calculation members.

### 7.5 Admin listener authentication

The admin listener (`/livez`, `/readyz`, `/metrics` on
`CYODA_ADMIN_PORT`, default `9091`) is served separately from the
main API listener and has its own authentication policy. This is
where the deployment probes live — `GET /health` on the main API
listener (§3.4) mirrors the same readiness flag as `/readyz` but is
a plain summary for humans and simple scripts, not the orchestrator
contract:

- **`/livez` and `/readyz`** are always unauthenticated. Kubelet
  probes carry no bearer token; authenticating these endpoints
  would break the standard readiness contract.
- **`/metrics`** is optionally bearer-gated and always exposes
  application metrics — OIDC subsystem metrics (`oidc_*`) when IAM
  runs in `jwt` mode, and transaction/dispatch metrics when
  `CYODA_OTEL_ENABLED=true` — in addition to Go runtime/process
  metrics. When `CYODA_METRICS_BEARER` (or
  `CYODA_METRICS_BEARER_FILE`) is non-empty, a request must carry
  `Authorization: Bearer <token>` and the token must match
  (constant-time compare) or the request receives `401 Unauthorized`.
- **`CYODA_METRICS_REQUIRE_AUTH=true`** is a coupled-predicate
  safety: if set true but `CYODA_METRICS_BEARER` is empty, startup
  fails with a fatal error naming the missing variable. Protects
  against "I thought I turned auth on" misconfiguration.

The canonical Helm chart binds the admin listener to `0.0.0.0`
(kubelet probes and Prometheus scraping reach the pod-facing
interface) and sets `CYODA_METRICS_REQUIRE_AUTH=true` with a
chart-managed bearer secret projected into the pod via a
projected-volume `_FILE` mount. Defense in depth: bind-address +
bearer + NetworkPolicy restricting :9091 ingress to the monitoring
namespace.

---

## 8. Error Model

### 8.1 Three-Tier Classification

```go
type ErrorLevel int
const (
    LevelOperational ErrorLevel = iota  // 4xx client errors
    LevelInternal                       // 500 unexpected errors
    LevelFatal                          // unrecoverable
)
```

| Tier | HTTP Status | Client Detail | Logging |
|------|-------------|---------------|---------|
| Operational | 4xx | Full domain error code + message | INFO |
| Internal | 500 | Generic message + ticket UUID | ERROR with ticket + full detail |
| Fatal | 500 | Generic message + ticket UUID | ERROR "FATAL" with ticket + full detail |

Internal and Fatal are indistinguishable to the client — both yield a generic 500 and a ticket. They differ in the log line. The HTTP panic-recovery middleware mints a Fatal `AppError`; the gRPC interceptors mark the node unhealthy the same way but return `codes.Internal` with a ticket-bearing message directly, without going through `AppError` (§3.4).

### 8.2 RFC 9457 Problem Details

All errors are returned as `application/problem+json`:

```go
type ProblemDetail struct {
    Type     string         `json:"type"`
    Title    string         `json:"title"`
    Status   int            `json:"status"`
    Detail   string         `json:"detail,omitempty"`
    Instance string         `json:"instance"`
    Ticket   string         `json:"ticket,omitempty"`
    Props    map[string]any `json:"properties,omitempty"`
}
```

Props always include `errorCode`. The optional `retryable` boolean is set to `true` only on transient conflicts that may succeed on a fresh attempt as-is — typically storage-level transaction serialization aborts (40001/40P01) and cluster-availability conditions. Permanent business-logic conflicts (locked-state mismatches, ETag/If-Match preconditions, cardinality precondition failures) are non-retryable: replaying the same request without an external state change cannot succeed.

In `verbose` mode (`CYODA_ERROR_RESPONSE_MODE=verbose`), internal error details are included in responses. In `sanitized` mode (default), only the ticket UUID is exposed.

### 8.3 Error Code Taxonomy

Codes are grouped by surface area:

- **Domain** — model lifecycle, entity CRUD, workflow, validation, generic 4xx (`BAD_REQUEST`, `UNAUTHORIZED`, `FORBIDDEN`, `SERVER_ERROR`, `NOT_IMPLEMENTED`), and storage availability (`STORAGE_UNAVAILABLE`).
- **IAM** — key pairs, trusted keys, M2M clients, unsupported algorithms and key types.
- **Cluster / transaction** — distributed-transaction hand-off (`TRANSACTION_*`), transaction-token handling (`TX_*`), gossip membership, idempotency.
- **Compute dispatch** — externalized processor / criteria / function invocation across cluster members.
- **Search** — async search-job lifecycle and scan-budget limits.
- **Composite unique keys** — uniqueness violations and unique-key definition errors.
- **Scheduled transitions** — Function-callout result validation.
- **OIDC provider registry** — provider lifecycle and SSRF rejection.
- **Help subsystem** — topic lookup.

The authoritative code list is `internal/common/error_codes.go`. Per-code semantics, HTTP status, retryable hint, structured `properties`, and remediation guidance live in the help subsystem at `cmd/cyoda/help/content/errors/<CODE>.md`, rendered via `cyoda help errors` (catalogue) and `cyoda help errors <CODE>` (per-code page). The `TestErrCode_Parity` gate in `cmd/cyoda/help` enforces that every constant in `error_codes.go` has a corresponding help topic.

Programmatic clients key on `errorCode`, not HTTP status: multiple codes may share the same status, and the code expresses the failure mode the dictionary preserves. New failure modes get a specific code rather than overloading a generic one (e.g. the model-lifecycle preconditions surface as `MODEL_ALREADY_LOCKED` / `MODEL_ALREADY_UNLOCKED` / `MODEL_HAS_ENTITIES`, not generic `CONFLICT`).

### 8.4 Warning/Error Accumulation

```go
common.AddWarning(ctx, "message")
common.AddError(ctx, "message")
```

Warnings and errors are accumulated in the request context and propagated to the caller. Processor/criteria response warnings are prefixed with the processor/criteria name and added to the context. Surfaced in gRPC `warnings` array and HTTP response body.

---

## 9. Configuration Reference

All values configurable via environment variables with the `CYODA_` prefix. Plugin-specific variables use the plugin's name as a secondary namespace (`CYODA_POSTGRES_*`, `CYODA_SQLITE_*`). Plugin-scoped variables are documented in the per-plugin reference under `docs/plugins/`. `./cyoda --help` on any binary renders the variables for the plugins it ships with — the help text is generated at runtime from the registered plugins' `ConfigVars()`.

The tables below cover the variables an operator sets to shape the architecture described in this document. `cyoda help config all` is the exhaustive, version-matched list; `cyoda help config <topic>` narrows it to one area.

### Credential loading (`_FILE` suffix)

Every credential-shaped environment variable accepts a `_FILE`
variant that reads the value from a file path. Precedence: `_FILE`
wins if both `<NAME>` and `<NAME>_FILE` are set. Trailing
whitespace (spaces, tabs, CR, LF) is stripped from file contents,
so multi-line PEM keys and DSN strings both round-trip cleanly. If
`<NAME>_FILE` is set to a path that cannot be read, the binary
fails at startup with the path and error.

Applies to: `CYODA_POSTGRES_URL`, `CYODA_JWT_SIGNING_KEY`,
`CYODA_HMAC_SECRET`, `CYODA_BOOTSTRAP_CLIENT_SECRET`,
`CYODA_METRICS_BEARER`. Plugin-scoped credentials are documented in
the per-plugin reference.

This is the canonical Docker / Kubernetes pattern for wiring
credentials from Secrets into the process without exposing them in
`env` output. Reference implementation: `app/config_secret_env.go`
`ResolveSecretEnv`.

### Profiles

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_PROFILES` | (none) | Comma-separated list of profile names; loads `.env` then `.env.<profile>` in declaration order. Shell environment always wins over file values. Example: `CYODA_PROFILES=postgres,otel`. |

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_HTTP_PORT` | `8080` | HTTP server listen port |
| `CYODA_HTTP_READ_HEADER_TIMEOUT` | `10s` | Time allowed to receive a request's headers on the API and admin servers. 0 falls back to `CYODA_HTTP_READ_TIMEOUT`. |
| `CYODA_HTTP_READ_TIMEOUT` | `5m` | Time allowed to receive a whole request, body included. Does not limit handler execution. 0 disables. |
| `CYODA_HTTP_WRITE_TIMEOUT` | `0s` | Time from the end of the request headers to the end of the response. Limits handler execution, so it ships disabled; set only if you want the server to cut off long-running requests. |
| `CYODA_HTTP_IDLE_TIMEOUT` | `2m` | How long an idle keep-alive connection is held open between requests. 0 falls back to `CYODA_HTTP_READ_TIMEOUT`. |
| `CYODA_CONTEXT_PATH` | `/api` | URL prefix for all API routes |
| `CYODA_ERROR_RESPONSE_MODE` | `sanitized` | `sanitized` or `verbose` (dev only) |
| `CYODA_MAX_STATE_VISITS` | `10` | Per-state visit limit for cascade loop protection |
| `CYODA_LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `CYODA_STARTUP_TIMEOUT` | `30s` | Deadline for binary startup (plugin factory init, migrations, cluster join). Fatal on expiry. |
| `CYODA_SUPPRESS_BANNER` | `false` | Suppress the ASCII banner at startup (useful for structured-logging environments). |

### Admin & metrics

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_ADMIN_PORT` | `9091` | Admin listener port (`/livez`, `/readyz`, `/metrics`). |
| `CYODA_ADMIN_BIND_ADDRESS` | `127.0.0.1` | Admin listener bind address. Helm chart sets `0.0.0.0` so kubelet probes and Prometheus can reach the pod. |
| `CYODA_METRICS_REQUIRE_AUTH` | `false` | Coupled predicate: if `true` and `CYODA_METRICS_BEARER` is empty, startup fails. |
| `CYODA_METRICS_BEARER` (with `_FILE` variant) | (none) | Bearer token required on `/metrics` when non-empty. Constant-time compare. |

See §7.5 for the authentication policy on admin endpoints.

### Observability

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_OTEL_ENABLED` | `false` | Enable OTLP push (metric + trace exporters) and `otelhttp` middleware. The Prometheus scrape endpoint (`/metrics`) and OIDC metrics are always on regardless of this flag. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (OTel SDK default) | Standard OTel environment variable — honored directly, no cyoda-specific alias. |

The trace sampler is swappable at runtime via `POST /api/admin/trace-sampler`
(see §11). The initial sampler honors `OTEL_TRACES_SAMPLER` and
`OTEL_TRACES_SAMPLER_ARG` at startup.

### Storage — plugin selection

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_STORAGE_BACKEND` | `memory` | Name of the active plugin. Must match a registered plugin (one of those blank-imported by the binary's `main.go`). Unknown names fail fast at startup with a listing of available plugins. |

Per-store routing is **not supported** — a running binary uses one plugin for all stores. Mixing backends per store type is by design not part of the plugin contract.

### PostgreSQL plugin (`CYODA_STORAGE_BACKEND=postgres`)

Advertised via `DescribablePlugin.ConfigVars()`; rendered in the binary's `--help`. Full reference: [docs/plugins/POSTGRES.md](plugins/POSTGRES.md).

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_POSTGRES_URL` (with `_FILE` variant) | (none, **required**) | PostgreSQL connection string |
| `CYODA_POSTGRES_MAX_CONNS` | `25` | Maximum pool connections |
| `CYODA_POSTGRES_MIN_CONNS` | `5` | Minimum pool connections |
| `CYODA_POSTGRES_MAX_CONN_IDLE_TIME` | `5m` | Max idle time before connection is closed |
| `CYODA_POSTGRES_AUTO_MIGRATE` | `true` | Run embedded SQL migrations at startup |
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | Maximum run time for a single SQL statement; `0` disables |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | Maximum time a connection may sit idle inside an open transaction; `0` disables |
| `CYODA_POSTGRES_ACQUIRE_TIMEOUT` | `10s` | Maximum wait for a free pooled connection before failing with `503 STORAGE_UNAVAILABLE`; `0` disables |
| `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` | `30m` | Statement ceiling for async search scans; `0` disables |
| `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` | `5m` | Maximum lock wait during schema migration; `0` disables |
| `CYODA_SCHEMA_SAVEPOINT_INTERVAL` | `64` | Rows between plugin-internal savepoints when folding schema extensions. Shared with the sqlite plugin — not plugin-namespaced. |

See §3.4 for what the ceilings bound and how they surface to callers.

### SQLite plugin (`CYODA_STORAGE_BACKEND=sqlite`)

Advertised via `DescribablePlugin.ConfigVars()`; rendered in the binary's `--help`. Full reference: [docs/plugins/SQLITE.md](plugins/SQLITE.md).

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_SQLITE_PATH` | Platform-specific (see below) | Database file path. |
| `CYODA_SQLITE_AUTO_MIGRATE` | `true` | Run embedded schema migrations at startup. |
| `CYODA_SQLITE_BUSY_TIMEOUT` | `5s` | SQLite `busy_timeout` pragma. |
| `CYODA_SQLITE_CACHE_SIZE` | `64000` | SQLite `cache_size` pragma (KiB), **per connection** — one writer plus the reader pool. |
| `CYODA_SQLITE_READER_POOL_SIZE` | `GOMAXPROCS` clamped to `4`..`8` | Max concurrent reader connections. Minimum 1; a value below it falls back to the default. Peak page-cache use is `(this + 1) × CYODA_SQLITE_CACHE_SIZE`. |
| `CYODA_SCHEMA_SAVEPOINT_INTERVAL` | `64` | Rows between plugin-internal savepoints when folding schema extensions. Shared with the postgres plugin — not plugin-namespaced. |

Default `CYODA_SQLITE_PATH`: on Linux / macOS, `$XDG_DATA_HOME/cyoda/cyoda.db` with fallback to `~/.local/share/cyoda/cyoda.db`; on Windows, `%LocalAppData%\cyoda\cyoda.db`.

### IAM

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_IAM_MODE` | `mock` | `mock` (dev) or `jwt` (production) |
| `CYODA_JWT_SIGNING_KEY` (with `_FILE` variant) | (none) | PEM-encoded RSA private key (or base64-encoded PEM) |
| `CYODA_JWT_ISSUER` | `cyoda` | JWT issuer claim |
| `CYODA_JWT_EXPIRY_SECONDS` | `3600` | Token expiry in seconds |
| `CYODA_REQUIRE_JWT` | `false` | Production safety floor: when `true`, the binary refuses to start unless `CYODA_IAM_MODE=jwt` AND `CYODA_JWT_SIGNING_KEY` is set. Protects against silently shipping a mock-auth deployment. |
| `CYODA_IAM_MOCK_ROLES` | `ROLE_ADMIN,ROLE_M2M` | Comma-separated roles attached to the default mock user (mock mode only). |

### OIDC Provider Registry

These variables apply globally to all tenant-registered OIDC providers. Per-provider overrides (`rolesClaim`, `issuers`, `expectedAudiences`) are stored per-record in KV, not as env vars. See §7.3.

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_OIDC_REQUIRE_HTTPS` | `true` | Reject OIDC provider URLs that do not use `https://`. Disable only in isolated test environments. |
| `CYODA_OIDC_CONNECT_TIMEOUT_MS` | `5000` | TCP connection timeout (ms) for JWKS discovery and fetch requests. |
| `CYODA_OIDC_SOCKET_TIMEOUT_MS` | `5000` | Socket read timeout (ms) for JWKS responses. |
| `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` | `5000` | Timeout (ms) to acquire a connection from the HTTP client pool for OIDC requests. |
| `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` | `false` | Allow OIDC provider URLs that resolve to private/loopback/link-local addresses. When `false`, registering such a URL returns `400 OIDC_SSRF_BLOCKED`. |
| `CYODA_OIDC_ROLES_CLAIM` | `roles` | Default JWT claim name to extract roles from for externally-issued tokens. Overridable per-provider at registration time. |

### Bootstrap

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_BOOTSTRAP_CLIENT_ID` | (none) | M2M client ID to create at startup. Must be set together with `CYODA_BOOTSTRAP_CLIENT_SECRET` or both left empty — half-configured rejected (jwt mode). |
| `CYODA_BOOTSTRAP_CLIENT_SECRET` (with `_FILE` variant) | (none) | M2M client secret. Required when `CYODA_BOOTSTRAP_CLIENT_ID` is set in jwt mode; ignored in mock mode. |
| `CYODA_BOOTSTRAP_TENANT_ID` | `default-tenant` | Tenant for bootstrap client. Must match the tenant grammar (§1); a value outside it refuses startup, but only in jwt mode with a bootstrap client configured — mock mode ignores bootstrap entirely, and a jwt deployment that configures no bootstrap client never reads the value, including when it is explicitly empty. |
| `CYODA_BOOTSTRAP_USER_ID` | `admin` | User ID for bootstrap client. Must pass the user-identifier rule (§1); a value outside it refuses startup in jwt mode with a bootstrap client configured. |
| `CYODA_BOOTSTRAP_ROLES` | `ROLE_ADMIN,ROLE_M2M` | Comma-separated roles |

### gRPC

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_GRPC_PORT` | `9090` | gRPC server listen port |
| `CYODA_KEEPALIVE_INTERVAL` | `10` | Seconds between server keep-alive pings to each compute member; also the transport keepalive idle time |
| `CYODA_KEEPALIVE_TIMEOUT` | `30` | Seconds of inbound silence or write stall before a compute member is evicted; also the transport keepalive ack timeout |
| `CYODA_RETRY_FIXED_NUM_RETRIES` | `3` | Retries after the first try when `retryPolicy` is `FIXED` or unset. |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` | `30000` | Answer limit when the callout sets no `responseTimeoutMs`. |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` | `60000` | Upper bound on `responseTimeoutMs`; workflow import refuses more. |

### Cluster

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_CLUSTER_ENABLED` | `false` | Enable multi-node cluster mode |
| `CYODA_NODE_ID` | (none) | Stable unique node identifier (required if cluster enabled) |
| `CYODA_NODE_ADDR` | `http://localhost:8080` | This node's reachable HTTP address (must include scheme) |
| `CYODA_GOSSIP_ADDR` | `:7946` | Memberlist gossip bind address |
| `CYODA_SEED_NODES` | (none) | Comma-separated `host:port` for gossip seeds |
| `CYODA_GOSSIP_STABILITY_WINDOW` | `2s` | Wait for stable membership count after join |
| `CYODA_PROXY_TIMEOUT` | `30s` | HTTP proxy response header timeout |
| `CYODA_HMAC_SECRET` (with `_FILE` variant) | (none) | Hex-encoded secret for token signing + gossip encryption (required if cluster enabled). See §4.2 for encoding details. |
| `CYODA_DISPATCH_WAIT_TIMEOUT` | `5s` | The patience: how long one callout waits, in total, for a compute member with matching tags to exist, on a single node as in a cluster. `0` disables waiting. Must not be negative; startup fails otherwise. |
| `CYODA_DISPATCH_CONNECT_TIMEOUT` | `2s` | Time allowed to open the connection when a callout is handed over to another node; a node that cannot be connected to costs no try. Must be `> 0`; startup fails otherwise. |
| `CYODA_CALLOUT_HANDOVER_ALLOWANCE` | `30s` | What the owning node allows a callout hand-over on top of `tries left × answer limit`; also the last term of a callout's overall deadline. Must be `> 0`; startup fails otherwise. |
| `CYODA_CALLOUT_PASS_ALLOWANCE` | `30s` | How long the transaction token given to a compute member outlives its try's answer limit. Must be `> 0`; startup fails otherwise. |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | `30s` | Whole-request timeout of the node-to-node call that delegates a scheduled transition. Does not govern callout hand-overs. Must be `> 0`; startup fails otherwise. |

### Search

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_SEARCH_SNAPSHOT_TTL` | `1h` | TTL for async search job results |
| `CYODA_SEARCH_REAP_INTERVAL` | `5m` | Frequency of the search **snapshot-TTL** reaper only (deletes terminal jobs past `CYODA_SEARCH_SNAPSHOT_TTL`); does not drive the stale/reclaim sweep |
| `CYODA_SEARCH_MAX_SORT_KEYS` | `16` | Max `sort`/`orderBy` keys per search request |
| `CYODA_SEARCH_ASYNC_WORKERS` | `8` | Async-search worker pool size; startup fails if `< 1` |
| `CYODA_SEARCH_ASYNC_QUEUE` | `256` | Async-search submit queue capacity beyond running workers; startup fails if `< 0` |
| `CYODA_SEARCH_ASYNC_MAX_PER_TENANT` | `8` | Max async-search jobs one tenant may hold in flight (queued + running) per node; `0` disables; startup fails if `< 0` |
| `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` | `15s` | How often a running async-search executor stamps liveness and polls for cross-node cancel/terminal status; also the ticker cadence for the stale/reclaim sweep (plus a startup sweep) |
| `CYODA_SEARCH_JOB_STALE_AFTER` | `5m` | How long a `RUNNING` job may go without a heartbeat before the reaper claims it for reclaim; must be `>= 4x` the heartbeat interval |
| `CYODA_SEARCH_JOB_MAX_ATTEMPTS` | `3` | Executions an async-search job may consume (initial run + one per executor lost) before the reaper fails it instead of reclaiming it again; startup fails if `< 1` |

---

## 10. Deployment Architecture

### 10.1 Single-Node

```bash
# Direct
go build -o bin/cyoda ./cmd/cyoda
./bin/cyoda

# Docker
./scripts/dev/run-docker-dev.sh
```

The Docker script builds the binary from source, produces a local `:dev` image, and runs `deploy/docker/compose.yaml`.

### 10.2 Multi-Node Cluster

```bash
# Start a 3-node cluster with nginx load balancer
./scripts/multi-node-docker/start-cluster.sh --nodes 3
```

Architecture:

```
                    +-----------+
     Client ------->|  nginx LB |
                    +-----------+
                    /     |      \
              +------+ +------+ +------+
              |Node 1| |Node 2| |Node 3|
              +------+ +------+ +------+
                  \       |       /
                   +------+------+
                   | PostgreSQL  |
                   +-------------+
```

- **nginx:** Round-robin load balancer, with an HTTP upstream and a separate HTTP/2 upstream for gRPC. It forwards every path to the node pool, including `/internal/*` — the dispatch endpoint's own AEAD authentication (§4.2) is what protects it, not the load balancer's path set. Restricting external exposure to `/api/*` is the deployment's responsibility.
- **Gossip:** Each node runs a memberlist listener on a distinct port. Seed nodes are configured so all nodes discover each other.
- **Shared PostgreSQL:** All nodes connect to the same PostgreSQL instance. `REPEATABLE READ` + application-layer SI+FCW validation + RLS ensure correctness (see [docs/CONSISTENCY.md](CONSISTENCY.md)).
- **Shared secrets:** All nodes share the same HMAC secret (for token verification and gossip encryption) and the same JWT signing key (for deterministic KID derivation).

**Scripts:**

| Script | Purpose |
|--------|---------|
| `scripts/multi-node-docker/start-cluster.sh` | Generate secrets, nginx config, docker-compose, start cluster |
| `scripts/multi-node-docker/stop-cluster.sh` | Stop and clean up cluster containers |

The start script:
1. Generates secrets once, persists to `.env` (reused on restart)
2. Generates nginx config with upstream entries for N nodes
3. Generates `docker-compose.generated.yml` with N node services + postgres + nginx
4. Runs `docker compose up`

---

## 11. Observability

OpenTelemetry is integrated end-to-end. The OTel SDK is initialised in `internal/observability/init.go`. The meter provider always carries an OpenTelemetry → Prometheus exporter (a dedicated `prometheus.Registry` served at `/metrics`); when `CYODA_OTEL_ENABLED=true` it additionally carries an OTLP `PeriodicReader` and the OTLP trace exporter. Thus `/metrics` exposes application metrics with no collector, while OTLP push remains opt-in. When `CYODA_OTEL_ENABLED=true`, W3C Trace Context and Baggage are installed as the global propagator; with OTel disabled no propagator is configured.

**HTTP middleware:** the generated API router is wrapped in `otelhttp.NewMiddleware` (enabled when `CYODA_OTEL_ENABLED=true`), producing `http.server` spans for every request and auto-extracting upstream trace context from `traceparent` headers.

**OIDC subsystem metrics** (`oidc_*`) are always exposed at `/metrics` when IAM runs in `jwt` mode — no collector required, no flag to toggle.

**Transaction manager decorator:** `TracingTransactionManager` wraps the underlying transaction manager and adds spans (`tx.begin`, `tx.commit`, `tx.rollback`, `tx.savepoint`, `tx.rollback_to_savepoint`, `tx.release_savepoint`) plus metrics (`cyoda.tx.duration`, `cyoda.tx.active`, `cyoda.tx.conflicts`). This decorator is active when `CYODA_OTEL_ENABLED=true`.

**Workflow and dispatch:** spans for `workflow.execute`, `workflow.manual_transition`, `workflow.loopback`, `workflow.cascade`; `dispatch.processor`, `dispatch.criteria` and `dispatch.function` with `cyoda.dispatch.duration`, `cyoda.dispatch.count`, `cyoda.callout.tries` and `cyoda.callout.wait.duration` metrics. These are active when `CYODA_OTEL_ENABLED=true`. Two more `cyoda.callout.*` counters are registered elsewhere and are exposed regardless of it: `cyoda.callout.handovers` on the peer router, in cluster mode, and `cyoda.callout.superseded` where a compute member's callback joins its transaction. The `cmd/cyoda/help/content/telemetry.md` help topic is the full reference.

**Plugin-level instrumentation:** plugins are free to add their own
spans and metrics under a plugin-specific namespace. The `memory`
plugin does not emit custom plugin-level telemetry; its behaviour is
fully captured by the core transaction / workflow / dispatch spans
listed above. The `postgres` plugin registers seven
`cyoda.storage.pool.*` instruments (connections by state, max
connections, acquires, empty acquires, canceled acquires, acquire
duration, empty-acquire wait — all labeled `backend="postgres"`) from
`pgxpool.Stat()`, unconditionally at `NewFactory` — pool saturation is
the dominant outage mode this instrumentation guards against, so
these are always on regardless of `CYODA_OTEL_ENABLED`, unlike the
core transaction/dispatch metrics above. Other plugins may add
detailed instrumentation scoped to their own namespace as their
hot-path semantics warrant.

**Exporter endpoint:** `OTEL_EXPORTER_OTLP_ENDPOINT` (standard OTel env var). `examples/compose-with-observability/` brings up a Grafana / Prometheus / Tempo stack via `grafana/otel-lgtm` with a dashboard provider registered for cyoda-go.

**Runtime sampler control.** The trace sampler is swappable at runtime via `POST /api/admin/trace-sampler` (requires `ROLE_ADMIN`), mirroring `/api/admin/log-level`. Operators can toggle between 100% sampling, probabilistic sampling, and off without restarting the service. The initial sampler honors the standard OTel env vars `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` at startup.

Trace context does not reach the search pipeline or outbound
external-processor calls — see §12.

---

## 12. Known Gaps

Capabilities this document's design implies but the system does not provide. Each is a known gap, not an oversight.

| Gap | What it would give |
|---------|---------|
| Commit markers (PostgreSQL plugin) | Resolve transaction commit ambiguity (L5 partition at COMMIT — see §4.5 Phase 4). Today a torn connection at COMMIT is reported as retryable; it is never disambiguated. |
| Strict context deadline propagation | A deadline derived from the inbound request and inherited by every downstream operation. The server bounds how long a request may take to *arrive* (`CYODA_HTTP_READ_TIMEOUT`, §9) but derives no deadline from it for handler execution; a callout enforces its own independent wall clock (§4.3), and a joined request runs detached from its client's cancellation from the moment it holds the transaction's lock (§3.8). |
| Idempotency keys | Client-provided keys preventing duplicate operations on retry. The `IDEMPOTENCY_CONFLICT` code is reserved, but no handler reads an `Idempotency-Key` header. |
| Trace propagation through the search pipeline | A unified search trace waterfall. The search packages emit no spans, and the async-search goroutine starts from a fresh context, severing the parent span. |
| Outbound trace propagation to external processors | End-to-end workflow tracing. Inbound gRPC trace context is extracted and dispatches are wrapped in spans, but no `traceparent` is injected into the dispatched CloudEvent or the peer-forward request. |
| Migration-runner retry tolerance for a deadlock-killed advisory lock | Being able to use `CREATE INDEX CONCURRENTLY` for an index added on an already-populated table without deadlocking the concurrent multi-node boot path. Today the migration runner holds one session-level advisory lock for a migrator's entire run with no retry on a `SQLSTATE 40P01` from a lock cycle against `CONCURRENTLY`'s own multi-phase wait, so `entities`' migration `000008` uses a plain `CREATE INDEX` (writer-blocking for the build's duration) instead — see `docs/plugins/POSTGRES.md`. Migrations `000011`, `000012` and `000013` have since taken the same exception for the same reason; every index-on-populated-table migration hits this choice until the gap closes. |

---

## 13. Design Decisions Log

### DD-1: HMAC Token + Separate UUID

**Context:** How to route requests to the correct transaction-owning node.

**Decision:** HMAC-signed opaque token — a **pass** — containing `{nodeID, txRef, expiresAt}` plus the callout it was minted for and that try's fencing number. The `txRef` is a separate UUID used as a key into the owner's local transaction map. The `nodeID` is extracted locally (no network call) for routing.

**Rationale:** The token is opaque to clients. HMAC verification is a CPU-local operation. No distributed registry lookup is needed for routing decisions, and because the callout and the number ride under the same signature, the node that routes the callback is also the node that can judge whether it is still current (§3.8).

### DD-2: Fencing Tokens Not Required for Transaction Ownership

**Context:** Whether to use fencing tokens to prevent stale writes from zombie transactions — two *nodes* believing they hold the same one.

**Decision:** Not required. The `pgx.Tx` single-owner property guarantees that only one goroutine on one node holds a physical PostgreSQL transaction.

**Rationale:** Fencing tokens exist to stop a process that believes it still owns a resource from writing after ownership has moved. That situation is unreachable here: a transaction is a connection, a connection has exactly one holder, and there is no mechanism by which two nodes come to hold the same one. The decision rests on that property alone — not on any liveness or expiry mechanism. If the owning node dies its connection drops and PostgreSQL rolls the transaction back; the ceilings in §3.4 bound how long an abandoned one can occupy a connection, but they are resource hygiene, not the reason fencing is unnecessary. A **compute member** that was replaced is a different case: it can still send callbacks into a transaction that is open and correctly owned, so ownership of a *callout's work* is fenced — by a number in the pass, decided in memory by the owner (§3.8).

### DD-3: Transparent Proxy

**Context:** How to handle requests that arrive at the wrong node.

**Decision:** HTTP middleware (`proxy.HTTPRouting`) uses `httputil.ReverseProxy` to transparently forward requests to the correct node. The target node sees the original request with all headers intact.

**Rationale:** Minimizes client complexity. The client does not need to know about cluster topology. The proxy is a standard reverse proxy pattern with connection pooling.

### DD-4: Gossip Over PostgreSQL for Registry

**Context:** How nodes discover each other.

**Decision:** HashiCorp memberlist (SWIM gossip) instead of a PostgreSQL-backed registry table.

**Rationale:** Gossip provides sub-second failure detection, requires no additional infrastructure, and scales to the target cluster size (2-20 nodes). A PostgreSQL registry would add polling latency and another failure mode on the critical path.

### DD-5: Operator-Assigned Node IDs

**Context:** How to identify nodes.

**Decision:** Node IDs are stable strings assigned by the operator via `CYODA_NODE_ID`, not auto-generated UUIDs.

**Rationale:** Stable IDs survive restarts, simplify log correlation, and make cluster configuration deterministic. Docker scripts generate them as `node-1`, `node-2`, etc.

### DD-6: Random Peer Selection

**Context:** How to pick among multiple peers with matching compute tags.

**Decision:** `RandomSelector` -- uniform random selection from the alive candidates, applied repeatedly to produce the order in which the owner's loop asks them. Each peer is asked at most once per pass.

**Rationale:** Simple, stateless, no coordination needed. Load balancing across peers is acceptable for the expected cluster size. More sophisticated strategies (round-robin, least-loaded) can be added by implementing the `PeerSelector` interface, which decides order only — whether a peer is asked at all, and what its answer costs, is the owner's loop's (§4.3).

### DD-7: Tag Lists Beside Gossip Metadata

**Context:** How to find which node has a compute member with the required tags.

**Decision:** Each node announces a tag-list version in its gossip metadata and sends the list itself, organized per tenant, to each peer over memberlist's reliable channel; a peer that is behind fetches it.

**Rationale:** Avoids a centralized registry. Tag lookups are local memory reads. memberlist caps node metadata at 512 bytes, which a handful of tenants exceeds; the reliable channel has no size limit, and the announced version makes a lost message detectable.

### DD-8: HTTP for Dispatch Forwarding

**Context:** What protocol to use for cross-node dispatch forwarding.

**Decision:** HTTP POST to `/internal/dispatch/callout` (single route for every callout kind, discriminated by `Kind` in the request body), authenticated and encrypted with AES-256-GCM AEAD (PeerAuth interface, AEADPeerAuth impl). The AEAD key is HKDF-derived from `CYODA_HMAC_SECRET`; the forwarder and handler share a `PeerAuth` seam so a future mTLS-based transport can be swapped in without changing the dispatch logic.

**Rationale:** Reuses the existing HTTP infrastructure. The dispatch payload is a single request-response pair (not a stream), making HTTP a natural fit. AEAD gives integrity, confidentiality and replay resistance (via timestamp skew + nonce cache) in one primitive. Identity is cluster-scoped rather than per-node; making it per-node is a transport change behind the `PeerAuth` seam, not a protocol change.

### DD-9: Event-Driven Wait for Missing Compute Members

**Context:** What to do when no compute member matches the required tags.

**Decision:** The owner's loop waits on change signals — the local member registry's and the cluster registry's `Changed()` channel, closed and replaced on every change — for up to `CYODA_DISPATCH_WAIT_TIMEOUT` (default 5s) in total per callout, in single-node and cluster mode alike. `0` disables waiting.

**Rationale:** Compute members may be joining or reconnecting; a brief wait avoids spurious failures. A signal wakes the callout the moment a member appears and costs nothing while nothing changes. The allowance is per callout, not per wait, so a callout's worst-case duration is known when it starts.

### DD-10: Store Entity IDs Only in Search Results

**Context:** What to store in async search result tables.

**Decision:** Only entity IDs are stored. Entity data is re-fetched from the entity store when results are read.

**Rationale:** Keeps the results table compact. Avoids data staleness -- the entity may have been updated between search execution and result retrieval. `pointInTime` on the search job ensures deterministic re-fetch.

### DD-11: pointInTime Always Populated

**Context:** Whether `pointInTime` should be optional on search jobs.

**Decision:** Always populated. If the client does not supply one, the service uses `time.Now()`.

**Rationale:** Ensures search results are deterministic. Repeated reads at the same `pointInTime` return the same set. Eliminates an entire class of bugs around "what time was this search as of?"

### DD-12: TTL-Based Cleanup in Every Plugin

**Context:** How to clean up expired search jobs.

**Decision:** Background reaper goroutine with configurable interval and TTL. Every plugin implements `ReapExpired`.

**Rationale:** Consistent behavior regardless of storage backend. The SQL plugins lean on `ON DELETE CASCADE` on the results foreign key; the in-memory plugin scans and deletes. All are driven by the same configuration variables.

---

## 14. Non-Functional Limits and Design Boundaries

This section describes where Cyoda-Go is expected to encounter limits. These are not bugs — they are the explicit trade-offs of the architecture. Understanding them is essential for sizing, capacity planning, and deciding when Cyoda-Go is the right tool vs. a horizontally scalable alternative like Cyoda Cloud.

### 14.1 Horizontal Scalability

**Design boundary:** Cyoda-Go targets small clusters — the design point DD-4 states is 2–20 nodes. It is not limitlessly horizontally scalable.

| Dimension | Scaling Behavior | Limit |
|-----------|-----------------|-------|
| **Node count** | Linear improvement in compute dispatch capacity (more nodes = more compute members). No improvement in write throughput — all writes go through PostgreSQL. | No hard limit is enforced. The upper end of DD-4's range is a judgement, not a derived bound; what does rise with cluster size is the probability that a request lands on a node other than the transaction owner and has to be proxied. |
| **Write throughput** | Bounded by PostgreSQL `REPEATABLE READ` + application-layer SI+FCW validation (see §3.7). A transaction holds a `pgx.Tx` for its full duration, including external compute phases — except across a `COMMIT_BEFORE_DISPATCH` boundary, where the connection is released for the callout. | Single PG instance is the bottleneck. Connection pool default is 25 per node; with 10 nodes that's 250 concurrent PG connections. Long-held transactions reduce effective throughput. |
| **Read throughput** | Scales with node count for non-transactional reads (entity queries, search). Each node can serve reads independently from PG. | Bounded by PG read capacity. Point-in-time queries require version table scans. |
| **Compute throughput** | Scales with compute member count across the cluster. Each node can host multiple compute members. Cross-node dispatch adds one HTTP hop. | Bounded by compute member availability per tag. If only one node has a member for a given tag, that node is the bottleneck for that tag. |

**Contrast with Cyoda Cloud:** Cyoda Cloud uses a fully distributed storage layer with no single-node write bottleneck. The open-source cyoda-go binary trades unlimited write scalability for simpler operational requirements (a single primary PostgreSQL — or none at all, with the memory or sqlite plugins).

### 14.2 Transaction Timing and Duration

**Design boundary:** Transactions are held open for the full workflow execution, including external compute phases.

| Constraint | Value | Consequence |
|------------|-------|-------------|
| **PG statement timeout** | Default 5m (`CYODA_POSTGRES_STATEMENT_TIMEOUT`) | PostgreSQL aborts any single statement that exceeds it. The abort is **not** retryable — re-running the statement would exceed the same ceiling — so it surfaces as a `500` with a ticket, not a `503` (§3.4). |
| **PG idle-in-transaction timeout** | Default 5m (`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`) | PostgreSQL aborts a transaction whose connection sits idle past it — which is what a transaction waiting on an external callout is doing. This is the authoritative bound on transaction lifetime; the callout deadline below must fit under it. |
| **Pool acquire timeout** | Default 10s (`CYODA_POSTGRES_ACQUIRE_TIMEOUT`) | An operation that cannot get a connection within it fails fast with `503 STORAGE_UNAVAILABLE` rather than queueing behind a saturated pool. Applies to writes, and to reads needing a *second* connection while the caller's transaction holds one — a point-in-time read or an async-search submit issued inside a transaction. Unbounded otherwise, so ordinary pool contention on a non-transactional read does not fail spuriously. |
| **Connection hold time** | Duration of entire flow chain (BEGIN → workflow → compute dispatch → callbacks → COMMIT) | Each in-flight transaction consumes one PG connection for its full lifetime. With 25 connections per node and 10 nodes, the cluster supports ~250 concurrent transactions. |
| **Proxy timeout** | Default 30s (configurable) | Cross-node proxy hops for CRUD callbacks must complete within this window. |
| **Callout deadline** | tries × answer limit + patience + hand-over allowance; 155s at the defaults | Fixed when the callout starts. No try or hand-over begins after it and one in progress is cut off at it, so the *time* a callout can take is bounded even though a lost hand-over answer can make the number of tries exceed the setting (§4.3). |
| **Hand-over answer wait** | tries left × answer limit + `CYODA_CALLOUT_HANDOVER_ALLOWANCE`, never past the callout deadline | The owner's wait for a peer's sealed answer. `CYODA_DISPATCH_CONNECT_TIMEOUT` (2s) bounds opening the connection only; `CYODA_DISPATCH_FORWARD_TIMEOUT` (30s) bounds the scheduler's peer RPC, not this. |
| **Compute member response timeout** | Per-callout `responseTimeoutMs` (default `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, 30s) | The answer limit of one try: a member that does not answer within it ends that try, and whether another member is tried depends on the failure kind (§4.3). Bounded at import by `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (60s), but not against the idle-in-transaction ceiling — a callout deadline above it means PostgreSQL aborts the transaction first (§3.4). |

**Expected bottleneck:** The dominant limit is long-running compute phases holding PG connections. A processor that runs for N seconds holds one connection for at least N seconds, so a node's concurrent-transaction ceiling is its pool size and its throughput is that ceiling divided by processor duration.

**Mitigation: `COMMIT_BEFORE_DISPATCH`.** This is the **primary connection-pool-pressure mitigation** for slow processors (§5.4). The engine splits the cascade into two transactions around the processor: `TX_pre` flushes the pre-callout entity state and commits **before** dispatch, releasing the PG connection for the duration of the external compute. The processor runs outside any transaction. `TX_post` opens on the same node when the processor returns, reapplies the result via `CompareAndSave`, and commits. The PG connection hold time collapses from "full cascade duration" to "`TX_pre.Commit` time + `TX_post` apply-result time", which is independent of processor wall-clock — so throughput stops scaling inversely with processor duration. Trade-offs: cascade atomicity is broken at the segment boundary (entity becomes publicly observable in pre-callout state; engine cannot rollback if `TX_post` aborts); processor must be idempotent (retries re-dispatch); CAS conflict at segment continuation surfaces as `409 retryable`. See `docs/CONSISTENCY.md` §10 for the full author-facing contract. `ASYNC_NEW_TX` (savepoint mode) does **not** relieve connection-pool pressure — it still holds the parent connection through the processor; it only changes failure semantics (savepoint rollback vs. cascade abort). For slow external work, prefer `COMMIT_BEFORE_DISPATCH`.

### 14.3 Data Volume Limits

| Dimension | Limit | Reason |
|-----------|----------------|--------|
| **Entity size** | 10 MB per request body | Enforced by the entity handler. Entity data is stored as JSONB in PostgreSQL; large entities degrade query performance and increase replication lag. |
| **Entities per model** | Bounded by PostgreSQL | Point-in-time queries scan `entity_versions`, which grows with write volume. Indexing reduces but does not eliminate the cost. |
| **Entity version history** | Unbounded (append-only) | The `entity_versions` table grows monotonically. No built-in compaction or archival. Long-lived entities with frequent updates accumulate large version histories. |
| **Search result sets** | Bounded by re-fetch cost | Async search stores entity IDs, not data, so the results table stays compact. Entity data is re-fetched on read, so page retrieval cost scales with page size × entity fetch cost. |
| **In-memory mode** | Process heap | Single-node standalone only (not multi-node compatible). All entities, versions, models and search results are held in process memory. Intended for rapid development and agentic application engineering, not production data volumes. |

### 14.4 Fault Tolerance and Reliability

| Scenario | Behavior | Recovery |
|----------|----------|----------|
| **Node crash** | PG rolls back all open transactions on that node. Gossip detects failure within seconds. Other nodes see `TRANSACTION_NODE_UNAVAILABLE` for in-flight tokens. | Automatic. Clients retry with new transactions on surviving nodes. No data loss (uncommitted work was never durable). |
| **Node network partition (from cluster)** | Partitioned node continues operating if it can reach PG. Other nodes cannot proxy to it. Transactions owned by the partitioned node continue normally if PG link is up. | Gossip re-merges when partition heals. Outstanding tokens for the partitioned node fail on other nodes. |
| **Node partition from PostgreSQL** | PG kills the connection after TCP timeout. All open transactions on that node are rolled back by PG. Node detects dead connection on next PG operation. | Node must reconnect to PG. All in-flight work is lost (rolled back). Clients get errors and retry. |
| **PostgreSQL failure** | All nodes lose write capability simultaneously. No new transactions can begin. Existing transactions cannot commit. | Requires PG recovery (HA failover, restart). Cyoda-Go nodes reconnect automatically via pgx pool. |
| **Compute member disconnect** | A callout in flight on the member is classified: given to another member when that is safe — the hand-off had not been made, or the callout is repeat-safe — and failed with `COMPUTE_MEMBER_DISCONNECTED` otherwise. The member's tag list is republished to peers within seconds, and the local change signal wakes any callout waiting for a member. | Automatic if other members exist. If no member for the tag, callouts fail once the patience is used up. |
| **nginx LB failure** | All external traffic stops. Nodes are healthy but unreachable. | LB must be restored. Nodes continue gossiping and can handle direct traffic if clients bypass the LB. |

**Single point of failure:** PostgreSQL. If PG is down, the cluster is effectively down for writes. This is by design — PG is the consistency authority. HA PostgreSQL (streaming replication with automatic failover) is the recommended mitigation.

**No split-brain:** The `pgx.Tx` single-owner property ensures that no two nodes can commit the same transaction. PostgreSQL `REPEATABLE READ` plus the application-layer SI+FCW validation (§3.7) catches conflicting concurrent writes from different transactions. There is no application-level consensus needed because PG is the sole arbiter.

### 14.5 Consistency Guarantees and Caveats

| Guarantee | Strength | Caveat |
|-----------|----------|--------|
| **Read-your-own-writes** | Strong (within a transaction) | Guaranteed by `pgx.Tx` — all reads within a transaction see its own buffered writes. Across transactions, reads are snapshot-isolated. |
| **Snapshot isolation** | Strong (SI+FCW across all plugins; see §3.7 and [docs/CONSISTENCY.md](CONSISTENCY.md)) | Commit-time conflict detection may abort with `ErrConflict` (40001 / 40P01 on PostgreSQL). The application retries. Under high contention, retry storms are possible. |
| **Cross-node consistency** | Strong (PG is the authority) | All nodes share the same PG instance. There is no eventual consistency between nodes — they all see the same data at the same isolation level. Cluster membership and the per-node compute-tag lists are eventually consistent with sub-second convergence. |
| **Temporal consistency** | Strong (point-in-time queries) | `GetAsAt` returns the entity as it was at a specific timestamp. A revision is dated at its transaction's commit instant, so a read at an instant is stable once every transaction that started before it has finished; a read taken *while* a transaction commits can still change (`docs/CONSISTENCY.md` §1a). Resolution is bounded by PG clock precision (microsecond). |
| **Commit ambiguity** | **Gap** (§12) | If the network partitions between Node A and PG at COMMIT time, Node A cannot determine whether PG committed or not. The client may see a false failure for a transaction that actually committed. |
| **Idempotency** | **Gap** (§12) | Client retries after timeout may create duplicate entities. There is no built-in idempotency key mechanism; clients must handle deduplication at the application level. |

### 14.6 Operational Limits

| Parameter | Default | Hard Limit | Notes |
|-----------|---------|------------|-------|
| PG connections per node | 25 | Configurable, bounded by PG `max_connections` | Each in-flight transaction holds one connection. |
| Gossip metadata size | ~100–150 bytes per node | memberlist `MetaMaxSize` = 512 bytes | Identity and a list version only; tenants and tags travel by reliable message and are unbounded. A node whose identity does not fit refuses to start. Alert on `cyoda.cluster.tags.lists_outstanding` staying non-zero. |
| Search snapshot TTL | 1 hour | Configurable | Snapshots older than TTL are reaped. Increase for long-running batch workflows. |
| Transaction lifetime | 5 minutes idle | Configurable | Enforced by PostgreSQL via `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`. The callout deadline must fit under it (§4.3). |
| Max cascade depth | 100 | Hardcoded | Total cascade steps across all states in one engine invocation. |
| Max state visits per workflow | 10 | Configurable | Prevents infinite loops in workflow cascading. Increase for deeply nested state machines. |
| HTTP body limit | 10 MB | Hardcoded in entity handler | Increase requires code change. |
| gRPC keep-alive interval | 10 seconds | Configurable | Shorter intervals detect compute member failure faster but increase network overhead. |
| Dispatch wait timeout | 5 seconds | Configurable | One allowance in total per callout: how long a callout waits for a compute member with matching tags to exist, on a single node as in a cluster. Event-driven — a signal wakes the wait the moment a member appears, no polling. `0` disables waiting. |
