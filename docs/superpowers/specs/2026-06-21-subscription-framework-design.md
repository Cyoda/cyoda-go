# Subscription Framework — v1 Design

**Date:** 2026-06-21 (revisions 2026-06-22)
**Status:** Design — awaiting plan
**Scope:** v1 notification framework for cluster-of-application-nodes consumption. Not a substitute for a queue subsystem; not for webscale user-facing fan-out; not a historical reprocessing API.

## §1. Scope & vocabulary

A subscriber framework that lets client nodes (not just compute nodes) attach to cyoda over gRPC, declare a filter on entity model + workflow + body, and receive change events as **transaction segments commit** — with a per-subscription choice of ephemeral or durable delivery.

This is **a v1 notification framework, not a messaging or entity-event-queue framework.** Applications that need a durable, replayable, high-throughput queue subsystem build it on their own infrastructure. A future cyoda-native entity-event-queue would be a separate capability built on a real queue substrate.

### Vocabulary

| Term | Meaning |
|---|---|
| **Segment** | A single transactional commit in cyoda's engine. `EngineResult.Segmented=true` means a request spanned multiple segments via `COMMIT_BEFORE_DISPATCH` processors. Within one segment, multiple workflow transitions can have cascaded (via `cascadeAutomated`). The segment is the natural emission unit. |
| **Change event** | One emission per **(segment commit, entity)** pair, carrying envelope and an ordered list of transitions that ran in that segment for that entity. |
| **Subscription** | A server-side declaration: filter + delivery mode + consumption mode + batching + body-include policy. Ephemeral (lives with the stream) or durable (persists across connections). |
| **Delivery mode** | `LIVE_ONLY` (no replay; ephemeral) or `DURABLE` (at-least-once delivery for events emitted after subscription start, replay possible from the subscription's last-committed cursor within outbox retention). |
| **Consumption mode** | `BROADCAST` (each attached consumer gets every event) or `GROUP` (events partition across consumers by `entity_id`). Ephemeral is always `BROADCAST`. **GROUP is only available on backends advertising `GroupConsumption=true` — in v1 that is the cassandra plugin only**, which delegates to redpanda's Kafka-compatible consumer-group machinery. |
| **ChangeFeed** | SPI capability each storage plugin implements internally. Atomic event append + filtered consume. Capabilities advertised per-backend. |
| **Cursor** | Opaque server-issued token; monotone within a partition. Subscriber commits to advance. Handles **ordering**. |
| **`event_id`** | Deterministic per-(tenant, transaction, entity) idempotency key. Format: `<transaction_id>:<entity_id>`. Recovery replays produce the identical id so consumer-side dedupe is correctness-stable. **Idempotency only — not an ordering primitive.** |
| **`startFromTimestamp`** | Optional subscription field. Permits bounded preload bridging — server-clamped by `maxLookback`. |
| **Bootstrap point** | The `(createdAt, cursorZero, effectiveStartFrom)` tuple returned on subscription create. Defines the strict-`>` boundary between "preload territory" and "subscription territory". |

### Non-goals for v1

- JSONPath body projection / JSON Patch diff in the envelope.
- Failed-segment events (extensibility hook reserved on `kind`).
- Processor outputs in the envelope (extensibility hook reserved).
- Subscriber-overridable partition key for GROUP mode (`entity_id` only).
- ChangeFeed as a separately pluggable SPI capability.
- Pulling redpanda into OSS core.
- Cursor rewind / historical reprocessing.
- Unbounded historical replay. Bounded preload bridging up to `maxLookback` (cluster-configurable, default 15 minutes) is in scope.
- A substitute for a real durable queue subsystem.
- Webscale per-user-session subscriptions. Operational target is **clusters of application nodes** (BFFs, cache invalidators, integration sinks). v1 caps active consumers per node.
- Granular per-model permissions. v1 reuses `ROLE_M2M`, matching the existing compute-node gRPC streaming surface.
- GROUP mode on OSS backends (memory/sqlite/postgres). v1 does not design a custom Kafka-style rebalance protocol.
- Cross-entity atomicity of change-event emission. Atomicity is **per-entity within a segment** (matching cassandra's natural guarantee).
- `LifecycleCondition` inside `bodyPredicate`. State-based filtering uses envelope fields (§2). `bodyPredicate` is purely data-focused.

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
    "fromState":  null,
    "toState":    "PAID",
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

### Filter semantics for segment-shaped events

- `kinds` — matches the segment-derived `kind`.
- `fromState` — matches **any** transition's `from` in the segment.
- `toState` — matches `final_state` **OR** any transition's `to` in the segment.
- `bodyPredicate` — evaluated at fan-out against `body_after`. Uses `spi.predicate.Condition` tree (SimpleCondition / GroupCondition). **`LifecycleCondition` is rejected at create with `400 Bad Request`** (`internal/match/match.go:Match` requires `EntityMeta` for lifecycle eval; envelope doesn't carry full meta; state-based filtering uses envelope fields instead — the envelope already exposes everything state-related via `final_state`, `transitions[*]`, `fromState`, `toState`, `kinds`).

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
- `GROUP` consumer joins identify with a client-chosen `consumerInstanceId` + server-managed `generation`. Two attaches with the same `consumerInstanceId`: server increments generation; the older generation is fenced (cannot ack); rolling-deploy safe.
- `lastAttachedAt`: updated on each successful **attach** (not on each ack, not on detach). On detach, the field is preserved at the attach time so reattach within TTL keeps the subscription alive. TTL countdown uses `now - lastAttachedAt`.

### TTL behaviour

- Durable subscription with no consumer attached for `ttl` (computed as `now - lastAttachedAt > ttl`) is auto-deleted. Default 30 days.
- At auto-delete: retention claim released atomically; outbox cleanup follows the sweeper.
- Backlog ceiling applies separately (§3.5). `DETACH_CONSUMER` default.

## §3. API surface — REST CRUD + gRPC stream

### REST management plane

All endpoints scoped under the tenant from JWT.

| Method & path | Purpose | Notes |
|---|---|---|
| `POST   /tenants/{t}/subscriptions` | Create durable subscription | Validates `bodyPredicate` rejects `LifecycleCondition`. Returns 201 + full resource + `cursorZero` + `effectiveStartFrom` + `maxLookback` + `enablementBoundary`. |
| `GET    /tenants/{t}/subscriptions/{id}` | Read subscription | Returns full resource + lag metrics. |
| `GET    /tenants/{t}/subscriptions` | List subscriptions | Paginated. |
| `PATCH  /tenants/{t}/subscriptions/{id}` | Update subscription | Mutable: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter, delivery, consumption, startFromTimestamp immutable. |
| `DELETE /tenants/{t}/subscriptions/{id}` | Delete subscription | Detaches consumers, deletes cursor state, releases retention claim. Idempotent. |

Ephemeral subscriptions have no REST surface.

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
| **Network blip** | State dropped after keepalive timeout. | Cursor at last ack; GROUP rebalance is redpanda's; reconnect with same `subscriptionId`+`consumerInstanceId` resumes (server bumps generation). |
| **Client restart / pod replacement** | New ephemeral subscription. | New `consumerInstanceId` joining = redpanda rebalance for GROUP, no-op for BROADCAST. Generation fencing prevents zombie acks from the prior pod. |
| **Server node dies** | Stream errors `UNAVAILABLE`. | Stream errors `UNAVAILABLE`. Reconnect to another node; subscription resource lives in shared storage. |
| **Long disconnection** | Subscription gone with stream. | Replay from last cursor if within retention; else `OUT_OF_RANGE` with earliest still-available cursor. |
| **TTL exceeded** | N/A | Subscription auto-deleted; reconnect = `NOT_FOUND`. |
| **Mid-batch disconnect** | Batch lost. | Batch redelivered. Consumer dedupes via `event_id`. |

### Mechanism

All authoritative state — subscription resource, cursor per consumer instance, partition assignments — lives in the storage tier (durable) or in-process on the attached node (ephemeral). gRPC stream death triggers fencing/grace; reconnect rehydrates state. At-least-once requires `event_id` dedupe.

For GROUP on cassandra, rebalance is **delegated to redpanda's consumer-group protocol** (`KafkaSubscribe`/`KafkaCommit` already used by the cassandra plugin).

### Operational knobs

| Knob | Default | Notes |
|---|---|---|
| gRPC keepalive interval | 10s | Matches `internal/grpc/streaming.go:21`. |
| gRPC keepalive timeout | 30s | Matches `internal/grpc/streaming.go:23`. |
| GROUP session timeout | redpanda default | Inherited. |
| Backlog ceiling | 1M events / 7d | Hard ceiling for un-acked backlog. |
| Backlog ceiling policy | `DETACH_CONSUMER` | Per-consumer-instance detach (NOT group-level). For GROUP, redpanda then rebalances the detached consumer's partitions to peers. Group-level health is the application's concern; we don't trip whole groups on one slow member. |
| Consumers per node | 1000 | Operational ceiling. |

## §4. Event source & emission

### Source

Single emission point: **the engine's segment commit hook** (in plugin `txMgr.Commit`, after `materialize()` succeeds). One segment commit emits one event **per entity** touched in that segment.

Engine semantics (verified in `internal/domain/workflow/engine.go`, `engine_processors.go`, `internal/domain/entity/service.go`):
- One user request = 1+ segments (`EngineResult.Segmented=true` if `COMMIT_BEFORE_DISPATCH` ran).
- One segment = exactly one tx commit.
- One segment can contain multiple cascaded transitions on the same entity (`cascadeAutomated`).
- One segment can touch multiple entities (in which case one event per entity is emitted).

### Atomicity

Per-entity within a segment:

- **Cassandra**: change event row written to a new `entity_change_events` table in the existing materialize batch, in the same tag group as the entity's other writes (`tx_coordinator.go materialize()`, slot identified around line 863 — see §4.4). Per-entity statements share a tag → same chunk → atomic at the per-entity row level via `USING TIMESTAMP <HLC>` fencing. **Redpanda is async fan-out, not the atomicity boundary**.
- **Postgres/SQLite**: change event row in the same SQL tx as the entity write.
- **Memory**: pushed to in-process channel after the in-memory write. Tx-aware (rollback discards).

**Atomicity claim**: if `txMgr.Commit` succeeds, the change event for each entity in the segment will eventually be visible to subscribers. Cross-entity atomicity within one segment is **not** claimed.

### Cross-entity partial-commit recovery

Cassandra's materialize chunks are independent; entity A's chunk can succeed while entity B's fails. The cassandra recovery path (`MaterializeFromLog`) replays the failed entities using the original HLC. Because `event_id` is deterministic (`<transaction_id>:<entity_id>`), recovery replay produces the **same** event id as the original would have — consumer-side dedupe sees them as one event. No "duplicates that look distinct" failure mode.

### Emission gate

The engine emits per-entity change event at segment commit **iff** both:

1. The entity's `(tenant, model_name, model_version)` has at least one subscription (existence check), AND
2. At least one subscription's envelope filter matches the segment for that entity. Short-circuit on first match. Body predicates are **not** evaluated at emission.

### Subscription cache strategy (two-tier)

Per the C4 fork: **existence set + positive-content cache**.

**Tier 1 — Existence set (per node, in-memory):**
- Type: `map[(tenant, model, version)] → has_subscriptions bool`.
- Bootstrap: at node startup, run `SELECT DISTINCT tenant_id, model_name, model_version FROM subscriptions` once. Populate the set.
- Maintenance: subscription create/delete on any node broadcasts via `ClusterBroadcaster`; receiving nodes set/unset the key.
- Emission fast-path: existence-set lookup → `false` means skip emission. **No storage read for unknown keys in steady state.**
- Broadcast loss recovery: on subscription create, the originating node both writes-and-broadcasts. If a peer misses the broadcast, the existence set is stale (`false` where it should be `true`). Mitigation: periodic full-sync (default 60s) replays the bootstrap query; bounded staleness.

**Tier 2 — Positive-content cache (per node, in-memory, TTL):**
- Type: `map[(tenant, model, version)] → [filter set]` with TTL 30s.
- Populated on emission when existence set says `true` and the content cache misses: read `subscriptions WHERE tenant=… AND model=… AND version=…`, cache for 30s.
- Used by the emission gate's filter-match step (step 2 above).
- On subscription create/delete, broadcast invalidates the entry.

**Removes the previous "fail-open on cache unavailable" framing.** The cache is two Go maps. Cold-start storms are bounded: existence set bootstraps once with a single query; content cache reads only happen for keys with at least one subscription.

### Envelope construction

```
ChangeEvent {
  event_id        string                          deterministic: "<transaction_id>:<entity_id>"
  tenant_id       string                          from request context
  transaction_id  string                          EntityMeta.TransactionID (cyoda-go-spi/types.go:18)
  cursor          string (opaque base64)          assigned by ChangeFeed.AppendChange; monotone within partition
  emitted_at      time.Time                       server clock at segment commit
  model_name      string
  model_version   string
  entity_id       string                          (SPI uses string, not uuid.UUID)
  kind            ChangeKind                      CREATE | UPDATE | DELETE — derived from segment
  transitions     []TransitionRecord              ordered list: {from, to, transitionName}
  final_state     string                          last transition's to_state
  body_after      *EntityBody                     attached at fan-out, not at emission
  body_before     *EntityBody                     attached at fan-out, not at emission
}

TransitionRecord {
  from           string
  to             string
  transition_name string
}
```

**`event_id` is deterministic (`<transaction_id>:<entity_id>`).** Recovery replay produces identical id. It is **purely an idempotency key**; ordering is the cursor's responsibility, not event_id's. ULID format is not required (and was over-engineered in earlier drafts).

**`kind` derivation:**
- `CREATE` — if the first transition was initial-state entry (no prior version of the entity).
- `DELETE` — if the segment ended with `info.Data == nil` (cassandra's hard-delete marker) or with the entity entering a workflow state marked for deletion.
- `UPDATE` — otherwise.

**`final_state`** = `transitions[-1].to`; empty on hard-delete.

**`body_before`** = entity body before the segment's first transition. See §4.4 for how this flows from engine to coordinator on cassandra.

**`body_after`** = entity body after the segment commit.

### What is not emitted in v1

- Failed segments (rolled-back tx).
- Per-individual-transition events (per-segment with `transitions` list is the v1 contract).
- Non-transition writes.
- Cross-entity correlation in one envelope. Each entity is its own change event; events from one segment share `transaction_id`.

## §4.1. Per-(tenant, model, version) enablement boundary

- **First subscription** for a key:
  1. Inserts the subscription row,
  2. Sets the enablement boundary marker for that key to `T_now`,
  3. Broadcasts existence-set + content-cache invalidation.
- Marker is the floor for subsequent `startFromTimestamp` on the same key.
- Surfaced as `enablementBoundary.tenantFirstEnabledAt` in the create response.
- Atomicity: row + marker in same storage tx. Broadcast is best-effort; periodic full-sync (60s) is the backstop on broadcast loss.

## §4.2. Filtering & body materialisation

### Pipeline per event E and candidate subscription S

1. **Envelope match** — pure metadata.
2. **Body predicate match** — `spi.predicate.Condition` evaluated against `body_after` at fan-out. Requires body load.
3. **Ship** — envelope and (per `includeBody`) `body_before` / `body_after`.

### Body load contract

The entity body is loaded at fan-out **iff** at least one local consumer has either a body predicate OR `includeBody != NONE`.

Bodies LRU-cached. Cache key: `(tenant_id, entity_id, version)`. **Tenant_id is always part of the lookup key**, both for cache and for storage queries (matches postgres row-level security on `entity_versions` and prevents cross-tenant leakage).

Cache bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB). On eviction during in-flight batch use: the in-flight batch has already captured a reference; cached body is GC-eligible only after the batch ships. The cache only evicts *available* (unreferenced) entries.

**Documented cost**: body-predicate + `includeBody: NONE` triggers a body load.

### Pre-state capture (body_before) — engine-side

The engine has the pre-state on the stack during the first transition of a segment. To make it available at emission, the engine captures `body_before` in `TransactionState`:

- `TransactionState.SegmentBodyBefore map[entity_id] []byte` — populated at the **start** of each segment for each entity touched.
- Snapshot/restore semantics with SAVEPOINTs: `SegmentBodyBefore` participates in savepoint snapshots like `WriteSet` / `ReadSet`. A rollback restores the pre-segment body-before (which is what subscribers should see — the rollback undid intermediate state, the "before" is still the pre-segment state).
- For multi-segment requests: each segment's `body_before` is "body at start of that segment" = "body at end of prior segment". Engine refreshes `SegmentBodyBefore` at segment boundaries.

### Outbox row vs. envelope shape

The outbox row stores envelope fields. Bodies looked up at fan-out by `(tenant_id, entity_id, version)`:

- Postgres/SQLite: `entity_versions` table — PK is `(tenant_id, entity_id, version)`. No new index needed for hydration. For outbox time-range scans (consume path), add composite index `(tenant_id, model_name, model_version, emitted_at DESC, version)`.
- Cassandra: lifecycle index by `(entity_id, version)` per-tenant scoping via tenant_id in the partition key.

### Subscription mutation invariants

PATCH-mutable: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter/delivery/consumption/startFromTimestamp immutable.

## §4.3. Cardinality and outbox volume

For a segment touching N entities → emits N change events (one per entity), all sharing `transaction_id`.
For a segment with M cascaded transitions on one entity → emits 1 event with `transitions` list of length M.
For a user request spanning S segments → emits ≥ S events (more if multi-entity).

Outbox volume scales with **entity-update rate**, not transition rate. Cascade-heavy workflows produce fewer outbox rows than a per-transition design would.

## §4.4. Engine → commit data plane (cassandra-specific)

On cassandra, the materialize batch runs in the TX coordinator's redpanda-consumer goroutine (`tx_coordinator.go:798`), potentially on a different node from the engine. The existing wire format `CommitEntityInfo` (`queue/tx_message.go`) carries `Data, PrevData, State, Version` — no transitions list, no `body_before`, no per-transition names.

To make per-segment emission work on cassandra, the wire format must be extended:

### Wire-format extension

```go
// New fields on CommitEntityInfo (cassandra plugin)
type CommitEntityInfo struct {
    // ... existing fields ...
    Transitions []TransitionRecord  // ordered list of cascaded transitions in the segment
    BodyBefore  []byte              // entity body at the start of the segment, nil if first transition was CREATE
}

type TransitionRecord struct {
    From           string
    To             string
    TransitionName string
}
```

### Engine-side population

The engine maintains `TransactionState.SegmentBodyBefore` and `TransactionState.SegmentTransitions[entity_id]`. As each transition in a segment commits its post-state, the engine appends a `TransitionRecord` to the entity's slice. At submit time (when the engine builds the `CommitRequest` for the coordinator), both fields are serialised into `CommitEntityInfo`.

### Coordinator-side deserialisation

The TX coordinator deserialises the new fields and passes them through to `materialize()`, which constructs the `ChangeEnvelope` for `entity_change_events` insertion (same tag group as the entity write — per-entity atomic).

### Effort budget

This is a non-trivial cross-repo change:
- `cyoda-go-cassandra` wire format (`queue/tx_message.go`) — schema migration, version bump.
- Engine-side `TransactionState` extension — touches `cyoda-go-spi` (TransactionState carries plugin-aware fields? or this is encapsulated in an engine-internal struct?).
- Serialisation/deserialisation tests + parity tests.

Plan-time decision: whether to bundle this into the cassandra-ChangeFeed PR or land it as a prerequisite PR.

### Non-cassandra backends

For memory/sqlite/postgres, the engine and the SPI tx commit happen in the same goroutine on the same node — no wire-format crossing. The engine constructs the `ChangeEnvelope` directly and calls `ChangeFeed.AppendChange(ctx, envelope)` inside the SPI tx. No extension needed.

## §5. SPI ChangeFeed capability

### Interface

```go
package spi

import "context"

// ChangeFeed is an optional capability. Plugins not implementing it advertise
// no ChangeFeed support; subscriptions cannot be created on those tenants/backends.
type ChangeFeed interface {
    // AppendChange writes the change event atomically with the entity write in
    // the current transaction. The transaction is retrieved from ctx via the
    // standard SPI pattern (spi.GetTransaction(ctx)).
    //
    // Per-entity atomicity: if the SPI tx commits, this event is durable and
    // will eventually be visible to consumers. Cross-entity atomicity within
    // one segment is NOT guaranteed (see §4 atomicity).
    //
    // Implementation note: the atomicity boundary is the plugin's commit
    // mechanism (cassandra: materialize batch; postgres/sqlite: SQL tx;
    // memory: in-process channel), not this function's return.
    //
    // envelope is value-passed; size is small (envelope ~few hundred bytes
    // plus transitions slice header; bodies are not in the envelope).
    AppendChange(ctx context.Context, envelope ChangeEnvelope) error

    // Consume returns a stream of change events matching spec.
    Consume(ctx context.Context, spec ConsumerSpec) (ChangeEventStream, error)

    // Retain/Release manage retention claims for subscriptions.
    Retain(ctx context.Context, claim RetentionClaim) error
    Release(ctx context.Context, subscriptionID string) error

    // Capabilities reports backend capabilities. Called at plugin init only;
    // implementations must return static values. Treat as memoized.
    Capabilities() ChangeFeedCapabilities
}

type ChangeEnvelope struct {
    EventID        string                 // "<transaction_id>:<entity_id>"
    TenantID       string
    TransactionID  string
    Cursor         string                 // assigned by AppendChange
    EmittedAt      time.Time
    ModelName      string
    ModelVersion   string
    EntityID       string
    Kind           ChangeKind
    Transitions    []TransitionRecord
    FinalState     string
    BodyVersionRef VersionRef             // {EntityID, Version} for body hydration
}

type ConsumerSpec struct {
    TenantID            string
    EnvelopeFilter      EnvelopeFilter
    StartCursor         string
    GroupID             string                 // empty for BROADCAST
    PartitionAssignment *PartitionAssignment   // GROUP mode only; backend-specific
}

type ChangeFeedCapabilities struct {
    Durable          bool
    GroupConsumption bool
    MaxRetention     time.Duration
    MaxBacklogPerSub int
}
```

### Implementation guidance

- Errors wrapped with `fmt.Errorf("…: %w", err)` per CLAUDE.md.
- Logging via `log/slog` only.
- Tenant scoping enforced at storage layer: every query carries `tenant_id`; no SPI method ever returns cross-tenant data.

### Per-backend implementation

| Backend | AppendChange | Consume | Retain/Release | Capabilities |
|---|---|---|---|---|
| **memory** | push to in-process channel; tx-aware (rollback discards) | range over channel buffer with envelope filters | no-op | `Durable=false, GroupConsumption=false`, retention=0 |
| **sqlite** | INSERT into `change_outbox` in the SQL tx | in-process broadcaster signals post-commit; per-consumer bounded queues; catchup-by-cursor on attach; no polling in normal operation | UPDATE claim row; sweep daemon | `Durable=true, GroupConsumption=false`, retention configurable |
| **postgres** | INSERT into `change_outbox` in the SQL tx; `pg_notify` post-commit | `LISTEN/NOTIFY` low-latency + periodic poll (default 10s) backstop. New composite index for time-range queries. | claim row + sweep daemon | `Durable=true, GroupConsumption=false`, retention configurable |
| **cassandra** | INSERT into `entity_change_events` table inside the existing materialize batch, same tag group as entity writes (atomic per-entity via HLC fencing). Engine→coordinator data plane extended per §4.4. A publisher daemon drains `entity_change_events` → redpanda for fan-out + GROUP. | redpanda consumer-group via existing `KafkaSubscribe`. Replay older than redpanda retention reads `entity_change_events` table directly. | redpanda offset commits for GROUP; table TTL | `Durable=true, GroupConsumption=true`, retention = max(redpanda retention, table TTL) |

### Cassandra publisher daemon

Drains `entity_change_events` → redpanda. Process model:
- Per-node singleton, elected via existing cluster-membership (one publisher across the cluster avoids duplicate publishes).
- Reads `entity_change_events` in HLC order; publishes to the appropriate redpanda topic keyed by entity_id.
- Publisher lag is observability-monitored (`changefeed.publisher_lag_seconds` metric).
- On publisher failure, leadership re-elects; new publisher resumes from last-committed publish offset stored in cassandra.

### GROUP consumption — cassandra-only via redpanda

- Redpanda topics partitioned by `entity_id` hash for per-entity ordering.
- Consumer groups handled by redpanda natively.
- Plugin advertises `GroupConsumption=true`.

OSS plugins advertise `false`; subscription create with `GROUP` on OSS → `400 Bad Request`.

### Engine-side contract

- Engine calls `AppendChange` once per entity per segment commit, inside the SPI tx.
- For non-cassandra backends, the engine constructs the envelope locally.
- For cassandra, the engine populates `TransactionState.SegmentBodyBefore` + `SegmentTransitions` per §4.4; the coordinator materialises.

### Fan-out layer (engine-internal)

- One goroutine per attached consumer per node, capped at `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` (default 1000).
- LRU body cache bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB).
- Tracks per-consumer ack cursor; periodic `Retain` calls.

## §6. Wire protocol & cursor semantics

### Wire format

Bare `CloudEvent` stream. CloudEvent `type` values (PascalCase + Event suffix matching cyoda convention):

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

Opaque, server-issued, monotone within a partition. Subscribers never interpret.
- Within retention bounds.
- Stable across reconnects.
- Encodes `(partition_id, sequence)` internally.

### Ack & commit semantics

- At-least-once.
- Cumulative acks.
- Ack window default 1024 events / 16 MB. **Single oversized event ships anyway** (never block on one event exceeding the byte cap).

### Batching

`batching.windowMs > 0` → server accumulates per window or until `maxEvents`. Whole batch replayed on disconnect.

### Heartbeats

10s/30s aligned with `streaming.go:21-23`.

### Idempotency

`event_id` (deterministic `<tx_id>:<entity_id>`) is the dedupe key. Required.

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

```
1. T_preload = server-current-time
2. Query entities matching <filter> as of T_preload
3. POST subscription with startFromTimestamp = T_preload
4. Consume stream
```

Strict-`>` on subscription + `≤` on as-of-query = clean partition. **SQLite plugin's existing `<` semantics on as-of must be aligned to `≤`** as a **prerequisite PR** with parity-test scope; tracked in §11.

### maxLookback

`CYODA_CHANGEFEED_MAX_LOOKBACK` default 15 minutes. Outbox retention ≥ `maxLookback`.

## §7. Backpressure & slow-consumer policy

### Per-subscriber limits

| Limit | Default | Notes |
|---|---|---|
| Ack window events | 1024 | Per-consumer. |
| Ack window bytes | 16 MB | Whichever first; single oversized event ships. |
| Per-consumer queue depth (in-process broadcaster) | 4096 | SQLite/memory; smaller on hot path. |
| Backlog ceiling | 1M events / 7d | Per-consumer (NOT per-group). |
| Consumers per node | 1000 | Operational ceiling. |

### Memory floor (per node, defaults)

- 1000 consumers × 4096 queue × ~1 KB envelope ≈ **4 GB worst case** before ack-window backpressure kicks in.
- Plus 256 MB body cache.
- **The defaults are sized for ≥8 GB nodes.** Smaller deployments must reduce `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` or the queue depth. Documented in §13.

### Slow-consumer policies

| Policy | Default | Effect |
|---|---|---|
| `DETACH_CONSUMER` | yes | Per-consumer-instance detach. GROUP: redpanda rebalances. |
| `BLOCK_APPEND` | no | **Not recommended**; explicit operator override. |
| `DROP_OLDEST` | no | At-most-once degradation. |

## §8. Auth & multitenancy

### Tenant scope

- `tenantId` always from JWT.
- Outbox/log physically partitioned by tenant.
- Body hydration lookups include `tenant_id` in the query key (enforces postgres row-level security + sqlite isolation).

### Role

**All subscription operations require `ROLE_M2M`**. No granular permissions invented.

### Audit logging

- Subscription create/update/delete logged with tenant + actor + filter shape (model name, kinds, fromState, toState, bodyPredicate's **structural shape** — operators used, path **depth/shape only**, e.g. `$.string.string`, not actual paths like `$.creditCardNumber`).
- Attach/detach logged with tenant + subscription id + consumer instance + generation.
- Per `.claude/rules/security.md`: never log credentials, tokens, secrets, signing keys, or event body content.

## §9. Failure modes & recovery

### Storage / plugin failures

| Failure | Effect |
|---|---|
| Storage tx rollback | Entity write + change event both fail. |
| AppendChange returns error | SPI tx rolls back; entity write fails too. |
| ChangeFeed.Consume backend error | Affected streams terminate `INTERNAL` + ticket; reconnect. |
| Cassandra publisher daemon lag/death | Outbox rows persist; redpanda publishes delayed; replay falls back to table. Daemon leadership re-elects. Lag metric exposed. |

### Cluster failures

| Failure | Recovery |
|---|---|
| Single node death | Subscribers reconnect to surviving nodes. |
| Network partition | Existing cluster protocol; delivery may pause on minority. |
| Subscription cache broadcast lost | Periodic full-sync (60s) refreshes existence set + invalidates content cache. Bounded staleness. |
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
- Per-individual-transition events.
- `LifecycleCondition` inside `bodyPredicate`.
- Time-sortable `event_id` (purely an idempotency key; ordering is the cursor's job).

## §11. Open verification items (plan-time)

1. **SQLite as-of-timestamp `<` → `≤` alignment** — prerequisite PR with parity-test scope and behavioural-change changelog entry. **Bundled into the ChangeFeed work as PR 0 of the plan.**
2. **Cassandra `entity_change_events` schema** — partition key, clustering key, TTL strategy; concrete design in the plan.
3. **Cassandra publisher daemon** — per-node singleton via existing leader election; offset tracking schema. Concrete design in the plan.
4. **Cassandra wire-format extension** (`CommitEntityInfo`) — schema migration, version bump, parity tests. Per §4.4.
5. **Outbox composite index** for postgres/sqlite — column ordering decision depends on cursor encoding; pin in the plan.
6. **Engine `TransactionState` extension** — `SegmentBodyBefore`, `SegmentTransitions`; SAVEPOINT snapshot/restore wiring; cyoda-go-spi changes if these are SPI-visible.

### Resolved across both review iterations

| Item | Resolution |
|---|---|
| `transaction_id` field surfacing | `EntityMeta.TransactionID` exists at `cyoda-go-spi/types.go:18`. |
| Search-DSL evaluator reuse | `spi.predicate.Condition` tree via `internal/match/match.go:Match`. `LifecycleCondition` forbidden in bodyPredicate. |
| gRPC keepalive defaults | 10s/30s, aligned with `streaming.go:21-23`. |
| Lifecycle-index body hydration | postgres/sqlite `entity_versions` PK index. |
| Multi-segment commits | Per-segment emission; transitions[] list in envelope. |
| SPI signature `TxHandle` | Removed. `AppendChange(ctx, envelope)`; tx via `spi.GetTransaction(ctx)`. |
| Permission model invention | Removed. `ROLE_M2M` only. |
| GROUP rebalance protocol | Removed. Cassandra-only via redpanda's native protocol. |
| Cluster-broadcast correctness gap | Existence set + positive content cache + periodic full-sync. |
| Cassandra atomicity (no LWT) | Per-entity atomic via tag-grouped chunk in materialize batch. |
| Goroutine scale | Capped at 1000/node; memory floor documented (~4 GB worst case). |
| LRU body cache scope | Bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES`, default 256 MB. Tenant_id is part of the lookup key. |
| Outbox volume model | Per-segment-per-entity; composite index for time-range query. |
| `event_id` determinism for replay | Deterministic format `<transaction_id>:<entity_id>`. Recovery replay produces same id. Idempotency only, not ordering. |
| `event_id` collision risk | Zero (no hashing; concatenation of already-unique inputs). |
| `LifecycleCondition` + EntityMeta | Forbidden in bodyPredicate; rejected at create. |
| Engine→cassandra data plane | §4.4 documents wire-format extension. |
| Subscription cache thundering herd | Existence set bootstrap eliminates per-key storage reads at steady state. |
| GROUP backlog interaction | Per-consumer-instance detach; redpanda handles partition rebalance. |
| Body hydration tenant scoping | Explicit `tenant_id` in lookup key. |
| bodyPredicate path audit redaction | Path depth/shape only, never raw paths. |
| `Capabilities()` call timing | Init-time only; treat as memoized static. |
| `AppendChange` envelope copy cost | Value-pass; small struct + slice header; documented intent. |
| `kind=DELETE` ambiguity | `info.Data == nil` (hard-delete) or workflow-marked deletion state. |
| `lastAttachedAt` semantics | Updated on attach; preserved on detach; used for TTL computation. |

## §12. Extension hooks reserved

- `kind` enum extensibility (e.g., `SEGMENT_FAILED`).
- `includeProcessorOutputs` per-transition.
- `partitionKey` JSONPath override.
- JSONPath projection / JSON Patch diff.
- ChangeFeed as separately pluggable SPI capability.
- Admin "always on" tenant change feed.
- Granular per-model permissions if `UserContext` gains a permission layer.

## §13. Configuration env vars

Per `.claude/rules/documentation-hygiene.md` (Gate 4), **every env var below requires synchronised updates to:**
1. `cmd/cyoda/help/content/config/*.md` (relevant topic)
2. `README.md` configuration section
3. `DefaultConfig()` in the appropriate Go config struct

| Env var | Default | Purpose |
|---|---|---|
| `CYODA_CHANGEFEED_MAX_LOOKBACK` | `15m` | `startFromTimestamp` past-clamp |
| `CYODA_CHANGEFEED_BODY_CACHE_BYTES` | `268435456` (256 MB) | Per-node body LRU cap |
| `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` | `1000` | Operational ceiling |
| `CYODA_CHANGEFEED_POLL_INTERVAL` | `10s` | Postgres missed-notify backstop |
| `CYODA_CHANGEFEED_RETENTION` | `7d` | Outbox retention; must be ≥ `MAX_LOOKBACK` |
| `CYODA_CHANGEFEED_BACKLOG_MAX_EVENTS` | `1000000` | Backlog ceiling |
| `CYODA_CHANGEFEED_BACKLOG_MAX_AGE` | `7d` | Backlog ceiling |
| `CYODA_CHANGEFEED_ACK_WINDOW_EVENTS` | `1024` | Per-consumer ack window |
| `CYODA_CHANGEFEED_ACK_WINDOW_BYTES` | `16777216` (16 MB) | Per-consumer ack window |
| `CYODA_CHANGEFEED_CACHE_TTL` | `30s` | Positive content-cache TTL |
| `CYODA_CHANGEFEED_EXISTENCE_SYNC_INTERVAL` | `60s` | Periodic full-sync of existence set |

## §14. Test strategy

### TDD per CLAUDE.md Gate 1

Every component is driven by a failing test first. The test suites are scoped per plan-PR; verification commands per CLAUDE.md.

### E2E per Gate 2

User-facing behaviour (REST CRUD, gRPC stream, error codes) covered by E2E tests in `internal/e2e/`. Pattern follows existing self-contained E2E (testcontainers postgres + httptest server). New tests:
- Subscription CRUD lifecycle (create / get / list / patch / delete).
- gRPC attach (durable + ephemeral; broadcast + group on cassandra).
- At-least-once delivery via simulated disconnect.
- Backlog ceiling tripping → `DETACH_CONSUMER`.
- Preload-bridging via `startFromTimestamp` end-to-end (query as-of + subscribe).
- Tenant isolation cross-check.

### Parity per existing pattern

Storage-plugin parity tests in `e2e/parity/registry.go` extend to cover `ChangeFeed`:
- `AppendChange` atomicity (commit + change-event visibility).
- `Consume` cursor semantics + envelope filter.
- `Retain` / `Release` retention claim accounting.
- Body hydration by version pair.

Cassandra plugin picks up new parity tests automatically via the registry; the cassandra plugin's CI runs against its own dependency-update PR.

### SPI tests

`cyoda-go-spi/spitest/` gets `ChangeFeed` contract tests (interface conformance, error semantics, capability enumeration).

### Race detector

Per CLAUDE.md, `go test -race ./...` is end-of-deliverable for the final PR series, not per-step.

### SQLite as-of `≤` alignment (PR 0)

Prerequisite PR. Adds parity test asserting `≤` semantics across plugins. SQLite plugin implementation update. Changelog entry. Verification: `go test ./plugins/sqlite/... -v` + parity suite.

---

## Workflow context

Standard cyoda flow per `CLAUDE.md`:
- Brainstorming → this spec (two fresh-context review iterations applied).
- Next: `superpowers:writing-plans` to produce the implementation plan.
- Implementation track is multi-PR. The plan partitions into reviewable units:
  - PR 0: SQLite as-of `≤` alignment (prerequisite).
  - PR 1: SPI ChangeFeed interface in `cyoda-go-spi`.
  - PR 2: Memory + SQLite ChangeFeed implementations.
  - PR 3: Postgres ChangeFeed implementation + composite index migration.
  - PR 4: Engine emission hook + `TransactionState` extensions.
  - PR 5: REST CRUD endpoints.
  - PR 6: gRPC `EntityChangeService` + fan-out + filter/body machinery.
  - PR 7: Cassandra wire-format extension (`CommitEntityInfo`) — cross-repo PR pair.
  - PR 8: Cassandra `entity_change_events` table + materialize integration + publisher daemon.
  - PR 9: Documentation, help topics, env-var wiring.
