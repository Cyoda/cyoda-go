# Discriminator mappings, and typed state-machine event data

Issue #625. Tracker #651, row 8. Base: `release/v0.9.0`. Research:
`docs/superpowers/research/2026-10-14-625-state-machine-event-data-research.md`.

Standing rulings that bind this spec: a contract gap is never fixed by removing API function; cyoda-go's
event-type names are kept and Cloud renames its values; a value cyoda-go does not truly have is never sent
(no substituted or made-up value); there are no production instances, so a breaking change is declared, not
avoided.

## 1. Terms

- **Discriminator** — the property of a polymorphic object that says which shape the object has, such as `type`
  on a condition.
- **Mapping** — the `discriminator.mapping` table: for each value of the discriminator, the schema of that shape.
  Without one, OpenAPI defines the value as the schema's name (`SimpleConditionDto`), which is not what the wire
  carries.
- **Union** — a schema with a `oneOf` list of members and a discriminator that picks the member.
- **Base** — a schema that members include with `allOf`.
- **Event** — one state-machine audit event (`StateMachineAuditEventDto`). Its **type** is `eventType`. Its
  **data** is the `data` member.
- **Stop reason** — why a state-machine run stopped: `WorkflowStopReasonDto`.
- **Member text** — a string a compute member sends in its answer: a failure message, a criterion reason, a
  warning, a scheduled function's result kind.

## 2. Rules

**Rule 1 — every discriminator has a mapping, and a mapping names only members.**

- A union's mapping lists every member, and every mapped schema is a member of the union. The mapping `$ref`
  is spelled exactly as the `oneOf` item `$ref`.
- A base with a discriminator lists, in its mapping, every schema in the spec that includes it with `allOf`.
  A base whose includers would share a discriminator value has no discriminator.

**Rule 2 — every event has typed data.** `data` is a union keyed on `eventType`. It is never `null` and never
absent. The event type is repeated inside `data`, as Cloud does: a union can only be told apart by a property
inside the object.

**Rule 3 — data states only what is true.** A field is present when the engine knows its value at the point the
event is recorded, and absent otherwise. Nothing is filled with a placeholder.

**Rule 4 — an audit event is written on every backend or on none.** The PostgreSQL audit row is a `jsonb`
document written inside the request's transaction (`plugins/postgres/sm_audit_store.go:31-36`). PostgreSQL
refuses U+0000 in `jsonb`, and the refused `INSERT` aborts the whole transaction; memory and SQLite store the same
event. Recording is best effort by design — a failed record is logged and the operation goes on
(`internal/domain/workflow/engine.go:1101-1115`) — so the engine refuses such an event itself, before any
backend sees it (section 8.2). With `failReason` (section 4) member text reaches `data` on a new path, which
makes this divergence easier to reach; it already exists for a criterion reason and a caller's transition name.

## 3. Wire contract (`api/openapi.yaml`)

### 3.1 Condition bases

- `AbstractConditionDto` loses its discriminator. `GroupConditionDto` and `CriterionGroupConditionDto` both
  include it and both use `type: group`, so no single mapping is true (Rule 1). It stays a base that requires
  `type`.
- `QueryConditionDto` loses its discriminator for the same reason: it is a base with one includer, and the
  condition unions already map `function` to `FunctionConditionDto`.
- The conditional `DELETE /entity/{entityName}/{modelVersion}` body becomes the inline local-condition union
  (`oneOf` Array, Group, Lifecycle, Simple with the `type` mapping), as the search bodies are. Its descriptions
  stop naming `AbstractConditionDto` (`api/openapi.yaml:2199`, `:2353`). `api/condition_union_test.go:56-99`
  gains the DELETE body as a row.
- No `enum` is added to a condition `type`. The mappings state the set; an enum on a base would reach every
  request body that includes it.

### 3.2 Audit event base

- `AuditEventDto` keeps its discriminator and gains the mapping `EntityChange`, `StateMachine`, `System`, the
  same as the inline union in `EntityAuditEventsResponseDto`.

### 3.3 State-machine event union

