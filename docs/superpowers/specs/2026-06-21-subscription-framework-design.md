# Subscription Framework — v1 Design

**Date:** 2026-06-21 (revisions through 2026-06-22)
**Status:** Design — awaiting plan
**Scope:** v1 notification framework for cluster-of-application-nodes consumption. Not a substitute for a queue subsystem; not for webscale user-facing fan-out; not a historical reprocessing API; not a per-transition trajectory log (the entity audit / workflow event log is that).

## §1. Scope & vocabulary

A subscriber framework that lets client nodes (not just compute nodes) attach to cyoda over gRPC, declare a filter on entity model + workflow + body, and receive change events as **transaction segments commit** — with a per-subscription choice of ephemeral or durable delivery.

This is **a v1 notification framework, not a messaging or entity-event-queue framework.** Applications that need a durable, replayable, high-throughput queue subsystem build it on their own infrastructure. A future cyoda-native entity-event-queue would be a separate capability built on a real queue substrate.

The change event publishes the **segment-level summary** — what the entity looked like at the start of the segment, what it looks like after, and the transition that initiated this segment's execution. The intermediate trajectory inside a cascade is **not** published; subscribers needing per-step detail query the entity audit / workflow event log.

### Vocabulary

| Term | Meaning |
|---|---|
| **Segment** | A single transactional commit in cyoda's engine. `EngineResult.Segmented=true` means a request spanned multiple segments via `COMMIT_BEFORE_DISPATCH` processors. Within one segment, multiple workflow transitions can have cascaded (via `cascadeAutomated`). The segment is the natural emission unit. |
| **Change event** | One emission per **(segment commit, entity)** pair carrying envelope. |
| **Subscription** | A server-side declaration: filter + delivery mode + consumption mode + batching + body-include policy. Ephemeral (lives with the stream) or durable (persists across connections). |
| **Delivery mode** | `LIVE_ONLY` (no replay; ephemeral) or `DURABLE` (at-least-once delivery for events emitted after subscription start, replay possible from the subscription's last-committed cursor within outbox retention). |
| **Consumption mode** | `BROADCAST` (each attached consumer gets every event) or `GROUP` (events partition across consumers by `entity_id`). Ephemeral is always `BROADCAST`. **GROUP is only available on backends advertising `GroupConsumption=true` — in v1 that is the cassandra plugin only**, which delegates to redpanda's Kafka-compatible consumer-group machinery. |
| **ChangeFeed** | SPI capability each storage plugin implements internally. Atomic event append + filtered consume. Capabilities advertised per-backend. |
| **Cursor** | Opaque server-issued token; monotone within a partition. Subscriber commits to advance. Handles **ordering**. |
| **`event_id`** | Deterministic per-(transaction, entity) idempotency key. Format: `<transaction_id>:<entity_id>`. Recovery replays produce the identical id so consumer-side dedupe is correctness-stable. **Idempotency only — not an ordering primitive.** `transaction_id` is validated UUID-shaped (no `:` in the value) so the separator is unambiguous. |
| **`transition_name`** | The transition that **initiated this segment's execution**. For the first segment of a user request: the user-invoked manual transition (e.g., `pay`). For segments after `COMMIT_BEFORE_DISPATCH`: the engine-chosen transition that continues the workflow post-dispatch. Within a segment (cascade), this is set once at segment-start and not overwritten by subsequent cascaded transitions. |
| **`startFromTimestamp`** | Optional subscription field. Permits bounded preload bridging — server-clamped by `maxLookback` and by the propagation floor (§6.1). |
| **Bootstrap point** | The `(createdAt, cursorZero, effectiveStartFrom)` tuple returned on subscription create. Defines the strict-`>` boundary between "preload territory" and "subscription territory". |

### Non-goals for v1

- Per-transition trajectory inside a cascade. The change event carries segment-summary only. Subscribers needing intermediate cascade steps use the entity audit / workflow event log.
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
- GROUP mode on OSS backends.
- Cross-entity atomicity of change-event emission within a segment.
- `LifecycleCondition` and `FunctionCondition` inside `bodyPredicate`.
- Time-sortable `event_id`.
- Server-side "search-with-bodyPredicate as-of T" endpoint for preload bridging.
- Body delivery for events whose body exceeds the gRPC max-message-size (16 MB); see §6.

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
    "bodyPredicate": { ... }            // spi.predicate.Condition; LifecycleCondition + FunctionCondition forbidden recursively
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

All filter fields are direct matches against the segment-summary envelope. Null/unset fields match any value.

- `kinds` — matches `envelope.kind` (`CREATE | UPDATE | DELETE`).
- `fromState` — matches `envelope.from_state` (entity state at segment-start).
- `toState` — matches `envelope.to_state` (entity state at segment-end).
- `bodyPredicate` — evaluated at fan-out against `body_after` using the existing `spi.predicate.Condition` tree. The validator walks the tree recursively and **rejects `LifecycleCondition` and `FunctionCondition` at any depth** (including inside `GroupCondition.Conditions`). `SimpleCondition`, `GroupCondition`, `ArrayCondition` are allowed.

Subscribers needing intermediate-state visibility ("ORDER passed through VALIDATING during the cascade") use the entity audit log, not the change feed.

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
- `GROUP` consumer joins identify with a client-chosen `consumerInstanceId` + server-managed `generation`. Server increments `generation` on every successful attach for that subscription. Two attaches with the same `consumerInstanceId`: the new generation supersedes; the older generation is fenced (cannot ack). Rolling-deploy safe. On detached re-attach, redpanda offset commits preserve the last acked position — at-least-once redelivery picks up from there.
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

**`subscriptions` table** follows the existing cyoda RLS pattern: `ENABLE ROW LEVEL SECURITY` + `tenant_isolation_subscriptions` policy keyed on `current_setting('app.current_tenant', true)`. Defence in depth. The app runs as owner, which bypasses RLS naturally, so the system-level existence-set bootstrap (§4) works without any privileged-role indirection. Matches `entities`, `entity_versions`, etc.

Errors follow existing cyoda conventions.

### gRPC stream — `EntityChangeService`

```proto
service EntityChangeService {
  rpc Stream(stream io.cloudevents.v1.CloudEvent) returns (stream io.cloudevents.v1.CloudEvent);
}
```

Streams bare `CloudEvent` directly, matching `CloudEventsService.startStreaming` idiom.

**gRPC max-message-size** explicitly configured to **16 MB** on `EntityChangeService` (`MaxSendMsgSize` / `MaxRecvMsgSize`), aligned with the ack-window byte cap (§7). Bodies exceeding 16 MB are not deliverable as `body_after` / `body_before`; subscribers needing such bodies must use `includeBody: NONE` + fetch via the entity API on receipt. Documented as a known limit.

One stream = one attached subscription.

### Auth

- REST + gRPC: existing JWT bearer + tenant scope. **All subscription operations require `ROLE_M2M`** — same surface as `internal/grpc/streaming.go:58`.
- Cross-tenant attach → `PERMISSION_DENIED`.
- Tenant isolation implicit via existing patterns + the RLS layer above.

## §3.5. Connection lifecycle & recovery

### Scenario matrix

| Scenario | Ephemeral | Durable |
|---|---|---|
| **Clean detach** | State dropped. | Cursor at last ack persisted. GROUP: partitions reassigned via redpanda group protocol; new owner resumes from last committed redpanda offset. |
| **Network blip** | State dropped after keepalive timeout. | Cursor at last ack; GROUP rebalance is redpanda's. Reconnect with same `subscriptionId`+`consumerInstanceId` resumes; server bumps generation. |
| **Client restart / pod replacement** | New ephemeral subscription. | New `consumerInstanceId` joining = redpanda rebalance for GROUP, no-op for BROADCAST. Generation fencing prevents zombie acks from prior pod. |
| **Server node dies** | Stream errors `UNAVAILABLE`. | Reconnect to another node; subscription resource lives in shared storage. |
| **Long disconnection** | Subscription gone with stream. | Replay from last cursor if within retention; else `OUT_OF_RANGE` with earliest still-available cursor. |
| **TTL exceeded** | N/A | Subscription auto-deleted; reconnect = `NOT_FOUND`. |
| **Mid-batch disconnect** | Batch lost. | Batch redelivered. Consumer dedupes via `event_id`. |

### Mechanism

All authoritative state lives in the storage tier (durable) or in-process on the attached node (ephemeral). gRPC stream death triggers fencing/grace; reconnect rehydrates state. At-least-once requires `event_id` dedupe.

For GROUP on cassandra, rebalance is **delegated to redpanda's consumer-group protocol** (`KafkaSubscribe`/`KafkaCommit` already used by the cassandra plugin). Detached consumers' partitions reassign to peers, which start from the last redpanda-committed offset — at-least-once preserved across rebalance.

### Operational knobs

| Knob | Default | Notes |
|---|---|---|
| gRPC keepalive interval | 10s | `internal/grpc/streaming.go:21`. |
| gRPC keepalive timeout | 30s | `internal/grpc/streaming.go:22`. |
| gRPC max message size | 16 MB | aligned with ack-window byte cap. |
| GROUP session timeout | `RedpandaSessionTimeoutSec` (default 30s) | Configured in cassandra plugin. |
| Backlog ceiling | 1M events / 7d | Hard ceiling for un-acked outbox backlog. Applies at durable-retention level. |
| Backlog ceiling enforcement | periodic sweep (default 5 min, `CYODA_CHANGEFEED_BACKLOG_SWEEP_INTERVAL`) | Sweep computes `(tail_cursor - last_acked_cursor)` per durable subscription; trips policy when ceiling breached. |
| Backlog ceiling policy | `DETACH_CONSUMER` | Per-consumer-instance detach. For GROUP, redpanda rebalances detached consumer's partitions to peers. |
| Consumers per node | 1000 | Operational ceiling. |

## §4. Event source & emission

### Source

Single emission point: **the engine's segment commit hook**. One segment commit emits one event **per entity** touched in that segment.

Engine semantics (verified in `internal/domain/workflow/engine.go`, `engine_processors.go`, `internal/domain/entity/service.go`):
- One user request = 1+ segments (`EngineResult.Segmented=true` if `COMMIT_BEFORE_DISPATCH` ran).
- One segment = exactly one tx commit.
- One segment can contain multiple cascaded transitions on the same entity (`cascadeAutomated`).
- One segment can touch multiple entities (one event per entity emitted).

### Emission call site (per backend)

- **Memory / SQLite / Postgres**: the engine constructs the envelope and calls `ChangeFeed.AppendChange(ctx, envelope)` **directly inside the SPI tx**, before commit. The plugin writes to its outbox in the same tx.
- **Cassandra**: the engine cannot call `AppendChange` synchronously — cassandra's commit machinery runs in the TX coordinator goroutine, potentially on a different node. Instead, the engine **populates `CommitEntityInfo.SegmentInvokedTransition`** (§4.4); the coordinator builds the envelope and writes `entity_change_events` inside the materialize batch in the same tag group as the entity's other writes.

The atomicity contract is the plugin's commit mechanism, not `AppendChange`'s return.

### Atomicity

Per-entity within a segment:

- **Cassandra**: `entity_change_events` row written in the existing materialize batch in the same tag group as the entity's other writes (`tx_coordinator.go materialize()`). Per-entity statements share a tag → same chunk → atomic at per-entity row level via `USING TIMESTAMP <HLC>` fencing. **Redpanda is async fan-out, not the atomicity boundary**.
- **Postgres/SQLite**: outbox row in the same SQL tx.
- **Memory**: pushed to in-process channel after the in-memory write. Tx-aware.

**Atomicity claim**: if `txMgr.Commit` succeeds, the change event for each entity in the segment will eventually be visible to subscribers. Cross-entity atomicity within one segment is **not** claimed.

**Cassandra chunk-size budget note**: adding one row to the per-entity tag group enlarges the chunk by ~few hundred bytes (envelope-only; bodies are not in the outbox row). For entities with many indexed fields, the per-entity tag group is already significant; deployments tuning `CYODA_CASSANDRA_BATCH_MAX_BYTES` should account for this small additional pressure. Operators on workloads with very-wide entities (many indexed fields) may need to raise the threshold. Documented in `cmd/cyoda/help/content/config/cassandra.md`.

### Cross-entity partial-commit recovery

Cassandra's materialize chunks are independent; entity A's chunk can succeed while entity B's fails. Recovery (`MaterializeFromLog`) replays the failed entities using the original HLC. The replay reconstructs `CommitEntityInfo` from `tx_writes` (which gains a `SegmentInvokedTransition` column per §4.4; `from_state` derived from `PrevData._meta.state`; `to_state` from `State`; `kind` from `(PrevData, Data)`). `event_id` is deterministic (`<transaction_id>:<entity_id>`), so the replayed emission has the same id and the same envelope as the original would have — consumer dedupe is correctness-stable.

### Emission gate

The engine emits a per-entity change event at segment commit **if and only if** both:

1. The entity's `(tenant, model_name, model_version)` has at least one subscription (existence check), AND
2. At least one subscription's envelope filter matches the segment for that entity. Short-circuit on first match. Body predicates are **not** evaluated at emission.

Events matching by envelope but failing every subscriber's body predicate are still written to the outbox; fan-out drops them per-subscriber. This is intentional: per-segment storage cost is paid once, subscriber-specific filtering happens at fan-out.

### `kind` derivation

From `(PrevData, Data)` alone:
- `CREATE` — `PrevData == nil && Data != nil`
- `DELETE` — `PrevData != nil && Data == nil` (hard-delete)
- `UPDATE` — otherwise

> **Soft-delete callout**: workflow soft-deletes (transitioning to a "deleted" workflow state while keeping the entity body) emit `kind=UPDATE`, not `kind=DELETE`. The `kind` field reflects body-presence, not workflow lifecycle. Subscribers detect soft-deletes via `to_state` filter on their tenant's deleted-state convention. This is intentional — keeps `kind` semantics simple and orthogonal to workflow design.

### Subscription cache strategy (two-tier)

**Tier 1 — Existence set (per node, in-memory):**
- Type: `map[(tenant, model, version)] → has_subscriptions bool`.
- Bootstrap: at node startup, run `SELECT DISTINCT tenant_id, model_name, model_version FROM subscriptions` once (app runs as owner, RLS bypassed naturally). Populate the set.
- Maintenance: subscription create/delete on any node broadcasts via `ClusterBroadcaster`; receiving nodes set/unset the key.
- Periodic full-sync default 60s (`CYODA_CHANGEFEED_EXISTENCE_SYNC_INTERVAL`) replays the bootstrap query, recovering from broadcast loss.

**Tier 2 — Positive-content cache (per node, in-memory, TTL):**
- Type: `map[(tenant, model, version)] → [filter set]` with TTL aligned to `EXISTENCE_SYNC_INTERVAL` (60s). Broadcast invalidation is the primary freshness mechanism; the TTL is a safety net.
- Populated when existence set says `true` and content cache misses.
- On subscription create/delete, broadcast invalidates the entry.

### Envelope construction

```
ChangeEvent {
  event_id        string      "<transaction_id>:<entity_id>"; transaction_id validated UUID-shaped
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
  transition_name string      the transition that initiated this segment (see §1 vocabulary)
  body_after      *EntityBody attached at fan-out, derived from CommitEntityInfo.Data
  body_before     *EntityBody attached at fan-out, derived from CommitEntityInfo.PrevData
}
```

`AppendChange` validates `event_id` format on entry (`transaction_id` must be UUID-shaped; `entity_id` may be any string with no `:`).

### What is not emitted in v1

- Per-individual-transition events (segment summary is the contract).
- Failed segments (rolled-back tx).
- Non-transition writes.
- Cross-entity correlation in one envelope.

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

Bodies LRU-cached. Cache key: **`(tenant_id, entity_id, version)`** — tenant always in key. Bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB). Eviction safety relies on Go GC + batch holding `[]byte` references directly; no custom refcount.

