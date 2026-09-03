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
  txgate/                 Per-transaction mutual exclusion
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
    txjoin/               Callback transaction-join resolution
  grpc/                   CloudEventsService, streaming, dispatch
  api/                    HTTP handlers (generated OpenAPI types); middleware/
  cluster/
    token/                HMAC-signed transaction routing tokens
    proxy/                HTTP reverse proxy + gRPC routing helpers
    registry/             Gossip (memberlist) and local node registries; the Gossip
                          registry implements spi.ClusterBroadcaster and is passed to
                          plugins via spi.WithClusterBroadcaster
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

**Panic containment.** Four recovery sites wrap code that runs the engine or the store on the application's behalf: the gRPC server (unary and stream interceptors), every HTTP route, the async-search goroutine, and the scheduler's dispatch goroutine. All four log the value and stack, record a sanitized outcome (a ticket-carrying error on the request doors, a `FAILED` job for async search, a log line plus the ordinary redispatch throttle for a scheduled fire, which has no caller to answer), and mark the node unhealthy. The criterion is what the recovered code was doing, not where it entered from: a panic inside engine or store code leaves state nothing has verified. That is why the scheduler site latches too — `ClusterExecutor.Execute` fires in-process whenever distribution picks this node, so otherwise an identical panicking fire would withdraw the node only when the pick happened to be a peer.

Two further recovery sites deliberately do **not** latch, because they wrap notification callbacks rather than domain work: the member-registry `onChange` fan-out (`internal/grpc/members.go`) and the OIDC broadcast handler with its dispatch goroutines (`internal/auth/oidc/broadcast.go`, which counts panics on its own metric). Neither holds a transaction, and both self-heal on the next event.

Nothing resets the flag: `GET /health` on the API listener reports `503 DOWN` from then on, and the admin listener's `/readyz` (§7.5) reports `503` for the same reason. A node that has panicked has unverified state, so taking it out of service is the correct response rather than continuing to serve from a state nothing has checked.

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

**Processor timeouts must fit under the idle ceiling.** A `SYNC` or `ASYNC_SAME_TX` callout holds its transaction's connection idle for the whole dispatch, so a processor's `responseTimeoutMs` (default 30s) has to be shorter than `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`. It is not currently capped against it: a workflow may configure a callout longer than the ceiling, in which case PostgreSQL aborts the transaction mid-dispatch and the caller sees `503 STORAGE_UNAVAILABLE`. `COMMIT_BEFORE_DISPATCH` (§5.4) removes the constraint for a given processor by committing before the dispatch and holding no connection across it.

### 3.5 `pgx.Tx` Single-Owner Property

Extracted to [docs/plugins/POSTGRES.md](plugins/POSTGRES.md).