- `StateMachineEventDto` (the base) keeps its discriminator on `eventType` and gains a mapping of all 19 event
  types (section 4) to their schemas.
- `StateMachineAuditEventDto.data` becomes an inline union:

  ```yaml
  data:
    type: object
    oneOf: [ <19 $refs> ]
    discriminator:
      propertyName: eventType
      mapping: { STATE_MACHINE_START: …, …, SCHEDULED_TRANSITION_FAIL: … }
  ```

  `type` changes from `[object, "null"]` to `object`: oapi-codegen cannot generate a union with the `null` type,
  and with no `type` at all oasdiff reports a type change on both audit endpoints. The `oneOf` cannot sit on
  `StateMachineEventDto`, because every member includes it (a cycle). `data` is required. `x-go-type` is removed
  so oapi-codegen generates the union type; the server keeps building the response as a map (section 6).
  `TestSpecDeclaresNoNullableKeyword` (`api/condition_dto_test.go:208-210`), which asserts that `data` accepts
  null, changes to assert that it does not.
- The outer `eventType` description (`api/openapi.yaml:11667-11674`) stops describing TRANSITION_ABORTED's data
  (it names `transitionName`); the data is the member schema's.
- `TRANSITION_NOT_MATCH_CRITERION`'s `criterion` description becomes what the value is: the function's name when a
  function condition is the whole criterion, otherwise the condition's type (`simple`, `group`, …)
  (`engine.go:135-155`; `docs/cloud-parity/criterion-stoppage-reason.md:67`).
- Generated-name side effect: the new enums (`WorkflowStopReasonDto.type` already has `FORCE_SUCCESS`; the
  `executionMode` enum) collide with existing constants, so oapi-codegen prefixes the
  `StateMachineAuditEventDtoEventType` and `ExternalizedProcessorDefinitionDtoExecutionMode` constants. Only tests
  use them (`api/scheduled_tasks_contract_test.go:239`); they are updated.
- Every event schema includes `StateMachineEventDto` with `allOf`. `StateMachineFinishedDto` gains that `allOf`.
- New schemas: `StateMachineTransitionAbortedDto`, `StateMachineScheduledTransitionArmedDto`,
  `…FiredDto`, `…ExpiredDto`, `…CancelledDto`, `…FailedDto`, `CriterionFunctionResultDto` (a `functions` entry).
- Removed fields: `StateMachineStateProcessResultDto.stateProcessId`, `.stateProcessClassName`,
  `.stateProcessType`; the schema `StateProcessIdDto`, which nothing else references. Reason: for an externalized
  processor Cloud fills `stateProcessId` with an internal cache key that no endpoint accepts, and
  `stateProcessClassName` with one constant Java class name; cyoda-go has no true value for either (Rule 3).
  The outcome is `success`, as on FINISH.
- The `data` description and the per-schema descriptions replace today's prose list (`api/openapi.yaml:11713-11730`).
- After the edit: `go generate ./api`, and commit the result.

## 4. Event data

Each row is the schema's own properties beside the inherited `eventType`. **R** = required, **O** = optional
(present when stated). Event names are cyoda-go's; Cloud's spelling is in section 11.

