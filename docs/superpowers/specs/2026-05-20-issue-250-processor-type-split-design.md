# Issue #250 — Split processor execution-location from scheduled-transition timing

**Date:** 2026-05-20
**Milestone:** v0.8.0
**Issue:** #250 (closed-by this PR)
**Prerequisite for:** #259 (scheduled-transition shape + SPI), #260 (internalized
processor execution-location shape + SPI)
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §H1 and §M1

> **Revision history**
>
> - **Revision 1 (2026-05-20):** initial spec.
> - **Revision 2 (2026-05-20):** incorporated first senior-architect review
>   (engine-dispatch integration corrected, audit-event coherence spelled
>   out, Type constants moved from SPI to `internal/domain/workflow`,
>   `discriminator.mapping` made explicit, test plan expanded, compat
>   table extended, silent-drop reframed as carried-over debt).
> - **Revision 3 (2026-05-21):** incorporated second senior-architect
>   review (engine-dispatch nested-switch shape, line range fixed, error
>   message de-doubled, SPI docstring matched to engine tolerance,
>   audit-event behaviour reframed as pass-through, docs sweep extended
>   to workflows.md:63 and grpc.md:219, vendored `docs/cyoda/*` declared
>   out-of-scope, parity-scenario sweep result recorded, test 5 pinned to
>   `A: SYNC`, test 3 expanded for case/whitespace, test 2b for empty
>   pipeline).
> - **Revision 4 (2026-05-22):** incorporated third senior-architect
>   review. Three load-bearing defects fixed: (a) **engine-dispatch
>   code shape rewritten as an early-return at the top of the
>   per-processor loop** — revision 3's nested-switch let `Type:
>   internalized + ExecutionMode: ASYNC_NEW_TX` fall through the line
>   109 abort gate (which keys on `proc.ExecutionMode`, not on rejection
>   origin), silently succeeding; the early-return is self-contained and
>   immune to the gate's behaviour. (b) **Audit-event cardinality
>   corrected** — fatal failures emit `SMEventStateProcessResult` TWICE,
>   once inside `executeProcessors` and once in the calling
>   `engine.go` (with the wrapped error string in Details);
>   revision 3 incorrectly claimed "exactly once" in §5.4 and test 4.
>   (c) **OpenAPI enum scope tightened** — `internalized` is removed from
>   the enum AND the discriminator mapping for #250; the engine still
>   rejects the value at fire time as a defensive guard for raw-JSON
>   clients; #260 will add `internalized` to enum, mapping, AND a new
>   subtype schema together. Also: `docs/PROCESSOR_EXECUTION_MODES.md`
>   added as a new file in #250's PR (was only present as untracked in
>   the parent worktree); existing test-fixture sweep added for stale
>   `Type: "EXTERNAL"`/`"INTERNAL"` strings (Gate 6); test 5 tightened
>   to differentiate in-memory `entity.Data` mutation from durable
>   persistence; test 8 trigger pinned; help-text wording tightened to
>   "any value other than `internalized` falls through"; `make generate`
>   spike got acceptance criteria; v0.7.x → v0.8.0 upgrade no-op
>   reassurance added.

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
  dispatch loop in `executeProcessors`
  (`internal/domain/workflow/engine_processors.go:42–118`) branches on
  `ExecutionMode`, not `Type`.
- `cmd/cyoda/help/content/workflows.md:135–139` claims `"EXTERNAL"` is the
  only valid value and that other values produce `errors.VALIDATION_FAILED`
  at import — neither claim matches the code. The value is documented in
  OpenAPI as `externalized`, not `EXTERNAL`, and import never rejects it.
  An example JSON at `cmd/cyoda/help/content/workflows.md:63` carries the
  same stale `"type": "EXTERNAL"` value; `cmd/cyoda/help/content/grpc.md:219`
  references an `EXTERNAL` processor in the gRPC compute-member section.
  Several test fixtures under `internal/domain/workflow/` also carry stale
  `Type: "EXTERNAL"`/`Type: "INTERNAL"` strings; these are normalised by
  this PR (§5.8 sweep).

The parity scenario `e2e/externalapi/scenarios/08-workflow-import-export.yaml`
`wf-import/03` imports a workflow with one externalized processor (no `type`
set on it) and one scheduled processor (`{"type":"scheduled",
"config":{"delayMs":300,"timeoutMs":30000,"transition":"Close"}}`) inside the
same `processors[]` array, and asserts round-trip equality. A
repository-wide grep for `"type":"scheduled"` confirms scenario 08 line 128
is the only match. The runtime accepts both processor entries because the
SPI's `Type` is a free string and `ScheduledTransitionConfigDto` fields
silently survive the unmarshal as part of `ProcessorConfig` (they don't
match `ProcessorConfig` fields and are dropped — see audit §H1 "the
scheduling config fields are silently dropped by JSON unmarshal").

## 2. Goals

- Reshape `api/openapi.yaml` so processor `type` is the execution-location
  axis only. For #250 the enum becomes `[externalized]` — a single value
  with an explicit `discriminator.mapping` entry. #260 later adds
  `internalized` to both the enum and the mapping, together with the new
  subtype schema. Keeping `internalized` out of the OpenAPI surface until
  its schema is defined avoids producing a strictly invalid document
  (enum value with no discriminator target).
- Remove `ScheduledTransitionProcessorDefinitionDto` and
  `ScheduledTransitionConfigDto` from the OpenAPI surface entirely. The new
  home for scheduled-transitions is the next carve-out (#259); both ship
  in the same v0.8.0 release, so no transition-window divergence between
  OpenAPI and Cloud-emitted payloads exists.
- Update the SPI `ProcessorDefinition.Type` field's docstring to declare its
  meaning (execution-location axis) and reflect the engine's actual
  tolerance behaviour (only the explicit `internalized` value is rejected;
  unknown values fall through). Surface untyped string constants for the
  two recognised values in `internal/domain/workflow` (the engine owns the
  values; the SPI is the wire type — same split as `ExecutionMode*` per
  `validate.go:13-21`).
- Bring `cyoda help workflows`, `cyoda help grpc`, and any other affected
  docs into sync with actual behaviour. Help text is derived from the
  codebase, not authoritative on its own. Add
  `docs/PROCESSOR_EXECUTION_MODES.md` as a new doc — drafted under v0.8.0
  scoping — with a §0 axis-summary anchoring the `type` × `executionMode`
  distinction.
- Reject `type: internalized` at engine dispatch with a clear,
  self-contained "not yet implemented" error message. Implemented as an
  early-return at the top of the per-processor loop so the rejection
  cannot be silently swallowed by the existing ExecutionMode-keyed abort
  gate (engine_processors.go:109). The engine-side guard is the
  defensive backstop for raw-JSON clients that bypass the OpenAPI DTOs.

## 3. Non-goals

- Implement scheduled-transition firing. (#259 owns the SPI shape, #251 owns
  the runtime — timer persistence, cluster ownership, work-stealing recovery,
  audit surface.)
- Implement internalized processor execution. (#260 owns the SPI shape +
  registration + the new `InternalizedProcessorDefinitionDto` OpenAPI
  subtype + enum/mapping extension, #252 owns the runtime.)
- Tighten import-time validation for unknown `type` values. That is #255's
  scope (state-graph, names, `ExecutionMode` enum, `Type` enum). #250
  reshapes the schema only; #255 enforces it at import.
- Wire `ProcessorConfig.Context` or `RetryPolicy`. (#253, #254, #262.)
- Touch the parity contract for `ProcessorConfig.AttachEntity` `bool` vs
  `*bool` discrepancy (audit §M1, separate consistency concern).
- Modify the vendored upstream OpenAPI copies under `docs/cyoda/`
  (`openapi.yml`, `api/openapi-workflow.yml`, `api/openapi-common.yml`).
  These are point-in-time snapshots of the Cloud contract maintained
  upstream and synchronised through a separate workflow; they will pick
  up the cleanup when Cloud lands the equivalent change.

## 4. Decision summary (post-clarification + three review passes)

| Question | Decision |
|---|---|
| OpenAPI scope | **Full split now.** Remove ScheduledTransitionProcessorDefinitionDto + ScheduledTransitionConfigDto entirely. processors[] oneOf collapses to ExternalizedProcessorDefinitionDto (single-member oneOf, retained as a forward-extension point for #260). |
| OpenAPI enum scope | **`type: enum: [externalized]`** for #250. `internalized` is added to the enum AND the discriminator mapping AND a new `InternalizedProcessorDefinitionDto` subtype together by #260. Keeping the enum consistent with the mapping avoids producing a strictly invalid OpenAPI document. The engine rejects `Type: "internalized"` at fire time regardless (defensive guard for raw-JSON clients). |
| OpenAPI discriminator | **Explicit `discriminator.mapping` block.** Mapping contains only `externalized: "#/components/schemas/ExternalizedProcessorDefinitionDto"`. Generator behaviour for value-to-schema mapping varies; explicit + consistent-with-enum is safest. |
| Where to reject `internalized` | **At engine fire time via early-return at the top of the per-processor loop.** §5.4 spells out the exact code. Mirrors #259's planned pattern for scheduled-transition runtime rejection. Import accepts (validation tightening is #255's scope). |
| Engine-dispatch integration shape | **Early-return from a Type-axis check at the top of the per-processor loop**, emitting the per-processor `SMEventStateProcessResult` audit event explicitly inside the branch (mirroring the line 104 shape) and calling `rollbackOpenSegmentOnFailure` before returning. **Not** a nested switch around the ExecutionMode dispatch (revision 3's choice), because the post-switch abort gate at line 109 keys on `proc.ExecutionMode` and would let `Type: internalized + ExecutionMode: ASYNC_NEW_TX` silently succeed. |
| Constants for Type values | **In `internal/domain/workflow`,** not the SPI. The SPI carries the wire field; the engine owns the value semantics. Same split as `ExecutionMode*` constants — see precedent + rationale in `validate.go:13-15`. |
| SPI `Type` field | **Keep, document.** Docstring matches engine tolerance: only `internalized` is rejected; unknown values fall through. No new symbols. SPI change is comment-only, so no SPI tag is forced by #250. |
| Audit-event behaviour | **Two `SMEventStateProcessResult` emits per fatal failure** — one inside `executeProcessors` (with `Data: {success: false, mode: <declared ExecutionMode>}`, Details `Processor %q completed`), one inside the calling `engine.go` failure path (with `Data: {success: false}`, no `mode`, Details embedding the full wrapped error string). The second emit's Details DOES describe the rejection cause; an audit consumer reading Details can see exactly which Type value triggered the rejection. This is the existing two-event shape for every fatal processor failure (SYNC/CBD); the internalized rejection does not change it. |
| Parity scenario 08 | **Strip the scheduled entry from `wf-import/03`.** Grep of `e2e/externalapi/scenarios/` for `"type":"scheduled"` confirmed scenario 08 line 128 is the only match. |
| Test-fixture sweep | **Normalise stale `Type: "EXTERNAL"`/`"INTERNAL"`/`"external"` strings** in `internal/domain/workflow/**_test.go` to `workflow.ProcessorTypeExternalized` (or drop where the value is irrelevant to the test). Gate 6 — resolve, don't defer. |
| `docs/PROCESSOR_EXECUTION_MODES.md` | **New file, lands in #250's PR.** A draft is currently in the parent worktree's working copy at `docs/PROCESSOR_EXECUTION_MODES.md` (untracked, written for v0.8.0). The first implementation commit copies the file in verbatim from the parent worktree, the second adds the §0 axis-summary anchoring `type` vs `executionMode`. Subsequent v0.8.0 carve-outs (#259, #260) extend the doc. |
| Error messages | **No issue-ID references in any shipped artefact.** Self-contained phrasing in error messages, log lines, code comments, OpenAPI descriptions, and help-topic content. Issue IDs are appropriate only in this spec doc, PR bodies, commit messages, and CHANGELOG. |
| Source of truth for Type strings | **`internal/domain/workflow` constants** are authoritative. Engine code references those, not the regenerated `api/generated.go`'s `ProcessorDefinitionDtoType*` constants (which are an artefact of `oapi-codegen` and should not leak across package boundaries). |

## 5. Detailed design

### 5.1 OpenAPI changes (`api/openapi.yaml`)

**`ProcessorDefinitionDto`** — `type` is a constrained enum containing only
the values that have a corresponding subtype schema. For #250 that is
exactly `externalized`. The discriminator mapping mirrors the enum.

```yaml
ProcessorDefinitionDto:
  type: object
  discriminator:
    propertyName: type
    mapping:
      externalized: "#/components/schemas/ExternalizedProcessorDefinitionDto"
  properties:
    type:
      type: string
      enum:
        - externalized
      description: |
        Processor execution-location axis. `externalized` dispatches the
        processor via gRPC to a calculation node selected by
        `calculationNodesTags`. Empty or omitted is treated as
        `externalized` by the runtime. The engine reserves the value
        `internalized` for an in-process execution location not yet
        implemented; any payload carrying `type: "internalized"` is
        rejected at engine dispatch with `WORKFLOW_FAILED` (400). The
        reserved value is intentionally absent from this enum until the
        in-process subtype lands together with its schema.
    name:
      type: string
      description: Name of the processor
  required:
    - name
```

**`ExternalizedProcessorDefinitionDto`** — unchanged in the schema. (The
inherited `Type` field comes via `allOf` composition from
`ProcessorDefinitionDto`; no `Type` is declared directly on the child.)
Note that `oapi-codegen` materialises the inherited field as a sibling
`Type *string` on the child Go struct (current behaviour at
`api/generated.go:2089`) — that is a generator artefact, scoped to
`api/generated.go`, and inherits semantics from the parent's description.
Engine code does not depend on the child-struct `Type` field; the
authoritative source is the parent's enum + the
`internal/domain/workflow` constants.

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

**Verification spike on the regenerated `api/generated.go`** is part of
the implementation sequence. Concrete acceptance criteria for the spike:

1. `make generate` runs to completion without error.
2. `go build ./...` is clean post-regeneration.
3. `api/generated.go` retains `AsExternalizedProcessorDefinitionDto`,
   `FromExternalizedProcessorDefinitionDto`, and
   `MergeExternalizedProcessorDefinitionDto` method signatures on the
   `TransitionDefinitionDto_Processors_Item` (or equivalent) type — i.e.,
   the union-helper API is structurally preserved for the single member.
4. `api/generated.go` no longer contains the three
   `*ScheduledTransitionProcessorDefinitionDto` union helpers nor the
   two `ScheduledTransition*Dto` struct declarations.

If any of (1)–(3) fails, the fallback is to drop the `oneOf` wrapper and
direct-reference `ExternalizedProcessorDefinitionDto` from
`TransitionDefinitionDto.processors[].items`; #260 reintroduces `oneOf`
when adding the second subtype. Document the fallback choice in the PR
description.

### 5.2 Generated DTOs (`api/generated.go`)

Regenerated from the updated `openapi.yaml`. The following entries
**disappear**:

- `type ScheduledTransitionConfigDto struct { … }` (line ~2382)
- `type ScheduledTransitionProcessorDefinitionDto struct { … }` (line ~2394)
- `AsScheduledTransitionProcessorDefinitionDto()` / `From…()` / `Merge…()`
  union helpers (line ~4139–4156)

The `ExternalizedProcessorDefinitionDto` entry and its union helpers
(`AsExternalizedProcessorDefinitionDto` at ~line 4113 et seq.) stay. The
`ProcessorDefinitionDto` struct gains a generator-emitted single-value
type constant — likely `ProcessorDefinitionDtoTypeExternalized = "externalized"`.
The previously-emitted `ProcessorDefinitionDtoTypeScheduled` constant
disappears. **These regenerator-emitted constants exist as a side effect
of the generator and are not the authoritative source of truth.** Engine
code in `internal/domain/workflow` references its own constants (§5.5);
`api/generated.go`'s constants are usable only inside `api/` and at the
boundary between OpenAPI tooling and the rest of the codebase.

`make generate` (or whichever target regenerates `api/generated.go`) is run
as part of the change.

### 5.3 SPI changes (`cyoda-go-spi/types.go`)

Repo: `https://github.com/Cyoda-platform/cyoda-go-spi`. Co-located at
`../cyoda-go-spi`.

**`ProcessorDefinition.Type`** — keep the field, add docstring only. No new
constants are exported by the SPI; the engine owns the value semantics
(see §5.5, mirroring how `ExecutionMode` is treated). The docstring
matches the engine's actual behaviour (§5.4) — only the explicit
`internalized` value is rejected; empty defaults to externalized; unknown
values fall through to the ExecutionMode dispatch path.

```go
// ProcessorDefinition represents a processor attached to a transition.
type ProcessorDefinition struct {
    // Type is the execution-location axis. Recognised values are defined
    // by the cyoda-go engine package; canonical values are "externalized"
    // (dispatched via gRPC to a calculation node selected by
    // Config.CalculationNodesTags) and "internalized" (reserved for an
    // in-process execution location, currently rejected at engine
    // dispatch as not yet implemented). Empty is treated as "externalized".
    // Any value other than "internalized" falls through to the
    // ExecutionMode dispatch path; import-time validation does not
    // constrain this field.
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

**Integration point.** The check goes at **the top of the per-processor
loop** in `executeProcessors`, between line 59 (loop entry) and line 60
(local `success`/`procErr` declarations). It is an **early-return**, not
a nested switch — the existing per-failure abort gate at line 109 keys
on `proc.ExecutionMode != ExecutionModeAsyncNewTx`, so a non-returning
rejection followed by fall-through would let `Type: internalized +
ExecutionMode: ASYNC_NEW_TX` silently succeed.

```go
for _, proc := range processors {
    // Execution-location axis. Rejection is fatal and self-contained:
    // emit the per-processor SMEventStateProcessResult audit row
    // explicitly (mirroring the shape recorded by the post-dispatch
    // emit at lines 104-106), perform the same segment-rollback that
    // the line 109 abort gate would, then return. The caller's
    // failure path in engine.go emits its own SMEventStateProcessResult
    // with the wrapped error in Details (lines 466 / 553).
    if proc.Type == ProcessorTypeInternalized {
        auditData := map[string]any{
            "success": false,
            "mode":    proc.ExecutionMode,
        }
        e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
            spi.SMEventStateProcessResult,
            fmt.Sprintf("Processor %q completed", proc.Name), auditData)
        e.rollbackOpenSegmentOnFailure(currentCtx, currentTxID, txID, proc.Name)
        return currentCtx, currentTxID, fmt.Errorf(
            "processor %s failed: execution type %q is not yet implemented",
            proc.Name, proc.Type)
    }

    var success bool
    var procErr error

    switch proc.ExecutionMode {
        // ... unchanged: lines 63-87 of the existing executeProcessors body ...
    }

    // ... unchanged: lines 89-115 (audit emit + abort gate) ...
}
```

**Failure surface.** The wrapped error chain is
`fmt.Errorf("processor %s failed: execution type %q is not yet implemented", proc.Name, proc.Type)`.
`classifyWorkflowError` (`internal/domain/entity/service.go:1449-1457`)
matches no specific sentinel, falls through to the default branch, and
returns `common.Operational(http.StatusBadRequest,
common.ErrCodeWorkflowFailed, err.Error())`. The HTTP response is
`400 WORKFLOW_FAILED`. The entity stays in the source state because the
engine never reaches the post-`executeProcessors` state-mutation line.

**Audit-event behaviour.** The full audit trail for a single internalized
rejection (assuming `executeProcessors` is called once for the failing
transition) is:

1. `SMEventProcessingPaused` at the start of `executeProcessors` (line 52-54
   in the existing code; **only emitted if `len(processors) > 0`** because
   of the early return at line 43-45). Recorded with the cascade-entry
   txID for correlation. Details: `"Paused for processors: [<names>]"`.
2. `SMEventStateProcessResult` from the early-return's explicit emit (the
   new code above). `Data: {success: false, mode: <declared ExecutionMode>}`.
   Details: `"Processor \"<name>\" completed"`. For an internalized
   processor with `executionMode` unset on the import payload, `mode`
   audits as `""`; for one with `executionMode: "SYNC"` declared, `mode`
   audits as `"SYNC"`. The `mode` field is a pass-through of the author's
   declaration — no synthesised sentinel value.
3. `SMEventStateProcessResult` from the caller-level failure emit
   (`engine.go:466` for manual transitions, `:553` for cascaded automatic
   transitions). `Data: {success: false}` — no `mode` key. Details:
   `"Processor failed for transition \"<transition>\": processor <name> failed: execution type \"internalized\" is not yet implemented"`.
   This second emit IS the audit-trail's self-descriptive record of the
   rejection cause: an audit consumer reading Details can see the
   processor name and the rejection sub-string. This is the existing
   two-event shape for every fatal processor failure (SYNC/CBD/timeout
   etc.); the internalized rejection does not change it.

`SMEventStateMachineFinish` is not emitted — correct, the cascade aborted.

**Operator-side slog.** No `slog.Warn` is emitted at the rejection site.
This is consistent with the existing engine pattern: fatal failure paths
(SYNC/CBD failures, ASYNC_SAME_TX failures) write the audit event but
do not duplicate it to slog. Only `ASYNC_NEW_TX` (non-fatal) calls
`slog.Warn` at `engine_processors.go:70` because the failure is
swallowed and would otherwise be invisible. The internalized rejection
is fatal, surfaces as a `400 WORKFLOW_FAILED` response, and is recorded
in two audit events; no slog is added.

**No issue IDs in the error message.** The message names the type value
and declares the absence of an implementation; it does not link to a
tracker.

### 5.5 Validator + new constants (`internal/domain/workflow/validate.go` or sibling)

`validateProcessorFlags` is **unchanged in scope.** It continues to enforce
only `StartNewTxOnDispatch=true` → `ExecutionMode=COMMIT_BEFORE_DISPATCH`.
No new `Type`-axis validation rule is added here.

The new constants for `ProcessorDefinition.Type` values are added alongside
the existing `ExecutionMode*` block (validate.go:10-21). The choice of file
(`validate.go` vs. a new `processor_types.go`) is a judgement call for the
implementer; the constants must live in `internal/domain/workflow` and be
exported.

```go
// Processor execution-location tokens. Sourced from the OpenAPI enum in
// api/openapi.yaml (mirrored in api/generated.go's ProcessorDefinitionDto
// type constants). Centralised here as untyped strings so engine logic,
// validator rules, and tests can compare against a single source — the
// SPI's ProcessorDefinition.Type field is itself a plain string, so an
// enum type would not buy compile-time safety.
//
// Empty value is treated as ProcessorTypeExternalized. Any value other
// than ProcessorTypeInternalized falls through to the ExecutionMode
// dispatch path at engine fire time; the only Type rejection performed
// by the engine is on the exact value ProcessorTypeInternalized.
const (
    ProcessorTypeExternalized = "externalized"
    ProcessorTypeInternalized = "internalized"
)
```

This mirrors the precedent for `ExecutionMode*` (validate.go:13-21).
Anywhere engine code or test code needs to name a Type value, it imports
these constants. The regenerated `api/generated.go` constants (e.g.
`ProcessorDefinitionDtoTypeExternalized`) are an OpenAPI-tooling artefact
and stay scoped to that package.

### 5.6 Help text (`cmd/cyoda/help/content/workflows.md`)

Three contiguous changes:

**(a) Line 63 — stale `EXTERNAL` in an example JSON payload.** The example
shows a processor with `"type": "EXTERNAL"` (uppercase). Update to
`"type": "externalized"` (the OpenAPI value) or remove the `type` field
entirely (it is optional and defaults to externalized in the runtime).

**(b) Line 130 — field listing.** Keep `type` in the field summary and
add a one-line note: "execution-location axis; see below for valid
values".

**(c) Lines 135–139 — the EXTERNAL claim.** Rewrite:

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

The engine reserves the value `"internalized"` for an in-process
execution location not yet implemented. Any transition that fires a
processor with `type: "internalized"` is rejected at dispatch with
`WORKFLOW_FAILED` (400) and the operator-visible error
`processor X failed: execution type "internalized" is not yet implemented`.
The reserved value is intentionally absent from the OpenAPI enum until
the subtype lands; workflow authors who include it in import payloads
will not be rejected at import, but their entities cannot transit past
the affected step.

Any value other than `"internalized"` (including the empty string, the
canonical `"externalized"`, and unknown values such as legacy
`"scheduled"` or `"EXTERNAL"`) falls through to the `executionMode`
dispatch path. This permissiveness will narrow in a future release; do
not rely on it.
```

### 5.7 Parity scenario (`e2e/externalapi/scenarios/08-workflow-import-export.yaml`)

A repository-wide grep for `"type":"scheduled"` across
`e2e/externalapi/scenarios/` confirms that scenario 08 line 128 is the
only match — no other scenario carries a scheduled-processor payload.
`wf-import/03` is the affected test. The import body's `processors[]`
array currently contains two entries:

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

### 5.8 Other docs hygiene + test-fixture sweep (Gate 4 + Gate 6 sweep)

- **`README.md`** — grep confirmed no `EXTERNAL` or scheduled-processor
  references. No change required, but re-run the grep as a verification
  step.
- **`cmd/cyoda/help/content/grpc.md:219`** — references an `EXTERNAL`
  processor in the gRPC compute-member section: "Server sends
  `EntityProcessorCalculationRequest` when a workflow transition invokes
  an `EXTERNAL` processor whose `calculationNodesTags` matches...". Update
  to `externalized` (matching the OpenAPI enum and §5.6 help text).
- **`docs/PROCESSOR_EXECUTION_MODES.md`** — **new file in #250's PR.** A
  draft was prepared during the v0.8.0 audit (currently untracked in the
  parent worktree). The first implementation commit copies the file
  verbatim into the worktree; a follow-up commit adds the §0 axis-summary:

  > **§0. Axis Summary.** Workflow processors have two orthogonal
  > configuration axes. `type` (this document refers to it as
  > "execution-location") determines where the processor runs — currently
  > only `externalized` (gRPC dispatch to a calculation node) is
  > implemented; `internalized` is reserved for in-process execution.
  > `executionMode` (the focus of this document) determines the
  > transactional semantics of dispatch — `SYNC`, `ASYNC_SAME_TX`,
  > `ASYNC_NEW_TX`, `COMMIT_BEFORE_DISPATCH`. All `executionMode`
  > semantics described below apply to `externalized` processors; the
  > `internalized` axis has no documented dispatch semantics yet.

- **`CLAUDE.md`** — grep confirmed no scheduled-processor or stale
  `EXTERNAL` references. No change required; re-run the grep.
- **`cmd/cyoda/help/content/workflows.md`** — covered in §5.6.
- **`cmd/cyoda/help/content/config/*.md`** — untouched. No env-var
  changes.
- **`DefaultConfig()`** — untouched. No env-var changes.
- **`COMPATIBILITY.md`** — untouched. SPI change is comment-only (§5.3),
  no tag bump forced by #250.
- **`docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md`** — the audit references #250
  as the schema-cleanup prerequisite (§H1, §M1, §0). After this PR
  merges, the audit's tracking remains accurate. No change.
- **`docs/cyoda/openapi.yml`, `docs/cyoda/api/openapi-workflow.yml`,
  `docs/cyoda/api/openapi-common.yml`** — **explicitly out of scope.**
  Vendored upstream Cloud snapshots, synchronised through a separate
  workflow.
- **Test-fixture sweep (`internal/domain/workflow/*_test.go`)** — Gate 6
  cleanup. Existing tests carry stale `Type: "EXTERNAL"` (uppercase) and
  `Type: "INTERNAL"` and one `Type: "external"` (lowercase non-canonical
  but functionally a fall-through value). Replace each occurrence with
  one of:
  - `workflow.ProcessorTypeExternalized` (for fixtures that are
    conceptually externalized — the majority).
  - drop the assignment entirely (for fixtures where the value is
    irrelevant to the test's assertion and the test relies only on
    `Name` / `ExecutionMode`).
  - `workflow.ProcessorTypeInternalized` (for new tests added by §6.1).

  Concrete grep command for the implementer:
  ```bash
  grep -rn 'Type:.*"\(EXTERNAL\|INTERNAL\|external\|scheduled\)"' \
       --include="*_test.go" internal/domain/workflow/
  ```
  Each match is normalised in the same PR. The `Type: "INTERNAL"`
  fixtures are particularly important: post-#250 they are one keystroke
  away from triggering the rejection path. Leaving them as test
  fixtures is a Gate 6 violation — fix now.

## 6. Test plan (TDD)

### 6.1 Failing-test-first sequence

The TDD protocol (`.claude/rules/tdd.md`) drives this work. Tests are
authored before implementation, run to confirm RED, then driven GREEN.
The unit-test target is `internal/domain/workflow/engine_processors_test.go`
or a sibling test file in the same package.

1. **Engine dispatch — `Type: internalized` is rejected at fire time
   regardless of `ExecutionMode`.** **Four matrix cases** (this is the
   F1 defect coverage that revisions 1-3 missed):
   - `Type: internalized + ExecutionMode: <unset>` → reject.
   - `Type: internalized + ExecutionMode: "SYNC"` → reject.
   - `Type: internalized + ExecutionMode: "ASYNC_NEW_TX"` → reject.
     **This is the critical case** — revision 3's nested-switch design
     would have let this silently succeed because the line 109 abort
     gate keys on `proc.ExecutionMode`. The early-return shape (§5.4)
     does not depend on the gate.
   - `Type: internalized + ExecutionMode: "COMMIT_BEFORE_DISPATCH"` → reject.

   All four assert: returned error contains `execution type
   "internalized" is not yet implemented`; outer wrap prefixes
   `processor <name> failed:`; the entity remains in the source state;
   no segment is left open (verify via `txMgr.Rollback` was called or
   no orphan TX visible to a subsequent `Get`).

2. **Engine dispatch — `Type: ""` and `Type: "externalized"` behave
   identically to today.** Two test cases, same workflow shape, asserting
   identical gRPC dispatch paths. Anchors regression detection for the
   new Type-axis check.

   **2b. Empty `processors[]` is a no-op regardless of any per-processor
   Type.** A transition with no processors at all imports, fires, and
   reaches the target state without entering the per-processor loop
   (early return at `engine_processors.go:43-45`). No
   `SMEventProcessingPaused` is emitted. Pins that the new Type-axis
   check does not regress the empty-pipeline path.

3. **Engine dispatch — unknown `Type` values fall through.** Cases:
   - `Type: "scheduled"` (legacy value removed from OpenAPI in this PR).
   - `Type: "EXTERNALIZED"` (case-mismatched uppercase).
   - `Type: " externalized "` (whitespace-padded).
   - `Type: "internalized\nfoo"` (newline injection — exact-match
     boundary).
   - `Type: "future_unknown_value"`.

   All five must dispatch via the ExecutionMode path and behave
   indistinguishably from `Type: "externalized"`. The case-sensitivity
   is exact — no normalisation, no trimming, no embedded-control-char
   handling beyond Go's natural `==` semantics.

4. **Audit-event sequence for the internalized rejection.** Two
   sub-cases anchoring the §5.4 audit contract:

   4a. **Manual transition firing.** Same workflow as test 1, fired via
   an explicit `POST /entity/X/transition/Y` (or the test's manual-fire
   helper). Assert the emitted sequence:
   - 1× `SMEventProcessingPaused` (from `executeProcessors:52-54`),
     with cascade-entry txID, Details `"Paused for processors: [<names>]"`.
   - 1× `SMEventStateProcessResult` from the early-return's explicit
     emit, with `Data: {success: false, mode: <declared ExecutionMode>}`
     (so `mode: ""` for the unset-ExecutionMode case, `mode: "SYNC"`
     for the SYNC case, etc.), Details `"Processor \"<name>\" completed"`.
   - 1× `SMEventStateProcessResult` from the caller's failure emit at
     `engine.go:466`, with `Data: {success: false}` (no `mode` key),
     Details containing `"Processor failed for transition \"<transition>\": processor <name> failed: execution type \"internalized\" is not yet implemented"`.
   - 0× `SMEventStateMachineFinish`.

   4b. **Cascaded automatic transition firing.** A workflow where the
   internalized processor lives on a Manual=false transition reached via
   cascade. Same audit sequence; second `SMEventStateProcessResult`
   comes from `engine.go:553` (cascade path) instead of `:466` (manual
   path). Assert the per-emit shape is identical.

5. **Cascade-position behaviour — `A: SYNC` succeeds, `B: internalized`
   aborts, `C` is never dispatched.** Workflow with three processors in
   order: `A_SYNC` (succeeds, returns a mutation to `entity.Data`),
   `B_internalized`, `C_SYNC` (would succeed if dispatched). The test
   distinguishes three observable layers — pick whichever the existing
   harness supports:

   - **In-memory `entity.Data`:** A's returned mutations are written
     to `entity.Data` by `executeSyncProcessor` at
     `engine_processors.go:148`. After B's rejection,
     `executeProcessors` returns the wrapped error and the engine never
     calls `executeProcessors`'s remaining iterations — C is never
     dispatched. So the in-memory entity carries A's mutations.
   - **In-memory `entity.Meta.State`:** unchanged from source. The
     engine never reaches the post-`executeProcessors` line that sets
     `entity.Meta.State = transition.Next`.
   - **Durable persistence:** depends on the caller's transaction
     control. The engine does not itself `Save` during SYNC dispatch;
     the caller's handler rolls back the open TX on the wrapped error.
     The durable row reflects the pre-transition state.

   The test asserts: returned error contains the rejection sub-string;
   `entity.Meta.State` is the source state; C was not invoked (verify
   via a test-double processor counter that A's count is 1, C's is 0);
   the caller-side `Save` either didn't fire or rolled back (test
   asserts a fresh `Get` returns the pre-transition entity row).

   Variant tests for `A: ASYNC_NEW_TX` and `A: COMMIT_BEFORE_DISPATCH`
   are out of scope for #250 — those transactional shapes have their
   own well-tested abort semantics independent of the Type-axis check.

6. **Round-trip of unknown `Type` values on the wire.** Import a
   workflow with `Type: "scheduled"` (or `"future_unknown_value"`),
   export, assert the exported JSON preserves the `type` field value
   verbatim. This confirms that removing the DTO from OpenAPI does not
   change wire behaviour for the SPI's free-string field.

7. **Validator ordering — `StartNewTxOnDispatch` flag-coherence runs at
   import; Type rejection runs at fire.** Two test cases:
   - `Type: internalized + ExecutionMode: SYNC + StartNewTxOnDispatch: true`.
     The flag is incoherent without `COMMIT_BEFORE_DISPATCH`, so
     import-time `validateProcessorFlags` (called from `validateWorkflows`
     at `validate.go:33`) returns `VALIDATION_FAILED` (400) before any
     engine dispatch can run.
   - `Type: internalized + ExecutionMode: COMMIT_BEFORE_DISPATCH +
     StartNewTxOnDispatch: true`. Imports successfully; engine rejects at
     fire time with the internalized "not yet implemented" error.

8. **E2E — manual-transition-fired internalized processor returns 400
   `WORKFLOW_FAILED`.** Integration test in `internal/e2e/` using the
   in-process HTTP server. Test sequence (model on existing
   `internal/e2e/` workflow tests):
   1. Create an entity model.
   2. `POST /model/X/1/workflow/import` with a workflow whose initial
      state has a Manual=true transition with a single processor
      `{type: "internalized", name: "calc", executionMode: "SYNC"}`.
      Assert HTTP 200 (import passes — no Type-axis validation at
      import).
   3. `POST /entity/X` to create an instance — initial state, no
      cascade fires.
   4. `POST /entity/X/<id>/transition/<name>` to fire the manual
      transition. Assert HTTP 400 with error code
      `WORKFLOW_FAILED` and the response body's message contains
      `execution type "internalized" is not yet implemented`.
   5. `GET /entity/X/<id>` returns the entity in the initial state
      (source state preserved).

   The OpenAPI request validator is not applied at handler level —
   `handler.go:43` unmarshals raw JSON into `spi.WorkflowDefinition` —
   so the import step succeeds regardless of the OpenAPI enum change.

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
| `processors[]` with `Type: "internalized"` | Accepted, dispatched as externalized (default branch) | **Accepted at import, rejected at engine dispatch with `WORKFLOW_FAILED` 400 regardless of declared `ExecutionMode`.** Rejection covers `ExecutionMode` unset, SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH. |
| `processors[]` with `Type: "scheduled"` + scheduled config fields (raw JSON path) | Accepted at handler.go:43 (DTOs bypassed); scheduled config fields silently dropped by SPI unmarshal; dispatched as externalized at runtime | **Same wire behaviour.** Carried-over technical debt, not a feature — audit §H1 grades the silent-drop as HIGH. #255 may tighten; #250 deliberately preserves the runtime tolerance to avoid breaking parity payloads. Same path applies to third-party tools pinned to v0.7.x OpenAPI emitting `type: "scheduled"` payloads, and to v0.7.x-stored workflows read by the engine on v0.8.0 startup (no upgrade migration required — the on-disk JSON survives untouched). |
| `processors[]` with `Type: "scheduled"` (clients using generated OpenAPI DTOs from v0.7.x) | Validated by the generated client against ScheduledTransitionProcessorDefinitionDto; serialised, sent, accepted | **Fails client-side serialisation.** ScheduledTransitionProcessorDefinitionDto no longer exists in api/generated.go. Client-side union helpers `AsScheduledTransitionProcessorDefinitionDto` / `FromScheduledTransitionProcessorDefinitionDto` / `MergeScheduledTransitionProcessorDefinitionDto` also disappear. Clients regenerating from the v0.8.0 OpenAPI cannot construct the shape. Acceptable because #259 ships in the same release with the replacement shape — clients regenerate once at v0.8.0 and pick up both changes. |
| `processors[]` with unknown `Type` (any case, whitespace, control chars) | Accepted, dispatched as externalized | Same. Type is exact-match against `"internalized"` only; no normalisation. |

The wire surface is **strictly tighter** in exactly one place
(`Type: internalized` now fails at runtime where previously it
default-dispatched). Everywhere else the wire is identical. The DTO
surface, however, is strictly tighter for two values (`scheduled` removed
entirely; `internalized` not yet added to OpenAPI in #250) — DTO-using
clients regenerate at v0.8.0 and pick up the new shape together with
#259's replacement and (later) #260's `internalized` subtype.

**v0.7.x → v0.8.0 upgrade.** No migration is required. Workflows
persisted under v0.7.x with any `Type` value (`""`, `"externalized"`,
`"scheduled"`, anything else) continue to operate identically on
v0.8.0. The only behaviour change for upgrade traffic is for workflows
that happen to carry `Type: "internalized"` — those weren't producible
in v0.7.x via DTO clients and would have come from a raw-JSON client
authoring against the future-reserved value; on v0.8.0 they fail at
fire time with the rejection error.

## 8. Risks

- **OpenAPI generator behaviour around discriminator + oneOf with a single
  member.** Some `oapi-codegen` versions emit broken or unused union
  helpers when the `oneOf` has one member. Mitigation: the §5.1 explicit
  `discriminator.mapping` block, plus the verification-spike acceptance
  criteria. Fallback: drop the `oneOf` wrapper and direct-reference
  `ExternalizedProcessorDefinitionDto`; #260 reintroduces `oneOf` when
  it needs to.
- **Generator-emitted child `Type` field.** `oapi-codegen` materialises
  the inherited `Type` field on `ExternalizedProcessorDefinitionDto` as a
  sibling on the child struct (current behaviour at `api/generated.go:2089`).
  Post-regeneration the child struct still carries this field, and its
  description may not perfectly track the parent's. Since engine code
  uses the in-package constants (§5.5) and does not consume the child's
  `Type` field directly, this is a cosmetic generator artefact, not a
  behaviour risk. Verification spike per §5.1 catches any actual
  divergence.
- **#260's `InternalizedProcessorDefinitionDto` shape may differ from
  `ExternalizedProcessorDefinitionDto`.** When #260 lands, the
  `processors[]` `oneOf` grows a second `$ref`. The single-entry `oneOf`
  is forward-compatible. The enum, mapping, and a new subtype schema all
  land together to preserve OpenAPI document validity.
- **SPI tag coupling.** Since §5.3 is comment-only, #250 does not force a
  SPI tag at all. v0.8.0's bundled SPI tag (carrying #259's and #260's
  substantive shape work) picks up the docstring change as part of the
  bundle.
- **Cyoda Cloud parity.** Cloud will need to regenerate its OpenAPI
  client and pick up the v0.8.0 changes (the new scheduled-transition
  shape from #259 + the Type-axis from #250 + the internalized reservation
  from #260). All three carve-outs ship in v0.8.0 so Cloud regenerates
  once. v0.7.x payloads emitted by an out-of-sync Cloud continue to be
  accepted (silent-drop of legacy scheduled config, runtime treats as
  externalized) — the engine's forward-compat is wider than the OpenAPI
  surface.
- **`api/generated.go` constants vs `internal/domain/workflow` constants.**
  Two source-of-truth sets exist post-regeneration. The §5.2 declaration
  resolves this — engine code uses the workflow-package constants;
  `api/generated.go`'s constants are scoped to that package. The rule is
  aspirational (no CI lint enforces it); implementers must respect it by
  convention. A future audit may add a `forbidigo`/`depguard` check if
  drift is observed.

## 9. Out of scope (sanity check)

- Scheduled-transition runtime, durability, cluster ownership — #251.
- Scheduled-transition SPI shape (sibling field on TransitionDefinition or
  array on StateDefinition) — #259.
- Internalized processor runtime (registration, in-process isolation,
  not-registered rejection mirroring `NO_COMPUTE_MEMBER_FOR_TAG`) — #260,
  #252.
- Internalized processor OpenAPI subtype, enum extension, mapping
  extension — #260.
- Import-time validator tightening — #255.
- `ProcessorConfig.Context` / `RetryPolicy` wiring — #253, #254, #262.
- `WorkflowStore.CompareAndSave` SPI addition — #35 (gates on
  cyoda-go-cassandra#22).
- Vendored upstream OpenAPI copies under `docs/cyoda/`.

---

## Appendix A: File-level impact summary

| File | Change |
|---|---|
| `api/openapi.yaml` | Rewrite `ProcessorDefinitionDto.type` enum to `[externalized]` + explicit `discriminator.mapping` with `externalized` only; remove `ScheduledTransitionProcessorDefinitionDto`, `ScheduledTransitionConfigDto`; collapse `processors[]` oneOf to single-member. |
| `api/generated.go` | Regenerated from the above. |
| `cyoda-go-spi/types.go` (sibling repo) | Docstring on `ProcessorDefinition.Type` only. Comment-only change; no SPI tag forced. |
| `internal/domain/workflow/validate.go` (or sibling) | Add `ProcessorTypeExternalized` and `ProcessorTypeInternalized` untyped string constants alongside the existing `ExecutionMode*` block. |
| `internal/domain/workflow/engine_processors.go` | Add early-return Type-axis check at the top of the per-processor loop (between lines 59 and 60). The internalized branch emits the per-processor `SMEventStateProcessResult` audit event explicitly (mirroring lines 104-106), calls `rollbackOpenSegmentOnFailure`, and returns `fmt.Errorf("processor %s failed: execution type %q is not yet implemented", proc.Name, proc.Type)`. The existing ExecutionMode switch + audit emit + abort gate (lines 63-115) are unchanged. |
| `internal/domain/workflow/engine_processors_test.go` (or sibling) | TDD tests per §6.1 items 1–7. Test 1 expanded to a four-case ExecutionMode matrix to catch the F1 silent-success defect. |
| `internal/domain/workflow/*_test.go` (broad sweep) | Normalise stale `Type: "EXTERNAL"`/`"INTERNAL"`/`"external"` fixture strings to `workflow.ProcessorTypeExternalized` or drop where irrelevant. Concrete grep in §5.8. |
| `internal/e2e/` | E2E tests per §6.1 items 8–9. Item 8 includes a five-step manual-transition fixture (import, create entity, fire manual transition, assert 400, re-GET to verify source-state preservation). |
| `cmd/cyoda/help/content/workflows.md` | Lines 63, 130, 135–139 rewritten per §5.6. |
| `cmd/cyoda/help/content/grpc.md` | Line 219 — replace `EXTERNAL` with `externalized`. |
| `docs/PROCESSOR_EXECUTION_MODES.md` | **New file.** Copy drafted v0.8.0 content from parent worktree's working copy verbatim; add §0 axis-summary per §5.8. |
| `e2e/externalapi/scenarios/08-workflow-import-export.yaml` | Strip scheduled entry from `wf-import/03`; retitle test; add comment. (Only scenario with a `"type":"scheduled"` payload — sweep confirmed.) |
| `README.md` | Grep — no current matches; re-verify as a hygiene step. |
| `CLAUDE.md` | Grep — no current matches; re-verify as a hygiene step. |

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
