# State-machine event data and the polymorphic form — Cloud twin-alignment spec

cyoda-go **defines** the contract; Cyoda Cloud aligns to it. This document
states what cyoda-go sends in a state-machine audit event, the one form every
polymorphic type of the spec has, how the contract grows, and what Cloud
changes to match.

CaaS ticket: (filed after the PR opens)

Cloud references are paths in the Cloud repository (`cyoda`); a bare
`openapi-*.yml` is under `client/src/main/resources/api/`. The state-machine
audit store is in cyoda-platform `4.0.0-SNAPSHOT`, the platform version Cloud
builds against (`pom.xml:91`).

## 1. Terms

- **Base** — a schema with a discriminator and a mapping. Its **members** are
  the schemas the mapping names; each member includes the base with `allOf`.
  A **use** of a base is any `$ref` to it other than a member's `allOf` entry.
- **Event** — one state-machine audit event (`StateMachineAuditEventDto`). Its
  **type** is the outer `eventType`; its **data** is the `data` member.
- **Member text** — a string a compute member sends in its answer, such as a
  failure message or a criterion reason.

## 2. One polymorphic form: a base

Every polymorphic type in cyoda-go's spec is a base: a discriminator and a
mapping that names every member, each member including the base with `allOf`,
and every use a `$ref` to the base. No `oneOf` or `anyOf` carries a
discriminator, and no schema includes two bases.

This is Cloud's form. The condition, audit-event and state-machine-event
bases have the same members in both specs, once Cloud has the event types of
section 4; Cloud's processor base also maps `scheduled` (section 9):

| cyoda-go base | Cloud base | Discriminator | Members |
|---|---|---|---|
| `QueryConditionDto` | `QueryCondition` (`openapi-common.yml:77-94`) | `type` | `array`, `function`, `group`, `lifecycle`, `simple` |
| `AuditEventDto` | `AuditEvent` (`openapi-audit.yml:604-656`) | `auditEventType` | `EntityChange`, `StateMachine`, `System` |
| `StateMachineEventDto` | `StateMachineEvent` (`openapi-audit.yml:823-846`) | `eventType` | the event types of section 4 |
| `ProcessorDefinitionDto` | `ProcessorDefinition` (`openapi-workflow.yml:710-726`) | `type` | `externalized` (section 9) |

Schema names differ (`QueryConditionDto` here, `QueryCondition` there), as
they do across the two specs. A condition value, `GroupKeyEntry.value`, and the
entity-create and model-import bodies are untyped schemas whose other keywords
state the rule, where Cloud writes `type: any` (`openapi-common.yml:111`,
`:200`). cyoda-go's untyped schema generates `Any?` in the CI check's Kotlin
(below); Cloud's `client/pom.xml` maps `any` and `object` to Jackson 3's
`JsonNode`, so Cloud's `SimpleConditionDto.value` is a `JsonNode?`.

cyoda-go's CI generates a `kotlin-spring` client (openapi-generator 7.18.0,
Cloud's `configOptions`) and a `java` client from cyoda-go's spec on every pull
request, compiles both, and reads and writes back a set of wire samples with
each (`.github/workflows/openapi-client-generation.yml`). A change to
cyoda-go's spec that Cloud's generator cannot read with those options fails that
check. The check does not apply the rest of Cloud's generator configuration:
the `Dto` model-name suffix, the `typeMappings` that make `object` and `any` a
Jackson 3 `JsonNode`, and the `importMappings` and `schemaMappings` that put a
hand-written `ProcessorDefinitionDto` and the `QueryConditionDto` generated from
Cloud's `openapi-common.yml` in place of the classes generated from each spec
file.

## 3. The contract grows by addition only

- **Additions are not breaking.** A field, a member of a base and an event type
  can be added in a later release.
- **A client reads what was added after it was generated**, if it ignores a
  field it does not know, and reads an object whose discriminator value it does
  not know as no object: an audit event of an unknown kind is an empty entry in
  `items`, and the `data` of an event of an unknown type is empty, while the
  outer `eventType` still names it. For a Jackson client these are two
  settings, both disabled: `FAIL_ON_UNKNOWN_PROPERTIES` and
  `FAIL_ON_INVALID_SUBTYPE`. `cyoda help openapi`, section "Generated clients",
  states this for client authors.
- **A value set that grows is a plain string, never an `enum`.** Every
  discriminator property is a plain string, and its mapping lists the known
  values. The outer `eventType` is a plain string whose known values are the
  keys of `data`'s mapping. A generated `kotlin-spring` client cannot read a
  value that a required response `enum` does not list, with any setting; so
  adding a value to a response `enum` is a breaking change. The `enum`s that
  remain (severity, execution mode, the stop-reason type, the operators) are
  value sets that are not expected to grow.