| Event | Schema | Properties |
|---|---|---|
| `STATE_MACHINE_START` | `StateMachineStartedDto` | `state` O — absent on create, where the entity has no state yet |
| `STATE_MACHINE_FINISH` | `StateMachineFinishedDto` | `state` R, `success` R (always `true`), `stopReason` R (section 5) |
| `CANCEL` | `StateMachineCancelledDto` | `reason` R |
| `FORCE_SUCCESS` | `StateMachineForcedSuccessDto` | none |
| `WORKFLOW_FOUND` | `StateMachineWorkflowFoundDto` | `workflowName` R, `functions` O |
| `WORKFLOW_SKIP` | `StateMachineWorkflowSkippedDto` | `workflowName` R, `reason` R, `functions` O |
| `WORKFLOW_NOT_FOUND` | `StateMachineWorkflowNotFoundDto` | none |
| `TRANSITION_MAKE` | `StateMachineTransitionMadeDto` | `workflowName` R, `transition` R, `newState` R, `functions` O |
| `TRANSITION_NOT_FOUND` | `StateMachineTransitionNotFoundDto` | `workflowName` R, `transition` R |
| `TRANSITION_NOT_MATCH_CRITERION` | `StateMachineTransitionCriterionNotMatchedDto` | `workflowName` R, `transition` R, `criterion` R, `reason` R, `functions` O |
| `TRANSITION_ABORTED` | `StateMachineTransitionAbortedDto` | `reason` R (`ENTITY_MODIFIED`), `transition` O, `expectedTxId` R, `actualTxId` R |
| `PROCESS_NOT_MATCH_CRITERION` | `StateMachineProcessCriterionNotMatchedDto` | unchanged |
| `PAUSE_FOR_PROCESSING` | `StateMachineProcessingPausedDto` | `workflowName` R, `transition` R, `processNames` R |
| `STATE_PROCESS_RESULT` | `StateMachineStateProcessResultDto` | `processorName` R, `executionMode` R, `success` R, `failReason` O |
| `SCHEDULED_TRANSITION_ARM` | `…ArmedDto` | `transition` R, `sourceState` R, `scheduledTime` R (int64, Unix ms) |
| `SCHEDULED_TRANSITION_FIRE` | `…FiredDto` | `transition` R, `sourceState` R, `workflowName` R |
| `SCHEDULED_TRANSITION_EXPIRE` | `…ExpiredDto` | `transition` R, `sourceState` R |
| `SCHEDULED_TRANSITION_CANCEL` | `…CancelledDto` | `transition` R, `sourceState` R, `workflowName` O — present at the fire-time cancels (`fire_scheduled.go:230`, `:259`, where the selected workflow is known); absent at the arm-time reconcile (`arm.go:220`), which cancels a task armed under an earlier state, so the current workflow is not necessarily the task's |
| `SCHEDULED_TRANSITION_FAIL` | `…FailedDto` | `transition` R, `sourceState` R, `reason` R (enum of the five FAIL reasons), `attempts` R, `lostOwners` R |

`CriterionFunctionResultDto`: `path` R, `name` R, `called` R, `preCheck` O (the local-condition union),
`preCheckMatched` O, `matches` O, `reason` O — as the engine records them today (`criterion_eval.go:43-56`).

Field rules:

- **`state` on START** is the outer event's `state`; on create it is absent in both places
  (`internal/domain/audit/events.go:96-98`).
- **`state` on FINISH** is the state the run settled in.
- **`newState` on TRANSITION_MAKE** is the transition's target; the outer `state` stays the state left.
- **CANCEL `reason`** is today's reason text (`engine.go:902`, `:988`), which moves out of `details` into `data`.
  `details` keeps its own sentence.
- **TRANSITION_NOT_FOUND** keeps its three causes (not declared, disabled, scheduled) in `details` only.
- **TRANSITION_ABORTED `transition`** replaces `transitionName`. It is absent when the abort is on a loopback,
  where there is no transition; today that path records the literal `"loopback"` (`engine.go:510`).
- **PAUSE_FOR_PROCESSING `processNames`** are the transition's processor names in pipeline order.
- **STATE_PROCESS_RESULT**:
  - `processorName` is the processor's workflow `name`.
  - `executionMode` is the processor's configured mode, with an empty stored mode recorded as `SYNC`: an empty
    mode runs as `SYNC` (`internal/domain/workflow/validate.go:46-55`, `engine_processors.go:167`). This holds also
    on the internalized-processor refusal (`engine_processors.go:137-147`), where nothing ran. The dispatch span
    attribute (`internal/observability/dispatch_tracing.go:155`) gets the same rule. The enum is the four modes.
  - `failReason` is present only when the member itself sent failure text: a `contract.CalloutFailure` of kind
    `MemberFailed` (found with `errors.As`) whose new field `MemberText` is non-empty (section 8.1). Every other
    failure (the member sent no text, no member, no answer, unreadable answer, internalized processor refused,
    savepoint failure, platform error) has no `failReason`. No platform text enters `data`: today's fixed
    sentences `"<label> returned failure"` (`internal/grpc/dispatch.go:267`) and `"peer node dispatch failed"`
    (`internal/cluster/dispatch/handover.go:27`) are never a `failReason`.
  - `success` is `false` for an `ASYNC_NEW_TX` processor that failed, as today, although the run continues.
