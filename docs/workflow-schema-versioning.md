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

### Criterion `jsonPath` grammar tightened at import (v0.8.4)

A `simple`/`array` criterion's `jsonPath` is now checked against the same
bracket-subscript grammar a search condition obeys, instead of a looser
check that only confirmed the `$.` leader. A bracket spelling outside the
grammar — `$.a[-1]`, `$.a[0:2]`, `$.a[?(@.x)]`, `$.a[`, `$.a[0]b`, and
similar — is now rejected at import with `400 VALIDATION_FAILED`, where it
previously imported cleanly and the criterion then silently never fired,
because no evaluator resolves those spellings. Landed as part of
[cyoda-go#538](https://github.com/Cyoda/cyoda-go/pull/538) ("one path
grammar and one path resolver"), which also unified the search and criterion
path validators onto the single grammar `docs/cloud-parity/path-grammar.md`
defines. See `cmd/cyoda/help/content/workflows.md`'s CRITERIA section.

This is the §"When NOT to bump" "bug-fixing a validator that was already
supposed to reject something" case, same as the v0.8.3 malformed-regex entry
above: a criterion carrying one of these bracket spellings never worked as a
correct guard — no evaluator resolves it, so the transition it guarded
always silently failed to fire — so no *working* config is newly rejected,
only the failure moves from a silent, permanent no-op at evaluation to a
loud one at import. `WorkflowConfigurationDto`'s shape is unchanged; only
what a `simple`/`array` clause's `jsonPath` value must look like tightens.
No `CurrentSchemaVersion` or `SupportedSchemaRanges` change.

**This is independent of, and predates, the 1.3 → 1.4 bump below.** It
applies to a criterion imported under any schema version — 1.1 through
1.4 alike — because it is a validation-layer change, not a DTO-shape one.
It is the reason the 1.4 entry below qualifies "every 1.3 payload remains
valid" rather than stating it unconditionally: a 1.3-stamped payload whose
criterion carries a malformed bracket path imported successfully before
this change and is rejected by the same v0.8.4 binary now, regardless of
which schema version it declares.

### Unrecognised-operator criteria rejected at evaluation, not import (v0.8.4)

A workflow-level or transition-level criterion carrying an operator name the
evaluator does not recognise (e.g. `FROBNICATE`) now fails the transition
attempt it guards with `400 WORKFLOW_FAILED`, rolled back, instead of the
condition silently evaluating to "not satisfied" for any entity that
short-circuited past the bad operator under the old lazy walk. See
[`docs/cloud-parity/unevaluable-criterion-fails-save.md`](./cloud-parity/unevaluable-criterion-fails-save.md).

This is the §"When NOT to bump" "bug-fixing a validator that was already
supposed to reject something" case, same as the v0.8.3 malformed-regex entry
above: a criterion nobody can evaluate never worked as a correct guard, so no
*working* config is newly rejected — only which entities surface the failure
changes (all of them, now, instead of only the ones that reached the bad
leaf), and evaluation-time failure replaces a silent no-op. Import-time
acceptance is unchanged: `WorkflowConfigurationDto` still validates only
`MATCHES_PATTERN` regex syntax, not operator names, so the same workflow
imports byte-identically. No `CurrentSchemaVersion` or `SupportedSchemaRanges`
change.

### Undeclared-field criteria rejected at evaluation, not import (v0.8.4)

A query never executes against a field the model does not declare — there is
no such thing as an undeclared field to query. A criterion naming such a
field now aborts and rolls back the save that evaluates it with `400
WORKFLOW_FAILED`, instead of the condition silently evaluating to "not
satisfied" and the save succeeding. See
[`docs/cloud-parity/unevaluable-criterion-fails-save.md`](./cloud-parity/unevaluable-criterion-fails-save.md).

This is the same "bug-fixing a validator that was already supposed to reject
something" case as the unrecognised-operator entry directly above it, reached
through a different check: a criterion on a field the model does not declare
never worked as a correct guard either, so no *working* config is newly
rejected. Import-time acceptance is unchanged — `walkCriterion` still checks
only path grammar, operator names, lifecycle type-soundness and pattern
operands at import, not model membership, so a criterion naming a field the
model has not yet declared still imports byte-identically; the field's
declaration is a modelling step, not an import-validation one. No
`CurrentSchemaVersion` or `SupportedSchemaRanges` change.

Not to be confused with the `NOT` group operator entry under the Changelog
below, which **is** a bump: the two are separate changes shipped in the same
release, and neither's bump decision generalises to the other.

## Required commit-/PR-time checks

Before merging a schema bump:

- `go test ./internal/domain/workflow/...` green, including `TestCurrentSchemaVersionIsSupported`, `TestOpenAPIWorkflowVersionContract`, `TestDefaultWorkflowFixtureSchemaVersion`, `TestValidateSchemaVersions_*`.
- `go test ./internal/e2e/...` green, including `TestWorkflowSchemaVersion_*` (happy path, retired-MINOR rejection, malformed rejection, export-stamping, discovery).
- `cyoda help workflows schema-version versions` returns the expected `{current, supported[]}` JSON. The CLI is the in-product documentation channel; if it drifts from the constants, customers will hit it.
- The CHANGELOG entry under `[Unreleased]` (or the current milestone's section) names the bump, the rationale, and any retired MINORs by version string.

## Changelog

### 1.4 — v0.8.4 contract (current)

Additive MINOR — one new condition operator, `NOT`, accepted on a criterion's
`group` clause:

- **`NOT` on `GroupConditionDto.operator`.** `NOT` takes exactly one entry in
  `conditions` (rejected at import with `400 VALIDATION_FAILED` if zero, or
  two or more) and inverts that entry's own two-valued answer. This widens
  the accepted-input set for the criterion embedded in
  `WorkflowDefinition.criterion` and `TransitionDefinition.criterion` — the
  bump rules above name "a new condition operator" as the canonical additive
  MINOR example. See
  [`docs/cloud-parity/negation.md`](./cloud-parity/negation.md) for the full
  contract.

Every 1.3 payload whose criteria import cleanly today — including one
whose `group` clauses use only `AND`/`OR` — is byte-identical and remains
valid under 1.4; `NOT` is purely additive alongside `AND`/`OR`, not a
replacement for either, and this bump by itself rejects nothing a prior
MINOR accepted.

**That is not the same claim as "every payload previously accepted by a
1.3-labelled binary still imports."** It is not, independent of this bump:
this same v0.8.4 release also tightens criterion `jsonPath` validation at
import (see "Criterion `jsonPath` grammar tightened at import (v0.8.4)"
under "When NOT to bump" above) — a 1.3-stamped criterion carrying a
malformed bracket path such as `$.a[-1]` imported cleanly before that
change and is rejected now. That tightening is orthogonal to the schema
version number and does not retire any MINOR; it is called out here only so
this entry's own additivity claim is not read more broadly than it is.

**Dual-shape retention of 1.1, 1.2 and 1.3.** Nothing is retired:
`SupportedSchemaRanges` widens in place to
`{Major: 1, MinMinor: 1, MaxMinor: 4}` — 1.1-, 1.2- and 1.3-stamped imports
keep working alongside 1.4.

This bump is for the criterion **shape** only. Two related evaluation-time
behaviour changes shipped in the same release do not bump this contract —
see the "Undeclared-field criteria rejected at evaluation, not import
(v0.8.4)" and "Unrecognised-operator criteria rejected at evaluation, not
import (v0.8.4)" entries under "When NOT to bump" above. Each of the three
changes is decided on its own terms; none of the three decisions
generalises to another.

### 1.3 — v0.8.3 contract

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
