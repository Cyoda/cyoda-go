# Workflow `annotations` — client-owned metadata on workflows, states, and transitions

**Date:** 2026-06-22
**Status:** Approved (brainstorm) → ready for implementation plan
**Branch / worktree:** `feat/workflow-meta-field` (based on `release/v0.8.0`)

## Problem

Client installations need to attach arbitrary, installation-specific metadata to
workflow configuration — for example a list of roles permitted to fire a
transition, display labels, UI grouping hints, or icons. Today the workflow
import handler uses a strict decoder (`DisallowUnknownFields`), so any extra key
is rejected with `400 BAD_REQUEST`. There is no sanctioned place to put
client-owned data.

The data is **owned and interpreted exclusively by the client application**. The
Cyoda engine must store it and round-trip it (canonical/compacted form), and
must never read, interpret, validate the contents of, or branch on it.

## Goals

- Add an optional `annotations` field to **workflows**, **states**, and
  **transitions**.
- The field holds an arbitrary JSON **object**, opaque to the engine.
- Canonical round-trip: import → export returns the annotations with structure,
  values, and **key order preserved**. Insignificant whitespace is normalised
  (the value is compacted on ingest; the engine compacts all JSON on output, so
  stored == exported). Absent annotations are omitted on export.
- Defensive validation at the import boundary: object-only, with a per-field
  size cap measured on the **compacted** bytes.

## Non-goals