- **SCHEDULED_TRANSITION_FIRE**: `sourceState` is the task's source state; `workflowName` is the selected
  workflow. The outer `state` is the state the run settled in, as today.
- **SCHEDULED_TRANSITION_EXPIRE** at fire time gains `transition` and `sourceState`; lateness and timeout stay in
  `details`.
- **`functions`** keeps today's rule: present, possibly empty, whenever the criterion holds a function condition
  (`engine.go:1029-1031`).
- **Never sent by cyoda-go**, kept for the shared contract: `PROCESS_NOT_MATCH_CRITERION` (a processor has no
  criterion) and `WORKFLOW_NOT_FOUND` (section 7.3). Their schema descriptions say so. They get no constructor
  (section 7.1); an `api/` test validates a literal sample of each against its schema.

## 5. Stop reason (FINISH)

`stopReason` is `{type, description, rejectedTransitions?}`. cyoda-go records FINISH only on a run that succeeded
(`engine.go:365, 456, 533, 561`), so three types are possible:

| `type` | When | `description` | `rejectedTransitions` |
|---|---|---|---|
| `COMPLETED_NO_AUTO_TRANSITIONS` | the settled state has no automated transition: none that is not `disabled`, not `manual` and not scheduled | `No automated transition from state "<state>"` | absent |
| `COMPLETED_ALL_CRITERIA_REJECTED` | the settled state has automated transitions and every one was refused by its criterion in the last pass | `Every automated transition from state "<state>" was refused by its criterion` | one entry per refused transition, in declaration order |
| `FORCE_SUCCESS` | a loopback on an entity whose state the selected workflow does not declare (`engine.go:519-541`) | `State "<state>" is not declared in workflow "<workflow>"` | absent |

- `rejectedTransitions` entries are `RejectedTransitionCriterionDto` `{transitionName, criterion, reason}`, with the
  same `criterion` and `reason` values as the matching TRANSITION_NOT_MATCH_CRITERION event.
- Only the last pass counts: the refusals in the state the run settled in. A refusal in an earlier state that
  the cascade then left is not a reason the run stopped. Cloud accumulates refusals over the whole transaction
  (`StopReasonComputer.kt:53-59`); the parity doc (section 11) states cyoda-go's rule.
- `cascadeAutomated` returns the stop reason of the pass it settled on. Execute, ManualTransition and Loopback put
  it into FINISH. A pass that settles fires nothing, so it runs no processor: no commit-before-dispatch segment
  falls between the last pass and FINISH. On a manual transition, a named transition the criterion refuses
  returns an error and records no FINISH, so the stop reason always comes from the cascade.
- The other types stay in the enum (Cloud sends them) and are never sent by cyoda-go: `PAUSED_FOR_PROCESSING`,
  `CANCELED`, `NO_WORKFLOW_FOUND`, `WORKFLOW_SKIPPED`, `INCOMPLETE_TRANSACTION`, `UNKNOWN`. The schema description
  says so.
- A scheduled fire records no START and no FINISH, as today (`fire_scheduled.go:139-345`). Its terminal event is
  SCHEDULED_TRANSITION_FIRE, which has no stop reason. `GET …/finished` answers 404 for a fire's transaction, as
  today. `cyoda help audit` says so.

## 6. Output (`internal/domain/audit/events.go`)

- `stateMachineItem` writes `data` as a copy of the stored map with `eventType` set to the event's type. It never
  changes the stored map. The inner and outer types therefore cannot disagree, and both recording sites
  (`Engine.recordEvent`, `scheduler.failWithAudit`) are covered by one rule.
- A stored event with nil data is written as `{"eventType": …}`.
- Nothing changes in storage or the SPI's `Data map[string]any`. memory, sqlite and postgres round-trip the map;
  cassandra stores the event as an opaque blob and needs no change.

