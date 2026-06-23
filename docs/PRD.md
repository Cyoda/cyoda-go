# Cyoda-Go: Product Requirements Document

**Version:** 2.1
**Date:** 2026-04-18
**Status:** Current as of 2026-04-18 (Helm provisioning shipped in PR #60; docs reconciled against commit at branch tip).

---

## 1. Product Vision and Target Use Case

Cyoda-Go is an Entity Database Management System (EDBMS) — a database engine where the first-class abstraction is not a row or document but a *stateful entity* with schema, lifecycle, temporal history, and transactional integrity. Storage is provided by pluggable backends; the stock binary ships with in-memory (default), SQLite, and PostgreSQL plugins, and third-party plugins can be compiled in to target other storage engines.

### Target Applications

High-complexity, high-consistency enterprise domains where correctness is non-negotiable:

- **Embedded subledgers for fintech and vertical SaaS** — double-entry ledgers for wallets, BNPL, lending, escrow, and remittance with FSM-enforced journal entries.
- **Loan origination and servicing** — multi-year loan entities spanning application, underwriting, disbursement, repayment, modification, default, and recovery, with retroactive corrections and point-in-time disclosure reconstruction.
- **Cross-border payment routing** — multi-leg settlement flows requiring strict state coordination across internal accounts and external clearing networks.
- **Trade lifecycle and surveillance** — order and execution state machines with bi-temporal reconstruction of order books for MiFID II, CAT, SEC Rule 613, and Reg BI.
- **Insurance claims with reserves and sub-claims** — hierarchical claim entities with retroactively-adjustable reserves, automated rule cascades, manual adjuster interventions, and quarter-end snapshots for IFRS 17 and Solvency II.
- **KYC, AML, and financial-crime case management** — customer lifecycle state machines with externalized sanctions screening, document OCR, and ML risk scoring under one transactional boundary.
- **Governed agentic AI workflows** — AI-driven business actions expressed as gated state transitions on entity workflows, with policy-as-code criteria, externalized agent processors, and per-version capture of action, reasoning, and context.
- **Digital twin orchestration** — behavioral clones of production Cyoda systems for rapid semi-automated application development using agentic engineering techniques, as well as scenario testing with high request volumes.

### Scale Profile

Scale envelope depends on the active storage plugin:

- **`memory`** — single process, bounded by host RAM.
- **`sqlite`** — single node, persistent; throughput bounded by local
  disk. No clustering.
- **`postgres`** — small compute clusters (3–10 stateless Go nodes)
  behind a load balancer, sharing a primary PostgreSQL. Active-active
  HA; any node serves any request. Write throughput bounded by the
  PostgreSQL primary.
- **`cassandra`** (commercial) — multi-cluster, horizontal write
  scale-out without a single-primary bottleneck.

> **Storage plugin architecture.** Cyoda-Go's storage layer is a plugin
> system defined by the stable `cyoda-go-spi` module (stdlib-only Go
> interfaces and value types). A running binary has exactly one active
> plugin, selected at startup via `CYODA_STORAGE_BACKEND`. The stock
> `cyoda-go` binary ships with three open-source plugins:
>
> - **`memory`** (default) — ephemeral, microsecond-latency concurrency
>   control for tests and high-throughput digital-twin workloads.
> - **`sqlite`** — persistent, zero-ops single-node storage for desktop,
>   edge, and containerised single-node production.
> - **`postgres`** — durable multi-node storage; works against any
>   managed PostgreSQL 14+ platform.
>
> A commercial `cassandra` plugin is also available from Cyoda for
> deployments that need horizontal write scalability beyond what a
> single-primary PostgreSQL can provide — see
> [cyoda.com](https://www.cyoda.com) and use its contact page.
>
> Third-party plugins (Redis, ScyllaDB, FoundationDB, etc.) can be
> authored against `cyoda-go-spi` and compiled into a custom binary via
> a blank import. See `docs/ARCHITECTURE.md` for the plugin contract,
> `docs/plugins/` for per-plugin specifics, and `docs/CONSISTENCY.md`
> for the isolation guarantee shared across all backends.

### Core Value Proposition

Cyoda-Go delivers two properties that are usually traded off against
each other: **zero-compromise transactional safety** and **immutable
bi-temporal history at current-read performance**. Both are uniform
across every storage plugin.

#### 1. Zero-compromise transactional safety

Every storage plugin delivers the same isolation contract: **Snapshot
Isolation with First-Committer-Wins on entity-level conflicts.** This
guarantees:

- **Strict read-your-own-writes** — within a transaction, reads always
  reflect prior writes from that transaction, even before commit.
- **Snapshot isolation** — concurrent transactions see a consistent
  snapshot taken at `Begin`; a concurrent committer's writes are
  invisible to us until we commit.
- **First-committer-wins on entity-level conflicts** — two transactions
  contending on the same entity: exactly one commits, the other
  aborts with `spi.ErrConflict` and can retry.
- **No separate coordination daemon** — cyoda-go does not run its own
  ZooKeeper, etcd, or Raft cluster for transaction coordination. Open-
  source plugins achieve the contract against their storage engine's
  own primitives (PostgreSQL `REPEATABLE READ` + row-level locks +
  commit-time read-set validation; application-layer committed-log
  tracking in memory / sqlite). The commercial Cassandra plugin
  coordinates transactions plugin-internally. In every case the
  deployment footprint is: cyoda-go + your chosen storage — nothing
  else to operate.

The uniform contract across backends is a deliberate design choice:
application code sees the same concurrency semantics regardless of
which storage plugin is active. See `docs/CONSISTENCY.md` for the
full contract, the phantom-read caveat, the operational rule for
workflow authors, and the isolation-level taxonomy.

#### 2. Immutable bi-temporal history as a first-class property

Every mutation to an entity produces an **immutable version**. Prior
versions are never overwritten and never deleted — soft-delete itself
is a version in the chain, reversible without data loss. The version
chain *is* the audit trail; there is no separate audit table that can
drift from state.

Every version carries two timestamps:

- **`valid_time`** — when the fact was true in the domain.
- **`transaction_time`** — when the system recorded it.

This bi-temporal model answers both "what did we know, and when?" and
"what was true, and when?" — the distinction that compliance,
reconciliation, and historical reporting workloads require. Correcting
a past fact adds a new version with earlier `valid_time` and current
`transaction_time`; the original recording remains visible for audit.

**Point-in-time reads run at the same performance class as current
reads.** Current-state reads are primary-key lookups on a
materialised `entities` table (one row per live entity). Historical
reads (`GetAsAt`, `GetAllAsAt`) are indexed seeks against a dedicated
bi-temporal composite index (`idx_ev_bitemporal` on
`(tenant_id, entity_id, valid_time DESC, transaction_time DESC)` in
the postgres plugin). Both are constant-time index operations — no
version-chain scan, no rebuild, no reconstruction from an event log.

This combination — transactional safety with no phantom-wide
trade-offs, plus audit-grade history with no performance penalty —
is what an Entity Database Management System is for. Systems that
bolt on audit trails after the fact typically pay either a write-path
tax (double-write to an audit table, eventually consistent), a
read-path tax (reconstruct state from an event log on every historical
read), or both. Cyoda-Go pays neither: history is the native storage
model, and the current-state projection is the cache.

### Cost Model

Cost shape by plugin:

- `memory` / `sqlite`: zero infra. The binary is the deployment.
- `postgres`: a standard HA PostgreSQL instance (managed or
  self-hosted) plus a small number of stateless Go binaries behind a
  load balancer. No ZooKeeper, no etcd, no Kafka, no distributed cache.
- `cassandra` (commercial): a Cassandra cluster plus stateless Go
  binaries. Aligned with the Cassandra operational model; contact
  Cyoda for a sizing conversation.

---

## 2. EDBMS Core

### Entities as State Machines

Every entity in Cyoda-Go is a JSON document governed by a finite state machine. An entity is not inert data — it has a current state, follows defined transitions, and enforces lifecycle rules through the workflow engine.

| Property | Description |
|----------|-------------|
| **Identity** | UUID, assigned on creation |
| **Model** | Schema reference (name + version) — entities must conform to a locked model |
| **State** | Current FSM state, managed by the workflow engine |
| **Data** | Arbitrary JSON payload, validated against the model schema |
| **Temporal History** | Append-only version chain — every mutation creates an immutable version |
| **Soft Delete** | Default deletion mode — marks the entity as DELETED, preserving full version history for audit and temporal queries. Reversible via undeletion (planned: [#66](../../issues/66)). |
| **Physical Delete** | A separate process that permanently removes soft-deleted entities and all associated data (versions, audit events), giving fine control to suit compliance requirements. (Planned: [#65](../../issues/65)) |

### Entity Models

Models define the structural schema that entities conform to. They are discovered from sample data, not declared upfront.

**Discovery:** Import sample JSON (or XML) data. The engine infers a tree-structured schema — field names, types, nesting, arrays. Successive imports merge via union: fields accumulate, types widen.

**Lifecycle:**

```
UNLOCKED ──lock──► LOCKED ──unlock──► UNLOCKED
                     │
                     ▼
              Entities may be created
```

- Models must be locked before entities can be created against them
- Locked models cannot be unlocked while entities exist
- Models can only be deleted when unlocked and empty

**Change Levels** — dynamic model extension on ingestion (post-lock):

| Level | Allows |
|-------|--------|
| `STRUCTURAL` | New fields added to schema |
| `TYPE` | Leaf type widening (e.g., int to float) |
| `ARRAY_ELEMENTS` | Array element type widening |
| `ARRAY_LENGTH` | Array width changes only |

**Export Formats:** `JSON_SCHEMA` (standard JSON Schema) and `SIMPLE_VIEW` (lossless internal representation).

### Temporal Integrity

The persistence layer maintains bi-temporal entity versioning:

| Dimension | Semantics |
|-----------|-----------|
| `valid_time` | When the entity version became the "current" truth |
| `transaction_time` | When the version was committed to the database |
| `wall_clock_time` | Physical wall clock at version creation |

**Point-in-time retrieval:** Any entity (or collection) can be queried as it existed at a specific timestamp via `?pointInTime=<ISO8601>`. This applies to single-entity reads, collection reads, and search operations.

**Version history:** Every mutation (create, update, delete) appends a new immutable version. The full history is queryable through the audit trail.

---

## 3. Workflow Engine

The workflow engine is the core differentiator. It enforces that entities are not passive documents but active state machines with auditable, deterministic lifecycle behavior.

### Finite State Machine

Each entity follows a workflow — a directed graph of states connected by transitions. On entity creation, the engine:

1. Selects a matching workflow by evaluating workflow-level selection criteria
2. Places the entity in the workflow's `initialState`
3. Cascades through automated transitions until a stable state is reached (no further automated transition criteria are satisfied)

### Transition Types

| Type | Trigger | Gating |
|------|---------|--------|
| **Automated** | Fires on state entry when criteria are met | Criteria evaluation |
| **Manual** | Explicit API call (`POST /entity/{entityId}/{transition}`) | Criteria evaluation |

#### Loopback

Loopback is an operation, not a transition type. A client can invoke
`PUT /entity/{id}?loopback` to re-evaluate the entity's automated
transitions from its current state without forcing a manual transition.
Useful for retriggering workflow logic after the entity's data has
changed via a non-workflow path (e.g. an external processor's
callback), or after external preconditions that criteria depend on
have changed.

### Criteria Types

| Type | Evaluation | Description |
|------|-----------|-------------|
| **Simple** | Local | JSONPath expression + operator + value against entity data |
| **Lifecycle** | Local | Predicate on entity metadata (state, creationDate, previousTransition) |
| **Group** | Local | AND/OR composition with arbitrary nesting depth |
| **Array** | Local | Positional matching against array elements |
| **Function** | Remote | Delegated to external compute node via gRPC CloudEvents |

### Processor Execution Modes

Processors execute during transitions — they transform entity data, perform side effects, or create/modify other entities.

| Mode | Transaction Scope | On Failure | Use Case |
|------|-------------------|------------|----------|
| `SYNC` | Caller's transaction | Rollback all | Validation, enrichment |
| `ASYNC_SAME_TX` | Caller's transaction | Rollback all | Multi-step processing that must be atomic |
| `ASYNC_NEW_TX` | Independent transaction | Caller unaffected | Fire-and-forget side effects |

### Cascade Behavior

Processors may create or mutate other entities, triggering further workflow traversals within the same (or new) transaction as appropriate. Loop protection is enforced via configurable maximum state visits per entity per cascade.

### Workflow Management

- **Import/Export** via REST API
- **Modes:** `MERGE` (additive), `REPLACE` (full swap), `ACTIVATE` (replace same-named + deactivate the rest; `active` on incoming is importer-controlled)
- **Multiple workflows per model** — workflow-level selection criteria determine which workflow applies to a given entity

### Audit Trail

12 event types track the full state machine narrative:

- Workflow selection (selected, not found)
- Transition attempts (attempted, made, denied, failed)
- Processor execution (dispatched, succeeded, failed)
- Criteria evaluation results
- Cancellation events

Filterable by event type, severity, time range, transaction ID. Cursor-based pagination.

---

## 4. Transaction Model

### ACID Guarantees

Cyoda-Go provides ACID transactions with a specific, uniform isolation
contract across all plugins: **Snapshot Isolation with
First-Committer-Wins on entity-level conflicts** (SI+FCW). This is the
"I" in ACID — it is not full ACID-Serializable, and the distinction
matters (see `docs/CONSISTENCY.md` for the phantom-read caveat and the
operational rule for workflow authors). Each storage plugin supplies
its own `TransactionManager` that realises this contract against its
engine's primitives:

| Plugin | Engine primitives | Handle |
|--------|-------------------|--------|
| **memory** | Application-layer committed-log scan; entity-level read set + write set tracking | In-process `Transaction` struct |
| **sqlite** | Application-layer first-committer-wins over a single-writer SQLite transaction | In-process coordinator + local `sql.Tx` |
| **postgres** | PostgreSQL `REPEATABLE READ` + row-level locks + commit-time read-set validation; conflicts surface as `spi.ErrConflict` | In-process lifecycle tracker; `pgx.Tx` held per-statement inside stores |
| **cassandra** (commercial) | Proprietary mechanism against Cassandra primitives, delivering the same SI+FCW contract | Plugin-managed |

The memory and sqlite plugins share an application-layer
first-committer-wins implementation; the commercial Cassandra plugin
implements the same contract against its own primitives.

### Transaction Lifecycle

```
BEGIN ──► READ/WRITE ──► COMMIT
  │                        │
  │         conflict ◄─────┘
  │            │
  └──► ROLLBACK ◄──── timeout (TTL reaper)
```

1. **Begin** — Snapshot established. Each plugin captures its own snapshot primitive (committed-log watermark for memory/sqlite; PostgreSQL transaction snapshot under `REPEATABLE READ`).
2. **Read** — Check transaction buffer first (read-your-own-writes), then snapshot view.
3. **Write** — Buffered in transaction. Not visible to other transactions until commit.
4. **Commit** — Entity-level read-set validation at commit time. Conflicts surface as `spi.ErrConflict` (retryable).
5. **Rollback** — Discard buffer; release plugin-level resources.

### Read-Your-Own-Writes

Within a transaction, all reads reflect prior writes from that transaction, even before commit. This is critical for processor cascade correctness — a processor that creates entity B must be able to read entity B in the same transaction.

**Implementation:** The transaction maintains a write buffer. Read operations check the buffer before the underlying store. This holds uniformly across all backends.

### Conflict Detection

Entity-level read-set validation at commit time; conflicts surface as
`spi.ErrConflict` (retryable, returned to the client as
`CONFLICT` / HTTP 409). The mechanism differs per plugin — memory and
sqlite scan a committed-transaction log for intersecting write sets;
postgres uses row-level locks plus a commit-time re-validation pass;
the commercial cassandra plugin uses its proprietary coordinator —
but the contract is the same. See `docs/CONSISTENCY.md` for depth.

### Transaction Timeout and Reaper

Transactions have a configurable TTL (default: 60 seconds, set via `CYODA_TX_TTL`). A background reaper goroutine periodically scans for expired transactions and rolls them back. For the PostgreSQL plugin, the TTL should align with the server-side `idle_in_transaction_session_timeout`.

### Multi-Node Transaction Affinity

In a multi-node cluster running the **postgres plugin**, the `pgx.Tx`
handle lives in a single node's process memory. The transaction token
encodes which node owns the transaction (see Section 9). All
subsequent requests for that transaction are routed to the owning
node — no distributed transaction coordination inside cyoda-go.

If the owning node dies, the transaction dies. PostgreSQL
automatically rolls back the connection. The client receives
`TRANSACTION_NODE_UNAVAILABLE` and must retry from scratch. Trading
this failure mode for operational simplicity (no Paxos, no Raft, no
ZooKeeper in the cyoda-go process) is a deliberate choice — see §9.

The commercial **Cassandra plugin** takes a different approach: the
plugin coordinates transactions across the cluster, so a transaction
is not pinned to a single owning node and does not fail when any one
node becomes unavailable mid-transaction. This removes the
`TRANSACTION_NODE_UNAVAILABLE` failure mode and is one of the
operational advantages of the Cassandra plugin over the postgres
plugin in high-availability deployments. The coordination mechanism
is plugin-internal; contact [Cyoda](https://www.cyoda.com) for
architectural details.

---

## 5. Multi-Tenancy

Tenant isolation in Cyoda-Go is structural, not conventional. There is no code path where tenant is absent, optional, or bypassable.

### Design Principles

- `TenantID` is a named Go type (not a bare `string`), providing compile-time safety
- Every request carries a resolved `UserContext` with tenant identity, injected by auth middleware
- The `StoreFactory` extracts tenant from context and returns a tenant-scoped view
- Persistence implementations partition by tenant at the storage level — not via application-level filtering

### Tenant Hierarchy

| Tenant | Access |
|--------|--------|
| **Regular tenant** | Own data only, plus SYSTEM-owned shared data (read-only) |
| **SYSTEM tenant** | Read/write own data; read all tenants' data for administration |

Cross-tenant data access is impossible for regular tenants, even with knowledge of entity IDs.

### gRPC Member Isolation

Calculation members (external compute nodes) are scoped to their authenticating tenant. A member connected under tenant A cannot receive dispatch requests for tenant B's entities.

---

## 6. Search

### Synchronous (Direct) Search

`POST /search/{entityName}/{modelVersion}` — Evaluates predicate conditions against entity data and returns results immediately.

### Asynchronous (Snapshot) Search

Snapshot search provides a job-based lifecycle for longer-running queries:

```
SUBMIT ──► RUNNING ──► SUCCESSFUL ──► (retrieve results) ──► DELETE
                  │
                  └──► FAILED / CANCELLED
```

- `POST /search/snapshot` — Submit search, receive job ID
- `GET /search/async/{jobId}` — Poll status
- Retrieve results with pagination
- `DELETE /search/async/{jobId}` — Cancel/cleanup

**Multi-node persistence:** In PostgreSQL mode, snapshot jobs and results are stored in `search_jobs` and `search_job_results` tables. Any node can create or retrieve snapshots. In memory mode, snapshots are node-local.

**Point-in-time:** The job captures `pointInTime` at submission (caller-specified or wall clock). Result entity IDs are stored; entity data is re-fetched at the job's point-in-time on retrieval.

### Predicate Operators (26)

| Category | Operators |
|----------|-----------|
| **Equality** (4) | `EQUALS`, `NOT_EQUAL`, `IS_NULL`, `NOT_NULL` |
| **Comparison** (6) | `GREATER_THAN`, `LESS_THAN`, `GREATER_OR_EQUAL`, `LESS_OR_EQUAL`, `BETWEEN`, `BETWEEN_INCLUSIVE` |
| **String** (8) | `CONTAINS`, `NOT_CONTAINS`, `STARTS_WITH`, `NOT_STARTS_WITH`, `ENDS_WITH`, `NOT_ENDS_WITH`, `MATCHES_PATTERN`, `LIKE` |
| **Case-Insensitive** (8) | `IEQUALS`, `INOT_EQUAL`, `ICONTAINS`, `INOT_CONTAINS`, `ISTARTS_WITH`, `INOT_STARTS_WITH`, `IENDS_WITH`, `INOT_ENDS_WITH` |

The four category subtotals sum to 26 (4 + 6 + 8 + 8).

Two additional operators, `IS_CHANGED` and `IS_UNCHANGED`, are planned
— they are reachable in the API and routed through the predicate
machinery, but currently return `"operator X not implemented"` at
runtime until the feature lands. They are excluded from the 26-count
above.

### Condition Types

| Type | Description |
|------|-------------|
| **Simple** | JSONPath + operator + value against entity data |
| **Lifecycle** | Predicate on entity metadata (state, creationDate, previousTransition) |
| **Group** | AND/OR composition with arbitrary nesting |
| **Array** | Positional matching against array elements |
| **Function** | Delegated to external compute node via gRPC |

---

## 7. Externalized Processing

### Protocol: gRPC CloudEvents

External computation nodes (calculation members) connect via bidirectional gRPC streaming using a CloudEvent envelope protocol.

### Connection Lifecycle

```
Client ──JoinEvent──► Server
Server ──GreetEvent──► Client (assigns memberId)
         ──keep-alive loop──
Server ──ProcessorRequest──► Client
Client ──ProcessorResponse──► Server
Server ──CriteriaRequest──► Client
Client ──CriteriaResponse──► Server
```

### CloudEvent Types

| Event Type | Direction | Purpose |
|------------|-----------|---------|
| `CalculationMemberJoinEvent` | Client to Server | Register with capability tags |
| `CalculationMemberGreetEvent` | Server to Client | Acknowledge, assign memberId |
| `CalculationMemberKeepAliveEvent` | Bidirectional | Liveness monitoring |
| `EntityProcessorCalculationRequest` | Server to Client | Dispatch processor execution |
| `EntityProcessorCalculationResponse` | Client to Server | Return processed entity |
| `EntityCriteriaCalculationRequest` | Server to Client | Dispatch criterion evaluation |
| `EntityCriteriaCalculationResponse` | Client to Server | Return evaluation result |
| `EventAckResponse` | Bidirectional | Acknowledge receipt |

### Tag-Based Routing

Processors and criteria define required tags in their configuration. Calculation members declare capability tags on join. The dispatcher routes requests only to members whose tags match. If no member matches, the request fails with `NO_COMPUTE_MEMBER_FOR_TAG`.

### Computation Member Lifecycle

- **Join:** Member connects, sends `JoinEvent` with tags and tenant credentials
- **Greet:** Server validates credentials, assigns `memberId`, sends `GreetEvent`
- **Active:** Member receives dispatch requests, sends responses
- **Keep-alive:** Server sends periodic keep-alive (configurable interval/timeout). Missed keep-alive marks member offline.
- **Disconnect:** Member disconnects or times out. Pending dispatches fail with `COMPUTE_MEMBER_DISCONNECTED`.

### Transaction Context Propagation

Processor callbacks (CRUD operations performed by the processor) carry the transaction token. In a multi-node cluster, callbacks may arrive at any node — the router forwards them to the transaction-owning node (see Section 9).

---

## 8. Authentication and Authorization

### Modes

| Mode | Configuration | Behavior |
|------|--------------|----------|
| **Mock** | `CYODA_IAM_MODE=mock` (default) | All requests auto-authenticated as a default user. Zero setup. |
| **JWT** | `CYODA_IAM_MODE=jwt` | Real OAuth 2.0 with RS256 JWT tokens |

### JWT Mode Capabilities

| Capability | Endpoint | Description |
|------------|----------|-------------|
| **Token issuance** | `POST /oauth/token` | `client_credentials` grant |
| **OBO exchange** | `POST /oauth/token` | RFC 8693 token exchange — a service acting on behalf of a user |
| **JWKS** | `GET /.well-known/jwks.json` | Public key discovery for token verification |
| **M2M clients** | `POST/DELETE /auth/m2m/...` | Create, delete, reset secret for machine-to-machine clients |
| **Key management** | `POST/DELETE /auth/keys/...` | Issue, invalidate, reactivate, delete signing key pairs |
| **Trusted keys** | `POST/DELETE /auth/trusted/...` | Register external signing keys for cross-system trust |
| **Bootstrap client** | `CYODA_BOOTSTRAP_CLIENT_ID` | Pre-configured M2M client at startup (solves chicken-and-egg) |

### Token Claims

```json
{
  "iss": "cyoda",
  "sub": "<client_id>",
  "jti": "<unique_id>",
  "iat": 1711700000,
  "exp": 1711703600,
  "caas_user_id": "<user_id>",
  "caas_org_id": "<tenant_id>",
  "scopes": "ROLE_ADMIN,ROLE_M2M",
  "caas_tier": "unlimited"
}

```

### Per-Tenant OIDC Provider Registry

In JWT mode, each tenant can register one or more external Identity Providers (IdPs) whose JWTs cyoda-go will accept alongside its own locally-issued tokens. This enables single-sign-on scenarios where users authenticate through an external IdP (e.g. Okta, Keycloak, Azure AD) and present those tokens directly to cyoda-go — no token exchange required.

**What you can do with OIDC providers:**

- **Register** a provider by supplying its URL, accepted issuer values, expected audiences, and (optionally) a custom roles claim name.
- **List, get, update, delete** providers through the `/oauth/oidc/providers` REST surface (7 endpoints, `ROLE_ADMIN` required).
- **Invalidate** a provider to suspend JWT acceptance without removing the record; **reactivate** to restore it.
- **Reload** to force a fresh JWKS fetch and evict the node-local cache — useful after an IdP rotates its signing keys outside the normal TTL window.

Provider records are per-tenant; a tenant's OIDC configuration is invisible to other tenants.

**Validation chain.** When a JWT arrives, cyoda-go first checks whether the `iss` claim matches the locally-configured issuer (`CYODA_JWT_ISSUER`). If not, it searches the requesting tenant's registered providers for one whose `issuers` list matches. A match triggers external JWKS validation; no match rejects the token.

**External-issuer token structure.** Tokens from a registered OIDC provider follow the standard JWT format. The roles claim name is configurable per-provider (defaults to `CYODA_OIDC_ROLES_CLAIM`):

```json
{
  "iss": "https://auth.example.com",
  "sub": "user@example.com",
  "aud": ["cyoda-api"],
  "iat": 1750000000,
  "exp": 1750003600,
  "roles": ["ROLE_ADMIN"]
}
```

Cyoda-go maps the extracted roles to its standard `ROLE_ADMIN` / `ROLE_M2M` role set. The `caas_org_id` tenant claim required for local tokens is not required from external issuers — tenant affinity is determined by which tenant registered the matching provider.

### Delegating Authenticator

The authenticator routes by `iss` (issuer) claim:
- **Local tokens** (issuer matches configured `CYODA_JWT_ISSUER`): Extract roles from `scopes` claim.
- **Registered OIDC provider tokens** (issuer matches a provider in the requesting tenant's OIDC registry): Validate via the provider's JWKS endpoint; extract roles from the provider's configured `rolesClaim`.
- **Trusted external key tokens** (legacy path; see Trusted Keys above): Extract roles from `user_roles` claim.

### gRPC Authentication

gRPC calls authenticate via `Authorization` metadata. The same JWT validation applies, including the OIDC provider chain. Calculation members must authenticate with `ROLE_M2M` tokens.

---

## 9. Multi-Node Cluster Architecture

### Overview

Cyoda-Go operates as a cluster of 3-10 stateless Go nodes behind a load balancer (nginx). Every node is identical — no leader election, no shard ownership. PostgreSQL is the single coordination layer.

### Node Discovery: Gossip (SWIM Protocol)

Nodes discover each other using HashiCorp memberlist (embedded, pure Go). No external service discovery infrastructure (no etcd, no Consul, no ZooKeeper).

| Property | Value |
|----------|-------|
| **Protocol** | SWIM (Scalable Weakly-consistent Infection-style Membership) |
| **Convergence** | O(log N) — failure detection scales logarithmically |
| **Bandwidth** | Constant per node — pings a small random subset |
| **Failure detection** | Automatic; dead nodes evicted within seconds |
| **Bootstrap** | Seed nodes via configuration. Exponential backoff with jitter. |

**Startup sequence:**

1. Filter self from seed list
2. `list.Join(seeds)` with exponential backoff (500ms initial, 10s max, 2min total)
3. Poll membership until stable for 2-second window (no changes)
4. Open gRPC/HTTP servers and mark node ready

### Transaction Routing Tokens

When a node begins a PostgreSQL transaction, it generates an opaque HMAC-signed token:

```
Token payload:
  nodeID    — ID of the node holding the pgx.Tx
  txRef     — UUID key into the node's local transaction map
  expiresAt — Unix timestamp (TTL)
```

The token is base64url-encoded with HMAC-SHA256 signature. Clients cannot forge or tamper with tokens. The router decodes the token locally (no network call) to determine the owning node.

**Wire transport:**
- HTTP: `X-Tx-Token` header
- gRPC: `tx-token` metadata key

**Separate from transaction ID:** The `transactionId` (UUID) remains the logical identifier for storage, audit, and temporal queries. The `txToken` is a routing-only concern. Single-node deployments never see `txToken` if cluster mode is disabled.

### Request Routing

Every node acts as a transparent HTTP proxy for requests that belong to another node's transaction:

```
1. Extract txToken from header/metadata
2. Verify HMAC signature
3. If nodeID == self → serve locally
4. If nodeID != self → resolve address from local memberlist → forward request
5. If nodeID not found in membership → TRANSACTION_NODE_UNAVAILABLE (node is dead, tx is gone)
```

Address resolution is a local scan over `list.Members()` — no I/O, O(N) over node count (N is small by design).

### Cluster-Aware Compute Dispatch

In a multi-node cluster, a calculation member's gRPC stream terminates at one node, but the workflow engine may need to dispatch to that member from any node. Dispatch requests are forwarded to the node hosting the target member's stream.

### Failure Semantics

| Failure | Consequence | Client Action |
|---------|------------|---------------|
| Node holding `pgx.Tx` dies | PostgreSQL auto-rollbacks the connection | Retry from scratch |
| Node holding gRPC stream dies | Member disconnects, pending dispatches fail | Reconnect member |
| PostgreSQL unreachable | All transactions on all nodes fail | Wait for recovery |
| Network partition (node-to-node) | Affected node's transactions unreachable from other nodes | Retry when partition heals |

**Key safety property:** The `pgx.Tx` handle lives exclusively in one node's process memory. No other node can commit, rollback, or interact with that transaction. There is no competing-commit scenario.

---

## 10. API Surface

### REST API (OpenAPI 3.1)

| Area | Endpoints | Description |
|------|-----------|-------------|
| **Health** | `GET /health` | Readiness probe |
| **Entity CRUD** | `POST/GET/PUT/DELETE /entity/...` | Create, read, update, delete (single and batch) |
| **Entity Stats** | `GET /entity/stats/...` | Count, state distribution per model |
| **Model Management** | `POST/GET/DELETE /model/...` | Import, export, lock, unlock, delete, validate, changeLevel |
| **Workflow** | `GET/POST /model/.../workflow/{export,import}` | Import/export workflow definitions |
| **Search** | `POST /search/{direct,async}/...` | Synchronous and snapshot search |
| **Audit** | `GET /audit/entity/{entityId}` | Entity change history, SM audit trail |
| **Messaging** | `POST/GET/DELETE /message/...` | Edge message store |
| **Auth** | `POST /oauth/token`, `GET /.well-known/jwks.json`, key/trusted/M2M management | Authentication and key management |
| **Account** | `GET /account` | Account info, subscriptions |
| **Cluster** | `GET /cluster/members/calculation/...` | Connected member registry |
| **Admin** | `GET/POST /admin/log-level` | Runtime log level control |

All REST responses follow RFC 9457 (Problem Details) for errors.

### gRPC API (CloudEventsService)

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

---

## 11. Deployment

### Single Binary

Cyoda-Go compiles to a single Go binary. No runtime dependencies beyond the binary itself (in memory or sqlite mode) or PostgreSQL (in postgres mode).

**Container image:** Distroless base (`gcr.io/distroless/static-debian12`). Minimal attack surface.

### Deployment Modes

| Mode | Dependencies | Use Case |
|------|-------------|----------|
| **Standalone (memory)** | None | Development, testing, CI |
| **Standalone (sqlite)** | None — embedded WASM SQLite + a writable data directory | Desktop, edge, containerised single-node production |
| **Standalone (PostgreSQL)** | PostgreSQL 14+ | Single-node production |
| **Multi-node cluster** | PostgreSQL 14+, nginx LB | HA production |

### Multi-Node Docker Deployment

```
┌─────────┐
│  nginx   │ ← Load balancer (port 8123)
│  (LB)   │
├─────────┤
│ Node 1  │ ← HTTP + gRPC + gossip
│ Node 2  │
│ Node 3  │
├─────────┤
│PostgreSQL│ ← Shared primary; SI+FCW contract (REPEATABLE READ + row locks)
└─────────┘
```

Provisioned via `start-cluster.sh` with configurable `--nodes` flag.

---

## 12. Storage Plugin Architecture

A running binary has exactly one active storage plugin. Per-store routing (mixing backends in a single instance) is not supported — every store in a given binary uses the same plugin. Plugins are selected at startup via `CYODA_STORAGE_BACKEND`.

### Stock Plugins (shipped with `cyoda-go`)

| Plugin | Dependencies | Use case |
|--------|--------------|----------|
| **memory** (default) | None — in-process Go maps | Rapid development, agent-driven application engineering, embedded/test usage. Single-node only; data is lost on restart. |
| **sqlite** | None — WASM SQLite embedded in the binary | Persistent single-node storage for desktop, edge, and containerised single-node production. Single-process only (flock-guarded); no NFS. |
| **postgres** | PostgreSQL 14+ | Production durability; single-node or multi-node clusters (3–10 nodes) behind a load balancer. All cluster state flows through PostgreSQL as the consistency authority. |

### Commercial Plugin (available from Cyoda)

A commercial `cassandra` plugin is also available for deployments that
need horizontal write scalability beyond what a single-primary
PostgreSQL can provide. It delivers the same SI+FCW isolation contract
as the stock plugins. See [cyoda.com](https://www.cyoda.com) for
details and sizing guidance.

### Writing a Third-Party Plugin

Plugin authors depend only on `github.com/cyoda-platform/cyoda-go-spi` (stdlib-only Go interfaces). A plugin implements the `Plugin` interface and registers itself from `init()`:

```go
import spi "github.com/cyoda-platform/cyoda-go-spi"

func init() { spi.Register(&myPlugin{}) }
```

Users compose a custom binary by blank-importing plugins alongside `cyoda-go`:

```go
import (
    _ "github.com/cyoda-platform/cyoda-go/plugins/memory"
    _ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
    _ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
    _ "example.com/my-redis-plugin"
)
```

The `memory`, `sqlite`, and `postgres` plugins serve as reference
implementations. See `docs/ARCHITECTURE.md` for the full plugin
contract and `docs/plugins/` for per-plugin operational detail.

---

## 13. Observability

### Structured Logging

All logging via Go's `log/slog`. Runtime-switchable via `POST /api/admin/log-level`.

| Level | Purpose |
|-------|---------|
| **ERROR** | Failures requiring investigation (stream send failed, commit failed, panic recovery) |
| **WARN** | Unexpected but recoverable (unknown CloudEvent type, auth failure) |
| **INFO** | High-level flow milestones (member joined, entity created, server started) |
| **DEBUG** | Detailed flow tracing with payload previews (first 200 chars, truncated) |

Structured context fields: `pkg`, `memberId`, `entityId`, `eventType`, `transactionId`.

### Error Classification

Three-tier error model:

| Tier | HTTP Status | Response Content | Server Action |
|------|-------------|-----------------|---------------|
| **Client Error** | 4xx | Full domain error detail with error code | Log at WARN |
| **Server Error** | 5xx | Generic message + correlation ticket UUID | Log at ERROR with full detail |
| **Fatal Error** | 5xx | Generic message + correlation ticket UUID | Log at ERROR, mark health unhealthy |

Error responses follow RFC 9457 (Problem Details). Error detail level controlled by `CYODA_ERROR_RESPONSE_MODE` (`sanitized` or `verbose`).

### Error Code Taxonomy

Every error response carries a structured `errorCode` in `properties` plus an optional `retryable` boolean (`true` only when the request is safe to replay as-is — typically transient cluster or storage-serialization conditions; absent or `false` otherwise). Programmatic clients key on `errorCode`, not HTTP status: multiple codes may share the same status, and the code expresses the failure mode the dictionary preserves.

Codes are grouped by surface area:

- **Domain** — model lifecycle, entity CRUD, workflow, validation, generic 4xx (`BAD_REQUEST`, `UNAUTHORIZED`, `FORBIDDEN`, `SERVER_ERROR`, `NOT_IMPLEMENTED`).
- **Cluster / transaction** — distributed-transaction hand-off, gossip membership, idempotency.
- **Compute dispatch** — externalized processor / criteria invocation across cluster members.
- **Search** — async search-job lifecycle and shard-scan limits.

Authoritative code list: `internal/common/error_codes.go`. Per-code semantics, HTTP status, retryable hint, and remediation guidance live in the help subsystem at `cmd/cyoda/help/content/errors/<CODE>.md`, surfaced via `cyoda help errors` and `cyoda help errors <CODE>`. The `TestErrCode_Parity` gate in `cmd/cyoda/help` enforces that every constant in `error_codes.go` has a corresponding help topic.

### OpenTelemetry

OpenTelemetry instrumentation is implemented end-to-end. Traces, metrics, and log correlation use the OTel SDK with OTLP HTTP exporters. HTTP requests are auto-traced via `otelhttp`. Transaction lifecycle operations (`tx.begin`, `tx.commit`, `tx.rollback`, `tx.savepoint`) produce spans with duration/active/conflict metrics regardless of which plugin is active (the core wraps the plugin's `TransactionManager` with a tracing decorator). Workflow engine and externalized processor dispatch are traced. Plugins may add their own plugin-namespaced spans and metrics as their hot-path semantics warrant. A Grafana / Prometheus / Tempo dashboard ships with the bundled docker environment. Standard OTel environment variables (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_TRACES_SAMPLER`) configure export and sampling.

---

## 14. Scale Profile and Operational Boundaries

### Target Operating Envelope (PostgreSQL plugin, multi-node)

The envelope below characterises the multi-node PostgreSQL
deployment. The `memory` and `sqlite` plugins are single-node and
bounded by host resources; the commercial `cassandra` plugin has its
own envelope (contact Cyoda).

| Dimension | Sweet Spot | Upper Bound | Notes |
|-----------|-----------|-------------|-------|
| Cluster size | 3–5 nodes | 10–20 nodes | Beyond 10, gossip metadata grows and proxy hop probability increases |
| Concurrent transactions | 50–250 | ~750 (3 nodes × 25 PG connections) | Bounded by PG connection pool × node count |
| Entity volume | Up to millions per model | Bounded by PG storage | Version history grows monotonically (append-only, no compaction) |
| Transaction duration | < 1 second (ideal) | 60 seconds (default TTL) | Each second of transaction duration consumes one PG connection |
| Write throughput | 50–200 entity creates/s per node | Bounded by PG primary throughput | Contention on same entities triggers SI+FCW conflicts + retries |
| Compute processor latency | < 500 ms | 30s (default timeout) | Processor duration dominates transaction duration |

### Where This Design Excels (PostgreSQL plugin)

- **Transactional correctness:** Zero-compromise SI+FCW isolation (see `docs/CONSISTENCY.md`). No eventual consistency, no conflict windows, no split-brain. PostgreSQL is the single source of truth.
- **Operational simplicity:** One PostgreSQL instance plus stateless Go binaries. No ZooKeeper, no Kafka, no external service discovery.
- **Development velocity:** The `memory` plugin runs with zero dependencies. Same code, same tests, same APIs as production.
- **Small-to-medium data volumes:** For financial ledgers, regulatory records, order management — datasets that fit comfortably in a single PostgreSQL instance (terabytes, not petabytes).

### Where This Design Has Limits (PostgreSQL plugin)

- **Write-heavy workloads at scale:** All writes go through a single PostgreSQL primary. The PostgreSQL plugin cannot shard writes across storage nodes. The commercial `cassandra` plugin from Cyoda is the answer for deployments that outgrow a single PostgreSQL instance and need horizontal write scalability.
- **Long-running transactions:** A 10-second processor holds a PG connection for 10+ seconds, limiting concurrency.
- **Large cluster sizes:** Beyond 10 nodes, the benefits of adding nodes diminish — compute capacity scales, but write capacity does not.
- **Unbounded version histories:** Append-only entity versioning has no built-in archival. Long-lived entities with frequent updates accumulate version chains that slow point-in-time queries.

### When to Switch Plugins

| Symptom | Signal to switch | Destination |
|---------|------------------|-------------|
| Transactions per second saturating a single PG instance, or growth trajectory will exceed PG single-node capacity within 12 months | Write throughput ceiling | Commercial `cassandra` plugin (contact Cyoda) |
| Cluster size consistently above 10 nodes with write bottleneck | Scale-out ceiling | Commercial `cassandra` plugin (contact Cyoda) |
| Storage engine has no match in the stock lineup (e.g. cloud-native KV store, specialized time-series engine) | Plugin fit | Third-party plugin (author against `cyoda-go-spi`) |

See [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) Section 14 for detailed technical limits, latency expectations, and sizing guidance.

---

## References

- **Architecture:** [`docs/ARCHITECTURE.md`](ARCHITECTURE.md)
- **Consistency contract:** [`docs/CONSISTENCY.md`](CONSISTENCY.md)
- **Per-plugin documentation:** [`docs/plugins/`](plugins/)
- **Architecture Decision Records:** [`docs/adr/`](adr/)
- **OpenAPI Specification:** `api/openapi.yaml`
- **Storage Plugin SPI module:** [github.com/cyoda-platform/cyoda-go-spi](https://github.com/cyoda/cyoda-go-spi)
- **Commercial Cassandra plugin:** contact Cyoda via [cyoda.com](https://www.cyoda.com)
