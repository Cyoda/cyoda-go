# Discriminator mappings, and typed state-machine event data

Issue #625. Tracker #651, row 8. Base: `release/v0.9.0`. Research:
`docs/superpowers/research/2026-10-14-625-state-machine-event-data-research.md`.

Standing rulings that bind this spec: a contract gap is never fixed by removing API function; cyoda-go's
event-type names are kept and Cloud renames its values; a value cyoda-go does not truly have is never sent
(no substituted or made-up value); there are no production instances, so a breaking change is declared, not
avoided; the contract grows by addition, and clients tolerate what was added after they were generated (ADR 0003
decision 4); a published value is never renamed or removed.

## 1. Terms

- **Discriminator** — the property of a polymorphic object that says which shape the object has, such as `type`
  on a condition.
- **Mapping** — the `discriminator.mapping` table: for each value of the discriminator, the schema of that shape.
  Without one, OpenAPI defines the value as the schema's name (`SimpleConditionDto`), which is not what the wire
  carries.
- **Base** — a schema with a discriminator and a mapping. Its **members** are the schemas the mapping names; each
  includes the base with `allOf`. A **use** of the base is any `$ref` to it other than a member's `allOf` entry.
- **Union** — a `oneOf` or `anyOf` list.
- **Untyped leaf** — a schema with none of `type`, `$ref`, `allOf`, `oneOf`, `anyOf`. In JSON Schema it accepts
  every value its other keywords allow, `null` included unless its `not` excludes `null`.
- **Client generator** — openapi-generator 7.18.0, the tool and version Cloud generates its classes with
  (`~/dev/cyoda/client/pom.xml:445-500`): `kotlin-spring` with Cloud's settings, and `java` (library `native`,
  Jackson).
- **Event** — one state-machine audit event (`StateMachineAuditEventDto`). Its **type** is `eventType`. Its
  **data** is the `data` member.
- **Stop reason** — why a state-machine run stopped: `WorkflowStopReasonDto`.
- **Member text** — a string a compute member sends in its answer: a failure message, a criterion reason, a
  warning, a scheduled function's result kind.

## 2. Rules

**Rule 1 — polymorphism has one form: a base.**

- Every polymorphic type is a base: a discriminator and a mapping that names every member. Each member includes
  the base with `allOf`, and every schema that includes the base is in its mapping.
- Every use refers to the base with `$ref`. No `oneOf` or `anyOf` has a discriminator.
- A schema includes at most one base.

This is Cloud's form. The client generator reads polymorphism only in this form. For any `oneOf`, inline or
named, `kotlin-spring` generates an interface that the member classes do not implement, so every read fails
("Class `SimpleConditionDto` not subtype of …"); no generator setting changes it. A member of two bases fails the
same way. The `java` generator ignores a discriminator on a `oneOf`. All of this was measured on prototypes of
this spec.

**Rule 2 — every event has typed data.** `data` is a use of the base `StateMachineEventDto`, keyed on
`eventType`. The server sends it on every event, never `null`; the schema does not require it (Rule 6). The event type is repeated inside `data`, as Cloud does: a
member is told apart by a property inside the object.

**Rule 3 — data states only what is true.** A field is present when the engine knows its value at the point the
event is recorded, and absent otherwise. Nothing is filled with a placeholder.

**Rule 4 — no text an audit event carries holds U+0000.** The PostgreSQL audit row is a `jsonb` document
written inside the request's transaction (`plugins/postgres/sm_audit_store.go:31-36`). PostgreSQL refuses U+0000
in `jsonb`, and the refused `INSERT` aborts the whole transaction; memory and SQLite store the same event. So every
text an event carries is refused where it enters, with nothing recorded (section 8.2): a compute answer at
dispatch, a workflow at import, a collection-update body at its handler, and a caller's transition name or
If-Match at the start of the run. A criterion's reason is copied into later events of the run (FINISH's stop
reason, the `functions` of TRANSITION_MAKE and WORKFLOW_FOUND), so this is also what keeps a committed operation's
trail complete.

