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
Cyoda engine must store it and round-trip it verbatim, and must never read,
interpret, validate the contents of, or branch on it.

## Goals

- Add an optional `annotations` field to **workflows**, **states**, and
  **transitions**.
- The field holds an arbitrary JSON **object**, opaque to the engine.
- Byte-preserving round-trip: import → export returns the annotations unchanged;
  absent annotations are omitted on export.
- Defensive validation at the import boundary: object-only, with a per-field
  size cap.

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
opaque and byte-preserving, mirroring the existing `Criterion json.RawMessage`
precedent:

```go
// WorkflowDefinition
Annotations json.RawMessage `json:"annotations,omitempty"`

// StateDefinition
Annotations json.RawMessage `json:"annotations,omitempty"`

// TransitionDefinition
Annotations json.RawMessage `json:"annotations,omitempty"`
```

`json.RawMessage` gives opaque, byte-preserving storage. The encoding/json
decoder still guarantees the raw value is *syntactically valid JSON* when it
populates a `RawMessage` field, so downstream validation only has to check the
JSON *kind* (object) and size. `omitempty` omits a nil/empty value on export,
giving a clean round-trip.

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

The import handler's strict decoder needs the field on its mirror struct
`workflowImportDef` (`handler.go`) too, so `annotations` is recognised rather
than rejected as unknown.

A new validation pass runs over the workflow's `annotations`, every state's
`annotations`, and every transition's `annotations`:

- **Kind: object-only.** Each present `annotations` value must be a JSON object
  (`{ ... }`). A JSON `null` (or an absent field) is treated as *no annotations*
  and normalised to nil so it is omitted on export. Any other JSON kind — array,
  string, number, boolean — is rejected with **`400 VALIDATION_FAILED`**, with a
  message naming the offending location (workflow / state `<name>` / transition
  `<name>`).
- **Size: per-field cap.** Each individual present `annotations` raw value is
  capped at **`maxAnnotationsBytes = 64 * 1024`** (64 KB), measured on the stored
  raw bytes. Over the cap → `400 VALIDATION_FAILED` naming the location. The
  aggregate is already bounded by the existing 10 MB request-body cap
  (`handler.go` `http.MaxBytesReader`); the per-field cap is a tighter guard
  against a single bloated blob.

Kind detection: because the value is guaranteed valid JSON, checking the first
non-whitespace byte is `{` is sufficient to distinguish an object; `null` is
detected and normalised to nil. A small helper performs trim + first-byte check.

Normalisation (null/empty → nil) happens before persistence so that export
omits it cleanly.

### 4. Merge / import modes

`annotations` rides along with its owning workflow/state/transition definition.
The REPLACE / MERGE / ACTIVATE import-mode logic operates on whole definitions,
so no annotations-specific merge handling is required. Validation runs on the
post-decode request (kind + size) before mode logic, consistent with existing
structural validation ordering.

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
    Arbitrary client-owned metadata, stored and round-tripped verbatim and
    never interpreted by the engine. Must be a JSON object; capped at 64 KB
    per field. Use for client concerns such as permitted roles, display
    labels, or UI hints.
```

`TestOpenAPIWorkflowVersionContract` and related OpenAPI tests must stay green.

### 6. Help content — `cmd/cyoda/help/content/workflows.md`

Document `annotations` on the WorkflowDefinition, StateDefinition, and
TransitionDefinition field lists, and in at least one example. Stress that it is
client-interpreted and engine-opaque, object-only, 64 KB per-field cap, and
round-tripped verbatim.

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

## Testing (TDD — RED first)

- **SPI** (`cyoda-go-spi`): struct round-trip test — marshal/unmarshal a
  workflow with annotations on all three levels, assert byte-preservation and
  that absent annotations are omitted.
- **Unit** (`internal/domain/workflow/validate_test.go`): object accepted;
  array / string / number / boolean rejected with location in message; `null`
  and absent treated as no-annotations (normalised to nil); at-cap accepted,
  over-cap rejected; rejection at each of the three levels.
- **E2E** (`internal/e2e/`): import a workflow with annotations on workflow,
  state, and transition → export → assert verbatim round-trip; import an
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
