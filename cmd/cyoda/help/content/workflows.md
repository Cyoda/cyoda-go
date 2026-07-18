---
topic: workflows
title: "workflows — state machine definitions"
stability: stable
see_also:
  - models
  - crud
  - grpc
  - search
  - workflows.schema-version
  - errors.TRANSITION_NOT_FOUND
  - errors.WORKFLOW_NOT_FOUND
  - errors.WORKFLOW_FAILED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED
  - errors.VALIDATION_FAILED
  - errors.MODEL_NOT_FOUND
  - errors.SCHEDULE_FUNCTION_INVALID_RESULT
---

# workflows

## NAME

workflows — workflow state machine definitions: states, transitions, processors, and criteria.

## SYNOPSIS

```
POST  /api/model/{entityName}/{modelVersion}/workflow/import
GET   /api/model/{entityName}/{modelVersion}/workflow/export
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). All endpoints require `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`.

## DESCRIPTION

A workflow definition is a named finite state machine attached to an entity model. Workflows are stored per model reference `(entityName, modelVersion)`. A model may have multiple workflow definitions; the engine selects the matching one per entity using the workflow-level `criterion` field evaluated at entity creation time. When no `criterion` matches, the engine uses the default built-in workflow.

The engine executes automatically after every entity write. It sets the initial state, evaluates automated transitions (cascade), and invokes processors on each transition. Manual transitions are triggered by the client via `PUT /entity/{format}/{entityId}/{transition}`.

The engine enforces a per-state visit limit of 10 by default (configurable via `WithMaxStateVisits`) and an absolute cascade depth limit of 100 to prevent infinite loops. Static cycle detection runs at import time.

## WORKFLOW SCHEMA

**WorkflowDefinition** (element of the `workflows` array in import):

```json
{
  "version": "1.2",
  "name": "prize-lifecycle",
  "desc": "State machine for Nobel Prize entities",
  "initialState": "NEW",
  "active": true,
  "annotations": { "roles": ["reviewer"], "label": "Prize lifecycle" },
  "criterion": null,
  "states": {
    "NEW": {
      "transitions": [
        {
          "name": "APPROVE",
          "next": "APPROVED",
          "manual": true,
          "annotations": { "ui": { "color": "green" } },
          "disabled": false,
          "criterion": null,
          "processors": [
            {
              "type": "externalized",
              "name": "notify-approval",
              "executionMode": "SYNC",
              "annotations": { "displayName": "Send approval email" },
              "config": {
                "attachEntity": true,
                "calculationNodesTags": "approval-service",
                "responseTimeoutMs": 30000,
                "retryPolicy": "",
                "context": ""
              }
            }
          ]
        },
        {
          "name": "AUTO_VALIDATE",
          "next": "VALIDATED",
          "manual": false,
          "disabled": false,
          "criterion": {
            "type": "simple",
            "jsonPath": "$.year",
            "operatorType": "EQUALS",
            "value": "2024"
          },
          "criterionAnnotations": { "displayName": "Year is 2024" },
          "processors": []
        }
      ]
    },
    "APPROVED": {
      "transitions": []
    },
    "VALIDATED": {
      "transitions": []
    }
  }
}
```

**WorkflowDefinition fields:**

- `version` — semver `MAJOR.MINOR` string identifying the workflow-import contract this definition was authored against. Validated strictly on import; stamped to the current contract version on export. See `cyoda help workflows schema-version` for the bump rules and current/supported list.
- `name` — string — unique within the model; the primary key for MERGE mode
- `desc` — string — optional human-readable description. Surfaced in the import audit log line (`workflow import applied` at `INFO`, or `workflow import resulted in zero workflows` at `WARN`) as part of the per-workflow `{name, desc}` digest, and round-tripped via export when non-empty. Use it to record change intent that operators reading logs can correlate without consulting the workflow JSON
- `initialState` — string — state assigned when the entity is first created; must exist in `states`
- `active` — boolean — when `false`, the engine skips this workflow during selection
- `criterion` — `Condition` JSON or `null` — evaluated against the entity at creation to select this workflow; `null` matches all entities
- `states` — object — map of state name → `StateDefinition`
- `annotations` — object or absent — optional client-owned metadata, stored and round-tripped (compacted) but never interpreted by the engine. Must be a JSON object; capped at 64 KB per field. Use for client concerns such as permitted roles, display labels, or UI hints. Two well-known optional keys, `displayName` and `description` (strings), are documented for renderer use (workflow visualisers, condition builders); the engine ignores them and the types are advisory, not enforced. All five workflow element types — workflow, state, transition, processor, and criterion via `criterionAnnotations` — share this same bag shape
- `criterionAnnotations` — object or absent — optional client-owned metadata attached to this workflow's `criterion` as a whole (see `annotations` above); a sibling field rather than embedded in the criterion, so the criterion blob keeps round-tripping byte-verbatim

**StateDefinition:**

- `transitions` — array of `TransitionDefinition` — may be empty
- `annotations` — object or absent — optional client-owned metadata (see WorkflowDefinition `annotations`); object-only, 64 KB cap, engine-opaque

## TRANSITIONS

**TransitionDefinition fields:**

- `name` — string — transition name; used by the client in `PUT /entity/{format}/{entityId}/{name}` and in engine cascade
- `next` — string — target state; must exist in `states`
- `manual` — boolean — `true` means the transition requires an explicit client request; `false` means the engine evaluates it automatically in cascade
- `disabled` — boolean — when `true`, the engine skips this transition entirely
- `criterion` — `Condition` JSON or `null` — evaluated before executing the transition; `null` means always matches; the same Condition DSL as search (see `search` topic)
- `criterionAnnotations` — object or absent — optional client-owned metadata attached to this transition's `criterion` as a whole (see WorkflowDefinition `annotations`); sibling field, engine-opaque
- `processors` — array of `ProcessorDefinition` — invoked sequentially on this transition
- `annotations` — object or absent — optional client-owned metadata (see WorkflowDefinition `annotations`); object-only, 64 KB cap, engine-opaque

## PROCESSORS

**ProcessorDefinition fields:**

- `type` — string — execution-location axis; see below for valid values
- `name` — string — logical processor name
- `executionMode` — string — execution mode; see valid values below
- `config` — `ProcessorConfig`
- `annotations` — object or absent — optional client-owned metadata (see WorkflowDefinition `annotations`); object-only, 64 KB cap, engine-opaque. Excluded from the gRPC `EntityProcessorCalculationRequest` sent to compute members — never delivered to external processor implementations

**Processor `type` (execution-location axis):**

- `"externalized"` (default when omitted) — dispatched via gRPC to a calculation node selected by `Config.calculationNodesTags`. This is the only execution location implemented today; all the `executionMode` semantics below apply to externalized processors.

The engine reserves the value `"internalized"` for an in-process execution location not yet implemented. Any transition that fires a processor with `type: "internalized"` is rejected at dispatch with `WORKFLOW_FAILED` (400) and the operator-visible error `processor X failed: execution type "internalized" is not yet implemented`. The reserved value is intentionally absent from the OpenAPI enum until the subtype lands; workflow authors who include it in import payloads will not be rejected at import, but their entities cannot transit past the affected step.

Any value other than `"internalized"` (including the empty string, the canonical `"externalized"`, and unknown values such as legacy `"scheduled"` or `"EXTERNAL"`) falls through to the `executionMode` dispatch path. This permissiveness will narrow in a future release; do not rely on it.

**Valid `executionMode` values (exhaustive):**

- `"SYNC"` — the engine dispatches the processor and blocks until a response is received; the entity write transaction remains open during the wait; processor failure (including timeout and `success=false` in the response) returns `errors.WORKFLOW_FAILED` (`400`) and the entity remains in the source state
- `"ASYNC_SAME_TX"` — same dispatch mechanics as `SYNC` (blocks inline, transaction stays open); failure semantics are identical to `SYNC`
- `"ASYNC_NEW_TX"` — dispatched within a savepoint; on failure the savepoint is rolled back and the error is logged as a warning; the pipeline continues to the next processor and the transition completes; returned entity modifications are discarded
- `"COMMIT_BEFORE_DISPATCH"` — the engine splits the cascade into two transactions around this processor. `TX_pre` flushes the pre-callout state of the transition and **commits before the processor is dispatched**, releasing the storage connection for the duration of the external compute. The processor runs outside any transaction (entity already durable in the pre-callout state). When the processor returns, the engine opens `TX_post` on the same node, reapplies the result via `CompareAndSave` (CAS expects the txID stamped at `TX_pre`'s commit), runs any subsequent SYNC processors and cascade transitions, then commits. CAS conflict at the boundary surfaces as `409 retryable`; entity remains durable in the pre-callout state, no engine-side retry, no automatic compensation. Failure of the dispatched processor (`success=false`, timeout, member crash) returns `errors.WORKFLOW_FAILED` (`400`) and the entity remains in the pre-callout state. Designed to relieve connection-pool pressure for slow processors and supersedes `ASYNC_NEW_TX` as the recommended mode for slow external work.

**`COMMIT_BEFORE_DISPATCH` configuration flag:**

- `startNewTxOnDispatch` — boolean — sibling field on the same processor object; default `false`; valid only when `executionMode == "COMMIT_BEFORE_DISPATCH"`. Validator rejects `true` for any other mode. When `true`, the engine opens a fresh transaction context (`TX_post`) for the dispatched processor's CRUD callbacks; the processor may use the supplied transaction token to read or write entities other than the cascade-anchor. When `false`, no transaction context is supplied to the dispatched call.

**`COMMIT_BEFORE_DISPATCH` workflow-author requirements:**

- **Idempotency.** A `COMMIT_BEFORE_DISPATCH` processor must be **idempotent or have an external mechanism for detecting prior completion** (e.g., a write-once external resource ID). Replays can fire from two distinct places: (a) CAS conflict during continuation — the caller's retry of the same API call restarts the cascade and re-dispatches the processor; (b) engine crash between segments — the entity is durable in the pre-callout state, the in-flight orchestration is gone, the caller retries, the cascade re-fires from the beginning, the processor is re-dispatched. The engine cannot deduplicate replays; idempotency is the workflow author's responsibility.
- **Visibility of segment-boundary states.** States on a segment boundary (the pre-callout state of a `COMMIT_BEFORE_DISPATCH` processor) are **publicly observable** to readers between segments. A concurrent transaction's `Get`/`GetAll`/`Search`/`Count` will see the entity in the pre-callout state, and a second cascade may decide to fire criteria-driven transitions based on that observed state. Workflow authors using `COMMIT_BEFORE_DISPATCH` must treat segment-boundary states as committed states — design state-machine criteria, transition guards, and external monitoring accordingly. If invisibility of an intermediate state is required, model it as a workflow-level `DRAFT` parent state with sub-stages in payload, or do not expose the entity until a designated terminal state.
- **Best-practice: a processor must not save the entity it is processing for.**
  Processors with TX-callback access (SYNC, ASYNC_SAME_TX, COMMIT_BEFORE_DISPATCH
  with startNewTxOnDispatch=true) can write the cascade-anchor entity via the
  supplied transaction token, but if they do AND also return mutations for the
  same entity in their result, the engine's apply-result will overwrite the
  processor's intra-TX writes (last-writer-wins inside the transaction buffer).
  Pick one path: let the engine apply the result, OR have the processor write
  the entity itself and return no mutations for it.

Import-time validation rejects any `executionMode` value not in the list above (and not empty) with `400 VALIDATION_FAILED`. The empty string continues to default to `SYNC` at engine fire.

**ProcessorConfig fields:**

- `attachEntity` — boolean — when `true`, the full entity payload is sent to the processor
- `calculationNodesTags` — string — comma-separated tags for routing to registered calculation nodes; the engine selects a node that declares all required tags; returns `errors.NO_COMPUTE_MEMBER_FOR_TAG` if no node matches
- `responseTimeoutMs` — int64 — timeout in milliseconds for `SYNC` processor response; `0` means use node default
- `retryPolicy` — string — selects the server-resolved retry strategy.
  Valid values: `NONE` (single attempt, no retry), `FIXED` (up to N
  additional attempts with fixed delay between tries, where N and delay
  are server-configured). When omitted, defaults to `FIXED` at engine
  fire. Import-time validation rejects any other value with `400
  VALIDATION_FAILED`. **cyoda-go status:** captured but not consumed —
  the dispatcher is single-shot regardless of policy; the full retry
  loop ships in a later release. Cloud honours both policies.
- `context` — string — pass-through string forwarded **verbatim** as the `parameters` JSON node of the outgoing `EntityProcessorCalculationRequest` (and `EntityCriteriaCalculationRequest` when used on a `function`-typed criterion's `config`). Marshalling shape is **pass-as-string**: the value is encoded as a JSON string, not parsed as JSON. The receiver gets a JSON-quoted string in `parameters`. Empty `context` causes `parameters` to be omitted entirely. Use to distinguish multiple workflow roles served by a single externalized processor or criterion implementation without registering a separate name per role.
- `asyncResult` — boolean (pointer; nil-default) — declared in the
  OpenAPI for Cloud parity; the runtime does **not** implement
  async-result semantics on this backend. Imports that set
  `asyncResult: true` are rejected with `400 VALIDATION_FAILED`. The
  explicit `asyncResult: false` and absent cases are accepted and
  round-tripped.
- `crossoverToAsyncMs` — int64 (pointer; nil-default) — crossover
  delay (ms) for the async-result semantic; declared in the OpenAPI
  for Cloud parity; the runtime does **not** implement it. Imports
  that set any non-nil value are rejected with `400 VALIDATION_FAILED`,
  including the orphan case where `asyncResult` is absent or false.

## SCHEDULED TRANSITIONS

A transition may carry an optional `schedule` object marking it as
scheduled. Presence of `schedule` declares that the transition fires
automatically at `scheduledTime = stateEntryTime + delayMs`. The
`schedule` shape is:

```json
{
  "name": "AutoClose",
  "next": "Closed",
  "manual": false,
  "schedule": {
    "delayMs": 86400000,
    "timeoutMs": 600000
  }
}
```

**Fields:**

- `delayMs` (integer, required) — delay between source-state entry
  and the scheduled execution time, in milliseconds. Must be `> 0`.
- `timeoutMs` (integer, optional) — late-tolerance window past the
  scheduled execution time, in milliseconds. When the scheduler
  picks the task up, if `(executionTime - scheduledTime) > timeoutMs`
  the task is dropped and the transition is NOT attempted. Absent
  (omitted) means no timeout — fire whenever the scheduler
  eventually picks it up. Explicit `0` is the strictest setting —
  drop on any lateness. Independent of `delayMs`; the two measure
  different quantities.

**Import-time validation rules:**

- `manual: true` and `schedule` present are mutually exclusive
  (`VALIDATION_FAILED`).
- `schedule.delayMs <= 0` is rejected (`VALIDATION_FAILED`).
- No further rule on `timeoutMs` beyond `>= 0` (enforced at the DTO
  boundary).

**Engine behaviour.** A scheduled transition fires automatically
`delayMs` after the entity enters the transition's source state — a
background scheduler arms a timer on entry and fires it when due,
independently of cascade evaluation and independently of any other
API call touching the entity.

- **Firing.** When the timer is due, the engine re-evaluates the
  transition's criterion exactly **once**. A `true` (or absent)
  criterion fires the transition normally (processors run, state
  advances, `TRANSITION_MAKE` is recorded). A `false` criterion
  **declines** the transition — the entity stays in its current
  state and the timer is **not** retried (`TRANSITION_NOT_MATCH_CRITERION`).
  See "One-shot vs. polling" below for how to model a retry.
- **Lateness (`timeoutMs`).** `timeoutMs` bounds how late the
  scheduler may pick up a due timer before giving up: if the timer
  is picked up more than `timeoutMs` past its scheduled time, it is
  **dropped** without evaluating the criterion (`Expired`) — the
  transition never fires and the entity stays put. Absent `timeoutMs`
  means no upper bound: the timer fires whenever it is eventually
  picked up.
- **Settled-interval reset.** Any write to the entity that leaves it
  in the same state — including an ordinary in-place data update or a
  self-loop — **resets the timer** to `delayMs` from that write. An
  entity written more often than its `delayMs` interval never reaches
  the scheduled fire. Authors relying on "escalate N after entry"
  semantics should account for this: routine touch-writes on a busy
  entity postpone escalation indefinitely.
- **Explicitly firing a scheduled transition by name still returns**
  HTTP 400 `TRANSITION_NOT_FOUND`, with the message `transition "X"
  in state "Y" is scheduled and fires automatically; it is not
  manually fireable`. Same code returned when a transition is
  `disabled: true` — same semantic: "the transition exists but is not
  currently dispatchable from the caller's POV." The entity remains
  in the source state. To allow early firing, give the state an
  ordinary manual transition alongside the scheduled one.
- **Audit trail.** Arming, firing, expiry, and cancellation (the
  entity leaving the source state before the timer fires) each emit a
  dedicated event: `SCHEDULED_TRANSITION_ARM`, `SCHEDULED_TRANSITION_FIRE`
  (alongside the ordinary `TRANSITION_MAKE`), `SCHEDULED_TRANSITION_EXPIRE`,
  `SCHEDULED_TRANSITION_CANCEL`. A loopback that re-arms the same state
  emits only `ARM`, not `CANCEL`.

**One-shot vs. polling.** The criterion is evaluated once per fire —
there is no built-in retry-until-true. Three shapes cover the common
cases:

- **Unconditional scheduled cycle** (`S1 →scheduled→ S2 →scheduled→
  S1`, no criteria) — an intentional recurring heartbeat: it fires
  every `delayMs`, forever, for every entity in the cycle. Requires
  `allowCycles: true` at import (below).
- **Conditional scheduled transition** (a criterion on the scheduled
  transition) — a one-shot deadline gate: "at the deadline, fire iff
  the condition holds, else abandon." A `false` criterion is a
  deliberate Decline, not a retry.
- **Poll-until-condition** — model it as an *unconditional* scheduled
  tick into a state whose **ordinary** (non-scheduled) transitions
  carry the condition and exit when it holds. The retry loop lives in
  normal workflow structure, not in the timer.

**Importing cyclic scheduled workflows.** A canonical scheduled-
transition use case is a polling pattern such as `S1 →scheduled→ S2
→scheduled→ S1`. The import-time cycle detector rejects unguarded
automated cycles by default — including this one, because a delayed
cycle is still a cycle. To import such a workflow, set the request-
level field `allowCycles: true` on the import body:

```json
{
  "importMode": "REPLACE",
  "allowCycles": true,
  "workflows": [ /* ... */ ]
}
```

`allowCycles: true` bypasses only the cycle-detection check. Schedule
shape rules (manual+schedule, delayMs) and all other validators
remain unconditional. The runtime cascade-depth and per-state visit
caps still catch actual runaway at fire time. Use only for workflows
whose cyclicity is intentional.

**Function-computed firing time (`schedule.function`).** In place of a
static `delayMs`, a transition can compute its own firing time (and
optionally an expiry) per entity via a Function callout — the same
dispatch mechanism as an externalized processor or a `function`-type
criterion, but returning a typed `Schedule` result instead of an
entity payload or a boolean. `delayMs` and `function` are mutually
exclusive: exactly one is required whenever `schedule` is present
(both are also mutually exclusive with `manual: true`).

```json
{
  "name": "Escalate",
  "next": "Escalated",
  "manual": false,
  "schedule": {
    "function": {
      "name": "compute-escalation-time",
      "resultKind": "Schedule",
      "calculationNodesTags": "escalation-service",
      "attachEntity": true
    }
  }
}
```

- `name` (string, required) — registered function name.
- `resultKind` (string, required) — must be `"Schedule"`.
- `calculationNodesTags` (string, required) — comma-separated tags
  selecting the dispatch target, same as a processor or criterion.
- `attachEntity` (boolean, optional, default `true`) — whether the
  entity payload is attached to the request.
- `context` (string, optional) — pass-through string forwarded
  verbatim as the request's `parameters`. Omitted when empty.
- `responseTimeoutMs` (integer, optional) — response timeout for this
  callout.

**Invocation.** The Function is called at arm time — synchronously,
inside the entity-write transaction — on every re-arm: the initial
entry into the source state, and every subsequent settled write that
leaves the entity in that state (the same trigger as the
settled-interval reset described above for `delayMs`). Each call
fully replaces the previous scheduling decision for that transition.

**Fail-closed.** If the compute node is unreachable, disconnected, or
times out, the triggering entity write fails as a retryable `503` —
the same dispatch error codes as a processor or criterion callout
(e.g. `NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`,
`COMPUTE_MEMBER_DISCONNECTED`). No state change commits against an
unschedulable transition. A structurally valid response with the
wrong `resultKind`, or a malformed `Schedule` value, is a `500`
`SCHEDULE_FUNCTION_INVALID_RESULT` instead of a dispatch failure — see
that error topic.

**The `Schedule` result.** The function responds with `resultKind:
"Schedule"` and a `result` object shaped:

```json
{ "fireAfterMs": 3600000, "expireAfterMs": 600000 }
```

- Exactly one of `fireAt` (absolute, epoch-ms) or `fireAfterMs`
  (relative to arm time) is required.
- At most one of `expireAt` (absolute) or `expireAfterMs` (relative to
  the *resolved fire time*, not arm time) may be present; both absent
  means no expiry, matching an absent `timeoutMs` for `delayMs`.
- A past `fireAt` (or non-positive `fireAfterMs`) is not an error —
  the transition is due immediately.
- A resolved expiry at or before the resolved fire time is **born
  expired**: the transition is not armed, any existing scheduling for
  it is cancelled, a `SCHEDULED_TRANSITION_EXPIRE` audit event is
  recorded, and the triggering write still succeeds.
- A resolved expiry after the fire time becomes `timeoutMs` (the gap
  between the two), applying the same late-tolerance semantics
  described above for `schedule.timeoutMs`.

## CRITERIA

Criteria on workflows and transitions use the same `Condition` DSL as search. All four condition types are supported: `simple`, `lifecycle`, `group`, `array`. Criteria are evaluated in-memory against the entity's JSON payload and lifecycle metadata.

`simple` criteria match entity data fields via JSONPath. `lifecycle` criteria match `state`, `creationDate`, or `previousTransition` from entity metadata.

A `null` criterion on a workflow means the workflow matches any entity. A `null` criterion on a transition means the transition always fires (automated) or is always available (manual). When multiple automated transitions are eligible, the engine selects the first one by declaration order whose criterion matches. A `null` criterion matches unconditionally, so a `null`-criterion automated transition must be the last automated transition in declaration order; any automated transitions declared after a `null`-criterion transition are unreachable.

### Workflow-level selection

When a model has more than one imported workflow definition, the engine picks the workflow per entity at execution time using these rules — applied in order on every `Execute` / `ManualTransition` / `Loopback` (no caching across calls):

1. Iterate workflows in their stored declaration order. (Storage preserves the order from the most recent import; MERGE inserts new workflows at the tail.)
2. Skip any workflow whose `active` flag is `false`. Inactive workflows are invisible to selection, regardless of their criterion.
3. For each active workflow, evaluate `criterion` against the entity payload and lifecycle metadata. A `null` (absent) criterion matches unconditionally — the workflow is selected immediately.
4. The first active workflow whose criterion matches is selected. Subsequent workflows in the array are not consulted.
5. If no active workflow matches — which includes the case where every active workflow has a criterion and none of them passes — the engine falls back to the embedded **default workflow**. The substitution surfaces on two channels: a body warning via `AddWarning` and an operator-visible `slog.Warn` line (`reason=no_criterion_matched`).

Place a `null`-criterion (or otherwise unconditional) workflow last in the import array if you want it to act as a catch-all. Any active workflows declared after it are unreachable for the same reason an unguarded automated transition shadows successors at the transition level.

Workflow-level selection is independent of transition-level selection: once a workflow is chosen, the engine then applies the transition-evaluation rules above against that workflow's `states` map.

## IMPORT REQUEST

**POST /api/model/{entityName}/{modelVersion}/workflow/import**

- `entityName` (path): string
- `modelVersion` (path): int32

Request body (`application/json`):

```json
{
  "importMode": "MERGE",
  "workflows": [
    { ...WorkflowDefinition... }
  ]
}
```

- `importMode` — `"MERGE"` (default): incoming workflows overwrite existing ones by name; existing workflows not in the import are preserved. `"REPLACE"`: all existing workflows are discarded; only the incoming set is stored. `"ACTIVATE"`: incoming workflows replace same-named existing ones; existing workflows not in the import set are kept but flipped `active=false`. `REPLACE` / `ACTIVATE` reject an empty `workflows` array (or a missing `workflows` key) with `400 VALIDATION_FAILED` — once a model has imported workflows it always carries ≥1; the built-in default workflow is only used when no workflow has ever been imported. `MERGE` with an empty `workflows` array is allowed as a no-op. The field defaults to `MERGE` when omitted or empty. Parsing is **case-insensitive**: `"merge"`, `"Merge"`, and `"MERGE"` are equivalent. Any value outside the documented enum (after case-folding) is rejected with `400 BAD_REQUEST`.
- `workflows` — array of `WorkflowDefinition`. The `active` flag on each incoming workflow is preserved as supplied; the server never overrides it. If the field is absent (or explicitly `null`), it defaults to `true`. Controlling which workflows are active is entirely up to the importer.

Static validation runs on the incoming request before saving. Any of the following returns `400 VALIDATION_FAILED` with the offending workflow / state / transition named in `detail`:

- Definite infinite loops — cycles reachable only via unguarded automated transitions.
- Empty workflow `name`, or two workflows in the same request sharing a `name`.
- Empty `initialState`, or `initialState` not declared in `states`.
- Empty state-map key (i.e. `"states": { "": { … } }`).
- Empty or duplicate transition `name` within a single state.
- Empty processor `name`.
- Workflow / state / transition / processor names longer than 256 characters.
- Transition `next` not declared in `states`.
- Unknown `executionMode` value on any processor (allowed: `SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX`, `COMMIT_BEFORE_DISPATCH`, or empty).
- Unknown `retryPolicy` value on any processor (allowed: `NONE`, `FIXED`, or empty).
- `startNewTxOnDispatch=true` on a processor whose `executionMode` is not `COMMIT_BEFORE_DISPATCH`.
- Empty `workflows` array (or a missing `workflows` key) when `importMode` is `REPLACE` or `ACTIVATE`. `MERGE` with an empty array is a legitimate no-op.

The new structural rules (state graph, name uniqueness, `executionMode` enum, `retryPolicy` enum) run on the incoming request only — existing stored workflows are not retroactively re-checked against them. The cycle-detection and `startNewTxOnDispatch` coherence checks continue to run against the merged result, so a legacy stored cycle or incoherent flag still surfaces at any subsequent import.

Response: `200 OK`, `application/json`:

```json
{"success": true}
```

### Audit log on success

Every successful import emits a single structured `log/slog` line so operators can correlate workflow-config changes in their log pipeline.

- **Normal path** — `level=INFO`, `msg="workflow import applied"`. Fields: `pkg=workflow`, `tenant`, `entityName`, `modelVersion`, `importMode`, `workflowCount` (size of THIS call's incoming payload), `storedWorkflowCount` (model's post-merge total), `workflows` (array of `{name, desc}` reflecting the incoming payload — the audit subject is what was applied, not the resulting model state).
- **Zero-result canary** — `level=WARN`, `msg="workflow import resulted in zero workflows"`, same field shape. After `REPLACE` / `ACTIVATE` empty became a `400 VALIDATION_FAILED` (see above), the only reachable path is a `MERGE` with an empty `workflows` array against a model that has no prior workflows. The model will then silently fall back to the embedded default on the next entity execution; this canary surfaces that outcome before it shows up in entity-execution logs.

The `desc` field on each workflow is surfaced in the audit log digest, truncated to 200 characters with a `...` suffix when longer — set a meaningful description to record change intent that log readers can correlate without consulting the workflow JSON.

## EXPORT RESPONSE

**GET /api/model/{entityName}/{modelVersion}/workflow/export**

Response: `200 OK`, `application/json`:

```json
{
  "entityName": "nobel-prize",
  "modelVersion": 1,
  "workflows": [
    { ...WorkflowDefinition... }
  ]
}
```

Returns `404 WORKFLOW_NOT_FOUND` when no workflows have been imported for the model.

**Export field omission:** The export response omits optional fields that were not explicitly set or are default values. Specifically, `TransitionDefinition` objects in the export may omit `disabled` (when `false`) and `processors` (when empty). States with no transitions are serialised as `{}` rather than `{"transitions":[]}`. The `desc` field on `WorkflowDefinition` is omitted when empty. `annotations` (on the workflow, any state, any transition, or any processor) and `criterionAnnotations` (on the workflow or any transition) are omitted when absent, and re-serialised in compacted form when present.

## ENGINE EXECUTION

The workflow engine runs synchronously within the entity write transaction. The execution sequence for a CREATE:

1. Load workflow definitions for the model.
2. Evaluate each workflow's `criterion` against the entity; select the first match. If none match (or if no workflows have been imported for the model), use the built-in default workflow. The substitution emits both a `slog.Warn` line (fields: `pkg=workflow`, `tenant`, `entityName`, `modelVersion`, `entityId`, `reason=no_workflows_imported`|`no_criterion_matched`) and an `AddWarning` entry surfaced in the response body, so operators can detect models silently running on the default.
3. Set `entity.Meta.State = workflow.initialState`.
4. If a named transition was requested (by the client), execute it: evaluate `criterion`, invoke processors, set `entity.Meta.State = transition.next`.
5. Cascade: repeatedly scan the current state's transitions; for each automated (`manual=false`) non-disabled transition, evaluate `criterion`; if it matches, invoke processors and advance the state. Stop when no automated transition matches or the state has no automated transitions.
6. The engine records `StateMachineEvent` entries to the audit log under the entity's `transactionId`.

Per-state visit limit (default 10) and total cascade depth limit (100) are enforced to prevent infinite loops.

## ERRORS

- `errors.TRANSITION_NOT_FOUND` — `404` — named transition does not exist in the current state's workflow
- `errors.WORKFLOW_NOT_FOUND` — `404` — no workflows found for the model (export endpoint)
- `errors.WORKFLOW_FAILED` — workflow engine encountered an unrecoverable error during execution
- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — no registered calculation node matches the required `calculationNodesTags`
- `errors.COMPUTE_MEMBER_DISCONNECTED` — a calculation node disconnected during processor dispatch
- `errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` — `400` — workflow declares a schema version this server does not accept
- `errors.VALIDATION_FAILED` — `400` — workflow import validation failed; see IMPORT REQUEST above for the enumerated rules

## EXAMPLES

**Import a workflow:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "importMode": "MERGE",
    "workflows": [
      {
        "version": "1.2",
        "name": "prize-lifecycle",
        "initialState": "NEW",
        "active": true,
        "states": {
          "NEW": {
            "transitions": [
              {
                "name": "APPROVE",
                "next": "APPROVED",
                "manual": true,
                "processors": []
              }
            ]
          },
          "APPROVED": {
            "transitions": []
          }
        }
      }
    ]
  }' \
  "http://localhost:8080/api/model/nobel-prize/1/workflow/import"
```

**Export workflows:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/model/nobel-prize/1/workflow/export"
```

**Trigger a manual transition:**

```
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"category":"physics","year":"2024"}' \
  "http://localhost:8080/api/entity/JSON/74807f00-ed0d-11ee-a357-ae468cd3ed16/APPROVE"
```

**Replace all workflows:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "importMode": "REPLACE",
    "workflows": [
      {
        "version": "1.2",
        "name": "simple-wf",
        "initialState": "OPEN",
        "active": true,
        "states": {
          "OPEN": { "transitions": [] }
        }
      }
    ]
  }' \
  "http://localhost:8080/api/model/nobel-prize/1/workflow/import"
```

## SEE ALSO

- models
- crud
- grpc
- search
- errors.TRANSITION_NOT_FOUND
- errors.WORKFLOW_NOT_FOUND
- errors.WORKFLOW_FAILED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED
- errors.VALIDATION_FAILED
- errors.MODEL_NOT_FOUND
