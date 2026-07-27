# Per-entity scheduled-transition firing time via a Function callout — design

- **Issue:** [#419](https://github.com/Cyoda/cyoda-go/issues/419)
- **Milestone:** v0.8.3
- **Status:** Design agreed (brainstorming). Four independent reviews; findings folded in (§14). Dispatch unification incorporated as first phase (§4.2). Uniform infra-503 reconciliation across all four compute-infra codes (§7).
- **Date:** 2026-07-17

## 1. Summary

Extend the scheduled-transition feature so an application can compute a
transition's firing time **per entity**, rather than only via a static
`delayMs` baked into the workflow definition. This is done by introducing a
generic **Function** — a third callout shape alongside **Processor** (mutates
the entity payload) and **Criterion** (returns a typed `bool`). A Function
returns a **declared typed value** without mutating the entity. Its first — and
initially only — consumer is scheduling, which uses a Function returning a
composite **`Schedule`** value (fire time + optional expiry).

## 2. Motivation

Today `schedule: {delayMs, timeoutMs}` fires at `scheduledTime = stateEntryTime
+ delayMs`, identical for every entity flowing through the transition. There is
no way to fire at a per-entity time — a deadline stored on the entity
(`expiresAt`), a per-customer SLA, a user-chosen reminder. The only existing
lever is the settled-interval re-arm, which can only postpone the fire by a
fixed delay measured from the write instant; it cannot express an absolute
per-entity instant or a data-derived delay.

## 3. Design philosophy (binding)

cyoda-go chooses **correctness and consistency over availability** and **fails
closed** (`CLAUDE.md` → Design Philosophy; `.claude/rules/correctness-over-availability.md`).
Applied here: an entity write that lands in a scheduled state and cannot
correctly determine its timer is **rejected and rolled back** — never committed
with a missing, guessed, or fallback timer. A committed entity in a scheduled
state with no valid armed task is a silent lost fire — the exact failure mode
the atomic arm/cancel dual-write exists to prevent
(`docs/cloud-parity/scheduled-transitions.md` §2). "The compute node is
unavailable" is therefore not a reason to degrade; it fails the operation.

## 4. The `Function` abstraction

The callout framework is uniform CloudEvents-over-gRPC (single `startStreaming`
bidi stream; a typed return is a JSON field on a response CloudEvent). It has
two shapes today; the Function is the third:

| Shape | Returns | Response field (precedent) |
|---|---|---|
| Processor | mutated entity payload | `EntityProcessorCalculationResponse.payload` |
| Criterion | typed `bool` (+ reason) | `EntityCriteriaCalculationResponse.matches (*bool)` |
| **Function** | **declared typed value** | **`EntityFunctionCalculationResponse.result` + `resultKind`** |

### 4.1 Wire contract (new)

New CloudEvent types, modelled exactly on the criterion callout:

- **Request** `EntityFunctionCalculationRequest` — same envelope as the criterion
  request: `requestId`, `entityId`, `functionName`, `workflow`, `transition`,
  `transactionId`, `parameters` (= `context` verbatim), optional `payload`
  (attached iff `attachEntity`). JSON schema in
  `docs/cyoda/schema/processing/EntityFunctionCalculationRequest.json`; Go type
  in `api/grpc/events`.
- **Response** `EntityFunctionCalculationResponse` — `requestId`, `entityId`,
  `success`, `result` (a JSON value), `resultKind` (string discriminator),
  `error {code, message, retryable}`, `warnings []string`. `result`/`resultKind`
  is the criterion `matches` slot widened to an arbitrary typed value. Schema in
  `docs/cyoda/schema/processing/EntityFunctionCalculationResponse.json`.

### 4.2 Dispatch unification (first implementation phase — prerequisite refactor)

Adding Function exposed a **pre-existing design smell**: there is no shared
callout-dispatch primitive. `DispatchProcessor` and `DispatchCriteria` are
parallel copies at every layer — single-node dispatcher (~60% byte-identical
boilerplate: tag routing, requestID, the entity-payload Meta block, CloudEvent
envelope, auth + tx-token attach, `TrackRequest`/`Send`, timeout, the whole
`select`/warnings/failure block), the cluster dispatcher (twin wire structs
sharing 9/11 fields, `ForwardProcessor`/`ForwardCriteria` differing only in a URL
path, twin peer handlers), and every decorator
(observability/skeleton/localproc). The tx-token mint is already duplicated 3×.
The *only* genuinely per-type code is (a) build the request body, (b) parse the
response body — plus **one** unavoidable fork: post-response interpretation
(processor merges the entity, criterion returns a bool, Function returns a raw
result). Adding Function as a third parallel copy would triple the smell.

So the **first phase of this work** is a behavior-preserving extraction, landed
green before any Function feature code:

- **Single-node** (`internal/grpc/dispatch.go`): extract `dispatchCalloutToMember(ctx,
  member, ceType, req, timeoutMs, label) (*ProcessingResponse, error)` (requestID,
  `NewCloudEvent`+auth+tx-token, `TrackRequest`/`Send`, timeout, the `select`
  + warnings/failure block) and `buildEntityPayload(entity)`. `DispatchProcessor`
  and `DispatchCriteria` become thin **build-request → dispatch → parse-response**
  wrappers over it.
- **Cluster** (`internal/cluster/dispatch/`): **collapse** the parallel peer wire
  into one generic path — a single `DispatchCalloutRequest/Response` (a `kind`
  discriminator + the shared fields + a body/result), one `Forward`, one handler,
  one route `/internal/dispatch/callout`. The per-type routes/structs
  (`/processor`, `/criteria`) and `ForwardProcessor`/`ForwardCriteria` are
  **removed**, not kept alongside. This is safe: pre-1.0, single live release
  line, same-version cluster deployments — there is no mixed-version peer-wire
  contract to preserve (do not add rolling-upgrade back-compat scaffolding for an
  internal node-to-node wire). Metrics keep per-type granularity via the `kind`
  label.
- **Decorators**: unchanged — the seam lives entirely *below*
  `contract.ExternalProcessingService`, so the engine and all implementors are
  untouched by the refactor.

The refactor is **invisible to real external consumers** — no public API, no
compute-node CloudEvents change. The internal node-to-node peer wire is
*simplified* (three routes → one), which is fine for a same-version cluster. It
is behavior-preserving at the `contract` boundary and guarded by the existing
`dispatch_test.go` / `streaming_test.go` / cluster tests (peer-wire tests are
updated to the unified route). Payoff: Function rides the shared primitive as a
thin delta, the multi-node path is the one generic forward (resolving the omitted
cross-node surface), and the primitive is the **single place** to mint the
infra-failure `AppError` (§7.1).

### 4.3 Engine-facing method (new)

`ExternalProcessingService` (`internal/contract/processing.go`) gains:

```go
DispatchFunction(ctx, entity, fn, workflow, transition, txID) (FunctionResult, error)
// FunctionResult = { Kind string; Value json.RawMessage }
```

The direct analog of `DispatchCriteria(...) (bool, reason, error)`, implemented as
a thin **build-request → `dispatchCalloutToMember` → parse-result** wrapper over
the §4.2 primitive (single-node and cluster). Dispatch is **kind-agnostic** — it
returns the raw typed result; each *consumer* validates the kind it needs.
Carrier struct `grpc.ProcessingResponse` (`internal/grpc/members.go:23`, already a
union) gains `Result json.RawMessage` + `ResultKind string`; the receive-loop
switch gains an `EntityFunctionCalculationResponse` case + `handleFunctionResponse`.
Note the node error shape carries `{message, retryable}` but **no code**
(`streaming.go`); `retryable` is a `*bool` — **nil ⇒ non-retryable**. A
node-returned functional error keeps today's callout-error surfacing (§7.2), so
no Function-specific code is assigned to it.

### 4.4 Deliberate generality (kept over a YAGNI objection)

There is one consumer (scheduling) and one kind (`Schedule`) today. The generic
typed-result callout is retained **by explicit product direction** so future
Function kinds (e.g. `Timestamp`, `Duration`, `Decimal`) slot in with **no new
wire type and no new dispatch method** — only a new consumer that asserts its
kind. We do **not** build a speculative consumer/kind *registry*: the single
consumer asserts `resultKind == "Schedule"` inline. Naming note: the callout is
a **Function**; it is distinct from the existing `"function"` *criterion*
predicate (`predicate.FunctionCondition`) — the new wire type name
`EntityFunctionCalculation*` keeps them separable.

## 5. Configuration

### 5.1 `schedule.function`

`schedule.function` is a field **on the schedule object** (not an entry in the
transition's `processors[]` array — so no processors-union / `Type`-discriminator
changes). It is **mutually exclusive** with `schedule.delayMs` and, like all
schedules, with `manual`.

```json
"schedule": {
  "function": {
    "name": "computeSlaDeadline",
    "resultKind": "Schedule",
    "calculationNodesTags": "sla",
    "attachEntity": true,
    "context": "…",
    "responseTimeoutMs": 5000
  }
}
```

Config fields (reuse the processor config surface, trimmed):

- `name` (required) — function name routed to the compute node.
- `resultKind` (required) — must be `"Schedule"` for the scheduling consumer;
  any other value is rejected at import.
- `calculationNodesTags` (required) — tag routing key.
- `attachEntity` (optional, **default `true`**) — whether the entity payload is
  sent. **Kept configurable**: some timers need no entity (e.g. "time to leave
  for the commute" derives purely from context/clock), so a node may opt out to
  avoid shipping the payload.
- `context` (optional) — pass-through, surfaced verbatim in `parameters`.
- `responseTimeoutMs` (optional) — arm-path callout timeout, **default
  5000 ms**. Deliberately lower than the 30s processor default because this call
  is on the synchronous write path (every settled save to a waiting entity, §6).
- **Dropped from the processor config surface:** `retryPolicy` (conflicts with
  the scheduler scan-retry, §8) and the async-suspend flags
  (`asyncResult`/`crossoverToAsyncMs`/`startNewTxOnDispatch` — N/A, the arm
  callout is inherently synchronous). `attachEntity` is **retained** (above).

The static path (`schedule: {delayMs, timeoutMs?}`) is **unchanged** and remains
available for its use cases (uniform "N after entry"). Absolute / per-entity
times live only on the Function path, since static config cannot read entity
data.

**Validation changes** (`internal/domain/workflow/validate.go`):

- The existing `schedule.delayMs > 0` check (~:341) hard-requires `delayMs > 0`
  for *any* non-nil schedule; it must become **conditional** — `delayMs > 0` is
  required only when `function` is **absent**. Otherwise a valid function-only
  schedule (no `delayMs`) fails import.
- New XOR: exactly one of `delayMs` / `function` must be present. The
  `function` + `manual` exclusion is inherited by the existing `Manual &&
  Schedule != nil` check (~:336), which already emits "manual and scheduled are
  mutually exclusive".
- `function` rules: `name` and `calculationNodesTags` required; `resultKind`
  must equal `"Schedule"`; else `VALIDATION_FAILED`.

### 5.2 The `Schedule` result

The compute node returns, as the typed `result`:

```json
{
  "fireAt":        1737072000000,   // absolute epoch-ms   ┐ exactly one — required
  "fireAfterMs":   3600000,         // relative to arm time ┘
  "expireAt":      1737075600000,   // absolute epoch-ms   ┐ at most one — optional
  "expireAfterMs": 600000           // relative to fireAt   ┘
}
```

Resolution at arm time (`e.now()` = `armTime`):

- **Fire** (required, exactly one field): `scheduledTime = fireAt`, or `armTime
  + fireAfterMs`. A past `scheduledTime` is **not** an error — it means "due
  now"; the scheduler picks it up on the next scan (subject to expiry).
- **Expiry** (optional, at most one field): absolute `expireAt`, or `fireAt +
  expireAfterMs` (relative expiry keeps today's "window past the fire time"
  meaning). Absent ⇒ never expires. Stored as the **existing relative**
  `ScheduledTask.TimeoutMs = resolvedExpiry − scheduledTime`, so the scheduler's
  scan/grace/expiry logic (`fire_scheduled.go:209-229`) runs **unchanged** and
  `ScheduledTask` needs **no new SPI field**. The absolute deadline is honored
  within the ±`expiryGraceMs` (default 100 ms) grace band — documented, not a
  bug (the grace band exists for cross-node clock-skew safety). A past `fireAt`
  paired with a short expiry can therefore arm and then expire-by-scan before
  firing (distinct from born-expired at arm) — consistent with existing
  scheduled-transition semantics.

### 5.3 Born-expired (replaces the naive "negative ⇒ malformed" rule)

If `resolvedExpiry ≤ scheduledTime` (the window is non-positive — e.g. a
relative fire re-armed until it drifts past a fixed absolute expiry, §14 A2),
the task is **born expired**: it must not fire past a deadline that has already
passed. Therefore:

- Do **not** arm the task, **and explicitly cancel any prior armed row for its
  `taskID` in the same transaction.** This is critical: `ReconcileForEntity`
  only deletes rows whose `SourceState != CurrentState`
  (`plugins/*/scheduled_task_store.go`), so a born-expired transition that is
  simply *omitted* from the arm set on a same-state re-arm would leave the
  **previous** armed row live — it would fire at its old time (a silent
  stale-fire, the inverse of the lost-fire we guard against). Born-expired
  transitions are therefore added to an explicit cancel set (by their
  deterministic `taskID`) passed to `ReconcileForEntity`, so the stale row is
  deleted in the same tx. (On first arm there is no prior row; the delete is a
  harmless no-op.)
- The write **succeeds** (this is a correct outcome, not a failure).
- Emit `SCHEDULED_TRANSITION_EXPIRE` with a "born expired at arm" detail (on this
  committed path, so it is durably recorded). **The born-expired IDs must be
  audited as `EXPIRE` and kept *out* of the reconcile `cancelled` result** —
  otherwise `arm.go` would also emit a spurious `SCHEDULED_TRANSITION_CANCEL`
  alongside the `EXPIRE` for the same row. So the cancel-set carries its own
  audit semantics, distinct from the `SourceState != CurrentState` cancels.

Only *genuinely malformed* results fail the write (§7): wrong `resultKind`,
non-numeric, missing fire field, both fire fields, both expiry fields.

This requires an additive `Cancel []string` set on the SPI `ReconcileRequest`
(task IDs to delete alongside `Arm`), returned/audited separately from the
`SourceState`-mismatch `cancelled` slice — see §9. The delete runs *inside*
`ReconcileForEntity` over committed state (not as a separate pre-reconcile
`Delete`), so it composes atomically with the entity write and is unaffected by
the same-tx-buffer caveat (`fire_scheduled.go:300-304`). Born-expired IDs are
never also in `Arm`, so there is no upsert-vs-delete ordering hazard.

## 6. Engine wiring (arm-time dispatch)

The Function is invoked in `reconcileScheduledTasks` (`arm.go`), the existing
arm path — the one place with the entity payload, tx `ctx`/`txID`, `e.extProc`,
and `e.now()` all in scope, running post-cascade on the final settle
(`engine.go:293/389/488`) and on the fire path (`fire_scheduled.go:305`). For a
transition whose `schedule.function` is set:

1. `e.extProc.DispatchFunction(ctx, entity, fn, wf, tr, txID)`, **suspending the
   tx-gate** for the external call (`txgate.Suspend`, no-op for the gate owner —
   `txgate.go:107`). **Scope the suspend to the callout only:** unlike
   `evaluateCriterion` (`engine.go:820`), which can `defer resume()` because it
   touches no tx buffer afterward, `reconcileScheduledTasks` writes the tx buffer
   *after* the dispatch (`ReconcileForEntity` stages the Upsert/Delete). The
   txgate contract requires `resume` **before** touching the shared tx buffer
   again (`txgate.go:104`), so here `resume()` must fire **immediately after
   `DispatchFunction` returns**, not be deferred around the reconcile body —
   otherwise the joined-callback case violates the gate rule.
2. Assert `Kind == "Schedule"`, parse `Value`, resolve fire/expiry to absolute,
   apply the born-expired rule (§5.3), else compute `scheduledTime` and
   `TimeoutMs`.
3. Write the `ScheduledTask` row in the same `ctx`/`txID` — atomic with the
   entity write, identical to today.

Because it sits on the settle/arm path, it runs on **every re-arm** — there is
no "entry vs re-arm" distinction; a loopback or same-state save is just a write
that cancels the prior timer and arms a fresh one. The callout is therefore made
on **every settled save** to an entity parked in a `schedule.function` state.
Routing is by `calculationNodesTags`. The scheduler *fire* path is unchanged: it
still just reads `scheduledTime`/`TimeoutMs` off the row.

**Coupling (explicit).** A routine data update to a waiting entity now makes a
synchronous compute-node round-trip inside the write transaction; if the node is
unavailable, the write fails (§3, by design). This is why `responseTimeoutMs`
defaults lower on this path (§5.1).

## 7. Error semantics

### 7.1 Making callout-infra failures 503 (decision: uniform, all four infra codes)

**Decision:** every **compute-infra-unavailable** condition surfaces as **`503`
retryable** — uniformly for processor, criterion, **and** Function. This fixes a
**pre-existing doc/code drift**: the runtime already defines four infra codes
(`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`, `DISPATCH_FORWARD_FAILED`,
`COMPUTE_MEMBER_DISCONNECTED` — `common/error_codes.go`), and their help topics
**already document `503 retryable`**, but **none is minted as an `AppError`** —
they are only string prefixes in `fmt.Errorf`, so all four land at
`classifyWorkflowError` → `400 WORKFLOW_FAILED`. The fix makes the code deliver
what the docs already promise. (Confirmed by round-4 review; the earlier "one
code" plan was incomplete and mislabeled timeouts.)

**Mint each at its emit site** as `Operational(503, <code>).AsRetryable()`:

1. **`NO_COMPUTE_MEMBER_FOR_TAG` — no member anywhere.** Preserve the
   `ErrNoMatchingMember` sentinel at the local `FindByTags → nil` site (cluster
   forwarding depends on `isNoMatchingMember` matching it — do **not** mint
   there). Mint at the two topology-terminal points: single-node via a
   `classifyWorkflowError` mapping (`entity/service.go` ~:1937), and cluster at
   `findPeerWithPolling`'s no-peer failure (`cluster_dispatcher.go:191`).
2. **`DISPATCH_TIMEOUT` — member resolved, no response in time.** Mint in the
   §4.2 shared primitive (`dispatch.go:164`; member exists, so no
   sentinel/forwarding concern). *This corrects the earlier draft, which wrongly
   labeled timeouts `NO_COMPUTE_MEMBER_FOR_TAG`.*
3. **`DISPATCH_FORWARD_FAILED` — peer had the tag but the forward failed.** Mint
   at the cluster peer-forward transport-failure site (`cluster_dispatcher.go:103,162`).
   The §4.2 primitive is single-node/local and never runs here, so this needs its
   own mint — **required for the Function to be correct multi-node**
   (`multi-node-primary`): a peer crash mid-forward of an arm-time Function
   callout must be retryable, not 400.
4. **`COMPUTE_MEMBER_DISCONNECTED` — member dropped mid-request.** Currently a
   dead code (no runtime emitter); wire it at the disconnect/`FailAllPending`
   path so an in-flight callout whose member disconnects surfaces 503 rather than
   400 (or a `nil`/closed-channel error).

The `classifyWorkflowError` change is an **`errors.As(err, &appErr)`** passthrough
(not a top-level type assertion): minted 503s are `%w`-wrapped several layers deep
(`engine_processors.go:136` → `fireTransition` → `engine.go:294` → `service.go:302`),
which a bare assertion misses. Verified: `retryable` propagates on any status when
`appErr.Retryable` is set (`common/errors.go:236`); the 5xx body stays generic +
ticket (no node internals leaked). gRPC shares this domain classifier (no separate
gRPC classifier), so both entrypoints get the 503.

`SCHEDULE_FUNCTION_UNAVAILABLE` from earlier drafts is **dropped** entirely.

### 7.2 Error codes

| Cause | Code | HTTP | Retryable | New? |
|---|---|---|---|---|
| No compute member for tags (anywhere) | `NO_COMPUTE_MEMBER_FOR_TAG` | 503 | yes | existing code; 400→503 |
| Member resolved, callout timed out | `DISPATCH_TIMEOUT` | 503 | yes | existing; 400→503 |
| Peer had tag, forward transport failed | `DISPATCH_FORWARD_FAILED` | 503 | yes | existing; 400→503 |
| Member disconnected mid-request | `COMPUTE_MEMBER_DISCONNECTED` | 503 | yes | existing (was dead); now wired |
| Function result malformed / wrong-kind | `SCHEDULE_FUNCTION_INVALID_RESULT` | 500 + ticket | no | **new (only new code)** |
| Compute node returned a functional error | *(unchanged callout-error surfacing)* | as today | per node's `retryable` | — |

Notes: all four infra codes **already have help topics** documenting 503, so
`TestErrCode_Parity` (code↔topic-name bijection) needs only the one new topic
`SCHEDULE_FUNCTION_INVALID_RESULT`; the four topics need no content change (they
already say 503 — the code catches up). No status↔doc guard test exists today
(parity is name-only), so the drift was untested; §13 adds the 503 assertions. A
node-returned *functional* error (node up, replies `success=false`) keeps today's
surfacing — not part of the infra-503 reconciliation. Past `fireAt` and
born-expired are **not** errors (§5.2/§5.3).

## 8. Observability of background arm failures

When the scheduler *fires* into a downstream state whose `schedule.function`'s
node is unavailable, the fire transaction fails and rolls back; the in-tx failed
audit is discarded, and the only surface today is `slog.Warn`
(`executor.go:75`) — so a broken downstream function silently blocks an
unrelated scheduled transition and retries every scan forever, only weakly
observable.

**Approach (decision: ERROR-log + metrics, not a durable audit event).** A
durable *audit* record surviving the fire rollback is deliberately **not**
built: `recordEvent` writes into the tx-bound context and rolls back **with** the
fire tx, and there is no out-of-tx audit facility (the criterion-stoppage work in
`f03ecc5` explicitly *preserved* the transactional-audit invariant — it records
on committed paths, not out-of-tx on rolled-back ones; so it is not the precedent
an earlier draft claimed). Building one would be new engine infrastructure with a
which-node/duplication story, out of scope here. It is also the **wrong layer**:
"a compute node was down so a timer could not be armed" is an *operational* event
to alert on, not entity audit-trail history.

So background arm-Function failures are surfaced by:

- Raising `executor.go:75` from `slog.Warn` → **`slog.Error`** (structured:
  `entityId`, `transition`, `sourceState`, tags, error), so it is alertable.
- The existing redispatch backoff (`CYODA_SCHEDULER_REDISPATCH_BACKOFF`) throttles
  the retry (no scan-storm); the task stays durable and fires once the node
  returns (failover delays, never loses).
- Scheduler metrics (fire-failure counter) already emitted by the scan loop.

The **request-driven** arm path needs no extra observability — the failure
surfaces to the caller as a 503 (§7). Node warnings on the background fire path
(no request ctx for `common.AddWarning`) are folded into the ERROR log rather
than lost. No new audit event type is introduced.

`DispatchFunction` calls `common.AddWarning`/`AddError` like the processor and
criterion dispatchers; those must remain **safe no-ops when the ctx carries no
diagnostics sink** (the background fire path uses a system ctx). The plan
verifies `AddWarning` does not panic on an absent sink, matching the existing
dispatchers' contract.

## 9. SPI changes

- Additive `TransitionSchedule.Function *ScheduleFunction` + new `ScheduleFunction`
  config type (fields per §5.1). `ScheduledTask` row shape is **unchanged** (§5.2
  stores expiry as the existing relative `TimeoutMs`).
- Additive **`ReconcileRequest.Cancel []string`** (task IDs to delete alongside
  `Arm`) so born-expired transitions cancel their prior row in the same tx
  (§5.3). All three plugin stores gain the explicit-delete branch in
  `ReconcileForEntity`; the SPI godoc for reconcile (currently "cancel any task
  whose `SourceState != CurrentState`") must be updated to describe the explicit
  cancel-set, and the SPI **`scheduled_task_store_conformance.go`** suite gains a
  born-expired-cancel case (picked up by all backends, including the commercial
  one).
- `DispatchFunction`/`FunctionResult` live in cyoda-go (`internal/contract`),
  not the SPI. The dispatch unification (§4.2) is entirely cyoda-go-internal — no
  SPI surface — and it *reduces* the per-callout-type transport fan-out. Adding
  the `DispatchFunction` **interface method** (§4.3) still touches every
  `ExternalProcessingService` implementor: `ProcessorDispatcher` (grpc),
  `ClusterDispatcher`, `TracingExternalProcessingService` (+ a `typeFunction`
  metric attribute / span), `skeleton.ExternalProcessingService`,
  `localproc.LocalProcessingService`, and test mocks — these are thin wrappers
  over the shared primitive, but they must be enumerated in the plan.
- **Additive interface surface ⇒ patch bump.** Coordinated cross-repo release
  (`MAINTAINING.md`): cut the `cyoda-go-spi` tag **first**, then bump the pin in
  the root + all three `plugins/*/go.mod` identically in one commit; `make
  check-spi-pin-sync`. During development, pseudo-version-pin to SPI `main` HEAD.

## 10. OpenAPI (HTTP contract)

- `TransitionScheduleDto` gains an optional `function` → new `ScheduleFunctionDto`
  (`name`, `resultKind` enum `["Schedule"]`, `calculationNodesTags`,
  `attachEntity`, `context?`, `responseTimeoutMs?`). Both `delayMs` and
  `function` stay optional; the **XOR is enforced server-side and documented**
  (OpenAPI cannot express it cleanly). Per the typed-but-open policy: enumerate
  properties, **no** `additionalProperties:false`, reject unknown input at the
  boundary.
- Adding an optional property is **non-breaking** ⇒ the oasdiff gate passes.
- The three error codes are documented on the affected write / transition /
  import responses.
- The **wire** schemas (`EntityFunctionCalculationRequest/Response`) are gRPC
  CloudEvents, not HTTP — they live in `docs/cyoda/schema/processing/*.json` +
  `api/grpc/events`, surfaced via `cyoda help cloudevents json` and `cyoda help
  grpc {proto,json}`.

## 11. Documentation set (Gate 4 / Gate 7)

- **`cmd/cyoda/help/content/workflows.md`** (SCHEDULED TRANSITIONS): new
  subsection — declaring `schedule.function`, the `Schedule` return shape,
  absolute-vs-relative fire/expiry, invoked-on-every-re-arm, fail-closed on
  callout failure, born-expired, XOR with `delayMs`.
- **Compute-node contract**: add `EntityFunctionCalculationRequest/Response` to
  `docs/cyoda/schema/processing/`; a Functions section for implementers aligned
  with the `cyoda:compute` skill's processor/criterion examples.
- **`errors/<CODE>.md`**: one new topic (`SCHEDULE_FUNCTION_INVALID_RESULT`). The
  four infra topics (`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`,
  `DISPATCH_FORWARD_FAILED`, `COMPUTE_MEMBER_DISCONNECTED`) **already document
  503** — no content change; the code catches up to the docs (§7). Consider a
  status↔doc guard test (none exists; parity is name-only) — folded into §13's
  503 assertions.
- **`docs/ARCHITECTURE.md`** + the `internal/cluster/scheduler_rpc.go:25-26` doc
  comment: update the peer-dispatch route references (the
  `/internal/dispatch/{processor,criteria}` → `/callout` collapse). No runtime
  caller depends on the old routes (verified: only the CORS prefix, which still
  matches, plus docs).
- **`docs/cloud-parity/scheduled-transitions.md`** (Gate 7): extend the
  twin-alignment contract with arm-time Function computation, the composite
  `Schedule` result, absolute/relative resolution → stored
  `scheduledTime`/`TimeoutMs`, born-expired (cancel prior row), and the
  503-retryable failure surface.
- **`docs/workflow-schema-versioning.md`** (Gate 4): `schedule.function` is
  additive accepted input ⇒ **MINOR** schema-version bump (loosening, not
  tightening — no previously-accepted shape is newly rejected; the XOR
  constrains only a shape the prior schema could not express). Record the
  rationale; every 1.x payload stays valid, dual-shape retention, `MaxMinor`
  widens in place.
- **`README.md`** scheduled-transitions section; **`CHANGELOG.md`** (Added);
  **`COMPATIBILITY.md`** (new SPI tag row). `config/scheduler.md` — **no change,
  no new env var** (the callout reuses the processor response-timeout path).

## 12. Error / status-code table per endpoint

Two HTTP surfaces are affected: **workflow import** (validation) and **entity
write / transition** (arm-time dispatch). gRPC entry points mirror both.

### Workflow import (`POST /api/model/{entityName}/{modelVersion}/workflow/import`)

| Scenario | Status | Code |
|---|---|---|
| Valid `schedule.function` | 200 | — |
| `function` + `delayMs` both present | 400 | `VALIDATION_FAILED` |
| `function` + `manual:true` | 400 | `VALIDATION_FAILED` |
| `resultKind` ≠ `Schedule` | 400 | `VALIDATION_FAILED` |
| Missing `name` / `calculationNodesTags` | 400 | `VALIDATION_FAILED` |

### Entity write / transition (arm-time dispatch)

| Scenario | Status | Code |
|---|---|---|
| Function returns valid `Schedule`, task armed | 2xx | — |
| No compute member for tags | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` (retryable) |
| Callout timed out | 503 | `DISPATCH_TIMEOUT` (retryable) |
| Peer forward failed / member disconnected | 503 | `DISPATCH_FORWARD_FAILED` / `COMPUTE_MEMBER_DISCONNECTED` (retryable) |
| Malformed / wrong-kind result | 500 | `SCHEDULE_FUNCTION_INVALID_RESULT` (+ticket) |
| Resolved expiry ≤ fire (born expired) | 2xx | — (not armed; `SCHEDULED_TRANSITION_EXPIRE`) |
| Past `fireAt` | 2xx | — (due now) |
| Explicit manual fire of a `schedule.function` transition | 400 | `TRANSITION_NOT_FOUND` (inherited, §14 E1) |

**Reconciled (uniform infra-503) — behavior change on existing processor/criterion paths.**
Each flips from `400 WORKFLOW_FAILED` → the documented `503 retryable` code, for
processor and FUNCTION-criterion alike (making code match the already-503 docs):

| Infra condition | Was | Now |
|---|---|---|
| No compute member for tags | 400 `WORKFLOW_FAILED` | 503 `NO_COMPUTE_MEMBER_FOR_TAG` |
| Callout timeout | 400 `WORKFLOW_FAILED` | 503 `DISPATCH_TIMEOUT` |
| Peer forward failed | 400 `WORKFLOW_FAILED` | 503 `DISPATCH_FORWARD_FAILED` |
| Member disconnected mid-request | 400 `WORKFLOW_FAILED` | 503 `COMPUTE_MEMBER_DISCONNECTED` |

## 13. Coverage matrix (scenario × layer)

Per `.claude/rules/test-coverage.md`. E2E needs a **fake Function-serving compute
node** (extend the existing gRPC test member that serves processor/criterion
responses).

| Scenario | unit | e2e (postgres) | parity (mem/sqlite/pg + commercial) | gRPC envelope |
|---|---|---|---|---|
| Import: valid `schedule.function` | ✓ | ✓ | | |
| Import: `function`+`delayMs` → `VALIDATION_FAILED` | ✓ | ✓ | | ✓ |
| Import: `function`+`manual` → `VALIDATION_FAILED` | ✓ | ✓ | | |
| Import: `resultKind≠Schedule` / missing name/tags | ✓ | ✓ | | |
| Arm: absolute `fireAt` / relative `fireAfterMs` | ✓ | ✓ | ✓ | |
| Arm: `expireAt` / `expireAfterMs` / no-expiry | ✓ | ✓ | ✓ | |
| Function no-member → 503 `NO_COMPUTE_MEMBER_FOR_TAG` | ✓ | ✓ | | ✓ |
| Function callout timeout → 503 `DISPATCH_TIMEOUT` | ✓ | ✓ | | ✓ |
| Malformed → 500 `SCHEDULE_FUNCTION_INVALID_RESULT` | ✓ | ✓ | | ✓ |
| Born expired (expiry ≤ fire) → not armed, write ok | ✓ | ✓ | ✓ | |
| Past `fireAt` → due now → fires | ✓ | ✓ | ✓ | |
| Expiry elapsed (+grace) → Expired, no fire | ✓ | ✓ | ✓ | |
| Re-arm on plain data update recomputes (callout each save) | ✓ | ✓ | ✓ | |
| Born-expired on re-arm cancels the prior armed row (no stale fire) | ✓ | ✓ | ✓ | |
| Background fire into unavailable-function state → ERROR log + backoff retry | ✓ | ✓ | | |
| **Infra-503 reconciliation — net-new coverage** (no existing test asserts no-member→400 at HTTP; existing 400 tests are domain-rejection, which stays 400): | | | | |
| · Processor no-member/timeout/forward-fail/disconnect → 503 | ✓ | ✓ | | ✓ |
| · FUNCTION-criterion no-member/timeout/forward-fail/disconnect → 503 | ✓ | ✓ | | ✓ |
| Cluster: no local member → forwards to peer with tag (sentinel preserved) | ✓ | ✓ (isolated multi-node) | | |
| Cluster: peer forward fails → 503 `DISPATCH_FORWARD_FAILED` (not 400) | ✓ | ✓ (isolated multi-node) | | ✓ |
| Explicit fire → 400 `TRANSITION_NOT_FOUND` (unchanged) | | ✓ | | ✓ |
| Concurrent writes race re-arm → one consistent armed task | | ✓ (isolated, **not** parity) | | |
| **Dispatch unification behavior-preserving** (processor+criteria unchanged) | existing `dispatch_test.go`/`streaming_test.go`/cluster tests stay green | ✓ | | ✓ |

## 14. Independent-review findings & resolutions

Four fresh-context reviews (unbiased) verified the design against the code. Each
round reviewed the committed spec and caught earlier "resolutions" that did not
hold against the code (born-expired stale-fire; miscited out-of-tx audit; the
503 mint in the wrong layer; half-uniform infra taxonomy); all are corrected below.

### Round 1 (design)

- **A1 — error codes flattened to 400.** `classifyWorkflowError` has no
  `AppError` passthrough. **Resolved:** §7.1.
- **A2 — mixed relative-fire + absolute-expiry time-bomb.** **Resolved:** §5.3
  born-expired rule — *but see R2-A1, the first form was incomplete*.
- **A3 — background arm failure unobservable.** **Superseded by R2-A2** (the
  proposed durable out-of-tx audit did not exist).
- **B1 — criterion vs Function HTTP inconsistency.** **Resolved:** §7.1 (both 503).
- **B2/B3/B4 — 502 novel; 5xx genericization; grace slop; write-path coupling.**
  **Resolved:** dropped 502 (§7.2); grace slop documented (§5.2); coupling +
  lower default timeout (§5.1, §6).
- **B6/D1 — naming.** One term `resultKind`; distinct wire-type name (§4.4).
- **C1 — generic Function YAGNI.** **Kept** by explicit product direction, minus
  a registry (§4.4).
- **C2 — trim config.** Dropped `retryPolicy`; **kept `attachEntity`** (§5.1).

### Round 2 (committed spec — verified against code)

- **R2-A1 (blocking) — born-expired left a stale row that would fire.** Merely
  omitting a born-expired transition from the arm set leaves its prior row live
  (`ReconcileForEntity` only cancels `SourceState != CurrentState`) → silent
  stale-fire. **Resolved:** §5.3 now explicitly cancels the prior row via an
  additive `ReconcileRequest` cancel-set (§9).
- **R2-A2 (blocking) — §8 durable out-of-tx audit did not exist and miscited
  `f03ecc5`** (which *preserved* the transactional-audit invariant). **Resolved:**
  §8 now uses ERROR-log + backoff + metrics (decision: right layer, no new
  infra); no new audit event.
- **R2-A3 (blocking) — omitted multi-node `DispatchFunction` forwarding.**
  **Resolved:** §4.2 dispatch unification designs the shared single-node **and**
  cluster path; the cluster peer wire collapses to one generic
  `/internal/dispatch/callout` route that Function rides (pre-1.0, no peer-wire
  back-compat scaffolding).
- **R2-B1 — §7.1 passthrough alone produces nothing; the 503 must be *minted*.**
  **Partly resolved, then corrected in R3-A1** — the mint could not live in the
  shared primitive.
- **R2-B2 — existing `delayMs > 0` check rejects a function-only schedule.**
  **Resolved:** §5.1 makes it conditional.
- **R2 nits:** node error carries no code → engine-assigned + `resp.Retryable`
  (§4.3); SPI cost is the implementor fan-out (§9); `function`+`manual` uses the
  inherited generic message (§5.1).

### Round 3 (committed spec — verified against code)

- **R3-A1 (blocking) — "mint once in the §4.2 shared primitive" is impossible and
  breaks cluster forwarding.** The primitive receives an already-resolved member,
  so it never sees the dominant no-member case (decided in the wrapper); and
  minting a 503 at the wrapper defeats `isNoMatchingMember` → cluster stops
  forwarding to a peer that has the tag. **Resolved:** §7.1 reworked — preserve
  `ErrNoMatchingMember`, mint the 503 at the topology-terminal no-member point
  (single-node classifier mapping; cluster `findPeerWithPolling`) and in the
  primitive for timeout/transport.
- **R3-A2 (blocking) — §7.1 was self-contradictory on the processor path** (mint
  in the shared primitive would flip *processor* mainline 400→503 untested, with
  no code, breaking `TestErrCode_Parity`). **Resolved by the (a) decision:** the
  reconciliation is now *deliberately* uniform (processor + criterion + function),
  reusing the **existing** `NO_COMPUTE_MEMBER_FOR_TAG` code, with the processor +
  criterion 400→503 flips owned explicitly in §12/§13.
- **R3-B1 — tx-gate suspend must be scoped, not deferred.** **Resolved:** §6.
- **R3-B2 — born-expired cancel-set must be excluded from `cancelled`.** **Resolved:** §5.3.
- **R3-B3 — SPI reconcile godoc + conformance case.** **Resolved:** §9.
- **R3-B4 — `DispatchFunction` implementor fan-out.** **Enumerated:** §9.
- **R3-B5 — `ARCHITECTURE.md` + `scheduler_rpc.go` doc.** **Added:** §11.
- **R3-B6 — `NO_COMPUTE_MEMBER_FOR_TAG` already exists.** **Resolved:** §7 reuses
  it; `SCHEDULE_FUNCTION_UNAVAILABLE` dropped.
- **R3-D nits:** `Retryable` is `*bool` (nil ⇒ non-retryable, §4.3); a past-`fireAt`
  + short expiry can arm-then-expire-by-scan (§5.2); minted-AppError detail logged
  on the background path is client-safe (Operational message).

### Round 4 (committed spec — verified against code)

- **R4-A1 (blocking) — "uniform 503" was only half-uniform.** The runtime has
  **four** compute-infra codes (`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`,
  `DISPATCH_FORWARD_FAILED`, `COMPUTE_MEMBER_DISCONNECTED`), all **already
  documented 503** but **none minted** (string prefixes in `fmt.Errorf` →
  `400 WORKFLOW_FAILED`). The round-3 draft fixed only `NO_COMPUTE_MEMBER_FOR_TAG`,
  mislabeled timeouts under it, and left the cluster peer-forward path (which the
  new Function uses multi-node) at 400. **Resolved (decision: full four-code
  reconciliation):** §7 mints all four as `503` at their emit sites, fixing the
  pre-existing doc/code drift. (Paul: fix all deficiencies found; blast radius is
  secondary.)
- **R4-B2 — classifier passthrough must be `errors.As`, not a type assertion**
  (minted 503s are `%w`-wrapped). **Resolved:** §7.1.
- **R4-B3 — "update existing tests" for the 400→503 flip is actually net-new
  coverage** (no existing test asserts no-member→400 at HTTP; existing 400 tests
  are domain-rejection, which stays 400). **Resolved:** §13 wording.
- **R4-C1 — phase-1 unification is a large refactor bundled with the feature.**
  **Addressed:** decision #7 stages it as its own green-first reviewable commit(s)
  within the branch (Paul: not a separate PR).
- **R4-D1 — `DispatchFunction` `AddWarning` must be safe on the background fire
  path** (no request ctx). **Resolved:** §8.
- **Verified correct (R4 E1–E8):** the sentinel survives `%w` to the classifier
  end-to-end; gRPC shares the domain classifier (no bypass); tx-gate scoping
  sufficient (no audit hazard); born-expired cancel-set composes atomically in all
  three stores; `Delete` of a nonexistent id is harmless; `TestErrCode_Parity`
  satisfied with one new topic.

- **Verified correct (all rounds):** relative-`TimeoutMs` conversion algebra for
  pure-abs/pure-rel/mixed/past-fire (§5.2); born-expired atomicity across all three
  stores (§5.3); additive-MINOR schema bump (§11); manual-fire 400 & cascade-skip
  inherited via `Schedule != nil`; tx-gate suspend ports to the arm path (scoped,
  §6); retryable propagates on any status (`common/errors.go:236`);
  `ProcessingResponse` union + registry plumbing already type-agnostic; peer-wire
  collapse safe (peer-auth binds path, forwarder+handler change together; no
  runtime caller of old routes).

## 15. Decisions log

1. Function return semantic: generic typed `result` + `resultKind` (kept
   general); scheduling uses composite `Schedule` supporting **both** absolute
   and relative for fire and expiry.
2. Invoked on **every re-arm** (settled-interval; no entry/re-arm distinction).
3. Callout failure → **transaction fails** (fail closed). **All four**
   compute-infra codes (`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`,
   `DISPATCH_FORWARD_FAILED`, `COMPUTE_MEMBER_DISCONNECTED`) → **503 retryable**,
   reconciled uniformly across processor, criterion, and Function — minted at each
   emit site, fixing a pre-existing doc-says-503/code-says-400 drift. Sentinel
   `ErrNoMatchingMember` preserved for cluster forwarding; classifier uses
   `errors.As` (R3/R4). Flips shipped processor/criterion infra failures 400→503
   (owned in §12/§13). Malformed Function result → 500
   `SCHEDULE_FUNCTION_INVALID_RESULT` (the one new code).
4. `attachEntity` kept configurable, **default true**.
5. Expiry folded into the Function return (composite `Schedule`), stored as the
   existing relative `TimeoutMs`; `ScheduledTask` row shape unchanged.
6. Static `delayMs`/`timeoutMs` path retained unchanged for its use cases.
7. **Dispatch unification is incorporated directly** as the **first phase** of
   this work (not a separate PR): a behavior-preserving extraction of a shared
   callout-dispatch primitive, staged as its **own green-first reviewable
   commit(s) within the branch** — the cluster/`streaming`/`dispatch` tests must
   be green before any Function code lands, so the refactor is reviewable on its
   own even though it ships in the one PR. The cluster peer wire collapses to one
   generic route (per-type `/processor`+`/criteria` routes removed) — pre-1.0, no
   rolling-upgrade back-compat scaffolding for an internal wire.
8. Background arm-failure observability = **ERROR-log + backoff + metrics**, not
   a durable audit event (right layer; no new infra).
9. Born-expired on re-arm **cancels the prior armed row** (additive
   `ReconcileRequest` cancel-set), not just skip-arm.

## 16. Out of scope

- Additional Function `resultKind`s beyond `Schedule` (no wire/dispatch work
  needed when they arrive — only a new consumer).
- Imperative per-entity timer APIs (setting a timer outside the workflow
  definition).
- Changes to the static `delayMs`/`timeoutMs` semantics.