- **A use of a base in a response is never a required property.**
  `kotlin-spring` makes a required property non-null, so an object of an
  unknown member, read as empty, would fail the whole response.
  `StateMachineAuditEventDto.data` is therefore not in `required`. The server
  sends it on every event, never `null`.
- **Nothing published is renamed or removed** — no mapping key, no `enum`
  value, no event type. A value that is no longer sent stays in the spec, and
  its description says it is not sent. A stored audit event therefore always
  carries a value the contract lists.
- **The server sends no event type that the mapping lacks.**

## 4. Event types and their data

Every event carries `data`, a use of `StateMachineEventDto` keyed on
`eventType`. `data.eventType` repeats the outer `eventType`. The names are
cyoda-go's; the third column is the name Cloud sends today.

**R** = required, **O** = optional (present only when stated).

| Event | Cloud today | Schema | Data |
|---|---|---|---|
| `STATE_MACHINE_START` | `STARTED` | `StateMachineStartedDto` | `state` O — absent on create, where the entity has no state yet |
| `STATE_MACHINE_FINISH` | `FINISHED` | `StateMachineFinishedDto` | `state` R, `success` R, `stopReason` R (section 6) |
| `CANCEL` | `CANCELLED` | `StateMachineCancelledDto` | `reason` R |
| `FORCE_SUCCESS` | `FORCED_SUCCESS` | `StateMachineForcedSuccessDto` | none |
| `WORKFLOW_FOUND` | `WORKFLOW_FOUND` | `StateMachineWorkflowFoundDto` | `workflowName` R, `functions` O |
| `WORKFLOW_SKIP` | `WORKFLOW_SKIPPED` | `StateMachineWorkflowSkippedDto` | `workflowName` R, `reason` R, `functions` O |
| `WORKFLOW_NOT_FOUND` | `WORKFLOW_NOT_FOUND` | `StateMachineWorkflowNotFoundDto` | none |
| `TRANSITION_MAKE` | `TRANSITION_MADE` | `StateMachineTransitionMadeDto` | `workflowName` R, `transition` R, `newState` R, `functions` O |
| `TRANSITION_NOT_FOUND` | `TRANSITION_NOT_FOUND` | `StateMachineTransitionNotFoundDto` | `workflowName` R, `transition` R |
| `TRANSITION_NOT_MATCH_CRITERION` | `TRANSITION_CRITERION_NOT_MATCHED` | `StateMachineTransitionCriterionNotMatchedDto` | `workflowName` R, `transition` R, `criterion` R, `reason` R, `functions` O |
| `TRANSITION_ABORTED` | — | `StateMachineTransitionAbortedDto` | `reason` R, `transition` O — absent on a loopback, `expectedTxId` R, `actualTxId` R |
| `PROCESS_NOT_MATCH_CRITERION` | `PROCESS_CRITERION_NOT_MATCHED` | `StateMachineProcessCriterionNotMatchedDto` | `workflowName` R, `processorClassName` R, `criterion` R, `reason` O |
| `PAUSE_FOR_PROCESSING` | `PROCESSING_PAUSED` | `StateMachineProcessingPausedDto` | `workflowName` R, `transition` R, `processNames` R |
| `STATE_PROCESS_RESULT` | `STATE_PROCESS_RESULT` | `StateMachineStateProcessResultDto` | `processorName` R, `executionMode` R, `success` R, `failReason` O (section 5) |
| `SCHEDULED_TRANSITION_ARM` | — | `StateMachineScheduledTransitionArmedDto` | `transition` R, `sourceState` R, `scheduledTime` R (int64, Unix ms) |
| `SCHEDULED_TRANSITION_FIRE` | — | `StateMachineScheduledTransitionFiredDto` | `transition` R, `sourceState` R, `workflowName` R |
| `SCHEDULED_TRANSITION_EXPIRE` | — | `StateMachineScheduledTransitionExpiredDto` | `transition` R, `sourceState` R |
| `SCHEDULED_TRANSITION_CANCEL` | — | `StateMachineScheduledTransitionCancelledDto` | `transition` R, `sourceState` R, `workflowName` O — present when the task is cancelled as it comes due, absent when a write cancels it |
| `SCHEDULED_TRANSITION_FAIL` | — | `StateMachineScheduledTransitionFailedDto` | `transition` R, `sourceState` R, `reason` R, `attempts` R, `lostOwners` R |

