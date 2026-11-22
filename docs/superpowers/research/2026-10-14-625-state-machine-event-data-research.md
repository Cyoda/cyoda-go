# #625 research — discriminator mappings and state-machine event data

Facts gathered at `c00966b9` (`release/v0.9.0`). Cloud facts are from
`~/dev/cyoda` (spec `client/src/main/resources/api/openapi-audit.yml`, Kotlin
under `backend/src/main/kotlin/net/cyoda/saas/`) and the platform 4.0.0-SNAPSHOT
source jars in `~/.m2` (core and service). "Could not verify" is stated where it
applies.

## 1. Discriminators in `api/openapi.yaml`

Mapped already (by #628): every inline condition `oneOf` (`:7169`, `:7602`,
`:9669`, `:9728`, `:9774`, `:10468`, `:10668`, `:11225`), the audit-item `oneOf`
in `EntityAuditEventsResponseDto` (`:11593`), `ProcessorDefinitionDto` (`:10385`).

Not mapped — four named bases:

| Schema | Property | Subtypes (by `allOf`) | Used directly |
|---|---|---|---|
| `QueryConditionDto` `:9819` | `type` | `FunctionConditionDto` | no |
| `AbstractConditionDto` `:12152` | `type` | simple, group, lifecycle, array | conditional `DELETE` body `:2271` |
| `AuditEventDto` `:11387` | `auditEventType` (plain string `:11432`) | EntityChange, StateMachine, System | no |
| `StateMachineEventDto` `:11747` | `eventType` | 12 subtypes, none referenced | no |

Cloud: every discriminator in its OpenAPI specs has a mapping. Its condition base
`QueryCondition` maps all five members and types `type` as the enum
`QueryConditionType` (`openapi-common.yml:67-94`). `AuditEvent` maps
`StateMachine`, `EntityChange`, `System` and types the property as an enum
(`openapi-audit.yml:534-540, 604-612`).

## 2. What cyoda-go records today

Two recording sites: `Engine.recordEvent` (`internal/domain/workflow/engine.go:1080-1115`)
and the scheduler's `failWithAudit` (`internal/scheduler/service.go:925-946`).
The SPI carries `Data map[string]any` (`cyoda-go-spi types.go:597`).

| Event | Sites | `data` today |
|---|---|---|
| STATE_MACHINE_START | `engine.go:319, 421, 508` | nil |
| STATE_MACHINE_FINISH | `engine.go:365, 456, 533, 561` | `{success: true}`; never false |
| FORCE_SUCCESS | `engine.go:524` (loopback, state not in selected workflow) | nil |
| CANCEL | `engine.go:903` (visit limit), `:989` (depth limit) | nil; reason only in `details` |
| WORKFLOW_FOUND | `engine.go:586, 598, 615` | nil, or `{functions}` |
| WORKFLOW_SKIP | `engine.go:603` | `{workflowName, reason, functions?}` |
| WORKFLOW_NOT_FOUND | `engine.go:620` | nil — unreachable in production: `NewEngine` always installs a default workflow (`engine.go:188-192`), selection falls back to it (`:611-618`) |
| TRANSITION_NOT_FOUND | `engine.go:754, 760, 766` (not found / disabled / scheduled) | nil |
| TRANSITION_NOT_MATCH_CRITERION | `engine.go:811, 936` | `{workflowName, transition, criterion, reason, functions?}` |
| TRANSITION_MAKE | `engine.go:846, 973` | nil, or `{functions}` |
| TRANSITION_ABORTED | `transition_aborted.go:56-64` | `{reason: ENTITY_MODIFIED, transitionName, expectedTxId, actualTxId}` |
| PROCESS_NOT_MATCH_CRITERION | none | never emitted: a processor has no criterion (SPI `ProcessorDefinition`; criteria only on workflow `:181` and transition `:207`) |
| PAUSE_FOR_PROCESSING | `engine_processors.go:107` | nil; processor names only in `details` |
| STATE_PROCESS_RESULT | `engine_processors.go:137` (internalized refused), `:194-211` (per processor) | `{success, mode}`; error text deliberately left out (`:198-202`) |
| STATE_PROCESS_RESULT | `engine.go:837, 965` (caller, after a processor failure) | `{success: false}` — a second record for the same failure (test `engine_processors_audit_test.go:69-100`); its `details` embeds `err` text |
| SCHEDULED_TRANSITION_ARM | `arm.go:214` | `{transition, sourceState, scheduledTime (unix ms)}` |
| SCHEDULED_TRANSITION_CANCEL | `arm.go:220`, `fire_scheduled.go:230, 259` | `{transition, sourceState, workflowName?}` |
| SCHEDULED_TRANSITION_EXPIRE | `arm.go:231`; `fire_scheduled.go:187` | `{transition, sourceState}`; nil at fire time |
| SCHEDULED_TRANSITION_FIRE | `fire_scheduled.go:338` | nil |
| SCHEDULED_TRANSITION_FAIL | `scheduler/service.go:938` | `{transition, sourceState, reason, attempts, lostOwners}` |

`functions` entries (`criterion_eval.go:43-56`): `path`, `name`, `called` always;
`preCheck` (decoded with `UseNumber`) and `preCheckMatched` with a pre-check;
`matches` when called, `reason` when the member gave one. `{functions: []}` is
possible.

Outer `state` (`engine.go:1092`) is omitted from the response when empty
(`internal/domain/audit/events.go:96-98`) — the case on create, where START and
selection are recorded before the initial state is set (`engine.go:319-328`).
On TRANSITION_MAKE it is the state left.

