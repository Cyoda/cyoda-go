# Workflow Schema Versioning

This document tracks the wire-format contract for `WorkflowConfigurationDto.version` — the semver `MAJOR.MINOR` string each workflow declares on import and the server stamps on export.

See the in-product help topic for the user-facing reference:

- CLI: `cyoda help workflows schema-version`
- HTTP: `GET /help/workflows/schema-version`
- JSON discovery: `GET /help/workflows/schema-version/versions`

## Bump rules

- **MAJOR**: a payload valid under the previous MAJOR is no longer valid, or vice-versa. Examples: removing a field, renaming a field, making an optional field required, changing semantics of an existing field.
- **MINOR**: additive, backward-compatible changes. Examples: new optional field, new enum value in an existing string-enum, new optional sub-object, new condition operator. **This is the common case.**

Both bumps must:

1. Update `CurrentSchemaVersion` in `internal/domain/workflow/schemaversion.go`.
2. Extend or amend the appropriate `SchemaRange` in the same file (raise `MaxMinor` for a MINOR; append a new range for a MAJOR).
3. Add an entry below describing the change and citing the PR.
4. Update `cmd/cyoda/help/content/workflows/schema-version.md` if the description of the contract changes.
5. Update `cmd/cyoda/help/content/workflows.md`, `cmd/cyoda/help/content/errors/WORKFLOW_SCHEMA_VERSION_UNSUPPORTED.md`, and the `WorkflowConfigurationDto.version` description in `api/openapi.yaml` if the example version string changes.
6. Bump `internal/domain/workflow/default_workflow.json` — `TestDefaultWorkflowFixtureSchemaVersion` asserts it matches `CurrentSchemaVersion`.
7. Update every workflow-typed test fixture under `internal/e2e/`, `internal/domain/workflow/`, `plugins/{memory,postgres}/workflow_store_test.go`, `test/recon/scenarios_*.go`, and `e2e/parity/` to use the new version string. The schema-version gate runs at the import-handler boundary; tests that pipe through the handler will reject stale literals with `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` even if their nominal subject is something else.

## Tightening releases — when MINOR bumps require retiring the previous MINOR

Strict MAJOR/MINOR semantics map cleanly when changes are purely additive: a new optional field, a new enum value, a new operator. The hard case is a **tightening release** — one where the rules accept a strict subset of what the previous MINOR accepted (typos rejected, unknown-field-strict decoder, empty-array-in-REPLACE rejected, stricter enum, length cap, etc.). v0.8.0 v.s. v0.7.x's 1.0 contract was this case: every shape v0.8.0 accepts is byte-identical to a shape 1.0 accepted, but v0.8.0 rejects strictly more inputs.

The decision rubric:

1. **If the new restrictions are essentially "fail-loud where 1.0 was silently a no-op"** — e.g., typo'd field names, unknown enums, empty arrays in REPLACE/ACTIVATE — call it MINOR. Calling it MAJOR would invite repeated MAJOR bumps every release we close another silent-no-op gap, devaluing the MAJOR signal.