- The engine interpreting annotations in any way (no role enforcement, no label
  rendering — that is the client's job).
- Annotations on processors, criteria, or processor config. Only the three types
  named above. (Can be added later under the same pattern if a need arises.)
- Adding annotations to the audit-log digest (see Out of scope).
- A configurable (env-var) size cap. A fixed constant is used.
- A schema-version bump (see Schema versioning).

## Naming

The field is named **`annotations`**, following the established
industry convention (Kubernetes and others) for *arbitrary, platform-stored,
never-interpreted metadata that tools and clients retrieve*. This deliberately
avoids `meta` / `metadata`, which collide conceptually with the engine's own
`_meta` managed-metadata convention used inside entity payloads (e.g.
`_meta.state`). `annotations` signals "client-owned, engine-opaque".

## Design

### 1. Data model — `cyoda-go-spi`

The workflow types live in the external `cyoda-go-spi` package
(`types.go`), not in cyoda-go. Add one field to each of the three structs,
opaque, mirroring the existing `Criterion json.RawMessage` precedent:

```go
// WorkflowDefinition
Annotations json.RawMessage `json:"annotations,omitempty"`

// StateDefinition
Annotations json.RawMessage `json:"annotations,omitempty"`

// TransitionDefinition
Annotations json.RawMessage `json:"annotations,omitempty"`
```

`json.RawMessage` gives opaque storage of the JSON value. The encoding/json
decoder guarantees the raw value is *syntactically valid JSON* when it populates
a `RawMessage` field, so downstream validation only has to check the JSON *kind*
(object), canonicalise (compact), and check size. `omitempty` omits a nil/empty
value on export, giving a clean round-trip.

**On "byte-preserving".** `json.Marshal` *compacts* a `json.RawMessage` on
output — insignificant whitespace is stripped (verified, Go 1.26). What is
preserved is structure, values, number representation, and **key order**; what is
not is whitespace. To make stored == exported (and to make the size cap measure
what is actually retained), the validation pass **compacts each `annotations`
value on ingest** (`json.Compact`) and stores the compacted bytes. The round-trip
is therefore *canonical*, not byte-identical to a whitespace-padded input. Tests
must compare against the compacted form (or unmarshal both sides to `any` and
`reflect.DeepEqual`), never against a padded literal.

### 2. Cross-repo coordination (mid-milestone window)

We are mid-v0.8.0-milestone: cyoda-go pseudo-version-pins to `cyoda-go-spi`
`main` HEAD across all four `go.mod` files, and the SPI is not yet tagged. So:

1. Land the three SPI fields on `cyoda-go-spi` `main` (its own PR / commit).
2. In the cyoda-go PR, bump the SPI pseudo-version pin to the new SPI `main`
   HEAD in **all four** `go.mod` files (root + `plugins/memory|postgres|sqlite`).

No SPI tag is created — consistent with the milestone window. Local development
composes against the local `../cyoda-go-spi` checkout via `go.work`
(skip-worktree), never a committed `replace` directive.

### 3. Validation — `internal/domain/workflow/validate.go`

**Decoder recognition.** The strict decoder rejects unknown fields. The
workflow level is decoded through the mirror struct `workflowImportDef`
(`handler.go`), which *replaces* the SPI workflow struct for decoding (it
overrides `Active` as `*bool`), so `annotations` must be added there. States and
transitions, however, decode **directly through the SPI types** (`States
map[string]spi.StateDefinition`) — adding `Annotations` to the SPI
`StateDefinition` / `TransitionDefinition` structs is therefore both necessary
and sufficient for the strict decoder to accept them at those two levels. No
state/transition mirror struct exists or is needed.

A new validation-and-normalisation pass runs over the workflow's `annotations`,
every state's `annotations`, and every transition's `annotations`. For each
present value, in order:

1. **Null / empty → nil.** A JSON `null` (the decoder populates the literal bytes
   `null`, non-nil) or an empty value is normalised to nil — treated as *no
   annotations*, omitted on export. This normalisation is required: without it a
   literal `null` would re-marshal as `"annotations":null` instead of being
   omitted.
2. **Kind: object-only.** The value must be a JSON object (`{ ... }`). Because the
   decoder already guaranteed valid JSON, checking the first non-whitespace byte
   is `{` is sufficient. Any other kind — array, string, number, boolean — is
   rejected with **`400 VALIDATION_FAILED`**, the message naming the offending
   location (workflow / state `<name>` / transition `<name>`).
3. **Compact.** `json.Compact` the value into a buffer; store the compacted bytes
   back onto the field. This makes stored == exported and makes the size check
   measure the retained size, closing the whitespace-padding vector.
4. **Size: per-field cap.** The **compacted** length is capped at
   **`maxAnnotationsBytes = 64 * 1024`** (64 KB). Over → `400 VALIDATION_FAILED`
   naming the location. The aggregate is already bounded by the existing 10 MB
   request-body cap (`handler.go` `http.MaxBytesReader`); this per-field cap is a
   tighter guard against a single bloated blob.

A small helper performs steps 1–3 and returns the canonical bytes (or nil); the
caller applies the size check and writes the result back.

### 4. Merge / import modes

`annotations` rides along with its owning workflow/state/transition definition.
The REPLACE / MERGE / ACTIVATE import-mode logic operates on whole definitions,
so no annotations-specific merge handling is required. The
validation-and-normalisation pass runs on the **incoming** request only (kind +
compact + size), before mode logic — consistent with the existing structural
validators, which `validate.go` documents as running on the incoming request and
*not* retroactively on already-stored workflows. So a stored workflow that is not
re-submitted under MERGE is not re-validated. This is the established
non-retroactive policy (H4/H6); it is stated here explicitly. Practical risk is
nil regardless, since `annotations` is new in this release — no pre-existing
stored annotations exist.

### 5. API surface — `api/openapi.yaml`

Add an opaque object property to the three DTOs:

- `WorkflowConfigurationDto`
- `StateDefinitionDto`
- `TransitionDefinitionDto`

```yaml
annotations:
  type: object
  additionalProperties: true
  description: >
    Arbitrary client-owned metadata, stored and round-tripped (compacted) and
    never interpreted by the engine. Must be a JSON object; capped at 64 KB
    per field. Use for client concerns such as permitted roles, display
    labels, or UI hints.
```

`annotations` is **optional** — it is added only as a property and is **never**
added to any `required` list on the three DTOs, consistent with the `omitempty`
struct tags and the "absent/null → no-annotations" normalisation.

These DTO additions are **load-bearing, not just documentation**: the OpenAPI
response validator runs in `ModeEnforce` and validates every E2E *response*
against the spec. An export carrying `annotations` against a DTO that does not
declare the property would fail conformance. (Request bodies are not
OpenAPI-validated — the request-side gate is purely the Go strict decoder.)
`TestOpenAPIWorkflowVersionContract` and related OpenAPI tests must stay green.

### 6. Help content — `cmd/cyoda/help/content/workflows.md`

Document `annotations` on the WorkflowDefinition, StateDefinition, and
TransitionDefinition field lists, and in at least one example. Stress that it is
client-interpreted and engine-opaque, object-only, 64 KB per-field cap, and
round-tripped (compacted). Also extend the existing **"Export field omission"**
note so it records that `annotations` is omitted on export when absent.

### 7. Schema versioning — no bump

Schema `1.1` is the v0.8.0 contract and v0.8.0 is **unreleased**, so
`annotations` folds into 1.1 rather than bumping to 1.2. `CurrentSchemaVersion`
and `SupportedSchemaRanges` are unchanged. Update:

- `docs/workflow-schema-versioning.md` — extend the `1.1 — v0.8.0 contract`
  changelog entry to list `annotations` as part of the contract.
- Root `CHANGELOG.md` — add a feature entry under the v0.8.0 / `[Unreleased]`
  section.

(If 1.1 had already shipped, this additive optional field would be a MINOR bump
to 1.2 per the versioning doc. It does not, so it is not.)

### 8. Out of scope / explicitly decided

- **Audit digest unchanged.** `annotations` is not added to the workflow audit
  digest (`{name, desc}`); the full workflow — including annotations — is already
  persisted, so the digest stays minimal.
- **No env-var knob.** The 64 KB cap is a named constant, matching the existing
  `maxEntityBodySize`-style constants.
- **Storage unchanged.** Workflows persist as whole JSON blobs (no column
  decomposition), so no migration is needed; annotations ride inside the blob.
- **Tenant isolation.** Annotations ride inside the workflow blob, stored under
  the same tenant-keyed path as the rest of the workflow — no new data path, no
  new isolation surface. Confirmed, no change to isolation handling.
- **Deep-nesting DoS — acknowledged, cap is the bound.** A 64 KB object can
  encode deep nesting (~tens of thousands of levels), and Go's `encoding/json`
  recurses on the stack. This is a pre-existing exposure on the existing
  `criterion` `RawMessage`; annotations widen it by three object-typed fields.
  The decision is to **accept it** — the strict decoder already fully parses the
  value (so any blow-up happens regardless of our validator), the 64 KB per-field
  cap plus 10 MB body cap bound the depth to a survivable range, and a bespoke
  depth guard would be gold-plating inconsistent with how `criterion` is handled.
  Recorded as an explicit decision, not silence.

## Testing (TDD — RED first)

  **Test-assertion caution (applies to all round-trip tests below):** never
  assert raw-byte equality against a whitespace-padded input — export is
  compacted. Assert against the **compacted** form, or unmarshal both sides to
  `any` and `reflect.DeepEqual`.
- **SPI** (`cyoda-go-spi`): struct round-trip test — marshal/unmarshal a
  workflow with annotations on all three levels, assert the value survives
  (compacted/DeepEqual) and that absent annotations are omitted on marshal.
- **Unit** (`internal/domain/workflow/validate_test.go`): object accepted and
  compacted (padded input → compacted stored); array / string / number / boolean
  rejected with location in message; `null` and absent treated as no-annotations
  (normalised to nil); at-cap (measured on compacted bytes) accepted, over-cap
  rejected; rejection at each of the three levels.
- **E2E** (`internal/e2e/`): import a workflow with annotations on workflow,
  state, and transition → export → assert canonical (compacted/DeepEqual)
  round-trip; import a no-annotations workflow → export → assert **no
  `annotations` key present** (guards the null/empty normalisation); import an
  oversized / non-object annotations → assert `400 VALIDATION_FAILED`.
- **Parity** (`e2e/parity/`): an annotations round-trip parity test so every
  storage backend — including the commercial Cassandra backend, which picks up
  the parity registry — inherits coverage.
- **Existing gates:** `TestDefaultWorkflowFixtureSchemaVersion`,
  `TestOpenAPIWorkflowVersionContract`, and the workflow schema-version tests
  stay green (no version change). Update fixtures only if a fixture is extended
  to exercise annotations.

## Files touched (anticipated)

**cyoda-go-spi:** `types.go` (+ a round-trip test; check `spitest/workflow.go`).

**cyoda-go:**
- `go.mod` ×4 (SPI pin bump)
- `internal/domain/workflow/handler.go` (mirror struct field; normalisation)
- `internal/domain/workflow/validate.go` (+ `validate_test.go`)
- `api/openapi.yaml`
- `cmd/cyoda/help/content/workflows.md`
- `docs/workflow-schema-versioning.md`
- `CHANGELOG.md`
- `internal/e2e/` (round-trip + rejection tests)
- `e2e/parity/` (round-trip parity test)

## Open questions

None blocking. Implementation proceeds via TDD per the plan.