A `functions` entry is a `CriterionFunctionResultDto`: `path` R, `name` R,
`called` R, `preCheck` O (a `QueryConditionDto`), `preCheckMatched` O,
`matches` O, `reason` O (`function-conditions-in-criteria.md` §5).

The rules:

- **A field is present only when the engine knows its value** when it records
  the event. Nothing is filled with a placeholder. On create, the outer `state`
  and START's `data.state` are both absent.
- **FINISH `state`** is the state the run settled in. **TRANSITION_MAKE
  `newState`** is the transition's target; the outer `state` is the state the
  transition left.
- **CANCEL `reason`** states the cascade limit the run exceeded.
- **TRANSITION_NOT_FOUND `transition`** is required: the event is recorded for
  a transition a request named. Whether it is not declared, disabled or
  scheduled is in `details` only.
- **TRANSITION_NOT_MATCH_CRITERION `reason`** is always present
  (`criterion-stoppage-reason.md` §1). `criterion` is the function's name when
  a function condition is the whole criterion, otherwise the condition's type.
- **TRANSITION_ABORTED** is cyoda-go's `If-Match` refusal
  (`entity-if-match.md`). `reason` is an open string whose known value is
  `ENTITY_MODIFIED`.
- **SCHEDULED_TRANSITION_FAIL `reason`** is an open string. Its known values
  are the five FAIL reasons, as on `ScheduledTaskDto.failureReason`
  (`scheduled-transitions.md`).
- **`functions`** is present, possibly empty, whenever the criterion holds a
  function condition.
- **Never sent by cyoda-go:** `PROCESS_NOT_MATCH_CRITERION` (a processor has
  no criterion) and `WORKFLOW_NOT_FOUND` (cyoda-go always has a default
  workflow). Both stay in the contract, which Cloud shares.

## 5. STATE_PROCESS_RESULT

One event per processor that ran.

- `processorName` — the processor's `name` in the workflow.
- `executionMode` — the processor's configured mode: `SYNC`, `ASYNC_SAME_TX`,
  `ASYNC_NEW_TX` or `COMMIT_BEFORE_DISPATCH`. A processor configured with no
  mode runs, and is recorded, as `SYNC`.
- `success` — whether the processor succeeded. `false` for a failed
  `ASYNC_NEW_TX` processor too, although the run continues.
- `failReason` — present only when the processor failed and the compute member
  itself sent failure text; then it is that text, bounded. It is absent for
  every other failure: the member sent no text, no member answered, the answer
  could not be read, the platform failed. No platform text is ever a
  `failReason`. A callout handed over to another node gives the same
  `failReason` as a local one (`callout-failover.md`).

`stateProcessId`, `stateProcessClassName` and `stateProcessType` are not in
the contract. For an externalized processor no value of them is true:
`stateProcessId` would be an internal cache key that no endpoint accepts, and
`stateProcessClassName` one constant class name. The outcome is `success`.

## 6. FINISH and its stop reason

cyoda-go records `STATE_MACHINE_FINISH` only for a run that succeeded, so
`success` is always `true` and three stop-reason types are possible:

| `type` | When | `rejectedTransitions` |
|---|---|---|
| `COMPLETED_NO_AUTO_TRANSITIONS` | the settled state has no automated transition: none that is not `disabled`, not `manual` and not scheduled | absent |
| `COMPLETED_ALL_CRITERIA_REJECTED` | the settled state has automated transitions, and its criterion refused every one in the last pass | one entry per refused transition, in declaration order |
| `FORCE_SUCCESS` | a loopback on an entity whose state the selected workflow does not declare | absent |

- **Only the last pass counts.** `rejectedTransitions` lists the refusals in
  the state the run settled in. A refusal in an earlier state that the cascade
  then left is not a reason the run stopped, and is not listed.
- Each entry is `{transitionName, criterion, reason}`, with the same
  `criterion` and `reason` as the matching TRANSITION_NOT_MATCH_CRITERION
  event.
- The stop reason is computed when the run settles and stored with the event.
- `PAUSED_FOR_PROCESSING`, `CANCELED`, `NO_WORKFLOW_FOUND`,
  `WORKFLOW_SKIPPED`, `INCOMPLETE_TRANSACTION` and `UNKNOWN` stay in the
  `enum`; cyoda-go never sends them.
- A scheduled fire records no START and no FINISH. Its last event is
  `SCHEDULED_TRANSITION_FIRE`, and `GET …/finished` answers `404` for a fire's
  transaction.