## 7. Engine changes (`internal/domain/workflow`, `internal/scheduler`)

### 7.1 Data at each site
Every recording site builds its data through one constructor per event type (one file, e.g. `audit_data.go`).
Each constructor takes the event's fields as typed arguments and returns the map. No site builds an event map
inline. `scheduler.failWithAudit` uses the FAIL constructor, exported from the workflow package (the scheduler
already imports it). The two event types cyoda-go never sends get no constructor.

### 7.2 Processor failure records
The second STATE_PROCESS_RESULT `{success:false}` that the caller records after a failed processor pipeline
(`engine.go:837-839`, `:965-968`) is removed. Every pipeline failure that reached a processor already has that
processor's own record (`engine_processors.go:137-147`, `:194-211`). The checkpoint exit before dispatch
(`:127`) has no processor result to report. The fence exit after dispatch (`:178`) has no per-processor record,
and the caller's record is not written there either, because `recordEvent` returns at once under a fence
(`engine.go:1086-1088`). The removed record's `details` embedded the wrapped error text (`"…: %v"`). That text
reaches the audit trail where the failed run's events survive: a batch-isolated item, and a joined callback,
whose audit rows commit with the owner's transaction. Removing the record is also a Gate 3 fix.
`engine_processors_audit_test.go:69-100`, which asserts the second record, changes to assert one.

### 7.3 Unreachable code removed
- `selectWorkflow`'s WORKFLOW_NOT_FOUND branch (`engine.go:620-622`): `NewEngine` always installs the default
  workflow (`engine.go:188-192`). The fallback to it becomes unconditional.
- `cascadeAutomated`'s "state not in workflow — stable" branch (`engine.go:908-911`): import requires every
  `next` state and the initial state to be declared (`validate.go:567-572`, `:623`), and Loopback checks its
  starting state first (`engine.go:519`).
- `ExecutionResult.StopReason = "STATE_NOT_IN_WORKFLOW"` (`engine.go:540`), a second spelling of FORCE_SUCCESS.
  Its only readers are the `if result.StopReason == ""` guards in `internal/domain/entity/service.go:322` and
  `:2093`, which only the create path reaches, where the value is always empty. The guards become unconditional
  assignments; `internal/domain/workflow/workflow_selection_test.go:246`, which asserts the string, asserts the
  FINISH stop reason instead.
- The `"CREATED"` state fallback and its comment in `service.go:312-315` and `:2087-2089`: the engine always sets
  a declared, non-empty initial state (`validate.go:567-572`, `engine.go:328`).
- SPI: `ExecutionResult` keeps `State`. `StopReason`, `Success` and `Error` have no reader after this change and
  are removed from `cyoda-go-spi`, in a PR into the SPI's `main`; cyoda-go pseudo-pins it (no tag mid-milestone).
  The cassandra plugin references none of them.

No test sets `defaultWorkflows` empty (the assignments at `arm_test.go:1079`, `:1101` and
`import_tasks_test.go:292` set non-empty lists), so the WORKFLOW_NOT_FOUND deletion removes no test.

## 8. Member text and the audit write

### 8.1 The member's own words (`internal/contract`, `internal/grpc`, `internal/cluster/dispatch`)

`failReason` must be the member's own text and nothing else (Rule 3). Today `CalloutFailure.Message` for
`MemberFailed` is the member's text or a fixed sentence when the member sent none
(`internal/grpc/dispatch.go:266-271`), and on a hand-over the owner cannot tell the two apart
(`internal/cluster/dispatch/handover.go:329-331`, `:420-428`).

- `contract.CalloutFailure` gains `MemberText`: for `MemberFailed`, the member's own failure text, bounded, and
  empty when the member sent none. `Message` keeps its role: the client-safe text, with a fixed sentence when the
  member sent none.
- Local try (`internal/grpc/dispatch.go:266-271`): `MemberText` is `boundMemberText(resp.Error)` when
  `resp.Error` is non-empty; `Message` as today.
