# Subscription Framework — v1 Design

**Date:** 2026-06-21 (revision 2026-06-22, post fresh-context review)
**Status:** Design — awaiting plan
**Scope:** v1 notification framework for cluster-of-application-nodes consumption. Not a substitute for a queue subsystem; not for webscale user-facing fan-out; not a historical reprocessing API.

## §1. Scope & vocabulary

A subscriber framework that lets client nodes (not just compute nodes) attach to cyoda over gRPC, declare a filter on entity model + workflow + body, and receive change events as **transaction segments commit** — with a per-subscription choice of ephemeral or durable delivery.

This is **a v1 notification framework, not a messaging or entity-event-queue framework.** Applications that need a durable, replayable, high-throughput queue subsystem build it on their own infrastructure. A future cyoda-native entity-event-queue would be a separate capability built on a real queue substrate.

### Vocabulary

| Term | Meaning |
|---|---|
| **Segment** | A single transactional commit in cyoda's engine. `EngineResult.Segmented=true` means a request spanned multiple segments via `COMMIT_BEFORE_DISPATCH` processors. Within one segment, multiple workflow transitions can have cascaded (via `cascadeAutomated`). The segment is the natural emission unit. |
| **Change event** | One emission per **segment commit**, carrying envelope and an ordered list of transitions that ran in that segment. |
| **Subscription** | A server-side declaration: filter + delivery mode + consumption mode + batching + body-include policy. Ephemeral (lives with the stream) or durable (persists across connections). |
| **Delivery mode** | `LIVE_ONLY` (no replay; ephemeral) or `DURABLE` (at-least-once delivery for events emitted after subscription start, replay possible from the subscription's last-committed cursor within outbox retention). |
| **Consumption mode** | `BROADCAST` (each attached consumer gets every event) or `GROUP` (events partition across consumers by `entity_id`). Ephemeral is always `BROADCAST`. **GROUP is only available on backends advertising `GroupConsumption=true` — in v1 that is the cassandra plugin only**, which delegates to redpanda's Kafka-compatible consumer-group machinery. |
| **ChangeFeed** | SPI capability each storage plugin implements internally. Atomic event append + filtered consume. Capabilities advertised per-backend. |
| **Cursor** | Opaque server-issued token; monotone within a partition. Subscriber commits to advance. |
| **`startFromTimestamp`** | Optional subscription field. Permits bounded preload bridging — server-clamped by `maxLookback`. |
| **Bootstrap point** | The `(createdAt, cursorZero, effectiveStartFrom)` tuple returned on subscription create. Defines the strict-`>` boundary between "preload territory" and "subscription territory". |

### Non-goals for v1

- JSONPath body projection / JSON Patch diff in the envelope.
- Failed-transition events (extensibility hook reserved on `kind`).
- Processor outputs in the envelope (extensibility hook reserved).
- Subscriber-overridable partition key for GROUP mode (`entity_id` only).
- ChangeFeed as a separately pluggable SPI capability.
- Pulling redpanda into OSS core.
- Cursor rewind / historical reprocessing.
- Unbounded historical replay. Bounded preload bridging up to `maxLookback` (cluster-configurable, default 15 minutes) is in scope.
- A substitute for a real durable queue subsystem.
- Webscale per-user-session subscriptions. Operational target is **clusters of application nodes** (BFFs, cache invalidators, integration sinks). v1 caps active consumers per node.
- Granular per-model permissions. v1 reuses `ROLE_M2M`, matching the existing compute-node gRPC streaming surface.
- GROUP mode on OSS backends (memory/sqlite/postgres). Designing our own Kafka-style consumer-group rebalance protocol is out of scope; GROUP is available only on cassandra in v1.
- Cross-entity atomicity of change-event emission. Atomicity is **per-entity within a segment** (matching cassandra's natural guarantee). A multi-entity segment produces a change event per entity; some events may commit and others fail.

## §2. Subscription model

A subscription is the unit the subscriber declares; everything else flows from it.

### Shape

```json
{
  "id": "sub_01H...",
  "tenantId": "...",                    // derived from JWT; never client-supplied
  "filter": {
    "modelName": "ORDER",
    "modelVersion": "1.0",              // exact match; "*" allowed
    "kinds":      ["CREATE","UPDATE","DELETE"],   // optional; default all
    "fromState":  null,                  // matches any transition in segment with this from
    "toState":    "PAID",                // matches final_state OR any transition's to
    "bodyPredicate": { ... }            // optional; existing spi.predicate condition tree
  },
  "delivery":    "LIVE_ONLY" | "DURABLE",
  "consumption": "BROADCAST" | "GROUP",  // GROUP requires backend.GroupConsumption=true
  "includeBody": "NONE" | "AFTER" | "BEFORE_AND_AFTER",
  "batching": {
    "windowMs":   100,                   // 0 = disabled (per-event delivery)
    "maxEvents":  500
  },
  "startFromTimestamp": "2026-06-21T10:15:00Z",  // optional; default = T_now at creation
  "ttl": "30d",                          // durable only
  "createdAt": "...",
  "lastAttachedAt": "..."
}
```

### Filter semantics for segment-shaped events

Because a segment can contain multiple transitions, envelope filter semantics:

- `kinds` — matches the segment-derived `kind` (`CREATE` if first transition was initial-state entry; `DELETE` if last transition entered terminal/hard-delete; otherwise `UPDATE`).
- `fromState` — matches **any** transition's `from` in the segment. So a subscription `fromState: DRAFT` catches a segment that started at DRAFT even if it ended elsewhere via cascade.
- `toState` — matches `final_state` **OR** any transition's `to` in the segment. So a subscription `toState: PAID` catches both "ended at PAID" and "transited through PAID in a cascade".
- `bodyPredicate` — evaluated at fan-out against `body_after` (post-segment state). Uses existing `cyoda-go-spi/predicate.Condition` tree (SimpleCondition / GroupCondition), same surface as the existing search API.

### Validity matrix

| delivery | consumption | allowed? |
|---|---|---|
| `LIVE_ONLY` | `BROADCAST` | yes (default) |
| `LIVE_ONLY` | `GROUP` | no — group requires durable cursor state |
| `DURABLE` | `BROADCAST` | yes, all durable-capable backends |
| `DURABLE` | `GROUP` | yes, only on backends with `GroupConsumption=true` (cassandra in v1) |

Backend capability mismatch at create → `400 Bad Request` with the supported capabilities echoed back.

### Defaults

| Field | Default | Rationale |
|---|---|---|
| `kinds` | all three | most subscribers want all CRUD |
| `includeBody` | `NONE` | smallest payload |
| `batching.windowMs` | `0` | predictable latency by default |
| `startFromTimestamp` | `T_now` at create | live-only by default; opt-in for bounded preload bridging |
| `delivery` | (required) | force the deliberate durability decision |
| `consumption` | `BROADCAST` | covers the two stated use cases |
| `ttl` | `30d` | durable only |

### Identity rules

- `tenantId` always derived from JWT, never accepted from client.
- Subscription `id`: server-generated ULID for durable subs.
- `GROUP` consumer joins identify with a client-chosen `consumerInstanceId`. Two attaches with the same `consumerInstanceId` on the same subscription: the second wins with a generation-counter fencing mechanism (the first is fenced off, not silently disconnected — prevents double-write during rolling deploys).

### TTL behaviour

- Durable subscription with no consumer attached for `ttl` is auto-deleted. Default 30 days.
- Backlog ceiling applies separately (§3.5). Exceeding ceiling triggers `DETACH_CONSUMER` policy by default.
- At TTL auto-delete: retention claim is released atomically; outbox cleanup follows the retention sweeper. No orphaned claims.

## §3. API surface — REST CRUD + gRPC stream

### REST management plane

All endpoints scoped under the tenant from JWT.

| Method & path | Purpose | Notes |
|---|---|---|
| `POST   /tenants/{t}/subscriptions` | Create durable subscription | Returns 201 + full resource + `cursorZero` + `effectiveStartFrom` + `maxLookback` + `enablementBoundary`. |
| `GET    /tenants/{t}/subscriptions/{id}` | Read subscription | Returns full resource + lag metrics. |
| `GET    /tenants/{t}/subscriptions` | List subscriptions | Paginated; filter by `modelName` / `consumerInstanceId` via query params. |
| `PATCH  /tenants/{t}/subscriptions/{id}` | Update subscription | Mutable fields only: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter, delivery, consumption, startFromTimestamp are immutable. |
| `DELETE /tenants/{t}/subscriptions/{id}` | Delete subscription | Detaches connected consumers, deletes cursor state, releases retention claim. Idempotent. |

Ephemeral subscriptions have no REST surface — they're created and destroyed via the gRPC stream.

Errors follow existing cyoda conventions (4xx full domain detail + error code; 5xx generic + ticket UUID).

### gRPC stream — `EntityChangeService`

New proto service, separate from `CloudEventsService`:

```proto
service EntityChangeService {
  rpc Stream(stream io.cloudevents.v1.CloudEvent) returns (stream io.cloudevents.v1.CloudEvent);
}
```

Streams bare `CloudEvent` directly, matching the existing `CloudEventsService.startStreaming` idiom. Typed messages inside the stream are listed in §6.

**Why separate from `CloudEventsService.startStreaming`:** compute-node streaming already mixes processor invocations and criterion evaluations; mixing subscriber traffic on the same channel creates three distinct request/response patterns on one stream. A separate service has its own interceptor scope, rate-limit bucket, and metrics namespace.

One stream = one attached subscription.

### Auth

- REST: existing JWT bearer + tenant scope. **All subscription endpoints require `ROLE_M2M`** — same surface as the existing compute-node gRPC streaming (`internal/grpc/streaming.go:58`). Consistent with cyoda's M2M-tooling story; no new permission system.
- gRPC stream: same JWT in metadata, same tenant derivation. `ROLE_M2M` required at attach time.
- Cross-tenant attach (subscription's tenantId ≠ JWT tenant) → `PERMISSION_DENIED`.
- Tenant isolation is implicit via existing patterns: all storage queries already filter by `uc.Tenant.ID`.

## §3.5. Connection lifecycle & recovery

Subscriptions, cursors, and partition assignments are authoritative server state. The gRPC stream itself carries **no** durable state.

### Scenario matrix

| Scenario | Ephemeral | Durable |
|---|---|---|
| **Clean detach** | State dropped immediately. | Cursor at last ack persisted. GROUP: partitions reassigned via redpanda native group protocol. |
| **Network blip** | State dropped after gRPC keepalive detects death. | Server detects death via keepalive; cursor at last ack; GROUP rebalance is redpanda's responsibility (consumer-group session timeout). Reconnect with same `subscriptionId`+`consumerInstanceId`+`generation` resumes. |
| **Client restart / pod replacement** | New ephemeral subscription. | New `consumerInstanceId` joining = standard redpanda rebalance for GROUP, no-op for BROADCAST. Generation counter prevents zombie writes. |
| **Server node dies** | Stream errors `UNAVAILABLE`. | Stream errors `UNAVAILABLE`. Reconnect to another node; subscription resource lives in shared storage. |
| **Long disconnection** | Subscription gone with the stream. | Replay from last cursor if within retention; else `OUT_OF_RANGE` with earliest still-available cursor. |
| **TTL exceeded with no consumer** | N/A | Subscription auto-deleted; reconnect = `NOT_FOUND`. |
| **Mid-batch disconnect** | Batch lost. | Batch redelivered from last committed cursor. Consumer dedupes via `event_id`. |

### Mechanism

All authoritative state — subscription resource, cursor per consumer instance, partition assignments — lives in the storage tier (durable) or in-process on the attached node (ephemeral). gRPC stream death triggers fencing/grace; reconnect rehydrates state. At-least-once requires `event_id` for consumer-side dedupe.

For GROUP mode on cassandra, rebalance and partition assignment are **delegated to redpanda's consumer-group protocol** (`KafkaSubscribe`/`KafkaCommit` primitives the cassandra plugin already uses). No custom rebalance protocol is built.

### Operational knobs

| Knob | Default | Notes |
|---|---|---|
| **gRPC keepalive interval** | 10s | Matches `internal/grpc/streaming.go:21` existing default. |
| **gRPC keepalive timeout** | 30s | Matches `internal/grpc/streaming.go:23` existing default. |
| **GROUP session timeout** | redpanda default | Inherited from redpanda consumer-group config. |
| **Backlog ceiling per subscription** | 1M events or 7d, whichever first | Hard ceiling for un-acked backlog. |
| **Backlog ceiling policy** | `DETACH_CONSUMER` | Alternatives: `BLOCK_APPEND` (risky), `DROP_OLDEST` (explicit at-most-once). |
| **Consumers per node cap** | 1000 | Operational ceiling. Reasonable for cluster-of-application-nodes deployments. |

## §4. Event source & emission

### Source

Single emission point: **the engine's segment commit hook** (in plugin `txMgr.Commit`, after `materialize()` succeeds). One segment commit → one or more change events (one per entity touched in that segment; see §4.3 for cardinality).

Per the engine semantics verified in `internal/domain/workflow/engine_processors.go` and `internal/domain/entity/service.go`:
- One user request = 1 or more segments (`EngineResult.Segmented=true` if `COMMIT_BEFORE_DISPATCH` ran).
- One segment = exactly one tx commit.
- One segment can contain multiple **transitions** that cascaded (`cascadeAutomated` engine.go:516).
- Each emission is the unit "one entity's state changed via N transitions in one segment commit".

### Atomicity

Per-entity within a segment, matching cassandra's natural guarantee:

- **Cassandra**: change event row written to a new `entity_change_events` table in the existing materialize batch, in the same tag group as the entity's other writes (`tx_coordinator.go materialize()`). Per-entity statements share a tag → same chunk → atomic at the per-entity row level via `USING TIMESTAMP <HLC>` fencing. **Redpanda is not the atomicity boundary**; it's an async fan-out transport driven by a publisher that drains `entity_change_events`.
- **Postgres/SQLite**: change event row written to `change_outbox` table within the same SQL transaction as the entity write.
- **Memory**: change event pushed to in-process channel after the in-memory write. Tx-aware (rollback discards).

The atomicity claim: **if `txMgr.Commit` succeeds, the change event for each entity in the segment will eventually be visible to subscribers.** Cross-entity atomicity within one segment is **not** claimed; cassandra's materialize chunks are independent. A segment that touches entities A, B, C may produce visible events for A and B but not C if a partial-chunk failure occurs — entity C's write rolls back (no entity visibility either). The §gate-3 invariant "entity write iff change event" holds per-entity.

### Emission gate

The engine emits per-entity change event at segment commit **iff** both:

1. At least one subscription exists for the entity's `(tenant, model_name, model_version)`, AND
2. At least one subscription's **envelope filter** matches the segment for that entity. Short-circuit on first match. Body predicates are **not** evaluated at emission.

The check uses a local cache (Fork 3, model iii):

- **Cache hit**: O(1) lookup of registered envelope filters for `(tenant, model, version)`.
- **Cache miss**: read authoritative `subscriptions` storage for that key (one query, cached for a short TTL like 30s).
- **Cache invalidation**: on subscription create/delete, the originating node broadcasts via `ClusterBroadcaster` (best-effort); receiving nodes invalidate the affected `(tenant, model, version)` entry. Next emission for that key triggers a lazy storage re-read.

This removes the "fail-open on cache unavailable" language from the earlier draft — the cache is just a Go map with a storage backstop. It cannot become "unavailable" except at startup; at startup, storage is the source of truth from the first emission onward.

### Envelope construction

```
ChangeEvent {
  event_id        ULID                            generated at emission
  tenant_id       string                          from request context (JWT-derived)
  transaction_id  string                          from EntityMeta.TransactionID (cyoda-go-spi/types.go:18)
  cursor          opaque (base64)                 assigned by ChangeFeed.AppendChange; monotone within partition
  emitted_at      time.Time                       server clock at segment commit
  model_name      string
  model_version   string
  entity_id       string                          (SPI uses string, not uuid.UUID)
  kind            ChangeKind                      CREATE | UPDATE | DELETE — derived from segment
  transitions     []Transition                    ordered list: {from, to, transitionName}
  final_state     string                          last transition's to_state
  body_after      *EntityBody                     attached at fan-out, not at emission
  body_before     *EntityBody                     attached at fan-out, not at emission
}

Transition {
  from           string
  to             string
  transition_name string
}
```

- `kind` derivation: `CREATE` if the first transition was initial-state entry; `DELETE` if the last transition was terminal or hard-delete; otherwise `UPDATE`.
- `final_state` = `transitions[-1].to`; empty on hard-delete.
- `body_before` = entity body before the segment's first transition. Engine captures this once at segment-start and carries through to emission (see §4.2).
- `body_after` = entity body after the segment commit.

### What is not emitted in v1

- Failed segments (rolled-back tx). Reserved.
- Per-individual-transition events. Per-segment with `transitions` list is the v1 contract.
- Non-transition writes (admin overrides, bulk imports). Confirmed not to exist today.
- Cross-entity correlation in one envelope. Each entity is its own change event; a segment touching N entities produces N events sharing `transaction_id`.

## §4.1. Per-(tenant, model, version) enablement boundary

- **First subscription** for a given `(tenant, model_name, model_version)`:
  1. Inserts the subscription row in storage,
  2. Sets the enablement boundary marker for that key to `T_now`,
  3. Best-effort broadcasts cache invalidation to other nodes.
- The boundary marker is the floor for any subsequent subscription's `startFromTimestamp` on the same key.
- Surfaced in the create-subscription response as `enablementBoundary.tenantFirstEnabledAt`.
- Atomicity scope: the subscription row + the marker are written in the same storage tx. Cache invalidation is broadcast separately; if a peer node doesn't receive the broadcast, its first emission for the key will cache-miss and re-read storage (§4 emission gate), still seeing the new subscription. Correctness preserved.

## §4.2. Filtering & body materialisation

### Pipeline per event E and candidate subscription S

1. **Envelope match** — `modelName`, `modelVersion`, `kinds`, `fromState`, `toState` matched against segment-shaped envelope (any-transition-from/to + final_state for `toState`). Pure metadata; no body needed.
2. **Body predicate match** (only if S has one) — `spi.predicate.Condition` evaluated against `body_after`. Requires the entity body.
3. **Ship** — envelope and (per S's `includeBody`) `body_before` / `body_after`.

### Body load contract

The entity body is loaded at fan-out **iff** at least one local consumer for that event has either:

- a body predicate (to evaluate the predicate), OR
- `includeBody != NONE` (to ship the body).

Bodies are LRU-cached. Cache size bounded — `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB). Evicted entries are re-fetched from storage.

**Documented cost**: body-predicate + `includeBody: NONE` triggers a body load. Subscribers opting in accept the I/O.

### Pre-state capture (body_before)

The engine has the pre-state on the stack during the first transition of a segment. To make it available at emission:

- Engine captures `body_before` once at segment-start (before the first transition runs), holds it in `TransactionState`.
- At segment commit, the captured `body_before` is part of the emission envelope reference.
- For replay (durable lagging consumer): re-fetch from `entity_versions` table by `(entity_id, version - 1)` via existing point-in-time queries (postgres/sqlite have `GetAsAt` and `GetAllAsAt`; cassandra has the lifecycle index). Cheap with existing PK indexes.

### Outbox row vs. envelope shape

The outbox row stores envelope fields except `body_before` / `body_after`. Bodies are looked up by version pair at fan-out:

- Postgres / SQLite: `entity_versions` rows by `(entity_id, version)`.
- Cassandra: lifecycle index by `(entity_id, version)`.

For efficient outbox time-range queries (postgres/sqlite), add composite index `(tenant_id, model_name, model_version, emitted_at DESC, version)`. Storage cost negligible; one B-tree insertion per save.

### Subscription mutation invariants

PATCH-mutable: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter, delivery, consumption, startFromTimestamp are immutable.

### Subscription create/delete cache invariant

Best-effort broadcast invalidates per-`(tenant, model, version)` cache entries on peer nodes. Stale-cache consequence: peers may miss the first emission for a new subscription if they retain a "no subscriptions" cache entry. **Mitigation**: the cache stores positive entries (filter sets) with a short TTL (30s); a "no subscriptions" answer is *not cached* — every emission for an unknown key forces a storage read. This bounds the staleness window to one storage read per unknown key per node.

## §4.3. Cardinality and outbox volume

For a segment touching N entities:
- Emits N change events (one per entity), all sharing `transaction_id`.
- N outbox rows.

For a segment containing M cascaded transitions on the same entity:
- Emits 1 change event with `transitions` list of length M.
- 1 outbox row.

For a user request spanning S segments (CBD boundaries):
- Emits ≥ S events (more if multi-entity segments).
- Each event's `transaction_id` is its own segment's tx_id (the post-CBD tx for cascade-final).

Outbox volume scales with **entity-update rate**, not transition rate. Cascade-heavy workflows produce fewer outbox rows than a per-transition design would.

## §5. SPI ChangeFeed capability

### Interface

```go
package spi

type ChangeFeed interface {
    // AppendChange writes the change event atomically with the entity write in
    // the current transaction. The transaction is retrieved from ctx via the
    // standard SPI pattern (spi.GetTransaction(ctx)).
    //
    // Per-entity atomicity: if tx.Commit() succeeds, this event is durable and
    // will eventually be visible to consumers. Cross-entity atomicity within a
    // single segment is NOT guaranteed (see §4 atomicity).
    AppendChange(ctx context.Context, envelope ChangeEnvelope) error

    // Consume returns a stream of change events matching spec.
    Consume(ctx context.Context, spec ConsumerSpec) (ChangeEventStream, error)

    // Retain/Release manage retention claims for subscriptions.
    Retain(ctx context.Context, claim RetentionClaim) error
    Release(ctx context.Context, subscriptionID string) error

    // Capabilities reports backend capabilities.
    Capabilities() ChangeFeedCapabilities
}

type ChangeEnvelope struct {
    EventID        string                 // ULID
    TenantID       string                 // NOT uuid.UUID; SPI convention is string
    TransactionID  string                 // from EntityMeta.TransactionID
    Cursor         string                 // assigned by AppendChange; opaque base64
    EmittedAt      time.Time
    ModelName      string
    ModelVersion   string
    EntityID       string                 // NOT uuid.UUID; SPI convention is string
    Kind           ChangeKind             // CREATE | UPDATE | DELETE
    Transitions    []TransitionRecord     // ordered list
    FinalState     string
    BodyVersionRef VersionRef             // for downstream body hydration (entity_id, version pair)
}

type TransitionRecord struct {
    From           string
    To             string
    TransitionName string
}

type ConsumerSpec struct {
    TenantID       string
    EnvelopeFilter EnvelopeFilter
    StartCursor    string
    GroupID        string                 // empty for BROADCAST
    PartitionAssignment *PartitionAssignment // GROUP mode only; backend-specific opaque token
}

type ChangeFeedCapabilities struct {
    Durable          bool
    GroupConsumption bool
    MaxRetention     time.Duration
    MaxBacklogPerSub int
}
```

### Per-backend implementation

| Backend | AppendChange | Consume | Retain/Release | Capabilities |
|---|---|---|---|---|
| **memory** | push to in-process channel; tx-aware (rollback discards) | range over channel buffer with envelope filters | no-op | `Durable=false, GroupConsumption=false`, retention=0 |
| **sqlite** | INSERT into `change_outbox` in the tx | in-process broadcaster signals post-commit; per-consumer bounded queues; catchup-by-cursor on attach; no polling in normal operation | UPDATE claim row; sweep daemon | `Durable=true, GroupConsumption=false`, retention configurable |
| **postgres** | INSERT into `change_outbox` in the tx; `pg_notify` post-commit (best-effort wake-up) | `LISTEN/NOTIFY` for low-latency wake-up + periodic poll (default 10s) as missed-notify backstop. Outbox time-range queries use new composite index `(tenant_id, model_name, model_version, emitted_at DESC, version)`. | claim row + sweep daemon | `Durable=true, GroupConsumption=false`, retention configurable |
| **cassandra** | INSERT new row into `entity_change_events` table inside the existing materialize batch, in the same tag group as the entity's other writes (`tx_coordinator.go:863` natural slot, same chunk → atomic per-entity via HLC `USING TIMESTAMP`). A separate publisher daemon (in the plugin) reads new rows and publishes to a redpanda topic for fan-out + group consumption. | For LIVE / GROUP: subscribe to redpanda consumer group (existing `KafkaSubscribe` machinery). For replay older than redpanda retention: read `entity_change_events` directly. | redpanda offset commits for GROUP; `entity_change_events` retention via TTL | `Durable=true, GroupConsumption=true`, retention = max(redpanda retention, table TTL) |

### GROUP consumption — cassandra-only via redpanda

The cassandra plugin reuses its existing redpanda integration:

- One redpanda topic per `(tenant, model_name, model_version)` (or a single topic with key=tenant:model:version), partitioned by `entity_id` hash for per-entity ordering.
- Consumer groups handled by redpanda natively: heartbeats, partition assignment, fencing all native.
- The plugin advertises `GroupConsumption=true`; subscription create validates against this.

OSS plugins (memory/sqlite/postgres) advertise `GroupConsumption=false`. Subscription create with `consumption=GROUP` on an OSS backend → `400 Bad Request` with backend capabilities in the response body.

### Engine-side contract

- Engine calls `AppendChange` once per entity per segment commit, inside the same SPI tx as the entity write.
- Engine constructs the envelope from data on the stack at segment commit: post-state, `transactions` list accumulated during cascade, `body_before` captured at segment-start.
- Engine never calls `Consume` — that's the fan-out layer.

### Fan-out layer (engine-internal, not SPI)

- One goroutine per attached consumer per node, capped at `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` (default 1000).
- Calls `ChangeFeed.Consume(spec)` with the consumer's envelope filter.
- Receives envelope stream; applies body predicate locally (with body hydration via `BodyVersionRef`); ships per `includeBody`.
- LRU body cache bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB).
- Tracks per-consumer ack cursor; periodically calls `Retain`.

### Plugin choice

`ChangeFeed` is internal to each plugin. A future `postgres-redpanda` plugin can layer redpanda on top of postgres storage via transactional-outbox + relay; the OSS postgres plugin uses LISTEN/NOTIFY. No external infrastructure required for OSS deployments.

## §6. Wire protocol & cursor semantics

### Wire format

Stream uses bare `CloudEvent` (no `ClientMessage`/`ServerMessage` wrapper).

**Server → client:**

| CloudEvent `type` | Notes |
|---|---|
| `EntityChangeEvent` | Single change event when `batching.windowMs == 0`. Data is the envelope JSON. |
| `EntityChangeBatch` | Batched events when batching enabled. Data is `{ events: [...], lastCursor: "..." }`. |
| `EntityChangeRebalance` | GROUP only. Carries redpanda-driven assignment changes. |
| `EntityChangeError` | Terminal error. Carries code + ticket UUID (5xx). |
| `EntityChangeHeartbeat` | Periodic keepalive when no events flow. |

**Client → server:**

| CloudEvent `type` | Notes |
|---|---|
| `EntityChangeAttachDurable` | `{ subscriptionId, consumerInstanceId, fromCursor?, generation? }`. `fromCursor`, if provided, must be ≥ last server-recorded ack (forward-skip allowed; rewind rejected). |
| `EntityChangeAttachEphemeral` | Inline subscription spec; no id; lives with the stream. |
| `EntityChangeAck` | `{ cursor }`. Cumulative ack. |
| `EntityChangeRebalanceAck` | GROUP only. Acks generation. |
| `EntityChangeDetach` | Graceful detach. |

Naming follows cyoda's existing PascalCase convention (`EntityCreateRequest`, `CalculationMemberJoinEvent`).

### Cursor semantics

Opaque, server-issued, monotone-within-partition. Base64 string on the wire. Subscribers never interpret.

- Monotonicity within a partition.
- No collation across partitions.
- Stable across reconnects.
- Within retention bounds — older than retention floor → `OUT_OF_RANGE` with earliest still-available cursor.

Internally cursor encodes `(partition_id, sequence)` where sequence is per-partition monotone, assigned by `AppendChange`. Wire format hides the structure.

### Ack & commit semantics

- At-least-once. Redelivery possible on disconnect.
- Cumulative acks.
- Ack window — default 1024 events or 16 MB, whichever first. **Special case**: if a single event exceeds the byte cap, the server sends it anyway (window can never block on a single event larger than the cap; otherwise the consumer stalls forever).
- Acks may be per-event, per-batch, or batched.

### Batching

When `batching.windowMs > 0`:
- Server accumulates per window or until `maxEvents`.
- Emits one `EntityChangeBatch` with `lastCursor`.
- Subscriber acks `lastCursor` to commit the batch.
- Whole batch replayed on disconnect.

In GROUP mode, batches respect partition boundaries.

### Heartbeats & keepalive

- 10s interval / 30s death detection — aligned with existing `internal/grpc/streaming.go:21-23`.

### Idempotency

`event_id` is the consumer's dedupe key. Required; at-least-once produces duplicates.

### Subscription resource changes mid-stream

REST PATCH that affects delivery (`includeBody`) triggers graceful stream close; client reconnects and picks up the new shape.

## §6.1. Bootstrap & preload bridging

### Bootstrap point

On `POST /tenants/{t}/subscriptions`:

```
effectiveStartFrom = clamp(
  requestedStartFromTimestamp ?? T_now,
  lower = max(enablementBoundary, T_now - maxLookback, retentionFloor),
  upper = T_now
)
```

Out-of-range → `400 Bad Request` with the valid range echoed back.

The subscription delivers every event with `emitted_at > effectiveStartFrom` matching the filter. **Strictly greater.**

`cursorZero` (first cursor that will be delivered) returned in the create response.

### Create response

```json
{
  "id": "sub_01H...",
  "createdAt": "2026-06-21T10:15:00.123Z",
  "effectiveStartFrom": "2026-06-21T10:15:00.000Z",
  "cursorZero": "<opaque>",
  "maxLookback": "15m",
  "enablementBoundary": { "tenantFirstEnabledAt": "..." }
}
```

### Preload-bridging pattern

```
1. T_preload = server-current-time
2. Query "entities matching <filter> as of T_preload" → preload cache
3. POST subscription with startFromTimestamp = T_preload
4. Consume stream — events from > T_preload forward
```

Strict-`>` on the subscription side plus `≤` on the as-of-timestamp query side gives a clean partition at exactly T_preload. **The "as of T" semantics must use `≤`** — this is the existing semantic in postgres and memory plugins. SQLite plugin currently uses `<` (after rounding); **this design aligns SQLite to `≤` as part of the implementation work**, addressed in §11.

### maxLookback

Cluster-configurable env var `CYODA_CHANGEFEED_MAX_LOOKBACK`, default 15 minutes. Outbox retention must be ≥ `maxLookback` (cross-checked at startup).

### What this enables

Applications combine preload + subscribe to build their own durable views or queues. cyoda's surface is a reliable change-data-capture tap.

## §7. Backpressure & slow-consumer policy

### In-band backpressure

The ack-window mechanism in §6 is primary. Server stops sending when un-acked window is full; consumer's pace drives the producer.

### Per-subscriber limits

| Limit | Default | Notes |
|---|---|---|
| Ack window — events | 1024 | Configurable per consumer |
| Ack window — bytes | 16 MB | Whichever trips first; single oversized event ships anyway |
| Per-consumer queue depth (SQLite/memory in-process) | 4096 | Local broadcaster buffer |
| Backlog ceiling | 1M events / 7d | Hard limit |
| Consumers per node | 1000 | Operational ceiling; reject attach beyond |

### Slow-consumer policies

| Policy | Default | Effect |
|---|---|---|
| `DETACH_CONSUMER` | yes | Consumer detached; must reconnect, accept any gap (`OUT_OF_RANGE` if past retention). |
| `BLOCK_APPEND` | no | Backpressures into the engine. **Not recommended**; explicit operator override only. A misbehaving consumer with this policy can stall writes engine-wide. |
| `DROP_OLDEST` | no | Cursor jumps forward; events silently lost. Explicit at-most-once degradation. |

Per-subscription via create-time policy field; PATCHable.

## §8. Auth & multitenancy

### Tenant scope

- `tenantId` always derived from JWT.
- All REST under `/tenants/{t}/...`; mismatch → `PERMISSION_DENIED`.
- gRPC attach validates subscription's `tenantId` matches JWT tenant.
- Outbox/log physically partitioned by tenant (postgres index, cassandra partition key).

### Role

**All subscription operations require `ROLE_M2M`** — matches `internal/grpc/streaming.go:58` precedent. No granular permissions invented for v1.

### Audit logging

- Subscription create/update/delete logged with tenant + actor + filter shape (model name, kinds, fromState, toState, bodyPredicate's structural metadata — operators used, paths referenced — not the raw values).
- Attach/detach logged with tenant + subscription id + consumer instance + generation.
- Per `.claude/rules/security.md`: never log credentials, tokens, secrets, signing keys, or body content of subscribed events.

## §9. Failure modes & recovery

§3.5 covers connection lifecycle.

### Storage / plugin failures

| Failure | Effect on emission | Effect on consume |
|---|---|---|
| **Storage tx rollback** | Entity write fails; change event never visible | No effect |
| **AppendChange returns error** | Engine `tx.Commit()` returns error; SPI tx rolls back; entity write fails too | No effect |
| **ChangeFeed.Consume backend error** | No effect on emission | Affected streams terminate with `INTERNAL` + ticket; reconnect retries |
| **Plugin restart** | Pending writes fail per existing semantics | Streams reconnect |
| **Cassandra publisher daemon lag** | Outbox rows committed; redpanda publishes lag; subscribers see delayed events; replay falls back to `entity_change_events` table | Bounded by daemon health monitoring |

### Cluster failures

| Failure | Recovery |
|---|---|
| **Single node death** | Subscribers reconnect to surviving nodes |
| **Network partition** | Existing cyoda cluster-membership protocol; delivery may pause on minority side |
| **Subscription cache-broadcast lost on peer node** | Next emission for unknown key cache-misses → storage read; correctness preserved |
| **Storage corruption** | Out of scope; same posture as any other cyoda data loss |

### Subscriber-induced failures

| Failure | Recovery |
|---|---|
| **Subscriber drops all acks** | Ack window fills; server stops sending; backlog ceiling eventually trips → `DETACH_CONSUMER` |
| **Subscriber acks ahead of received cursors** | `INVALID_ARGUMENT` |
| **Subscriber attaches outside retention** | `OUT_OF_RANGE` + earliest available cursor |
| **Constant-reconnect storm** | Existing cyoda rate-limit interceptor applies |
| **Generation-fencing race** (two pods attach with same `consumerInstanceId`) | Second attach increments generation; first is fenced (cannot ack); standard rolling-deploy pattern |

## §10. Out of scope for v1

- JSONPath body projection / JSON Patch diff in envelope.
- Failed-segment events.
- Processor outputs in envelope.
- Subscriber-overridable partition key.
- ChangeFeed as separately pluggable SPI capability.
- Pulling redpanda into OSS core.
- Cursor rewind / historical reprocessing.
- Unbounded historical replay.
- Substitute for queue subsystem.
- Webscale per-user-session subscriptions.
- Granular per-model permissions.
- GROUP mode on OSS backends.
- Cross-entity atomicity within a segment.
- Per-individual-transition events (per-segment with `transitions` list is v1).

## §11. Open verification items

Reduced from earlier draft based on investigation:

1. **SQLite `<` vs `≤` on as-of-timestamp** — sqlite plugin currently uses `<` (after millisecond rounding); needs to align to `≤` to match postgres + memory and enable the §6.1 preload bridging contract. **Action: include in v1 plan**, with a behavioural-change note in the changelog.
2. **Cassandra entity_change_events schema** — table design (partition key, clustering key, TTL strategy) needs concrete proposal in the plan. Investigation identified the natural slot in `tx_coordinator.go:863`; schema specifics pending.
3. **Cassandra publisher daemon** — process model (per-node? per-shard? singleton via leader election?) for the daemon that drains `entity_change_events` to redpanda. Plan-time decision; affects ops surface.
4. **Body cache placement in OSS plugins** — sqlite/postgres body hydration uses `entity_versions.doc` (JSONB / BLOB). The cache key is `(tenant_id, entity_id, version)`; need to verify the existing tx_writes/entity_versions schema doesn't create a hidden join cost in body re-reads.
5. **Composite outbox index in postgres/sqlite** — `(tenant_id, model_name, model_version, emitted_at DESC, version)`. Migration script must be added to the postgres/sqlite migrations chain.
6. **Non-transition writes** — confirmed not to exist today; if added in the future (admin overrides, bulk imports), invisibility decision is deferred.

### Resolved during this revision (no longer open)

| Original item | Status |
|---|---|
| `transaction_id` field surfacing | `EntityMeta.TransactionID` exists in `cyoda-go-spi/types.go:18`. |
| Search-DSL evaluator reuse | Reuse `spi.predicate.Condition` tree directly. |
| gRPC keepalive defaults | 10s/30s, matches `internal/grpc/streaming.go:21-23`. |
| Lifecycle-index version-pair body hydration | Postgres/SQLite `entity_versions` table + PK index suffices. |
| Multi-segment commits | Per-segment emission unit; transitions list inside envelope. |
| SPI signature `TxHandle` | Removed. `AppendChange(ctx, envelope)`; tx via `spi.GetTransaction(ctx)`. |
| Permission model invention | Removed. `ROLE_M2M` only, matching existing surface. |
| GROUP rebalance protocol design | Removed. Cassandra-only via redpanda's native group protocol. |
| Cluster-broadcast correctness gap | Local cache + storage backstop on miss; "fail-open" language removed. |
| Cassandra atomicity (no LWT) | Per-entity atomic via tag-grouped chunk in materialize batch. |
| Goroutine scale | Capped at 1000 consumers/node; operational ceiling. |
| LRU body cache scope | Bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES`, default 256 MB. |
| Outbox volume model | Per-segment-per-entity (lower than per-transition); composite index for time-range query. |
| `transaction_id` envelope shape | Each event carries one `transaction_id`; cross-entity segments share it. |

## §12. Extension hooks reserved

- `kind` enum extensibility — add `SEGMENT_FAILED` or other event kinds.
- `includeProcessorOutputs: bool` — populate per-transition processor outputs in each `TransitionRecord`.
- `partitionKey` override — JSONPath alternative to `entity_id`.
- JSONPath body projection / JSON Patch diff — new `includeBody` values.
- ChangeFeed as separately pluggable SPI capability.
- Admin "always on" tenant change feed for forensics.
- Granular per-model permissions if `UserContext` gains a permission layer.

## §13. Configuration env vars

Per `.claude/rules/documentation-hygiene.md`, every env var below requires updates to `cmd/cyoda/help/content/config/*.md`, `README.md`, and `DefaultConfig()` together.

| Env var | Default | Purpose |
|---|---|---|
| `CYODA_CHANGEFEED_MAX_LOOKBACK` | `15m` | Cap on `startFromTimestamp` past-clamp |
| `CYODA_CHANGEFEED_BODY_CACHE_BYTES` | `268435456` (256 MB) | Per-node body LRU cache cap |
| `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` | `1000` | Operational ceiling |
| `CYODA_CHANGEFEED_POLL_INTERVAL` | `10s` | Postgres missed-notify backstop |
| `CYODA_CHANGEFEED_RETENTION` | `7d` | Outbox retention; must be ≥ `MAX_LOOKBACK` |
| `CYODA_CHANGEFEED_BACKLOG_MAX_EVENTS` | `1000000` | Backlog ceiling |
| `CYODA_CHANGEFEED_BACKLOG_MAX_AGE` | `7d` | Backlog ceiling |
| `CYODA_CHANGEFEED_ACK_WINDOW_EVENTS` | `1024` | Per-consumer ack window |
| `CYODA_CHANGEFEED_ACK_WINDOW_BYTES` | `16777216` (16 MB) | Per-consumer ack window |
| `CYODA_CHANGEFEED_CACHE_TTL` | `30s` | Subscription-cache positive-entry TTL |

---

## Workflow context

This spec follows the standard cyoda flow per `CLAUDE.md`:

- Brainstorming → this spec.
- Next: `superpowers:writing-plans` to produce the implementation plan.
- Implementation track will be multi-PR. The plan partitions into reviewable units (SPI bump; cassandra outbox + daemon; OSS plugin ChangeFeed implementations; engine emission hook; REST CRUD; gRPC service; fan-out + filter/body machinery; documentation).
