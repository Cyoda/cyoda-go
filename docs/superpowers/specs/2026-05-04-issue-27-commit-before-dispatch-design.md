# Issue #27 — `COMMIT_BEFORE_DISPATCH` processor execution mode

**Status:** Design accepted (revision 2 — post-review) — pending plan
**Issue:** [#27](https://github.com/Cyoda-platform/cyoda-go/issues/27)
**Milestone:** v0.7.0 (release branch `release/v0.7.0`, pre-tag)
**Date:** 2026-05-04
**Author:** Paul Schleger (with Claude)

---

## 1. Problem

The cascade engine runs workflow processors in three execution modes today: `SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX` (`api/generated.go:439-447`, `internal/domain/workflow/engine.go:533-638`). All three execute the processor while the cascade's transaction is open and the PG connection is held — `SYNC`/`ASYNC_SAME_TX` inline in the parent transaction, `ASYNC_NEW_TX` in a savepoint nested inside it (postgres) or in a child TX after committing the parent (cassandra — see [§9](#9-out-of-scope-finding-cassandra-vs-postgres-async_new_tx-divergence) below).

Under processor-heavy workloads with deep cascades and slow external compute (ML inference, third-party APIs), this couples external latency directly to PG connection-pool capacity. A 5-step cascade with 2-second processors holds a connection for ~10 seconds; ten concurrent such cascades consume 40% of a 25-connection pool for that whole window, blocking other tenants.

The predecessor's design analysis names this fault line 3 of `fault-analysis.md` §4 (located at `/Users/paul/go-projects/cyoda-light/cyoda-light-go/docs/design/fault-analysis.md`; not currently mirrored in this repo, referenced for context only — see §11 of that doc for the design history).

Workflow authors who don't need full cascade atomicity have no way to opt out. Their only workaround is to split the cascade into separate API calls and have the client drive a manual loopback transition after each external compute step — possible but tedious.

## 2. What we're adding

A fourth `executionMode` value: `"COMMIT_BEFORE_DISPATCH"`. When a processor declares this mode, the engine commits the current transaction before dispatching the processor and opens a new one on return. The PG connection is released during the external compute window (when `startNewTxOnDispatch=false`). The processor's returned result (entity mutations) is applied to the entity in the new transaction via `CompareAndSave`, using the txID of the prior commit as the expected version. The cascade then continues normally.

This is the server-side automation of a sequence the client could already do today (`Update` → wait for response → `ManualTransition` with the result). The engine claims **no special consistency rights** beyond what a client doing the same loopback by hand would have. Conflicts surface through the existing `ErrConflict` / HTTP `409` retryable path. This framing — "engine just does what a client could do, in fewer round-trips" — is the design's load-bearing principle and the reason no new SPI surface, no new entity-meta semantics, no read filter, no write guard, no reaper, and no cascade ledger are needed.

An optional `startNewTxOnDispatch: bool` flag (default `false`) selects between two sub-variants:

- **`false` (default):** commit parent → dispatch processor with no transaction context → on return, open fresh `TX_post` → CAS-apply result → continue cascade. Connection is released entirely during processor execution. Solves fault-line-3.
- **`true`:** commit parent → open `TX_post` → dispatch processor with `TX_post`'s transaction token → processor may do transactional work during its execution → on return, apply result in `TX_post` → commit `TX_post`. Connection is held in `TX_post` (a fresh connection) during processor execution. Does **not** solve fault-line-3, but isolates the processor's work in its own atomic unit decoupled from the parent cascade's pre-commit work. The semantic is identical to a client-driven manual loopback transition.

## 3. Mechanism (canonical sequence)

For a transition `T: S_pre → S_post` whose processor `P` has `executionMode: "COMMIT_BEFORE_DISPATCH"`:

Let `TX_pre` be the parent transaction the cascade was running in when it reached `T`. Let `T_pre` be the txID of `TX_pre` (assigned at `Begin` and stamped onto each entity row at `Save` time inside `TX_pre`, becoming durable on `TX_pre.Commit` — verified at `plugins/postgres/entity_store.go:46-48`). Let `TX_post` be the transaction in which the result is applied and the cascade continues; let `T_post` be its txID.

```
1. Cascade in TX_pre reaches transition T, processor P
2. Engine sets entity.Meta.State = S_pre on the in-memory struct
3. Engine calls EntityStore.Save(ctx, entity) inside TX_pre
   [flushes the in-memory cascade state into TX_pre's buffer]
4. Engine calls TxMgr.Commit(ctx, TX_pre)
   [TX_pre commits; entity row stamped with T_pre; PG connection released
    if no other active TX on this goroutine]

5. Branch on startNewTxOnDispatch:

   (a) startNewTxOnDispatch == false (default):
       5a. Engine dispatches P(entity) with no tx token in ctx
           [P executes externally; no transaction is open anywhere]
       6a. P returns result R (or error E)
       7a. Engine calls TxMgr.Begin(ctx) → returns TX_post
           [fresh PG connection acquired]

   (b) startNewTxOnDispatch == true:
       5b. Engine calls TxMgr.Begin(ctx) → returns TX_post
           [PG connection acquired for TX_post; held throughout dispatch]
       6b. Engine dispatches P(entity) with TX_post's token in ctx
           [P may perform transactional work via TX_post during execution;
            see §10 caveat on processor double-writing the cascade-anchor
            entity]
       7b. P returns result R (or error E)

8. Engine, inside TX_post: applies R to its in-memory entity struct,
   then calls EntityStore.CompareAndSave(ctx, entity, expectedTxID = T_pre)
   • CAS conflict: bubbles ErrConflict → 409 retryable; cascade aborts
     at this segment; TX_post is rolled back; caller retries.
   • CAS success: entity is now in the post-callout state
     (S_post, or whatever R encodes).
9. Cascade continues from the post-callout state inside TX_post until
   either the next COMMIT_BEFORE_DISPATCH segment boundary or final commit.
```

After step 9, the cascade either reaches another `COMMIT_BEFORE_DISPATCH` boundary (loop back to step 2 with `TX_pre := TX_post`, `T_pre := T_post`) or reaches its terminal state. The terminal-segment flush is the engine's `EntityStore.Save(entity)` followed by `TxMgr.Commit` — Save (not chained CAS) because no segment boundary has run between the prior CAS and this terminal write; the existing **first-committer-wins** mechanism (postgres uses `RepeatableRead` isolation at `plugins/postgres/transaction_manager.go:68` plus application-layer read-set re-validation in `Commit` at `transaction_manager.go:128-153`; other plugins implement equivalent FCW protection) captures any racing committer at commit time. (For a single-segment cascade with no `COMMIT_BEFORE_DISPATCH` processors, the entire cascade is "the terminal segment": engine does one `Save` + one `Commit`, observably identical to today's handler-driven behaviour.)

