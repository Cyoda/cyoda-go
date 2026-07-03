# Processor Execution Modes

This document describes the four `executionMode` values the workflow engine
accepts on a `ProcessorDefinition` and the exact transactional semantics each
one produces. It is the single reference for workflow authors choosing a mode
and for engine contributors maintaining the dispatch path.

Companion documents:

- [`cmd/cyoda/help/content/workflows.md`](../cmd/cyoda/help/content/workflows.md) — workflow schema, validation rules, JSON shape
- [`docs/CONSISTENCY.md`](CONSISTENCY.md) — SI+FCW isolation contract, phantom anomalies, operational rules
- [`docs/CONCURRENCY.md`](CONCURRENCY.md) — locks, in-process state scope, OpMu contract
- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) §3, §4.5 — transaction model and the `COMMIT_BEFORE_DISPATCH` swimlane
- [`cmd/cyoda/help/content/cluster.md`](../cmd/cyoda/help/content/cluster.md) — multi-node routing and segment pinning

---

## 0. Axis Summary

Workflow processors have two orthogonal configuration axes.

**`type` — execution-location.** Determines where the processor runs.
Currently only `externalized` (gRPC dispatch to a calculation node) is
implemented. The value `internalized` is reserved for in-process
execution; any transition firing a processor with `type: "internalized"`
is rejected at dispatch with `WORKFLOW_FAILED` (400). Empty or omitted
on the wire is treated as `externalized`.

**`executionMode` — transactional semantics of dispatch.** Determines
whether the dispatch is synchronous or asynchronous, and whether the
caller's transaction stays open across the dispatch. The four values
(`SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX`, `COMMIT_BEFORE_DISPATCH`) are
the focus of this document. All `executionMode` semantics described
below apply to `externalized` processors; the `internalized` location
has no documented dispatch semantics yet.

---

## 1. Quick Reference

