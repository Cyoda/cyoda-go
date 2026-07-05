# Cyoda-Go — Feature & API Surface Inventory

This document inventories every feature implemented in cyoda-go and lists the REST and gRPC API surfaces. It is the answer to "what can this thing do" and "where does endpoint X live."

For **architecture** — modular layout, storage plugin contract, transaction model, multi-node routing, network partition analysis — see [`docs/ARCHITECTURE.md`](ARCHITECTURE.md).

For **product context** — value proposition, target use cases, scale envelope, cost model — see [`docs/PRD.md`](PRD.md).

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
- Processor execution on transitions (SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH) — see [`docs/PROCESSOR_EXECUTION_MODES.md`](PROCESSOR_EXECUTION_MODES.md)
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
- Entity change history derived from version metadata (CREATE, UPDATE, DELETE)
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
- OIDC provider per-tenant registry (register, list, get, update, invalidate, reactivate, delete, reload). External-IdP JWT validation via chained multi-issuer validator. Cross-node cache eviction via cluster broadcast.
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
- SQLite backend (single-file persistent storage; no external server; embedded SQL migrations)
- PostgreSQL backend (durable, SI+FCW via `REPEATABLE READ` + first-committer-wins, automatic migrations; required for cyoda multi-node cluster mode)

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
| **Auth** | `POST /oauth/token`, `GET /.well-known/jwks.json`, key + trusted-key management |
| **M2M Clients** | `GET/POST /clients`, `DELETE /clients/{clientId}`, `PUT /clients/{clientId}/secret` |
| **Account** | `GET /account`, subscriptions |
| **Admin** | `GET/POST /admin/log-level`, `GET/POST /admin/trace-sampler` |

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
