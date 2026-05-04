# Issue #27 — `COMMIT_BEFORE_DISPATCH` processor execution mode

**Status:** Design accepted — pending plan
**Issue:** [#27](https://github.com/Cyoda-platform/cyoda-go/issues/27)
**Milestone:** v0.7.0 (release branch `release/v0.7.0`, pre-tag)
**Date:** 2026-05-04
**Author:** Paul Schleger (with Claude)

---

## 1. Problem

The cascade engine runs workflow processors in three execution modes today: `SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX` (`api/generated.go:439-447`, `internal/domain/workflow/engine.go:533-638`). All three execute the processor while the cascade's transaction is open and the PG connection is held — `SYNC`/`ASYNC_SAME_TX` inline in the parent transaction, `ASYNC_NEW_TX` in a savepoint nested inside it (postgres) or in a child TX after committing the parent (cassandra — see [§9](#9-out-of-scope-finding-cassandra-vs-postgres-async_new_tx-divergence) below).

Under processor-heavy workloads with deep cascades and slow external compute (ML inference, third-party APIs), this couples external latency directly to PG connection-pool capacity. A 5-step cascade with 2-second processors holds a connection for ~10 seconds; ten concurrent such cascades consume 40% of a 25-connection pool for that whole window, blocking other tenants. This is fault line 3 of the predecessor's `fault-analysis.md` §4 (located at `/Users/paul/go-projects/cyoda-light/cyoda-light-go/docs/design/fault-analysis.md`; not currently mirrored in this repo, referenced for context only — see §11 of that doc for the design history).

Workflow authors who don't need full cascade atomicity have no way to opt out. Their only workaround is to split the cascade into separate API calls and have the client drive a manual loopback transition after each external compute step — possible but tedious.

## 2. What we're adding

A fourth `executionMode` value: `"COMMIT_BEFORE_DISPATCH"`. When a processor declares this mode, the engine commits the current transaction before dispatching the processor and opens a new one on return. The PG connection is released during the external compute window. The processor's returned result (entity mutations) is applied to the entity in the new transaction via `CompareAndSave`, using the txID of the prior commit as the expected version. The cascade then continues normally.

This is the server-side automation of a sequence the client could already do today (`Update` → wait for response → `ManualTransition` with the result). The engine claims **no special consistency rights** beyond what a client doing the same loopback by hand would have. Conflicts surface through the existing `ErrConflict` / HTTP `409` retryable path. This framing — "engine just does what a client could do, in fewer round-trips" — is the design's load-bearing principle and the reason no new SPI surface, no new entity-meta semantics, no read filter, no write guard, no reaper, and no cascade ledger are needed.

An optional `startNewTxOnDispatch: bool` flag (default `false`) selects between two sub-variants:

- **`false` (default):** commit parent → dispatch processor with no transaction context → on return, open fresh TX2 → CAS-apply result → continue cascade. Connection is released entirely during processor execution. Solves fault-line-3.
- **`true`:** commit parent → open TX2 → dispatch processor with TX2's transaction token → processor may do transactional work during its execution → on return, apply result in TX2 → commit TX2. Connection is held in TX2 (a fresh connection) during processor execution. Does **not** solve fault-line-3, but isolates the processor's work in its own atomic unit decoupled from the parent cascade's pre-commit work.

## 3. Mechanism (canonical sequence)

For a transition `T: S_pre → S_post` whose processor `P` has `executionMode: "COMMIT_BEFORE_DISPATCH"`:

Let `TX_pre` be the parent transaction the cascade was running in when it reached `T`. Let `T_pre` be the txID of `TX_pre`'s commit (assigned at commit time and stamped onto the entity row). Let `TX_post` be the transaction in which the result is applied and the cascade continues.

```
1. Cascade in TX_pre reaches transition T, processor P
2. Engine writes pre-callout state into TX_pre: entity at state S_pre
3. Engine calls TxMgr.CommitTx(TX_pre) → returns T_pre
   [PG connection released; entity durable in S_pre stamped with T_pre]

4. Branch on startNewTxOnDispatch:

   (a) startNewTxOnDispatch == false (default):
       4a. Engine dispatches P(entity) with no tx token in ctx
           [P executes externally; no transaction is open anywhere]
       5a. P returns result R (or error E)
       6a. Engine calls TxMgr.Begin() → opens TX_post
           [fresh PG connection acquired]

   (b) startNewTxOnDispatch == true:
       4b. Engine calls TxMgr.Begin() → opens TX_post
           [PG connection acquired for TX_post; held throughout dispatch]
       5b. Engine dispatches P(entity) with TX_post token in ctx
           [P may perform transactional work via TX_post during execution]
       6b. P returns result R (or error E)

7. Engine, inside TX_post: applies R via CompareAndSave(entity,
   expectedTxID = T_pre)
   • CAS conflict: bubbles ErrConflict → 409 retryable; cascade aborts
     at this segment; TX_post rolled back; caller retries.
   • CAS success: entity is now in S_post (or whatever R encodes).
8. Cascade continues from S_post inside TX_post until either the next
   COMMIT_BEFORE_DISPATCH segment boundary or final commit.
```

If the processor itself returns an error E (not a CAS conflict), the engine surfaces it the same way it surfaces a `SYNC` processor failure today: cascade aborts. There is no automatic compensation routing — the workflow author's options are the same as for any processor failure (criteria-based branching on subsequent transitions, or letting the caller retry the original API call which restarts the cascade from the beginning).

## 4. Engine TX ownership inversion

Today, `service.UpdateEntity` (`internal/domain/entity/service.go:867-995`) opens TX1 around the entire `engine.ManualTransition()` call. The engine never commits or begins; it just runs.

With `COMMIT_BEFORE_DISPATCH` introduced, the engine must own TX boundaries. The handler hands `txMgr` (and any context the cascade needs) to `engine.Execute()` / `engine.ManualTransition()` and the engine drives commit/begin around segment boundaries. Cascades with no `COMMIT_BEFORE_DISPATCH` processors run as a single segment — degenerate case, observable behavior identical to today.

This is the largest internal refactor in the change. The same pattern applies to `engine.Execute()` (creation cascade) and `engine.ManualTransition()` (manual transition cascade); `engine.Loopback()` is reviewed for consistency.

## 5. SPI changes

**None.**

The mode is implementable entirely with existing SPI primitives:

- `TransactionManager.CommitTx(ctx, txID)` — commits parent
- `TransactionManager.Begin(ctx)` — starts new TX
- `EntityStore.Get(ctx, entityID)` / `EntityStore.CompareAndSave(ctx, entity, expectedTxID)` — read + CAS-apply (`spi/persistence.go:23-57`)
- Existing `spi.WithTransaction(ctx, state)` / `spi.GetTransaction(ctx)` for plumbing TX context (`spi/txcontext.go:101-106`)

Postgres, sqlite, memory, and cassandra plugins all support these calls today. **The cassandra plugin needs no parity work for this feature.** No SPI version bump.

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

### Workflow validation at registration

At workflow registration (`internal/domain/workflow/validate.go`), the validator MUST reject:

- `startNewTxOnDispatch: true` combined with any `executionMode` other than `COMMIT_BEFORE_DISPATCH`. This is a configuration error and must be a hard reject, not a warning, so misconfigurations surface at registration time rather than at first cascade execution.

Reachability and compensation-path validation are explicitly **NOT** added — `COMMIT_BEFORE_DISPATCH` does not require them. The workflow author's failure-handling responsibility is the same as for any processor failure.

## 7. HTTP API and error surface

No new HTTP endpoints, no new request/response shapes, no new error codes. Cascades using `COMMIT_BEFORE_DISPATCH` look identical to clients:

- The HTTP request stays open until the full cascade completes (server-side: TX1 commit, dispatch, TX2 begin, CAS-apply, continue, …, final commit). The PG connection is **not** held during external dispatch, but the HTTP socket still is.
- CAS conflict during continuation surfaces as the existing `ErrConflict` → `409` with `retryable: true`. Caller's retry of the same API call restarts the cascade from the beginning; the processor will be re-dispatched. Idempotency is the workflow author's responsibility (see §10).
- Processor failure during dispatch surfaces the same way as a `SYNC` processor failure today.

There is **no `202 Accepted` / async-poll variant in this scope**. Synchronous client wait is the only client UX.

## 8. Audit trail (side benefit)

Each segment of a `COMMIT_BEFORE_DISPATCH` cascade commits its own audit events at TX commit time. This is an improvement over today's all-or-nothing model where a cascade failure rolls back all audit events for that cascade. Operators investigating a stranded cascade get durable audit for the segments that completed; today they get nothing.

This is observable behavior and worth a note in `docs/CONSISTENCY.md` and `docs/ARCHITECTURE.md`.

## 9. Out-of-scope finding: cassandra-vs-postgres `ASYNC_NEW_TX` divergence

While verifying the design, we confirmed via the cassandra plugin's `tx_manager_test.go:192-218` (`TestTxManager_Savepoint_CommitsParent`) that **`Savepoint` on cassandra commits the parent transaction and starts a fresh child**, whereas on postgres it issues a PG `SAVEPOINT` nested in the parent. This means today's `ASYNC_NEW_TX` execution mode behaves materially differently across backends:

- **Postgres**: parent rollback discards the side-effect work (savepoint shares parent's commit fate).
- **Cassandra**: parent rollback does **not** discard the side-effect work (parent already committed at the savepoint call).

The current canonical doc (`docs/ARCHITECTURE.md:344, 914, 1522`, `cmd/cyoda/help/content/workflows.md:145`) describes the postgres semantics as canonical. This is a real cross-plugin Cloud-parity-style split that is **out of scope for this issue** and should be filed as a separate ticket. No changes to `ASYNC_NEW_TX` are made by this design.

## 10. Workflow author requirements

A `COMMIT_BEFORE_DISPATCH` processor must be **idempotent or have an external mechanism for detecting prior completion** (e.g., a write-once external resource ID). On CAS conflict during continuation, or on processor crash mid-dispatch, the caller's retry will re-dispatch the processor. The engine cannot deduplicate.

This requirement is documented in `cmd/cyoda/help/content/workflows.md` alongside the new mode and in any operator-facing docs. It is the same idempotency guidance Cyoda Cloud applies to its own ASYNC_NEW_TX processors.

## 11. Changes by file (high-level)

- **`internal/domain/workflow/engine.go`** — engine takes `txMgr` (and the prior-commit txID context); cascade loop becomes segment-aware; `executeProcessors` gains a new branch for `COMMIT_BEFORE_DISPATCH` that performs commit-before-dispatch and CAS-apply-on-return.
- **`internal/domain/entity/service.go`** — `UpdateEntity`, `CreateEntity`, batch variants no longer wrap the engine call in a transaction; they hand `txMgr` to the engine and let it own boundaries. Today's behavior preserved for cascades that have no `COMMIT_BEFORE_DISPATCH` processor (single-segment degenerate case).
- **`internal/domain/workflow/validate.go`** — reject `startNewTxOnDispatch: true` outside `COMMIT_BEFORE_DISPATCH`.
- **`api/spec/*` (OpenAPI source)** — add the enum value and the new field; regenerate `api/generated.go`.
- **`docs/ARCHITECTURE.md`** — update the execution-modes table to add `COMMIT_BEFORE_DISPATCH` row, note audit-trail durability change.
- **`docs/CONSISTENCY.md`** — document that segment commits are observable to readers between segments and that this is the workflow author's modelling concern, not an engine invariant.
- **`cmd/cyoda/help/content/workflows.md`** — document the new mode and the flag, including the idempotency requirement.
- **Tests** — new E2E test in `internal/e2e/` exercising a multi-segment cascade with `COMMIT_BEFORE_DISPATCH`, including the CAS-conflict path. Unit tests in `internal/domain/workflow/engine_test.go` for the new branch and validation rules. Existing `ASYNC_NEW_TX` tests must remain green untouched.

## 12. Out of scope for this issue

- Stranded-entity recovery (reaper). Engine crashes between segments leave the entity durable in `S_pre`; recovery is the application's concern, the same as today's "client crashes mid-loopback" scenario.
- Async/202+poll API mode for long-running cascades. v1 is synchronous client wait.
- Per-state `externalWritable: false` policy for guaranteeing cascade-wins semantics on race. CAS conflict surfaces normally; if a workflow needs cascade-wins behavior, file separately.
- Workflow validation for compensation paths. `COMMIT_BEFORE_DISPATCH` reuses existing processor-failure semantics; no new validation needed.
- Any rename or deprecation of existing `executionMode` values.
- The cassandra-vs-postgres `ASYNC_NEW_TX` semantic divergence (§9). To be filed separately.

## 13. Risks

**Engine TX-ownership refactor (§4) is the largest change and the riskiest.** The engine becomes responsible for TX lifecycle in cases where the handler used to be. Bugs here can manifest as connection leaks, double commits, or lost cascades. Mitigation: the change is gated behind the new mode in observable behavior — single-segment cascades preserve today's outcomes — but the code path runs for every cascade. Coverage for the single-segment degenerate case must be maintained.

**Idempotency is now a workflow-author responsibility** (§10). Authors who use `COMMIT_BEFORE_DISPATCH` with non-idempotent processors will see double-execution under retry. Mitigation: prominent documentation; perhaps a runtime warning at workflow registration if a `COMMIT_BEFORE_DISPATCH` processor is detected (advisory only, not a hard reject).

**Observable intermediate states** are a behavior change for any reader currently depending on cascade atomicity for visibility. Mitigation: documented as a property of the new mode; workflow authors who need invisibility model that explicitly through state machine design.

## 14. Decision log

The following alternatives were considered and rejected during brainstorming. Recording them so future readers don't relitigate.

- **`_meta.pending` flag with read-filter, write-guard, bypass-context, and stranded-entity reaper.** Rejected: overloaded three concerns (visibility, liveness, concurrency) into one flag; visibility is a workflow-modelling concern, liveness can be left to the application, concurrency is solved by `CompareAndSave`. The engine claims no special consistency rights.
- **Cascade ledger (`cascade_in_flight` table).** Rejected: introduced for stranded-entity detection, but stranded entities are equivalent to "client crashed mid-loopback" — out of scope for the engine.
- **Per-state `externalWritable: false` policy.** Rejected: useful but separable; ships only if a workflow asks for it.
- **`commitOnCallout: bool` flag on existing `executionMode` values.** Rejected in favor of a new enum value (better OpenAPI compatibility, named declaratively).
- **Renaming `ASYNC_NEW_TX` → savepoint mode and reusing the name for the new semantic.** Rejected: too much migration tax against minimal naming-honesty gain; also flips Cloud-parity config-token meaning.
- **SPI bump for new `EntityMeta.Pending` field, `ErrEntityPending` sentinel, `WithPendingAccess` context scope.** Rejected as a knock-on of dropping the `pending` flag; not needed.

---

## Appendix A: Worked example

5-step cascade where step 3 is `COMMIT_BEFORE_DISPATCH` (default flag), 2-second processor.

**Today (SYNC for step 3):** PG connection held for full ~10s; cascade ships in one TX; if anything fails, full rollback.

**With `COMMIT_BEFORE_DISPATCH` on step 3:**
- TX1 covers steps 1, 2, and the pre-callout state of step 3. Connection held ~50-100ms.
- TX1 commits. Connection released. Entity durable in S_pre at step 3.
- Processor dispatched, runs ~2s outside any transaction.
- TX2 opens. ~50ms of work: CAS-apply result, run steps 4 and 5 inline (still TX2, since step 4 and 5 are SYNC), commit.
- Total: ~2s wall clock. PG connection held for ~150ms cumulative across TX1 + TX2.

Pool pressure under 10 concurrent such cascades: ~10 × 150ms = 1.5 connection-seconds, vs today's 10 × 10s = 100 connection-seconds. ~67× improvement on connection-pool pressure for this workload.

## Appendix B: References

- Issue #27 (this document's subject)
- `/Users/paul/go-projects/cyoda-light/cyoda-light-go/docs/design/fault-analysis.md` (predecessor's design analysis, §3 fault line 3, §4 fault line 4, §11 commit-on-callout option)
- `internal/domain/workflow/engine.go:529-638` — current `executeProcessors` implementation
- `internal/domain/entity/service.go:867-995` — current handler-owned TX boundaries around the engine
- `api/generated.go:439-447` — current `executionMode` enum values
- `spi/persistence.go:23-57` — `EntityStore` interface, `CompareAndSave` signature
- `spi/txcontext.go:101-106` — context-scoped TX state pattern (existing precedent)
- `cyoda-go-cassandra/internal/integration/tx_manager_test.go:192-218` — evidence for §9 finding