## 7. The finished endpoint

`GET /audit/entity/{entityId}/workflow/{transactionId}/finished` answers `200`
with the whole audit event, a `StateMachineAuditEventDto` whose `data` is a
`StateMachineFinishedDto` — not the `data` alone.

## 8. An event holding U+0000 is recorded on no backend

An event in which any string holds U+0000 — the outer `state`, `details`, or
any key or string value in `data` at any depth — is not recorded, on every
backend. The engine logs the refusal at WARN with the entity id and event type,
never the text, and the operation goes on with the outcome it has without that
event. Such text can reach an event from member text (a criterion reason, a
processor's failure text) or from a caller (a transition name, an `ifMatch`
value).

The reason: cyoda-go's PostgreSQL audit row is a `jsonb` document written in
the request's transaction. PostgreSQL refuses U+0000 in `jsonb`, and the
refused `INSERT` aborts the whole transaction; memory and SQLite would store
the event. Recording is best effort by design: a failed record is logged and
the operation goes on. On PostgreSQL it cannot go on, because the transaction
is already aborted. So the engine refuses the event before any backend sees
it, and every backend gives the same result.

What Cloud does today: Cloud records such an event, and the request succeeds.
The state-machine store is the Cassandra table `STATE_MACHINE_EVENTS`; each
string field is written as the raw bytes of its UTF-8 encoding
(`setBytesUnsafe`, `TypeCodecs.TEXT`) and nothing checks for U+0000
(`com/cyoda/dao/statemachine/StateMachineEventCqlDao.java:579-668`, cyoda-platform
`4.0.0-SNAPSHOT`). For TRANSITION_NOT_MATCH_CRITERION and
PROCESS_NOT_MATCH_CRITERION the store packs `criterion` and `reason` into one
value with a 0x00 byte between them
(`StateMachineEventCqlDao.java:633`, `:655`;
`com/cyoda/core/cql/utils/CQLUtils.java:577`, `:588-599`), and splits it at the
first 0x00 when it reads it (`CQLUtils.java:618-647`). A `criterion` that holds
U+0000 therefore reads back as a different `criterion` and `reason` than were
written.

A noted divergence in the same store: `stringPairSerialize` cuts the `reason`
to 1000 UTF-16 characters, the marker `...[TRUNCATED]` included
(`CQLUtils.java:570`, `:652-661`). cyoda-go keeps a compute member's criterion
reason up to 2048 characters, with `…` appended when it was cut
(`internal/grpc/dispatch.go:441`, `MaxCriterionReasonRunes`;
`criterion-stoppage-reason.md` §1). So a reason between those lengths reaches
Cloud's audit trail shorter than the contract keeps it.

## 9. Processor `type` at workflow import

`ProcessorDefinitionDto.type` is the base's discriminator, a plain string. Its
one mapped value is `externalized`. At workflow import:

- an omitted or empty `type` is stored as `externalized`, and the export
  carries `externalized`;
- any other value, `internalized` included, is refused with
  `400 VALIDATION_FAILED`.

So an export always carries a value that a client reads through the base.

The contract has no `scheduled` processor. A transition is scheduled by its
`schedule` (`scheduled-transitions.md`).

What Cloud has today: its `ProcessorDefinition` maps `externalized` and
`scheduled` (`openapi-workflow.yml:710-726`), and
cyoda-go's copy of that spec still shows the `scheduled` type
(`docs/cyoda/api/openapi-workflow.yml:717`, example at `:473`). Cloud's
export writes no `type` for an externalized processor and `"scheduled"` for a
scheduled one
(`backend/src/main/kotlin/net/cyoda/saas/model/workflow/mapper/CyodaToCloudWorkflowMapper.kt:167-182`).

## 10. What Cloud changes

1. **Event-type names.** Rename the event-type values to cyoda-go's, in the
   outer `eventType`, in the `StateMachineEvent` mapping and inside `data`:
   `STARTED`→`STATE_MACHINE_START`, `FINISHED`→`STATE_MACHINE_FINISH`,
   `CANCELLED`→`CANCEL`, `FORCED_SUCCESS`→`FORCE_SUCCESS`,
   `WORKFLOW_SKIPPED`→`WORKFLOW_SKIP`, `TRANSITION_MADE`→`TRANSITION_MAKE`,
   `TRANSITION_CRITERION_NOT_MATCHED`→`TRANSITION_NOT_MATCH_CRITERION`,
   `PROCESS_CRITERION_NOT_MATCHED`→`PROCESS_NOT_MATCH_CRITERION`,
   `PROCESSING_PAUSED`→`PAUSE_FOR_PROCESSING`. Cloud has these values in the
   outer `eventType` enum (`openapi-audit.yml:672-688`), in
   `StateMachineEventType` (`:805-821`) and in the mapping (`:826-841`), and
   maps its internal type to them in
   `backend/src/main/kotlin/net/cyoda/saas/audit/source/StateMachineEventFunctions.kt:15-91`
   and `:100-113`. Cloud's internal `StateMachineEvent.Type` already uses
   cyoda-go's names (`StateMachineEventFunctions.kt:101-113`).