**Documented cost**: `bodyPredicate` + `includeBody: NONE` triggers a body load.

### Pre-state body (`body_before`)

For cassandra: derived from `CommitEntityInfo.PrevData` (already on the wire). For postgres/sqlite: hydrated from the prior `entity_versions` row. For memory: from the prior in-memory version.

No new wire field needed; `PrevData` is reused.

### Outbox row contents

Envelope (without bodies). Bodies looked up at fan-out by `(tenant_id, entity_id, version)`.

**Composite outbox index for postgres/sqlite**: `(tenant_id, model_name, model_version, sequence)` for cursor reads (the dominant path); a second index `(tenant_id, emitted_at)` for the retention sweeper. Both small; storage cost negligible.

### Subscription mutation invariants

PATCH-mutable: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Others immutable. PATCH applies forward.

## §4.3. Cardinality and outbox volume

- A segment touching N entities → N change events sharing `transaction_id`.
- A segment with M cascaded transitions on one entity → exactly **1** change event.
- A user request spanning S segments → ≥ S events.

Outbox volume scales with **entity-update rate**, not transition rate.

## §4.4. Small wire-format extension on cassandra

Cassandra's commit pipeline runs in the TX coordinator goroutine. To deliver `SegmentInvokedTransition` (the transition that initiated the segment, per §1 vocabulary) to the coordinator:

```go
// cyoda-go-cassandra/internal/queue/tx_message.go
type CommitEntityInfo struct {
    // ... existing: Data, PrevData, State, Version, ...
    SegmentInvokedTransition string  // segment-invariant; carries the initiating transition
}
```

`tx_writes` schema gains the same column (with a comment: `// segment-invariant; duplicated per-row for recovery simplicity`) so `MaterializeFromLog` sees it during recovery.

Engine-side: `TransactionState` gains `SegmentInvokedTransition string`, **set at the start of each segment**:
- Segment 1 (user request entry): set to the user-invoked transition.
- Segment 2+ (post-CBD): when `cascadeAutomated` selects the first transition for the new segment, set to that transition's name. Within the segment, not overwritten by further cascade.

Snapshot/restore semantics: participates in `Savepoint` / `RollbackToSavepoint` like `WriteSet` / `ReadSet`. Each plugin's savepoint implementation needs a small update.

**Backward compatibility**: `CommitEntityInfo` is JSON-encoded; the new field is `omitempty`. Rolling upgrade is byte-compatible. Semantic gap during rollout: change events emitted before both engine + coordinator are upgraded have empty `transition_name`. Documented.

This is the **only** wire-format extension required. `from_state` from `PrevData._meta.state`, `to_state` from `State`, `kind` from `(PrevData, Data)` — no further wire fields.

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
    // Validates event_id format: transaction_id must be UUID-shaped (no ':').
    //
    // Per-entity atomicity: if the SPI tx commits, this event is durable and
    // will eventually be visible to consumers. Cross-entity atomicity within
    // one segment is NOT guaranteed.
    //
    // envelope is value-passed (small struct).
    AppendChange(ctx context.Context, envelope ChangeEnvelope) error

    Consume(ctx context.Context, spec ConsumerSpec) (ChangeEventStream, error)

    Retain(ctx context.Context, claim RetentionClaim) error
    Release(ctx context.Context, subscriptionID string) error

    // Capabilities is called at plugin init only; returns static values.
    // The SPI layer caches the result; runtime config changes require restart.
    Capabilities() ChangeFeedCapabilities
}
```

`ChangeEnvelope`, `ConsumerSpec`, `ChangeFeedCapabilities` shapes per §4 envelope and validity matrix.

### Implementation guidance

- Errors wrapped with `fmt.Errorf("…: %w", err)`.
- Logging via `log/slog` only.
- Tenant scoping at storage layer.
- Body hydration queries carry `tenant_id` in the lookup key.

### Per-backend implementation

| Backend | AppendChange | Consume | Capabilities |
|---|---|---|---|
| **memory** | push to in-process channel; tx-aware | range over channel buffer with envelope filters | `Durable=false, GroupConsumption=false`, retention=0 |
| **sqlite** | INSERT into `change_outbox` in SQL tx | in-process broadcaster + per-consumer queues + catchup-by-cursor on attach; no polling | `Durable=true, GroupConsumption=false` |
| **postgres** | INSERT into `change_outbox` in SQL tx; `pg_notify` post-commit | `LISTEN/NOTIFY` + periodic poll (10s, `CYODA_CHANGEFEED_POSTGRES_POLL_INTERVAL`) backstop | `Durable=true, GroupConsumption=false` |
| **cassandra** | Engine threads `SegmentInvokedTransition` via `CommitEntityInfo`; coordinator builds envelope and writes `entity_change_events` row in materialize batch, same tag group as entity write (atomic per-entity). Publisher daemon drains → redpanda for fan-out + GROUP. | redpanda consumer-group via existing `KafkaSubscribe`. Replay older than redpanda retention reads `entity_change_events` directly. | `Durable=true, GroupConsumption=true` |

### Cassandra `entity_change_events` schema (sketch)

```cql
CREATE TABLE entity_change_events (
    tenant_id        text,
    model_name       text,
    model_version    text,
    bucket_hour      int,
    hlc              bigint,
    entity_id        text,
    transaction_id   uuid,
    from_state       text,
    to_state         text,
    transition_name  text,
    kind             tinyint,
    body_version     bigint,
    PRIMARY KEY ((tenant_id, model_name, model_version, bucket_hour), hlc, entity_id)
) WITH default_time_to_live = 604800;  -- 7d; configurable
```

Partition by `(tenant, model, version, bucket_hour)`; tenant-isolated; time-range scannable.

### Cassandra publisher daemon

Drains `entity_change_events` → redpanda. **Leader election via redpanda single-partition coordination topic** — the consumer-group owner of that topic is the publisher. No new election primitive. Recovery latency = `RedpandaSessionTimeoutSec` + restart (≤ ~30-60s).

**Live BROADCAST consumers** see a delivery pause during publisher leadership transfer equal to the recovery latency. Outbox rows persist; replay catches up. Bounded by redpanda availability — if redpanda is unavailable for an extended period, `entity_change_events` accumulates and publisher backlog grows.

Publisher lag exposed via metric `changefeed.publisher_lag_seconds`. Metrics are **not** tagged with `transition_name` or `entity_id` to avoid cardinality explosion.

### GROUP consumption — cassandra-only via redpanda

Redpanda topics partitioned by `entity_id` hash; consumer groups native. OSS plugins advertise `GroupConsumption=false`; create with `GROUP` on OSS → `400`.

### Fan-out layer (engine-internal)

- One goroutine per attached consumer per node, capped at `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` (default 1000).
- LRU body cache bounded by `CYODA_CHANGEFEED_BODY_CACHE_BYTES` (default 256 MB).
- Tracks per-consumer ack cursor; periodic `Retain` calls.

## §6. Wire protocol & cursor semantics

### Wire format

Bare `CloudEvent` stream. CloudEvent `type` values (PascalCase + Event/Request suffix per cyoda convention). The `EntityChangeFeed*` prefix distinguishes from the existing `EntityChanges*` audit-log family:

**Server → client:**

| `type` | Notes |
|---|---|
| `EntityChangeFeedEvent` | Single event when batching disabled. |
| `EntityChangeFeedBatchEvent` | Batched events; data is `{events: [...], lastCursor: ...}`. |
| `EntityChangeFeedRebalanceEvent` | GROUP only. Redpanda-driven assignment. |
| `EntityChangeFeedErrorEvent` | Terminal error; code + ticket UUID (5xx). |
| `EntityChangeFeedHeartbeatEvent` | Periodic keepalive. |

**Client → server:**

| `type` | Notes |
|---|---|
| `EntityChangeFeedAttachDurableRequest` | `{subscriptionId, consumerInstanceId, fromCursor?, generation?}`. `fromCursor` ≥ last ack. |
| `EntityChangeFeedAttachEphemeralRequest` | Inline subscription spec. |
| `EntityChangeFeedAckRequest` | Cumulative ack. |
| `EntityChangeFeedRebalanceAckRequest` | GROUP. Acks generation. |
| `EntityChangeFeedDetachRequest` | Graceful detach. |

(Naming note: cyoda already has an `EntityChanges*` audit-log family in `internal/grpc/cloudevent_types.go`. The `EntityChangeFeed*` prefix disambiguates: change feed = live/segment-summary; entity changes = historical audit log query.)

### Cursor semantics

Opaque, server-issued, monotone within a partition. Base64. Subscribers never interpret.
- Within retention bounds.
- Stable across reconnects.
- Encodes `(partition_id, sequence)` internally. For BROADCAST mode on non-partitioned backends, `partition_id = 0` (uniform encoding).

### Ack & commit semantics

- At-least-once.
- Cumulative acks.
- Ack window default 1024 events / 16 MB. **Single oversized event ships anyway up to gRPC message-size limit (16 MB)**. Events whose body exceeds the gRPC limit are not deliverable as `body_after` / `body_before` — the envelope ships, body fields are omitted with a flag.

### Batching, heartbeats, idempotency

- `batching.windowMs > 0` → server accumulates per window or until `maxEvents`. Whole batch replayed on disconnect.
- 10s/30s keepalive aligned with `streaming.go:21-22`.
- `event_id` (deterministic) is the dedupe key. Required.

## §6.1. Bootstrap & preload bridging

### Bootstrap point

```
propagationFloor = createdAt + EXISTENCE_SYNC_INTERVAL    // honest at-least-once contract
effectiveStartFrom = clamp(
  requestedStartFromTimestamp ?? T_now,
  lower = max(enablementBoundary, T_now - maxLookback, retentionFloor, propagationFloor),
  upper = T_now
)
```

The `propagationFloor` clamp is critical: subscription cache propagation across peer nodes is best-effort + periodic full-sync. Until `createdAt + EXISTENCE_SYNC_INTERVAL`, peer nodes may emit events without this subscription in their existence set. The strict-`>` boundary on the subscription side combined with the `propagationFloor` lower bound ensures **honest at-least-once delivery** for events with `emitted_at > effectiveStartFrom`.

The subscription delivers events with `emitted_at > effectiveStartFrom`. **Strictly greater.**

`cursorZero` returned on create.

### Preload-bridging pattern

```
1. T_preload = effectiveStartFrom from the create response (NOT client clock)
2. Subscribe FIRST; begin buffering events client-side
3. Query entity state via existing API:
   - Known entities: GetAsAt(entityID, T_preload) per entity
   - All entities of a model: GetAllAsAt(modelRef, T_preload)
   - Filtered queries: GetAllAsAt + client-side filter (no server-side as-of+filter v1)