| Mode | Synchrony | Open TX during dispatch | Result mutations applied | Failure | Suitable for |
|---|---|---|---|---|---|
| `SYNC` | blocks inline | yes (caller's TX) | yes | fatal — `WORKFLOW_FAILED` 400, entity stays in source state | fast, in-TX work; standard processor |
| `ASYNC_SAME_TX` | blocks inline | yes (caller's TX) | yes | fatal — same as `SYNC` | indistinguishable from `SYNC` today; reserved label |
| `ASYNC_NEW_TX` | blocks inline | yes (savepoint inside caller's TX) | **no — discarded** | non-fatal — warning logged, pipeline continues | fire-and-forget side effects (notifications, audit pings) |
| `COMMIT_BEFORE_DISPATCH` | blocks inline | **no** — `TX_pre` committed first | yes, via `CompareAndSave` against `T_pre` | fatal — `WORKFLOW_FAILED` 400, entity durable in pre-callout state | slow external work; connection-pool relief |

The engine implementation is in
[`internal/domain/workflow/engine_processors.go`](../internal/domain/workflow/engine_processors.go).
The dispatch switch lives at `executeProcessors:42-118`.

---

## 2. `SYNC` and `ASYNC_SAME_TX` (identical at runtime)

`SYNC` and `ASYNC_SAME_TX` are dispatched by the **same code path** —
`executeSyncProcessor` at `engine_processors.go:84-85`:

```go
default: // SYNC, ASYNC_SAME_TX — both inline in caller's transaction.
    procErr = e.executeSyncProcessor(currentCtx, entity, proc, workflow, transition, currentTxID)
```

There is no behavioural difference between them. The distinction is a label
for workflow authors, not a runtime contract — the engine never inspects
which of the two strings was used.

### Lifecycle

1. The engine is mid-transition inside the caller's open transaction `T`.
2. It calls `extProc.DispatchProcessor(ctx, entity, proc, …, txID=T)` over gRPC
   to a calculation member chosen by `calculationNodesTags`.
3. The transaction stays open and the database connection (or in-memory
   transaction state) is held for the duration of the wait.
4. The gRPC call uses `Config.ResponseTimeoutMs` (default 30000ms) as the
   round-trip deadline.
5. On a successful response, `entity.Data` is replaced with the processor's
   returned mutations and the pipeline continues to the next processor.
6. On any failure — gRPC error, timeout, member disconnect, processor reply
   with `success:false` — the engine returns
   `processor X failed: …`, the cascade aborts, the caller's handler rolls
   back `T`, and the response is `400 WORKFLOW_FAILED`. The entity is **not
   persisted in the target state**; it remains in the source state.

### Transaction-bound callbacks

Before dispatching to a compute node the engine mints a signed HMAC tx-token
`{NodeID, TxRef}` and attaches it to the outbound CloudEvent as the
`cyodatxtoken` extension attribute.

**Compute node contract:** the compute node MUST echo the received token on
every callback into cyoda-go:
- HTTP CRUD callbacks: `X-Tx-Token: <token>` request header
- gRPC EntityManage callbacks: `tx-token` metadata key

When a callback arrives carrying the token, the receiving node verifies the
HMAC and joins the transaction: if `NodeID` equals self, it calls
`Join(TxRef)` locally; otherwise it forwards the full request to the owning
node (HTTP: reverse proxy; gRPC EntityManage: B→A forward). Inside `T` the
callback sees the cascade's uncommitted writes; other readers do not.

A callback ack is **provisional** — it is not durable until the owning
transaction commits. If the processor fails or the engine rolls back `T`,
all callback writes are rolled back atomically with the rest of the cascade.

When the token is absent (empty `cyodatxtoken`), the callback runs in a
standalone transaction (`Begin`/`Commit`). This is the normal case for
`COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch=false` — the processor
receives no token because no live transaction is held during the dispatch.

The `cmd/compute-test-client` binary demonstrates the echo pattern for both
HTTP and gRPC callbacks.

### When to use

- The processor finishes in tens of milliseconds.
- The processor's result must be part of the same atomic state change as the
  transition.
- The work is idempotent or the consequence of accidental re-execution is
  tolerable (failure rolls back `T`, so partial writes never become durable).

### Pitfalls

- Long-running processors hold a database connection and a transaction for
  the entire wait. On PostgreSQL this can exhaust the connection pool under
  load — use `COMMIT_BEFORE_DISPATCH` instead.
- Both modes propagate the processor's `warnings` and `errors` arrays via
  `common.AddWarning` / `common.AddError`.

---

## 3. `ASYNC_NEW_TX`

`ASYNC_NEW_TX` is **synchronous in wall-clock terms** (the cascade blocks on
the dispatch) but its writes are isolated in a savepoint and its failure is
non-fatal. The engine code is `executeAsyncNewTx` at
`engine_processors.go:158-188`.

### Lifecycle

1. Inside the caller's transaction `T`, the engine creates a savepoint
   `S` via `txMgr.Savepoint(ctx, T)`.
2. It dispatches the processor. **The processor's returned entity mutations
   are intentionally discarded** — see the explicit `_, dispatchErr := …` at
   line 174 and the comment at line 153.
3. On failure: `RollbackToSavepoint(T, S)` undoes any writes the processor
   made via gRPC callbacks; a warning is logged at WARN level; **the pipeline
   continues** to the next processor.
4. On success: `ReleaseSavepoint(T, S)` discards the savepoint marker.

### Why mutations are discarded

`ASYNC_NEW_TX` is treated as a side-effect channel: the workflow author has
opted in to "this might fail and I don't care; don't let it abort my
transition." Applying processor result mutations on success but not on
failure would produce a confusing partial-result model. Discarding always is
simpler and matches the documented intent.

### When to use

- Notifications, metrics emission, "tell the analytics service" — work where
  it's fine to lose the result if the calculation member is down.
- Auxiliary writes (audit pings, side-channel inserts) the processor performs
  via its own gRPC callbacks, where the savepoint isolates them from the
  cascade's main writes.

### Transaction-bound callbacks in `ASYNC_NEW_TX`

The engine sends a tx-token to the compute node (same mechanism as `SYNC` —
see §2 Transaction-bound callbacks). Callbacks that echo the token join `T`
directly (via `txMgr.Join`). The engine independently scopes the entire
dispatch in a savepoint `S`: if the processor fails, `RollbackToSavepoint(T, S)`
undoes all callback writes and the pipeline continues; if the processor
succeeds, `ReleaseSavepoint(T, S)` retains those writes inside `T` (subject
to `T`'s eventual commit).

### Pitfalls

- **The savepoint protects writes the processor makes via callbacks, not the
  processor's returned mutations.** If you need the result, do not use
  `ASYNC_NEW_TX`.
- A misconfigured `ASYNC_NEW_TX` processor that fails silently every time is
  invisible to the cascade — it only surfaces in logs at `WARN`. Monitor
  `level=warn pkg=workflow processor=<name>` patterns.

---

## 4. `COMMIT_BEFORE_DISPATCH`

`COMMIT_BEFORE_DISPATCH` is the only mode that
**splits a cascade across two transactions** around a processor dispatch. It
is the recommended mode for slow external work — minutes-long callouts,
external batch jobs, third-party APIs — because it releases the database
connection (or in-memory transaction state) for the duration of the wait.
The engine code is `executeCommitBeforeDispatch` at
`engine_processors.go:201-296`.

### Concept

```
caller's TX  ──Save(pre-state)──Commit(T_pre)──┐
                                                ├── processor runs
                  ┌── Begin(T_post) ────────────┘   (outside any TX)
                  │
                  └── CompareAndSave(result, T_pre)
                                                └── cascade continues in T_post
```

`T_pre`'s commit makes the **pre-callout state durable and publicly
observable**. Concurrent readers, search queries, and audit queries see the
entity in that state. When the processor returns, the engine opens `T_post`
on the same node and applies the result via `CompareAndSave(entity, T_pre)`
— a conflict means another writer changed the entity in the window, surfaces
as a retryable `409`, and the entity remains in the pre-callout state.

### The `startNewTxOnDispatch` flag

`COMMIT_BEFORE_DISPATCH` has two sub-modes, controlled by the
`startNewTxOnDispatch` boolean on the same processor object. The validator
rejects this flag for any other execution mode.

#### `startNewTxOnDispatch = false` (default)

- Engine sequence: `Save → Commit(T_pre) → dispatch with NO TX context →
  Begin(T_post) → CompareAndSave(T_pre) → cascade continues in T_post`.
- The processor sees no transaction token. Its CRUD callbacks back to Cyoda
  cannot join a transaction; each callback runs as its own atomic operation.
- This is the mode to pick when the processor needs only its own input and
  produces only its own output. The connection is fully released for the
  duration of the dispatch.

#### `startNewTxOnDispatch = true`

- Engine sequence: `Save → Commit(T_pre) → Begin(T_post) → dispatch with
  T_post's token in context → CompareAndSave(T_pre) → cascade continues in
  T_post`.
- The processor's CRUD callbacks join `T_post`. It can read/write other
  entities transactionally.
- **Hazard — last-writer-wins on the cascade-anchor entity.** If the
  processor writes the cascade-anchor entity through its TX-callback AND
  returns mutations for the same entity in its result, the engine's
  `CompareAndSave(T_pre)` overwrites the processor's intra-TX writes (the
  result is applied last). Pick one path: either let the engine apply the
  result, or have the processor write the entity itself and return no
  mutations for it. The same warning applies in `SYNC` /
  `ASYNC_SAME_TX` when the processor has a TX callback (see workflows help).

### `If-Match` precondition

If the caller supplied an `If-Match: <txID>` header on the API request, the
engine applies it as a `CompareAndSave` against the supplied txID at the
**first segment flush** of the cascade — i.e. before `T_pre` commits and
before the processor is dispatched. This is consumed exactly once
(`consumeIfMatch` at `engine_processors.go:213`). A mismatch surfaces as
`spi.ErrConflict` → `412 Precondition Failed`, an audit
`TRANSITION_ABORTED` event is emitted with
`{reason: ENTITY_MODIFIED, expectedTxId, actualTxId}`, and `T_pre` is rolled
back — no segmentation happens, no external dispatch fires.

Subsequent `COMMIT_BEFORE_DISPATCH` segments in the same cascade fall back to
chained-CAS against the prior segment's commit-stamped txID; no further
`If-Match` is honoured.

### Failure semantics

| Failure | Outcome |
|---|---|
| Processor returns `success:false` or times out | `T_post` rolled back, entity durable in pre-callout state, `400 WORKFLOW_FAILED`, no engine retry |
| CAS conflict at apply-result boundary | `T_post` rolled back, entity durable in pre-callout state, error bubbles as `409 retryable`, client may retry |
| `If-Match` mismatch at first-segment flush | `T_pre` rolled back, no dispatch, `412 Precondition Failed`, `TRANSITION_ABORTED` audit event emitted |
| Infrastructure failure (Begin, Commit, EntityStore lookup) | wrapped with `ErrCommitBeforeDispatchInfra`, mapped to sanitized 5xx with ticket UUID — not 4xx (we don't leak driver text) |
| Calculation member disconnects mid-dispatch | `400 WORKFLOW_FAILED`, entity durable in pre-callout state |
| Engine crash between segments | entity durable in pre-callout state; in-flight cascade is gone; client must retry the same API call to re-fire the cascade from the start |

There is **no engine-side retry** and **no automatic compensation**. The
workflow author is responsible for designing the cascade so that the
pre-callout state is a sensible resting place on its own (e.g. a `PROCESSING`
state from which both `SUCCESS` and `FAILED` transitions exist).

### Idempotency requirement

A `COMMIT_BEFORE_DISPATCH` processor **must be idempotent or have an
external mechanism for detecting prior completion** (e.g. a write-once
external resource ID). Replays can fire from two distinct places:

1. **CAS conflict during continuation** — the caller's retry of the same API
   call restarts the cascade and re-dispatches the processor.
2. **Engine crash between segments** — the entity is durable in the
   pre-callout state, the in-flight orchestration is gone, the caller
   retries, the cascade re-fires from the beginning, the processor is
   re-dispatched.

The engine cannot deduplicate replays.

### Segment-boundary visibility

The pre-callout state is **publicly observable** to readers between
segments. A concurrent transaction's `Get` / `GetAll` / `Search` / `Count`
will see the entity in the pre-callout state, and a second cascade may
decide to fire criteria-driven transitions based on that observed state.
Workflow authors must treat segment-boundary states as committed states —
design state-machine criteria and external monitoring accordingly. If
invisibility of an intermediate state is required, either model it as a
parent state with sub-stages in the payload, or do not expose the entity
until a designated terminal state.

### Cluster pinning

`COMMIT_BEFORE_DISPATCH` segments are pinned to the same home node via an
HMAC-signed routing token. Cross-node continuation is out of scope: a
mid-cascade home-node crash leaves the entity durable in the pre-callout
state and surfaces as a `503` on the caller's retry until the node is back.
See [`cluster.md`](../cmd/cyoda/help/content/cluster.md).

### Batch interaction

`POST /entity/{format}` (batch create) and bulk-update endpoints degrade to
per-item processing when any item's workflow contains a
`COMMIT_BEFORE_DISPATCH` processor: items can no longer share a single
transaction because the boundary commits a per-item segment. Failures are
per-item-isolated rather than rolling back the whole batch. See
[`CONSISTENCY.md`](CONSISTENCY.md) §10.

---

## 5. Cross-Cutting Concerns

### `CompareAndSave` and txID stamping

Every entity mutation that lands through a transaction stamps the entity's
`_meta.transaction_id` with that transaction's txID at commit time.
`CompareAndSave(entity, expectedTxID)` reads the current row's stamp; on
mismatch it returns `spi.ErrConflict`. Three places use it:

- **`If-Match` request header** — handler-side optimistic concurrency for
  ordinary updates (see `crud.md`).
- **First-segment flush of `COMMIT_BEFORE_DISPATCH`** — applies the
  request's `If-Match` precondition before the segment commits.
- **Apply-result phase of `COMMIT_BEFORE_DISPATCH`** — applies the
  processor's mutations against `T_pre`'s stamped txID, catching concurrent
  writes that happened during the dispatch.

### Audit events

For every processor the engine emits, at the cascade-entry txID for
client-side correlation continuity:

- `STATE_MACHINE_PROCESSING_PAUSED` once before the processor pipeline begins.
- `STATE_PROCESS_RESULT` after each processor with `{success: bool, mode:
  string}`. `success:false` is emitted even for `ASYNC_NEW_TX` failures.
- `TRANSITION_ABORTED` on `If-Match` rejection at first-segment flush, with
  `{reason: ENTITY_MODIFIED, expectedTxId, actualTxId}`.

`STATE_PROCESS_RESULT` deliberately does **not** include the error string —
engine-wrapped error text (e.g. raw pgx messages) could leak internals to
same-tenant audit readers. The `success:false` flag is the audit-level
signal; full detail is in the operator-side slog log for the request.

### Cascade-depth limits

- **Per-state visit limit** — default 10, configurable via `WithMaxStateVisits`.
- **Absolute cascade depth limit** — 100 iterations, hard-coded safety net.
- Static cycle detection runs at workflow import time (cycles reachable only
  via automated transitions cause `400 VALIDATION_FAILED`).

A cascade that hits a limit emits `STATE_MACHINE_CANCELLED` and returns
`WORKFLOW_FAILED`.

### Engine return value: `EngineResult`

The engine returns `(FinalCtx, FinalTxID, Segmented bool)`. For
non-segmenting cascades `FinalTxID` equals the input txID and the caller's
handler commits it. For segmenting cascades `FinalTxID` is `T_post`'s ID
(the engine already committed all prior `T_pre`s); the handler commits
`T_post`. The `Segmented` flag tells the handler whether the engine already
consumed the request's `If-Match` (it has) or whether the handler should
apply post-engine CAS itself (only for non-segmenting cascades).

---

## 6. Per-Plugin Notes

The SPI primitives `Begin` / `Commit` / `Rollback` / `Savepoint` /
`RollbackToSavepoint` / `ReleaseSavepoint` / `CompareAndSave` are implemented
differently across plugins, but each implementation preserves the same
contract from the engine's point of view.

### In-memory (`plugins/memory/`)

- Transactions are in-process state; `Begin` captures a snapshot time and a
  buffer; `Commit` performs SI+FCW validation against the committed log and
  flushes the buffer under `factory.entityMu.Lock`.
- Savepoints are deep-copy snapshots of the buffer/readSet/writeSet/deletes
  maps. `RollbackToSavepoint` restores by wholesale assignment.
- `CompareAndSave` checks the committed store (not the buffer) for the txID
  stamp under read locks for TOCTOU safety.
- `COMMIT_BEFORE_DISPATCH`'s `Commit(T_pre)` is a synchronous flush; nothing
  long-running is being released.

### SQLite (`plugins/sqlite/`)

- App-layer SI+FCW over native SQLite transactions: `Commit` validates
  against an in-memory committed log under `commitMu`, then opens a real
  SQLite TX, writes the buffer to `entities` / `entity_versions`, records a
  monotonic submit time in `submit_times`, and commits the SQLite TX.
- Savepoints are app-layer snapshots, **not** real SQLite SAVEPOINTs —
  SQLite's native rollback would not restore the application-layer
  readSet/writeSet, breaking SI+FCW validation.
- `COMMIT_BEFORE_DISPATCH`'s benefit on SQLite is modest (no connection pool
  to relieve) but valid for clean transaction-boundary audit semantics.

### PostgreSQL (`plugins/postgres/`)

- Real `pgx.Tx` with `IsoLevel: RepeatableRead`. Row-level locking provides
  write-write conflict detection at DML time (SQLSTATE `40001` mapped to
  `spi.ErrConflict`).
- Read-set validation: at `Commit`, all entities in `readSet` are re-read
  inside the same TX and versions compared; mismatch aborts with
  `spi.ErrConflict`.
- Savepoints are **real** `SAVEPOINT` / `ROLLBACK TO` / `RELEASE` SQL,
  paired with an app-layer stack of readSet/writeSet snapshots so the
  isolation-validation state matches the database state.
- `COMMIT_BEFORE_DISPATCH`'s primary win is here: long external work no
  longer holds a pooled connection. The design (see
  [`docs/superpowers/specs/2026-05-04-issue-27-commit-before-dispatch-design.md`](superpowers/specs/2026-05-04-issue-27-commit-before-dispatch-design.md))
  is motivated by pool exhaustion under slow processors.
- One known divergence: `CompareAndSave` called **outside** any transaction
  (the `startNewTxOnDispatch=false` dispatch path) performs its read and its
  write in separate implicit transactions. The SPI contract is preserved
  (conflict returned on txID mismatch) but the read-then-write window is
  not protected by a row-level lock from the CAS read; concurrent
  `CompareAndSave` calls on the same entity rely on PostgreSQL's
  upsert-level locking for serialization. This is acceptable because
  conflicts are user-level retries, not system errors.

### Commercial Cassandra backend (separate repository)

The commercial Cassandra-backed storage plugin satisfies the same SPI
contract via different primitives — there is no MVCC and no global ordering.
Highlights:

- **Transactions** are a coordinator-driven 2-phase commit protocol over
  Redpanda. `Begin` writes an `ACTIVE` row to `transaction_log`; `Commit`
  transitions it `PENDING → COMMITTED` (the linearization point), then
  idempotently materialises writes with HLC-stamped `USING TIMESTAMP` for
  replay safety. Concurrent readers see writes only when the txID's log
  entry reaches `COMMITTED` and the entry's HLC ≤ their snapshot HLC.
- **Savepoints** are emulated as **child transactions**. `Savepoint`
  actually commits the current transaction, then begins a child whose
  `parent_tx_id` points to the now-committed parent; `RollbackToSavepoint`
  aborts the child and starts a sibling. The reader visibility filter walks
  the ancestor chain so writes from any committed ancestor are visible to
  the current transaction. **Workflow authors should be aware: an
  `ASYNC_NEW_TX` processor's preceding cascade work is durably committed on
  this backend before the processor runs**, where on the in-tree plugins it
  is not. The SPI contract (rollback-to-savepoint restores prior state for
  this transaction) holds, but observers see intermediate states that the
  in-tree plugins keep buffered.
- **CompareAndSave** is optimistic locking on `(version, tx_id)` per
  partition. Conflicts are detected by per-entity shard owners which apply
  SSI checks against an in-memory committed-write log; not Cassandra LWT.
- **COMMIT_BEFORE_DISPATCH** maps naturally: `Commit(T_pre)` runs the full
  2-phase commit, the pre-callout state becomes durably observable, and the
  apply-result CAS uses the same per-entity conflict detection as any other
  write. There is no pool-pressure motivation, but the segmentation gives
  the cascade clean recovery points.
- **Operational limits worth surfacing to workflow authors on this backend:**
  entities updated thousands of times per day are an anti-pattern (oversized
  partitions); SSI is entity-level only (no phantom protection for
  predicate-based reads — same as the in-tree plugins, but documented
  explicitly).

---

## 7. Choosing a Mode

```
Is the processor going to take more than ~1 second?
├── No  → SYNC (default, simplest).
└── Yes → Does the cascade need the processor's result?
         ├── No, it's fire-and-forget → ASYNC_NEW_TX.
         └── Yes →
             Does the processor need to read/write OTHER entities
             transactionally as part of its work?
             ├── No  → COMMIT_BEFORE_DISPATCH (startNewTxOnDispatch=false).
             └── Yes → COMMIT_BEFORE_DISPATCH (startNewTxOnDispatch=true)
                       AND ensure the processor either writes the
                       cascade-anchor itself OR returns mutations for it,
                       never both.
```

Avoid `ASYNC_SAME_TX` until/unless its semantics diverge from `SYNC` — it is
currently a labelling-only variant.

---

## 8. Validation and Error Mapping

- The workflow import validator enforces that `startNewTxOnDispatch=true`
  only appears on `COMMIT_BEFORE_DISPATCH` processors; otherwise import
  fails with `400 VALIDATION_FAILED`.
- An unknown `executionMode` string falls through the dispatch switch to
  the `SYNC` / `ASYNC_SAME_TX` branch — it is not rejected at import time.
  Treat any value not in the four listed above as undefined behaviour and
  do not rely on it.
- `classifyWorkflowError` maps engine outputs to HTTP:
  - `ErrCommitBeforeDispatchInfra` → sanitized 5xx with ticket UUID
  - `ErrTransitionNotFound` → 404 `TRANSITION_NOT_FOUND`
  - `spi.ErrConflict` from CAS → 409 retryable (or 412 if `If-Match`)
  - everything else (processor `success:false`, criterion mismatches, timeouts) → 400 `WORKFLOW_FAILED`

---

## 9. Source Index

| Concern | File |
|---|---|
| Dispatch switch | `internal/domain/workflow/engine_processors.go:42` |
| `SYNC` / `ASYNC_SAME_TX` | `engine_processors.go:139` |
| `ASYNC_NEW_TX` | `engine_processors.go:158` |
| `COMMIT_BEFORE_DISPATCH` | `engine_processors.go:201` |
| Segment flush + commit | `engine_processors.go:314` |
| `If-Match` plumbing | `internal/domain/workflow/ifmatch.go` |
| `TRANSITION_ABORTED` audit | `internal/domain/workflow/transition_aborted.go` |
| gRPC processor dispatch | `internal/grpc/dispatch.go:43` |
| Tx-token mint + attach to CloudEvent | `internal/grpc/dispatch.go` (token injected before dispatch) |
| HTTP callback routing (X-Tx-Token) | `internal/cluster/proxy/` |
| gRPC callback routing (tx-token metadata) | `internal/grpc/` (txRouteInterceptor) |
| Compute-test-client echo example | `cmd/compute-test-client/callback.go`, `grpc_callback.go` |
| Memory plugin txmanager | `plugins/memory/txmanager.go` |
| SQLite plugin txmanager | `plugins/sqlite/txmanager.go` |
| PostgreSQL plugin txmanager | `plugins/postgres/transaction_manager.go` |
| Workflow schema (help topic) | `cmd/cyoda/help/content/workflows.md` |
