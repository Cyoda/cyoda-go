# Issue #250 — Split processor execution-location from scheduled-transition timing

**Date:** 2026-05-20
**Milestone:** v0.8.0
**Issue:** #250 (closed-by this PR)
**Prerequisite for:** #259 (scheduled-transition shape + SPI), #260 (internalized
processor execution-location shape + SPI)
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §H1 and §M1

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
  dispatch switch (`internal/domain/workflow/engine_processors.go:42–118`)
  branches on `ExecutionMode`, not `Type`.
- `cmd/cyoda/help/content/workflows.md:135–139` claims `"EXTERNAL"` is the
  only valid value and that other values produce `errors.VALIDATION_FAILED`
  at import — neither claim matches the code. The value is documented in
  OpenAPI as `externalized`, not `EXTERNAL`, and import never rejects it.

The parity scenario `e2e/externalapi/scenarios/08-workflow-import-export.yaml`
`wf-import/03` imports a workflow with one externalized processor (no `type`
set on it) and one scheduled processor (`{"type":"scheduled",
"config":{"delayMs":300,"timeoutMs":30000,"transition":"Close"}}`) inside the
same `processors[]` array, and asserts round-trip equality. The runtime
accepts both because the SPI's `Type` is a free string and `ScheduledTransitionConfigDto`
fields silently survive the unmarshal as part of `ProcessorConfig` (they don't
match `ProcessorConfig` fields and are dropped — see audit §H1 "the scheduling
config fields are silently dropped by JSON unmarshal").

## 2. Goals

- Reshape `api/openapi.yaml` so processor `type` is the execution-location axis
  only. Enum becomes `[externalized, internalized]`.
- Remove `ScheduledTransitionProcessorDefinitionDto` and
  `ScheduledTransitionConfigDto` from the OpenAPI surface entirely. The new
  home for scheduled-transitions is the next carve-out (#259); this issue
  removes the conflated shape and stops short of defining the replacement.
- Update the SPI `ProcessorDefinition.Type` field's docstring to declare its
  meaning (execution-location axis); optionally surface untyped string
  constants for the two recognised values.
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

## 4. Decision summary (post-clarification)

| Question | Decision |
|---|---|
| OpenAPI scope | **Full split now.** Remove ScheduledTransitionProcessorDefinitionDto + ScheduledTransitionConfigDto entirely. processors[] oneOf collapses to ExternalizedProcessorDefinitionDto. |
| Type enum values | **`[externalized, internalized]`.** Empty/omitted defaults to `externalized` on the wire — preserves current parity payloads where `type` is omitted on externalized processors. |
| Where to reject `internalized` | **At engine fire time.** Mirrors #259's planned pattern for scheduled-transition runtime rejection. Import accepts (validation tightening is #255's scope). |
| SPI `Type` field | **Keep, document.** Type is the execution-location axis going forward — not dormant. Add docstring naming the semantic; add optional constants. No SPI release coupled to #250. |
| Parity scenario 08 | **Strip the scheduled entry from `wf-import/03`.** Keep the externalized processor sibling to preserve group-criterion + processor round-trip coverage. |
| Error messages | **No issue-ID references in any shipped artefact.** Self-contained phrasing in error messages, log lines, code comments, OpenAPI descriptions, and help-topic content. Issue IDs are appropriate only in this spec doc, PR bodies, commit messages, and CHANGELOG. |

## 5. Detailed design

### 5.1 OpenAPI changes (`api/openapi.yaml`)

**`ProcessorDefinitionDto`** — `type` field becomes a constrained enum, the
discriminator stays.

```yaml
ProcessorDefinitionDto:
  type: object
  discriminator:
    propertyName: type
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

```yaml
processors:
  type: array
  description: List of processors to execute for this transition
  items:
    oneOf:
      - $ref: "#/components/schemas/ExternalizedProcessorDefinitionDto"
```

Keeping the `oneOf` wrapper (vs. a direct `$ref`) is deliberate: when #260
lands `InternalizedProcessorDefinitionDto`, the extension is one line. Cost
of carrying it now is zero (oneOf with a single member is well-defined).

### 5.2 Generated DTOs (`api/generated.go`)

Regenerated from the updated `openapi.yaml`. The following entries
**disappear**:

- `type ScheduledTransitionConfigDto struct { … }` (line ~2382)
- `type ScheduledTransitionProcessorDefinitionDto struct { … }` (line ~2394)
- `AsScheduledTransitionProcessorDefinitionDto()` / `From…()` / `Merge…()` union helpers (line ~4139–4156)

The `ExternalizedProcessorDefinitionDto` entry and its union helpers stay.
The `ProcessorDefinitionDto` struct gains an `enum`-driven constants block
(generator behaviour) — likely `ProcessorDefinitionDtoTypeExternalized` and
`ProcessorDefinitionDtoTypeInternalized` constants.

`make generate` (or whichever target regenerates `api/generated.go`) is run
as part of the change.

### 5.3 SPI changes (`cyoda-go-spi/types.go`)

Repo: `https://github.com/Cyoda-platform/cyoda-go-spi`. Co-located at
`../cyoda-go-spi`.

**`ProcessorDefinition.Type`** — keep the field, add docstring + constants.

```go
// ProcessorTypeExternalized and ProcessorTypeInternalized are the recognised
// values for ProcessorDefinition.Type. Empty is treated as Externalized.
const (
    ProcessorTypeExternalized = "externalized"
    ProcessorTypeInternalized = "internalized"
)

// ProcessorDefinition represents a processor attached to a transition.
type ProcessorDefinition struct {
    // Type is the execution-location axis. Recognised values:
    //   - ""             — treated as Externalized.
    //   - "externalized" — dispatched via gRPC to a calculation node
    //                      selected by Config.CalculationNodesTags.
    //   - "internalized" — runs in-process within cyoda-go. Reserved;
    //                      currently rejected by the engine at dispatch
    //                      as not yet implemented.
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

**Cross-repo coordination.** This is a **non-breaking** SPI change — new
constants + docstring only. No version bump strictly required. The v0.8.0
"pay the SPI cost once" approach favours bundling these constants into the
same SPI tag that #259 and #260 will need anyway; couple the SPI tag to the
last of #250/#259/#260 to land, not to #250 alone.

### 5.4 Engine dispatch (`internal/domain/workflow/engine_processors.go`)

Add a type-axis check at the entry of `executeProcessor` (or wrapping
`executeProcessors`). The exact integration point depends on the current
dispatch switch shape; the rule is:

```go
switch proc.Type {
case "", spi.ProcessorTypeExternalized:
    // existing dispatch (SYNC / ASYNC_SAME_TX / ASYNC_NEW_TX / COMMIT_BEFORE_DISPATCH)
case spi.ProcessorTypeInternalized:
    return fmt.Errorf("processor %q: execution type %q is not yet implemented",
        proc.Name, proc.Type)
default:
    // tolerate unknown — pass through to executionMode branch.
    // Import-time validation (#255) is where unknown values become errors;
    // engine stays permissive to keep current parity behaviour.
}
```

The returned error is mapped by `classifyWorkflowError` to
`WORKFLOW_FAILED` (400) — entity stays in source state, cascade aborts. This
matches the failure surface of any other processor-level error.

**No issue IDs in the error message.** The message names the type value and
declares the absence of an implementation; it does not link to a tracker.

### 5.5 Validator (`internal/domain/workflow/validate.go`)

`validateProcessorFlags` is **unchanged in scope**. It continues to enforce
`StartNewTxOnDispatch=true` → `ExecutionMode=COMMIT_BEFORE_DISPATCH` only.

No new `Type`-axis validation rule is added here. Tightening unknown values
at import is explicitly #255's scope. #250's validator delta is zero.

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

Comment added above the test:

```yaml
# wf-import/03 originally exercised a scheduled-transition processor inside
# processors[]. That conflation has been removed from the schema; the
# scheduled-transition primitive will return at its proper home in a later
# carve-out. This test now covers group-criterion + function-criterion +
# externalized-processor round-trip only.
```

`json_equals_normalized` assertion is preserved unchanged.

### 5.8 Other docs hygiene (Gate 4 sweep)

- `README.md` — grep for `type=EXTERNAL`, `"EXTERNAL"` in workflow context,
  `scheduled.*processor`. Update or remove any matches.
- `docs/PROCESSOR_EXECUTION_MODES.md` — currently scoped to `executionMode`,
  not `type`. Verify no scheduled-processor cross-references; add a one-line
  note in §1 ("Quick Reference") if useful to anchor the `type` axis vs the
  `executionMode` axis.
- `CLAUDE.md` — grep for any mention of scheduled processors. Update if
  found.
- `cmd/cyoda/help/content/workflows.md` — covered above (§5.6).
- `cmd/cyoda/help/content/config/*.md` — untouched. No env-var changes.
- `DefaultConfig()` — untouched. No env-var changes.
- `COMPATIBILITY.md` — touched **only if** the SPI is re-tagged in this PR.
  Per §5.3, the SPI tag is bundled with #259/#260 — so #250 does not touch
  `COMPATIBILITY.md`.
- `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` — the audit references #250 as
  the schema-cleanup prerequisite (§H1, §M1, §0). After this PR merges, the
  audit's tracking remains accurate. No change.

## 6. Test plan (TDD)

### 6.1 Failing-test-first sequence

The TDD protocol (`.claude/rules/tdd.md`) drives this work. Tests are
authored before implementation, run to confirm RED, then driven GREEN.

1. **Engine dispatch — `Type: internalized` is rejected at fire time.**
   Add to `internal/domain/workflow/engine_processors_test.go` (or sibling
   test file): a unit test that builds a workflow whose transition has a
   single processor with `Type: "internalized"`, fires the transition, and
   asserts the error string contains `not yet implemented` and the entity
   remains in the source state. Run — must fail (RED). Implement §5.4. Run
   — must pass (GREEN).

2. **Engine dispatch — `Type: ""` and `Type: "externalized"` behave
   identically.** Existing tests cover the default-Type case (no `type` in
   the parity scenario's first processor). Add an explicit unit test for
   `Type: "externalized"` that asserts the dispatch path is the same gRPC
   path. Run — passes immediately (RED on the additive case may be empty;
   keep the test as a regression anchor).

3. **Engine dispatch — unknown `Type` value is tolerated.** A test with
   `Type: "scheduled"` (the legacy value) confirms the engine does not
   reject it; behaviour is the same as `Type: ""`/`Type: "externalized"`.
   This anchors §5.4's "default: pass through" branch and ensures the
   parity scenario's runtime tolerance stays intact even though OpenAPI no
   longer documents `scheduled`.

4. **E2E — POST /workflow/import accepts `Type: internalized` payload;
   subsequent entity creation that fires the internalized processor
   returns 400 `WORKFLOW_FAILED`.** Add to `internal/e2e/` an integration
   test using the in-process HTTP server.

5. **E2E — round-trip of a workflow with `Type: externalized` succeeds.**
   Verifies that the new oneOf in `processors[]` parses correctly on
   import (whether by the OpenAPI validator at handler level if applied,
   or by the existing JSON unmarshal).

6. **Parity — scenario 08/wf-import/03 passes after the scheduled-entry
   strip.** Run the parity test suite; the round-trip assertion holds.

### 6.2 Coverage gaps explicitly NOT closed in #250

- Import-time rejection of unknown `Type` values is **not** added. (#255
  scope.)
- Import-time rejection of malformed `ScheduledTransitionConfigDto`
  fields embedded in payloads (after the DTO is removed) — also not
  added. The audit (#H1) confirms these fields are silently dropped by
  the JSON unmarshal today; that behaviour persists until #259 lands the
  scheduled-transition shape at its proper home and #255 hardens unknown-
  field rejection.

## 7. Backwards compatibility

| Inbound payload shape | Behaviour before | Behaviour after #250 |
|---|---|---|
| `processors[]` with `Type` omitted | Accepted, dispatched as externalized | Same |
| `processors[]` with `Type: "externalized"` | Accepted, dispatched as externalized | Same |
| `processors[]` with `Type: "internalized"` | Accepted, dispatched as externalized (default branch behaviour) | **Accepted at import, rejected at engine dispatch with `WORKFLOW_FAILED`** |
| `processors[]` with `Type: "scheduled"` + scheduled config fields | Accepted (config fields silently dropped); dispatched as externalized at runtime | **Same** — runtime tolerance preserved; OpenAPI no longer documents the shape |
| `processors[]` with unknown `Type` | Accepted, dispatched as externalized | Same |

The wire surface is **strictly more conservative** in one place
(`internalized` now fails at runtime) and unchanged everywhere else. No
existing workflows that work today on cyoda-go v0.7.x stop working on
v0.8.0 unless they happened to carry `Type: "internalized"`, which is a
new value not yet in the wild.

## 8. Risks

- **Cyoda Cloud may still emit `Type: "scheduled"` in exports.** Parity
  is preserved because cyoda-go tolerates unknown `Type` values at the
  engine. Cloud's eventual move to the new scheduled-transition home
  (#259's deliverable) requires Cloud-side coordination; this PR makes no
  Cloud-side change.
- **#260's `InternalizedProcessorDefinitionDto` shape may differ from
  `ExternalizedProcessorDefinitionDto`.** When #260 lands, the
  `processors[]` `oneOf` will grow a second `$ref`. The current single-
  entry `oneOf` wrapper is forward-compatible.
- **SPI tag coupling.** Tagging cyoda-go-spi for this PR alone would
  create churn for downstream consumers (cyoda-go-cassandra, out-of-tree
  plugins) and violate the v0.8.0 "pay once" framing. Mitigation: defer
  the SPI tag to the last of #250/#259/#260 to land, document the SPI
  diff in a v0.8.0 SPI changelog entry.

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
| `api/openapi.yaml` | Rewrite `ProcessorDefinitionDto.type` enum; remove `ScheduledTransitionProcessorDefinitionDto`, `ScheduledTransitionConfigDto`; collapse `processors[]` oneOf. |
| `api/generated.go` | Regenerated from the above. |
| `cyoda-go-spi/types.go` (sibling repo) | Add `ProcessorTypeExternalized`, `ProcessorTypeInternalized` constants; add docstring on `ProcessorDefinition.Type`. |
| `internal/domain/workflow/engine_processors.go` | Add `Type`-axis branch at dispatch entry: empty/externalized → existing path; internalized → "not yet implemented" error; unknown → fall through. |
| `internal/domain/workflow/engine_processors_test.go` (or sibling) | TDD tests per §6.1 items 1–3. |
| `internal/e2e/` | E2E tests per §6.1 items 4–5. |
| `cmd/cyoda/help/content/workflows.md` | Lines 130, 135–139 rewritten per §5.6. |
| `e2e/externalapi/scenarios/08-workflow-import-export.yaml` | Strip scheduled entry from `wf-import/03`; retitle test; add comment. |
| `README.md` | Grep + correct any stale references. |
| `docs/PROCESSOR_EXECUTION_MODES.md` | Grep + optionally anchor the `type` vs `executionMode` axis distinction. |
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
- `make todos` — no new TODOs introduced.

## Appendix C: Out-of-tree plugin impact

The cyoda-go-cassandra plugin (commercial backend, separate repo) consumes
the SPI. Because §5.3 is a non-breaking SPI change (new constants +
docstring only, no field added/removed/renamed), Cassandra is unaffected.
Verification: run the parity registry against the cassandra plugin after
the SPI tag lands, alongside the in-tree plugins. Per memory
`feedback_cross_plugin_design_verification`, this verification is part of
the bundled v0.8.0 SPI tag landing, not #250 in isolation.