- Hand-over: the answering node puts `MemberText` (not `Message`) on the wire as `memberError` (`fillFailure`,
  `handover.go:329-331`). The owner (`readAnswer`, `handover.go:357`, `:420-428`) sets `MemberText` from
  `memberError` after `boundPeerText` (`internal/cluster/dispatch/peer_router.go:174-175`), and `Message` from it
  or, when it is empty, `call.Kind.String() + " returned failure"` — the sentence a local try uses, so the client
  text does not depend on which node held the member. The cluster wire is internal; no compatibility shape is
  kept. `peerFailedClientMessage` keeps its other use (`handover.go:450`).
- Several tries: a `MemberFailed` try ends the callout (`internal/grpc/run_local.go:154`), and `verdict` returns it
  through `withAttempts`, which copies the struct (`internal/callout/coordinator.go:386-388`). So `MemberText`
  survives earlier `NoHandOff`/`NoAnswer` tries with no further change.

### 8.2 The audit write (`internal/domain/workflow`)

`Engine.recordEvent` refuses an event in which any string holds U+0000 — `State`, `Details`, and every key and
string value in `Data` at any depth — before it calls `Record`. It logs the refusal at WARN with the entity id and
event type, never the text, as it logs a failed `Record` today, and the operation goes on. So no backend writes
the event, and PostgreSQL's transaction is not aborted. One helper does the walk.

`scheduler.failWithAudit` (`internal/scheduler/service.go:925-946`) is not changed: its text is the task's
transition and state names, from a workflow import, and fixed enum values. Workflow configuration is a reviewed
change, not runtime input.

Invalid UTF-8 is not a divergence: JSON decoding and `json.Marshal` turn it into U+FFFD the same way on every
backend. It is out of scope.

## 9. Errors per entry point

No status or error code changes on any endpoint. The rows below are what this change can make an existing code
mean.

**`GET /audit/entity/{entityId}`, `GET /audit/entity/{entityId}/workflow/{transactionId}/finished`**

| Status | Code | When |
|---|---|---|
| 200 | — | every state-machine event carries typed `data` (section 4); FINISH carries `stopReason` |
| 400, 401, 403, 404, 410, 413, 500, 503 | as today | unchanged |
| 404 | `ENTITY_NOT_FOUND` | `…/finished` for a scheduled fire's transaction (no FINISH, as today) |

**Entity create, update, PATCH, collection update, manual transition (HTTP and gRPC), scheduled firing**

| Status / gRPC envelope | Code | When |
|---|---|---|
| as today | as today | an event holding U+0000 (a member's reason or failure text, a caller's transition name or `ifMatch`) is not recorded, on every backend; the operation's outcome is the one it has without that event. Today the refused `INSERT` aborts the transaction on PostgreSQL, so the operation fails there and succeeds on the others |

## 10. Coverage matrix

Layers: **U** unit, **E** e2e HTTP on PostgreSQL (`internal/e2e`), **G** gRPC (`internal/grpc`), **P**
cross-backend parity (`e2e/parity`, registered in `registry.go`), **M** multi-node (hand-over to a peer), **S**
spec test in `api/` beside `api/condition_union_test.go` (it reads the embedded spec through `GetSwagger`).

The e2e OpenAPI validator in enforce mode checks the shape of every response the full e2e suite makes. The
scenarios below also assert values.

