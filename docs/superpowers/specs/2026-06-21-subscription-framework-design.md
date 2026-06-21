# Subscription Framework — v1 Design

**Date:** 2026-06-21
**Status:** Design — awaiting plan
**Scope:** v1 notification framework. Out of scope: substitute for a queue subsystem (redpanda, kafka, etc.); historical event replay; entity-event-queue semantics.

## §1. Scope & vocabulary

A subscriber framework that lets client nodes (not only compute nodes) attach to cyoda over gRPC, declare a filter on entity model + workflow + body, and receive change events as transitions commit — with a per-subscription choice of ephemeral or durable delivery.

This is **a v1 notification framework, not a messaging or entity-event-queue framework.** Applications that need a durable, replayable, high-throughput queue subsystem build it on their own infrastructure (redpanda, kafka, NATS, …); a future cyoda-native entity-event-queue would be a separate capability built on a real queue substrate.

### Vocabulary

| Term | Meaning |
|---|---|
| **Change event** | One emission from the engine when a workflow transition commits, carrying envelope (`model_name`, `model_version`, `entity_id`, `kind`, `fromState`, `toState`, `transition_name`, `transaction_id`, `cursor`, `event_id`, `emitted_at`, `tenant_id`) and optionally the entity body (`body_before` / `body_after`). |
| **Subscription** | A server-side declaration: filter + delivery mode + consumption mode + batching + body-include policy. Ephemeral (lives with the stream) or durable (persists across connections). |
| **Delivery mode** | `LIVE_ONLY` (no replay; ephemeral) or `DURABLE` (at-least-once delivery for events emitted after subscription start, replay possible from the subscription's last-committed cursor within outbox retention). |
| **Consumption mode** | `BROADCAST` (each attached consumer gets every event) or `GROUP` (events partition across consumers by `entity_id`). Ephemeral is always `BROADCAST`. |
| **ChangeFeed** | SPI capability each storage plugin implements internally. Atomic event append + filtered consume. |
| **Cursor** | Opaque server-issued token; monotone within a partition. Subscriber commits to advance. |
| **`startFromTimestamp`** | Optional subscription field. Permits bounded preload bridging — server-clamped by `maxLookback`. |
| **Bootstrap point** | The `(createdAt, cursorZero, effectiveStartFrom)` tuple returned on subscription create. Defines the strict-`>` boundary between "preload territory" and "subscription territory". |

### Non-goals for v1

- JSONPath body projection / JSON Patch diff in the envelope.
- Failed-transition events (extensibility hook reserved on `kind`).
- Processor outputs in the envelope (extensibility hook reserved).
- Subscriber-overridable partition key for GROUP mode (`entity_id` only).
- ChangeFeed as a separately pluggable SPI capability (it lives internal to the storage plugin in v1).
- Pulling redpanda into OSS core (each plugin chooses its own backing).
- Cursor rewind / historical reprocessing of acknowledged events — subscribers needing this build it from the lifecycle-index / search APIs on their side.
- Unbounded historical replay. **Bounded preload bridging up to `maxLookback`** (cluster-configurable, default 15 minutes) is in scope; longer historical reads are the lifecycle-index API's job.
- A substitute for a real durable queue subsystem.

## §2. Subscription model

A subscription is the unit the subscriber declares; everything else flows from it.

### Shape

```json
{
  "id": "sub_01H...",                   // server-assigned for durable; omitted for ephemeral
  "tenantId": "...",                    // derived from JWT; never client-supplied
  "filter": {
    "modelName": "ORDER",
    "modelVersion": "1.0",              // exact match; "*" allowed for any version
    "kinds":      ["CREATE","UPDATE","DELETE"],   // optional; default all
    "fromState":  null,                  // optional; envelope predicate
    "toState":    "PAID",                // optional; envelope predicate
    "bodyPredicate": "$.amount > 1000"   // optional; search DSL subset
  },
  "delivery":    "LIVE_ONLY" | "DURABLE",
  "consumption": "BROADCAST" | "GROUP",  // GROUP requires DURABLE
  "includeBody": "NONE" | "AFTER" | "BEFORE_AND_AFTER",
  "batching": {
    "windowMs":   100,                   // 0 = disabled (per-event delivery)
    "maxEvents":  500                    // server flushes whichever trips first
  },
  "startFromTimestamp": "2026-06-21T10:15:00Z",  // optional; default = T_now at creation
  "ttl": "30d",                          // durable only; auto-delete if no consumer attaches for ttl
  "createdAt": "...",                    // server-issued
  "lastAttachedAt": "..."
}
```

### Validity matrix

| delivery | consumption | allowed? |
|---|---|---|
| `LIVE_ONLY` | `BROADCAST` | yes (default) |
| `LIVE_ONLY` | `GROUP` | no — group requires durable cursor state |
| `DURABLE` | `BROADCAST` | yes |
| `DURABLE` | `GROUP` | yes |

### Defaults

| Field | Default | Rationale |
|---|---|---|
| `kinds` | all three | most subscribers want all CRUD; explicit subset is the narrow case |
| `includeBody` | `NONE` | smallest payload by default; opt-in for bodies |
| `batching.windowMs` | `0` | predictable latency by default; opt-in for fan-out efficiency |
| `startFromTimestamp` | `T_now` at create | live-only behaviour by default; opt-in for bounded preload bridging |
| `delivery` | (required, no default) | force the deliberate durability decision |
| `consumption` | `BROADCAST` | covers the two stated use cases (cache invalidation, BFF push) |
| `ttl` | `30d` | durable only |

### Identity rules

- `tenantId` always derived from JWT, never accepted from client. Cross-tenant subscribing is rejected at create.
- Subscription `id`: server-generated ULID for durable subs. Ephemeral subs have no id — they're scoped to the stream.
- `GROUP` consumer joins identify themselves with a free-form `consumerInstanceId` (client-chosen, typically pod name). Two attaches with the same `consumerInstanceId` to the same subscription: the second wins; the first is disconnected.

### TTL behaviour

- Durable subscription with no consumer attached for `ttl` is auto-deleted. Default 30 days.
- "Never expire" requires admin-level scope.
- Backlog ceiling (per subscription) applies separately (see §3.5). Exceeding the backlog ceiling does not delete the subscription; it triggers `DETACH_CONSUMER` policy by default.

## §3. API surface — REST CRUD + gRPC stream

### REST management plane

All endpoints scoped under the tenant from JWT.

| Method & path | Purpose | Notes |
|---|---|---|
| `POST   /tenants/{t}/subscriptions` | Create durable subscription | Body = subscription shape from §2 minus server-issued fields. Returns 201 + full resource + `cursorZero` + `effectiveStartFrom` + `maxLookback`. |
| `GET    /tenants/{t}/subscriptions/{id}` | Read subscription | Returns full resource + lag metrics (events behind tail, last cursor advanced). |
| `GET    /tenants/{t}/subscriptions` | List subscriptions | Paginated; filter by `modelName` / `consumerInstanceId` via query params. |
| `PATCH  /tenants/{t}/subscriptions/{id}` | Update subscription | Mutable fields only: `ttl`, `batching.windowMs`, `batching.maxEvents`, `includeBody`. Filter, delivery, consumption, startFromTimestamp are immutable — create a new subscription instead. |
| `DELETE /tenants/{t}/subscriptions/{id}` | Delete subscription | Detaches connected consumers, deletes cursor state, removes outbox retention claim. Idempotent. |

Ephemeral subscriptions have no REST surface — they're created and destroyed via the gRPC stream.

Errors follow existing cyoda conventions (4xx full domain detail with error code; 5xx generic + ticket UUID; see `.claude/rules/security.md`).

### gRPC stream — `EntityChangeService`

New proto service, separate from `CloudEventsService`:

```proto
service EntityChangeService {
  // Bidirectional stream. Client sends Attach + Acks; server sends ChangeEvent batches.
  rpc Stream(stream ClientMessage) returns (stream ServerMessage);
}
```

Inside the stream, messages are CloudEvents (wire format consistency with the rest of the gRPC plane). Typed messages are listed in §6.

**Why separate from `CloudEventsService.startStreaming`:** compute-node `startStreaming` already mixes processor invocations and criterion evaluations; adding subscriber traffic on the same channel would mix three different request/response patterns and complicate diagnostic and load-isolation work. The CloudEvents wire format is shared.

One stream = one attached subscription. A subscriber attaching to N subscriptions opens N streams. Avoids head-of-line blocking across subscriptions.

### Auth

- REST: existing JWT bearer + tenant scope. New permission `subscriptions:write` for create/update/delete; `subscriptions:read` for get/list.
- gRPC stream: same JWT in metadata, same tenant derivation. Permission `subscribe:model:<modelName>` checked at attach time — granular enough to let a tenant grant a BFF service account access to `ORDER` events but not `INVOICE`.
- Cross-tenant attach = `PERMISSION_DENIED`.

## §3.5. Connection lifecycle & recovery

Subscriptions, cursors, and partition assignments are authoritative server state. The gRPC stream itself carries **no** durable state — it's a delivery channel that can be torn down and re-established.

### Scenario matrix

| Scenario | Ephemeral (`LIVE_ONLY` + `BROADCAST`) | Durable (`DURABLE`, `BROADCAST` or `GROUP`) |
|---|---|---|
| **Clean detach** | Subscription state dropped immediately. No replay. | Cursor at last ack persisted. GROUP: partitions reassigned to other consumers. Subscription resource untouched. |
| **Network blip** | State dropped after gRPC keepalive detects death. Reconnect = new ephemeral subscription. | Server detects death via keepalive. Cursor at last ack. GROUP: partitions held under grace period (default 30s) before reassignment. Reconnect via `attach.durable.v1` with same `subscriptionId`+`consumerInstanceId` resumes from last ack. |
| **Client restart / pod replacement** | New ephemeral subscription on the new stream. | Same as network blip; new pod with new `consumerInstanceId` joining = standard GROUP rebalance. |
| **Server node dies** | Stream errors `UNAVAILABLE`. Reconnect to another node = new ephemeral subscription. | Stream errors `UNAVAILABLE`. Reconnect to another node; subscription resource lives in shared storage. Cursor picks up where the dead node's last ack landed. GROUP: dead node's partitions enter grace period and reassign. |
| **Cluster rebalance** | No effect — subscriber stays attached. | Same — subscription resource is in shared storage; fan-out reloads partition assignments on cluster-membership events. |
| **Long disconnection** | Subscription gone with the stream. | Durable subscription persists; on reconnect, replay from last cursor if within retention; else `OUT_OF_RANGE` with the earliest still-available cursor returned. Operator decides to accept the gap or fail. |
| **TTL exceeded with no consumer** | N/A | Subscription auto-deleted; reconnect = `NOT_FOUND`. |
| **Mid-batch disconnect** | Batch lost. | Batch redelivered from last committed cursor (at-least-once). Consumer dedupes via `event_id`. |

### Mechanism (in one paragraph)

All authoritative state — subscription resource, cursor per consumer instance, partition assignments — lives in the storage tier (durable) or in-process on the attached node (ephemeral). gRPC stream death is just a signal to mark the consumer-instance disconnected and start the grace timer (GROUP) or release the slot (BROADCAST). Reconnect is a fresh `attach` with the same identifiers; the server materialises per-stream goroutines from persisted state and resumes from the last committed cursor. This is why at-least-once requires `event_id` for consumer-side dedupe.

### Operational knobs

| Knob | Default | Notes |
|---|---|---|
| **GROUP rebalance grace period** | 30s | Time between consumer-instance disconnect detection and partition reassignment. Cluster-tunable; 0 = rebalance immediately. |
| **Backlog ceiling per subscription** | 1M events or 7d, whichever first | Hard ceiling for un-acked backlog. |
| **Backlog ceiling policy** | `DETACH_CONSUMER` | Alternatives: `BLOCK_APPEND` (back-pressures the engine; risky), `DROP_OLDEST` (explicit at-most-once degradation). |

## §4. Event source & emission

### Source

Single emission point: **the engine's transition-commit hook**, after processors run, after criteria pass, after the SPI write has been ordered into the same transaction. No other code path emits change events.

### Atomicity

The change event is part of the same SPI transaction as the entity write. Either both commit or neither does. This is the property that makes durable subscribers' at-least-once guarantee real (no event-without-write, no write-without-event).

```
tx := spi.Begin(ctx)
spi.SaveEntity(tx, newState)
spi.ChangeFeed.AppendChange(tx, envelope)   // same tx handle
tx.Commit()                                  // success ⇒ event will eventually be visible
```

### Emission gate (after §4.2 revision)

The engine emits at every transition commit **iff** both:

1. At least one subscription exists for the event's `(tenant, model_name, model_version)` (the per-`(tenant, model, version)` enablement boundary, §4.1), AND
2. At least one registered subscription's **envelope filter** matches the event (model, version, kinds, fromState, toState). Short-circuit on first match. Body predicates are **not** evaluated at emission.

The check is O(1) on an in-memory per-`(tenant, model, version)` index of envelope filters maintained on subscription create/delete.

**Fail-open**: if the subscription-registry cache is unavailable, emit anyway. Correctness over cost.

The engine never consults specific subscription IDs at emission time. The outbox row is the event, not a delivery list. Per-subscriber decisions live at fan-out.

### Envelope construction

Constructed from data the engine already has on hand at transition-commit time:

```
event_id          ULID            generated at emission
tenant_id         from request context (JWT-derived)
transaction_id    from engine tx context (verification item — §11)
cursor            opaque, monotone within partition; assigned by ChangeFeed.AppendChange
emitted_at        server clock at emission
model_name        from entity model
model_version     from entity model
entity_id         from entity
kind              CREATE (initial-state entry) | UPDATE | DELETE (terminal or hard-delete)
from_state        from transition (empty on CREATE)
to_state          from transition (empty on hard DELETE; terminal state on soft-delete)
transition_name   from transition
body_after        attached at fan-out, not at emission
body_before       attached at fan-out, not at emission
```

### What is *not* emitted in v1

- Failed transitions (criterion rejected, processor errored). Reserved for future via `kind` extensibility on the envelope.
- Non-transition writes (admin overrides, bulk imports). Confirmed not to exist today; if they ever do, design choice deferred (synthetic system transition vs. invisibility).
- Engine-internal events (rebalance, leader election). Out of scope.
- Events prior to the tenant's enablement boundary or outside outbox retention. Historical reconstruction is the lifecycle-index API's job, a separate contract.

## §4.1. Per-(tenant, model, version) enablement boundary

- **First subscription** created in a tenant for a given `(model_name, model_version)` atomically:
  1. writes the subscription row,
  2. sets the `(tenant, model_name, model_version)` enablement boundary marker to `T_now`,
  3. invalidates the per-tenant emission-decision cache so subsequent transitions emit.
- From that moment on, every transition matching that `(tenant, model, version)` is subject to the §4 emission gate.
- The boundary timestamp becomes the **floor** for the implicit and explicit `startFromTimestamp` of any subsequent subscription for the same `(tenant, model, version)`.
- The boundary is surfaced in the create-subscription response (`enablementBoundary: {tenantFirstEnabledAt: "..."}`) so operators can see the floor explicitly.
- Earlier transitions are not in the outbox and cannot be replayed via the change feed — by design.

## §4.2. Filtering & body materialisation

### Pipeline per event E and candidate subscription S

1. **Envelope match** — `modelName`, `modelVersion`, `kinds`, `fromState`, `toState`. Pure metadata; no body needed. Always done at fan-out.
2. **Body predicate match** (only if S has one) — search-DSL expression like `$.amount > 1000`. Needs the entity body.
3. **Ship** — envelope and (per S's `includeBody`) body_before / body_after.

### Body load contract at fan-out

The entity body is loaded at fan-out **iff** at least one local consumer for that event has either:

- a body predicate (to evaluate the predicate), OR
- `includeBody != NONE` (to ship the body).

Bodies are LRU-cached per event so multiple consumers needing the same body share one load. Cache is scoped per-event and released after all local consumers of the event have shipped.

**Documented cost** — the body-predicate + `includeBody: NONE` combination is allowed but triggers a body load at fan-out for predicate evaluation alone. Subscribers opting into this combination accept the I/O cost; envelope-only subscribers with no body predicate never trigger a body load.

### Replay (durable lagging)

For replayed events older than the live cache, body hydration goes through the lifecycle index by version pair. The lifecycle-index retention must not be shorter than the change-feed outbox retention (cross-checked as a plugin invariant).

### Pre-state visibility

For `BEFORE_AND_AFTER`, the pre-state is available on the engine stack at live-emission time (the engine had to read it to perform the transition). For replay, the lifecycle index supplies it. This is a property the fan-out layer leverages — passing pre-state through the in-process channel where possible, falling back to lifecycle-index reads for replay.

### Subscription mutation invariants

PATCH to `includeBody`, `batching`, `ttl` is fine — these don't affect emission decisions or stored event content. Filter (envelope + body predicate), delivery, consumption, and startFromTimestamp are **immutable** — changing them changes semantics that ripple through cursor identity. Mutation = create a new subscription.

### Subscription create/delete cache invariant

The per-`(tenant, model, version)` envelope-filter index must reflect subscription creates/deletes before any subsequent transition is emitted, otherwise emission gates can miss new subscriptions or include deleted ones. Standard cluster-membership broadcast on subscription resource change; emission nodes refresh their cache on receipt.

## §5. SPI ChangeFeed capability

### Interface

```go
package spi

type ChangeFeed interface {
    // AppendChange writes the change event atomically with the entity write.
    // tx is the same transaction handle the engine used for SaveEntity / DeleteEntity.
    // The implementation MUST guarantee: if tx.Commit() returns nil, the event is
    // durable and will eventually be visible to consumers; if tx rolls back, the
    // event is never visible.
    AppendChange(ctx context.Context, tx TxHandle, envelope ChangeEnvelope) error

    // Consume returns a stream of change events matching spec, starting at
    // spec.StartCursor. Implementations apply envelope filters and tenant scoping
    // at the storage layer where possible. Body predicate evaluation is the
    // caller's responsibility.
    //
    // For GROUP mode, the implementation receives a partition range (computed by
    // the fan-out layer from the consumer-group assignment) and returns only
    // events whose entity_id hashes into that range.
    //
    // The returned channel is closed when ctx is cancelled or when the
    // implementation can no longer serve the stream (in which case Err returns
    // the reason).
    Consume(ctx context.Context, spec ConsumerSpec) (ChangeEventStream, error)

    // Retain advertises a retention claim for a subscription. Implementations
    // MUST retain events at or after the claimed cursor until Release is called
    // or the claim expires.
    Retain(ctx context.Context, claim RetentionClaim) error
    Release(ctx context.Context, subscriptionID SubscriptionID) error

    // Capabilities reports what this implementation supports.
    Capabilities() ChangeFeedCapabilities
}

type ChangeEnvelope struct {
    EventID        ULID
    TenantID       uuid.UUID
    TransactionID  string             // empty if engine doesn't surface tx id
    Cursor         Cursor             // assigned by AppendChange
    EmittedAt      time.Time
    ModelName      string
    ModelVersion   string
    EntityID       uuid.UUID
    Kind           ChangeKind         // CREATE | UPDATE | DELETE
    FromState      string
    ToState        string
    TransitionName string
    BodyRef        LifecycleVersionRef // for downstream body hydration
}

type ConsumerSpec struct {
    TenantID       uuid.UUID
    EnvelopeFilter EnvelopeFilter   // model, version, kinds, fromState, toState
    StartCursor    Cursor           // always ≥ subscription's cursorZero
    PartitionRange *PartitionRange  // GROUP mode only
}

type ChangeFeedCapabilities struct {
    Durable          bool
    GroupConsumption bool
    MaxRetention     time.Duration
    MaxBacklogPerSub int
}
```

### Per-backend implementation sketch

| Backend | AppendChange | Consume | Retain/Release | Capabilities |
|---|---|---|---|---|
| **memory** | push to in-process channel (no tx) | range over channel buffer with filters | no-op (events live only in flight) | `Durable=false, Group=false`, retention=0 |
| **sqlite** | INSERT into `change_outbox` in the tx | **in-process broadcaster signals post-commit**; per-consumer bounded queues; catchup-by-cursor on attach. No polling in normal operation. | UPDATE retention claim row; janitor sweeps released events past min-claim | `Durable=true, Group=true (single-node)`, retention configurable |
| **postgres** | INSERT into `change_outbox` in the tx; `pg_notify` post-commit (best-effort wake-up) | `LISTEN/NOTIFY` for low-latency wake-up; periodic poll (default 10s, configurable) as missed-notify backstop. Both paths read from `change_outbox` with envelope filters in SQL WHERE. Cross-node delivery is per-node: each node LISTENs and polls independently. | claim row + per-row retention floor; sweep daemon | `Durable=true, Group=true`, retention configurable |
| **cassandra** | append to existing redpanda topic via existing `QueueAdaptor`; cassandra row in lifecycle index covers durable replay before redpanda retention | consumer-group on redpanda with envelope filters applied post-fetch; cassandra read for older-than-redpanda backlog | redpanda consumer-group offset commits; cassandra retention via lifecycle-index TTL | `Durable=true, Group=true`, retention = max(redpanda retention, lifecycle-index retention) |

Polling defaults are sized for missed-event recovery, not primary delivery latency:
- SQLite: in-process notify is the primary signal; no polling in normal operation.
- Postgres: LISTEN/NOTIFY is the primary signal; poll default 10s, cluster-tunable, observability counter `changefeed.notify_missed_events_total`.

### Engine-side contract

- Engine calls `AppendChange` inside the same SPI tx as `SaveEntity` / `DeleteEntity`.
- Engine produces the envelope from data already in scope at transition commit; no extra reads.
- Engine never calls `Consume` — that's the fan-out layer's interface.

### Fan-out layer (engine-internal, not SPI)

- One goroutine per attached consumer, per node.
- Calls `ChangeFeed.Consume(spec)` with the consumer's envelope filter (body predicate is applied locally in the fan-out goroutine).
- Receives envelope stream; for each event, applies body predicate (which may require body hydration via `BodyRef`); ships to gRPC stream per consumer's `includeBody`.
- LRU cache for hydrated bodies, keyed by `BodyRef`, scoped per-event.
- Tracks per-consumer ack cursor; periodically calls `Retain` to advance the retention floor.

### Cross-node coordination

- Subscription resource lives in shared storage (durable) or in the local node's memory (ephemeral).
- Any node can serve any consumer for any durable subscription. The fan-out goroutine is wherever the consumer's stream landed (gRPC LB routes).
- For GROUP mode: consumer-instance membership is per-subscription cluster resource. Standard rebalance protocol (heartbeat + revocation + assignment) over the existing cluster-membership channel; partition assignment metadata in storage.

### Plugin choice

The `ChangeFeed` interface makes the implementation choice **internal to the plugin**. A future `postgres-redpanda` plugin can layer redpanda on top of postgres storage via the transactional-outbox pattern (outbox row in pg tx + relay to redpanda). The OSS-shipped postgres plugin uses LISTEN/NOTIFY + poll backstop; no external infrastructure required.

## §6. Wire protocol & cursor semantics

### Wire format

All stream messages are CloudEvents.

**Server → client:**

| `type` | `data` schema | Notes |
|---|---|---|
| `cyoda.change.event.v1` | single `ChangeEvent` JSON | Used when `batching.windowMs == 0`. |
| `cyoda.change.batch.v1` | `{ events: [ChangeEvent...], lastCursor: string }` | Used when batching enabled. Ack of `lastCursor` commits the whole batch. |
| `cyoda.subscriber.rebalance.v1` | `{ revoked: [partitionId...], assigned: [partitionId...], generation: int }` | GROUP only. Drain in-flight work for revoked partitions and ack the rebalance marker before new partition events begin. |
| `cyoda.subscriber.error.v1` | `{ code: string, message: string, ticket?: string }` | Terminal. 5xx codes carry ticket UUID. |
| `cyoda.subscriber.heartbeat.v1` | `{ serverTimeMs: int }` | Periodic keepalive when no events flow. Default 15s. |

**Client → server:**

| `type` | `data` schema | Notes |
|---|---|---|
| `cyoda.subscriber.attach.durable.v1` | `{ subscriptionId: string, consumerInstanceId: string, fromCursor?: string }` | `fromCursor` omitted = resume from last server-recorded ack. If provided, must be ≥ last server-recorded ack (forward-skip allowed; rewind rejected with `INVALID_ARGUMENT`). Must be within retention. |
| `cyoda.subscriber.attach.ephemeral.v1` | inline subscription spec | No id, no cursor; lives with the stream. |
| `cyoda.subscriber.ack.v1` | `{ cursor: string }` | Commits all events ≤ this cursor on the partition. |
| `cyoda.subscriber.rebalance.ack.v1` | `{ generation: int }` | GROUP only. Acks the rebalance marker; server begins delivering for newly assigned partitions. |
| `cyoda.subscriber.detach.v1` | empty | Graceful detach; server commits any pending ack, releases partition assignment, closes stream cleanly. |

### Cursor semantics

A `Cursor` is an opaque, server-issued, monotone-within-partition token. Wire shape: base64 string. Subscribers never interpret or construct one.

Properties the server guarantees:

- **Monotonicity within a partition** — `cursor_n+1 > cursor_n` for consecutive events on the same partition.
- **No collation across partitions.**
- **Stable across reconnects** — same cursor refers to the same event.
- **Within retention bounds** — older than retention floor returns `OUT_OF_RANGE` with the earliest still-available cursor.

Internally, a cursor is most naturally `(partition_id, sequence)`. The base64 encoding hides the structure so it can change without breaking clients.

### Ack & commit semantics

- **At-least-once** — an event is delivered until acked. Redelivery possible on disconnect.
- **Cumulative acks** — acking cursor C commits all events ≤ C on that partition. No selective ack within a batch.
- **Ack window** — server keeps un-acked events in-flight up to a per-consumer limit (default 1024 events or 16 MB, whichever first). When the window is full, the server stops sending until acks arrive. **This is the backpressure mechanism, in-band.**
- **Acks are cheap** — per event, per batch, or periodic flush.

### Batching

When `batching.windowMs > 0`:

- Server accumulates events for the window or until `maxEvents`.
- Emits one `cyoda.change.batch.v1` with the ordered event list and `lastCursor`.
- Subscriber acks `lastCursor` to commit the whole batch.
- Partial-batch loss on disconnect → whole batch replayed on reconnect (idempotency via `event_id`).

In GROUP mode, batches respect partition boundaries — one batch contains events from one partition only.

### Heartbeats & keepalive

- Server sends `heartbeat.v1` every 15s when no events flow.
- Client missing 3 consecutive heartbeats (45s default) treats the stream as dead.

### Idempotency

`event_id` is a ULID and the consumer's idempotency key. The server never reuses an event_id within a tenant. Consumers MUST dedupe by `event_id` — at-least-once redelivery can produce the same event twice.

### Subscription resource changes mid-stream

When a durable subscription is mutated via REST (e.g., `includeBody` updated), the server gracefully closes any attached stream with a specific code; the client reconnects and picks up the new shape. Avoids running multiple stream protocol versions on one connection.

## §6.1. Bootstrap & preload bridging

### Bootstrap point

On `POST /tenants/{t}/subscriptions`, the server records `subscription.createdAt` and resolves `effectiveStartFrom`:

```
effectiveStartFrom = clamp(
  requestedStartFromTimestamp ?? T_now,
  lower = max(enablementBoundary, T_now - maxLookback, retentionFloor),
  upper = T_now
)
```

If the requested value is outside this range, the server returns `400 Bad Request` with the valid range echoed back so the client can re-request with a clamped value.

The subscription delivers every event with `emitted_at > effectiveStartFrom` matching the filter. **Strictly greater.** No event at-or-before `effectiveStartFrom` is delivered.

The cursor of the first event ever delivered is the smallest cursor whose `emitted_at > effectiveStartFrom` — `cursorZero`. The server returns `cursorZero` as part of the create response so applications don't have to infer it.

### Create response

```json
{
  "id": "sub_01H...",
  "createdAt": "2026-06-21T10:15:00.123Z",
  "effectiveStartFrom": "2026-06-21T10:15:00.000Z",
  "cursorZero": "...",
  "maxLookback": "15m",
  "enablementBoundary": { "tenantFirstEnabledAt": "..." }
}
```

### Recommended preload-bridging pattern

```
1. T_preload = server-current-time (or known timestamp from your last snapshot)
2. Query "entities matching <filter> as of T_preload" → preload cache
3. POST subscription with startFromTimestamp = T_preload
4. Consume stream — events from > T_preload forward
```

The server-side strict-`>` contract makes the partition clean: state-as-of-`T_preload` reflects every transition with `emitted_at ≤ T_preload`; the subscription delivers every transition with `emitted_at > T_preload`. No gap, no overlap. No client-side buffering needed.

### maxLookback

Cluster-configurable (no hard ceiling baked in). Default 15 minutes. Operator chooses what fits their deployment.

- Configured via `CYODA_CHANGEFEED_MAX_LOOKBACK` (or similar naming consistent with cyoda's env-var convention).
- Outbox retention must be ≥ `maxLookback` to be meaningful; documented as a plugin invariant cross-checked at startup.
- Documentation guidance: "minutes, not hours; not for historical replay. For historical reads, use the lifecycle-index / search APIs."

### Why strict-`>`

A transition committing at wall-clock `T` is given `emitted_at = T`. A subscription with `effectiveStartFrom = T` could either include or exclude that transition; one choice must be made:

- **Strict `>`** — preload query "as of T" includes the T-transition; subscription excludes it. Clean partition; idempotency only needed for at-least-once redelivery (disconnect/reconnect), not for the bootstrap path.
- **`≥`** — both include; guaranteed double-apply at the boundary, dedupe at every bootstrap.

Strict `>` is the cleaner contract.

### What this enables vs. what cyoda offers as a primitive

Applications can build their own durable views or queues by combining preload + subscription. cyoda's surface is a **reliable change-data-capture tap**, not a queue subsystem. The downstream queue (if any) lives in the application's infrastructure.

## §7. Backpressure & slow-consumer policy

### In-band backpressure (live consumers)

The ack-window mechanism in §6 is the primary backpressure path. The server stops sending when the un-acked window is full; the consumer's pace drives the producer.

### Per-subscriber resource limits

| Limit | Default | Notes |
|---|---|---|
| Ack window — events | 1024 | Configurable per consumer at attach |
| Ack window — bytes | 16 MB | Whichever trips first |
| Per-consumer queue depth (SQLite/memory in-process) | 4096 events | Local buffer between broadcaster and per-consumer goroutine |
| Backlog ceiling | 1M events / 7d | Hard limit; see §3.5 |

### Slow-consumer policy

| Policy | Default | Effect |
|---|---|---|
| `DETACH_CONSUMER` | yes | Consumer exceeding backlog ceiling is detached; must reconnect, accepting any resulting gap (OUT_OF_RANGE if past retention). |
| `BLOCK_APPEND` | no | Backpressures into the engine. Risky — slow consumer can stall writes. Not recommended; available as an explicit operator override. |
| `DROP_OLDEST` | no | Cursor jumps forward; events silently lost. Explicit at-most-once degradation. |

Configured per subscription via the create-time policy field; can be PATCHed.

## §8. Auth & multitenancy

### Tenant scope

- `tenantId` is always derived from the JWT, never accepted from the client.
- All REST endpoints scoped under `/tenants/{t}/...`; mismatch between path tenant and JWT tenant → `PERMISSION_DENIED`.
- gRPC attach validates that the subscription's `tenantId` matches the JWT tenant; cross-tenant attach → `PERMISSION_DENIED`.
- Outbox / log is physically partitioned by tenant (postgres index, cassandra partition key) so a leaking query at the ChangeFeed layer cannot return another tenant's events (§gate-3).

### Permissions

| Permission | Required for |
|---|---|
| `subscriptions:read` | GET / list subscriptions |
| `subscriptions:write` | POST / PATCH / DELETE subscriptions |
| `subscribe:model:<modelName>` | Attach (durable or ephemeral) to a subscription whose filter targets `modelName` |
| `subscriptions:admin` | "Never expire" TTL; future ops knobs |

`subscribe:model:*` is the wildcard form for service accounts that need broad subscription rights.

### Audit logging

- Subscription create/update/delete logged with tenant + actor + filter summary (no body predicate content if it could carry sensitive substrings — predicate hashes only).
- Attach/detach logged with tenant + subscription id + consumer instance.
- Per `.claude/rules/security.md`: never log credentials, tokens, secrets, signing keys, or body content of subscribed events.

## §9. Failure modes & recovery

§3.5 covers connection lifecycle. This section catalogues the remaining failure modes.

### Storage / plugin failures

| Failure | Effect on emission | Effect on consume |
|---|---|---|
| **Storage tx rollback** | Entity write fails; change event never visible (atomicity guarantee) | No effect |
| **AppendChange returns error** | SPI tx rolls back; entity write fails too | No effect |
| **ChangeFeed.Consume backend error** | No effect on emission | Affected consumers' streams terminate with `INTERNAL` + ticket. Reconnect retries. |
| **Plugin restart / replacement** | Pending writes during restart fail; clients retry per existing cyoda semantics | Consumer streams reconnect, resume from last ack |
| **Storage corruption / data loss** | Out of scope for the framework — same posture as any other cyoda data loss event |

### Cluster failures

| Failure | Recovery |
|---|---|
| **Single node death** | Subscribers reconnect to surviving nodes; durable cursors picked up from shared storage. GROUP partitions reassign after grace period. |
| **Network partition (split-brain)** | Treated by existing cyoda cluster-membership protocol. Change feed delivery may pause on the minority side. |
| **Subscription registry cache unavailable** | Emission fails-open (emits anyway); fan-out treats it as transient and retries; alert metric `changefeed.registry_cache_unavailable_total` exposed |
| **Cluster-membership broadcast lost** | Per-`(tenant, model, version)` envelope-filter index goes stale on the affected node. Stale entries cause spurious emission (cost) or missed emission (correctness risk). Cache is refreshed on a periodic full-sync (default 60s); cluster broadcast is the primary, periodic sync is the backstop. |

### Subscriber-induced failures

| Failure | Recovery |
|---|---|
| **Subscriber dropping all acks** | Ack window fills; server stops sending; consumer is effectively stalled; backlog ceiling eventually trips → `DETACH_CONSUMER`. |
| **Subscriber acking ahead of received cursors** | Server rejects with `INVALID_ARGUMENT`; cursor must be one the server issued. |
| **Subscriber attaching with a cursor outside retention** | Server returns `OUT_OF_RANGE` + the earliest still-available cursor. Client decides to accept the gap or fail. |
| **Subscriber misbehaving (constant reconnect storm)** | Existing cyoda rate-limit interceptor applies to the gRPC stream's attach path. |

## §10. Out of scope for v1 (explicit non-goals)

- JSONPath body projection / JSON Patch diff in the envelope.
- Failed-transition events (extensibility hook reserved).
- Processor outputs in the envelope (extensibility hook reserved).
- Subscriber-overridable partition key (entity_id only in v1; subscription option non-breaking later).
- ChangeFeed as a separately pluggable SPI capability.
- Pulling redpanda into OSS core.
- Cursor rewind / historical reprocessing of acknowledged events.
- Unbounded historical replay. Bounded preload bridging up to cluster-configurable `maxLookback` is in scope.
- A substitute for a real durable queue subsystem.
- Admin override `tenant.changeFeed.alwaysOn = true` for forensics tenants (backlog item).
- Wire-tap / historical one-time subscription. Operators wanting this use the lifecycle-index API.

## §11. Open verification items (to be resolved during writing-plans)

1. **`transaction_id` field** — verify whether the engine surfaces a transaction id at the SPI boundary today. If yes, use it; if no, thread one through at plan time. The envelope field exists regardless; value may be empty until the thread-through lands.
2. **Point-in-time query clock-coherence** — `subscription.createdAt` is "monotone within a node" by easy implementation. The search/lifecycle-index API's "as of timestamp" semantics must use the same clock source for the preload-bridging contract to be airtight in multi-node deployments. Confirm cyoda's existing model uses a cluster-coordinated clock (HLC or similar). If not, surface as a known operational caveat: single-node deployments unaffected; multi-node deployments require clock-sync guarantees.
3. **Non-transition entity writes** — confirm that all entity mutations today go through the transition machinery. If admin overrides, bulk imports, or migrations bypass it, those mutations are invisible to the change feed in v1; downstream design decision (synthetic system transition) deferred unless/until that path is added.
4. **Search-DSL evaluator reuse** — confirm that the existing search-query DSL evaluator can be invoked in-process at fan-out time against a post-state body, without requiring a storage round-trip. If not, identify what extraction is needed.
5. **gRPC keepalive defaults** — confirm cyoda's existing gRPC server keepalive timings; default 15s heartbeat and 45s client-side death detection chosen to align, may need adjustment.
6. **Lifecycle-index version-pair body hydration** — confirm the postgres/sqlite/cassandra lifecycle-index APIs expose direct version-pair lookup for replay body hydration; if not, identify the extension.

## §12. Extension hooks reserved (for future versions)

These shape the v1 API so that v2 additions are non-breaking:

- **`kind` enum extensibility** — add `TRANSITION_FAILED` or other event kinds as additional enum values. Consumers ignore unknown kinds.
- **`includeProcessorOutputs: bool`** — populate processor outputs in the envelope. Off by default; new field on the subscription create schema.
- **`partitionKey` override** — `entity_id | tenant_id | jsonPath` instead of `entity_id` only. New optional field; default unchanged.
- **JSONPath body projection / JSON Patch diff** — new `includeBody` enum values or a sibling `projection` field. Subscribers ignore unsupported values.
- **ChangeFeed as separately pluggable SPI capability** — separate capability from `Storage`; coordination via the transactional-outbox pattern. Existing single-plugin-implements-both shape continues to work.
- **Admin "always on" change feed for forensics tenants** — bypass the §4 emission gate; ops knob.

---

## Workflow context

This spec follows the standard cyoda flow per `CLAUDE.md`:

- Brainstorming → this spec.
- Next: `superpowers:writing-plans` to produce the implementation plan.
- Implementation track will likely be multi-PR given the scope (SPI bump for `ChangeFeed`, new gRPC service, REST CRUD, per-plugin implementations, engine emission hook). The plan will partition this into reviewable units.
