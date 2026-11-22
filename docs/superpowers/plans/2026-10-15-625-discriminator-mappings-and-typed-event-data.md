# Discriminator mappings and typed state-machine event data — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every polymorphic type in `api/openapi.yaml` becomes a base with a mapping that Cloud's client generator reads. Every state-machine audit event carries typed `data`, under Rule 6 (the contract grows by addition only). A CI job proves that the generated Kotlin and Java clients read the wire.

**Architecture:** The contract work is in `api/openapi.yaml` and spec tests in `api/`. The e2e validator and the spec tests share one *view* of the spec, which makes kin-openapi give JSON Schema's answers. The engine builds each event's data through one constructor per event type. The audit output adds `eventType` inside `data`. A new Maven project, `api/clientprobe`, generates and exercises the clients in CI.

**Tech Stack:**
- Go 1.26;
- kin-openapi v0.149.0, oapi-codegen (`go tool oapi-codegen`);
- oasdiff v1.21.0;
- openapi-generator 7.18.0 (`kotlin-spring`, `java` native), Maven, Temurin Java 21.

**Spec:** `docs/superpowers/specs/2026-10-14-625-discriminator-mappings-and-typed-event-data-design.md`. Section numbers below (§N) refer to it. Read it with this plan: the tables of §3.4, §4, §5 and §9 are the values, and this plan does not copy them.

## Global Constraints

- Base: `release/v0.9.0`. Branch `worktree-fix-625-discriminator-mappings`. The PR targets `release/v0.9.0`.
- Pinned versions:
  - openapi-generator **7.18.0** (`openapi-generator-maven-plugin`);
  - oasdiff **v1.21.0**;
  - kin-openapi as in `go.mod`;
  - Java: Temurin **21**.
- Event-type names are cyoda-go's (`STATE_MACHINE_START`, `TRANSITION_MAKE`, …). Nothing published is renamed or removed (Rule 6).
- Rule 3: a `data` field is present only when the engine knows its true value. Nothing is filled with a placeholder.
- No issue numbers (`#625`, …) in code, comments, error text, OpenAPI descriptions or help. They are allowed only in commits, the PR and specs or plans.
- `log/slog` only. 4xx errors carry the domain detail; 5xx errors carry a generic message and a ticket UUID.
- After every edit of `api/openapi.yaml`: run `go generate ./api` and commit `api/generated.go` with it. `make check-codegen` must pass.
- Verification:
  - while iterating: the touched packages, then `make test`, then `go vet ./...`;
  - the full suite: CI on the PR head. Never run `make test-full` or `make race` locally.
  - e2e: `go test -timeout 30m ./internal/e2e/... -run <Name>` while iterating (Docker required), without `-v` and without `-count=1`.
- The SPI change goes as a PR into `cyoda-go-spi` `main`, with no tag. cyoda-go pseudo-pins it, and `COMPATIBILITY.md` is updated in the same change.

## Review Focus

These five inputs are implied by the spec but not named by any matrix row. Each has a test in the task named after it.

1. **The batch create path** (`internal/domain/entity/service.go:2085-2095`) loses the `"CREATED"` fallback and the `StopReason` guard just as the single create does. A batch-created entity must keep the same state and `TransitionForLatestSave`. Test: Task 10.
2. **FINISH on a manual transition and on a loopback**, not only on create. Each of the three entry points records the stop reason of the pass it settled on. Test: Task 10.
3. **U+0000 inside typed slices.** `functions` is a `[]any` of `map[string]any` and `processNames` is a `[]string`. A walk that handles only `[]any` and `map[string]any` misses a `[]string` and a `[]map[string]any`. Test: Task 14.
4. **Base uses that the view must replace in every position:** inside `oneOf [null, $ref]` (the pre-check), inside array `items` (group `conditions`, audit `items`), and through recursion (a group inside a group). Test: Task 1.
5. **A stored `data` map shared with the store.** The memory backend can hand back the stored map itself. Output that sets `eventType` on it in place would change stored history, or race with another reader. Test: Task 8 reads the same event twice from the memory backend and checks that the store's map is unchanged.

---

## Streams

- **A, the contract:** Tasks 1 → 2 → 3 → 4 → 5 → 6. One file, so it runs in sequence.
- **B, the callout:** Task 7. It is independent and can run in parallel with A from the start.
- **C, the engine and output:** Tasks 8 → 9 → 10 → 11 → 12 → 13 → 14 → 15. Task 9 needs Task 3, Task 12 needs Task 7, and Task 15 needs every C task.
- **Last:** Task 16 (the oasdiff gate), then Task 17 (docs), then Task 18 (Cloud).

---

### Task 1: The validator view

**Files:**
- Create: `internal/e2e/openapivalidator/view.go`, `internal/e2e/openapivalidator/view_test.go`
- Modify: `internal/e2e/openapivalidator/validator.go:49` (`NewValidator` applies the view)

**Interfaces:**
- Produces: `func ApplyView(doc *openapi3.T) error`, in package `openapivalidator`. It changes a freshly loaded document in place (`api.GetSwagger()` parses a new one on every call). Tasks 2–6 and 9 call it, and `NewValidator` calls it.

- [ ] **Step 1: Write the failing tests** in `view_test.go`. They use a small inline spec loaded with `loadSpec` (`recursive_test.go:88`). The spec has:
  - a base `Shape` with discriminator `kind`, mapping `a → A`, `b → B`;
  - `A` with `allOf: [Shape, {required: [x], properties: {x: {type: integer}}}]`, and `B` the same with `y: {type: string}`;
  - `Holder` with these properties:
    - `one: $ref Shape`;
    - `many: {type: array, items: $ref Shape}`;
    - `opt: {oneOf: [{type: "null"}, {$ref: Shape}]}`;
    - `leaf: {not: {type: object}}`;
    - `notNull: {not: {type: [object, "null"]}}`;
  - a recursive `G` (member `g` of `Shape`) with `items: {type: array, items: $ref Shape}`.

  `TestApplyView` has these sub-tests. Each validates with `VisitJSON` under both entries of `schemaValidators` (copy the two-option table from `api/condition_dto_test.go:131`):
  - `member_defect_under_use_is_reported`: `{"one":{"kind":"a","x":"no"}}` fails;
  - `unmapped_value_is_reported`: `{"one":{"kind":"zzz"}}` fails;
  - `defect_in_array_items_and_nested_group_is_reported`: `{"many":[{"kind":"g","items":[{"kind":"a","x":"no"}]}]}` fails;
  - `defect_inside_nullable_oneOf_is_reported`: `{"opt":{"kind":"a","x":"no"}}` fails, and `{"opt":null}` passes;
  - `null_passes_untyped_leaf`: `{"leaf":null}` passes, and `{"leaf":{}}` fails;
  - `null_fails_leaf_whose_not_excludes_it`: `{"notNull":null}` fails;
  - `null_fails_member_and_replaced_use`: `{"one":null}` fails, and `A` visited with `nil` fails;
  - `member_allOf_entry_left_alone`: after `ApplyView`, `A.AllOf[0].Ref` is still `#/components/schemas/Shape`.