**Rule 5 — the client generator reads every shape.** The spec generates Kotlin (Cloud's settings) and Java that
compile, and every sample (section 13.2) reads into the right class and writes back unchanged. What this
requires, each measured on prototypes of this spec:

- **No union of scalar types.** For a `oneOf` of scalars, or a `type` list with more than one non-null type,
  `kotlin-spring` generates a class that cannot hold a scalar ("Cannot construct instance of
  `SimpleConditionDtoAllOfValue`"). A value that may be one of several scalar types is an untyped leaf whose
  other keywords state the rule (section 3.4); `kotlin-spring` generates `Any?` for it, and a `type` list inside
  its `items` or `not` generates nothing. The only `oneOf` or `anyOf` allowed is `[{type: "null"}, {$ref}]`:
  `kotlin-spring` reads it as a nullable reference.
- **No inline enum on a property of a base.** `kotlin-spring` does not compile one: each member's class names
  the enum as its own (`StateMachineAuditEventDto.Severity`), which does not exist. Such an enum is a named
  schema. An inline enum on a member's own property compiles (`EntityChangeAuditEventDto.changeType`).
- **No member declares its base's discriminator property.** A member that declares it, for example as a
  one-value `enum`, fails to compile ("'type' hides member of supertype"). The mapping states the set of values.

**Rule 6 — the contract grows by addition only.** This applies ADR 0003 decision 4 to the forms of Rule 1.

- **Additions are not breaking.** A field, a member of a base and an event type can be added. A client
  generated from an earlier spec still reads every response, if it does what the contract requires of a client:
  - it ignores a field it does not know (ADR 0003 decision 3);
  - it reads an object whose discriminator value it does not know as no object: an audit event of an unknown
    kind is an empty entry in `items`, and the `data` of an event of an unknown type is empty. The event type
    itself is still readable.

  For a Jackson client these are two settings: `FAIL_ON_UNKNOWN_PROPERTIES` and `FAIL_ON_INVALID_SUBTYPE`, both
  disabled. Spring Boot's mapper and the `java` generator's mapper already disable the first. Both generators
  were probed on a prototype of this spec with a future event type, a future audit kind and an added field. With
  the two settings, all three read as described. With the defaults, each one fails the whole page.
- **A use of a base in a response is never a required property.** `kotlin-spring` makes a required property
  non-null, so an object of an unknown member, read as empty, would fail the whole response. A spec test walks
  every schema a response reaches and asserts that no property that is a `$ref` to a base is in its parent's
  `required`.
- **So a value set that grows is a plain string, never an `enum`.** Every discriminator property is a plain string,
  and its mapping lists the known values. The outer `eventType` is a plain string whose known values are the keys
  of `data`'s mapping (section 3.3). A `kotlin-spring` client cannot read a value that a required response `enum`
  does not list, whatever its settings. The generated `forValue` throws; with `READ_UNKNOWN_ENUM_VALUES_AS_NULL`
  the value becomes `null`, which the non-nullable property refuses; the generator's `enumUnknownDefaultCase`
  option adds a case that `forValue` never returns (all measured). So adding a value to a response `enum` is a
  breaking change, and the oasdiff gate reports it as one (section 13.3). The remaining `enum`s (severity, execution mode, the stop
  reason type, the operators) are value sets that this change does not expect to grow.
- **Nothing published is renamed or removed.** This covers mapping keys, `enum` values and event types. A value
  that cyoda-go stops sending stays, and its description says it is not sent (as for `WORKFLOW_NOT_FOUND`,
  section 4). Stored audit events therefore always carry a value the contract lists, and no stored event is ever
  rewritten. The oasdiff gate reports a removal (section 13.3).
- **The server sends no event type that the mapping lacks.** A spec test collects every constant of type
  `spi.StateMachineEventType` declared in the SPI and in cyoda-go, by parsing the Go source as the exit-check tests
  do (`internal/fence/check_call_sites_test.go`). It asserts that this set equals the keys of
  `StateMachineEventDto`'s mapping.

## 3. Wire contract (`api/openapi.yaml`)

### 3.1 Conditions

- One base, `QueryConditionDto`: `type` required, a plain string; discriminator `type`; mapping `array` →
  `ArrayConditionDto`, `function` → `FunctionConditionDto`, `group` → `GroupConditionDto`, `lifecycle` →
  `LifecycleConditionDto`, `simple` → `SimpleConditionDto`. These are Cloud's five members.
- `AbstractConditionDto` and `CriterionGroupConditionDto` are removed. Simple, Lifecycle, Array and Group include
  `QueryConditionDto` (Function already does). The two group schemas differ only in their `conditions` items
  (`api/openapi.yaml:9687-9786`), so one `GroupConditionDto` remains, with `conditions.items` a use of
  `QueryConditionDto`.
- Every condition union becomes a use of `QueryConditionDto`:
  - the direct and async search bodies;
  - the stats `condition`;
  - `WorkflowConfigurationDto.criterion` and `TransitionDefinitionDto.criterion`;
  - the conditional `DELETE /entity/{entityName}/{modelVersion}` body (`:2271`, today `AbstractConditionDto`);
    its descriptions at `:2199` and `:2353` change to name `QueryConditionDto`.
- The schema no longer says where `function` is valid; each description does. The server rule does not change:
  - In a search-shaped body, a `function` condition at any depth is refused with 400 `INVALID_CONDITION`
    (`internal/domain/search/operators.go:163-172`). e2e already covers direct, async, stats and conditional
    delete, and nesting in a group and in `NOT` (`internal/e2e/search_function_condition_test.go:72-140`).
  - A criterion accepts `function` at any depth.
- The function pre-check `ExternalizedFunctionDto.criterion` is `oneOf: [{type: "null"}, {$ref: QueryConditionDto}]`,
  with no discriminator and its `x-go-type` as today. A `function` anywhere in it is refused at import with
  400 `VALIDATION_FAILED`, as today; the description (`:9655-9660`) keeps saying so.
- `GroupConditionDto.operator` becomes the named enum `GroupOperator`, Cloud's name. Rule 5 does not require it
  (a member's property); it aligns the two specs.
- `api/condition_union_test.go` and `api/condition_dto_test.go` assert the old union form. They are rewritten to
  the form above.

### 3.2 Audit events

- `AuditEventDto` keeps its discriminator `auditEventType` and gains the mapping `EntityChange`, `StateMachine`,
  `System`.
- `EntityAuditEventsResponseDto.items.items` becomes a use of `AuditEventDto` (today an inline union,
  `:11589`).
- `AuditEventDto.severity` becomes the named enum `AuditEventSeverity` (Cloud's name), by Rule 5.

### 3.3 State-machine event data

- `StateMachineEventDto` (the base) keeps its discriminator on `eventType` and gains a mapping of all 19 event
  types (section 4) to their schemas. Every event schema includes it with `allOf`. `StateMachineFinishedDto`
  gains that `allOf`.
- `StateMachineAuditEventDto.data` becomes `$ref: StateMachineEventDto`. It is not in `required`, so that a
  client reads an event of an unknown type (Rule 6); the server always sends it (Rule 2), which the tests of
  section 10 assert. Its `x-go-type` is removed. The server keeps building the response as a map (section 6).
- `TestSpecDeclaresNoNullableKeyword` (`api/condition_dto_test.go:200-221`) lists the values that accept `null`
  and checks them in both kin modes. `data` leaves the list, which then has three entries. The test, and
  `TestArrayConditionDtoValuesValidateUnderOpenAPI31` (`:136`), validate through the view of section 13.1:
  without it, kin's default mode refuses `null` for an untyped leaf.
- The outer `eventType` loses its `enum` and becomes a plain string (Rule 6). Its description says that its
  known values are the keys of `data`'s mapping and that a client accepts any other value. The description
  (`api/openapi.yaml:11667-11674`) also stops describing TRANSITION_ABORTED's data (it names `transitionName`);
  that data is the member schema's.
- No event member declares `eventType` (Rule 5); the base's `eventType` stays a plain string.
- `TRANSITION_NOT_MATCH_CRITERION`'s `criterion` description becomes what the value is: the function's name when a
  function condition is the whole criterion, otherwise the condition's type (`simple`, `group`, …)
  (`engine.go:135-155`; `docs/cloud-parity/criterion-stoppage-reason.md:67`).
- Generated Go constant names change with the named enums. Only tests use them
  (`api/scheduled_tasks_contract_test.go:239`); they are updated.
- New schemas: `StateMachineTransitionAbortedDto`, `StateMachineScheduledTransitionArmedDto`,
  `…FiredDto`, `…ExpiredDto`, `…CancelledDto`, `…FailedDto`, `CriterionFunctionResultDto` (a `functions` entry).
- Removed fields: `StateMachineStateProcessResultDto.stateProcessId`, `.stateProcessClassName`,
  `.stateProcessType`; the schema `StateProcessIdDto`, which nothing else references. Reason: for an externalized
  processor Cloud fills `stateProcessId` with an internal cache key that no endpoint accepts, and
  `stateProcessClassName` with one constant Java class name; cyoda-go has no true value for either (Rule 3).
  The outcome is `success`, as on FINISH.
- The `data` description and the per-schema descriptions replace today's prose list (`api/openapi.yaml:11713-11730`).

### 3.4 Scalar values and polymorphic bodies

Each of these is today a union of scalar types or inline members (Rule 5). Each becomes an untyped leaf that
accepts exactly the same values:

| Where | Today | Becomes |
|---|---|---|
| `SimpleConditionDto.value`, `LifecycleConditionDto.value`, each entry of `ArrayConditionDto.values` | `oneOf` of `null`, a scalar, a `[low, high]` pair of scalars | `not: {type: object}`, `minItems: 2`, `maxItems: 2`, `items: {type: [string, number, boolean]}` |
| `GroupKeyEntry.value` (stats response) | `type: [string, number, boolean, null]` | `not: {type: [object, array]}` |
| the entity-create body and the model-import body | `oneOf` of an object and an array of objects | `not: {type: [string, number, boolean, "null"]}`, `items: {type: object}` |

`minItems`, `maxItems` and `items` apply only when the value is an array. The descriptions say in prose what
the keywords say. `kotlin-spring` generates `Any?` for each of these, and the round-trip of every value form
passes in Kotlin and Java, `GroupKeyEntry.value` included. The `java` generator also writes uncompilable Java
for today's create body (`List<Map<String, Object>>.class`); the untyped form compiles. The create and import
body parameters are generated as `Any`/`Object`, so for them the check proves only that the code compiles.

### 3.5 Processors

- `TransitionDefinitionDto.processors.items` becomes a use of `ProcessorDefinitionDto` (today a `oneOf` of its
  one member, `:10446`). `ProcessorDefinitionDto.type`, its discriminator, is a plain string; the mapping lists
  its values (Rules 5 and 6).
- Import checks `type` (Gate 6, fail closed). Today import does not constrain it (`cyoda-go-spi types.go:222-229`,
  `internal/domain/workflow/validate.go:36-40`). Any value other than `internalized` runs as `externalized`, and
  the value is stored and exported as given, so an export can carry a type no client can read through the base.
  After this change:
  - an omitted or empty `type` is stored as `externalized`, which is what the engine runs. The export therefore
    always carries a mapped value;
  - `internalized`, and any other value, is refused at import with 400 `VALIDATION_FAILED`.
- The engine's refusal of `internalized` at dispatch (`engine_processors.go:137-147`) becomes unreachable and is
  deleted, with its STATE_PROCESS_RESULT record. The tests that drive it (`engine_processors_type_test.go`,
  `handler_type_validation_test.go`, and the internalized case of `engine_processors_audit_test.go`) are
  rewritten to the import refusal, or deleted where that is their only subject. The schema description stops
  describing a dispatch-time refusal.
- This narrows what import accepts, so it is recorded in `docs/workflow-schema-versioning.md` as a tightening of
  the 1.5 contract. It falls under rule 1 of the doc's rubric: an unknown value that was silently accepted now
  fails loudly.

### 3.6 Examples and the document check

kin-openapi's document validation (`doc.Validate` with `EnableMultiError`) reports seven errors on today's spec:
- three examples that do not match their schemas: `TokenResponseDto`, the token example on
  `POST /tenants/{tenant}/oauth/token`, and the XML example on `PUT /entity/{format}/{entityId}/{transition}`;
- `bearerAuth` declaring `name` (`api/openapi.yaml:12342`), which an `http` scheme does not have;
- three path-template conflicts (`/entity/{format}` with `/entity/{entityId}`, and two more).

The examples and `bearerAuth` are corrected. The path conflicts are the API's real routes, and kin has no switch
to skip that check. So the `api/` test does not call `doc.Validate`. It walks the spec and validates every
example against its schema through the view of section 13.1. A wrong example then fails the build. The prototype
of this spec also reported `GroupedStatsRequest`'s example `2026-01-01 12:00:00+00:00`, which is not RFC 3339
`date-time`; the walk covers it, and the example or the schema is corrected to agree with what the server
accepts.

After the edit: `go generate ./api`, and commit the result.

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
| `SCHEDULED_TRANSITION_FAIL` | `…FailedDto` | `transition` R, `sourceState` R, `reason` R (an open string; the five FAIL reasons are its known values, as on `ScheduledTaskDto.failureReason`), `attempts` R, `lostOwners` R |

`CriterionFunctionResultDto`: `path` R, `name` R, `called` R, `preCheck` O (a `QueryConditionDto`),
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
    mode runs as `SYNC` (`internal/domain/workflow/validate.go:46-55`, `engine_processors.go:167`). The dispatch span
    attribute (`internal/observability/dispatch_tracing.go:155`) gets the same rule. The enum is the four modes.
  - `failReason` is present only when the member itself sent failure text: a `contract.CalloutFailure` of kind
    `MemberFailed` (found with `errors.As`) whose new field `MemberText` is non-empty (section 8.1). Every other
    failure (the member sent no text, no member, no answer, unreadable answer, savepoint failure, platform error)
    has no `failReason`. No platform text enters `data`: today's fixed
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
processor's own record (`engine_processors.go:194-211`; the internalized refusal at `:137-147` is deleted,
section 3.5). The checkpoint exit before dispatch
(`:127`) has no processor result to report. The fence exit after dispatch (`:178`) has no per-processor record,
and the caller's record is not written there either, because `recordEvent` returns at once under a fence
(`engine.go:1086-1088`). The removed record's `details` embedded the wrapped error text (`"…: %v"`). That text
reaches the audit trail where the failed run's events survive: a joined callback, whose audit rows commit with
the owner's transaction. A batch-isolated item is not such a place: a collection update isolates an item only on
its `If-Match` conflict, which the engine raises before any processor runs, so the item keeps only
STATE_MACHINE_START and TRANSITION_ABORTED (`internal/domain/entity/service.go:2617-2632`, `:2758-2765`).
Removing the record is also a Gate 3 fix.
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

### 8.2 Where U+0000 is refused

- A compute answer whose `reason` or `error` holds U+0000 is unreadable (`internal/grpc/dispatch.go`); the owner
  does not believe a peer's relayed answer carrying one (`internal/cluster/dispatch/handover.go` `readAnswer`).
- Workflow import and the collection update run `ingest.RejectUnstorable` on their raw bodies: `400 BAD_REQUEST`.
- `refuseNULCallerText` (`internal/domain/workflow/caller_text.go`) refuses a transition name or an If-Match
  value holding U+0000 at the start of `manualTransition` and `loopback`, before anything is recorded:
  `400 BAD_REQUEST`, on every door, a joined write included.

The engine's audit write does not check the text again: no input reaches it with U+0000.

`scheduler.failWithAudit` (`internal/scheduler/service.go:925-946`) records the task's transition and state names,
from a workflow import, and fixed enum values.

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

**Direct search, async search, stats, conditional delete**

| Status | Code | When |
|---|---|---|
| 400 | `INVALID_CONDITION` | a `function` condition at any depth, as today. The schema now admits the shape (section 3.1); the server refuses it |
| all others | as today | unchanged |

**Workflow import**

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_FAILED` | a `function` condition inside a function's pre-check, as today |
| 400 | `VALIDATION_FAILED` | **new**: a processor `type` other than `externalized`, omitted or empty (`internalized` included) |
| 200 | — | **new**: an omitted or empty processor `type` is stored, and exported, as `externalized` |
| all others | as today | unchanged |

**Entity create, update, PATCH, collection update, manual transition, scheduled firing — processors**

| Status / gRPC envelope | Code | When |
|---|---|---|
| 400 | `WORKFLOW_FAILED` | no longer for an `internalized` processor: import refuses that workflow |

**Entity create, update, PATCH, collection update, manual transition (HTTP and gRPC), scheduled firing**

| Status / gRPC envelope | Code | When |
|---|---|---|
| 400 | `BAD_REQUEST` | a transition name or an If-Match value holding U+0000, refused before the run records anything, on every door, a joined write included |
| 400 | `WORKFLOW_FAILED`, not retryable | a compute member's answer whose `reason` or `error` holds U+0000: the answer is unreadable (Terminal, none of the member's text), wherever its failure fails the operation; a peer's relayed answer carrying such text is not believed |
| 400 | `BAD_REQUEST` | a collection update whose body holds U+0000, an unpaired surrogate or invalid UTF-8 outside an item's payload (an `ifMatch`, a `transition`), refused whole before any item runs, since a failed item is isolated while its siblings commit |
| 400 | `BAD_REQUEST` | a workflow import whose body holds U+0000, an unpaired surrogate or invalid UTF-8 (a workflow's names reach the audit trail), as for an entity payload |

## 10. Coverage matrix

Layers: **U** unit, **E** e2e HTTP on PostgreSQL (`internal/e2e`), **G** gRPC (`internal/grpc`), **P**
cross-backend parity (`e2e/parity`, registered in `registry.go`), **M** multi-node (hand-over to a peer), **S**
spec test in `api/` (it reads the embedded spec through `GetSwagger`), **C** the client-generation check
(section 13.2).

The e2e OpenAPI validator in enforce mode checks the shape of every response the full e2e suite makes, with the
view of section 13.1. The scenarios below also assert values.

| Scenario | U | E | G | P | M | S | C |
|---|---|---|---|---|---|---|---|
| Rule 1: every discriminator is on a base; its mapping names every schema that includes the base and nothing else; no `oneOf`/`anyOf` has a discriminator; no schema includes two bases | | | | | | ✓ | |
| Rule 5, static part: no `type` list with two non-null types, except inside the `items` or `not` of an untyped leaf; no `oneOf`/`anyOf` other than `[{type: "null"}, {$ref}]`; no inline enum on a property of a base; no member declares its base's discriminator property | | | | | | ✓ | |
| Rule 6, static part: every discriminator property is a plain string with no `enum`; the outer `eventType` has no `enum`; no use of a base in a response is a required property | | | | | | ✓ | |
| The `spi.StateMachineEventType` constants declared in the SPI and in cyoda-go equal the keys of `StateMachineEventDto`'s mapping | | | | | | ✓ | |
| Every example in the spec validates against its schema through the view; `bearerAuth` has no `name` | | | | | | ✓ | |
| Every sample marked valid validates against its schema through the view, and every sample marked invalid fails; every mapping entry of every base has a valid sample; each value form of section 3.4 has samples (string, number, boolean, `null`, pair valid; object and triple invalid) | | | | | | ✓ | |
| Kotlin and Java compile; every valid sample reads into the class its mapping names and writes back equal | | | | | | | ✓ |
| Future samples, read with the two settings of Rule 6: an audit event of an unknown kind → an empty entry; an event of an unknown type → the event, with its `eventType`, and empty `data`; an added field → ignored. Read without the two settings, each fails | | | | | | | ✓ |
| Validator view: a member-field defect under a use of a base is reported; an unmapped discriminator value is reported; `null` passes an untyped value leaf; an object fails it; `null` fails an untyped leaf whose `not` excludes it; `null` fails an `allOf` member (a whole condition, a whole audit event) and a replaced use (`data`) | ✓ | | | | | | |
| Processor `type`: omitted or empty → stored and exported as `externalized`; `internalized` → 400 `VALIDATION_FAILED`; another value → 400 `VALIDATION_FAILED` | ✓ | ✓ | | | | | |
| `go generate ./api`: no generated name still holds `AbstractConditionDto` or `CriterionGroupConditionDto`; the grep exit check of section 12 is empty | | | | | | ✓ | |
| `data` refuses null; absent `data` validates (Rule 6); the two never-sent event types: a literal sample validates | | | | | | ✓ | |
| A `function` condition in each search-shaped body is refused with 400 `INVALID_CONDITION` | | ✓ (existing) | | | | | |
| Conditional DELETE with a typed body still deletes only matches | | ✓ (existing) | | | | | |
| Every event type cyoda-go sends: the data produced by driving the engine path validates against the member schema its `eventType` maps to, through the view (kin-openapi), not hand-built maps | ✓ | | | | | | |
| Create → START (no `state`), WORKFLOW_FOUND `workflowName`, TRANSITION_MAKE `newState`, FINISH `stopReason` `COMPLETED_NO_AUTO_TRANSITIONS` | ✓ | ✓ | | ✓ | | | |
| FINISH `COMPLETED_ALL_CRITERIA_REJECTED` with `rejectedTransitions` of the last pass only (an earlier state's refusal not listed) | ✓ | ✓ | | ✓ | | | |
| Loopback with the state not in the selected workflow → FORCE_SUCCESS, FINISH `stopReason` `FORCE_SUCCESS` | ✓ | | | | | | |
| `eventType` inside `data` equals the outer `eventType`, on search and `…/finished`; nil stored data → `{eventType}` | ✓ | ✓ | | ✓ | | | |
| PAUSE_FOR_PROCESSING `processNames`; STATE_PROCESS_RESULT `processorName`, `executionMode`; one record per failed processor | ✓ | ✓ | | | | | |
| Empty stored execution mode → `executionMode: SYNC` | ✓ | ✓ | | | | | |
| `failReason` present: `ASYNC_NEW_TX` processor whose member answers `success:false` with a message | ✓ | ✓ | | | ✓ | | |
| `failReason` absent: member answers `success:false` with no message (local and handed over); callout timeout | ✓ | | | | ✓ | | |
| TRANSITION_ABORTED: `transition` on a manual transition; absent on a loopback | ✓ | ✓ | | | | | |
| CANCEL `reason` (visit limit) | ✓ | | | | | | |
| TRANSITION_NOT_FOUND `workflowName`, `transition` | ✓ | | | | | | |
| SCHEDULED_TRANSITION_FIRE and fire-time EXPIRE data; CANCEL `workflowName` at the fire-time sites only | ✓ | ✓ | | | | | |
| SCHEDULED_TRANSITION_ARM / FAIL data (existing assertions updated) | ✓ | ✓ | | ✓ | | | |
| Member `success:false` with no text, handed over → client text `"<kind> returned failure"`, as locally; `failReason` absent | ✓ | | | | ✓ | | |
| A `MemberFailed` try after an earlier `NoAnswer` try → `failReason` is that member's text | ✓ | | | | | | |
| A member answer whose `reason` or `error` holds U+0000 is unreadable: Terminal, the fixed message, no member text, no warnings read (criterion reason, criterion and processor failure text, an `error` on a success answer) | ✓ | | | | | | |
| An automated transition behind a criterion whose member reason holds U+0000: the create fails with 400 `WORKFLOW_FAILED`, not retryable, and commits nothing, on memory, SQLite and PostgreSQL | | ✓ | | ✓ | | | |
| The same through a hand-over (the peer refuses the answer) | | | | | ✓ | | |
| A peer's relayed answer whose reason or failure text holds U+0000 is not believed | ✓ | | | | | | |
| A transition name or If-Match holding U+0000 → 400 `BAD_REQUEST`, no event recorded (manual transition, If-Match on a transition and on a loopback) | ✓ | ✓ | | | | | |
| A collection update or workflow import whose body holds U+0000 → 400 `BAD_REQUEST`, nothing committed | | ✓ | | | | | |
| `…/finished` returns FINISH with typed data | | ✓ | | ✓ | | | |

Failure-path events (CANCEL, TRANSITION_NOT_FOUND, a failed SYNC processor) roll back with the request on the
owner path, so their values are asserted in unit tests, and in e2e only where a path makes them durable (an
`ASYNC_NEW_TX` processor). A batch-isolated item makes only STATE_MACHINE_START and TRANSITION_ABORTED durable.

Existing tests that read changed keys are updated: `engine_processors_audit_test.go:69-100` (the duplicate
record), `engine_transition_aborted_test.go:119-130`, `criterion_function_audit_test.go:259`, `:287` (nil
`data`), `workflow_selection_test.go:246`, `internal/e2e/collection_update_ifmatch_test.go:264-276`,
`callback_joined_commit_test.go:405-411`, `scheduled_transition_test.go:75-86`, the parity helpers in
`e2e/parity/audit.go`, `api/condition_dto_test.go:208-210`, `api/scheduled_tasks_contract_test.go:239`, and
every `api/` test that asserts the old condition unions (`api/condition_union_test.go`,
`api/condition_dto_test.go`).

The concurrency rule does not apply: nothing here is a race.

## 11. Cloud (Gate 7)

New `docs/cloud-parity/state-machine-event-data.md` (and the `README.md` index), and a CaaS ticket (`[CaaS]`
prefix, component CaaS). Cloud must:

- make the outer `eventType` a plain string, as cyoda-go does (Rule 6), and follow Rule 6 from then on: add,
  never rename or remove;
- rename its event-type values to cyoda-go's, in the outer `eventType`, the union mapping and inside `data`
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
- no change of form: cyoda-go now uses Cloud's form for every polymorphic type (Rule 1), with the same five
  condition members and the same three audit members. Schema names differ (`QueryConditionDto` here,
  `QueryCondition` there), as they do elsewhere. cyoda-go's scalar values are untyped schemas with constraints
  where Cloud writes `type: any` (section 3.4). The ticket says that cyoda-go's CI now generates Cloud's
  `kotlin-spring` classes from cyoda-go's spec (section 13.2);
- Rule 4: text holding U+0000 is refused where it enters, so no audit event holds it (check what Cloud's store
  does with such text before filing).

Updated parity docs: `criterion-stoppage-reason.md` (`data` now typed), `entity-if-match.md:27`
(`transitionName` → `transition`, absent on loopback), `scheduled-transitions.md` (FIRE and EXPIRE data),
`function-conditions-in-criteria.md` (`CriterionFunctionResultDto`; the criterion and pre-check schemas are uses
of `QueryConditionDto`), `openapi-conformance.md` (Rules 1, 5 and 6, and the client-generation check).

## 12. Documentation

- `cmd/cyoda/help/content/audit.md`: the FINISH example with `stopReason`; the per-event data table; "null for
  most event types" goes; `transition` on TRANSITION_ABORTED; `…/finished` for a scheduled fire.
- `cmd/cyoda/help/content/workflows.md`, `crud.md`, `grpc.md`: every statement of an event's data.
- `docs/ARCHITECTURE.md:1758-1776` (the event table); `docs/PROCESSOR_EXECUTION_MODES.md:412-436` (the record's
  fields, and names the code does not emit: `STATE_MACHINE_PROCESSING_PAUSED`, `STATE_MACHINE_CANCELLED`).
- The doc comment at the top of `internal/domain/workflow/transition_aborted.go` (the data example).
- `docs/cloud-parity/callout-failover.md`: `failReason` is the member's own text only; a hand-over with no member
  text gives the same client text as a local try.
- `cmd/cyoda/help/content/audit.md`: text holding U+0000 is refused where it enters.
- `CHANGELOG.md` `### Breaking`:
  - `data` is always typed and never null, with `eventType` inside; the new data fields;
  - TRANSITION_ABORTED `transitionName` → `transition`; the STATE_PROCESS_RESULT fields; one record per failed
    processor;
  - text holding U+0000 is refused where it enters, so no audit event holds it;
  - every polymorphic type is a base with a mapping: `AbstractConditionDto` and `CriterionGroupConditionDto` are
    removed, and every condition is a `QueryConditionDto`;
  - the scalar-value and polymorphic-body schemas of section 3.4; the named enums;
  - the outer `eventType` is a plain string (Rule 6); a client reads an unknown audit kind or event type as
    Rule 6 states, and needs the two settings to do so;
  - workflow import refuses a processor `type` other than `externalized` (`internalized` included) and stores an
    omitted one as `externalized`.
- `COMPATIBILITY.md`: the SPI pin.
- New `docs/adr/0005-openapi-client-generation.md`, beside ADR 0003, and the `docs/adr/README.md` index. It
  records Rules 1, 5 and 6, why each holds (the measured generator behaviour), the validator view of section 13.1,
  the client-generation check of section 13.2 and the severity file of section 13.3. It applies ADR 0003
  decision 4 to the base form and does not change it: the "unknown/default variant" is the empty object a client
  reads, since neither generator can declare such a variant. ADR 0003 gets a one-line pointer to ADR 0005.
- `cmd/cyoda/help/content/openapi.md`, a new section on generated clients: what Rule 6 promises and requires (no
  help topic states the consumer obligation of ADR 0003 decision 3 today), the two Jackson settings, and what a
  client reads for an unknown audit kind or event type.
- `CONTRIBUTING.md`: the client-generation check, how to run it locally (`make client-probe`, which needs Java 21
  and Maven), and how to add a sample; Rule 6 for a contributor: a value set that grows is a plain string, nothing
  published is renamed or removed, and the severity file is why the gate refuses either.
- `docs/workflow-schema-versioning.md`: the processor-`type` tightening under 1.5 (section 3.5).
- Every live mention of the removed schema names changes to `QueryConditionDto` or `GroupConditionDto`:
  - help: `cmd/cyoda/help/content/crud.md:329`, `cmd/cyoda/help/content/errors/INVALID_CONDITION.md:15`, `:29`;
  - docs: `docs/cloud-parity/function-condition-search-rejection.md:59`,
    `docs/cloud-parity/function-conditions-in-criteria.md`, `docs/cloud-parity/openapi-conformance.md`,
    `docs/analysis/openapi/README.md`;
  - code comments: `internal/common/error_codes.go:142`, `internal/domain/workflow/validate.go:16-18`, `:29-30`
    (the generated constant names), `internal/e2e/entity_delete_conditional_test.go:26`,
    `internal/e2e/transaction_control_test.go:61`.

  Exit check: `grep -rn "AbstractConditionDto\|CriterionGroupConditionDto" --exclude-dir=.git .` lists only
  history: `CHANGELOG.md`, `docs/release-notes/`, `docs/superpowers/`, `.github/oasdiff-err-ignore.txt`, and
  Cloud's copy `docs/cyoda/openapi.yml`.

## 13. Tooling and CI

### 13.1 The validator view (`internal/e2e/openapivalidator`)

kin-openapi v0.149.0, which the e2e validator runs, differs from JSON Schema in two ways that matter here. Both
were measured against a prototype of this spec:

- It checks a value against a base's own properties only. `{"type":"simple"}`, a simple condition with wrong
  field types, and `{"type":"nosuch"}` all pass `QueryConditionDto`. It follows a mapping only on a `oneOf`.
- It refuses `null` for an untyped leaf ("Value is not nullable"), which JSON Schema accepts. It also accepts
  `null`, before any other check, for every schema it considers nullable (`openapi3/schema.go:2021-2026`).

The validator therefore validates against a view of the spec, built once when the spec is loaded. The published
spec does not change.

1. Each use of a base is replaced by a `oneOf` of the base's mapping targets that carries the base's
   discriminator. Each member `$ref` is spelled as in the mapping, so kin and the recursive walk pick the mapped
   member (`recursive.go:374-407`). A member's `allOf` entry is left as it is.
2. Each untyped leaf is marked nullable, unless its `not` excludes `null`.
3. Each union with a `{type: "null"}` branch is marked nullable. kin accepts any value for a schema it is
   already visiting higher up, so `null` that reaches a recursive member through the pre-check
   (`FunctionConditionDto` → `ExternalizedFunctionDto.criterion` → `FunctionConditionDto`) matches both
   branches of the `oneOf` and is refused. Marked nullable, the union accepts `null` before its branches are
   tried, which is the answer it gives without the recursion.

No other schema is marked: a member (an `allOf`), a replaced use (a `oneOf`) and any typed schema keep
refusing `null`. The same kin rule means kin accepts any value at a position where a schema recurs: `null`, `42`
or `"x"` in a group's `conditions`, and anything below a group in a group or a function in a pre-check. The
validator's recursive walk (`recursive.go`) checks those values with a fresh visit, and `ValidateValue` runs
kin and the same walk for the `api/` tests.

One function builds the view. The validator and the `api/` tests (sections 3.3, 3.6, 13.2) all use it, so they
check samples, examples and responses the same way. On the prototype, the view gives JSON Schema's answer in
every case tried: `null`, a scalar and a pair are accepted as a condition `value`; an object and a triple are
refused; `null` is refused as a whole condition, as a whole audit event, and as `data`; `null` is accepted and an
array refused as `GroupKeyEntry.value`.

### 13.2 The client-generation check

A new CI job generates client classes from `api/openapi.yaml` and proves that they read and write the wire
shapes. It is a merge gate, like oasdiff.

- **`api/clientprobe/`**, a Maven project:
  - It runs `openapi-generator-maven-plugin` **7.18.0**, pinned like oasdiff and bumped on purpose. It runs twice:
    - `kotlin-spring` with Cloud's `configOptions` (`~/dev/cyoda/client/pom.xml:446-500`), models and API
      interfaces;
    - `java`, library `native`, Jackson, with `containerDefaultToNull=true`, so that an absent list stays absent.
  - It compiles both, then runs one JUnit test per generator over every valid sample. The test reads the sample's
    value as its schema's class and asserts the concrete class. It writes the value back and compares the two
    JSON documents after it removes, from each document on its own, every object member whose value is `null`.
    The reason: Cloud serializes `non_null`
    (`~/dev/cyoda/backend/src/main/resources/config/local-dev/application.properties:29`), so a `null` the sample
    holds is absent in the output, and for these fields an explicit `null` and an absent member mean the same
    thing. `null` entries inside arrays are kept. A valid sample states every property that has a schema
    `default`, because the generated classes write an absent defaulted property back as its default; a spec
    test checks that each one is present.
- **Samples**, `api/clientprobe/samples/*.json`, each one `{schema, class, valid, future, added, value}`; `valid`
  defaults to `true` and `future` to `false`. `class` is the member the base's discriminator selects, or the
  schema itself. A request body that is not a component schema (the create and import bodies) is named by
  `body: "<METHOD> <path>"` in place of `schema`, with no `class`: the generated clients take it as an untyped
  value, so the client test skips it; the spec test validates it as a request. The client test skips invalid
  samples. A **future sample** (`future: true`) is a response that a later
  version of the spec could send: an audit event of an unknown kind, an event of an unknown type, a known event
  with an added field. Its `added` lists, as JSON pointers into its value, the members the spec does not
  declare; a spec test asserts that the list is exact. The client test reads it with the two settings of Rule 6
  and asserts the result Rule 6 states; for a known event, the event written back equals the sample's with
  exactly the `added` members removed. It also reads it without the two settings and asserts that the read fails
  with the exception of its case, which proves the sample exercises the case. An `api/` spec test (S) validates each value that is not a future sample against its schema with
  `ValidateValue`, through the view of section 13.1: a valid sample must pass and an invalid one must fail. It
  also checks coverage:
  - every mapping entry of every base has a sample;
  - the value forms of section 3.4 have samples;
  - the pre-check has samples present, `null` and absent;
  - the create and import bodies have samples for an object and for an array;
  - there is one audit page holding each audit member, and FINISH with a stop reason;
  - invalid samples hold a non-object entry in a group's `conditions`, at the top and in a nested group;
  - the three future samples.
- **CI**: `.github/workflows/openapi-client-generation.yml`. It runs on every pull request and on every push to
  `main` and `release/**`, with no path filter: a required check with a path filter stays pending on a PR that
  does not touch those paths. It uses Temurin Java 21 with the Maven cache and runs
  `mvn -B -f api/clientprobe/pom.xml verify`. Making it a required check is a repository setting (Paul).
- **Locally**: `make client-probe` runs the same command. It is not part of `make test`, which needs only Go
  and Docker.

### 13.3 oasdiff

oasdiff v1.21.0 rates the changes Rule 6 forbids below `error`, so the gate (`--fail-on ERR`) passes them today:
adding a value to a response `enum` is a warning, and removing a mapping key or a response `enum` value is info.
A new file, `.github/oasdiff-severity-levels.txt`, passed with `--severity-levels`, raises these checks to
`err`:

- `response-property-enum-value-added`;
- `response-property-enum-value-removed`, `response-mediatype-enum-value-removed`, `request-body-enum-value-removed`;
- `response-body-discriminator-mapping-deleted`, `response-property-discriminator-mapping-deleted`,
  `request-body-discriminator-mapping-deleted`, `request-property-discriminator-mapping-deleted`;
- the four `…-discriminator-property-name-changed` checks.

A new mapping key stays `info`: it is an addition. Measured on the prototype with the file: an added event type
passes the gate (a new mapping key, the outer `eventType` being open); a removed event type fails it (a deleted
mapping key); an added value of a response `enum` fails it. The `internal/oasdiffcheck` premise test
(`oasdiff_gate_test.go`) gains these three cases.

With the file, a prototype of this spec's form (sections 3.1–3.5, the outer `eventType` opened) gives 40 ERR
lines. Every one is a change of form, not a value the server stops accepting:

| Count | oasdiff id | Why no working client breaks |
|---|---|---|
| 6 | `new-required-request-property` | `type` was already required in every condition member; `name` was already required in the only processor member |
| 4 | `request-body-one-of-removed` | the condition members are reached through the base; the create and import bodies accept the same values |
| 4 | `request-property-one-of-removed` | as above, for the stats `condition`, the criteria and the processors |
| 2 + 4 | `request-body-type-changed`, `request-property-type-changed` | every member was already an object |
| 1 | `response-property-type-changed` | `GroupKeyEntry.value`: oasdiff reads the untyped leaf as `any`; the value set is the same |
| 19 | `response-property-enum-value-removed` | the outer `eventType` is opened (Rule 6): oasdiff reports each value of the dropped `enum` as removed, but every value is still sent and is still a key of `data`'s mapping |

The prototype does not have the processor-`type` import check of section 3.5, which narrows accepted input and
is declared under `### Breaking`. The lines for the final spec are measured again and go into
`.github/oasdiff-err-ignore.txt` after review, with these reasons. `data` and the audit items give no ERR.

### 13.4 Others

- **codegen sync**: `go generate ./api` output committed.
- The full `internal/e2e` package runs, so `TestOpenAPIConformanceReport` sees every response through the view.

## 14. Not in this change

- cassandra: no change (it stores the event as an opaque blob and records none itself).
- gRPC and CloudEvents: no surface carries these events.
- START and FINISH on a scheduled fire, and a stop reason on SCHEDULED_TRANSITION_FIRE: the scheduled-transition
  design records neither; unchanged.
- Other client generators (TypeScript, Python, C#): not probed. The check covers Cloud's generator and the Java
  one.
- Request validation in e2e: the validator checks responses only, as today. Request shapes are covered by the
  samples (section 13.2).
