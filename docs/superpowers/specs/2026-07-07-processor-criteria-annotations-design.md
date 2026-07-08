# Renderer annotations on processors & criteria

**Date:** 2026-07-07
**Milestone:** v0.8.2
**Issue:** #384
**Status:** Design approved; spec under review

## Problem

Workflow, state, and transition definitions each carry an opaque, engine-ignored
`annotations` bag (client-owned metadata, canonicalised at import to a JSON object
≤ 64 KB, round-tripped verbatim). Processors and criteria — the two remaining
workflow elements — have no such field. Renderers (workflow visualisers,
condition builders) therefore cannot attach a human-readable label or description
to a processor or a guard condition.

This design extends the existing annotations mechanism to processors and criteria,
and documents a small set of well-known optional keys (`displayName`, `description`)
useful to renderers, uniformly across all five element types.

## Non-goals

- The engine does **not** interpret annotation contents (unchanged policy). The
  well-known keys are a documented convention for renderers, not engine behaviour.
- No **documented/validated** labelling of individual nested sub-conditions inside a
  criterion. Annotations attach to the criterion **as a whole** (the guard) via the
  sibling field. Leaf-level labelling is YAGNI. Note this is a convention choice, not
  a technical limit: because the criterion round-trips verbatim and `ParseCondition`
  tolerates unknown keys, a client can already stash keys inside condition nodes today
  — engine-ignored and preserved. If nested labelling is ever wanted it needs no schema
  change, which further supports the sibling decision.
- Annotations are not delivered to external compute nodes over gRPC (see §4).

## Design

### 1. One uniform bag, two placements

A single "annotations bag" shape across all five element types. Well-known optional
keys are `displayName` (string) and `description` (string). The bag stays **open** —
modelled typed-but-open in OpenAPI (properties enumerated, never
`additionalProperties: false`), so clients may carry additional keys. The engine
never reads any of them.

| Element | Field | Placement | Status |
|---|---|---|---|
| Workflow | `annotations` | embedded | exists |
| State | `annotations` | embedded | exists |
| Transition | `annotations` | embedded | exists |
| Processor | `annotations` | embedded (new SPI field) | **new** |
| Criterion (workflow + transition guard) | `criterionAnnotations` | sibling to `criterion` | **new** |

**Why a sibling for criteria.** A criterion is an opaque, polymorphic, recursively
nested JSON value (`SimpleCondition` / `GroupCondition` / `FunctionCondition` /
`ArrayCondition`), stored and exported **verbatim** (`json.RawMessage`, never
re-marshalled; only `version` is restamped on export). Embedding annotations inside
it would force the import validator to parse, walk, and risk re-serialising the
criterion — breaking the verbatim invariant. A sibling field on the structs we own
(`WorkflowDefinition`, `TransitionDefinition`) avoids all of that: the criterion
blob stays 100 % untouched, and the sibling carries the same bag shape as the other
four, so the design remains uniform in shape.

Only workflow and transition own a criterion, so only they gain `criterionAnnotations`.
States and processors have no criterion.

Wire shape at a transition:

```jsonc
{
  "name": "APPROVE",
  "next": "approved",
  "criterion": { /* opaque condition tree — untouched, verbatim */ },
  "criterionAnnotations": {
    "displayName": "Approved & funded",
    "description": "Amount within limit and manager sign-off"
  },
  "processors": [
    { "name": "notify", "type": "externalized",
      "annotations": { "displayName": "Send approval email" } }
  ],
  "annotations": { "displayName": "Approve request" }   // the transition itself
}
```

### 2. Validation — reuse the existing walk

Reuse `canonicalizeAnnotations` unchanged (object-only, ≤ 64 KB compacted; `null`,
blank, or absent → nil). Extend `validateAndNormalizeAnnotations`
(`internal/domain/workflow/validate.go`) to also canonicalise, on the incoming
import request only (non-retroactive, matching the existing policy):

- `workflow.CriterionAnnotations`
- each `transition.CriterionAnnotations`
- each `processor.Annotations` (looping processors within each transition)

The criterion JSON itself is never parsed by the validator. Violations surface via
the existing path: a plain error wrapped by the handler as **400
`VALIDATION_FAILED`**. No new error code is introduced.

**Addressability:** processors must be mutated by index within the transition loop
(`p := &tr.Processors[k]; p.Annotations = canon`), never a range-value copy, or the
canonicalised blob is lost. Transition-level `CriterionAnnotations` uses the existing
`tr := &stateDef.Transitions[j]` pointer; the state-map write-back
(`wf.States[stateName] = stateDef`) is retained for state-level annotations. This is
the same non-addressable-map-value trap the current walk already navigates — the
implementer must not reintroduce it.

### 3. Import / export wire behaviour