- [ ] **Step 2: Run** `go test ./internal/e2e/openapivalidator/ -run TestApplyView`. Expected: FAIL to compile (`undefined: ApplyView`).
- [ ] **Step 3: Implement `ApplyView`** by §13.1:
  1. Find every base (a schema with a discriminator and a mapping).
  2. Walk every schema, each one once (dedupe by pointer). Visit properties, `items`, `additionalProperties`, `allOf`, `oneOf`, `anyOf` and `not`, and also request bodies, parameters and responses.
  3. Replace each `*SchemaRef` whose `Ref` names a base, except an `allOf` entry, with `{Value: &Schema{OneOf: <one $ref per mapping target, spelled exactly as in the mapping>, Discriminator: base.Discriminator}}`.
  4. Set `Nullable = true` on each untyped leaf (no `Type`, no `$ref` target, no `AllOf`, `OneOf` or `AnyOf`), unless its `Not` has a type list that includes `"null"`.
- [ ] **Step 4: Make `NewValidator` call `ApplyView(doc)`.** Return its error. Then run `go test ./internal/e2e/openapivalidator/`. Expected: PASS, the existing tests included.
- [ ] **Step 5: Commit** `feat(openapivalidator): a view of the spec that gives JSON Schema's answers`.

### Task 2: One polymorphic form: Rules 1, 5 and 6 in the spec

**Files:**
- Modify: `api/openapi.yaml` (§3.1, §3.2, §3.4, §3.5 schema part, the Rule 6 parts of §3.3), `api/generated.go`
- Create: `api/spec_rules_test.go`
- Rewrite: `api/condition_union_test.go` (the old `oneOf` mappings at :17-26 go), `api/condition_dto_test.go` (validate through `ApplyView`)
- Modify: `api/scheduled_tasks_contract_test.go:215-240` (`TestStateMachineAuditEventType_HasScheduledTransitionFail` checks the mapping key, not the removed outer `enum`), `internal/domain/search/array_clause_spec_example_test.go:21-23` if the example moves

**Interfaces:**
- Consumes: `openapivalidator.ApplyView` (Task 1).
- Produces:
  - schema names `QueryConditionDto` (base), `GroupConditionDto` (the only group), `AuditEventSeverity`, `GroupOperator`;
  - `StateMachineEventDto` with a mapping for the event schemas that exist today;
  - the outer `eventType` as a plain string.

- [ ] **Step 1: Write the failing tests** in `api/spec_rules_test.go`. Each loads `GetSwagger()` and walks every schema with a dedupe helper.
  - `TestRule1_EveryDiscriminatorIsOnABase`:
    - every schema with a `discriminator` has a non-empty `mapping`;
    - no `oneOf`/`anyOf` carries a discriminator;
    - the set of component schemas whose `allOf` includes base X equals the set of X's mapping targets;
    - no schema includes two bases.
  - `TestRule5_GeneratorReadableShapes`:
    - no `type` list with two non-null types, except under the `items` or `not` of an untyped leaf;
    - every `oneOf`/`anyOf` is exactly `[{type: "null"}, {$ref}]`;
    - no inline `enum` on a property of a base;
    - no member declares its base's discriminator property.
  - `TestRule6_ValueSetsThatGrowAreStrings`:
    - every discriminator property of a base is `type: string` with no `enum`;
    - `StateMachineAuditEventDto`'s `eventType` has no `enum`.
  - `TestConditionsAreUsesOfTheBase`: each condition position listed in §3.1 is a `$ref` to `QueryConditionDto`. These are the search, async search, stats and conditional `DELETE` bodies, the two `criterion` properties and `GroupConditionDto.conditions.items`. The pre-check is `oneOf [null, $ref QueryConditionDto]`.
  - Rewrite `TestConditionUnionsDiscriminateByType` and `TestCriterionShapesValidate` so that they validate through `ApplyView`, keeping their accept/refuse cases. A `function` condition now validates in a search body: the server refuses it, not the schema (§3.1).
- [ ] **Step 2: Run** `go test ./api/ -run 'TestRule|TestConditions'`. Expected: FAIL, with every rule reporting today's violations.
- [ ] **Step 3: Edit `api/openapi.yaml`:**
  - §3.1:
    - make `QueryConditionDto` the base with Cloud's five-member mapping;
    - remove `AbstractConditionDto` and `CriterionGroupConditionDto`;
    - make every condition position a `$ref`;
    - give the pre-check `oneOf [null, $ref]`;
    - add the named enum `GroupOperator`;
    - update the descriptions at `:2199` and `:2353`;
    - the description of each search-shaped body states that `function` is refused there with 400 `INVALID_CONDITION`.
  - §3.2: the `AuditEventDto` mapping; `items` as a `$ref`; the named enum `AuditEventSeverity`.
  - §3.3, form only:
    - `StateMachineEventDto` gets a mapping for the existing event schemas, and each includes it with `allOf`;
    - `StateMachineFinishedDto` gains the `allOf`;
    - no member declares `eventType`;
    - the outer `eventType` drops its `enum`. Its description says that its known values are the keys of `data`'s mapping and that a client accepts any other value.
  - §3.4: the three untyped-leaf forms, with the description text stating each rule in prose.
  - §3.5 schema part: processors `items` is a `$ref ProcessorDefinitionDto`, and its `type` is a plain string.
- [ ] **Step 4: Run `go generate ./api`.** Then `go build ./... && go vet ./...`. Fix the compile fallout: the generated constant names change, and only tests use them.
- [ ] **Step 5: Update `TestSpecDeclaresNoNullableKeyword`** (`api/condition_dto_test.go:166`). Its null checks run through `ApplyView`. `StateMachineAuditEventDto.data` stays in its list until Task 3. Also update `TestArrayConditionDtoValuesValidateUnderOpenAPI31` (`:136`) to validate through `ApplyView`.
- [ ] **Step 6: Run** `go test ./api/ ./internal/e2e/openapivalidator/ ./internal/domain/search/`. Expected: PASS.
- [ ] **Step 7: Commit** `feat(api)!: one polymorphic form — a base with a mapping, used by $ref`.

### Task 3: The state-machine event schemas

