# Subscription Framework — v1 Design

**Date:** 2026-06-21 (revisions 2026-06-22)
**Status:** Design — awaiting plan
**Scope:** v1 notification framework for cluster-of-application-nodes consumption. Not a substitute for a queue subsystem; not for webscale user-facing fan-out; not a historical reprocessing API; not a per-transition trajectory log (the entity audit / workflow event log is that).

## §1. Scope & vocabulary

A subscriber framework that lets client nodes (not just compute nodes) attach to cyoda over gRPC, declare a filter on entity model + workflow + body, and receive change events as **transaction segments commit** — with a per-subscription choice of ephemeral or durable delivery.

This is **a v1 notification framework, not a messaging or entity-event-queue framework.** Applications that need a durable, replayable, high-throughput queue subsystem build it on their own infrastructure. A future cyoda-native entity-event-queue would be a separate capability built on a real queue substrate.

The change event publishes the **segment-level summary** — what the entity looked like at the start of the segment, what it looks like after, and the user-invoked transition name. The intermediate trajectory inside a cascade is **not** published; subscribers needing per-step detail query the entity audit / workflow event log.

### Vocabulary

| Term | Meaning |
|---|---|
| **Segment** | A single transactional commit in cyoda's engine. `EngineResult.Segmented=true` means a request spanned multiple segments via `COMMIT_BEFORE_DISPATCH` processors. Within one segment, multiple workflow transitions can have cascaded (via `cascadeAutomated`). The segment is the natural emission unit. |
| **Change event** | One emission per **(segment commit, entity)** pair carrying envelope (`from_state` at segment-start, `to_state` at segment-end, `transition_name` of the user-invoked transition, `kind`, `transaction_id`, etc.). Optionally `body_before` / `body_after`. |
| **Subscription** | A server-side declaration: filter + delivery mode + consumption mode + batching + body-include policy. Ephemeral (lives with the stream) or durable (persists across connections). |
| **Delivery mode** | `LIVE_ONLY` (no replay; ephemeral) or `DURABLE` (at-least-once delivery for events emitted after subscription start, replay possible from the subscription's last-committed cursor within outbox retention). |
| **Consumption mode** | `BROADCAST` (each attached consumer gets every event) or `GROUP` (events partition across consumers by `entity_id`). Ephemeral is always `BROADCAST`. **GROUP is only available on backends advertising `GroupConsumption=true` — in v1 that is the cassandra plugin only**, which delegates to redpanda's Kafka-compatible consumer-group machinery. |
| **ChangeFeed** | SPI capability each storage plugin implements internally. Atomic event append + filtered consume. Capabilities advertised per-backend. |
| **Cursor** | Opaque server-issued token; monotone within a partition. Subscriber commits to advance. Handles **ordering**. |
| **`event_id`** | Deterministic per-(transaction, entity) idempotency key. Format: `<transaction_id>:<entity_id>`. Recovery replays produce the identical id so consumer-side dedupe is correctness-stable. **Idempotency only — not an ordering primitive.** |
| **`startFromTimestamp`** | Optional subscription field. Permits bounded preload bridging — server-clamped by `maxLookback`. |
| **Bootstrap point** | The `(createdAt, cursorZero, effectiveStartFrom)` tuple returned on subscription create. Defines the strict-`>` boundary between "preload territory" and "subscription territory". |

### Non-goals for v1

- Per-transition trajectory inside a cascade. The change event carries segment-summary only (`from_state` → `to_state` plus user-invoked `transition_name`). Subscribers needing the intermediate cascade steps use the entity audit / workflow event log.
- JSONPath body projection / JSON Patch diff in the envelope.
- Failed-segment events (extensibility hook reserved on `kind`).
- Processor outputs in the envelope (extensibility hook reserved).
- Subscriber-overridable partition key for GROUP mode (`entity_id` only).
- ChangeFeed as a separately pluggable SPI capability.
- Pulling redpanda into OSS core.
- Cursor rewind / historical reprocessing.
- Unbounded historical replay. Bounded preload bridging up to `maxLookback` is in scope.
- A substitute for a real durable queue subsystem.
- Webscale per-user-session subscriptions. Operational target is **clusters of application nodes**.
- Granular per-model permissions. v1 reuses `ROLE_M2M`.
- GROUP mode on OSS backends. v1 does not design a custom Kafka-style rebalance protocol.
- Cross-entity atomicity of change-event emission within a segment.
- `LifecycleCondition` inside `bodyPredicate`.
- Time-sortable `event_id`.

## §2. Subscription model

### Shape

```json
{
  "id": "sub_01H...",
  "tenantId": "...",                    // derived from JWT
  "filter": {
    "modelName": "ORDER",
    "modelVersion": "1.0",
    "kinds":      ["CREATE","UPDATE","DELETE"],
    "fromState":  null,                  // direct match on envelope.from_state
    "toState":    "PAID",                // direct match on envelope.to_state
    "bodyPredicate": { ... }            // spi.predicate.Condition; LifecycleCondition forbidden
  },
  "delivery":    "LIVE_ONLY" | "DURABLE",
  "consumption": "BROADCAST" | "GROUP",
  "includeBody": "NONE" | "AFTER" | "BEFORE_AND_AFTER",
  "batching": { "windowMs": 100, "maxEvents": 500 },
  "startFromTimestamp": "2026-06-21T10:15:00Z",
  "ttl": "30d",
  "createdAt": "...",
  "lastAttachedAt": "..."
}
```

### Filter semantics

All filter fields are direct matches against the segment-summary envelope:

- `kinds` — matches `envelope.kind` (`CREATE | UPDATE | DELETE`).
- `fromState` — matches `envelope.from_state` (the entity's state at the start of the segment).
- `toState` — matches `envelope.to_state` (the entity's state after the segment commits).
- `bodyPredicate` — evaluated at fan-out against `body_after` using the existing `spi.predicate.Condition` tree. The validator walks the tree recursively and **rejects `LifecycleCondition` at any depth** (including inside `GroupCondition.Conditions`). `FunctionCondition` is also rejected for v1 (it's a placeholder). `SimpleCondition`, `GroupCondition`, `ArrayCondition` are allowed.

Subscribers needing intermediate-state visibility ("ORDER passed through VALIDATING during the cascade") use the entity audit log, **not** the change feed.

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
| `batching.windowMs` | `0` | predictable latency |
| `startFromTimestamp` | `T_now` at create | live-only by default |
| `delivery` | (required) | force the deliberate durability decision |
| `consumption` | `BROADCAST` | covers stated use cases |
| `ttl` | `30d` | durable only |

### Identity rules

- `tenantId` always derived from JWT, never accepted from client.
- Subscription `id`: server-generated ULID for durable subs.
- `GROUP` consumer joins identify with a client-chosen `consumerInstanceId` + server-managed `generation`. Server increments `generation` on every successful attach for that subscription. Two attaches with the same `consumerInstanceId`: the new generation supersedes; the older generation is fenced (cannot ack). Rolling-deploy safe.
- `lastAttachedAt`: updated on each successful **attach**. Preserved across detach. Used for TTL math (`now - lastAttachedAt > ttl`).

### TTL behaviour

- Durable subscription with no consumer attached for `ttl` is auto-deleted. Default 30 days.
- At auto-delete: retention claim released atomically; outbox cleanup follows the sweeper.

## §3. API surface — REST CRUD + gRPC stream

### REST management plane

All endpoints scoped under the tenant from JWT.

| Method & path | Purpose | Notes |
|---|---|---|
| `POST   /tenants/{t}/subscriptions` | Create durable subscription | Validates `bodyPredicate` recursively rejecting `LifecycleCondition` / `FunctionCondition`. Returns 201 + full resource + `cursorZero` + `effectiveStartFrom` + `maxLookback` + `enablementBoundary`. |
| `GET    /tenants/{t}/subscriptions/{id}` | Read subscription | Full resource + lag metrics. |
| `GET    /tenants/{t}/subscriptions` | List subscriptions | Paginated. |
| `PATCH  /tenants/{t}/subscriptions/{id}` | Update subscription | Mutable: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter / delivery / consumption / startFromTimestamp immutable. PATCH takes effect for events delivered after the PATCH commit; in-flight or buffered events keep their original shape. |
| `DELETE /tenants/{t}/subscriptions/{id}` | Delete subscription | Detaches consumers, deletes cursor state, releases retention claim. Idempotent. |

Ephemeral subscriptions have no REST surface.

**`subscriptions` table is intentionally not row-level-security protected** — tenant scoping enforced at the application/SQL layer (matching cyoda's existing entity-store pattern). This permits the system-level existence-set bootstrap (§4) to scan across tenants efficiently without `BYPASSRLS` or `SECURITY DEFINER` indirection.

Errors follow existing cyoda conventions.

### gRPC stream — `EntityChangeService`

```proto
service EntityChangeService {
  rpc Stream(stream io.cloudevents.v1.CloudEvent) returns (stream io.cloudevents.v1.CloudEvent);
}
```

Streams bare `CloudEvent` directly, matching `CloudEventsService.startStreaming` idiom.

One stream = one attached subscription.

### Auth

- REST + gRPC: existing JWT bearer + tenant scope. **All subscription operations require `ROLE_M2M`** — same surface as `internal/grpc/streaming.go:58`. No new permission system.
- Cross-tenant attach → `PERMISSION_DENIED`.
- Tenant isolation is implicit via existing patterns.

## §3.5. Connection lifecycle & recovery

### Scenario matrix

| Scenario | Ephemeral | Durable |
|---|---|---|
| **Clean detach** | State dropped. | Cursor at last ack persisted. GROUP: partitions reassigned via redpanda group protocol. |
| **Network blip** | State dropped after keepalive timeout. | Cursor at last ack; GROUP rebalance is redpanda's. Reconnect with same `subscriptionId`+`consumerInstanceId` resumes; server bumps generation. |
| **Client restart / pod replacement** | New ephemeral subscription. | New `consumerInstanceId` joining = redpanda rebalance for GROUP, no-op for BROADCAST. Generation fencing prevents zombie acks from prior pod. |
| **Server node dies** | Stream errors `UNAVAILABLE`. | Reconnect to another node; subscription resource lives in shared storage. |
| **Long disconnection** | Subscription gone with stream. | Replay from last cursor if within retention; else `OUT_OF_RANGE` with earliest still-available cursor. |
| **TTL exceeded** | N/A | Subscription auto-deleted; reconnect = `NOT_FOUND`. |
| **Mid-batch disconnect** | Batch lost. | Batch redelivered. Consumer dedupes via `event_id`. |

### Mechanism

All authoritative state lives in the storage tier (durable) or in-process on the attached node (ephemeral). gRPC stream death triggers fencing/grace; reconnect rehydrates state. At-least-once requires `event_id` dedupe.

For GROUP on cassandra, rebalance is **delegated to redpanda's consumer-group protocol** (`KafkaSubscribe`/`KafkaCommit` already used by the cassandra plugin).

### Operational knobs

| Knob | Default | Notes |
|---|---|---|
| gRPC keepalive interval | 10s | `internal/grpc/streaming.go:21`. |
| gRPC keepalive timeout | 30s | `internal/grpc/streaming.go:23`. |
| GROUP session timeout | `RedpandaSessionTimeoutSec` (default 30s) | Configured in cassandra plugin's redpanda config. |
| Backlog ceiling | 1M events / 7d | Hard ceiling for un-acked outbox backlog. Applies at durable-retention level, not in-process queue (in-process queue is limited separately, §7). |
| Backlog ceiling policy | `DETACH_CONSUMER` | Per-consumer-instance detach. For GROUP, redpanda rebalances detached consumer's partitions to peers. |
| Consumers per node | 1000 | Operational ceiling. |

## §4. Event source & emission

### Source

Single emission point: **the engine's segment commit hook**. One segment commit emits one event **per entity** touched in that segment. The envelope is the segment-level summary (state at segment-start → state at segment-end; the user-invoked transition that started the segment).

Engine semantics (verified in `internal/domain/workflow/engine.go`, `engine_processors.go`, `internal/domain/entity/service.go`):
- One user request = 1+ segments (`EngineResult.Segmented=true` if `COMMIT_BEFORE_DISPATCH` ran).
- One segment = exactly one tx commit.
- One segment can contain multiple cascaded transitions on the same entity (`cascadeAutomated`).
- One segment can touch multiple entities (one event per entity emitted).

### Emission call site (reconciling engine vs. plugin)

- **Memory / SQLite / Postgres**: the engine constructs the envelope and calls `ChangeFeed.AppendChange(ctx, envelope)` **directly inside the SPI tx**, before commit. The plugin writes to its outbox in the same tx.
- **Cassandra**: the engine cannot call `AppendChange` synchronously because cassandra's commit machinery runs in the TX coordinator goroutine, potentially on a different node. Instead, the engine **populates envelope-source data** (the user-invoked transition name; from-state is derivable from existing `PrevData`; to-state is `CommitEntityInfo.State`) via the existing `CommitRequest` message. The coordinator builds the envelope and writes `entity_change_events` inside the materialize batch in the same tag group as the entity's other writes. See §4.4.

The atomicity contract is the plugin's commit mechanism, not `AppendChange`'s return.

### Atomicity

Per-entity within a segment:

- **Cassandra**: `entity_change_events` row written in the existing materialize batch in the same tag group as the entity's other writes (`tx_coordinator.go materialize()`). Per-entity statements share a tag → same chunk → atomic at per-entity row level via `USING TIMESTAMP <HLC>` fencing. **Redpanda is async fan-out, not the atomicity boundary**.
- **Postgres/SQLite**: outbox row in the same SQL tx.
- **Memory**: pushed to in-process channel after the in-memory write. Tx-aware.

**Atomicity claim**: if `txMgr.Commit` succeeds, the change event for each entity in the segment will eventually be visible to subscribers. Cross-entity atomicity within one segment is **not** claimed.

### Cross-entity partial-commit recovery

Cassandra's materialize chunks are independent; entity A's chunk can succeed while entity B's fails. Recovery (`MaterializeFromLog`) replays the failed entities using the original HLC. The replay reconstructs `CommitEntityInfo` from `tx_writes` (which holds `Data, PrevData, State, Version` — sufficient to derive `from_state` from `PrevData._meta.state`, `to_state` from `State`, `kind` from `(PrevData, Data)`). `event_id` is deterministic (`<transaction_id>:<entity_id>`), so the replayed emission has the same id as the original would have — consumer-side dedupe is correctness-stable.

Note: the user-invoked transition name is part of the small wire-format extension on `CommitEntityInfo` (§4.4); it persists in `tx_writes` as well, so recovery has full fidelity.

### Emission gate

The engine emits a per-entity change event at segment commit **if and only if** both:

1. The entity's `(tenant, model_name, model_version)` has at least one subscription (existence check), AND
2. At least one subscription's envelope filter matches the segment for that entity. Short-circuit on first match. Body predicates are **not** evaluated at emission.

### Subscription cache strategy (two-tier)

**Tier 1 — Existence set (per node, in-memory):**
- Type: `map[(tenant, model, version)] → has_subscriptions bool`.
- Bootstrap: at node startup, run `SELECT DISTINCT tenant_id, model_name, model_version FROM subscriptions` once (subscriptions is not RLS-protected; bootstrap is direct). Populate the set.
- Maintenance: subscription create/delete on any node broadcasts via `ClusterBroadcaster`; receiving nodes set/unset the key.
- Emission fast-path: existence-set lookup → `false` means skip emission. **No storage read for unknown keys in steady state.**
- Periodic full-sync default 60s replays the bootstrap query, recovering from broadcast loss.

**Tier 2 — Positive-content cache (per node, in-memory, TTL):**
- Type: `map[(tenant, model, version)] → [filter set]` with TTL 30s.
- Populated when existence set says `true` and content cache misses: read `subscriptions WHERE tenant=… AND model=… AND version=…`, cache for 30s.
- On subscription create/delete, broadcast invalidates the entry.

**Staleness consequence**: a subscription created at time T can see up to `EXISTENCE_SYNC_INTERVAL` (default 60s) of missed emissions on nodes that didn't receive the create-broadcast. Documented; configurable.

### Envelope construction

```
ChangeEvent {
  event_id        string      "<transaction_id>:<entity_id>" — deterministic, idempotency only
  tenant_id       string
  transaction_id  string      EntityMeta.TransactionID (cyoda-go-spi/types.go:18)
  cursor          string      opaque base64; monotone within partition; assigned by AppendChange
  emitted_at      time.Time   server clock at segment commit
  model_name      string
  model_version   string
  entity_id       string
  kind            ChangeKind  CREATE | UPDATE | DELETE
  from_state      string      entity state at segment-start (derived from PrevData._meta.state; empty on CREATE)
  to_state        string      entity state at segment-end (derived from Data._meta.state; empty on hard-DELETE)
  transition_name string      user-invoked transition name (preserved through cascade; see §4.4)
  body_after      *EntityBody attached at fan-out, derived from CommitEntityInfo.Data
  body_before     *EntityBody attached at fan-out, derived from CommitEntityInfo.PrevData
}
```

**`kind` derivation** from `(PrevData, Data)` alone:
- `CREATE` — `PrevData == nil && Data != nil`
- `DELETE` — `PrevData != nil && Data == nil` (hard-delete)
- `UPDATE` — otherwise (includes soft-deletes to terminal states; subscribers filter on `to_state` for those)

**Note**: `body_before` and `body_after` are not part of the outbox row itself; they're hydrated at fan-out from the corresponding entity-version (postgres/sqlite: `entity_versions` PK lookup; cassandra: lifecycle index by `(entity_id, version)`). `PrevData` is reused — no new `BodyBefore` wire field added.

### What is not emitted in v1

- Per-individual-transition events (the segment summary is the contract).
- Failed segments (rolled-back tx).
- Non-transition writes.
- Cross-entity correlation in one envelope (one event per entity; events from one segment share `transaction_id`).

## §4.1. Per-(tenant, model, version) enablement boundary

- **First subscription** for a key:
  1. Inserts the subscription row,
  2. Sets the enablement boundary marker for that key to `T_now`,
  3. Broadcasts existence-set + content-cache invalidation.
- Marker is the floor for subsequent `startFromTimestamp` on the same key.
- Surfaced as `enablementBoundary.tenantFirstEnabledAt` in the create response.
- Atomicity: row + marker in same storage tx. Broadcast is best-effort; periodic full-sync (60s) is the backstop.

## §4.2. Filtering & body materialisation

### Pipeline per event E and candidate subscription S

1. **Envelope match** — direct field comparison on `kind`, `from_state`, `to_state`. Pure metadata; no body needed.
2. **Body predicate match** (if S has one) — `spi.predicate.Condition` evaluated against `body_after` at fan-out. Requires body load.
3. **Ship** — envelope and (per `includeBody`) `body_before` / `body_after`.

### Body load contract

The entity body is loaded at fan-out **if and only if** at least one local consumer has either a body predicate OR `includeBody != NONE`.

Bodies LRU-cached. Cache key: **`(tenant_id, entity_id, version)`** — tenant always in key; matches postgres RLS on `entity_versions`; prevents cross-tenant cache poisoning. Bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB).

**Eviction safety**: cached bodies are `[]byte`. When an in-flight batch captures a body, it holds the `[]byte` reference directly; cache eviction removes the map entry but Go's GC keeps the underlying array alive as long as the batch references it. No custom refcount needed; rely on Go GC.

**Documented cost**: `bodyPredicate` + `includeBody: NONE` triggers a body load (predicate eval requires the body).

### Pre-state body (`body_before`) — comes from `PrevData`

For cassandra: `CommitEntityInfo.PrevData` already carries the body at segment-start. The change-event fan-out hydrates `body_before` from the same source the engine had (or, for replay, from the lifecycle index via the version pair).

For postgres/sqlite: hydrated from the prior `entity_versions` row.

For memory: from the prior in-memory version.

No new `BodyBefore` wire field is added.

### Outbox row contents

The outbox row stores the envelope (less bodies). Bodies looked up at fan-out by `(tenant_id, entity_id, version)`. The version comes from `EntityMeta.Version` (post-segment) and `version - 1` (pre-segment).

For outbox time-range scans (consume path on postgres/sqlite), add composite index `(tenant_id, model_name, model_version, sequence, emitted_at)` — `sequence` first inside the partition so cursor reads are pure index seeks. `emitted_at` for time-range filters.

### Subscription mutation invariants

PATCH-mutable: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter/delivery/consumption/startFromTimestamp immutable. PATCH applies forward; in-flight/buffered events keep original shape.

## §4.3. Cardinality and outbox volume

- A segment touching N entities → N change events sharing `transaction_id`.
- A segment with M cascaded transitions on one entity → exactly **1** change event (segment-summary). Cascade fidelity is in the entity audit log, not the change feed.
- A user request spanning S segments → ≥ S events.

Outbox volume scales with **entity-update rate**, not transition rate. Cascade-heavy workflows do not amplify outbox cost.

## §4.4. Small wire-format extension on cassandra

Cassandra's commit pipeline runs in the TX coordinator goroutine, potentially on a different node from the engine. To deliver the user-invoked `transition_name` to the coordinator (where the change-event row is built), `CommitEntityInfo` (`cyoda-go-cassandra/internal/queue/tx_message.go`) gains one field:

```go
type CommitEntityInfo struct {
    // ... existing: Data, PrevData, State, Version, ...
    SegmentInvokedTransition string  // user-invoked transition name; preserved through cascade
}
```

`tx_writes` schema gains the same column so recovery (`MaterializeFromLog`) sees it.

Engine-side: `TransactionState` gains `SegmentInvokedTransition string` field set once at segment start, never overwritten by cascade. Snapshot/restore semantics: participates in `Savepoint` / `RollbackToSavepoint` like `WriteSet` / `ReadSet`. Each plugin's savepoint implementation needs a small update.

**Backward compatibility**: `CommitEntityInfo` is JSON-encoded; the new field is `omitempty`. Rolling upgrade is byte-compatible. Semantic gap during rollout: until both engine and coordinator are upgraded, change events emitted during the window have empty `transition_name`. Documented.

This is the **only** wire-format extension required — much smaller than the previously-drafted full `Transitions[]` + `BodyBefore` extension, which is no longer needed.

`from_state` and `to_state` come from existing `PrevData._meta.state` (cassandra has `ExtractMetaState` helper at `internal/store/index_engine.go`) and `CommitEntityInfo.State`. `kind` derives from `(PrevData, Data)`. No further wire fields needed.

## §5. SPI ChangeFeed capability

### Interface

```go
package spi

import "context"

// ChangeFeed is an optional capability. Plugins not implementing it advertise
// no ChangeFeed support; subscriptions cannot be created on those backends.
type ChangeFeed interface {
    // AppendChange writes the change event atomically with the entity write in
    // the current transaction. The transaction is retrieved from ctx via the
    // standard SPI pattern (spi.GetTransaction(ctx)).
    //
    // Per-entity atomicity: if the SPI tx commits, this event is durable and
    // will eventually be visible to consumers. Cross-entity atomicity within
    // one segment is NOT guaranteed.
    //
    // Implementation note: the atomicity boundary is the plugin's commit
    // mechanism (cassandra: materialize batch; postgres/sqlite: SQL tx;
    // memory: in-process channel), not this function's return.
    //
    // envelope is value-passed (small struct ~few hundred bytes).
    AppendChange(ctx context.Context, envelope ChangeEnvelope) error

    // Consume returns a stream of change events matching spec.
    Consume(ctx context.Context, spec ConsumerSpec) (ChangeEventStream, error)

    // Retain/Release manage retention claims for subscriptions.
    Retain(ctx context.Context, claim RetentionClaim) error
    Release(ctx context.Context, subscriptionID string) error

    // Capabilities reports backend capabilities. Called at plugin init only;
    // implementations must return static values. Runtime config changes
    // require restart for capability changes to take effect.
    Capabilities() ChangeFeedCapabilities
}

type ChangeEnvelope struct {
    EventID         string        // "<transaction_id>:<entity_id>"
    TenantID        string
    TransactionID   string
    Cursor          string        // assigned by AppendChange
    EmittedAt       time.Time
    ModelName       string
    ModelVersion    string
    EntityID        string
    Kind            ChangeKind
    FromState       string
    ToState         string
    TransitionName  string
    BodyVersionRef  VersionRef    // {EntityID, Version} for body hydration
}

type ConsumerSpec struct {
    TenantID            string
    EnvelopeFilter      EnvelopeFilter
    StartCursor         string
    GroupID             string                  // empty for BROADCAST
    PartitionAssignment *PartitionAssignment    // GROUP only; backend-specific
}

type ChangeFeedCapabilities struct {
    Durable          bool
    GroupConsumption bool
    MaxRetention     time.Duration
    MaxBacklogPerSub int
}
```

### Implementation guidance

- Errors wrapped with `fmt.Errorf("…: %w", err)`.
- Logging via `log/slog` only.
- Tenant scoping enforced at storage layer.
- Body hydration queries carry `tenant_id` in the lookup key (matches postgres RLS, prevents cross-tenant cache poisoning).

### Per-backend implementation

| Backend | AppendChange | Consume | Retain/Release | Capabilities |
|---|---|---|---|---|
| **memory** | push to in-process channel; tx-aware (rollback discards) | range over channel buffer with envelope filters | no-op | `Durable=false, GroupConsumption=false`, retention=0 |
| **sqlite** | INSERT into `change_outbox` in the SQL tx | in-process broadcaster signals post-commit; per-consumer bounded queues; catchup-by-cursor on attach; no polling | UPDATE claim row; sweep daemon | `Durable=true, GroupConsumption=false`, retention configurable |
| **postgres** | INSERT into `change_outbox` in the SQL tx; `pg_notify` post-commit | `LISTEN/NOTIFY` low-latency + periodic poll (default 10s, `CYODA_CHANGEFEED_POSTGRES_POLL_INTERVAL`) backstop. Composite index on outbox. | claim row + sweep daemon | `Durable=true, GroupConsumption=false`, retention configurable |
| **cassandra** | Engine threads `SegmentInvokedTransition` through `CommitEntityInfo`; coordinator builds envelope at materialize and writes to `entity_change_events` table in the same tag group as the entity write. Atomic per-entity via HLC `USING TIMESTAMP`. Publisher daemon drains `entity_change_events` → redpanda for fan-out + GROUP. | redpanda consumer-group via existing `KafkaSubscribe`. Replay older than redpanda retention reads `entity_change_events` directly. | redpanda offset commits for GROUP; table TTL | `Durable=true, GroupConsumption=true`, retention = max(redpanda retention, table TTL) |

### Cassandra `entity_change_events` schema (sketch — concrete in plan)

```cql
CREATE TABLE entity_change_events (
    tenant_id        text,
    model_name       text,
    model_version    text,
    bucket_hour      int,           -- floor(emitted_at / 1 hour) for time-range scans
    hlc              bigint,        -- monotone within partition
    entity_id        text,
    transaction_id   uuid,
    from_state       text,
    to_state         text,
    transition_name  text,
    kind             tinyint,       -- 0=CREATE, 1=UPDATE, 2=DELETE
    body_version     bigint,        -- for hydration via lifecycle index
    PRIMARY KEY ((tenant_id, model_name, model_version, bucket_hour), hlc, entity_id)
) WITH default_time_to_live = 604800;  -- 7d default; configurable
```

Partition key by `(tenant, model, version, bucket_hour)` — tenant-isolated, time-range scannable, single hour bucket per partition keeps partitions bounded.

### Cassandra publisher daemon

Drains `entity_change_events` → redpanda. Leader election: **uses redpanda single-partition consumer-group as the election mechanism** (the consumer-group owner of the publisher's coordination topic is the publisher). No separate leader-election primitive needed. On publisher failure, redpanda rebalances the partition to another node; recovery latency = `RedpandaSessionTimeoutSec` (default 30s) + restart.

**Latency consequence for live BROADCAST consumers**: during publisher leadership transfer (≤ ~30-60s), live consumers see delivery pause; outbox rows persist; replay catches up after leadership restores. Documented operational caveat.

Publisher lag exposed via metric `changefeed.publisher_lag_seconds`.

### GROUP consumption — cassandra-only via redpanda

- Redpanda topics partitioned by `entity_id` hash for per-entity ordering.
- Consumer groups handled by redpanda natively.

OSS plugins advertise `GroupConsumption=false`; subscription create with `GROUP` on OSS → `400 Bad Request`.

### Fan-out layer (engine-internal)

- One goroutine per attached consumer per node, capped at `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` (default 1000).
- LRU body cache bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB).
- Tracks per-consumer ack cursor; periodic `Retain` calls.

## §6. Wire protocol & cursor semantics

### Wire format

Bare `CloudEvent` stream. CloudEvent `type` values (PascalCase + suffix per cyoda convention):

**Server → client:**

| `type` | Notes |
|---|---|
| `EntityChangeEvent` | Single event when batching disabled. |
| `EntityChangeBatchEvent` | Batched events; data is `{events: [...], lastCursor: ...}`. |
| `EntityChangeRebalanceEvent` | GROUP only. Carries redpanda-driven assignment. |
| `EntityChangeErrorEvent` | Terminal error; code + ticket UUID (5xx). |
| `EntityChangeHeartbeatEvent` | Periodic keepalive. |

**Client → server:**

| `type` | Notes |
|---|---|
| `EntityChangeAttachDurableRequest` | `{subscriptionId, consumerInstanceId, fromCursor?, generation?}`. `fromCursor` ≥ last ack (forward-skip allowed). |
| `EntityChangeAttachEphemeralRequest` | Inline subscription spec. |
| `EntityChangeAckRequest` | Cumulative ack. |
| `EntityChangeRebalanceAckRequest` | GROUP. Acks generation. |
| `EntityChangeDetachRequest` | Graceful detach. |

### Cursor semantics

Opaque, server-issued, monotone within a partition. Base64 wire shape. Subscribers never interpret.
- Within retention bounds.
- Stable across reconnects.
- Encodes `(partition_id, sequence)` internally. For BROADCAST mode on non-partitioned backends, `partition_id = 0` (uniform encoding).

### Ack & commit semantics

- At-least-once.
- Cumulative acks.
- Ack window default 1024 events / 16 MB. **Single oversized event ships anyway** (never block on one event exceeding the byte cap).

### Batching

`batching.windowMs > 0` → server accumulates per window or until `maxEvents`. Whole batch replayed on disconnect.

### Heartbeats

10s/30s aligned with `streaming.go:21-23`.

### Idempotency

`event_id` (deterministic `<tx_id>:<entity_id>`) is the dedupe key. The `:` separator is unambiguous (UUID strings contain no `:`).

## §6.1. Bootstrap & preload bridging

### Bootstrap point

```
effectiveStartFrom = clamp(
  requestedStartFromTimestamp ?? T_now,
  lower = max(enablementBoundary, T_now - maxLookback, retentionFloor),
  upper = T_now
)
```

The subscription delivers events with `emitted_at > effectiveStartFrom`. **Strictly greater.**

`cursorZero` returned on create.

### Preload-bridging pattern

The change feed pairs with cyoda's **existing entity APIs** for preload. The pattern in v1:

```
1. T_preload = server-current-time
2. Subscribe FIRST with startFromTimestamp = T_preload
3. Begin buffering subscription events client-side (live but not yet processed)
4. Query entity state via existing API:
   - For a known set of entities: GetAsAt(entityID, T_preload) per entity
   - For all entities matching a model: GetAllAsAt(modelRef, T_preload)
   - For filtered queries: existing search API (note: "search as-of timestamp" is not a v1 cyoda primitive — applications doing this either filter client-side or use the GetAllAsAt + filter pattern)
5. Apply preload result + buffered events to local cache
```

The strict-`>` boundary on the subscription side + `≤` semantics on the `GetAsAt` / `GetAllAsAt` side give a clean partition at `T_preload`. Existing semantics across plugins are effectively `≤` at millisecond resolution (postgres uses `valid_time <= T`, memory uses `!After(T)`; sqlite uses `<` at sub-millisecond, identical at ms resolution).

**Note on `bodyPredicate` and preload**: v1 does not provide a server-side "search-with-bodyPredicate as-of T" endpoint. Applications combining `bodyPredicate` filtering with preload must either (a) fetch a superset via `GetAllAsAt` and filter client-side, or (b) accept that preload covers entity-state without body-predicate narrowing. Future work could add an as-of search endpoint.

### maxLookback

`CYODA_CHANGEFEED_MAX_LOOKBACK` default 15 minutes. Outbox retention ≥ `maxLookback`.

## §7. Backpressure & slow-consumer policy

### Per-subscriber limits

| Limit | Default | Notes |
|---|---|---|
| Ack window events | 1024 | Per-consumer. |
| Ack window bytes | 16 MB | Whichever first; single oversized event ships. |
| Per-consumer queue depth (in-process broadcaster) | 4096 | SQLite/memory in-process. |
| Backlog ceiling (durable outbox) | 1M events / 7d | Per-consumer-instance (NOT per-group). |
| Consumers per node | 1000 | Operational ceiling. |

### Layering

- Ack window is the in-band backpressure mechanism (live).
- In-process queue depth limits per-consumer buffer between broadcaster and stream send (only on memory/sqlite in-process fan-out).
- Backlog ceiling applies at durable storage retention — relevant for long-disconnected durable consumers, not live ones.

### Memory floor (per node, defaults)

- 1000 consumers × 4096 queue × ~512 B envelope ≈ **2 GB worst case** before ack-window backpressure kicks in.
- Plus 256 MB body cache.
- **Defaults sized for ≥4 GB nodes.** Smaller deployments must reduce `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` or queue depth.

(Envelope is ~512 B; segment-summary shape — `from_state`, `to_state`, `transition_name` strings + fixed-size IDs — has no cascade-dependent growth. The prior estimate of 8 KB was based on the now-dropped `transitions[]` list.)

### Slow-consumer policies

| Policy | Default | Effect |
|---|---|---|
| `DETACH_CONSUMER` | yes | Per-consumer-instance detach. GROUP: redpanda rebalances. |
| `BLOCK_APPEND` | no | **Not recommended**; explicit operator override only. |
| `DROP_OLDEST` | no | Explicit at-most-once degradation. |

## §8. Auth & multitenancy

### Tenant scope

- `tenantId` always from JWT.
- Outbox/log physically partitioned by tenant.
- Body hydration includes `tenant_id` in the lookup key.

### Role

**All subscription operations require `ROLE_M2M`**. No granular permissions invented.

### Subscriptions table RLS

Intentionally **not RLS-protected** at the row level. Tenant scoping enforced at the application/SQL WHERE-clause layer, matching cyoda's existing entity-store patterns. Permits the existence-set bootstrap (§4) to scan across tenants without privileged role indirection.

### Audit logging

- Subscription create/update/delete logged with tenant + actor + filter shape (model name, kinds, fromState, toState, bodyPredicate's **structural shape** — operators used, path **depth/shape only**, e.g. `$.string.string`, never actual paths like `$.creditCardNumber`).
- Attach/detach logged with tenant + subscription id + consumer instance + generation.
- Per `.claude/rules/security.md`: never log credentials, tokens, secrets, signing keys, or event body content.

## §9. Failure modes & recovery

### Storage / plugin failures

| Failure | Effect |
|---|---|
| Storage tx rollback | Entity write + change event both fail. |
| AppendChange returns error | SPI tx rolls back. |
| ChangeFeed.Consume backend error | Affected streams terminate `INTERNAL` + ticket; reconnect. |
| Cassandra publisher daemon lag/death | Outbox rows persist; redpanda publishes delayed; replay falls back to table. Daemon leadership re-elects via redpanda group. Live consumers may see delivery pause ≤ `RedpandaSessionTimeoutSec + restart`. Lag metric exposed. |

### Cluster failures

| Failure | Recovery |
|---|---|
| Single node death | Subscribers reconnect to surviving nodes. |
| Network partition | Existing cluster protocol; delivery may pause on minority. |
| Subscription cache broadcast lost | Periodic full-sync (60s default) refreshes existence set. Bounded staleness; subscribers may see up to 60s of missed deliveries on un-synced nodes after a subscription creation. |
| Storage corruption | Out of scope. |

### Subscriber-induced

| Failure | Recovery |
|---|---|
| All acks dropped | Ack window fills → server stops → backlog ceiling trips → `DETACH_CONSUMER`. |
| Ack ahead of received cursor | `INVALID_ARGUMENT`. |
| Attach outside retention | `OUT_OF_RANGE` + earliest cursor. |
| Reconnect storm | Existing rate-limit interceptor. |
| Generation race (two pods, same consumerInstanceId) | Server bumps generation; older fenced; rolling-deploy safe. |

## §10. Out of scope for v1

- Per-transition trajectory inside a cascade. Segment-summary only; audit log carries trajectory.
- JSONPath projection / JSON Patch diff.
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
- GROUP on OSS backends.
- Cross-entity atomicity within a segment.
- `LifecycleCondition` and `FunctionCondition` inside `bodyPredicate`.
- Time-sortable `event_id`.
- Server-side "search-with-bodyPredicate as-of T" endpoint for preload bridging.

## §11. Open verification items (plan-time)

1. **Cassandra `entity_change_events` schema** — sketch in §5; concrete partitioning, clustering, TTL strategy pending.
2. **Cassandra publisher daemon** — single-partition redpanda coordination topic for leader election; offset tracking schema; concrete design pending.
3. **CommitEntityInfo `SegmentInvokedTransition` extension** — per §4.4. JSON `omitempty`; tx_writes column add; recovery test coverage.
4. **Engine `TransactionState.SegmentInvokedTransition`** — small SPI extension; per-plugin Savepoint/RollbackToSavepoint integration; bundled with PR 1.
5. **Outbox composite index for postgres/sqlite** — `(tenant_id, model_name, model_version, sequence, emitted_at)` — column ordering may need refinement based on cursor encoding decision.
6. **SQLite as-of sub-millisecond `≤` alignment** — optional parity cleanup, not a prerequisite (functional equivalence at ms resolution). Lands independently when convenient.

### Resolved across three review iterations

| Item | Resolution |
|---|---|
| `transaction_id` field surfacing | `EntityMeta.TransactionID` exists at `cyoda-go-spi/types.go:18`. |
| Search-DSL evaluator reuse | `spi.predicate.Condition` tree via `internal/match/match.go:Match`. `LifecycleCondition` + `FunctionCondition` forbidden in `bodyPredicate`; recursive walker rejects at any depth. |
| gRPC keepalive defaults | 10s/30s, aligned with `streaming.go:21-23`. |
| Lifecycle-index body hydration | postgres/sqlite `entity_versions` PK index. |
| Multi-segment commits | Per-segment emission; segment-summary envelope (`from_state`, `to_state`, `transition_name`). |
| SPI signature `TxHandle` | Removed. `AppendChange(ctx, envelope)`; tx via `spi.GetTransaction(ctx)`. |
| Permission model invention | Removed. `ROLE_M2M` only. |
| GROUP rebalance protocol | Cassandra-only via redpanda's native protocol. |
| Cluster-broadcast correctness gap | Existence set bootstrap + content cache + periodic full-sync. |
| Cassandra atomicity (no LWT) | Per-entity atomic via tag-grouped chunk in materialize batch. |
| Goroutine scale | Capped at 1000/node; memory floor ~2 GB worst case. |
| LRU body cache scope | Bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES`. Tenant_id in lookup key. Eviction safety via Go GC + batch byte-slice refs. |
| Outbox volume model | Per-segment-per-entity; composite index for time-range query. |
| `event_id` determinism | `<transaction_id>:<entity_id>`; deterministic; zero collision risk by concatenation. Idempotency only, not ordering. |
| `LifecycleCondition` + EntityMeta | Forbidden in bodyPredicate; recursive walker. |
| Engine→cassandra data plane | Reduced to single field (`SegmentInvokedTransition`) on `CommitEntityInfo`; from_state derived from `PrevData._meta.state`; to_state from `State`; kind from `(PrevData, Data)`. |
| Subscription cache thundering herd | Existence set bootstrap eliminates per-key storage reads at steady state. |
| GROUP backlog interaction | Per-consumer-instance detach; redpanda handles partition rebalance. |
| Body hydration tenant scoping | Explicit `tenant_id` in lookup key. |
| bodyPredicate path audit redaction | Path depth/shape only. |
| `Capabilities()` call timing | Init-time only; static; restart required for config changes. |
| `AppendChange` envelope copy cost | Value-pass; small struct. |
| `kind=DELETE` ambiguity | Derived directly from `(PrevData, Data)`. |
| `lastAttachedAt` semantics | Updated on attach; preserved on detach. |
| `transitions[]` over-design | Removed. Segment-summary only. |
| `BodyBefore` field redundancy | Removed. `body_before` from `PrevData`. |
| Recovery replay fidelity | Replay reconstructs envelope from `tx_writes` (which carries `SegmentInvokedTransition`); same `event_id`; same envelope shape. |
| `subscriptions` table RLS | Intentionally not RLS-protected; app-layer tenant scoping (matches existing pattern). |
| PR 0 SQLite ≤ prerequisite | Dropped; functional equivalence at ms resolution. |
| Existence-set staleness window | Documented; subscribers may see ≤ 60s missed deliveries on un-synced nodes after subscription create; configurable via `CYODA_CHANGEFEED_EXISTENCE_SYNC_INTERVAL`. |
| Publisher leader election | Redpanda single-partition consumer group; no new primitive needed. |
| Wire-format rolling upgrade | JSON `omitempty`; byte-compatible; semantic gap during rollout window documented (empty `transition_name`). |
| Preload bridging API mapping | Existing `GetAsAt` / `GetAllAsAt`; client-side filter for `bodyPredicate`-narrowed preload; "search as-of T" deferred. |
| Cache eviction safety | Go GC + batch byte-slice refs. |
| COMPATIBILITY.md | Required on PR 1 (SPI bump) and PR 7 (cassandra wire-format extension). |
| CloudEvent type naming | PascalCase + Event/Request suffix per cyoda convention. |

## §12. Extension hooks reserved

- `kind` enum extensibility (e.g., `SEGMENT_FAILED`).
- `includeProcessorOutputs` — populate processor outputs in the envelope.
- `partitionKey` JSONPath override.
- JSONPath projection / JSON Patch diff.
- ChangeFeed as separately pluggable SPI capability.
- Admin "always on" tenant change feed.
- Granular per-model permissions if `UserContext` gains a permission layer.
- "Search-with-bodyPredicate as-of T" endpoint for richer preload.
- Per-transition events (would be a separate, additional event type; not a replacement for segment-summary).

## §13. Configuration env vars

Per `.claude/rules/documentation-hygiene.md` (Gate 4), **every env var below requires synchronised updates to:**
1. `cmd/cyoda/help/content/config/*.md` (relevant topic — likely new `config/changefeed.md`)
2. `README.md` configuration section
3. `DefaultConfig()` in the appropriate Go config struct

| Env var | Default | Purpose |
|---|---|---|
| `CYODA_CHANGEFEED_MAX_LOOKBACK` | `15m` | `startFromTimestamp` past-clamp |
| `CYODA_CHANGEFEED_BODY_CACHE_BYTES` | `268435456` (256 MB) | Per-node body LRU cap |
| `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` | `1000` | Operational ceiling |
| `CYODA_CHANGEFEED_POSTGRES_POLL_INTERVAL` | `10s` | Postgres missed-notify backstop |
| `CYODA_CHANGEFEED_RETENTION` | `7d` | Outbox retention; must be ≥ `MAX_LOOKBACK` |
| `CYODA_CHANGEFEED_BACKLOG_MAX_EVENTS` | `1000000` | Backlog ceiling |
| `CYODA_CHANGEFEED_BACKLOG_MAX_AGE` | `7d` | Backlog ceiling |
| `CYODA_CHANGEFEED_ACK_WINDOW_EVENTS` | `1024` | Per-consumer ack window |
| `CYODA_CHANGEFEED_ACK_WINDOW_BYTES` | `16777216` (16 MB) | Per-consumer ack window |
| `CYODA_CHANGEFEED_CACHE_TTL` | `30s` | Positive content-cache TTL |
| `CYODA_CHANGEFEED_EXISTENCE_SYNC_INTERVAL` | `60s` | Periodic full-sync of existence set |
| `CYODA_CHANGEFEED_QUEUE_DEPTH` | `4096` | Per-consumer in-process queue depth (sqlite/memory) |

## §14. Test strategy

### TDD per CLAUDE.md Gate 1

Failing tests drive every component. Scoped per plan-PR; verification per CLAUDE.md.

### E2E per Gate 2

User-facing behaviour (REST CRUD, gRPC stream, error codes) covered by E2E tests in `internal/e2e/`. Pattern follows existing testcontainers postgres + httptest server. New tests:
- Subscription CRUD lifecycle.
- gRPC attach (durable + ephemeral; broadcast + group on cassandra).
- At-least-once delivery via simulated disconnect.
- Backlog ceiling tripping → `DETACH_CONSUMER`.
- Preload-bridging via `startFromTimestamp` end-to-end.
- Tenant isolation cross-check.
- Cascade segment emits one envelope, not multiple.
- Recovery-replay produces same `event_id` and envelope shape.

### Parity per existing pattern

Storage-plugin parity tests in `e2e/parity/registry.go` extend to cover `ChangeFeed`:
- `AppendChange` atomicity.
- `Consume` cursor semantics + envelope filter.
- `Retain` / `Release` retention claim accounting.
- Body hydration by version pair.

Cassandra plugin picks up new parity tests automatically via the registry.

### SPI tests

`cyoda-go-spi/spitest/` gets `ChangeFeed` contract tests (interface conformance, error semantics, capability enumeration).

### gRPC stream tests

Follow existing `internal/grpc/streaming_test.go` pattern for new `EntityChangeService`.

### Race detector

Per CLAUDE.md, `go test -race ./...` is end-of-deliverable for the final PR series.

---

## Workflow context

Standard cyoda flow per `CLAUDE.md`:
- Brainstorming → this spec (three fresh-context review iterations applied).
- Next: `superpowers:writing-plans` to produce the implementation plan.
- Implementation track is multi-PR:
  - **PR 1**: SPI ChangeFeed interface + `TransactionState.SegmentInvokedTransition` + `EntityMeta` clarifications. Bundled into one SPI tag per `feedback_spi_coordinated_release_procedure`. Includes `COMPATIBILITY.md` update.
  - **PR 2**: Memory + SQLite ChangeFeed implementations. Engine emission for non-cassandra path.
  - **PR 3**: Postgres ChangeFeed implementation + outbox composite index migration.
  - **PR 4**: REST CRUD endpoints + auth + audit logging + `bodyPredicate` validator (recursive walker).
  - **PR 5**: gRPC `EntityChangeService` + fan-out goroutine + filter/body machinery + body LRU cache.
  - **PR 6**: Subscription cache (existence set + content cache + periodic sync + broadcast invariants).
  - **PR 7**: Cassandra `CommitEntityInfo.SegmentInvokedTransition` extension + `tx_writes` schema migration. **Cross-repo PR pair** (cyoda-go-cassandra change → tag → cyoda-go pin bump). Includes `COMPATIBILITY.md`.
  - **PR 8**: Cassandra `entity_change_events` table + materialize integration + publisher daemon + redpanda consumer-group integration for GROUP.
  - **PR 9**: Documentation, help topics (`config/changefeed.md`), env-var wiring, README configuration section, COMPATIBILITY.md final sync.

PR 0 (sqlite ≤ alignment) is **dropped from the prerequisite chain** — functional equivalence at millisecond resolution. It lands independently as a parity cleanup whenever convenient.