| Scenario | U | E | G | P | M | S |
|---|---|---|---|---|---|---|
| Every discriminator has a mapping; every mapping target is a member, spelled as the `oneOf` `$ref`; every `allOf` includer of a discriminated base is in its mapping (named schemas and inline unions) | | | | | | ✓ |
| `data` refuses null and absence; the two never-sent event types: a literal sample validates | | | | | | ✓ |
| Conditional DELETE with a typed body still deletes only matches | | ✓ (existing) | | | | |
| Every event type cyoda-go sends: the data produced by driving the engine path validates against its schema (kin-openapi), not hand-built maps | ✓ | | | | | |
| Create → START (no `state`), WORKFLOW_FOUND `workflowName`, TRANSITION_MAKE `newState`, FINISH `stopReason` `COMPLETED_NO_AUTO_TRANSITIONS` | ✓ | ✓ | | ✓ | | |
| FINISH `COMPLETED_ALL_CRITERIA_REJECTED` with `rejectedTransitions` of the last pass only (an earlier state's refusal not listed) | ✓ | ✓ | | ✓ | | |
| Loopback with the state not in the selected workflow → FORCE_SUCCESS, FINISH `stopReason` `FORCE_SUCCESS` | ✓ | | | | | |
| `eventType` inside `data` equals the outer `eventType`, on search and `…/finished`; nil stored data → `{eventType}` | ✓ | ✓ | | ✓ | | |
| PAUSE_FOR_PROCESSING `processNames`; STATE_PROCESS_RESULT `processorName`, `executionMode`; one record per failed processor | ✓ | ✓ | | | | |
| Empty stored execution mode → `executionMode: SYNC`, also on the internalized refusal | ✓ | ✓ | | | | |
| `failReason` present: `ASYNC_NEW_TX` processor whose member answers `success:false` with a message | ✓ | ✓ | | | ✓ | |
| `failReason` absent: member answers `success:false` with no message (local and handed over); callout timeout; internalized processor refused | ✓ | | | | ✓ | |
| TRANSITION_ABORTED: `transition` on a manual transition; absent on a loopback | ✓ | ✓ | | | | |
| CANCEL `reason` (visit limit) | ✓ | | | | | |
| TRANSITION_NOT_FOUND `workflowName`, `transition` | ✓ | | | | | |
| SCHEDULED_TRANSITION_FIRE and fire-time EXPIRE data; CANCEL `workflowName` at the fire-time sites only | ✓ | ✓ | | | | |
| SCHEDULED_TRANSITION_ARM / FAIL data (existing assertions updated) | ✓ | ✓ | | ✓ | | |
| Member `success:false` with no text, handed over → client text `"<kind> returned failure"`, as locally; `failReason` absent | ✓ | | | | ✓ | |
| A `MemberFailed` try after an earlier `NoAnswer` try → `failReason` is that member's text | ✓ | | | | | |
| Event holding U+0000 is not recorded and the operation commits: an automated transition refused by a criterion whose member reason holds U+0000 — the entity is saved, no TRANSITION_NOT_MATCH_CRITERION event, on memory, SQLite and PostgreSQL (today PostgreSQL fails the save) | ✓ | ✓ | | ✓ | | |
| The same through a hand-over (the owner records) | | | | | ✓ | |
| The walk: U+0000 in `State`, `Details`, a nested key, a nested value → refused; none → recorded | ✓ | | | | | |
| `…/finished` returns FINISH with typed data | | ✓ | | ✓ | | |

Failure-path events (CANCEL, TRANSITION_NOT_FOUND, a failed SYNC processor) roll back with the request on the
owner path, so their values are asserted in unit tests, and in e2e only where a path makes them durable (a
batch-isolated item, an `ASYNC_NEW_TX` processor).

Existing tests that read changed keys are updated: `engine_processors_audit_test.go:69-100` (the duplicate
record), `engine_transition_aborted_test.go:119-130`, `criterion_function_audit_test.go:259`, `:287` (nil
`data`), `workflow_selection_test.go:246`, `internal/e2e/collection_update_ifmatch_test.go:264-276`,
`callback_joined_commit_test.go:405-411`, `scheduled_transition_test.go:75-86`, the parity helpers in
`e2e/parity/audit.go`, `api/condition_dto_test.go:208-210`, `api/scheduled_tasks_contract_test.go:239`.

The concurrency rule does not apply: nothing here is a race.

## 11. Cloud (Gate 7)

New `docs/cloud-parity/state-machine-event-data.md` (and the `README.md` index), and a CaaS ticket (`[CaaS]`
prefix, component CaaS). Cloud must:

- rename its event-type values to cyoda-go's, in the outer enum, the union mapping and inside `data`
  (`STARTED`→`STATE_MACHINE_START`, `FINISHED`→`STATE_MACHINE_FINISH`, `CANCELLED`→`CANCEL`,
  `FORCED_SUCCESS`→`FORCE_SUCCESS`, `WORKFLOW_SKIPPED`→`WORKFLOW_SKIP`, `TRANSITION_MADE`→`TRANSITION_MAKE`,
  `TRANSITION_CRITERION_NOT_MATCHED`→`TRANSITION_NOT_MATCH_CRITERION`,
  `PROCESS_CRITERION_NOT_MATCHED`→`PROCESS_NOT_MATCH_CRITERION`, `PROCESSING_PAUSED`→`PAUSE_FOR_PROCESSING`);
- add the six event types it lacks (`TRANSITION_ABORTED`, the five `SCHEDULED_TRANSITION_*`) where it has the
  behaviour, and the `functions` field on the four criterion events;
- STATE_PROCESS_RESULT: send `processorName`, `executionMode`, `success`, `failReason`; drop `stateProcessId`,
  `stateProcessClassName`, `stateProcessType`;
- FINISH: compute `rejectedTransitions` from the settled state's last pass only;
- TRANSITION_MADE has `newState`; TRANSITION_NOT_FOUND requires `transition`;
- `…/finished` returns the whole audit event, not only `data`;
- the condition and audit discriminator mappings as in section 3;
- Rule 4: an audit event holding U+0000 is written on no backend (check what Cloud's store does with such text
  before filing).

Updated parity docs: `criterion-stoppage-reason.md` (`data` now typed), `entity-if-match.md:27`
(`transitionName` → `transition`, absent on loopback), `scheduled-transitions.md` (FIRE and EXPIRE data),
`function-conditions-in-criteria.md` (`CriterionFunctionResultDto`), `openapi-conformance.md` (the mapping rule).

## 12. Documentation

- `cmd/cyoda/help/content/audit.md`: the FINISH example with `stopReason`; the per-event data table; "null for
  most event types" goes; `transition` on TRANSITION_ABORTED; `…/finished` for a scheduled fire.
- `cmd/cyoda/help/content/workflows.md`, `crud.md`, `grpc.md`: every statement of an event's data.
- `docs/ARCHITECTURE.md:1758-1776` (the event table); `docs/PROCESSOR_EXECUTION_MODES.md:412-436` (the record's
  fields, and names the code does not emit: `STATE_MACHINE_PROCESSING_PAUSED`, `STATE_MACHINE_CANCELLED`).
- The doc comment at the top of `internal/domain/workflow/transition_aborted.go` (the data example).
- `docs/cloud-parity/callout-failover.md`: `failReason` is the member's own text only; a hand-over with no member
  text gives the same client text as a local try.
- `cmd/cyoda/help/content/audit.md`: an event holding U+0000 is not recorded.
- `CHANGELOG.md` `### Breaking`: `data` always typed and never null, with `eventType` inside; the new data fields;
  TRANSITION_ABORTED `transitionName` → `transition`; STATE_PROCESS_RESULT fields; one record per failed processor;
  an event holding U+0000 is not recorded on any backend; the conditional DELETE body schema.
- `COMPATIBILITY.md`: the SPI pin.

## 13. CI gates

- **oasdiff** (`.github/workflows/openapi-breaking-change.yml`, pinned v1.21.0): a prototype of section 3 gives
  two ERR lines, `response-property-one-of-added` on `GET /audit/entity/{entityId}` and on `…/finished`; they go
  into `.github/oasdiff-err-ignore.txt` after review, with any other ERR the final spec produces. The DELETE body
  change is a WARN; the discriminator removals are INFO.
- **codegen sync**: `go generate ./api` output committed.
- The full `internal/e2e` package runs, so `TestOpenAPIConformanceReport` sees every response.

## 14. Not in this change

- cassandra: no change (it stores the event as an opaque blob and records none itself).
- gRPC and CloudEvents: no surface carries these events.
- START and FINISH on a scheduled fire, and a stop reason on SCHEDULED_TRANSITION_FIRE: the scheduled-transition
  design records neither; unchanged.
