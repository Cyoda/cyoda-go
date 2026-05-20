# Issue #250 — Split processor execution-location from scheduled-transition timing

**Date:** 2026-05-20
**Milestone:** v0.8.0
**Issue:** #250 (closed-by this PR)
**Prerequisite for:** #259 (scheduled-transition shape + SPI), #260 (internalized
processor execution-location shape + SPI)
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §H1 and §M1

> **Revision 2 (2026-05-20):** Incorporates an independent senior-architect
> review. Material changes vs. revision 1: (a) engine-dispatch integration
> point corrected to "inside the per-processor loop in `executeProcessors`",
> (b) audit-event coherence with the rejection path made explicit, (c) Type
> string constants moved from the SPI to `internal/domain/workflow`
> (mirroring the precedent for `ExecutionMode*` in `validate.go:16-21`) —
> this also resolves a sequencing contradiction with the SPI tag bundling,
> (d) OpenAPI `discriminator.mapping` block made explicit to avoid generator
> ambiguity, (e) test plan expanded with five additional coverage areas
> (audit events, cascade-position, unknown-Type round-trip, Type ×
> ExecutionMode validation ordering, OpenAPI-validator non-application),
> (f) backwards-compat table extended with a DTO-using-client row and the
> drop-through framing rewritten as "carried-over debt, not a feature".

---

## 1. Problem

`ProcessorDefinitionDto.type` in `api/openapi.yaml:8672–8712` is currently a
discriminator over two values — `externalized` and `scheduled` — modelled as
alternatives on a single axis. Static reading of the runtime and the
import/export audit (§H1, §M1) shows this conflates two unrelated concerns:

1. **Execution-location** — where the processor runs. Today only `externalized`
   is wired (gRPC dispatch to a calculation node). An `internalized` variant
   (run inside the cyoda-go process) is planned in #260.
2. **Scheduled future transitions** — `ScheduledTransitionProcessorDefinitionDto`
   (config: `delayMs` + `transition` + `timeoutMs`) is **not** a processor that
   runs later; it is a workflow-timing primitive that fires a future state
   transition on the same entity. It rides in `processors[]` only as a
   schema-shape convenience.

A future scheduled-transition feature should be expressible alongside any
processor execution location, not instead of one.

Three compounding facts make the conflation harmless today but actively
misleading:

- The workflow import handler (`internal/domain/workflow/handler.go:43`)
  unmarshals JSON straight into `[]spi.WorkflowDefinition`, bypassing every
  OpenAPI DTO. The DTOs exist only as the contract surface — they are not
  the runtime parsing target.
- The SPI's `ProcessorDefinition.Type` (`cyoda-go-spi@v0.7.1/types.go:141`)
  is a free `string` that no engine code reads. `validateProcessorFlags`
  (`internal/domain/workflow/validate.go:47–61`) never inspects it; the
  dispatch switch in `executeProcessors`
  (`internal/domain/workflow/engine_processors.go:42–118`) branches on
  `ExecutionMode`, not `Type`.
- `cmd/cyoda/help/content/workflows.md:135–139` claims `"EXTERNAL"` is the
  only valid value and that other values produce `errors.VALIDATION_FAILED`
  at import — neither claim matches the code. The value is documented in
  OpenAPI as `externalized`, not `EXTERNAL`, and import never rejects it.