If the processor itself returns an error E (not a CAS conflict), the engine surfaces it the same way it surfaces a `SYNC` processor failure today: cascade aborts, `TX_post` rolls back. There is no automatic compensation routing — the workflow author's options are the same as for any processor failure (criteria-based branching on subsequent transitions, or letting the caller retry the original API call which restarts the cascade from the beginning).

The CAS argument source is `T_pre`, the txID stamped on the row's `_meta.transaction_id` at the prior segment's commit (verified against `plugins/postgres/entity_store.go:46,151`). **The CAS check has identical strength in both `startNewTxOnDispatch` branches** — both compare against the committed-store's stamped txID. The branches differ on a separate axis: in the `=true` branch, if the processor writes the cascade-anchor entity itself via the `TX_post` token, those writes are buffered in `TX_post` and the engine's later apply-result `CompareAndSave` overwrites them (last-writer-wins inside the TX buffer). This intra-`TX_post`-buffer collision is **not** a CAS-strength issue; it is a separate ordering hazard governed by the existing best-practice "a processor must not save the entity it is processing for" (see §10.3). The `=false` branch cannot exhibit this hazard because no transaction is open while the processor runs.

(In postgres, "PG connection released" between segments is the operationally significant property. Memory, sqlite, and cassandra plugins manage their own connection / session model; the connection-pool-relief framing is postgres-specific. Cross-plugin, the segment boundary releases whatever transactional resource the plugin holds open during a TX.)

## 4. Engine TX ownership inversion

Today, `service.UpdateEntity` (`internal/domain/entity/service.go:867-995`) opens TX1 around the entire `engine.ManualTransition()` call. The engine never commits or begins; it never calls `EntityStore.Save` either; it just mutates an in-memory `*spi.Entity` and returns. The handler then calls `Save`/`CompareAndSave` once to flush the final state and commits.

With `COMMIT_BEFORE_DISPATCH`, the engine must own three things the handler used to own:

1. **TX boundaries** — `Begin` and `Commit` around each cascade segment. Cascades with no `COMMIT_BEFORE_DISPATCH` processors run as a single segment; observable behavior is identical to today. Cascades with one or more `COMMIT_BEFORE_DISPATCH` processors are segmented at each such processor.
2. **Per-segment SPI writes** — `EntityStore.Save(entity)` (or `CompareAndSave` for the `If-Match` first-segment case, see below) is called by the engine before each segment commit, flushing the in-memory cascade state into the TX's buffer so the commit persists it. The engine uses **per-entity `Save` calls**, not `SaveAll`, so partial-save semantics of `SaveAll` do not apply. The handler's single end-of-cascade `Save` is removed; what was a single write per cascade becomes one write per segment. (For single-segment cascades, that's still a single write — degenerate case.)
3. **Audit event placement** — events tied to a transition are recorded inside the TX whose commit they should be durable with. See §8.

This applies uniformly to **all three engine cascade entry-points**: `engine.Execute()` (creation cascade triggered by `CreateEntity`), `engine.ManualTransition()` (manual transition triggered by `UpdateEntity` with a transition name), and `engine.Loopback()` (engine.go:228, re-runs the automated cascade against the entity's current state — a request to "see if any automated transitions match now"). All three drive `cascadeAutomated`, which evaluates whatever automated transitions are user-configured for the current state. There is no engine-defined "loopback transition" — Loopback is just the third entry-point, and the transitions it fires are user-configured like any other. The segmentation logic lives in the cascade driver below the entry-points (`cascadeAutomated` and the per-processor `executeProcessors` switch), not above, so all three entry-points get segmenting for free. Implementers must not edit only `Execute` / `ManualTransition` and miss `Loopback`.

### 4.1 `If-Match` propagation across segments

The HTTP `If-Match` header (and `input.IfMatch` field at `service.go:978-992`) supplies the caller's expected txID. Today this is enforced once at the handler's single `CompareAndSave` at end-of-cascade. With segmenting, it moves to the **first segment's** entity-flush:

- The first segment's entity-flush is `CompareAndSave(entity, expectedTxID = input.IfMatch)` instead of `Save`. For cascades **containing a `COMMIT_BEFORE_DISPATCH` processor**, this is strictly *earlier* enforcement than today — a stale `If-Match` aborts before any segment commits or any external dispatch fires. For cascades **without** a `COMMIT_BEFORE_DISPATCH` processor (single-segment), the first segment is the whole cascade and observable timing is identical to today.
- Subsequent segments use chained CAS: each segment's CAS-on-continuation expects the prior segment's commit-time txID (`T_pre` of that segment), as described in §3.

This keeps `If-Match` as a single-shot client-supplied check at cascade entry, while internal segmentation uses engine-managed CAS.

### 4.2 Per-segment version history

Today a cascade produces one `EntityVersion` row in the version-history (`spi.EntityStore.GetVersionHistory`). With segmenting, a cascade produces one `EntityVersion` per segment. Auditors and `GET /api/entity/{id}/changes` consumers see more rows for a `COMMIT_BEFORE_DISPATCH` cascade. This is observable behavior; it is documented in `cmd/cyoda/help/content/workflows.md` and `docs/ARCHITECTURE.md`.

### 4.3 Cluster routing

The cascade is driven server-side by the same goroutine that holds the client's HTTP request open. That goroutine lives on the node that received the API call (call it node A). With segmenting:

- `TX_post.Begin` is invoked on **the same node A**, because the goroutine never moved. No new cluster-routing logic, no cross-node hand-off, no HMAC token churn.
- The dispatch step (between segments) goes through existing cluster dispatch routing — the processor may run on any compute node tagged for it — but the gRPC response flows back through node A's existing stream.
- The HTTP response carries `T_post` (the **final segment's** txID), the same txID the client would have observed if they had performed a manual loopback themselves at end-of-cascade. Intermediate segment txIDs are observable only via `GetVersionHistory` / audit reads.
- New failure mode: node A dies between `TX_pre.Commit` and `TX_post.Begin` (or between `Begin` and any further work). The entity is durable in the pre-callout state; the HTTP socket is dead; the cascade is stranded. This is structurally the same as a client crashing mid-loopback: the persistent state is durable, the in-flight orchestration is lost, recovery is the application's concern. See §10 for the equivalence statement and §12 for the explicit out-of-scope decision.

### 4.4 SaveAll and multi-entity cascades

Cascades may mutate multiple entities (cross-entity CRUD via processors). When a `COMMIT_BEFORE_DISPATCH` segment touches multiple entities, the segment's `Commit` flushes all of them as one atomic unit; the segment's CAS-on-continuation guards **only the cascade-anchor entity** (the entity the cascade was started on).

Other entities mutated during the segment get FCW protection from the **read-set captured during `TX_post`'s execution** (`plugins/postgres/transaction_manager.go:128-153` re-validates each read entity's `transaction_id` at commit). The implication for workflow authors using `COMMIT_BEFORE_DISPATCH`: a secondary entity that is **written without first being read** inside `TX_post` has *no* FCW guard against external mutation between the secondary entity's last read (in `TX_pre` or earlier) and `TX_post.Commit`. Write-without-read on a secondary entity inside a `COMMIT_BEFORE_DISPATCH` segment is the same hazard as write-without-read in any other transaction; it is not introduced by this design. Workflow authors should ensure cross-entity work follows read-then-write order if they need FCW protection across segments.

This is the same per-entity-CAS scope as the client-driven manual loopback: `If-Match` covers the anchor entity only.

## 5. SPI changes

**One additive field.** `spi.ProcessorConfig` (`spi/types.go:148-154`) gains `StartNewTxOnDispatch *bool` (omitempty). No other SPI surface changes. Bumps SPI from v0.6.1 to v0.7.0 — additive, backward-compatible (`nil` == default == false).

Why the bump is needed: the workflow JSON arriving via the OpenAPI layer carries `startNewTxOnDispatch`; the handler converts the DTO into `spi.WorkflowDefinition` before persisting via `WorkflowStore.Save`. Today's `spi.ProcessorConfig` has no field for this flag, so a JSON unmarshal drops it on round-trip. Adding the field preserves it through storage. No flexible `map[string]any` exists on `spi.ProcessorConfig` to carry it as a side-channel; even if it did, ad-hoc maps are not the right shape for a typed semantic flag.

The new mode is implementable entirely with existing SPI primitives apart from this one struct field:

- `TransactionManager.Begin(ctx) → (txID, ctx, error)` — starts new TX (`spi/transaction.go:23`)
- `TransactionManager.Commit(ctx, txID) error` — commits TX (`spi/transaction.go:25`); the txID is the input, the row stamping happens at commit time, no return value
- `EntityStore.Save(ctx, entity) (int64, error)` — buffer write
- `EntityStore.CompareAndSave(ctx, entity, expectedTxID) (int64, error)` — CAS write (`spi/persistence.go:27`)
- `spi.WithTransaction(ctx, state)` / `spi.GetTransaction(ctx)` — TX context propagation (`spi/txcontext.go:101-106`)

Postgres, sqlite, memory, and cassandra plugins persist `WorkflowDefinition` as JSON; once they bump their SPI dependency to v0.7.0 the new field flows through their JSON marshalling with no plugin code changes. **The cassandra plugin needs no per-feature code work — only a routine SPI dependency bump.**

## 6. Workflow definition schema

Extend the `executionMode` enum on `ExternalizedProcessorDefinitionDto`:

```yaml
executionMode:
  type: string
  enum:
    - SYNC
    - ASYNC_SAME_TX
    - ASYNC_NEW_TX
    - COMMIT_BEFORE_DISPATCH   # new
startNewTxOnDispatch:
  type: boolean
  default: false
  description: |
    Only meaningful when executionMode is COMMIT_BEFORE_DISPATCH.
    If true, a new transaction is opened before dispatching the processor
    so the processor can perform transactional work during its execution.
    Connection is held in that new transaction during dispatch.
    If false (default), the processor runs with no transaction context;
    the connection is released entirely during processor execution.
```

`api/generated.go` regenerated from the updated OpenAPI source. JSON Schema for workflow validation updated correspondingly.

### 6.1 Workflow validation at registration

At workflow registration (`internal/domain/workflow/validate.go`), the validator MUST reject:

- `startNewTxOnDispatch: true` combined with any `executionMode` other than `COMMIT_BEFORE_DISPATCH`. This is a configuration error and must be a hard reject, not a warning, so misconfigurations surface at registration time rather than at first cascade execution.

Reachability and compensation-path validation are explicitly **NOT** added — `COMMIT_BEFORE_DISPATCH` does not require them. The workflow author's failure-handling responsibility is the same as for any processor failure.

## 7. HTTP API and error surface

No new HTTP endpoints, no new request/response shapes, no new error codes. Cascades using `COMMIT_BEFORE_DISPATCH` look identical to clients:

- The HTTP request stays open until the full cascade completes (server-side: `TX_pre` commit, dispatch, `TX_post` begin, CAS-apply, continue, …, final commit). The PG connection is **not** held during external dispatch in the `false` branch; the HTTP socket still is in both branches.
- CAS conflict during continuation surfaces as the existing `ErrConflict` → `409` with `retryable: true`. Caller's retry of the same API call restarts the cascade from the beginning; the processor will be re-dispatched. Idempotency is the workflow author's responsibility (see §10).
- Processor failure during dispatch surfaces the same way as a `SYNC` processor failure today.
- The response's transaction-ID field carries the **final segment's** txID (see §4.3).

There is **no `202 Accepted` / async-poll variant in this scope**. Synchronous client wait is the only client UX.

## 8. Audit trail

Each segment of a `COMMIT_BEFORE_DISPATCH` cascade commits its own audit events at TX commit time. Operators get durable audit for completed segments; today a cascade failure rolls back all audit events for that cascade, leaving operators blind.

The cascade engine's audit emission must be split at segment boundaries. The taxonomy:

- **Existing event preserved.** `SMEventStateProcessResult` (currently emitted at `engine.go:574` after every processor dispatch with `success/mode/error` data) **continues to fire** for `COMMIT_BEFORE_DISPATCH` processors, recorded into `TX_post` after the result is applied. No semantic change for this event; readers who consume it today see the same shape.

**Audit-event txID labelling decision.** Today every cascade event passed to the audit store carries the cascade-entry `txID` as its `transaction_id` (`engine.go:543, 479, 471`). After segmentation, the engine continues to use the **cascade-entry txID as the audit `transaction_id` label across all events of one cascade**, even when the events are physically durable in different segment transactions. Rationale: the `transaction_id` field on audit events identifies a logical cascade for client-facing correlation (used by `GET /api/audit/entity/{id}/workflow/{transactionId}`), not a physical transaction boundary. This preserves the existing `transactionId`-keyed audit-query API.

**No parent→child transaction trail today.** The SPI does not carry a `parent_tx_id` / `cascade_root_tx_id` field on either audit events or `EntityVersion` rows. Operators or consumers who need to correlate a specific event back to the segment transaction it was physically committed in must use proxies: the entity's per-version `_meta.transaction_id` snapshot at the event's `event_time`, plus the segment-bracketing `SMEventProcessingPaused` / `SMEventStateProcessResult` markers. Adding a real parent-segment trail would be an SPI surface change (new field on audit events and possibly on `EntityVersion`, plus plugin schema migrations across postgres/sqlite/cassandra) — **out of scope for this issue**, tracked separately if observability ergonomics warrant.
- **Reused TX_pre boundary event.** `SMEventProcessingPaused` (existing in `spi/types.go:173`, value `"PAUSE_FOR_PROCESSING"`) is recorded into `TX_pre` at the point the engine flushes the pre-callout entity state. Durable on `TX_pre.Commit`. The event name accurately describes the situation — the cascade is paused awaiting external processing — so reusing it requires no new SPI surface. An engine crash between commit and dispatch leaves the audit trail showing exactly this boundary.
- **TX_post counterpart is the existing `SMEventStateProcessResult`** (already preserved per the bullet above). No new event type needed in TX_post.
- **Other audit events** (state-machine transitions, criterion evaluations) follow the existing convention and land in whichever TX the engine is currently operating in — no behavioural change.

A reader scanning audit history can detect a stranded segment programmatically: `SMEventProcessingPaused` exists, no matching `SMEventStateProcessResult` follows, and `last_modified_date` is older than some operator-chosen threshold. This is an observability improvement, not a recovery mechanism — recovery remains out of scope (§12).

**No SPI bump required.** Both event types already exist in the SPI (`spi/types.go:162-175`).

## 9. Out-of-scope finding: cassandra-vs-postgres `ASYNC_NEW_TX` divergence

Verified during review: cassandra plugin's `Savepoint` commits the parent transaction and starts a fresh child (`cyoda-go-cassandra/internal/integration/tx_manager_test.go:192-218`, `TestTxManager_Savepoint_CommitsParent`). Postgres `Savepoint` issues a PG `SAVEPOINT` nested in the parent. These give materially different durability properties for `ASYNC_NEW_TX`-mode side-effect work. **Out of scope here**; to be filed separately. No changes to `ASYNC_NEW_TX` are made by this design.

### 9.1 Mixed-mode processor list on a single transition

A transition may declare a `processors: [...]` list with multiple processors that have **different `executionMode` values**. This subsection covers that case — multiple processors on one transition. The cross-transition case (different transitions in a cascade chain, each with their own processor list) is implicitly covered by the segmentation model in §3: each `COMMIT_BEFORE_DISPATCH` processor is a segment boundary regardless of which transition it lives on, and `ASYNC_NEW_TX` runs inside whichever segment its transition belongs to.

For a mixed pipeline on a single transition, the interaction between `ASYNC_NEW_TX` (savepoint on postgres, child TX on cassandra per §9) and `COMMIT_BEFORE_DISPATCH` (segment commit) is well-defined by sequencing:

- **`[ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH]`:** the `ASYNC_NEW_TX` processor runs first inside `TX_pre` (savepoint released or committed-then-resumed depending on plugin per §9). Whatever durable state results from `ASYNC_NEW_TX` is part of `TX_pre`'s commit at the segment boundary. Then `COMMIT_BEFORE_DISPATCH` runs as described in §3.
- **`[COMMIT_BEFORE_DISPATCH, ASYNC_NEW_TX]`:** the `COMMIT_BEFORE_DISPATCH` processor runs first; its segment commits as `TX_pre`. The cascade re-enters in `TX_post`. The `ASYNC_NEW_TX` processor then runs *inside `TX_post`* (savepoint there). The `ASYNC_NEW_TX` plugin-divergence (§9) applies to whichever plugin is in use; this design does not paper over it.

Workflow authors should not assume `ASYNC_NEW_TX` work is atomic with `COMMIT_BEFORE_DISPATCH` work in the same processor list — segmentation cuts the atomic scope at the `COMMIT_BEFORE_DISPATCH` boundary. The §9 cross-plugin divergence on `ASYNC_NEW_TX` durability under parent rollback inherits unchanged into mixed pipelines.

## 10. Workflow author requirements

### 10.1 Idempotency under retry

A `COMMIT_BEFORE_DISPATCH` processor must be **idempotent or have an external mechanism for detecting prior completion** (e.g., a write-once external resource ID).

Replays can fire from two distinct places:
- **CAS conflict during continuation**: the caller's retry of the same API call restarts the cascade and re-dispatches the processor.
- **Engine crash between segments**: the entity is durable in the pre-callout state; the in-flight orchestration is gone. The caller retries; the cascade re-fires from the beginning; the processor is re-dispatched.

The two cases are not perfectly equivalent to "client crashed mid-loopback." A client driving its own loopback knows it crashed and owns the retry decision; with `COMMIT_BEFORE_DISPATCH`, an engine crash mid-cascade leaves the **server** silently holding a partially-progressed entity, and the client cannot distinguish "engine crashed mid-cascade" from "request timed out, processor never ran." Workflow authors should not assume client-driven idempotency disciplines automatically transfer; they should design the processor so that re-dispatch with the same input is safe regardless of whether the prior dispatch completed.

### 10.2 Visibility of segment-boundary states

States on a segment boundary (the pre-callout state of a `COMMIT_BEFORE_DISPATCH` processor) are **publicly observable** to readers between segments. A concurrent transaction's `Get`/`GetAll`/`Search`/`Count` will see the entity in the pre-callout state. A second cascade may decide to fire criteria-driven transitions based on that observed state.

This is a behavior change for any reader currently depending on cascade atomicity for visibility. Workflow authors using `COMMIT_BEFORE_DISPATCH` must treat segment-boundary states as committed states — design state-machine criteria, transition guards, and external monitoring accordingly. If invisibility of an intermediate state is required, model it as a workflow-level `DRAFT` parent state with sub-stages in payload, or do not expose the entity until a designated terminal state.

### 10.3 No double-writing the cascade-anchor entity

This is **not new** to `COMMIT_BEFORE_DISPATCH`; it applies to any processor in any TX-bearing mode (`SYNC`, `ASYNC_SAME_TX`, `COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch=true`, and any client-driven manual loopback). Surfaced here because it is the relevant caveat for the `=true` branch's CAS-strength asymmetry described in §3.

A processor must not save the entity it is processing for via in-flight TX callbacks **and** also return mutations for that same entity in its result. The engine will apply the returned mutations after the processor returns, overwriting the buffer entry the processor wrote during its execution (last-writer-wins inside the TX buffer). Pick one path: either let the engine apply the result, or have the processor write the entity itself and return no mutations for it.

If existing best-practice docs do not state this rule explicitly, the plan adds it. See §15 for the doc-edit list.

### 10.4 Multi-entity cascades

When a `COMMIT_BEFORE_DISPATCH` segment mutates multiple entities, only the cascade-anchor entity is CAS-guarded at segment continuation (§4.4). Secondary entities get FCW protection only if they are **read inside the same segment** (entering the segment's read-set, which is re-validated at commit by application-layer FCW — postgres `RepeatableRead` + read-set check, not SSI). Write-without-read inside a segment leaves the secondary entity unprotected against external mutation; workflow authors must read-then-write if they need cross-entity FCW within a segment.

## 11. Risks

**Engine TX-ownership refactor (§4) is the largest change and the riskiest.** The engine becomes responsible for TX lifecycle, per-segment SPI writes, and audit-event placement in cases where the handler used to be. Bugs here can manifest as connection leaks, double commits, lost cascades, missing audit events, or incorrect `If-Match` enforcement. Mitigation: the change is gated behind the new mode in observable behavior — single-segment cascades preserve today's outcomes — but the code path runs for every cascade. Coverage for the single-segment degenerate case must be maintained as a regression bound.

**Idempotency is now a workflow-author responsibility** (§10.1). Authors who use `COMMIT_BEFORE_DISPATCH` with non-idempotent processors will see double-execution under retry. Mitigation: prominent documentation; a runtime warning at workflow registration when a `COMMIT_BEFORE_DISPATCH` processor is detected (advisory only, not a hard reject — workflows that opt in have done so deliberately).

**Observable intermediate states** are a behavior change for readers depending on cascade atomicity for visibility (§10.2). Mitigation: documented as a property of the new mode; workflow authors who need invisibility model that explicitly through state-machine design.

**Hot-entity livelock with cost amplification** is a new attractor under load. Multiple concurrent cascades operating on the same entity all expect `T_pre` from the prior segment's commit; whichever cascade commits its segment first invalidates every other in-flight cascade's CAS expectation. Under sustained high concurrency on a small set of hot entities, all but one cascade per round abort with `409 retryable` and restart from the beginning. Unlike today's serialization-conflict retry pattern (where retry costs are bounded by re-running local logic against postgres), every retry under `COMMIT_BEFORE_DISPATCH` **re-dispatches every prior `COMMIT_BEFORE_DISPATCH` processor in the cascade**, re-incurring external compute cost — ML inference billing, third-party API quotas, latency to external services. Cost amplification scales with `cascade_depth × external_dispatch_cost × livelock_round_count`.

**Mitigations and ownership.** None of the following are engine-side; the engine has no built-in safeguard for this. Cost-amplification under hot-entity livelock is a workflow-design and deployment concern.

| Mitigation | Owner | Where applied |
|---|---|---|
| Per-anchor concurrency limits, queue-front-end throttling | Application / operator running cyoda-go | Outside the engine — workload shaping at the API ingress, message broker, or application middleware |
| Jittered exponential backoff on `409 retryable` | Client of the cyoda-go HTTP API | Client retry policy. Already a documented expectation in `docs/CONSISTENCY.md` §10 for ordinary `ErrConflict` retries |
| Avoid placing `COMMIT_BEFORE_DISPATCH` on transitions that fan into a single hot-entity write target | Workflow author | Workflow definition design |

Engine-side options were considered and rejected: a per-anchor concurrency lock would add new shared state and slow normal workloads; a server-side retry budget would change the existing client-driven retry contract; a registration-time validator warning that flags hot-entity fan-in is hard to define statically (the validator does not know which entities are "hot"). Test coverage of the livelock attractor itself is in §16 case 8; the cost-amplification cannot be asserted in tests, only documented as a deployment concern.

## 12. Out of scope for this issue

- **Stranded-entity recovery (reaper).** Engine crashes between segments leave the entity durable in `S_pre`; recovery is the application's concern. The asymmetry vs client-driven loopback described in §10.1 is acknowledged but not resolved here.
- **Async/202+poll API mode** for long-running cascades. v1 is synchronous client wait.
- **Per-state `externalWritable: false` policy** for guaranteeing cascade-wins semantics on race. CAS conflict surfaces normally; if a workflow needs cascade-wins behavior, file separately.
- **Workflow validation for compensation paths.** `COMMIT_BEFORE_DISPATCH` reuses existing processor-failure semantics; no new validation.
- **Any rename or deprecation of existing `executionMode` values.**
- **Cross-node continuation of a cascade after a segment commit.** §4.3 nails `TX_post` to the same node as `TX_pre`. Cross-node continuation would broaden the design unnecessarily for this issue.
- **Cassandra-vs-postgres `ASYNC_NEW_TX` semantic divergence (§9).** To be filed separately.

## 13. Decision log

The following alternatives were considered and rejected during brainstorming and design review. Recording them so future readers don't relitigate.

- **`_meta.pending` flag with read-filter, write-guard, bypass-context, and stranded-entity reaper.** Rejected: overloaded three concerns (visibility, liveness, concurrency) into one flag; visibility is a workflow-modelling concern, liveness can be left to the application, concurrency is solved by `CompareAndSave`. The engine claims no special consistency rights.
- **Cascade ledger (`cascade_in_flight` table).** Rejected: introduced for stranded-entity detection, but stranded entities are equivalent in shape to a client crash mid-loopback (with the asymmetry noted in §10.1) — out of scope for the engine.
- **Per-state `externalWritable: false` policy.** Rejected: useful but separable; ships only if a workflow asks for it.
- **`commitOnCallout: bool` flag on existing `executionMode` values.** Rejected in favor of a new enum value (better OpenAPI compatibility, named declaratively).
- **Renaming `ASYNC_NEW_TX` → savepoint mode and reusing the name for the new semantic.** Rejected: too much migration tax against minimal naming-honesty gain; also flips Cloud-parity config-token meaning.
- **SPI bump for new `EntityMeta.Pending` field, `ErrEntityPending` sentinel, `WithPendingAccess` context scope.** Rejected as a knock-on of dropping the `pending` flag; not needed.
- **Side-channel storage for `startNewTxOnDispatch` (e.g. KV table) to avoid SPI bump.** Rejected: ad-hoc storage doesn't match how other workflow-config knobs persist, requires migration, and the SPI bump for one additive struct field is backward-compatible. Path A (SPI v0.7.0 bump) chosen.
- **Dropping `startNewTxOnDispatch` from v1 over LWW concerns.** Rejected after design review: the LWW between processor's intra-TX writes and the engine's apply-result is the same hazard that already exists in `SYNC` and any client-driven manual loopback whose processor does TX-callback writes. It is governed by an existing best-practice ("a processor must not save the entity it is processing for"), not by a new engine-level guarantee. Documenting the caveat (§10.3) is sufficient.

## 14. Changes by file (high-level)

- **`internal/domain/workflow/engine.go`** — engine takes `txMgr`; cascade loop becomes segment-aware; engine performs per-segment `EntityStore.Save` flush; engine performs `Commit`/`Begin` around segment boundaries; `executeProcessors` gains a new branch for `COMMIT_BEFORE_DISPATCH` covering both `startNewTxOnDispatch` values; emit existing `SMEventProcessingPaused` into `TX_pre` and existing `SMEventStateProcessResult` into `TX_post` (no new event types).
- **`internal/domain/entity/service.go`** — `UpdateEntity`, `CreateEntity`, batch variants no longer wrap the engine call in a transaction nor do the end-of-cascade `Save`; they hand `txMgr` and `If-Match` to the engine and let it own boundaries. `If-Match` enforcement moves to engine's first-segment `CompareAndSave` (§4.1). Today's behaviour preserved for cascades with no `COMMIT_BEFORE_DISPATCH` processor (single-segment degenerate case, single Save, single Commit — observably identical to today).
- **`internal/domain/workflow/validate.go`** — reject `startNewTxOnDispatch: true` outside `COMMIT_BEFORE_DISPATCH`.
- **`api/spec/*` (OpenAPI source)** — add the enum value and the new field; regenerate `api/generated.go`.
- **`docs/ARCHITECTURE.md`** — execution-modes table, audit-trail-durability note, transaction-flow swimlane, network-partition analysis, performance-sizing — see §15 for line-level edits.
- **`docs/CONSISTENCY.md`** — transactional-umbrella qualifier, segment-boundary visibility, idempotency caveat, replacement of "prefer ASYNC_NEW_TX for slow work" guidance — see §15.
- **`docs/CONCURRENCY.md`** — engine-owned per-cascade segment state, cluster-routing note for segments, application-responsibility class addition — see §15.
- **`cmd/cyoda/help/content/workflows.md`** — document the new mode and the flag, the idempotency requirement, the visibility caveat, the no-double-write best-practice cross-reference (§10.3).
- **Tests** — new E2E and unit tests per §16. Existing `ASYNC_NEW_TX` tests remain green untouched.

## 15. Reconciliation requirements

The plan that follows this spec must include explicit doc-update tasks for every line below. These are not optional polish — the docs are load-bearing references and silent drift is a regression.

### `docs/ARCHITECTURE.md`

- **§3.1 (around line 327ff):** add note that the engine, not the handler, owns TX boundaries when `COMMIT_BEFORE_DISPATCH` is in use, and owns per-segment SPI writes.
- **§3.4 Transaction Lifecycle Manager (around line 358):** add the new mid-cascade home-node-crash failure mode (entity durable in pre-callout state, in-flight cascade lost; recovery is application's concern).
- **§4.2 Transaction Routing (around line 486):** state explicitly that a `COMMIT_BEFORE_DISPATCH` cascade pins all segments to the home node, and that the response txID is the final segment's txID.
- **§4.4 Transaction Flow swimlane (around line 642ff):** add a `COMMIT_BEFORE_DISPATCH` swimlane variant showing the segment boundary.
- **§4.5 Network Partition Analysis (around line 685ff):** add a Phase covering segment-boundary partitioning and the resulting stranded-entity state.
- **§5.4 Execution Modes table (around lines 906–914):** add `COMMIT_BEFORE_DISPATCH` row including the `startNewTxOnDispatch` flag and the audit-trail durability change.
- **§5.5 Audit Trail (around line 916ff):** document the segment-boundary placement rule for the existing `SMEventProcessingPaused` and `SMEventStateProcessResult` events (no new event types).
- **§7 Operational Bottlenecks (around lines 1513, 1520, 1522):** rewrite the Mitigation paragraph — `COMMIT_BEFORE_DISPATCH` is the primary connection-pool-pressure mitigation; ASYNC_NEW_TX no longer is.
- **§8 Performance Sizing (around lines 1582, 1593):** add a row for `COMMIT_BEFORE_DISPATCH` connection-time arithmetic (Appendix A's worked example).

### `docs/CONSISTENCY.md`

- **§1 contract preamble (lines 13–34):** add a sentence "Per-segment commit visibility is *not* part of this contract; see §10 for `COMMIT_BEFORE_DISPATCH`."
- **§4 Transactional umbrella (lines 156–191):** rewrite the "All cross-entity CRUD performed by workflow processors during transition execution happens inside that enclosing transaction" claim. Qualify with "except segments crossed by a `COMMIT_BEFORE_DISPATCH` processor, which split the cascade into multiple transactions."
- **§5 Operational rule (lines 195–204):** add: "Workflow criteria on transitions following a `COMMIT_BEFORE_DISPATCH` segment must not depend on cascade atomicity for the work in earlier segments."
- **§10 Practical guidance (lines 401–427):** add the visibility-caveat bullet, the idempotency-asymmetry bullet, and the no-double-write best-practice (with explicit statement if not currently documented elsewhere). Replace the "prefer `ASYNC_NEW_TX` for slow work" recommendation with `COMMIT_BEFORE_DISPATCH` guidance.
- **Appendix A (lines 447–854):** add a closing subsection "Interaction with `COMMIT_BEFORE_DISPATCH`" — the on-duty-cap example relies on cascade atomicity for the PROMOTE pipeline; mode-4 in PROMOTE breaks it.

### `docs/CONCURRENCY.md`

- **§2 State scope table (lines 50–67):** add a row for engine-owned per-cascade segment state (per-request, per-cascade lifetime).
- **§6 Cluster routing (lines 158–211):** add a bullet on segment-pinning to the home node and the new mid-cascade-node-death mode.
- **§7 Application responsibility (lines 213–242):** add a fourth class — "Workflow authors using `COMMIT_BEFORE_DISPATCH` must implement processor idempotency themselves; the engine cannot deduplicate replays."

### `cmd/cyoda/help/content/workflows.md`

- Add the `COMMIT_BEFORE_DISPATCH` enum value (and the `startNewTxOnDispatch` flag) to the existing executionMode list (currently around line 143).
- Add the no-double-write best-practice as an explicit bullet — even if §10.3 is the first place it's written down. (Audit during plan: if it is documented elsewhere, cross-link instead of duplicating.)

## 16. Test coverage strategy

The following cases must have tests in the implementation plan. E2E tests live in `internal/e2e/`; unit tests in `internal/domain/workflow/engine_test.go` and `internal/domain/workflow/validate_test.go`.

1. **`false` branch happy path** (E2E): multi-segment cascade with one `COMMIT_BEFORE_DISPATCH` processor, processor returns mutations, CAS succeeds, cascade completes, response carries final segment's txID.
2. **`false` branch CAS conflict** (E2E): concurrent committer between `TX_pre.Commit` and `TX_post`'s CAS; assert `409 retryable`, cascade aborts, no partial post-callout state visible.
3. **`true` branch happy path** (E2E): processor performs CRUD via `TX_post`'s token on a *different* entity (not the cascade-anchor), returns mutations for the anchor; both writes commit atomically.
4. **`true` branch double-write violation** (unit): processor writes the cascade-anchor entity itself AND returns mutations for it; assert engine's apply-result wins (LWW per §10.3); explicitly mark this test as documenting the violation, not endorsing it.
5. **Engine crash between `TX_pre.Commit` and dispatch** (unit, kill-simulation): assert entity durable in pre-callout state, audit shows `SMEventProcessingPaused` and no matching `SMEventStateProcessResult`.
6. **Engine crash between dispatch return and `TX_post.Commit`** (unit, kill-simulation): assert entity durable in pre-callout state.
7. **Multi-entity cascade with mid-cascade `COMMIT_BEFORE_DISPATCH`** (E2E): cascade mutates A (anchor) and B; segment-boundary CAS guards A only, B's protection comes from re-read in `TX_post` (§4.4 / §10.4).
8. **Hot-entity concurrent cascades** (E2E): two cascades on the same anchor entity; assert one wins, the other gets `409 retryable`, retries cleanly (covers §11 livelock attractor).
9. **`If-Match` propagation** (E2E): supply stale `If-Match` to a cascade containing a `COMMIT_BEFORE_DISPATCH` processor; assert abort on first segment, no segment commits, no external dispatch fires (§4.1).
10. **Concurrent search across a segment boundary** (E2E): another transaction's `Search`/`GetAll` runs during the dispatch wait; assert it sees the pre-callout state (covers §10.2).
11. **Single-segment cascade regression bound** (E2E): cascade with no `COMMIT_BEFORE_DISPATCH` processors; assert byte-for-byte identical observable behaviour to current (single Save, single Commit, single audit event grouping, single `EntityVersion` row).
12. **Validator rejects `startNewTxOnDispatch: true` outside `COMMIT_BEFORE_DISPATCH`** (unit): registration fails with a clear error message.
13. **Audit-event placement** (unit): assert `SMEventProcessingPaused` lands in `TX_pre`'s audit, `SMEventStateProcessResult` in `TX_post`'s, no audit event spans both TXs.
14. **Cluster mode: `TX_post` opens on the same node that committed `TX_pre`** (unit / integration): assert via the cluster TX-token registry that the same node owns both segments' TX state.
15. **`engine.Loopback()` entry-point coverage** (E2E): trigger Loopback on an entity whose current state has an outgoing user-configured automated transition bearing a `COMMIT_BEFORE_DISPATCH` processor; assert segmentation applies via the third cascade entry-point exactly as it does for `Execute` and `ManualTransition`. (No special "loopback transition" — the test exercises the entry-point, not a hypothetical transition kind.)

These cases are also the verification gate before merging — every one must be green.

---

## Appendix A: Worked example

5-step cascade where step 3 is `COMMIT_BEFORE_DISPATCH` (default flag), 2-second processor.

**Today (SYNC for step 3):** PG connection held for full ~10s; cascade ships in one TX; if anything fails, full rollback.

**With `COMMIT_BEFORE_DISPATCH` on step 3:**
- `TX_pre` covers steps 1, 2, and the pre-callout state of step 3. Connection held ~50–100ms (engine flushes pre-callout entity state, commits).
- `TX_pre` commits. Connection released. Entity durable in pre-callout state.
- Processor dispatched, runs ~2s outside any transaction.
- `TX_post` opens. ~50ms of work: CAS-apply result (expected = `T_pre`), run steps 4 and 5 inline (still `TX_post`, since step 4 and 5 are SYNC), commit.
- Total: ~2s wall clock. PG connection held for ~150ms cumulative across `TX_pre` + `TX_post`.

Pool pressure under 10 concurrent such cascades: ~10 × 150ms = 1.5 connection-seconds, vs today's 10 × 10s = 100 connection-seconds. ~67× improvement on connection-pool pressure for this workload.

## Appendix B: Application-builder semantics

This appendix answers integrator-level questions surfaced during design review. None of these introduce new mechanisms — they pin down the engine's observable contract for the questions an integrator will ask in practice.

### C.1 TX_post conflict-retry policy

When `CompareAndSave` in TX_post fails because a concurrent transaction modified the anchor entity (e.g. a manual cancel transition fired while a `COMMIT_BEFORE_DISPATCH` processor was mid-flight):

- **The engine fails TX_post once and surfaces `ErrConflict` → `409 retryable`.** Cascade halts. Entity remains durable in the pre-callout committed state. **No engine-side retry. No engine-side compensation transition.** The caller decides what to do — retry the original API call (which restarts the cascade from the beginning), give up, or route via application-layer logic.
- **No automatic compensation transition** is fired. The workflow's existing transition / criteria mechanisms are evaluated only when a transition explicitly fires; CAS conflict is not an event the workflow can subscribe to.
- **External side effects produced by the dispatched processor are not rolled back.** If the processor created a TeamCity build (or any external resource) before the conflict, the build persists while the entity has reverted to pre-callout. This is a workflow-author concern: either dispatch-then-CAS-fail-recovery is designed at the application layer, or the processor's external effect must be detect-and-cleanup-friendly.

Engine retry was rejected because it duplicates external dispatch cost (cf. §11 cost amplification) and changes the existing 409-retryable client contract. Engine-side compensation was rejected because evaluating a "criterion against the conflict outcome" requires a mechanism the workflow model does not have, and adding one would broaden the change beyond this issue's scope.

### C.2 Re-dispatch identity on transient processor failures

When the processor fails to respond (DISPATCH_TIMEOUT, member crash, network partition between dispatch and response):

- **The engine does not retry the dispatch.** There is no reaper, no automatic re-dispatch mechanism for in-flight processors. The engine surfaces the failure; TX_post is not opened; entity remains in pre-callout state; cascade halts.
- **Each client-driven retry is a fresh dispatch with a new dispatch ID.** The dispatch ID is not stable across retries; it is generated per-call. Processors that need deduplication must do it on **application-meaningful keys** the workflow author embeds in the entity payload (e.g. an idempotency key, a stable resource identifier, a deterministic external-resource name derived from entity ID + a stable timestamp) — not on the dispatch ID.
- **Max-redispatch count is zero engine-side.** The client's retry policy is the only bound. Q1's CAS-conflict retries and Q2's transient-failure retries share no engine-side budget because both are zero engine-side; the client-side budget covers both.

For external-side-effect processors (creating builds, sending notifications, charging payments): the workflow author must design idempotency on application-meaningful keys — typically by setting a deterministic external identifier on the entity *before* the dispatch fires, and having the processor check the external system for that identifier before triggering a new side effect.

### C.3 Behaviour on `success=false`

When a `COMMIT_BEFORE_DISPATCH` processor returns `success=false` (or times out, or the dispatch fails):

- **Same as `SYNC` mode today** (`cmd/cyoda/help/content/workflows.md:143`): returns `WORKFLOW_FAILED` (`400`); entity remains in the pre-callout state (the source state of the failing transition); cascade halts; no automatic compensation.
- **No criterion-based compensation routing.** The workflow model does not provide a mechanism to read processor return data and route to a compensation transition. If you need this pattern, model it at the application layer: the application explicitly fires a follow-up transition based on its own logic, and that transition's criteria can read the entity's data.

### C.4 Idempotency-skip return shape

When a processor decides to no-op (e.g. detects that the external resource already exists):

- **Recommended return: `success=true, payload.data=null`.** TX_post still opens and commits — the cascade still needs to advance the entity from S_pre to S_post (the transition's target state). `payload.data=null` means the engine does **not** overwrite the entity's `data` field; it does still write the new `state` and the row receives a new `transaction_id`. So:
  - The version bumps (the row is rewritten with the new state + new txID).
  - The data field is preserved.
  - CAS still applies (`expected=T_pre`); a concurrent committer is still detected.
- **No "skip TX_post" mode exists.** The engine always opens TX_post and commits the state advance when the processor returns success. There is no way to return success and have the cascade stay in pre-callout state.
- **For true no-op (no version bump, no state change), use a transition criterion instead of a processor-side check.** A criterion on the transition that returns false aborts the transition before dispatch; the entity stays in the prior state, no processor runs, no version bumps. This is the right pattern for "if external resource already exists, do nothing" — the criterion reads the entity's payload (which holds the resource identifier) and short-circuits.

### C.5 Config attribute confirmations

- **`executionMode: COMMIT_BEFORE_DISPATCH`** is a per-processor field on `ExternalizedProcessorDefinitionDto` (the existing schema location for `executionMode`), sibling to the existing `SYNC` / `ASYNC_SAME_TX` / `ASYNC_NEW_TX` values. Per §6.
- **`startNewTxOnDispatch: bool`** is a sibling field on the same processor object, default `false`, valid only when `executionMode == "COMMIT_BEFORE_DISPATCH"`. Validator rejects `true` for any other mode (§6.1).
- **`responseTimeoutMs`** semantics unchanged — same per-processor wall-clock budget for the dispatched call. For `COMMIT_BEFORE_DISPATCH`, this is the timeout window between `TX_pre.Commit` and `TX_post.Begin` (the external compute interval). The connection-pool relief benefit of the `false` branch applies during this window.

## Appendix C: References

- Issue #27 (this document's subject)
- `/Users/paul/go-projects/cyoda-light/cyoda-light-go/docs/design/fault-analysis.md` (predecessor's design analysis — fault line 3 §4, fault line 4 §5, commit-on-callout option §11)
- `internal/domain/workflow/engine.go:529-638` — current `executeProcessors` implementation
- `internal/domain/entity/service.go:867-995` — current handler-owned TX boundaries around the engine
- `api/generated.go:439-447` — current `executionMode` enum values
- `spi/persistence.go:23-57` — `EntityStore` interface, `CompareAndSave` signature
- `spi/transaction.go:23-25` — `Begin` and `Commit` signatures
- `spi/txcontext.go:101-106` — context-scoped TX state pattern
- `cyoda-go-cassandra/internal/integration/tx_manager_test.go:192-218` — evidence for §9 finding
- `docs/CONSISTENCY.md` (full document, especially §4, §10, Appendix A) — primary reconciliation target
- `docs/ARCHITECTURE.md` (full document, especially §3.1, §3.4, §4.2, §4.4, §4.5, §5.4, §5.5, §7, §8) — primary reconciliation target
- `docs/CONCURRENCY.md` (full document, especially §2, §6, §7) — primary reconciliation target