The property remains load-bearing for the cluster design: see DD-2 in [Section 13](#13-design-decisions-log) — fencing tokens are not required because no two nodes can share a PostgreSQL transaction.

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

**Node metadata** (JSON, serialized in memberlist node meta):

```go
type nodeMeta struct {
    ID       string              `json:"id"`                 // stable, operator-assigned
    Addr     string              `json:"addr"`               // HTTP address (e.g., "http://node-1:8123")
    GRPCAddr string              `json:"grpcAddr,omitempty"` // gRPC address, when advertised
    Tags     map[string][]string `json:"tags,omitempty"`     // tenantID → compute member tags
}
```

Tags are updated whenever a compute member joins or leaves a node. The update is pushed to the memberlist via `UpdateNode()`, and gossip propagates the change to all peers within milliseconds.

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
    NodeID    string `json:"n"`   // ID of the node holding the transaction
    TxRef     string `json:"t"`   // UUID, key into the node's local tx map
    ExpiresAt int64  `json:"e"`   // Unix timestamp
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
HKDF-SHA256-derived key (info string `"cyoda-dispatch-v1"`), which
separates the dispatch key from the raw gossip-encryption secret
despite both being derived from the same `CYODA_HMAC_SECRET`. Wire
format is `[nonce(12) || ciphertext||tag]` with Content-Type
`application/cyoda-dispatch-v1`; `X-Dispatch-Timestamp` is bound as
associated data along with HTTP method and path, preventing
cross-endpoint and timestamp-strip replays. A bounded, TTL-evicted
nonce cache rejects replays within the 30s skew window.

The token is opaque to the client. The router decodes it to extract `nodeID` without any network call -- address resolution is a local scan over `list.Members()`.

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

Three strategy interfaces, each with a default implementation:

| Component | Interface | Default Impl | Purpose |
|-----------|-----------|--------------|---------|
| Dispatch Strategy | `contract.ExternalProcessingService` | `ClusterDispatcher` | Local first, then cluster |
| Peer Selection | `PeerSelector` | `RandomSelector` | Pick from candidates |
| Forwarding Transport | `DispatchForwarder` | `HTTPForwarder` | HTTP POST to peer |

**`ClusterDispatcher` algorithm:**

```
1. Try local dispatch: registry.FindByTags(tenantID, tags)
   - If found → dispatch locally (existing gRPC stream path)
   - If error is NOT "no matching member" → return error

2. Cluster lookup with polling:
   a. Query gossip: registry.List() → filter by:
      - Not self
      - Alive
      - Tags[tenantID] overlaps required tags
   b. If candidates found → PeerSelector.Select(candidates) → forward
   c. If no candidates → wait 200ms, retry
   d. After CYODA_DISPATCH_WAIT_TIMEOUT (default 5s) → fail
      with NO_COMPUTE_MEMBER_FOR_TAG

3. Forward to selected peer:
   HTTPForwarder.ForwardCallout(ctx, peer.Addr, request)
   - POST to http://peer/internal/dispatch/callout
   - AES-256-GCM AEAD envelope over the request body
     (Content-Type: application/cyoda-dispatch-v1)
   - Peer verifies envelope, decrypts, calls local dispatch, returns result
```

Dispatch forwarding reuses a shared `http.Transport` (`MaxIdleConns: 20`,
`MaxIdleConnsPerHost: 5`, timeout via `CYODA_DISPATCH_FORWARD_TIMEOUT`,
default 30s) across all peer requests.

**Internal dispatch endpoint:**

```
POST /internal/dispatch/callout
```

- Single route for every callout kind (processor, criteria, function); `Kind` in
  the request body discriminates
- Authenticated and encrypted with the AES-256-GCM AEAD envelope described in §4.2
- 10MB max body size
- Reconstruct `UserContext` from request fields (tenantID, userID, roles, principal kind)

**Dispatch request/response types** (`internal/cluster/dispatch/types.go`): the
request carries the entity payload and meta, the workflow/transition names, the
callout txID and tx-token, the caller's tenant/user/roles/principal, the required
tags, and one kind-specific member (`Processor`, `Criterion` + `Target` +
`ProcessorName`, or `Function`). The response carries a success flag, the
kind-specific result (`EntityData` for a processor, `Matches` + `Reason` for a
criterion, `Result` + `ResultKind` for a function), accumulated warnings, and —
on failure — the peer's error code, HTTP status and retryable flag so the
originating node re-mints the same `AppError` the peer would have returned.

**Error handling:**

| Scenario | Behavior | Error Code |
|----------|----------|------------|
| No local member, no peer with tag | Poll gossip for wait timeout, then fail | `NO_COMPUTE_MEMBER_FOR_TAG` |
| Peer selected but unreachable | Fail (one peer, one attempt; no server-side failover to a second candidate). Marked retryable, so the client may retry | `DISPATCH_FORWARD_FAILED` |
| Peer dispatch times out | HTTP timeout, transaction rolls back | `DISPATCH_TIMEOUT` |
| Peer's local member disconnects | Peer returns error, propagated | `COMPUTE_MEMBER_DISCONNECTED` |
| Gossip metadata stale | Peer returns "no member for tag" | `NO_COMPUTE_MEMBER_FOR_TAG` |

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
t1   Node A: BEGIN tx-123, generate txToken --> PG: BEGIN REPEATABLE READ
t2   Node A: Save entity --> PG: INSERT entity (in tx-123)
t3   Node A: SM engine dispatches to processor
t3a  Node A: Check local MemberRegistry --> not found
t3b  Node A: Query gossip --> Node B has tag for this tenant
t3c  Node A: PeerSelector picks Node B (random from candidates)
t3d  Node A: Mint signed tx-token {NodeID=A, TxRef=tx-123}, attach as cyodatxtoken attribute
t3e  Node A: HTTPForwarder --> POST Node B /internal/dispatch/callout
t3f  Node B: Receives forwarded dispatch, finds local member, dispatches via gRPC stream
t4   Compute: Receives CloudEvent with cyodatxtoken (signed tx-token referencing Node A / tx-123)
t5   Compute: Executes business logic
t6   Compute: CRUD callback; echoes cyodatxtoken as X-Tx-Token (HTTP) or tx-token (gRPC metadata)
              --> Node B receives request with X-Tx-Token header
t7   Node B: Decode txToken --> extract Node A from claims --> proxy to Node A
t8   Node A: Receive proxied CRUD, Join tx-123 --> PG: INSERT/UPDATE (in tx-123)
t9   Node A: CRUD OK --> respond to Node B --> Node B forwards to Compute
t10  Compute: Receive CRUD OK --> finish logic
t11  Compute: Respond OK to Node A (via Node B dispatch return)
t12  Node A: SM complete, all processors finished
t13  Node A: COMMIT tx-123 --> PG: COMMIT
t14  Node A: 200 OK {entityId, transactionId} --> Client
```

Key observations:
- Node A is the single transaction owner throughout. All writes go through Node A.
- Node A mints the tx-token and embeds it in the CloudEvent (`cyodatxtoken`). The compute node echoes it on callbacks — `X-Tx-Token` header (HTTP) or `tx-token` metadata (gRPC). Without the echo the callback runs in a standalone transaction rather than joining T.
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
| L6 | Node B <-> PostgreSQL | TCP (proxy resolution only, not tx) |
| L7 | Node A <-> Node B | HTTP POST /internal/dispatch (NEW) |

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

Node A dispatched the CloudEvent to compute. gRPC stream breaks. Node A's dispatch call returns error or times out (gRPC keepalive). Node A detects failure -> rolls back tx-123.

Meanwhile: Compute may still be executing business logic, unaware the stream is dead. When it tries to callback (t6), it will fail.

SAFE: Node A rolls back. Compute's work is discarded (stateless).

**L5 partitions (Node A <-> PG):**

Node A is waiting for compute response. PG connection may drop. Two sub-cases:

1. *pgx connection killed by PG:* tx-123 is rolled back server-side. When compute responds and Node A tries to use the tx, pgx returns error. Node A detects and aborts.
2. *pgx connection survives (brief partition):* No PG operations happening during this phase. Transaction still alive. If partition heals before t8, everything proceeds normally.

SAFE: Either PG kills the tx, or partition heals and flow continues.

---

#### Phase 3: t6--t9 (CRUD callback through Node B, proxied to Node A)

**L3 partitions (Compute <-> Node B):**

Compute's CRUD callback cannot reach Node B. Compute gets connection error. Compute reports failure back to Node A (via the gRPC stream, if L2 is still up). Node A receives processor failure -> rolls back tx-123.

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

**Before forward sent:** Node A detects connection error, tries another peer (if available) or fails with `NO_COMPUTE_MEMBER_FOR_TAG`.

**Forward sent, waiting for response:** HTTP timeout fires. Node A returns timeout error to workflow engine -> transaction rolls back. Node B may still be dispatching -- its local dispatch will eventually complete or timeout, but the result is discarded (no one listening).

**Response lost:** Same as above -- timeout -> rollback. No split-brain because the dispatch is read-only from Node A's perspective (the entity update has not been applied yet).

SAFE: All cases lead to rollback or retry. No data corruption possible because the dispatch response must be received by Node A before it updates the entity.

---

#### Phase 5: `COMMIT_BEFORE_DISPATCH` segment-boundary partition

This phase covers the new partition windows opened by the segmented cascade described in §4.4 (variant). The boundary sits between `TX_pre.Commit` (entity durable in pre-callout state) and `TX_post.Begin` (engine resumes after dispatch returns).

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
| **Timeout / liveness** | A dispatch to a dead compute node is bounded by the callout's `responseTimeoutMs` and, behind it, by `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` — the connection is idle inside its transaction for the whole callout, not running a statement, so the statement ceiling does not apply (§3.4). What is not bounded is the inbound request itself — no deadline is derived from it and propagated downstream. | Deadline propagation via context |
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
(`internal/domain/search.FailStaleJobs`) claims any `RUNNING` job whose
heartbeat has gone silent for `CYODA_SEARCH_JOB_STALE_AFTER` via
`ClaimStale` — which atomically bumps the job's `Epoch` so concurrent
claimers obtain disjoint jobs — and marks it `FAILED` through the
epoch-fenced `UpdateJobStatus`. Every executor-side write (`Heartbeat`,
`SaveResults`, the terminal `UpdateJobStatus`) carries the epoch the
executor was started or claimed with; a store refuses a write whose epoch
does not match the job's current epoch with `spi.ErrStaleClaim`, so a
deposed executor that later recovers cannot corrupt a result set another
node has since taken over. **This milestone's disposition is
claim-then-FAIL**, not re-execution: a crashed node's async job now reaches
a terminal state instead of staying `RUNNING` forever, but it is not picked
up and re-run elsewhere in the cluster. `ClaimStale` and `ReapExpired` are
cross-tenant, called with a tenant-less context (precedent:
`ScheduledTaskStore.ScanDue`); the reaper's follow-up writes reconstruct a
per-job tenant context from the claimed job's own `TenantID`.

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
context on an externally-recorded `CANCELLED`. Node shutdown drains the
registry: every in-flight job is cancelled and marked `FAILED` (via the
same epoch-fenced write) with a safe message, so a shutdown never leaves a
job stuck `RUNNING`.

**TTL-based cleanup.** Independent of the stale-job reaper above, a
background reaper goroutine runs on `CYODA_SEARCH_REAP_INTERVAL` (default
5m) and deletes *terminal* jobs older than `CYODA_SEARCH_SNAPSHOT_TTL`
(default 1h) — `DD-12`. The PostgreSQL implementation uses `CASCADE` on the
foreign key from `search_job_results` to `search_jobs`.

**PostgreSQL schema.** Two tables in
`plugins/postgres/migrations/`: `search_jobs` holds the job record (status,
model ref, condition, point-in-time, search options, result count, timings,
`epoch`, `heartbeat_time`) and `search_job_results` holds the ordered entity
IDs, keyed `(job_id, seq)`. Both are tenant-scoped — `search_jobs` has the
composite primary key `(tenant_id, id)` and `search_job_results` carries
`tenant_id` with a composite foreign key back to it, `ON DELETE CASCADE` —
and both carry RLS policies enforcing tenant isolation.

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
`000008`, `COLLATE "C"` for byte-wise order); sqlite adds a
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
  probe — this is also what `HasEntity` in the wire response now derives
  from (`!Deleted`), so a tombstone's `hasEntity` reads uniformly across
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

Processors are dispatched via the `ExternalProcessingService` SPI. In multi-node mode, this is the `ClusterDispatcher` (see Section 4.3). Four execution modes are defined in the Cyoda model:

| Mode | Behavior |
|------|----------|
| `SYNC` | Processor executes within the current transaction. Entity data is updated in-place before the next transition. |
| `ASYNC_SAME_TX` | Executes inline in the caller's transaction, exactly as `SYNC` does. CRUD callbacks are routed back to the transaction owner. The `ASYNC` label is preserved for Cyoda Cloud configuration compatibility; execution in cyoda-go is not asynchronous. |
| `ASYNC_NEW_TX` | Processor executes sequentially within a SAVEPOINT of the parent transaction. Fire-and-forget error semantics: failure rolls back the SAVEPOINT only, parent pipeline continues. Entity mutations returned by the processor are discarded. Parent rollback discards all ASYNC_NEW_TX work. The `ASYNC` label is preserved for Cyoda Cloud configuration compatibility — execution is sequential in cyoda-go. |
| `COMMIT_BEFORE_DISPATCH` | Engine splits the cascade into two transactions around this processor. `TX_pre` flushes the pre-callout entity state and commits **before** the processor is dispatched, releasing the storage connection during the external compute window. The processor runs outside any transaction. When the processor returns, the engine opens `TX_post` on the same node, reapplies the result via `CompareAndSave` (CAS expects the txID stamped at `TX_pre`'s commit), runs subsequent SYNC processors and cascade transitions inline, then commits. CAS conflict at the boundary surfaces `ErrConflict` → `409 retryable`; entity remains durable in the pre-callout state, no engine-side retry, no automatic compensation. Companion field `startNewTxOnDispatch: bool` (default `false`, sibling on the same processor object, validator rejects `true` for any other mode) controls whether a fresh transaction context is supplied to the dispatched call for processor-side CRUD on entities other than the cascade-anchor. **Audit-trail durability change**: the existing `SMEventProcessingPaused` is recorded in `TX_pre` and durably committed at the segment boundary; the existing `SMEventStateProcessResult` is recorded in `TX_post`. No new event types are introduced. See [docs/CONSISTENCY.md](CONSISTENCY.md) §10 for visibility caveats and idempotency requirements. |

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

**Segment-boundary placement for `COMMIT_BEFORE_DISPATCH`** (§5.4): when the engine segments a cascade around a `COMMIT_BEFORE_DISPATCH` processor, the existing `SMEventProcessingPaused` is recorded in `TX_pre` (and durably committed at the segment boundary, surviving an engine crash before the dispatch returns) and the existing `SMEventStateProcessResult` is recorded in `TX_post`. **No event spans both transactions; no new event types are introduced.** Audit consumers can detect a stranded mid-cascade entity by the presence of `SMEventProcessingPaused` without a matching `SMEventStateProcessResult` for the same dispatch.

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

2. **Keep-alive:** Server sends `CalculationMemberKeepAliveEvent` at configurable interval (default 10s). Client must respond with a keep-alive within the timeout (default 30s). If not, the server considers the member dead and unregisters it.

3. **Dispatch/Response:** Server sends `EntityProcessorCalculationRequest` or `EntityCriteriaCalculationRequest`. Client processes and returns the corresponding `Response` type. Correlation is by `requestID` field in the CloudEvent payload.

4. **Leave:** Stream closes (client disconnect or server eviction). `MemberRegistry.Unregister()` is called, which fails all pending requests for that member.

### 6.3 Tag-Based Member Selection

`MemberRegistry.FindByTags(tenantID, tagsCSV)` returns a member matching the tenant whose tags overlap with the required tags (CSV comparison). If `tagsCSV` is empty, any member for that tenant matches. Selection is the first hit while ranging over a Go map, so among equally-qualified local members it is unordered rather than round-robin.

### 6.4 Response Correlation

Each dispatch request generates a unique `requestID` (TimeUUID). The dispatcher:

1. Creates a buffered channel: `member.TrackRequest(requestID) -> chan *ProcessingResponse`
2. Sends the CloudEvent to the member's stream
3. Waits on the channel with a configurable timeout

When the member responds, the streaming handler matches the response's `requestID` to the pending channel and delivers the result. If the member disconnects, `FailAllPending()` sends error responses to all waiting channels.

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

`KVTrustedKeyStore` keeps a per-node cache over the KV store and converges across the cluster on three layers: mutations publish a payload-free change ping on the `auth.trustedkeys` gossip topic (receivers re-read KV, coalescing concurrent pings); a periodic reconcile loop (`CYODA_AUTH_CACHE_RECONCILE_INTERVAL`, default 60s, jittered ±10%) rebuilds the cache from KV as the backstop when a ping drops; and `Get` reads through to KV on a cache miss. If reconciliation has not succeeded for 10× the interval — KV unavailable that long means the platform is effectively down — verification-path enumeration fails closed (trusted-key-signed grants are rejected) while `Get` switches to KV read-through, keeping admin operations on ground truth. First-party JWT validation is unaffected (in-process key source, no KV dependency).

**Deterministic KID derivation:**

```go
pubDER, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
kidHash := sha256.Sum256(pubDER)
kid := hex.EncodeToString(kidHash[:16])  // first 16 bytes of SHA-256
```

This is critical for multi-node clusters: all nodes sharing the same RSA private key produce the same KID. Any node can validate tokens issued by any other node without key synchronization.

**OBO (On-Behalf-Of) exchange:** A compute member authenticated via M2M credentials can exchange its token for an OBO token carrying the original user's tenant and identity. This allows CRUD callbacks to carry the correct authorization context.

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
main API listener and has its own authentication policy:

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
| `CYODA_BOOTSTRAP_TENANT_ID` | `default-tenant` | Tenant for bootstrap client |
| `CYODA_BOOTSTRAP_USER_ID` | `admin` | User ID for bootstrap client |
| `CYODA_BOOTSTRAP_ROLES` | `ROLE_ADMIN,ROLE_M2M` | Comma-separated roles |

### gRPC

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_GRPC_PORT` | `9090` | gRPC server listen port |
| `CYODA_KEEPALIVE_INTERVAL` | `10` | Keep-alive ping interval (seconds) |
| `CYODA_KEEPALIVE_TIMEOUT` | `30` | Keep-alive timeout before eviction (seconds) |

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
| `CYODA_TX_TOKEN_TTL` | `1m30s` | TTL of the signed transaction routing token minted on dispatch |
| `CYODA_HMAC_SECRET` (with `_FILE` variant) | (none) | Hex-encoded secret for token signing + gossip encryption (required if cluster enabled). See §4.2 for encoding details. |
| `CYODA_DISPATCH_WAIT_TIMEOUT` | `5s` | How long to poll for a compute member with matching tags |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | `30s` | HTTP timeout for cross-node dispatch forwarding |

### Search

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_SEARCH_SNAPSHOT_TTL` | `1h` | TTL for async search job results |
| `CYODA_SEARCH_REAP_INTERVAL` | `5m` | Frequency of search snapshot reaper |
| `CYODA_SEARCH_MAX_SORT_KEYS` | `16` | Max `sort`/`orderBy` keys per search request |
| `CYODA_SEARCH_ASYNC_WORKERS` | `8` | Async-search worker pool size; startup fails if `< 1` |
| `CYODA_SEARCH_ASYNC_QUEUE` | `256` | Async-search submit queue capacity beyond running workers; startup fails if `< 0` |
| `CYODA_SEARCH_ASYNC_MAX_PER_TENANT` | `8` | Max async-search jobs one tenant may hold in flight (queued + running) per node; `0` disables; startup fails if `< 0` |
| `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` | `15s` | How often a running async-search executor stamps liveness and polls for cross-node cancel/terminal status |
| `CYODA_SEARCH_JOB_STALE_AFTER` | `5m` | How long a `RUNNING` job may go without a heartbeat before the reaper claims and fails it; must be `>= 4x` the heartbeat interval |

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

**Workflow and dispatch:** spans for `workflow.execute`, `workflow.manual_transition`, `workflow.loopback`, `workflow.cascade`; `dispatch.processor`, `dispatch.criteria` and `dispatch.function` with `cyoda.dispatch.duration` and `cyoda.dispatch.count` metrics. These are active when `CYODA_OTEL_ENABLED=true`.

**Plugin-level instrumentation:** plugins are free to add their own
spans and metrics under a plugin-specific namespace. The `memory`
and `postgres` plugins do not emit custom plugin-level telemetry;
their behaviour is fully captured by the core transaction /
workflow / dispatch spans listed above. Other plugins may add
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
| Strict context deadline propagation | A deadline derived from the inbound request and inherited by every downstream operation. Today the HTTP server sets no read/write timeout and no request deadline is propagated; only dispatch enforces its own independent wall clock. |
| Idempotency keys | Client-provided keys preventing duplicate operations on retry. The `IDEMPOTENCY_CONFLICT` code is reserved, but no handler reads an `Idempotency-Key` header. |
| Trace propagation through the search pipeline | A unified search trace waterfall. The search packages emit no spans, and the async-search goroutine starts from a fresh context, severing the parent span. |
| Outbound trace propagation to external processors | End-to-end workflow tracing. Inbound gRPC trace context is extracted and dispatches are wrapped in spans, but no `traceparent` is injected into the dispatched CloudEvent or the peer-forward request. |
| Migration-runner retry tolerance for a deadlock-killed advisory lock | Being able to use `CREATE INDEX CONCURRENTLY` for an index added on an already-populated table without deadlocking the concurrent multi-node boot path. Today the migration runner holds one session-level advisory lock for a migrator's entire run with no retry on a `SQLSTATE 40P01` from a lock cycle against `CONCURRENTLY`'s own multi-phase wait, so `entities`' migration `000008` uses a plain `CREATE INDEX` (writer-blocking for the build's duration) instead — see `docs/plugins/POSTGRES.md`. Any future index-on-populated-table migration hits the same choice until this gap closes. |
| Async job re-execution after an orphan claim | An orphaned async job (owning node crashed) surviving as a result the cluster completes elsewhere. The SPI groundwork (`Heartbeat`/`ClaimStale`/`ClearResults`, epoch fencing) ships now; the interim disposition is claim-then-`FAILED` (§4.6), not re-execution. |

---

## 13. Design Decisions Log

### DD-1: HMAC Token + Separate UUID

**Context:** How to route requests to the correct transaction-owning node.

**Decision:** HMAC-signed opaque token containing `{nodeID, txRef, expiresAt}`. The `txRef` is a separate UUID used as a key into the node's local transaction map. The `nodeID` is extracted locally (no network call) for routing.

**Rationale:** The token is opaque to clients. HMAC verification is a CPU-local operation. No distributed registry lookup is needed for routing decisions.

### DD-2: Fencing Tokens Not Required

**Context:** Whether to use fencing tokens to prevent stale writes from zombie transactions.

**Decision:** Not required. The `pgx.Tx` single-owner property guarantees that only one goroutine on one node holds a physical PostgreSQL transaction.

**Rationale:** Fencing tokens exist to stop a process that believes it still owns a resource from writing after ownership has moved. That situation is unreachable here: a transaction is a connection, a connection has exactly one holder, and there is no mechanism by which two nodes come to hold the same one. The decision rests on that property alone — not on any liveness or expiry mechanism. If the owning node dies its connection drops and PostgreSQL rolls the transaction back; the ceilings in §3.4 bound how long an abandoned one can occupy a connection, but they are resource hygiene, not the reason fencing is unnecessary.

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

**Decision:** `RandomSelector` -- uniform random selection from alive candidates.

**Rationale:** Simple, stateless, no coordination needed. Load balancing across peers is acceptable for the expected cluster size. More sophisticated strategies (round-robin, least-loaded) can be added by implementing the `PeerSelector` interface.

### DD-7: Gossip Metadata for Tag Discovery

**Context:** How to find which node has a compute member with the required tags.

**Decision:** Each node publishes its compute member tags in gossip metadata, organized per tenant. Tag updates are pushed to memberlist on member join/leave and propagated via SWIM gossip.

**Rationale:** Avoids a centralized registry. Tag lookups are local memory reads against the gossip view. Convergence is within milliseconds for LAN configurations.

### DD-8: HTTP for Dispatch Forwarding

**Context:** What protocol to use for cross-node dispatch forwarding.

**Decision:** HTTP POST to `/internal/dispatch/callout` (single route for every callout kind, discriminated by `Kind` in the request body), authenticated and encrypted with AES-256-GCM AEAD (PeerAuth interface, AEADPeerAuth impl). The AEAD key is HKDF-derived from `CYODA_HMAC_SECRET`; the forwarder and handler share a `PeerAuth` seam so a future mTLS-based transport can be swapped in without changing the dispatch logic.

**Rationale:** Reuses the existing HTTP infrastructure. The dispatch payload is a single request-response pair (not a stream), making HTTP a natural fit. AEAD gives integrity, confidentiality and replay resistance (via timestamp skew + nonce cache) in one primitive. Identity is cluster-scoped rather than per-node; making it per-node is a transport change behind the `PeerAuth` seam, not a protocol change.

### DD-9: Poll-Based Wait for Missing Compute Members

**Context:** What to do when no compute member matches the required tags.

**Decision:** Poll gossip metadata every 200ms for up to `CYODA_DISPATCH_WAIT_TIMEOUT` (default 5s).

**Rationale:** Compute members may be joining. A brief wait avoids spurious failures during cluster startup or member reconnection. The 200ms interval is short enough to be responsive but does not hammer the gossip view. After the timeout, the failure is deterministic.

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
| **PG idle-in-transaction timeout** | Default 5m (`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`) | PostgreSQL aborts a transaction whose connection sits idle past it — which is what a transaction waiting on an external callout is doing. This is the authoritative bound on transaction lifetime; a processor's `responseTimeoutMs` must fit under it. |
| **Pool acquire timeout** | Default 10s (`CYODA_POSTGRES_ACQUIRE_TIMEOUT`) | An operation that cannot get a connection within it fails fast with `503 STORAGE_UNAVAILABLE` rather than queueing behind a saturated pool. Applies to writes, and to reads needing a *second* connection while the caller's transaction holds one — a point-in-time read or an async-search submit issued inside a transaction. Unbounded otherwise, so ordinary pool contention on a non-transactional read does not fail spuriously. |
| **Connection hold time** | Duration of entire flow chain (BEGIN → workflow → compute dispatch → callbacks → COMMIT) | Each in-flight transaction consumes one PG connection for its full lifetime. With 25 connections per node and 10 nodes, the cluster supports ~250 concurrent transactions. |
| **Proxy timeout** | Default 30s (configurable) | Cross-node proxy hops for CRUD callbacks must complete within this window. |
| **Dispatch forward timeout** | Default 30s (configurable) | Cross-node compute dispatch forwarding must complete within this window. |
| **Compute member response timeout** | Per-processor `responseTimeoutMs` (default 30s) | If a compute member doesn't respond within this window, the dispatch fails and the transaction rolls back. Not validated against the idle-in-transaction ceiling — a value above it means PostgreSQL aborts the transaction first (§3.4). |

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
| **Compute member disconnect** | Pending dispatch requests fail with "member disconnected." Gossip tag metadata updated within seconds. Subsequent dispatches route to other members with the same tag. | Automatic if other members exist. If no member for the tag, dispatches fail after poll timeout. |
| **nginx LB failure** | All external traffic stops. Nodes are healthy but unreachable. | LB must be restored. Nodes continue gossiping and can handle direct traffic if clients bypass the LB. |

**Single point of failure:** PostgreSQL. If PG is down, the cluster is effectively down for writes. This is by design — PG is the consistency authority. HA PostgreSQL (streaming replication with automatic failover) is the recommended mitigation.

**No split-brain:** The `pgx.Tx` single-owner property ensures that no two nodes can commit the same transaction. PostgreSQL `REPEATABLE READ` plus the application-layer SI+FCW validation (§3.7) catches conflicting concurrent writes from different transactions. There is no application-level consensus needed because PG is the sole arbiter.

### 14.5 Consistency Guarantees and Caveats

| Guarantee | Strength | Caveat |
|-----------|----------|--------|
| **Read-your-own-writes** | Strong (within a transaction) | Guaranteed by `pgx.Tx` — all reads within a transaction see its own buffered writes. Across transactions, reads are snapshot-isolated. |
| **Snapshot isolation** | Strong (SI+FCW across all plugins; see §3.7 and [docs/CONSISTENCY.md](CONSISTENCY.md)) | Commit-time conflict detection may abort with `ErrConflict` (40001 / 40P01 on PostgreSQL). The application retries. Under high contention, retry storms are possible. |
| **Cross-node consistency** | Strong (PG is the authority) | All nodes share the same PG instance. There is no eventual consistency between nodes — they all see the same data at the same isolation level. Gossip metadata (node registry, compute tags) is eventually consistent with sub-second convergence. |
| **Temporal consistency** | Strong (point-in-time queries) | `GetAsAt` returns the entity as it was at a specific timestamp. Accuracy depends on PG clock precision (microsecond) and correct use of `transaction_time` vs `wall_clock_time`. |
| **Commit ambiguity** | **Gap** (§12) | If the network partitions between Node A and PG at COMMIT time, Node A cannot determine whether PG committed or not. The client may see a false failure for a transaction that actually committed. |
| **Idempotency** | **Gap** (§12) | Client retries after timeout may create duplicate entities. There is no built-in idempotency key mechanism; clients must handle deduplication at the application level. |

### 14.6 Operational Limits

| Parameter | Default | Hard Limit | Notes |
|-----------|---------|------------|-------|
| PG connections per node | 25 | Configurable, bounded by PG `max_connections` | Each in-flight transaction holds one connection. |
| Gossip metadata size | ~100 bytes per node (without tags) | memberlist `MetaMaxSize` = 512 bytes | With many tenants and many tags, metadata could exceed 512 bytes. Monitor and alert. |
| Search snapshot TTL | 1 hour | Configurable | Snapshots older than TTL are reaped. Increase for long-running batch workflows. |
| Transaction lifetime | 5 minutes idle | Configurable | Enforced by PostgreSQL via `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`. Processor `responseTimeoutMs` must fit under it. |
| Max cascade depth | 100 | Hardcoded | Total cascade steps across all states in one engine invocation. |
| Max state visits per workflow | 10 | Configurable | Prevents infinite loops in workflow cascading. Increase for deeply nested state machines. |
| HTTP body limit | 10 MB | Hardcoded in entity handler | Increase requires code change. |
| gRPC keep-alive interval | 10 seconds | Configurable | Shorter intervals detect compute member failure faster but increase network overhead. |
| Dispatch poll interval | 200 ms | Hardcoded | Polls local gossip metadata (no network I/O). Low overhead. |
| Dispatch wait timeout | 5 seconds | Configurable | Time to wait for a compute member when none is available for the required tag. |