**Files:**
- Modify: `api/openapi.yaml` (§3.3, the §4 table, §5), `api/generated.go`, `api/condition_dto_test.go` (the null list)
- Create: `api/event_schemas_test.go`

**Interfaces:**
- Consumes: Task 2's base `StateMachineEventDto`.
- Produces:
  - `StateMachineEventDto`'s mapping of all 19 event types;
  - the new schemas `StateMachineTransitionAbortedDto`, `StateMachineScheduledTransition{Armed,Fired,Expired,Cancelled,Failed}Dto` and `CriterionFunctionResultDto`;
  - `data` required, with no `x-go-type`.

  Task 9's constructors must produce exactly these properties.

- [ ] **Step 1: Write the failing tests** in `api/event_schemas_test.go`:
  - `TestEventTypeConstantsEqualTheMapping`:
    - parse the Go source with `go/parser`, as `internal/fence/check_call_sites_test.go:213` does;
    - parse the SPI's `types.go`, located with `go list -m -f '{{.Dir}}' github.com/cyoda-platform/cyoda-go-spi` run through `exec.Command`, and `internal/domain/workflow/*.go` (not tests);
    - collect the string values of every constant of type `StateMachineEventType`;
    - assert that the set equals the keys of `StateMachineEventDto.discriminator.mapping`, and that the set has 19 members.
  - `TestEventSchemasMatchTheDataTable`: for each row of §4, the member schema's own properties and `required` list equal the table's. Write the table into the test as a Go map.
  - `TestStopReasonSchema`: `WorkflowStopReasonDto` has `type`, `description` and `rejectedTransitions` (items `RejectedTransitionCriterionDto`), and its `type` enum keeps Cloud's values (§5).
  - `TestNeverSentEventsHaveValidSamples`: a literal sample of `PROCESS_NOT_MATCH_CRITERION` and one of `WORKFLOW_NOT_FOUND` validate against `StateMachineEventDto` through `ApplyView`.
  - `TestDataIsRequiredAndNeverNull`: under the view, an audit event with `data: null` fails and one with `data` absent fails.
- [ ] **Step 2: Run** `go test ./api/ -run 'TestEvent|TestStopReason|TestNeverSent|TestDataIs'`. Expected: FAIL.
- [ ] **Step 3: Edit `api/openapi.yaml`** by §3.3, §4 and §5:
  - the 19-entry mapping, and the new schemas;
  - remove `stateProcessId`, `stateProcessClassName` and `stateProcessType`, and the schema `StateProcessIdDto`;
  - `data: $ref StateMachineEventDto`, required, with `x-go-type` removed;
  - the `criterion` description; the `data` description replacing `:11713-11730`;
  - the "never sent by cyoda-go" descriptions (§4, §5).

  Then run `go generate ./api`.
- [ ] **Step 4: Update `TestSpecDeclaresNoNullableKeyword`.** `data` leaves the list, which now has three entries, and the count check becomes 3.
- [ ] **Step 5: Run** `go test ./api/ ./internal/e2e/openapivalidator/` and `go build ./...`. Expected: PASS.
- [ ] **Step 6: Commit** `feat(api)!: typed state-machine event data — every event type mapped`.

### Task 4: Examples and the example walk

**Files:**
- Modify: `api/openapi.yaml`: the `TokenResponseDto` example, the token example on `POST /tenants/{tenant}/oauth/token`, the XML example on `PUT /entity/{format}/{entityId}/{transition}`, `bearerAuth`'s `name` (`:12342`), and the `GroupedStatsRequest` date example
- Create: `api/examples_test.go`