Durability: events are staged in the transaction. Those recorded on a failure
return are lost when the request rolls back
(`e2e/parity/externalapi/transition_aborted_audit.go:142-153`), but survive when
a batch isolates the failed item (`internal/e2e/collection_update_ifmatch_test.go:233-276`)
and for a failed `ASYNC_NEW_TX` processor (the transaction commits).

## 3. Storage and output

- memory keeps the Go value; sqlite and postgres store `json.Marshal(event)` and
  decode with `UseNumber` (`plugins/sqlite/sm_audit_store.go:168-178`,
  `plugins/postgres/sm_audit_store.go:127-137`).
- cassandra stores the whole event as an opaque JSON blob and never reads keys
  in `Data` (`cyoda-go-cassandra internal/store/sm_audit_store.go:35-104`); it
  does not record events itself. Typed `data` needs no plugin change.
- HTTP: `stateMachineItem` (`internal/domain/audit/events.go:73-113`) emits
  `data` as stored, `null` when nil. Used by search (`handler.go:124-145`) and
  `GET …/finished` (`handler.go:226-279`), which returns the whole audit event.
- Generated Go: `Data *map[string]interface{}` via `x-go-type`
  (`api/generated.go:2289`).
- Not exposed over gRPC or CloudEvents (no audit in `internal/grpc`, no audit
  CloudEvent type).

## 4. Cloud

- `StateMachineAuditEvent.data` is `$ref StateMachineEvent`, a discriminated union
  on `eventType` with a 13-entry mapping (`openapi-audit.yml:658-693, 805-846`).
- The type value appears twice: on the outer event and inside `data`, where
  Jackson writes it as the type id (`GEN/StateMachineEventDto.kt:26-30`;
  spec examples `:189/194`; `EntityAuditInteractorIT.kt:689-701`).
- Wire names differ from cyoda-go's: STARTED, FINISHED, CANCELLED,
  FORCED_SUCCESS, WORKFLOW_SKIPPED, TRANSITION_MADE,
  TRANSITION_CRITERION_NOT_MATCHED, PROCESS_CRITERION_NOT_MATCHED,
  PROCESSING_PAUSED; mapped from the platform's own names, which equal
  cyoda-go's (`StateMachineEventFunctions.kt:100-114`).
- Subtype fields (spec `:848-1055`): Started `{state}`; Finished
  `{state, success, stopReason}` (all required); Cancelled `{reason}`;
  ForcedSuccess `{}`; WorkflowFound `{workflowName}`; WorkflowNotFound `{}`;
  WorkflowSkipped `{workflowName, reason}`; TransitionMade
  `{workflowName, transition, newState}`; TransitionNotFound
  `{workflowName, transition?}`; TransitionCriterionNotMatched
  `{workflowName, transition, criterion, reason?}`; ProcessCriterionNotMatched
  `{workflowName, processorClassName, criterion, reason?}`; ProcessingPaused
  `{workflowName, transition, processNames}`; StateProcessResult
  `{stateProcessId, stateProcessClassName, stateProcessType, failReason?}`.
- Finished's `stopReason` (`WorkflowStopReason {type, description, rejectedTransitions?}`)
  is computed by Cloud from the transaction's events (`StopReasonComputer.kt:23-130`);
  absent → `{UNKNOWN, "Stop reason not computed"}` (`StateMachineEventFunctions.kt:24-28`).
- StateProcessResult for an externalized processor:
  - `stateProcessClassName` is always the constant
    `net.cyoda.saas.externalize.processor.ExternalizedProcessor`
    (`CyodaToCloudWorkflowMapper.kt:1081`); the processor's name is not in it.
  - `stateProcessId` is an internal cache key (`ProcessingImpl.java:121-124`);
    no endpoint accepts it.
  - `failReason` holds the member's error code and message, or an exception
    message (`ExternalizerBase.kt:404-427`, `ProcessingImpl.java:248-250`).
  - RESUBMIT/UNKNOWN: not produced on the externalized path (could not rule out
    other platform jars).
  - No event records the execution mode.
- Cloud has no events for transition abort, scheduled transitions or function
  results.
- Cloud's `/finished` endpoint returns the `data` object alone
  (`EntityAuditInteractor.kt:81-83`); cyoda-go returns the whole audit event.

## 5. Tests and docs that read `data`

Unit: `internal/domain/workflow/criterion_function_audit_test.go`,
`engine_test.go:3317-3559`, `engine_processors_audit_test.go:82-100`,
`engine_transition_aborted_test.go:119-130`, `internal/scheduler/outcome_test.go:349-477`,
`panic_test.go:47`. Backends: `plugins/sqlite/sm_audit_data_numbers_test.go`,
`plugins/postgres/sm_audit_store_test.go:206-243`. e2e:
`scheduled_transition_test.go:75-86`, `criterion_reason_test.go`,
`criterion_function_eval_test.go`, `collection_update_ifmatch_test.go:264-276`,
`callback_joined_commit_test.go:405-411`, `scheduled_run_*_test.go`. Parity:
`e2e/parity/audit.go:271-321`, `criterion_function.go`, `criterion_reason.go`,
`externalapi/workflow_externalization.go:474-486`,
`scheduledtransition/ownership.go:349-398`. No test asserts FINISH `success`.

Help/docs: `cmd/cyoda/help/content/audit.md:78-123`, `workflows.md:420, 514-569`,
`crud.md:226`, `grpc.md:395-402`; `docs/ARCHITECTURE.md:1758-1776`;
`docs/PROCESSOR_EXECUTION_MODES.md:412-436` (uses names the code does not emit:
`STATE_MACHINE_PROCESSING_PAUSED`, `STATE_MACHINE_CANCELLED`); cloud-parity
`criterion-stoppage-reason.md`, `function-conditions-in-criteria.md:130-150`,
`scheduled-transitions.md:320-335`, `entity-if-match.md:25-26`.