2. **Growth by addition (section 3).** Make the outer `eventType` a plain
   string with no `enum`, and every discriminator property a plain string whose
   mapping lists the known values: today `StateMachineEventType`
   (`openapi-audit.yml:805-821`), `AuditEventType` (`:534-540`) and
   `QueryConditionType` (`openapi-common.yml:67-75`) are `enum`s. From then on,
   add and never rename or remove a mapping key, an `enum` value or an event
   type. Keep `data` out of `required`, as it is today (`openapi-audit.yml:689-693`).
3. **New event types.** Add `TRANSITION_ABORTED` and the five
   `SCHEDULED_TRANSITION_*` types, with the data of section 4, where Cloud has
   the behaviour.
4. **`functions`** on WORKFLOW_FOUND, WORKFLOW_SKIP, TRANSITION_MAKE and
   TRANSITION_NOT_MATCH_CRITERION (section 4).
5. **STATE_PROCESS_RESULT (section 5).** Send `processorName`,
   `executionMode`, `success` and `failReason` with cyoda-go's rule for
   `failReason`. Stop sending `stateProcessId`, `stateProcessClassName` and
   `stateProcessType`. Today Cloud sends the three and `failReason`
   (`StateMachineEventFunctions.kt:80-90`; `openapi-audit.yml:1035-1055`).
6. **No placeholders (section 4).** Send a field only when its value is known.
   Today Cloud fills START and FINISH `state` with `"UNKNOWN STATE"`
   (`StateMachineEventFunctions.kt:18`, `:23`), the outer `state` with
   `"UNKNOWN"`
   (`backend/src/main/kotlin/net/cyoda/saas/audit/source/StateMachineAuditEventSource.kt:124`),
   a missing stop reason with an `UNKNOWN` one (`StateMachineEventFunctions.kt:24-28`),
   and a missing criterion reason with `"Not specified"`
   (`backend/src/main/kotlin/net/cyoda/saas/audit/source/StopReasonComputer.kt:57`).
   The outer `state` and START's `state` are optional; Cloud's spec requires
   them today (`openapi-audit.yml:691-693`, `:857-858`).
7. **TRANSITION_MAKE and TRANSITION_NOT_FOUND.** TRANSITION_MAKE has `newState`,
   as Cloud has it. TRANSITION_NOT_FOUND requires `transition`; Cloud's is
   optional and nullable (`openapi-audit.yml:949-963`).
8. **FINISH stop reason (section 6).** Compute `rejectedTransitions` from the
   settled state's last pass only. Cloud accumulates every refusal of the
   transaction (`StopReasonComputer.kt:53-59`).
9. **`…/finished` (section 7).** Return the whole audit event. Today Cloud
   returns `data` alone
   (`backend/src/main/kotlin/net/cyoda/saas/controller/EntityAuditController.kt:49-56`;
   `backend/src/main/kotlin/net/cyoda/saas/audit/EntityAuditInteractor.kt:62-86`;
   `openapi-audit.yml:376`).
10. **U+0000 (section 8).** Refuse an event in which any string holds U+0000,
    as cyoda-go does: record nothing for it, log without the text, and do not
    fail the operation. Cloud records such an event today.
11. **Processor `type` (section 9).** Store an omitted or empty `type` as
    `externalized` and export it as `externalized`; refuse any other value at
    import with `400 VALIDATION_FAILED`, `scheduled` included. A scheduled
    transition is written with `schedule` (`scheduled-transitions.md`).
12. **No change of form.** cyoda-go now uses Cloud's form for every
    polymorphic type (section 2). Nothing changes on Cloud's side for it,
    except the processor mapping, which loses `scheduled` (item 11).
13. **Criterion reason length (section 8).** Keep a criterion reason in the
    audit trail up to the contract's length, 2048 characters
    (`criterion-stoppage-reason.md` §1). Today the store cuts it to 1000
    (`CQLUtils.java:570`).