- [ ] **Step 1: Write the failing test** `TestEveryExampleValidates`. It walks every `example` and `examples` of every schema, media type and parameter. It validates each against its schema through `ApplyView`, skipping non-JSON media types: the XML example is checked as text by its schema `type: string`. It also asserts that `bearerAuth` has no `name`. It does not call `doc.Validate` (§3.6: kin reports the API's real routes as path conflicts).
- [ ] **Step 2: Run** `go test ./api/ -run TestEveryExampleValidates`. Expected: FAIL on the five examples and `bearerAuth`.
- [ ] **Step 3: Correct the examples and `bearerAuth`.** The `GroupedStatsRequest` date becomes RFC 3339 (`2026-01-01T12:00:00Z`) unless the server accepts the space form. Check `internal/domain/search` stats parsing before you choose, and make the example and the schema agree with what the server accepts. Then run `go generate ./api`.
- [ ] **Step 4: Run** `go test ./api/`. Expected: PASS.
- [ ] **Step 5: Commit** `fix(api): examples that match their schemas; bearerAuth has no name`.

### Task 5: Samples and the sample spec test

**Files:**
- Create: `api/clientprobe/samples/*.json`, `api/samples_test.go`

**Interfaces:**
- Produces: the sample format `{"schema": "<component name>", "class": "<expected concrete class simple name>", "valid": true|false, "future": true|false, "value": <json>}`. `valid` defaults to `true`, and `future` to `false`. Task 6 reads this format.

- [ ] **Step 1: Write the failing test** `TestSamples` in `api/samples_test.go`:
  - every non-future sample with `valid: true` passes its schema through `ApplyView`, and every sample with `valid: false` fails;
  - a `future` sample is not validated: by construction it is not in the current mapping;
  - `class` names an existing component schema.

  It also checks coverage, by §13.2:
  - every mapping entry of every base has a valid sample;
  - the §3.4 value forms (string, number, boolean, `null` and a pair are valid; an object and a triple are invalid) for `SimpleConditionDto.value`, an `ArrayConditionDto.values` entry and `GroupKeyEntry.value`;
  - a pre-check that is present, `null` and absent;
  - a create body that is an object and one that is an array;
  - one audit page holding each audit member, and FINISH with a stop reason;
  - the three future samples of §13.2: an unknown audit kind, an unknown event type, and a known event with an added field.
- [ ] **Step 2: Run** `go test ./api/ -run TestSamples`. Expected: FAIL (no samples).
- [ ] **Step 3: Write the samples.** Use the prototype's shapes as a starting point, extended to all 19 event types:
  - `/private/tmp/claude-501/-Users-paul-go-projects-cyoda-light-cyoda-go/15ac8d0e-86fc-4853-9240-1681d2f0d654/scratchpad/gen/BProbe.kt`;
  - `/private/tmp/claude-501/-Users-paul-go-projects-cyoda-light-cyoda-go/15ac8d0e-86fc-4853-9240-1681d2f0d654/scratchpad/gen/OpenProbe.kt`.

  If they are gone, the §4 table is enough.
- [ ] **Step 4: Run** `go test ./api/ -run TestSamples`. Expected: PASS.
- [ ] **Step 5: Commit** `test(api): wire samples for every mapped member and value form`.

### Task 6: The client-generation check

**Files:**
- Create:
  - `api/clientprobe/pom.xml`;
  - `api/clientprobe/src/test/kotlin/probe/KotlinSamplesTest.kt`;
  - `api/clientprobe/src/test/java/probe/JavaSamplesTest.java`;
  - `.github/workflows/openapi-client-generation.yml`
- Modify: `Makefile` (target `client-probe`, added to `.PHONY`), `.gitignore` (`api/clientprobe/target/`)

**Interfaces:**
- Consumes: Task 5's sample format.

- [ ] **Step 1: Write `pom.xml`.** It holds `openapi-generator-maven-plugin` **7.18.0** with two executions on `../openapi.yaml`:
  - **`kotlin-spring`:** Cloud's `configOptions`, verbatim from `~/dev/cyoda/client/pom.xml:446-500`:
    - `dateLibrary=java8`, `serializationLibrary=jackson`, `useCoroutines=true`, `interfaceOnly=true`;
    - `requestMappingMode=api_interface`, `useSpringBoot3=true`, `reactive=true`, `modelMutable=true`;
    - `useTags=true`, `useBeanValidation=true`, `beanQualifiers=false`, `useFlowForArrayReturnType=true`;
    - `modelNameSuffix` stays unset, so the class names are the schema names.
  - **`java`:** `library=native`, `serializationLibrary=jackson`, `containerDefaultToNull=true`, `hideGenerationTimestamp=true`.

  Add the Kotlin and Java compile plugins, the Spring/Jackson/validation dependencies that the generated code needs (take them from the prototype project's `pom.xml`), JUnit 5, and `kotlin-test`.
- [ ] **Step 2: Write `KotlinSamplesTest` and `JavaSamplesTest`.** Each loads every sample under `../samples`.
  - **A valid sample:**
    - read the value as `org.openapitools.model.<schema>` (Java: `org.openapitools.client.model.<schema>`);
    - assert the class simple name equals `class`;
    - write it back and compare the two trees, after removing every object member whose value is `null` from each tree on its own, and keeping `null` array entries.

    The Kotlin mapper is a plain `ObjectMapper` with the Kotlin module, `findAndRegisterModules()`, `NON_NULL`, and `WRITE_DATES_AS_TIMESTAMPS` and `ADJUST_DATES_TO_CONTEXT_TIME_ZONE` disabled. The Java mapper is the generated `JSON().getMapper()` with `NON_NULL`.
  - **A future sample:**
    - read it with `FAIL_ON_UNKNOWN_PROPERTIES` and `FAIL_ON_INVALID_SUBTYPE` disabled, and assert Rule 6's result:
      - an unknown audit kind → the `items` entry is `null`;
      - an unknown event type → `eventType` equals the value and `data` is `null`;
      - an added field → the read succeeds with the known fields equal;
    - then read it with both settings enabled, and assert that the read throws.
  - Invalid samples are skipped.
- [ ] **Step 3: Run** `mvn -B -f api/clientprobe/pom.xml verify`. Expected: BUILD SUCCESS, with both test classes reporting every sample. Then remove one mapping entry from a copy of the spec, point the pom at it, and check that the run fails. This proves the check can fail. Restore the pom afterwards.
- [ ] **Step 4: Write `.github/workflows/openapi-client-generation.yml`:**
  - `on: pull_request` and `push` to `main` and `release/**`, with no `paths` filter;
  - `actions/setup-java` with Temurin 21 and `cache: maven`;
  - `run: mvn -B -f api/clientprobe/pom.xml verify`.

  Add the `make client-probe` target, which runs the same command.
- [ ] **Step 5: Commit** `ci: generate Cloud's Kotlin and the Java client from the spec and round-trip samples`.

### Task 7: The member's own words (`MemberText`)

**Files:**
- Modify:
  - `internal/contract/callout.go:78` (the field);
  - `internal/grpc/dispatch.go:266-272`;
  - `internal/cluster/dispatch/handover.go:327-331` (`fillFailure`) and `:420-428` (`readAnswer`)
- Test:
  - `internal/grpc/dispatch_member_text_test.go` (create);
  - `internal/cluster/dispatch/handover_test.go`;
  - `internal/callout/handover_test.go`;
  - `e2e/parity/multinode/callout_handover.go`

**Interfaces:**
- Produces: `contract.CalloutFailure.MemberText string`. For `MemberFailed` it is the member's own failure text, bounded, and empty when the member sent none. Task 12 reads it with `errors.As`.

- [ ] **Step 1: Write the failing tests:**
  - `TestLocalDispatch_MemberText`:
    - `resp.Error="boom"` → `MemberText=="boom"` and `Message=="boom"`;
    - `resp.Error=""` → `MemberText==""` and `Message==label+" returned failure"`;
    - a 600-rune error → `MemberText` is bounded exactly as `boundMemberText` bounds it.
  - `TestFillFailure_PutsMemberTextOnTheWire`: `MemberText=""` and `Message="processor returned failure"` → `resp.MemberError==""`.
  - `TestReadAnswer_MemberFailed_NoText`: `memberError=""` → `MemberText==""` and `Message==call.Kind.String()+" returned failure"`, not `peerFailedClientMessage`.
  - `TestReadAnswer_MemberFailed_Text`: `memberError="x"` → `MemberText=="x"` and `Message=="x"`.
  - `TestVerdict_MemberTextSurvivesEarlierNoAnswer`: a `NoAnswer` try, then `MemberFailed` with text → the final failure's `MemberText` is that text.
- [ ] **Step 2: Run** `go test ./internal/grpc/ ./internal/cluster/dispatch/ ./internal/callout/`. Expected: FAIL (unknown field).
- [ ] **Step 3: Implement** by §8.1. `peerFailedClientMessage` keeps its other use (`handover.go:450`). Update `TestReadAnswer_MemberFailed` (`handover_test.go:553`) to the new client text.
- [ ] **Step 4: Add the multi-node scenario** `RunCallout_HandOverMemberFailureWithoutText`. A member on node 1 answers `success:false` with no message, and the request goes through node 0. The 400 `WORKFLOW_FAILED` detail contains `"processor returned failure"`, as a local try's does. Register it in the file's `init`.
- [ ] **Step 5: Run** the packages above, then `make test`. Expected: PASS.
- [ ] **Step 6: Commit** `fix(callout): the member's own text is carried apart from the client text`.

### Task 8: Output: `data` with `eventType` inside

**Files:**
- Modify: `internal/domain/audit/events.go:73-110` (`stateMachineItem`)
- Test: `internal/domain/audit/events_test.go`, `internal/e2e/audit_event_data_test.go` (create), `e2e/parity/audit.go` (+ registry), `e2e/parity/registry_count_test.go`

- [ ] **Step 1: Write the failing tests:**
  - `TestStateMachineItem_DataCarriesEventType`:
    - stored `Data{"x":1}` and type `TRANSITION_MAKE` → `body["data"]` equals `{"x":1,"eventType":"TRANSITION_MAKE"}`;
    - stored `Data{"eventType":"WRONG"}` → the output's inner `eventType` is the event's type;
    - nil `Data` → `{"eventType":"<type>"}`;
    - after each case, the stored map is unchanged.
  - `TestStateMachineItem_DoesNotShareTheStoredMap` (Review Focus 5): through the memory backend's audit store, record an event, read the item twice, and check that the store's map holds no `eventType` key.
  - An e2e test `TestAudit_DataEventTypeEqualsOuter`: create an entity, then `GET /api/audit/entity/{id}` and `…/finished`. Every state-machine item has `data.eventType == eventType`.
  - A parity scenario `RunAuditDataEventTypeMatchesOuter`. Register it and bump `wantParityScenarioCount`.
- [ ] **Step 2: Run** `go test ./internal/domain/audit/`. Expected: FAIL.
- [ ] **Step 3: Implement** by §6: copy the map with `maps.Clone`, set `eventType`, and use `{eventType}` for nil data.
- [ ] **Step 4: Run** `go test ./internal/domain/audit/`, the e2e test and `make test`. Expected: PASS. The e2e validator (enforce) now checks `data` through the view on every e2e response.
- [ ] **Step 5: Commit** `feat(audit)!: state-machine data always carries its eventType`.

### Task 9: One constructor per event type

**Files:**
- Create: `internal/domain/workflow/audit_data.go`, `internal/domain/workflow/audit_data_test.go`

**Interfaces:**
- Consumes: Task 3's schemas, and Task 1's `ApplyView` in tests.
- Produces, in package `workflow`. Each returns `map[string]any`, and an optional argument that is absent leaves its key out:
  - `startedData(state string)`: `state` is omitted when empty (create).
  - `finishedData(state string, stop StopReason)`, with `type StopReason struct { Type, Description string; Rejected []RejectedTransition }` and `type RejectedTransition struct { TransitionName, Criterion, Reason string }`. These are the wire names of §5.
  - `cancelledData(reason string)`.
  - `forcedSuccessData()`.
  - `workflowFoundData(workflowName string, out *criterionOutcome)`: `out` may be nil; `functions` is added through `out.addFunctions`.
  - `workflowSkippedData(workflowName, reason string, out criterionOutcome)`.
  - `transitionMadeData(workflowName, transition, newState string, out *criterionOutcome)`.
  - `transitionNotFoundData(workflowName, transition string)`.
  - `transitionNotMatchedData(workflowName, transition, criterion, reason string, out criterionOutcome)`.
  - `transitionAbortedData(transition string, expectedTxID, actualTxID string)`: `transition` is omitted when empty (loopback); `reason` is `"ENTITY_MODIFIED"`.
  - `processingPausedData(workflowName, transition string, processNames []string)`.
  - `stateProcessResultData(processorName, executionMode string, success bool, failReason string)`: an empty `executionMode` is written as `"SYNC"`, and `failReason` is omitted when empty.
  - `ScheduledArmedData(transition, sourceState string, scheduledTimeMs int64)`, `ScheduledFiredData(transition, sourceState, workflowName string)`, `ScheduledExpiredData(transition, sourceState string)`, `ScheduledCancelledData(transition, sourceState, workflowName string)` (`workflowName` omitted when empty), and `ScheduledFailedData(transition, sourceState, reason string, attempts int, lostOwners bool)`. These are exported: the scheduler uses the FAIL one.
  - No constructor for `PROCESS_NOT_MATCH_CRITERION` or `WORKFLOW_NOT_FOUND`.

  Check `lostOwners`' type and `reason`'s values against `internal/scheduler/service.go:925-946` and the FAIL schema before you write the signature.

- [ ] **Step 1: Write the failing test** `TestConstructors_ValidateAgainstTheirMember`. It is table-driven, with one row per constructor and realistic arguments, the optional ones both present and absent. For each row:
  - add `eventType`;
  - validate against `StateMachineEventDto` through `ApplyView(api.GetSwagger())`;
  - assert the exact key set.

  Add `TestStateProcessResultData_EmptyModeIsSync`.
- [ ] **Step 2: Run** `go test ./internal/domain/workflow/ -run 'TestConstructors|TestStateProcessResultData'`. Expected: FAIL to compile.
- [ ] **Step 3: Implement the constructors.**
- [ ] **Step 4: Run** the tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(workflow): one constructor per state-machine event's data`.

### Task 10: Engine sites, the stop reason, and the unreachable branches

**Files:**
- Modify:
  - `internal/domain/workflow/engine.go`: every site in the table below; `cascadeAutomated` (`:861`); `selectWorkflow` (`:577`, `:620-622`); the FINISH sites `:365`, `:456`, `:533`, `:561`; `:536-544`;
  - `internal/domain/workflow/transition_aborted.go:56`;
  - `internal/domain/entity/service.go:312-325`, `:2085-2095`
- Test:
  - `internal/domain/workflow/engine_event_data_test.go` (create), `stop_reason_test.go` (create);
  - existing: `engine_transition_aborted_test.go:119-130`, `criterion_function_audit_test.go:259` and `:287`, `workflow_selection_test.go:246`;
  - e2e: `internal/e2e/audit_event_data_test.go`, `collection_update_ifmatch_test.go:264-276`;
  - parity: `e2e/parity/audit.go`

**Interfaces:**
- Consumes: Task 9's constructors.
- Produces: `cascadeAutomated(...) (retCtx context.Context, retTxID string, stop StopReason, retErr error)`.

Sites (engine.go line → constructor):

| Line | Constructor |
|---|---|
| 319, 421, 508 | `startedData` |
| 586, 598, 615 | `workflowFoundData` |
| 605 | `workflowSkippedData` |
| 754, 760, 766 | `transitionNotFoundData`; the three causes stay in `details` |
| 818, 943 | `transitionNotMatchedData` |
| 846, 973 | `transitionMadeData`, with `newState` the target |
| 903, 989 | `cancelledData` with today's reason text; `details` keeps its own sentence |
| 524 | `forcedSuccessData` |
| `transition_aborted.go:56` | `transitionAbortedData`; empty `transition` on a loopback, where today it records `"loopback"` (`engine.go:510`) |
| `engine_processors.go:107` | `processingPausedData` |

- [ ] **Step 1: Write the failing unit tests** in `engine_event_data_test.go`. Drive the engine with the memory audit store and assert the data keys of §4:
  - create → START has no `state`; WORKFLOW_FOUND has `workflowName`; TRANSITION_MAKE has `newState`;
  - a missing, a disabled and a scheduled transition → TRANSITION_NOT_FOUND has `workflowName` and `transition`;
  - the visit limit → CANCEL has `reason`;
  - TRANSITION_ABORTED has `transition` on a manual transition and none on a loopback;
  - PAUSE_FOR_PROCESSING `processNames` are in pipeline order.
- [ ] **Step 2: Write the failing stop-reason tests** in `stop_reason_test.go`:
  - `TestStop_NoAutomatedTransitions`: FINISH `stopReason.type == "COMPLETED_NO_AUTO_TRANSITIONS"`, with `description` exactly `No automated transition from state "<state>"`. Run it on Execute, ManualTransition and Loopback (Review Focus 2).
  - `TestStop_AllCriteriaRejected_LastPassOnly`: state A has an automated transition refused once, then the cascade moves to B, where every automated transition is refused. `rejectedTransitions` lists only B's refusals, in declaration order, with the same `criterion` and `reason` values as the matching TRANSITION_NOT_MATCH_CRITERION events. The description is §5's sentence.
  - `TestStop_ForceSuccess`: a loopback with the state not in the workflow → FORCE_SUCCESS, then FINISH `stopReason.type == "FORCE_SUCCESS"` with §5's description.
  - FINISH `state` is the state the run settled in, and `success` is `true`.
- [ ] **Step 3: Run** `go test ./internal/domain/workflow/ -run 'TestStop|TestEngineEventData'`. Expected: FAIL.
- [ ] **Step 4: Implement the sites and the stop reason.** `cascadeAutomated` returns the stop reason of the pass it settled on (§5), and Execute, manualTransition and loopback pass it to `finishedData`.
- [ ] **Step 5: Delete the unreachable branches** (§7.3), with tests:
  - the WORKFLOW_NOT_FOUND branch (the fallback becomes unconditional);
  - the "state not in workflow — stable" branch (`:908-911`);
  - `StopReason: "STATE_NOT_IN_WORKFLOW"`;
  - the `"CREATED"` fallbacks;
  - the two `if result.StopReason == ""` guards, which become unconditional assignments.

  `workflow_selection_test.go:246` asserts the FINISH stop reason. Add `TestBatchCreate_StateAndTransitionForLatestSave` (Review Focus 1): a batch create through `service.go:2085` sets the initial state and `TransitionForLatestSave` exactly as the single create does.
- [ ] **Step 6: Update the existing tests** listed under Files to the new keys.
- [ ] **Step 7: Add the e2e and parity scenarios:**
  - e2e `TestAudit_CreateEventData`: START with no `state`; WORKFLOW_FOUND `workflowName`; TRANSITION_MAKE `newState`; FINISH `COMPLETED_NO_AUTO_TRANSITIONS`.
  - e2e `TestAudit_FinishAllCriteriaRejected`.
  - e2e: `collection_update_ifmatch_test.go:264-276` asserts `transition` on a manual abort. Add `TestAudit_LoopbackAbortHasNoTransition` for a loopback with a stale `ifMatch`.
  - Parity `RunAuditCreateEventData` and `RunAuditFinishAllCriteriaRejected`. Bump the count. `RunAuditPostTxIdMatchesWorkflowFinished` also asserts that `…/finished` returns FINISH with `data.stopReason`.
- [ ] **Step 8: Run** `go test ./internal/domain/workflow/ ./internal/domain/entity/`, the new e2e tests and `make test`. Expected: PASS.
- [ ] **Step 9: Commit** `feat(workflow)!: typed data at every engine event; FINISH carries its stop reason`.

### Task 11: Remove the dead `ExecutionResult` fields (SPI)

**Files:**
- SPI repo `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`: `types.go:606-611`
- cyoda-go: `go.mod`, `plugins/*/go.mod` (through `make repin-plugins` if the plugins pin the SPI), `internal/domain/workflow/engine.go` (the result literals), `COMPATIBILITY.md`

- [ ] **Step 1:** In cyoda-go, delete every write of `Success`, `StopReason` and `Error` (`engine.go:370-373`, `:461-464`, `:537-541`, `:566-569`). `go build ./...` must pass, since the readers are gone after Task 10.
- [ ] **Step 2:** In the SPI, on a branch from `main`, remove `StopReason`, `Success` and `Error` from `ExecutionResult`. Run `go build ./... && go test ./...`. Push, and open a PR into `main` (follow `MAINTAINING.md`). Do not tag.
- [ ] **Step 3:** Pseudo-pin cyoda-go to the SPI PR's head commit (`go get github.com/cyoda-platform/cyoda-go-spi@<sha>`), then `make check-spi-pin-sync`. Check that `../cyoda-go-cassandra` does not reference the fields (`grep -rn "StopReason\|ExecutionResult" ../cyoda-go-cassandra` is empty). Update `COMPATIBILITY.md`.
- [ ] **Step 4:** Run `go build ./... && make test`. Expected: PASS.
- [ ] **Step 5: Commit** `chore(spi): drop ExecutionResult fields with no reader; pseudo-pin`.

### Task 12: Processor records

**Files:**
- Modify:
  - `internal/domain/workflow/engine_processors.go:194-211` (the per-processor record);
  - `internal/domain/workflow/engine.go:837-839`, `:965-968` (the caller's second record goes);
  - `internal/observability/dispatch_tracing.go:155` (an empty mode → `SYNC`)
- Test: `engine_processors_audit_test.go:69-100`, `internal/observability/dispatch_tracing_test.go`, `internal/e2e/audit_event_data_test.go`, `e2e/parity/multinode/`

**Interfaces:**
- Consumes: `stateProcessResultData` (Task 9) and `contract.CalloutFailure.MemberText` (Task 7).

- [ ] **Step 1: Write the failing tests:**
  - `TestProcessorRecord_Fields`: `processorName` is the workflow `name`; `executionMode` is the configured mode, `SYNC` for an empty one; `success`.
  - `TestProcessorRecord_FailReason`:
    - present only when `errors.As(err, &*contract.CalloutFailure)` gives `Kind == MemberFailed` and a non-empty `MemberText`, and then equal to that text;
    - absent for a member with no text, a callout timeout, no member, an unreadable answer, and a platform error.
  - `TestProcessorFailure_OneRecord`: a failed SYNC processor leaves exactly one STATE_PROCESS_RESULT. Update `engine_processors_audit_test.go:69-100` to assert one.
  - `TestDispatchSpan_EmptyModeIsSync`.
  - e2e `TestAudit_ProcessorRecord`:
    - an `ASYNC_NEW_TX` processor whose member answers `success:false` with a message → `failReason` equals the message, and `success` is `false`;
    - a transition with two processors, one with no `executionMode` → PAUSE_FOR_PROCESSING `processNames` lists both in order, and that processor's record has `executionMode: "SYNC"`.
  - Multi-node `RunAudit_FailReasonThroughHandOver`: the same answer from a member on a peer → the same `failReason`. With no message → no `failReason`.
- [ ] **Step 2: Run** `go test ./internal/domain/workflow/ ./internal/observability/ -run 'TestProcessor|TestDispatchSpan'`. Expected: FAIL.
- [ ] **Step 3: Implement** by §4 (STATE_PROCESS_RESULT) and §7.2.
- [ ] **Step 4: Run** the tests, the e2e test and `make test`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(workflow)!: one processor record with name, mode and the member's own failure text`.

### Task 13: Processor `type` checked at import

**Files:**
- Modify:
  - `internal/domain/workflow/validate.go`: the processor loop around `:738-750`, and the comment at `:36-45`;
  - import normalisation, where the request is stored: an omitted or empty `type` is stored as `externalized`;
  - `engine_processors.go:137-147` (delete the internalized refusal);
  - `docs/workflow-schema-versioning.md`: item 13 under `### 1.5`, and "twelve" becomes "thirteen" in both places
- Rewrite or delete:
  - `engine_processors_type_test.go`: `TestInternalizedRejection_*` and `TestType_UnknownValues_FallThrough` are deleted, because their subject is gone; `TestType_RoundTrip_PreservesUnknownValues` becomes the stored-as-externalized test;
  - `handler_type_validation_test.go:14`;
  - `engine_processors_audit_test.go:16`, `:106`
- Test: `internal/domain/workflow/validate_processor_type_test.go` (create), `internal/e2e/workflow_import_processor_type_test.go` (create)

- [ ] **Step 1: Write the failing tests:**
  - unit `TestImport_ProcessorType`, table-driven:
    - `""` and omitted → stored as `externalized`;
    - `externalized` → accepted;
    - `internalized` → a validation error;
    - `banana` → a validation error naming the value and the allowed one.
  - e2e `TestWorkflowImport_ProcessorType`:
    - `internalized` → 400 `VALIDATION_FAILED`;
    - `banana` → 400 `VALIDATION_FAILED`;
    - omitted → 200, and `GET` export shows `"type":"externalized"`.
- [ ] **Step 2: Run** them. Expected: FAIL.
- [ ] **Step 3: Implement the check and the normalisation.** Delete the dispatch refusal and the dead tests. Update the processor `type` description in `api/openapi.yaml`, so that it no longer describes a dispatch-time refusal, then run `go generate ./api`. Write the versioning entry with rubric point 1, in the format of item 1.
- [ ] **Step 4: Run** `go test ./internal/domain/workflow/`, the e2e test and `make test`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(workflow)!: import refuses a processor type other than externalized`.

### Task 14: An event holding U+0000 is not recorded

**Files:**
- Modify: `internal/domain/workflow/engine.go:1080-1115` (`recordEvent`)
- Create: `internal/domain/workflow/audit_nul.go` (`func holdsNUL(state, details string, data map[string]any) bool`), `audit_nul_test.go`
- Test: an e2e test, parity `RunAuditNULEventNotRecorded`, and a multi-node scenario

- [ ] **Step 1: Write the failing tests:**
  - `TestHoldsNUL`, table-driven:
    - NUL in `state`, in `details`, in a nested key, and in a nested value → true;
    - inside a `[]string` (`processNames`), inside a `[]any` of `map[string]any` (`functions`), and inside a `[]map[string]any` → true (Review Focus 3);
    - no NUL anywhere → false.
  - `TestRecordEvent_RefusesNUL`: the memory store gets no event, and a WARN is logged. Capture it with an `slog` handler and assert that it holds `entityId` and `eventType` but not the text.
  - e2e `TestAudit_NULReasonNotRecorded`: an automated transition whose criterion member answers with a reason holding U+0000 → the entity is saved (200), and there is no TRANSITION_NOT_MATCH_CRITERION event. This runs on PostgreSQL.
  - Parity `RunAuditNULEventNotRecorded`, the same on memory, SQLite and PostgreSQL. Bump the count.
  - Multi-node: the same through a hand-over, with the owner recording.
- [ ] **Step 2: Run** them. Expected: FAIL. Today the PostgreSQL save fails, and the parity run shows the divergence.
- [ ] **Step 3: Implement `holdsNUL`** with a type switch over `string`, `map[string]any`, `[]any`, `[]string` and `[]map[string]any`. Call it at the top of `recordEvent`, after the fence check.
- [ ] **Step 4: Run** the tests and `make test`. Expected: PASS.
- [ ] **Step 5: Commit** `fix(workflow): an audit event holding U+0000 is written on no backend`.

### Task 15: Scheduler events, and the engine-driven validation of every type

**Files:**
- Modify:
  - `internal/domain/workflow/fire_scheduled.go:187`, `:230`, `:259`, `:338`;
  - `internal/domain/workflow/arm.go:215`, `:221`, `:232`;
  - `internal/scheduler/service.go:925-946` (uses `workflow.ScheduledFailedData`)
- Test:
  - `internal/e2e/scheduled_transition_test.go:75-86`, `callback_joined_commit_test.go:405-411`;
  - `e2e/parity/audit.go` (existing ARM/FAIL assertions);
  - `internal/domain/workflow/event_data_conformance_test.go` (create)

- [ ] **Step 1: Write the failing tests:**
  - FIRE has `transition`, `sourceState` and `workflowName`.
  - The fire-time EXPIRE has `transition` and `sourceState`.
  - CANCEL has `workflowName` at `fire_scheduled.go:230` and `:259`, and none at `arm.go:221`.
  - FAIL carries the same values as today.

  Then `TestEveryEventTypeValidatesWhenDrivenThroughTheEngine` (matrix row "every event type cyoda-go sends"):
  - drive each engine path that sends an event type, reusing the Task 10, 12 and 15 set-ups, with the memory store;
  - pass each recorded event through `stateMachineItem` (export a test helper from `internal/domain/audit` only if the package cannot be reached otherwise);
  - validate it against `AuditEventDto` through `ApplyView`;
  - assert that the set of types seen equals the mapping minus the two never-sent types.
- [ ] **Step 2: Run** them. Expected: FAIL.
- [ ] **Step 3: Implement the sites with Task 9's constructors.** Update the e2e and parity assertions listed under Files.
- [ ] **Step 4: Run** `go test ./internal/domain/workflow/ ./internal/scheduler/`, the e2e tests and `make test`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(scheduler)!: typed data on scheduled-transition events`.

### Task 16: The oasdiff gate: the severity file and the ignore lines

**Files:**
- Create: `.github/oasdiff-severity-levels.txt`
- Modify:
  - `.github/workflows/openapi-breaking-change.yml` (add `--severity-levels .github/oasdiff-severity-levels.txt`);
  - `internal/oasdiffcheck/oasdiff_gate_test.go` and `testdata/oasdiff/` (new fixtures);
  - `.github/oasdiff-err-ignore.txt`

- [ ] **Step 1: Write the failing premise cases** in `TestOasdiffGatePremise`. Each runs with `--severity-levels ../../.github/oasdiff-severity-levels.txt`.
  - `added_mapping_key_is_not_breaking`: `base_union.yaml` → `union_added.yaml` exits 0.
  - `deleted_mapping_key_is_breaking`: `base_union.yaml` → `union_deleted.yaml` exits non-zero.
  - `added_response_enum_value_is_breaking`: `base_union.yaml` → `enum_added.yaml` exits non-zero.

  Each fixture is a minimal spec with one GET whose response is a base with two members and an `enum` property.
- [ ] **Step 2: Run** `go test ./internal/oasdiffcheck/` with oasdiff v1.21.0 on `PATH`. Expected: FAIL (the file is missing).
- [ ] **Step 3: Write the severity file** with the twelve lines of §13.3, then add the flag in the workflow.
- [ ] **Step 4: Measure the gate on the final spec:**

  ```
  git show origin/release/v0.9.0:api/openapi.yaml > /tmp/base.yaml
  oasdiff breaking /tmp/base.yaml api/openapi.yaml --fail-on ERR --severity-levels .github/oasdiff-severity-levels.txt --format singleline
  ```

  Every line must fall in a class of the §13.3 table, or be the processor-type tightening (declared under Breaking). Add each line to `.github/oasdiff-err-ignore.txt` with a one-line reason above its group. Any line outside those classes is a defect: stop and report it.
- [ ] **Step 5: Run** the gate command again, now with `--err-ignore`. Expected: exit 0. Then run `go test ./internal/oasdiffcheck/`. Expected: PASS.
- [ ] **Step 6: Commit** `ci(oasdiff): refuse removals and growing response enums; ignore the declared changes of form`.

### Task 17: Documentation

**Files:** see §12. Each edit is one bullet there.
- help: `cmd/cyoda/help/content/audit.md` (line `:106` and the sections of §12), `workflows.md`, `crud.md`, `grpc.md`, `openapi.md` (new section "Generated clients"), `errors/INVALID_CONDITION.md:15`, `:29`
- `docs/ARCHITECTURE.md:1758-1776`, `docs/PROCESSOR_EXECUTION_MODES.md:412-436`, the doc comment of `internal/domain/workflow/transition_aborted.go`
- `docs/cloud-parity/`: `callout-failover.md`, `criterion-stoppage-reason.md`, `entity-if-match.md:27`, `scheduled-transitions.md`, `function-conditions-in-criteria.md`, `function-condition-search-rejection.md:59`, `openapi-conformance.md`; also `docs/analysis/openapi/README.md`
- `CHANGELOG.md` `### Breaking`: every §12 bullet
- `docs/adr/0005-openapi-client-generation.md` (format from `docs/adr/README.md`; status Accepted, date 2026-10-15), and a one-line pointer in `docs/adr/0003-…md` under decision 4
- `CONTRIBUTING.md`: `make client-probe`, adding a sample, and Rule 6 for a contributor
- the comments at `internal/common/error_codes.go:142`, `internal/domain/workflow/validate.go:16-18`, `:29-30`, `internal/e2e/entity_delete_conditional_test.go:26`, `internal/e2e/transaction_control_test.go:61`, `internal/e2e/openapivalidator/recursive_test.go:15`

- [ ] **Step 1: Write the failing exit check.** Add `TestNoLiveMentionOfRemovedConditionSchemas` to `api/spec_rules_test.go`.
  - It walks the repo from the module root, as `internal/fence/check_call_sites_test.go:273` does, skipping `.git`.
  - It fails on any file holding `AbstractConditionDto` or `CriterionGroupConditionDto`, except the paths that keep history: `CHANGELOG.md`, `docs/release-notes/`, `docs/superpowers/`, `.github/oasdiff-err-ignore.txt`, `docs/cyoda/openapi.yml`, and the test file itself.
- [ ] **Step 2: Run it.** Expected: FAIL, listing the files above.
- [ ] **Step 3: Make the edits.** In help and docs, use present tense only, with no issue numbers. The `openapi.md` section states:
  - Rule 6's promise;
  - the two Jackson settings by name;
  - what a client reads for an unknown audit kind (an empty entry) and an unknown event type (the `eventType`, and empty `data`);
  - that adding a value to a response `enum` is a declared breaking change.
- [ ] **Step 4: Run** `go test ./api/ ./cmd/cyoda/help/...` and `make test`. Expected: PASS, including the help-topic tests.
- [ ] **Step 5: Commit** `docs: typed event data, the client contract, ADR 0005`.

### Task 18: Cloud parity

**Files:**
- Create: `docs/cloud-parity/state-machine-event-data.md`, with a row in `docs/cloud-parity/README.md`'s `| File | Covers |` table

- [ ] **Step 1: Write the parity doc** from §11, with one heading per Cloud change. Before you write Rule 4's bullet, read what Cloud's audit store does with text holding U+0000 (`~/dev/cyoda`), and state the finding with its `file:line`.
- [ ] **Step 2: Commit** `docs(cloud-parity): state-machine event data and the polymorphic form`.
- [ ] **Step 3: File the CaaS ticket** after the PR is open, through the Atlassian connector. Use project CP, the title prefix `[CaaS]` and component CaaS. The body is §11's list and a link to the parity doc on the PR branch. Put the ticket key in the parity doc, and also in the PR body and tracker #651.

---

## Done means

- Every task is committed. `make test` and `go vet ./...` pass locally, and `mvn -B -f api/clientprobe/pom.xml verify` passes.
- The gates run in this order on the exact PR head:
  1. the whole-branch fresh-context code review;
  2. `cyoda-go-security-audit`, over the whole PR range at the final head.

  Any fix after them reruns both.
- The PR is open into `release/v0.9.0`, with CI green on its head: every check, the new client-generation job included. Paul makes that job a required check.
- Tracker #651 is updated at each step. The CaaS ticket is filed. The SPI PR is open into `main`, and cyoda-go pseudo-pins it.