- **Import** uses a strict decoder (`DisallowUnknownFields`). Today an `annotations`
  key on a processor is *rejected* as an unknown field; after this change it is a
  known field and accepted — a pure loosening (every previously-valid payload stays
  valid). `criterionAnnotations` is a new known field on the workflow and transition
  shapes.
- **Round-trip invariant (corrected):** export already restamps `version` to
  `CurrentSchemaVersion`, so a pre-feature config that exported as `"1.1"` exports as
  `"1.2"` after the bump — the bytes are **not** identical in the version field. The
  genuine invariant is narrower and is what the tests assert: **no new keys are
  emitted when the fields are unset** (both are `omitempty`), i.e. no spurious
  `annotations`/`criterionAnnotations` appear, and the criterion blob is unchanged.
  Round-trip equality tests compare with `version` excluded.
- **Export** serialises both fields with `omitempty`; the criterion is restamped only
  for `version`, as today.
- `workflowImportDef` (the handler's request-shape mirror of `WorkflowDefinition`)
  gains `CriterionAnnotations`, and the request→SPI conversion copies it. Processor
  and state/transition annotations flow through the embedded `spi` types automatically.

### 4. Dispatch — annotations never reach external compute members

Two distinct hops, and the distinction matters (multi-node is the primary target,
per `.claude/rules/multi-node-primary.md`):

- **External gRPC hop (compute member):** the local `DispatchProcessor`
  (`internal/grpc/dispatch.go:83`) builds a **field-selected**
  `EntityProcessorCalculationRequestJson` (processor name, `Config.Context`,
  `Config.AttachEntity` payload) — it does **not** marshal the whole
  `ProcessorDefinition`. So processor `annotations` **never reach an external compute
  member**. The gRPC processor contract is unchanged.
- **Internal peer-forward hop (node-to-node):** `buildProcessorRequest`
  (`internal/cluster/dispatch/cluster_dispatcher.go:235`) sets
  `Processor: processor` — the **whole** `spi.ProcessorDefinition` — into
  `DispatchProcessorRequest`, which `HTTPForwarder.ForwardProcessor` JSON-marshals and
  POSTs to the owning peer. Annotations therefore **do** ride this internal wire
  (bounded by the same 64 KB cap). The receiving peer then field-selects before its
  gRPC hop, so nothing leaks onward.

`criterionAnnotations` ride **neither** wire: `buildCriteriaRequest` forwards only the
bare `criterion` `json.RawMessage`, and transitions are never forwarded whole — so the
transition sibling never leaves the coordinator. No other whole-`ProcessorDefinition`
or whole-criterion serialisation path exists (async-result delivery, cascade
re-dispatch, and audit do not carry the definition).

**Decision:** accept processor annotations on the internal peer-forward payload rather
than special-case-stripping them. It is consistent with `Config.Context`, which is
already forwarded whole (same per-dispatch bandwidth tradeoff — annotations add up to
64 KB per forwarded dispatch, not just at import); the data is client-owned and
tenant-scoped (no cross-tenant exposure); and stripping would add a bespoke code path
for no security benefit. The gRPC-layer test asserts the *external* invariant: an
annotated processor still dispatches, and annotations are absent from the
`EntityProcessorCalculationRequestJson` sent to the compute member.

### 5. Schema version — single v0.8.2 bump

Current `CurrentSchemaVersion = "1.1"` was introduced in v0.8.0; nothing on
`release/v0.8.2` has bumped it. This feature is v0.8.2's one bump:

- **1.1 → 1.2**, additive MINOR (new optional fields — the common case).
- `SupportedSchemaRanges = {Major:1, MinMinor:1, MaxMinor:2}` — **dual-shape**, keep
  1.1. Purely additive, so 1.1 payloads stay fully valid; no retirement.
- Follows `docs/workflow-schema-versioning.md`: bump `CurrentSchemaVersion`, raise
  `MaxMinor`, add the 1.1 → 1.2 changelog entry with rationale, update the
  schema-version help topic and fixtures.
- **`internal/domain/workflow/default_workflow.json` must be bumped to `"1.2"`.** The
  embedded default workflow bypasses the import handler (loaded directly by the
  engine), and `default_workflow_test.go` asserts its `version` equals
  `CurrentSchemaVersion` — so the drift-guard test fails if the fixture is not bumped
  alongside the constant. This fixture does **not** pipe through the import handler, so
  the general "bump fixtures that pipe through the handler" rule does not cover it;
  it is called out explicitly here.

Any later v0.8.2 schema change rides on 1.2 — no second bump.

### 6. SPI change

Add fields in `cyoda-go-spi`:

- `ProcessorDefinition.Annotations json.RawMessage` — `json:"annotations,omitempty"`
- `WorkflowDefinition.CriterionAnnotations json.RawMessage` — `json:"criterionAnnotations,omitempty"`
- `TransitionDefinition.CriterionAnnotations json.RawMessage` — `json:"criterionAnnotations,omitempty"`

All additive (recompile-only; non-breaking for SPI consumers). Per the v0.8.2 plan
the SPI tag is deferred to milestone-end, so cyoda-go pseudo-version-pins to SPI
`main` HEAD across all four `go.mod` files, as done for other v0.8.2 SPI work.
Verify the change does not break the commercial (Cassandra) plugin, which consumes
the same SPI.

## API surface

Only affected endpoints:

- `POST /model/{entityName}/{modelVersion}/workflow/import`
- `GET  /model/{entityName}/{modelVersion}/workflow/export`

No new endpoints. Workflow import/export is HTTP-only (no gRPC import entry point).

### Error / status table — `POST …/workflow/import`

| Scenario | Status | Code |
|---|---|---|
| Valid `annotations` / `criterionAnnotations` | 200 | — |
| Annotations value not a JSON object | 400 | `VALIDATION_FAILED` |
| Annotations > 64 KB compacted | 400 | `VALIDATION_FAILED` |
| Unknown / typo'd field | 400 | `BAD_REQUEST` |
| `version: "1.2"` | 200 | — |
| `version: "1.1"` (no new fields) | 200 | — |
| `version` unsupported (e.g. `"1.3"`, `"2.0"`) | 400 | `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` |

`GET …/workflow/export` → 200, round-trips both fields verbatim; no new error.

**No new error codes → no new `cmd/cyoda/help/content/errors/<CODE>.md`** (the
`TestErrCode_Parity` bijection is unaffected).

## Coverage matrix (scenario × layer)

Layers: unit (`internal/domain/workflow`) · running-backend e2e
(`internal/e2e`, real Postgres) · cross-backend parity (`e2e/parity`,
memory/sqlite/postgres + commercial) · gRPC (`internal/grpc`).

| Scenario | unit | e2e | parity | gRPC |
|---|:--:|:--:|:--:|:--:|
| Round-trip preserves processor `annotations` + `criterionAnnotations` | ✓ | ✓ | ✓ | — (HTTP-only import) |
| Absent → no new keys emitted (compare with `version` excluded) | ✓ | ✓ | — | — |
| Annotations not an object → 400 `VALIDATION_FAILED` | ✓ | ✓ | — | — |
| Annotations > 64 KB → 400 `VALIDATION_FAILED` | ✓ | ✓ | — | — |
| Typo'd annotations-adjacent field (e.g. `annotationss`) → 400 `BAD_REQUEST` | — | ✓ | — | — |
| `1.2` accepted / `1.1` accepted / unsupported rejected | ✓ | ✓ | — | — |
| Criterion blob unchanged by sibling (verbatim) | ✓ | — | — | — |
| Well-known keys pass through (engine ignores `displayName`/`description`) | ✓ | — | — | — |
| Annotated processor still dispatches; annotations **not** in external `EntityProcessorCalculationRequestJson` | ✓ | — | — | ✓ |

The typo'd-field row exercises pre-existing strict-decoder behaviour (not new to this
feature) but is cheap to assert on the new field names, so it is included rather than
waived. Parity scenario registered in `e2e/parity/registry.go`. No concurrency/race
scenario is required (no new shared-state mutation path). Cross-backend behaviour is
the round-trip fidelity, covered by the parity row.