2. **Decide separately whether to retain the previous MINOR in `SupportedSchemaRanges`.** Two options:

   - **Dual-shape acceptance** (`MinMinor: N, MaxMinor: N+1`). Old clients keep working. The server interprets payloads stamped with the older MINOR using the strict-superset interpretation — i.e., it still applies the new restrictions. Old shape, new strictness. **Choose this only if you can document the failure mode of old payloads under new strictness, and the impact is acceptable.**
   - **Retirement** (`MinMinor: N+1, MaxMinor: N+1`). Old clients sending the previous MINOR get `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (400) with the new MINOR named in the error. The diagnosis is explicit rather than degrading to a confusing `VALIDATION_FAILED` on whichever stricter rule the workflow first hits. **Choose this when the tightening surface is broad enough that a v.N payload "happens to import" only by coincidence — clients must regenerate against v.(N+1) to be safe.**

3. **A genuinely breaking shape change** — field removed, field renamed, optional → required, semantics changed — is always MAJOR. Add a new `SchemaRange` rather than amending the old one. A deprecation window (both MAJORs concurrently in `SupportedSchemaRanges`) is fine if you want one; sunset the old MAJOR by dropping its range.

Document the choice (dual-shape vs retirement, with rationale) in the per-version changelog entry below. The 1.0 → 1.1 entry is the worked example.

## When NOT to bump

- Adding a new HTTP endpoint or response field that's outside `WorkflowConfigurationDto`. The version contract scopes the import DTO, not the wider workflow API.
- Bug-fixing a validator that was already supposed to reject something — i.e., the rejection was always documented and the validator was the bug. Add a test, ship the fix; no schema bump. (Borderline cases: if a validator was widely-relied-upon-via-its-absence, treat as a tightening release per §above.)
- Internal refactoring of the engine, store, or audit shape. The wire contract is unchanged.

### Malformed-regex criteria rejected at import (v0.8.3)

A workflow-level or transition-level criterion whose `MATCHES_PATTERN`
operator carries a regex that fails to compile is now rejected at import
(`400 VALIDATION_FAILED`), instead of importing successfully and only
erroring the first time the transition is evaluated. This is the §"When NOT
to bump" "bug-fixing a validator that was already supposed to reject
something" case: a malformed regex never worked — every evaluation attempt
already failed — so no *working* config is newly rejected, only the failure
moves from eval time to import time. No `CurrentSchemaVersion` or
`SupportedSchemaRanges` change.

### Model export surface — `uniqueKeys` field (v0.8.2)

`GET /model/export/…` responses carry a top-level `uniqueKeys` array listing the model's declared composite unique keys. The field uses **omitempty** semantics — it is present only when the model declares at least one key; a model with no keys exports byte-identically to a pre-feature model (matching the descriptor storage DTOs, which also omit the empty case). This is a purely additive change to the **model export DTO** (`ExportModel`) — it is separate from `WorkflowConfigurationDto` and therefore does **not** trigger a workflow schema version bump per the rule above. No `CurrentSchemaVersion` change is required.

## Required commit-/PR-time checks

Before merging a schema bump:

- `go test ./internal/domain/workflow/...` green, including `TestCurrentSchemaVersionIsSupported`, `TestOpenAPIWorkflowVersionContract`, `TestDefaultWorkflowFixtureSchemaVersion`, `TestValidateSchemaVersions_*`.
- `go test ./internal/e2e/...` green, including `TestWorkflowSchemaVersion_*` (happy path, retired-MINOR rejection, malformed rejection, export-stamping, discovery).
- `cyoda help workflows schema-version versions` returns the expected `{current, supported[]}` JSON. The CLI is the in-product documentation channel; if it drifts from the constants, customers will hit it.
- The CHANGELOG entry under `[Unreleased]` (or the current milestone's section) names the bump, the rationale, and any retired MINORs by version string.

## Changelog

### 1.3 — v0.8.3 contract (current)

Additive MINOR — one new optional field, mutually exclusive with an existing
one, both still under the same parent object:

- **`function` on `TransitionScheduleDto`** (the `schedule` object) — a
  compute-node Function callout that computes a scheduled transition's
  firing time (and optional expiry) per entity, as an alternative to the
  existing static `delayMs`. Mutually exclusive with `delayMs` (exactly one
  of the two is required); this exclusivity is enforced by the import
  validator, not expressible in the OpenAPI schema itself, matching the
  existing `manual`/`schedule` mutual-exclusion precedent.

Every 1.2 payload — including every existing `schedule: {delayMs, timeoutMs}`
transition — is byte-identical and remains valid; `function` is purely
additive alongside `delayMs`, not a replacement for it. See
[`docs/cloud-parity/scheduled-transitions.md`](./cloud-parity/scheduled-transitions.md)
§9 for the full `function`/`Schedule`-result contract.

**Dual-shape retention of 1.1 and 1.2.** Nothing is retired:
`SupportedSchemaRanges` widens in place to
`{Major: 1, MinMinor: 1, MaxMinor: 3}` — 1.1- and 1.2-stamped imports keep
working alongside 1.3.

### 1.2 — v0.8.2 contract

Additive MINOR — two new optional fields, both reusing the existing `annotations` validation (client-owned JSON object, ≤ 64 KB compacted, engine-ignored):

- **`annotations` on `ProcessorDefinition`** — same shape as the existing workflow/state/transition `annotations`.
- **`criterionAnnotations` on `WorkflowDefinition` and `TransitionDefinition`** — a sibling field next to `criterion`, not embedded in it, so the criterion blob keeps round-tripping byte-verbatim.

Both fields document two well-known optional keys for renderers, `displayName` and `description` (strings) — the engine never interprets them, and the key names/types are an advisory convention, not enforced beyond the existing object-shape/size check.

**Dual-shape retention of 1.1.** This is purely additive: every payload 1.1 accepted is still valid, unchanged, under 1.2. There is nothing to retire, so `SupportedSchemaRanges` widens in place to `{Major: 1, MinMinor: 1, MaxMinor: 2}` — 1.1-stamped imports keep working alongside 1.2.

### 1.1 — v0.8.0 contract

v0.8.0 tightened the import surface beyond what 1.0 accepted:

- **Strict JSON decoder** ([#264](https://github.com/Cyoda/cyoda-go/issues/264)). Unknown fields anywhere in the import body — top-level, on a workflow, on a state, on a transition, on a processor, on a processor's `config` — are rejected with `400 BAD_REQUEST`. Trailing JSON content after the request object is rejected. Typos like `"transitionn"` no longer silently import as a no-op.
- **Structural validation** ([#255](https://github.com/Cyoda/cyoda-go/issues/255)). Empty/dangling `initialState`, transitions pointing at undeclared states, duplicate workflow names within a request, duplicate transition names within a state, empty workflow/state/transition/processor names, identifiers longer than 256 characters, and unknown `executionMode` values are rejected with `400 VALIDATION_FAILED`.
- **`active` semantics** ([#256](https://github.com/Cyoda/cyoda-go/issues/256)). Explicit `"active": false` is now honoured (was previously force-overridden to `true`). Empty `workflows: []` is rejected in `REPLACE` / `ACTIVATE` modes.
- **`asyncResult` / `crossoverToAsyncMs`** ([#261](https://github.com/Cyoda/cyoda-go/issues/261)). Declared in OpenAPI for Cloud parity; v0.8.0 runtime rejects non-default values with `400 VALIDATION_FAILED`.
- **`retryPolicy` enum** ([#262](https://github.com/Cyoda/cyoda-go/issues/262)). Restricted to `NONE`, `FIXED`, or empty; any other value is rejected with `400 VALIDATION_FAILED`. The retryable flag is surfaced back through inbound dispatch responses.
- **Scheduled-transition shape**. New `schedule: {delayMs, timeoutMs}` object on transitions; mutually exclusive with `manual: true`. Engine runtime not yet implemented — declared workflows accept the shape, but `schedule` transitions are silently skipped during cascade and return `TRANSITION_NOT_FOUND` if fired by name (same wire semantic as `disabled`).
- **Import/export boundary hygiene** ([#257](https://github.com/Cyoda/cyoda-go/issues/257)). Export omits default/empty optional fields (`disabled: false`, empty `processors`, empty `desc`); states with no transitions serialise as `{}`.
- **Client annotations**. Optional `annotations` JSON-object field on workflows, states, and transitions — opaque client-owned metadata, stored and round-tripped (compacted), object-only, 64 KB per field. Additive within 1.1 (the field ships in the same unreleased contract); no version bump.

The new rules tighten the accepted-input set rather than merely add fields, so this is a meaningful contract change. Strict semver would call that MAJOR. We bump MINOR (1.0 → 1.1) because every shape v0.8.0 accepts that 1.0 also accepted is byte-identical on the wire — the breakage is purely in what's rejected. Calling it MAJOR would invite repeated MAJOR bumps every release we close another silent-no-op gap, devaluing the MAJOR signal. Clients see a clean diagnosis either way: payloads stamped "1.0" are rejected with `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (1.0 retired from `SupportedSchemaRanges`), so the error is explicit rather than a confusing `VALIDATION_FAILED`.

`SupportedSchemaRanges` declares `{Major: 1, MinMinor: 1, MaxMinor: 1}`. v0.7.x clients sending `"1.0"` are rejected; regenerate against `"1.1"`. No dual-shape acceptance.

### 1.0 — v0.7.2 initial contract (retired in v0.8.0)

First version. Wire shape matches the `WorkflowConfigurationDto` schema in `api/openapi.yaml` at the v0.7.2 commit. Prior to this version, `WorkflowConfigurationDto.version` was an unvalidated free-form string; values such as `"1"` and `"1.0"` were both accepted but conveyed no contract. Pre-1.0 binary; no migration window. Retired in v0.8.0 in favour of 1.1; v0.8.0 binaries reject `"1.0"` with `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`.