4. Apply preload result FIRST to local cache (replace baseline)
5. Replay buffered events in cursor order on top of the preload baseline
```

### Client-side coordination contract

The strict-`>` server-side partition guarantees no overlap or gap. But the client must apply the preload baseline before processing buffered events, otherwise events arriving between subscribe (step 2) and preload return (step 3) would be applied to a stale baseline.

- Buffer subscription events from attach onwards; do not process them yet.
- Apply preload result first (replace cache baseline at `T_preload`).
- Replay buffered events in `cursor` order on top.
- Subscribers using `bodyPredicate` must apply the same predicate to preload bodies client-side AND let the server apply it to live events — server-side `bodyPredicate` filtering applies only to the subscription stream, not to the preload `GetAllAsAt` result.

**Clock skew**: `T_preload` is the server's `effectiveStartFrom`, NOT the client's wall clock. The create response gives the authoritative value; client uses it verbatim for `GetAsAt` queries.

### maxLookback

`CYODA_CHANGEFEED_MAX_LOOKBACK` default 15 minutes. Outbox retention ≥ `maxLookback`.

## §7. Backpressure & slow-consumer policy

### Per-subscriber limits

| Limit | Default | Notes |
|---|---|---|
| Ack window events | 1024 | Per-consumer. |
| Ack window bytes | 16 MB | Whichever first; single oversized event ships up to 16 MB. |
| Per-consumer queue depth (in-process broadcaster) | 4096 | SQLite/memory. |
| Backlog ceiling (durable outbox) | 1M events / 7d | Per-consumer-instance. |
| Backlog ceiling sweep | 5 min | Periodic sweep; `CYODA_CHANGEFEED_BACKLOG_SWEEP_INTERVAL`. |
| Consumers per node | 1000 | Operational ceiling. |

### Layering

- Ack window: in-band backpressure (live).
- In-process queue depth: per-consumer buffer (memory/sqlite in-process).
- Backlog ceiling: durable-retention abandonment detection (long-disconnected durable consumers).

### Memory floor (per node, defaults)

- 1000 × 4096 × ~512 B envelope ≈ **2 GB worst case**.
- Plus 256 MB body cache.
- **Defaults sized for ≥4 GB nodes.** Smaller deployments must reduce `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` or queue depth.

### Slow-consumer policies

| Policy | Default | Effect |
|---|---|---|
| `DETACH_CONSUMER` | yes | Per-consumer-instance detach. GROUP: redpanda rebalances. Re-attach within retention replays from last ack. |
| `BLOCK_APPEND` | no | **Not recommended; can stall engine writes.** Explicit operator override only. |
| `DROP_OLDEST` | no | At-most-once degradation. |

## §8. Auth & multitenancy

### Tenant scope

- `tenantId` always from JWT.
- Outbox/log physically partitioned by tenant.
- Body hydration includes `tenant_id` in the lookup key.

### Role

**All subscription operations require `ROLE_M2M`**.

### RLS on subscription tables

`subscriptions`, `change_outbox`, and any new subscription-related postgres tables follow the **existing RLS pattern**: `ENABLE ROW LEVEL SECURITY` + `tenant_isolation_*` policy keyed on `current_setting('app.current_tenant', true)`. Defence in depth; the app runs as owner and bypasses RLS naturally, so existence-set bootstrap and other system-level queries work without privileged-role indirection. Consistent with `entities`, `entity_versions`, `models`, `kv_store`, `sm_audit_events`, etc.

### Audit logging

- Subscription create/update/delete logged with tenant + actor + filter shape (model name, kinds, fromState, toState, bodyPredicate's **structural shape** — operators used, path **depth/shape only** like `$.string.string`, never actual paths like `$.creditCardNumber`).
- Attach/detach logged with tenant + subscription id + consumer instance + generation.
- Per `.claude/rules/security.md`: never log credentials, tokens, secrets, signing keys, or event body content.

## §9. Failure modes & recovery

### Storage / plugin failures

| Failure | Effect |
|---|---|
| Storage tx rollback | Entity write + change event both fail. |
| AppendChange returns error | SPI tx rolls back. |
| ChangeFeed.Consume backend error | Affected streams terminate `INTERNAL` + ticket; reconnect. |
| Cassandra publisher daemon lag/death | Outbox rows persist; redpanda publish delayed; replay falls back to table. Daemon leadership re-elects via redpanda group. Live consumers may see pause ≤ `RedpandaSessionTimeoutSec + restart`. Metric `changefeed.publisher_lag_seconds`. |
| Cassandra entity_change_events chunk-size breach | Per-entity tag group exceeds `batch_size_fail_threshold`; entity write fails. Recovery same as any chunk failure. Operators on workloads near the threshold should raise `CYODA_CASSANDRA_BATCH_MAX_BYTES`. |

### Cluster failures

| Failure | Recovery |
|---|---|
| Single node death | Subscribers reconnect to surviving nodes. |
| Network partition | Existing cluster protocol; delivery may pause on minority. |
| Subscription cache broadcast lost | Periodic full-sync (60s) refreshes existence set. Bounded staleness; subscribers in the propagation window see clamped `effectiveStartFrom` (§6.1). |
| Storage corruption | Out of scope. |

### Subscriber-induced

| Failure | Recovery |
|---|---|
| All acks dropped | Ack window fills → server stops → backlog sweep trips ceiling → `DETACH_CONSUMER`. |
| Ack ahead of received cursor | `INVALID_ARGUMENT`. |
| Attach outside retention | `OUT_OF_RANGE` + earliest cursor. |
| Reconnect storm | Existing rate-limit interceptor. |
| Generation race (two pods, same consumerInstanceId) | Server bumps generation; older fenced; rolling-deploy safe. |
| Body too large for gRPC limit | Envelope ships with body fields omitted + flag; subscriber fetches via entity API if needed. |

## §10. Out of scope for v1

- Per-transition trajectory inside a cascade.
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
- Server-side "search-with-bodyPredicate as-of T" endpoint.
- Body delivery for bodies exceeding 16 MB (gRPC max-message-size).

## §11. Open verification items (plan-time)

1. **Cassandra `entity_change_events` schema** — sketch in §5; concrete partitioning, clustering, TTL strategy pending.
2. **Cassandra publisher daemon** — redpanda single-partition coordination topic; offset tracking schema; concrete design pending.
3. **Outbox indexes for postgres/sqlite** — cursor-read index `(tenant_id, model_name, model_version, sequence)` + sweeper index `(tenant_id, emitted_at)`; verify column ordering with cursor encoding.
4. **SQLite as-of sub-millisecond `≤` alignment** — optional parity cleanup, not a prerequisite (functional equivalence at ms resolution).

### Resolved across four review iterations

| Item | Resolution |
|---|---|
| `transaction_id` field | `EntityMeta.TransactionID` exists at `cyoda-go-spi/types.go:18`. |
| Search-DSL evaluator reuse | `spi.predicate.Condition` via `internal/match/match.go:Match`. `LifecycleCondition` + `FunctionCondition` forbidden recursively. |
| gRPC keepalive defaults | 10s/30s, aligned with `streaming.go:21-22`. |
| Lifecycle-index body hydration | postgres/sqlite `entity_versions` PK index. |
| Multi-segment commits | Per-segment emission; segment-summary envelope. |
| SPI signature `TxHandle` | Removed. `AppendChange(ctx, envelope)`; tx via `spi.GetTransaction(ctx)`. |
| Permission model invention | Removed. `ROLE_M2M` only. |
| GROUP rebalance protocol | Cassandra-only via redpanda's native protocol. |
| Cluster-broadcast correctness gap | Existence set + content cache + periodic sync. |
| Cassandra atomicity (no LWT) | Per-entity atomic via tag-grouped chunk. |
| Goroutine scale | 1000/node; memory floor ~2 GB worst case. |
| LRU body cache scope | Bounded 256 MB; tenant in key; Go-GC eviction safety. |
| Outbox volume model | Per-segment-per-entity; composite indexes for cursor + sweeper. |
| `event_id` determinism | `<transaction_id>:<entity_id>`; UUID-shape validated at AppendChange. |
| `LifecycleCondition` + EntityMeta | Forbidden in bodyPredicate; recursive walker. |
| Engine→cassandra data plane | Single field on `CommitEntityInfo`. |
| Subscription cache thundering herd | Existence set bootstrap eliminates per-key reads. |
| GROUP backlog interaction | Per-consumer-instance detach; redpanda offset preserves progress on rebalance. |
| Body hydration tenant scoping | Explicit `tenant_id` in lookup key. |
| bodyPredicate path audit redaction | Path depth/shape only. |
| `Capabilities()` call timing | Init-time only; SPI caches the result; restart for changes. |
| `AppendChange` envelope copy cost | Value-pass; small struct. |
| `kind=DELETE` ambiguity | Derived from `(PrevData, Data)`; soft-delete callout in §4. |
| `lastAttachedAt` semantics | Updated on attach; preserved on detach. |
| `transitions[]` over-design | Removed. Segment-summary only. |
| `BodyBefore` field redundancy | Removed. `body_before` from `PrevData`. |
| Recovery replay fidelity | `tx_writes` carries `SegmentInvokedTransition`; replay produces identical envelope. |
| `subscriptions` table RLS | **RLS enabled** matching existing cyoda pattern (defence in depth; app-as-owner bypasses for system ops). |
| PR 0 SQLite ≤ prerequisite | Dropped; functional equivalence at ms resolution. |
| Existence-set staleness window | `effectiveStartFrom` clamped to `createdAt + EXISTENCE_SYNC_INTERVAL` propagation floor. Honest at-least-once contract. |
| Publisher leader election | Redpanda single-partition consumer group. |
| Wire-format rolling upgrade | JSON `omitempty`; byte-compatible; semantic gap documented. |
| Preload bridging API mapping | `GetAsAt` / `GetAllAsAt` + client-side filter; explicit client-side coordination contract in §6.1. |
| Cache eviction safety | Go GC + batch byte-slice refs. |
| COMPATIBILITY.md | Required on PR 1 (SPI) and PR 7 (cassandra wire-format). |
| CloudEvent type naming | `EntityChangeFeed*` family disambiguates from existing `EntityChanges*` audit family. |
| `transition_name` semantics across CBD boundaries | Each segment carries the transition that initiated **that segment's** execution. Segment 1 = user-invoked; segment 2+ (post-CBD) = engine-chosen continuation transition. |
| Cassandra chunk-size budget | Documented; operators near threshold raise `CYODA_CASSANDRA_BATCH_MAX_BYTES`. |
| Soft-delete kind ambiguity | Explicit callout in §4: `kind` reflects body-presence, not workflow lifecycle. |
| Backlog ceiling enforcement | Periodic sweep (`CYODA_CHANGEFEED_BACKLOG_SWEEP_INTERVAL`). |
| Content cache TTL redundancy | Aligned with `EXISTENCE_SYNC_INTERVAL` (60s). |
| event_id collision check | `AppendChange` validates `transaction_id` is UUID-shaped. |
| gRPC max-message-size | Explicit 16 MB; bodies >16 MB ship envelope-only with flag. |
| Metric cardinality | No `transition_name` / `entity_id` tags on built-in metrics. |
| `make test-all` per CLAUDE.md | Test strategy §14 calls it out. |

## §12. Extension hooks reserved

- `kind` enum extensibility.
- `includeProcessorOutputs`.
- `partitionKey` JSONPath override.
- JSONPath projection / JSON Patch diff.
- ChangeFeed as separately pluggable SPI capability.
- Admin "always on" tenant change feed.
- Granular per-model permissions.
- Server-side "search-with-bodyPredicate as-of T" endpoint.
- Per-transition events (additional event type, not replacement).

## §13. Configuration env vars

Per Gate 4: each env-var-introducing PR updates **its slice** of `cmd/cyoda/help/content/config/changefeed.md` + `README.md` + `DefaultConfig()` **in the same change**. The PR plan in `§Workflow` is restructured to enforce this (each PR carries its own env-var docs slice).

| Env var | Default | Purpose | Introduced in |
|---|---|---|---|
| `CYODA_CHANGEFEED_MAX_LOOKBACK` | `15m` | `startFromTimestamp` past-clamp | PR 4 |
| `CYODA_CHANGEFEED_BODY_CACHE_BYTES` | `268435456` (256 MB) | Body LRU cap | PR 5 |
| `CYODA_CHANGEFEED_MAX_CONSUMERS_PER_NODE` | `1000` | Operational ceiling | PR 5 |
| `CYODA_CHANGEFEED_POSTGRES_POLL_INTERVAL` | `10s` | Missed-notify backstop | PR 3 |
| `CYODA_CHANGEFEED_RETENTION` | `7d` | Outbox retention; ≥ `MAX_LOOKBACK` | PR 2 |
| `CYODA_CHANGEFEED_BACKLOG_MAX_EVENTS` | `1000000` | Backlog ceiling | PR 5 |
| `CYODA_CHANGEFEED_BACKLOG_MAX_AGE` | `7d` | Backlog ceiling | PR 5 |
| `CYODA_CHANGEFEED_BACKLOG_SWEEP_INTERVAL` | `5m` | Backlog ceiling enforcement | PR 5 |
| `CYODA_CHANGEFEED_ACK_WINDOW_EVENTS` | `1024` | Per-consumer ack window | PR 5 |
| `CYODA_CHANGEFEED_ACK_WINDOW_BYTES` | `16777216` (16 MB) | Per-consumer ack window; aligned with gRPC limit | PR 5 |
| `CYODA_CHANGEFEED_CACHE_TTL` | `60s` | Positive content-cache TTL (= `EXISTENCE_SYNC_INTERVAL`) | PR 6 |
| `CYODA_CHANGEFEED_EXISTENCE_SYNC_INTERVAL` | `60s` | Periodic full-sync of existence set; also the propagation floor | PR 6 |
| `CYODA_CHANGEFEED_QUEUE_DEPTH` | `4096` | Per-consumer in-process queue depth | PR 5 |

CHANGELOG entries land per-PR (Keep a Changelog format). Final aggregation in PR 9.

## §14. Test strategy

### TDD per CLAUDE.md Gate 1

Failing tests drive every component.

### E2E per Gate 2

User-facing behaviour covered in `internal/e2e/`. New tests:
- Subscription CRUD lifecycle.
- gRPC attach (durable + ephemeral; broadcast + group on cassandra).
- At-least-once delivery via simulated disconnect.
- Backlog ceiling tripping → `DETACH_CONSUMER`.
- Preload-bridging via `startFromTimestamp` with explicit propagation-floor verification.
- Tenant isolation cross-check.
- Cascade segment emits one envelope, not multiple.
- Post-CBD segment carries the engine-chosen continuation `transition_name`.
- Recovery-replay produces same `event_id` and envelope.

### Parity per existing pattern

Storage-plugin parity tests in `e2e/parity/registry.go` extend to cover `ChangeFeed`. Cassandra plugin picks up new parity tests automatically via the registry.

### SPI tests

`cyoda-go-spi/spitest/` gets `ChangeFeed` contract tests.

### gRPC stream tests

Follow existing `internal/grpc/streaming_test.go` pattern for new `EntityChangeService`.

### Verification commands

Per CLAUDE.md "Plugin submodules need explicit test runs": `go test ./...` from root skips `plugins/{memory,sqlite,postgres}`. Use **`make test-all`** (or `make test-short-all` for iteration) to cover root + all plugin submodules. Race detector (`go test -race ./...`) is end-of-deliverable for the final PR series.

---

## Workflow context

Standard cyoda flow per `CLAUDE.md`:
- Brainstorming → this spec (four fresh-context review iterations applied).
- Next: `superpowers:writing-plans` to produce the implementation plan.
- Implementation track is multi-PR. Each PR carries its env-var doc slice (help topic + README + DefaultConfig) and a CHANGELOG entry:

  - **PR 1**: SPI ChangeFeed interface + `TransactionState.SegmentInvokedTransition` + `EntityMeta` clarifications. Bundled into one SPI tag per `feedback_spi_coordinated_release_procedure`. Includes `COMPATIBILITY.md` update.
  - **PR 2**: Memory + SQLite ChangeFeed implementations. Engine emission for non-cassandra path. Carries `CYODA_CHANGEFEED_RETENTION` docs.
  - **PR 3**: Postgres ChangeFeed implementation + outbox indexes (cursor + sweeper) migration. Carries `CYODA_CHANGEFEED_POSTGRES_POLL_INTERVAL` docs.
  - **PR 4**: REST CRUD endpoints + auth + audit logging + `bodyPredicate` validator (recursive walker). Carries `CYODA_CHANGEFEED_MAX_LOOKBACK` docs.
  - **PR 5**: gRPC `EntityChangeService` + fan-out + filter/body machinery + body LRU cache + ack window + backlog sweep + gRPC max-message-size config. Carries the bulk of `CYODA_CHANGEFEED_*` env-var docs.
  - **PR 6**: Subscription cache (existence set + content cache + periodic sync + broadcast invariants + propagation-floor enforcement). Carries `CYODA_CHANGEFEED_CACHE_TTL` and `CYODA_CHANGEFEED_EXISTENCE_SYNC_INTERVAL` docs.
  - **PR 7**: Cassandra `CommitEntityInfo.SegmentInvokedTransition` extension + `tx_writes` schema migration. **Cross-repo PR pair** (cyoda-go-cassandra change → tag → cyoda-go pin bump). Includes `COMPATIBILITY.md`.
  - **PR 8**: Cassandra `entity_change_events` table + materialize integration + publisher daemon + redpanda consumer-group integration for GROUP. Carries chunk-size guidance update in `cmd/cyoda/help/content/config/cassandra.md`.
  - **PR 9**: CHANGELOG aggregation, final `COMPATIBILITY.md` sync, README configuration-section polish.

PR 0 (sqlite ≤ alignment) dropped from prerequisite chain — functional equivalence at ms resolution. Lands independently as a parity cleanup whenever convenient.