## Documentation (Gate 4 + Gate 7)

- `docs/workflow-schema-versioning.md` — 1.1 → 1.2 changelog entry (additive,
  dual-shape retention, rationale).
- `CHANGELOG.md` — v0.8.2 / `[Unreleased]` entry naming the bump and the new fields.
- `cmd/cyoda/help/content/workflows/schema-version.md` — supported set / example
  version.
- `cmd/cyoda/help/content/workflows.md` — document annotations and the well-known
  keys (`displayName`, `description`) across all five element types; note the bag is
  open and engine-ignored.
- `api/openapi.yaml` (+ regenerate `api/generated.go`, embedded-spec:false). Today the
  existing three annotations fields are modelled inline as `type: object,
  additionalProperties: true` with no enumerated properties (`StateDefinitionDto`,
  `TransitionDefinitionDto`, `WorkflowConfigurationDto`). **Decision (honoring "all
  five uniform"):** introduce one shared schema (e.g. `WorkflowElementAnnotations`)
  that enumerates `displayName` + `description` as optional strings and keeps the bag
  open (`additionalProperties: true` — never `false`), and reference it from **all
  five** occurrences: the existing three, plus new `annotations` on
  `ProcessorDefinitionDto` (base — `ExternalizedProcessorDefinitionDto` inherits via
  `allOf`), plus new `criterionAnnotations` on `WorkflowConfigurationDto` and
  `TransitionDefinitionDto`. This changes the generated Go field type of the existing
  three from `*map[string]interface{}` to `*WorkflowElementAnnotations` (additive for
  consumers; typed-but-open preserved). **Verify at plan time** that no runtime code
  consumes those generated DTO annotation fields (handlers parse via `workflowImportDef`
  + `spi` types, not the generated DTOs; there is **no production request-side OpenAPI
  validation** — kin-openapi is used only in doc-serving and in the test-only
  `internal/e2e/openapivalidator` response validator), so the generated-Go field-type
  change is production-inert. **The well-known-key types are advisory, not enforced:**
  the server validates annotations only for object-shape + ≤64 KB (`canonicalizeAnnotations`),
  never the value types, consistent with "the engine never interprets contents." So a
  client *could* import `{"displayName": 123}` and it would be stored/exported as-is.
  Enumerate `displayName`/`description` as `type: string` for renderer/codegen benefit,
  but add a schema `description` noting the types are a documented convention the engine
  does not enforce — do not describe them as strictly validated. (One consequence: the
  E2E response validator would reject a *non-string* well-known value on export, so our
  own fixtures use string values; clients that store non-string values own that risk.)
  Update the `WorkflowConfigurationDto.version` example if it names a version string.
- `COMPATIBILITY.md` — SPI pin bump row.
- `docs/cloud-parity/` — one file for this contract extension (Cloud mirrors the
  import/export shape and the schema version).

## Files touched (summary)

**cyoda-go-spi:** `types.go` (three field additions), matching tests.

**cyoda-go:**
- `internal/domain/workflow/handler.go` — `workflowImportDef.CriterionAnnotations`
  + conversion.
- `internal/domain/workflow/validate.go` — extend `validateAndNormalizeAnnotations`
  (processors, workflow + transition `criterionAnnotations`).
- `internal/domain/workflow/schemaversion.go` — `CurrentSchemaVersion = "1.2"`,
  `MaxMinor: 2`.
- `internal/domain/workflow/default_workflow.json` — `version` `"1.1"` → `"1.2"`
  (drift-guard test; see §5).
- `api/openapi.yaml`, `api/generated.go` — DTO fields + shared annotations schema.
- `go.mod` ×4 — SPI pseudo-version pin bump.
- Tests: workflow unit, `internal/e2e`, `e2e/parity` (+ `registry.go`),
  `internal/grpc`.
- Docs per §Documentation.

**Schema-version literal discipline (corrected — the naive rule is backwards).**
Because the bump is **dual-shape (1.1 retained)**, the large body of `"1.1"` *import*
fixtures across `internal/e2e`, `e2e/parity`, `plugins/*/workflow_store_test.go`, and
`test/recon` **must stay `"1.1"`** — they remain valid and now double as proof of
dual-shape retention. Do **not** bulk-rewrite import fixtures to `"1.2"`. What must
change are assertions of the **current / exported / discovered** version:
- `internal/e2e/workflow_schema_version_test.go` — the export-stamp assertion (expects
  exported `version == "1.1"`, line ~169) → `"1.2"`; the discovery assertion
  (`current == "1.1"`, `supported[0] == {major:1,minMinor:1,maxMinor:1}`, lines ~192/199)
  → `"1.2"` and `{1,1,2}` (list length stays 1 — dual-shape is a single widened range).
  Refresh the now-stale "v0.8.0 ships 1.1" comment.
- `internal/domain/workflow/default_workflow.json` → `"1.2"` (drift guard, §5).
- Symbolic `CurrentSchemaVersion` tests (`export_schema_version_test.go`,
  `schemaversion_test.go`, `schemaversion_validate_test.go`) auto-follow the constant.
- Add a **new** explicit "`1.2` accepted on import" test rather than repurposing the
  existing `1.1`-import test (which stays as the dual-shape retention proof).

## Test plan (TDD)

RED → GREEN per element:

1. Schema-version: 1.2 accepted, 1.1 accepted, 1.3 rejected (unit + e2e). Bump
   `default_workflow.json` and keep `default_workflow_test.go` green.
2. Processor `annotations`: accept valid; reject non-object; reject > 64 KB;
   round-trip preserved; absent → no new keys emitted, `version` excluded from the
   comparison (unit + e2e).
3. `criterionAnnotations` on workflow + transition: same set; assert criterion blob
   is byte-unchanged (unit + e2e).
4. Cross-backend parity round-trip; register in `registry.go`.
5. gRPC: annotated processor dispatches; annotations absent from the external
   `EntityProcessorCalculationRequestJson`. (The internal peer-forward payload
   carrying them whole is the accepted behaviour per §4 — not asserted against.)
6. Verification: `go test ./... -v` (incl. e2e), `go vet ./...`, per-plugin test
   runs, `make race` once before PR.
