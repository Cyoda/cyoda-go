# Cyoda-Go — Architecture & Feature Overview

This document describes the system architecture and implemented features of Cyoda-Go, a single-node Go digital twin of the [Cyoda](https://cyoda.com) EDBMS platform.

## System Architecture

Cyoda-Go is a **modular monolith** organized by domain-driven design boundaries. Every cross-cutting concern is defined as a Go interface (SPI) before any implementation, with components depending only on abstractions.

```
┌──────────────────────────────────────────────────┐
│  REST API (OpenAPI spec-first, oapi-codegen)     │
│  gRPC Server (CloudEventsService)                │
├──────────────────────────────────────────────────┤
│  Auth Middleware ─► UserContext injection        │
│  Recovery Middleware ─► Error handling (RFC 9457)│
├──────────────────────────────────────────────────┤
│  Domain Handlers                                 │
│  Entity · Model · Search · Audit · Messaging     │
├──────────────────────────────────────────────────┤
│  Workflow Engine                                 │
│  State machine execution · Criteria evaluation   │
│  Processor dispatch (local or gRPC)              │
├──────────────────────────────────────────────────┤
│  SPI Layer (Persistence Abstraction)             │
│  StoreFactory ─► EntityStore, ModelStore, etc.   │
├──────────────────────────────────────────────────┤
│  Storage Backend (selected at startup)           │
├────────────────────┬─────────────────────────────┤
│  In-Memory Store   │  PostgreSQL Store           │
│  (SI+FCW)          │  (SI+FCW: RR + FCW)         │
└────────────────────┴─────────────────────────────┘
```

### Domain Modules

| Module | Responsibility |
|--------|---------------|
| **Entity** | CRUD, versioning, batch operations, point-in-time retrieval |
| **Model** | Schema discovery from sample data, lock/unlock lifecycle, validation, dynamic extension |
| **Workflow** | State machine execution, automated/manual transitions, criteria evaluation, processor dispatch |
| **Search** | Predicate-based queries (sync and async), pagination, point-in-time search |
| **Audit** | Entity change history, state machine event trail (13 event types) |
| **Messaging** | Edge message storage with streaming payloads |
| **Auth** | JWT issuance, JWKS, M2M clients, OBO token exchange (RFC 8693), trusted keys |
| **gRPC** | Bidirectional streaming for externalized processors and criteria via CloudEvents |
| **Cluster** | Connected calculation member registry, tag-based routing |

### Persistence

Two storage backends, selected at startup via `CYODA_STORAGE_BACKEND`:

**In-Memory** — Thread-safe maps with `sync.RWMutex`, tenant-partitioned, append-only entity versioning. Transactions use Snapshot Isolation with First-Committer-Wins (SI+FCW), with conflict detection at commit.

**PostgreSQL** — `REPEATABLE READ` snapshot isolation plus application-layer first-committer-wins (SI+FCW), bi-temporal entity versioning (`valid_time`, `transaction_time`, `wall_clock_time`), automatic schema migrations, row-level security policies. A `Querier` interface abstracts `pgxpool.Pool` and `pgx.Tx` so stores work transparently inside and outside transactions. See [docs/CONSISTENCY.md](docs/CONSISTENCY.md) for the full contract.

### Multi-Tenancy

Tenant isolation is structural, not conventional. `TenantID` is a named Go type (not a bare string). Every request carries a resolved `UserContext` with tenant identity, injected by auth middleware. The `StoreFactory` extracts tenant from context and returns a tenant-scoped view — there is no API path where tenant is absent or bypassable. A `SYSTEM` tenant exists for platform-level shared data.

### Transactions

ACID transactions with Snapshot Isolation and First-Committer-Wins (SI+FCW):

1. **Begin** — snapshot time captured, empty read/write sets
2. **Read** — buffer first (read-your-own-writes), then snapshot view
3. **Write** — buffered, not yet visible to other transactions
4. **Commit** — conflict detection against concurrent writes, atomic flush
5. **Rollback** — discard buffer

Three processor execution modes control transactional participation:

| Mode | Transaction | On Failure |
|------|-------------|------------|
| `SYNC` | Caller's transaction | Rollback all |
| `ASYNC_SAME_TX` | Caller's transaction | Rollback all |
| `ASYNC_NEW_TX` | Independent transaction | Caller unaffected |

### Workflow Engine

Entities are finite state machines. On creation, the engine selects a matching workflow by evaluating criteria, sets the initial state, then cascades through automated transitions until a stable state is reached.

**Criteria types:** Simple (JSONPath + operator), Lifecycle (metadata fields), Group (AND/OR composition), Array (positional matching), Function (delegated to external compute nodes via gRPC).

**23 operators:** equality, comparison, string matching, case-insensitive variants, pattern matching, between, null checks.

**Audit trail:** 13 event types track the full state machine narrative — workflow selection, transitions attempted/made/denied, processor results, cancellation.

### gRPC & Externalized Processing

Cyoda-Go implements Cyoda's `CloudEventsService` with bidirectional streaming. Calculation members (external compute nodes) connect via gRPC, register with capability tags, and receive processor/criteria dispatch requests as CloudEvents.

**Protocol:** Join → Greet → Keep-alive loop → Processor/Criteria requests → Responses. Tag-based routing matches processor requirements to member capabilities. Members must authenticate with `ROLE_M2M` JWT tokens.

### Authentication

Two modes: **mock** (all requests auto-authenticated, zero setup) and **jwt** (real OAuth 2.0 with RS256 JWT tokens).

JWT mode provides:
- Token issuance via `client_credentials` grant
- On-behalf-of token exchange (RFC 8693)
- JWKS endpoint for public key discovery
- M2M technical user management
- Trusted external key registration (persisted via KV store)
- Bootstrap M2M client at startup (solves chicken-and-egg)

### Error Handling

Three-tier classification: **Operational** (4xx, full detail to client), **Internal** (5xx, generic message + correlation ticket), **Fatal** (5xx, marks health unhealthy). Responses follow RFC 9457 Problem Details format. Error detail level controlled by `CYODA_ERROR_RESPONSE_MODE` (sanitized or verbose).

---

## Feature List

### Entity Management
- Create, read, update, delete entities (single and batch)
- JSON and XML input formats
- Entity versioning — every mutation creates an immutable version
- Soft delete with deletion markers (preserves history)
- Point-in-time retrieval (`?pointInTime=<ISO8601>`)
- Entity statistics (count, state distribution per model)
- Optimistic concurrency via `If-Match` header (transaction ID based) — layered on top of always-on SI+FCW at the transaction layer

### Entity Models
- Schema discovery from sample JSON/XML data
- Successive imports merge via union (fields accumulate, types widen)
- Export as JSON_SCHEMA or SIMPLE_VIEW (lossless)
- Lock/unlock lifecycle (lock required before entity creation)
- Delete (only if unlocked and no entities)
- Dynamic model extension on ingestion via `changeLevel`:
  - `STRUCTURAL` — new fields allowed
  - `TYPE` — leaf types can widen
  - `ARRAY_ELEMENTS` — array element types can widen
  - `ARRAY_LENGTH` — only array widths change
- Validation against locked schema

### Workflow Engine
- Entities as finite state machines with automated and manual transitions
- Multiple workflows per model with criteria-based selection
- Transition criteria (JSONPath predicates, lifecycle conditions, group logic, external functions)
- Processor execution on transitions (SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX)
- Cascade loop — automated transitions fire until stable state
- Loopback — re-evaluate automation from current state on data update
- Workflow import/export (MERGE, REPLACE, ACTIVATE modes)
- Loop protection (configurable max state visits)

### Search
- Direct synchronous search with predicate conditions
- Async snapshot search with job lifecycle (submit, poll, retrieve, cancel)
- 23 operators (equality, comparison, string, case-insensitive, pattern, range, null)
- Group conditions with AND/OR nesting
- Point-in-time search across entity versions
- Pagination (offset/limit)

### Audit Trail
- Entity change history derived from version metadata (CREATED, UPDATED, DELETED)
- State machine audit with 13 event types tracking full workflow narrative
- Filterable by event type, severity, time range, transaction ID
- Cursor-based pagination

### Edge Messaging
- Message creation with AMQP-aligned headers and arbitrary metadata
- Streaming payload storage (file-backed, avoids heap bloat)
- Single and batch delete

### gRPC Integration
- Cyoda-compatible `CloudEventsService` (bidirectional streaming)
- Calculation member registration with capability tags
- Server-initiated keep-alive with configurable interval/timeout
- Processor and criteria dispatch to external compute nodes
- Entity, model, and search operations over gRPC
- Transaction context propagation (gRPC callbacks can join caller's transaction)

### Authentication & Authorization
- OAuth 2.0 token issuance (`client_credentials`)
- On-behalf-of token exchange (RFC 8693)
- JWKS public key endpoint
- M2M technical user management (create, delete, reset secret)
- JWT key pair management (issue, invalidate, reactivate, delete)
- Trusted external key registration (persistent across restarts)
- Mock mode for zero-setup development

### Multi-Tenancy
- Structural tenant isolation at every layer
- Named `TenantID` type (compile-time safety)
- Per-tenant data partitioning in all stores
- SYSTEM tenant for platform-level shared data
- gRPC member isolation (members scoped to their tenant)

### Temporal Integrity
- Bi-temporal entity versioning (valid time, transaction time, wall clock time)
- Point-in-time retrieval for single entities and collections
- Point-in-time search
- Version history with change metadata

### Pluggable Persistence
- In-memory backend (zero dependencies, sub-millisecond)
- PostgreSQL backend (durable, SI+FCW via `REPEATABLE READ` + first-committer-wins, automatic migrations)

---

## REST API Surface

| Area | Endpoints |
|------|-----------|
| **Health** | `GET /health` |
| **Entity CRUD** | `POST/GET/PUT/DELETE /entity/...` |
| **Entity Stats** | `GET /entity/stats/...` |
| **Model Management** | `POST/GET/DELETE /model/...`, lock, unlock, changeLevel, validate |
| **Workflow** | `GET/POST /model/.../workflow/{export,import}` |
| **Search** | `POST /search/{direct,async}/...`, `GET /search/async/{jobId}` |
| **Audit** | `GET /audit/entity/{entityId}`, workflow finished event |
| **Messaging** | `POST/GET/DELETE /message/...` |
| **Auth** | `POST /oauth/token`, `GET /.well-known/jwks.json`, key/trusted/M2M management |
| **Account** | `GET /account`, subscriptions |
| **Cluster** | `GET /cluster/members/calculation/...` |
| **Admin** | `GET/POST /admin/log-level` |

## gRPC API Surface

```protobuf
service CloudEventsService {
  rpc startStreaming(stream CloudEvent) returns (stream CloudEvent);
  rpc entityModelManage(CloudEvent) returns (CloudEvent);
  rpc entityManage(CloudEvent) returns (CloudEvent);
  rpc entityManageCollection(CloudEvent) returns (stream CloudEvent);
  rpc entitySearch(CloudEvent) returns (CloudEvent);
  rpc entitySearchCollection(CloudEvent) returns (stream CloudEvent);
}
```

All operations use CloudEvent envelopes with JSON payloads, matching the Cyoda Cloud protocol.
