---
topic: workflows
title: "workflows — state machine definitions"
stability: stable
see_also:
  - models
  - crud
  - grpc
  - search
  - errors.TRANSITION_NOT_FOUND
  - errors.WORKFLOW_NOT_FOUND
  - errors.WORKFLOW_FAILED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.COMPUTE_MEMBER_DISCONNECTED
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
  "version": "1",
  "name": "prize-lifecycle",
  "desc": "State machine for Nobel Prize entities",
  "initialState": "NEW",
  "active": true,
  "criterion": null,
  "states": {
    "NEW": {
      "transitions": [
        {
          "name": "APPROVE",
          "next": "APPROVED",
          "manual": true,
          "disabled": false,
          "criterion": null,
          "processors": [
            {
              "type": "externalized",
              "name": "notify-approval",
              "executionMode": "SYNC",
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

- `version` — string — schema version tag (informational; not interpreted by the engine)
- `name` — string — unique within the model; the primary key for MERGE mode
- `desc` — string — optional description
- `initialState` — string — state assigned when the entity is first created; must exist in `states`
- `active` — boolean — when `false`, the engine skips this workflow during selection
- `criterion` — `Condition` JSON or `null` — evaluated against the entity at creation to select this workflow; `null` matches all entities
- `states` — object — map of state name → `StateDefinition`

**StateDefinition:**

- `transitions` — array of `TransitionDefinition` — may be empty

## TRANSITIONS

**TransitionDefinition fields:**

- `name` — string — transition name; used by the client in `PUT /entity/{format}/{entityId}/{name}` and in engine cascade
- `next` — string — target state; must exist in `states`
- `manual` — boolean — `true` means the transition requires an explicit client request; `false` means the engine evaluates it automatically in cascade
- `disabled` — boolean — when `true`, the engine skips this transition entirely
- `criterion` — `Condition` JSON or `null` — evaluated before executing the transition; `null` means always matches; the same Condition DSL as search (see `search` topic)
- `processors` — array of `ProcessorDefinition` — invoked sequentially on this transition

## PROCESSORS

**ProcessorDefinition fields:**

- `type` — string — execution-location axis; see below for valid values
- `name` — string — logical processor name
- `executionMode` — string — execution mode; see valid values below
- `config` — `ProcessorConfig`

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
- `retryPolicy` — string — retry policy name (plugin/platform-defined); empty means no retry
- `context` — string — pass-through string forwarded **verbatim** as the `parameters` JSON node of the outgoing `EntityProcessorCalculationRequest` (and `EntityCriteriaCalculationRequest` when used on a `function`-typed criterion's `config`). Marshalling shape is **pass-as-string**: the value is encoded as a JSON string, not parsed as JSON. The receiver gets a JSON-quoted string in `parameters`. Empty `context` causes `parameters` to be omitted entirely. Use to distinguish multiple workflow roles served by a single externalized processor or criterion implementation without registering a separate name per role.

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
- `startNewTxOnDispatch=true` on a processor whose `executionMode` is not `COMMIT_BEFORE_DISPATCH`.
- Empty `workflows` array (or a missing `workflows` key) when `importMode` is `REPLACE` or `ACTIVATE`. `MERGE` with an empty array is a legitimate no-op.

The new structural rules (state graph, name uniqueness, `executionMode` enum) run on the incoming request only — existing stored workflows are not retroactively re-checked against them. The cycle-detection and `startNewTxOnDispatch` coherence checks continue to run against the merged result, so a legacy stored cycle or incoherent flag still surfaces at any subsequent import.

Response: `200 OK`, `application/json`:

```json
{"success": true}
```

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

**Export field omission:** The export response omits optional fields that were not explicitly set or are default values. Specifically, `TransitionDefinition` objects in the export may omit `disabled` (when `false`) and `processors` (when empty). States with no transitions are serialised as `{}` rather than `{"transitions":[]}`. The `desc` field on `WorkflowDefinition` is omitted when empty.

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
        "version": "1",
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
        "version": "1",
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