The parity scenario `e2e/externalapi/scenarios/08-workflow-import-export.yaml`
`wf-import/03` imports a workflow with one externalized processor (no `type`
set on it) and one scheduled processor (`{"type":"scheduled",
"config":{"delayMs":300,"timeoutMs":30000,"transition":"Close"}}`) inside the
same `processors[]` array, and asserts round-trip equality. The runtime
accepts both because the SPI's `Type` is a free string and
`ScheduledTransitionConfigDto` fields silently survive the unmarshal as
part of `ProcessorConfig` (they don't match `ProcessorConfig` fields and
are dropped — see audit §H1 "the scheduling config fields are silently
dropped by JSON unmarshal").

## 2. Goals

- Reshape `api/openapi.yaml` so processor `type` is the execution-location axis
  only. Enum becomes `[externalized, internalized]`, with an explicit
  `discriminator.mapping` block.
- Remove `ScheduledTransitionProcessorDefinitionDto` and
  `ScheduledTransitionConfigDto` from the OpenAPI surface entirely. The new
  home for scheduled-transitions is the next carve-out (#259); both ship
  in the same v0.8.0 release, so no transition-window divergence between
  OpenAPI and Cloud-emitted payloads exists.
- Update the SPI `ProcessorDefinition.Type` field's docstring to declare its
  meaning (execution-location axis). Surface untyped string constants
  for the two recognised values in `internal/domain/workflow` (the engine
  owns the values; the SPI is the wire type — same split as
  `ExecutionMode*` per `validate.go:16-21`).
- Bring `cyoda help workflows` and any other affected docs into sync with
  actual behaviour. Help text is derived from the codebase, not authoritative
  on its own.
- Reject `type: internalized` at engine dispatch with a clear,
  self-contained "not yet implemented" error message. Surfaces the planned
  shape to workflow authors so they can compose against it before #260 lands
  the runtime.

## 3. Non-goals

- Implement scheduled-transition firing. (#259 owns the SPI shape, #251 owns
  the runtime — timer persistence, cluster ownership, work-stealing recovery,
  audit surface.)
- Implement internalized processor execution. (#260 owns the SPI shape +
  registration, #252 owns the runtime — in-process isolation, lifecycle.)
- Tighten import-time validation for unknown `type` values. That is #255's
  scope (state-graph, names, `ExecutionMode` enum, `Type` enum). #250
  reshapes the schema only; #255 enforces it at import.
- Wire `ProcessorConfig.Context` or `RetryPolicy`. (#253, #254, #262.)
- Touch the parity contract for `ProcessorConfig.AttachEntity` `bool` vs
  `*bool` discrepancy (audit §M1, separate consistency concern).

## 4. Decision summary (post-clarification + post-review)

| Question | Decision |
|---|---|
| OpenAPI scope | **Full split now.** Remove ScheduledTransitionProcessorDefinitionDto + ScheduledTransitionConfigDto entirely. processors[] oneOf collapses to ExternalizedProcessorDefinitionDto (single-member oneOf, retained as a forward-extension point for InternalizedProcessorDefinitionDto in #260). |
| OpenAPI discriminator | **Explicit `discriminator.mapping` block.** Generator behaviour for value-to-schema mapping varies; explicit is safer than implicit-name-match (`externalized` ≠ `ExternalizedProcessorDefinitionDto`). |
| Type enum values | **`[externalized, internalized]`.** Empty/omitted defaults to `externalized` on the wire — preserves current parity payloads where `type` is omitted on externalized processors. |
| Where to reject `internalized` | **At engine fire time, inside the per-processor dispatch loop.** Mirrors #259's planned pattern for scheduled-transition runtime rejection. Import accepts (validation tightening is #255's scope). |
| Constants for Type values | **In `internal/domain/workflow`,** not the SPI. The SPI carries the wire field; the engine owns the value semantics. Same split as `ExecutionMode*` constants — see precedent + rationale in `validate.go:13-15`. |
| SPI `Type` field | **Keep, document.** Add docstring naming the execution-location axis. No new symbols. SPI change is comment-only, so no SPI tag is forced by #250. |
| Parity scenario 08 | **Strip the scheduled entry from `wf-import/03`.** Keep the externalized processor sibling to preserve group-criterion + processor round-trip coverage. |
| Error messages | **No issue-ID references in any shipped artefact.** Self-contained phrasing in error messages, log lines, code comments, OpenAPI descriptions, and help-topic content. Issue IDs are appropriate only in this spec doc, PR bodies, commit messages, and CHANGELOG. |
| Source of truth for Type strings | **`internal/domain/workflow` constants** are authoritative. Engine code references those, not the regenerated `api/generated.go`'s `ProcessorDefinitionDtoType*` constants (which are an artefact of `oapi-codegen` and should not leak across package boundaries). |

## 5. Detailed design

### 5.1 OpenAPI changes (`api/openapi.yaml`)

**`ProcessorDefinitionDto`** — `type` becomes a constrained enum with an
explicit discriminator mapping. The mapping defends against generator
behaviour that defaults to implicit name-match (`externalized` →
`Externalized` capitalised → schema search), which works only by
coincidence today and is fragile to renaming.

```yaml
ProcessorDefinitionDto:
  type: object
  discriminator:
    propertyName: type
    mapping:
      externalized: "#/components/schemas/ExternalizedProcessorDefinitionDto"
      # internalized: "#/components/schemas/InternalizedProcessorDefinitionDto"
      # ^ added by #260 when the internalized shape lands.
  properties:
    type:
      type: string
      enum:
        - externalized
        - internalized
      description: |
        Processor execution-location axis. `externalized` dispatches the
        processor via gRPC to a calculation node selected by
        `calculationNodesTags`. `internalized` runs the processor in-process
        within cyoda-go; reserved value, currently rejected at engine
        dispatch as not yet implemented. Empty or omitted is treated as
        `externalized`.
    name:
      type: string
      description: Name of the processor
  required:
    - name
```

**`ExternalizedProcessorDefinitionDto`** — unchanged.

**`ScheduledTransitionProcessorDefinitionDto`** and
**`ScheduledTransitionConfigDto`** — **removed entirely.** Lines 8685–8712.

**`TransitionDefinitionDto.processors[]`** — `oneOf` becomes a single entry.
The wrapper is kept (vs. a direct `$ref`) so #260's
`InternalizedProcessorDefinitionDto` can be added as a second member with a
one-line change.

```yaml
processors:
  type: array
  description: List of processors to execute for this transition
  items:
    oneOf:
      - $ref: "#/components/schemas/ExternalizedProcessorDefinitionDto"
```

A verification spike on the regenerated `api/generated.go` (run `make
generate` and inspect the union helpers) is part of the implementation
sequence — if `oapi-codegen` emits malformed helpers without the explicit
mapping, the mapping is already in place to fix it.

### 5.2 Generated DTOs (`api/generated.go`)

Regenerated from the updated `openapi.yaml`. The following entries
**disappear**:

- `type ScheduledTransitionConfigDto struct { … }` (line ~2382)
- `type ScheduledTransitionProcessorDefinitionDto struct { … }` (line ~2394)
- `AsScheduledTransitionProcessorDefinitionDto()` / `From…()` / `Merge…()`
  union helpers (line ~4139–4156)

The `ExternalizedProcessorDefinitionDto` entry and its union helpers stay.
The `ProcessorDefinitionDto` struct gains generator-emitted constants —
likely `ProcessorDefinitionDtoTypeExternalized` and
`ProcessorDefinitionDtoTypeInternalized`. **These exist as a side effect of
the generator and are not the authoritative source of truth.** Engine code
in `internal/domain/workflow` references its own constants (§5.5);
`api/generated.go`'s constants are usable only inside `api/` and at the
boundary between OpenAPI tooling and the rest of the codebase.

`make generate` (or whichever target regenerates `api/generated.go`) is run
as part of the change.

### 5.3 SPI changes (`cyoda-go-spi/types.go`)

Repo: `https://github.com/Cyoda-platform/cyoda-go-spi`. Co-located at
`../cyoda-go-spi`.

**`ProcessorDefinition.Type`** — keep the field, add docstring only. No new
constants are exported by the SPI; the engine owns the value semantics
(see §5.5, mirroring how `ExecutionMode` is treated).

```go
// ProcessorDefinition represents a processor attached to a transition.
type ProcessorDefinition struct {
    // Type is the execution-location axis. Recognised values are defined by
    // the cyoda-go engine package; canonical values are "externalized"
    // (dispatched via gRPC to a calculation node selected by
    // Config.CalculationNodesTags) and "internalized" (runs in-process
    // within cyoda-go; reserved, currently rejected by the engine at
    // dispatch as not yet implemented). Empty is treated as "externalized".
    // Implementations must not treat unknown values as fatal at import —
    // engine-level rejection is where unknown values become errors. The
    // empty/omitted default is intentional for wire compatibility with
    // payloads that omit Type entirely.
    Type          string          `json:"type"`
    Name          string          `json:"name"`
    ExecutionMode string          `json:"executionMode,omitempty"`
    Config        ProcessorConfig `json:"config,omitempty"`
}
```

**Cross-repo coordination.** This is a **comment-only** SPI change. No new
exported symbols, no signature change. No SPI tag is required for #250 to
land. v0.8.0's bundled SPI tag (the one that #259 and #260 will need
anyway) can pick up this docstring change alongside the substantive shape
work in those carve-outs.

### 5.4 Engine dispatch (`internal/domain/workflow/engine_processors.go`)

**Integration point.** The check goes **inside the per-processor loop** in
`executeProcessors` at `engine_processors.go:59-87`, alongside the existing
`switch proc.ExecutionMode`. The Type-axis branch runs **before** the
ExecutionMode branch — the rejection short-circuits dispatch entirely; the
ExecutionMode value is irrelevant when Type is not implemented.

```go
// (inside the for _, proc := range processors loop, before
//  the switch proc.ExecutionMode)
switch proc.Type {
case "", workflow.ProcessorTypeExternalized:
    // fall through to existing ExecutionMode-based dispatch.
case workflow.ProcessorTypeInternalized:
    procErr = fmt.Errorf(
        "processor %q: execution type %q is not yet implemented",
        proc.Name, proc.Type)
default:
    // tolerate unknown — fall through to existing ExecutionMode dispatch.
    // Import-time validation owns rejection of unknown values; engine
    // stays permissive to avoid double-rejection of parity-payload values.
}
// existing switch proc.ExecutionMode { … } follows here for the
// fall-through cases.
```

The returned `procErr` flows through the existing per-processor failure
path — `classifyWorkflowError` (`internal/domain/entity/service.go:1449`,
default branch) maps it to `WORKFLOW_FAILED` (HTTP 400), the entity stays
in the source state, the cascade aborts. Behaviour is identical to a SYNC
processor failure.

**Audit-event coherence.** Because the rejection runs inside the
per-processor loop, the existing audit-event sequence is preserved:

- `SMEventProcessingPaused` (emitted at `engine_processors.go:52`, before
  the loop) fires once for the processor pipeline — correct, the pipeline
  is entered.
- `SMEventStateProcessResult{success: false, mode: <ExecutionMode>}`
  (emitted at `engine_processors.go:104`) fires once for the internalized
  processor — correct, the processor was visited and failed.
- `SMEventStateMachineFinish` is **not** emitted — correct, the cascade
  did not complete.

This matches the audit surface of a SYNC processor failure and requires
zero new audit-event code. The test plan (§6.1 item 4) asserts the
sequence explicitly.

**No issue IDs in the error message.** The message names the type value
and declares the absence of an implementation; it does not link to a
tracker.

### 5.5 Validator + new constants (`internal/domain/workflow/validate.go` or sibling)

`validateProcessorFlags` is **unchanged in scope.** It continues to enforce
only `StartNewTxOnDispatch=true` → `ExecutionMode=COMMIT_BEFORE_DISPATCH`.
No new `Type`-axis validation rule is added here.

The new constants for `ProcessorDefinition.Type` values are added alongside
the existing `ExecutionMode*` block. The choice of file (`validate.go` vs.
a new `processor_types.go`) is a judgement call for the implementer; the
constants must live in `internal/domain/workflow` and be exported.

```go
// Processor execution-location tokens. Sourced from the OpenAPI enum in
// api/openapi.yaml (mirrored in api/generated.go's
// ProcessorDefinitionDtoType constants). Centralised here as untyped
// strings so engine logic, validator rules, and tests can compare against
// a single source — the SPI's ProcessorDefinition.Type field is itself a
// plain string, so an enum type would not buy compile-time safety.
//
// Empty value is treated as ProcessorTypeExternalized.
const (
    ProcessorTypeExternalized = "externalized"
    ProcessorTypeInternalized = "internalized"
)
```

This mirrors the precedent for `ExecutionMode*` (validate.go:13-21).
Anywhere engine code or test code needs to name a Type value, it imports
these constants. The regenerated `api/generated.go` constants
(`ProcessorDefinitionDtoTypeExternalized` etc.) are an OpenAPI-tooling
artefact and stay scoped to that package.

### 5.6 Help text (`cmd/cyoda/help/content/workflows.md`)

Lines 130 (field listing) and 135–139 (the EXTERNAL claim) are rewritten.

**Before** (135–139):

```
**Valid `type` values (exhaustive for v0.6.1):**

- `"EXTERNAL"` — dispatches to a calculation node via gRPC using `calculationNodesTags` for routing

No other types are supported. Supplying any other value produces `errors.VALIDATION_FAILED` at workflow import time.
```

**After:**

```
**Processor `type` (execution-location axis):**

- `"externalized"` (default when omitted) — dispatched via gRPC to a
  calculation node selected by `Config.calculationNodesTags`. This is the
  only execution location implemented today; all the `executionMode`
  semantics below apply to externalized processors.
- `"internalized"` — reserved for in-process execution. The engine rejects
  this value at dispatch with `WORKFLOW_FAILED` (400) and the message
  `processor X: execution type "internalized" is not yet implemented`.
  Authors may include the value in workflow definitions for forward-
  compatibility, but any transition that fires it will fail until the
  internalized runtime lands.

Unknown values are currently tolerated at import and fall through to the
default `executionMode` branch (behaving as `SYNC`/`ASYNC_SAME_TX`). This
permissiveness will narrow in a future release; do not rely on it.
```

The line 130 field listing keeps `type` and adds a one-line summary —
"execution-location axis; see below for valid values".

### 5.7 Parity scenario (`e2e/externalapi/scenarios/08-workflow-import-export.yaml`)

`wf-import/03` is the affected test. The import body's `processors[]` array
currently contains two entries:

```yaml
"processors":[
  {"name":"send_approval_notification","executionMode":"ASYNC_NEW_TX", … },
  {"type":"scheduled","name":"schedule_close_process",
   "config":{"delayMs":300,"timeoutMs":30000,"transition":"Close"}}
]
```

**Change:** remove the second entry. Test name remains "Advanced FSM: group
criterion (AND), function criterion, scheduled processor", retitled to drop
the scheduled-processor mention. Source-test annotation
(`integration-tests/.../EntityModelWorkflowInteractorIT.kt`) is preserved.

Comment added above the test (YAML `#` comment):

```yaml
# wf-import/03 originally exercised a scheduled-transition processor inside
# processors[]. That conflation has been removed from the schema; the
# scheduled-transition primitive is reintroduced at its proper home (a
# sibling primitive on TransitionDefinition) in a separate scenario. This
# test now covers group-criterion + function-criterion + externalized-
# processor round-trip only.
```

`json_equals_normalized` assertion is preserved unchanged.

### 5.8 Other docs hygiene (Gate 4 sweep)

- **`README.md`** — grep for `type=EXTERNAL`, `"EXTERNAL"` in workflow
  context, `scheduled.*processor`. Update or remove any matches.
- **`docs/PROCESSOR_EXECUTION_MODES.md`** — this user-facing doc currently
  scopes to `executionMode` only and does not mention the `type` axis.
  That's the right scope for the runtime-semantics doc, but a one-paragraph
  preface in §1 ("Quick Reference") anchoring the two-axis distinction
  (execution-**location** = `type`; execution-**mode** = `executionMode`)
  is added by this PR. A new `## 0. Axis Summary` (or appendix) section is
  acceptable. The goal: a reader of this doc should know that `type`
  exists and is a different axis, even if its detail lives in the
  workflows help-topic.
- **`CLAUDE.md`** — grep for any mention of scheduled processors. Update
  if found.
- **`cmd/cyoda/help/content/workflows.md`** — covered above (§5.6).
- **`cmd/cyoda/help/content/config/*.md`** — untouched. No env-var
  changes.
- **`DefaultConfig()`** — untouched. No env-var changes.
- **`COMPATIBILITY.md`** — untouched. SPI change is comment-only (§5.3),
  no tag bump forced by #250.
- **`docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md`** — the audit references #250
  as the schema-cleanup prerequisite (§H1, §M1, §0). After this PR
  merges, the audit's tracking remains accurate. No change.

## 6. Test plan (TDD)

### 6.1 Failing-test-first sequence

The TDD protocol (`.claude/rules/tdd.md`) drives this work. Tests are
authored before implementation, run to confirm RED, then driven GREEN.
The unit-test target is `internal/domain/workflow/engine_processors_test.go`
or a sibling test file in the same package.

1. **Engine dispatch — `Type: internalized` is rejected at fire time.**
   Build a workflow whose transition has a single processor with
   `Type: workflow.ProcessorTypeInternalized`, fire the transition, assert
   the error string contains `not yet implemented`, the error contains the
   processor name, and the entity remains in the source state. RED →
   implement §5.4 → GREEN.

2. **Engine dispatch — `Type: ""` and `Type: "externalized"` behave
   identically to today.** Two test cases, same workflow shape, asserting
   identical gRPC dispatch paths. Anchors regression detection for the
   new Type-axis switch.

3. **Engine dispatch — unknown `Type` value falls through.** A test with
   `Type: "scheduled"` (a known legacy value that this PR removes from
   OpenAPI) and a test with `Type: "garbage"` (an arbitrary string) — both
   assert the engine does NOT reject and dispatches via the
   `ExecutionMode` path. Confirms the default branch in §5.4 and
   preserves runtime tolerance until #255 lands import-time tightening.

4. **Audit-event coherence for the internalized rejection.** Same
   workflow as test 1, but assert the audit-event sequence emitted by the
   engine matches the SYNC-failure shape:
   - `SMEventProcessingPaused` fires exactly once before the loop.
   - `SMEventStateProcessResult{success: false, mode: <ExecutionMode>}`
     fires exactly once for the internalized processor.
   - `SMEventStateMachineFinish` does NOT fire.
   This anchors the §5.4 audit-coherence claim and catches future code
   changes that move the Type-axis check out of the loop.

5. **Cascade-position behaviour — internalized abort is fatal, not
   skippable.** A workflow with `processors[A_externalized_succeeds,
   B_internalized, C_externalized_would_succeed]`. Assert: A's mutations
   are NOT applied (transaction rolls back), B's rejection error
   surfaces, C is never dispatched, the entity stays in the source
   state. Confirms internalized rejection behaves like SYNC failure, not
   like `ASYNC_NEW_TX` (which is non-fatal and continues).

6. **Round-trip of unknown `Type` values on the wire.** Import a workflow
   with `Type: "scheduled"` (or `"future_unknown_value"`), export, assert
   the exported JSON preserves the `type` field value verbatim. This
   confirms that removing the DTO from OpenAPI does not change wire
   behaviour for the SPI's free-string field.

7. **Validator ordering — `StartNewTxOnDispatch` flag-coherence runs at
   import; Type rejection runs at fire.** A workflow with `Type:
   internalized + ExecutionMode: SYNC + StartNewTxOnDispatch: true`. The
   `StartNewTxOnDispatch=true` flag is incoherent without
   `COMMIT_BEFORE_DISPATCH`, so import-time `validateProcessorFlags`
   should reject the workflow. Assert: the import returns
   `VALIDATION_FAILED` (400) — the flag-coherence check fires BEFORE
   engine-time Type rejection ever runs. Conversely, a workflow with
   `Type: internalized + ExecutionMode: COMMIT_BEFORE_DISPATCH +
   StartNewTxOnDispatch: true` imports successfully and is rejected at
   fire time. Two test cases.

8. **E2E — POST /workflow/import accepts `Type: internalized` payload;
   subsequent entity creation that fires the internalized processor
   returns 400 `WORKFLOW_FAILED`.** Integration test in `internal/e2e/`
   using the in-process HTTP server. The OpenAPI request validator is
   NOT applied at handler level — `handler.go:43` unmarshals raw JSON
   into `spi.WorkflowDefinition`. No conditional language in the test
   description.

9. **E2E — round-trip of a workflow with `Type: externalized` succeeds.**
   Verifies that the new oneOf in `processors[]` parses correctly on
   import via the existing JSON unmarshal path.

10. **Parity — scenario 08/wf-import/03 passes after the scheduled-entry
    strip.** Run the parity test suite; the round-trip assertion holds.

### 6.2 Coverage gaps explicitly NOT closed in #250

- Import-time rejection of unknown `Type` values is **not** added. (#255
  scope.)
- Import-time rejection of payloads carrying the legacy
  `ScheduledTransitionConfigDto` shape (`delayMs`, `timeoutMs`,
  `transition` inside a `config` block on a `processors[]` entry) is
  **not** added. After this PR, those fields are silently dropped by the
  JSON unmarshal — the existing behaviour, unchanged. #255 may tighten
  this; #259 reintroduces the fields at a different shape entirely.

## 7. Backwards compatibility

| Inbound payload shape | Behaviour before | Behaviour after #250 |
|---|---|---|
| `processors[]` with `Type` omitted | Accepted, dispatched as externalized | Same |
| `processors[]` with `Type: "externalized"` | Accepted, dispatched as externalized | Same |
| `processors[]` with `Type: "internalized"` | Accepted, dispatched as externalized (default branch) | **Accepted at import, rejected at engine dispatch with `WORKFLOW_FAILED` 400** |
| `processors[]` with `Type: "scheduled"` + scheduled config fields (raw JSON path) | Accepted at handler.go:43 (DTOs bypassed); scheduled config fields silently dropped by SPI unmarshal; dispatched as externalized at runtime | **Same wire behaviour.** Carried-over technical debt, not a feature — audit §H1 grades the silent-drop as HIGH. #255 may tighten; #250 deliberately preserves the runtime tolerance to avoid breaking parity payloads. |
| `processors[]` with `Type: "scheduled"` (clients using generated OpenAPI DTOs) | Validated by the generated client against ScheduledTransitionProcessorDefinitionDto; serialised, sent, accepted | **Fails client-side serialisation.** ScheduledTransitionProcessorDefinitionDto no longer exists in api/generated.go. Clients regenerating from the v0.8.0 OpenAPI cannot construct the shape. Acceptable because #259 ships in the same release with the replacement shape — clients regenerate once at v0.8.0 and pick up both changes. |
| `processors[]` with unknown `Type` | Accepted, dispatched as externalized | Same |

The wire surface is **strictly tighter** in exactly one place
(`Type: internalized` now fails at runtime where previously it
default-dispatched). Everywhere else the wire is identical. The DTO
surface, however, is strictly tighter for two values (`scheduled` removed
entirely, `internalized` added as a reserved value) — DTO-using clients
regenerate at v0.8.0 and pick up the new shape together with #259's
replacement.

## 8. Risks

- **OpenAPI generator behaviour around discriminator + oneOf with a single
  member.** Some `oapi-codegen` versions emit broken or unused union
  helpers when the `oneOf` has one member. Mitigation: the §5.1 explicit
  `discriminator.mapping` block, plus a verification spike on the
  regenerated `api/generated.go` before committing the generation.
  Fallback: drop the `oneOf` wrapper and direct-reference
  `ExternalizedProcessorDefinitionDto`; #260 reintroduces `oneOf` when it
  needs to.
- **#260's `InternalizedProcessorDefinitionDto` shape may differ from
  `ExternalizedProcessorDefinitionDto`.** When #260 lands, the
  `processors[]` `oneOf` grows a second `$ref`. The single-entry `oneOf`
  is forward-compatible. The `discriminator.mapping` block gains a second
  entry.
- **SPI tag coupling.** Since §5.3 is now comment-only, #250 does not
  force a SPI tag at all. v0.8.0's bundled SPI tag (carrying #259's and
  #260's substantive shape work) picks up the docstring change as part of
  the bundle. No sequencing contradiction.
- **Cyoda Cloud parity.** Cloud will need to regenerate its OpenAPI
  client and pick up the v0.8.0 changes (the new scheduled-transition
  shape from #259 + the Type-axis from #250 + the internalized reservation
  from #260). All three carve-outs ship in v0.8.0 so Cloud regenerates
  once.
- **`api/generated.go` constants vs `internal/domain/workflow` constants.**
  Two source-of-truth sets exist post-regeneration. The §5.2 declaration
  resolves this — engine code uses the workflow-package constants;
  `api/generated.go`'s constants are scoped to that package. Implementer
  must not import `api`'s constants into the workflow package.

## 9. Out of scope (sanity check)

- Scheduled-transition runtime, durability, cluster ownership — #251.
- Scheduled-transition SPI shape (sibling field on TransitionDefinition or
  array on StateDefinition) — #259.
- Internalized processor runtime (registration, in-process isolation) —
  #252.
- Internalized processor SPI shape — #260.
- Import-time validator tightening — #255.
- `ProcessorConfig.Context` / `RetryPolicy` wiring — #253, #254, #262.
- `WorkflowStore.CompareAndSave` SPI addition — #35 (gates on
  cyoda-go-cassandra#22).

---

## Appendix A: File-level impact summary

| File | Change |
|---|---|
| `api/openapi.yaml` | Rewrite `ProcessorDefinitionDto.type` enum + add explicit `discriminator.mapping` block; remove `ScheduledTransitionProcessorDefinitionDto`, `ScheduledTransitionConfigDto`; collapse `processors[]` oneOf to single-member. |
| `api/generated.go` | Regenerated from the above. |
| `cyoda-go-spi/types.go` (sibling repo) | Docstring on `ProcessorDefinition.Type` only. Comment-only change; no SPI tag forced. |
| `internal/domain/workflow/validate.go` (or sibling) | Add `ProcessorTypeExternalized` and `ProcessorTypeInternalized` untyped string constants alongside the existing `ExecutionMode*` block. |
| `internal/domain/workflow/engine_processors.go` | Add `Type`-axis switch at the entry of the per-processor loop in `executeProcessors` (~line 59-87). Order: Type-axis check runs before the existing `switch proc.ExecutionMode`. Empty/externalized → fall through; internalized → return "not yet implemented" error; unknown → fall through. |
| `internal/domain/workflow/engine_processors_test.go` (or sibling) | TDD tests per §6.1 items 1–7. |
| `internal/e2e/` | E2E tests per §6.1 items 8–9. |
| `cmd/cyoda/help/content/workflows.md` | Lines 130, 135–139 rewritten per §5.6. |
| `docs/PROCESSOR_EXECUTION_MODES.md` | One-paragraph axis-summary preface (or new §0) anchoring `type` vs `executionMode` distinction. |
| `e2e/externalapi/scenarios/08-workflow-import-export.yaml` | Strip scheduled entry from `wf-import/03`; retitle test; add comment. |
| `README.md` | Grep + correct any stale references. |
| `CLAUDE.md` | Grep + correct any stale references. |

## Appendix B: Verification before completion

Following `superpowers:verification-before-completion`:

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `go test -short ./...` — green.
- `go test ./internal/e2e/...` — green (Docker required for the
  testcontainers Postgres backend).
- Parity scenario suite — green.
- `go test -race ./...` — one-shot before PR creation per
  `.claude/rules/race-testing.md`.
- `make todos` — no new TODOs introduced. The engine's default branch
  ("tolerate unknown Type") is morally a deferral to #255; per the
  no-issue-IDs rule, the source comment must not reference #255 — the
  deferral is recorded in this spec and in the PR body instead.

## Appendix C: Out-of-tree plugin impact

The cyoda-go-cassandra plugin (commercial backend, separate repo) consumes
the SPI. Because §5.3 is a comment-only change, Cassandra is unaffected.
Verification: run the parity registry against the cassandra plugin after
the v0.8.0 SPI tag lands (alongside the in-tree plugins). Per memory
`feedback_cross_plugin_design_verification`, this verification is part of
the bundled v0.8.0 SPI tag landing, not #250 in isolation.
