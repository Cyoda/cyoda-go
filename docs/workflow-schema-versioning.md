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

## Changelog

### 1.1 — v0.8.0 contract (current)

v0.8.0 tightened the import surface beyond what 1.0 accepted:

- **Strict JSON decoder** ([#264](https://github.com/Cyoda/cyoda-go/issues/264)). Unknown fields anywhere in the import body — top-level, on a workflow, on a state, on a transition, on a processor, on a processor's `config` — are rejected with `400 BAD_REQUEST`. Trailing JSON content after the request object is rejected. Typos like `"transitionn"` no longer silently import as a no-op.
- **Structural validation** ([#255](https://github.com/Cyoda/cyoda-go/issues/255)). Empty/dangling `initialState`, transitions pointing at undeclared states, duplicate workflow names within a request, duplicate transition names within a state, empty workflow/state/transition/processor names, identifiers longer than 256 characters, and unknown `executionMode` values are rejected with `400 VALIDATION_FAILED`.
- **`active` semantics** ([#256](https://github.com/Cyoda/cyoda-go/issues/256)). Explicit `"active": false` is now honoured (was previously force-overridden to `true`). Empty `workflows: []` is rejected in `REPLACE` / `ACTIVATE` modes.
- **`asyncResult` / `crossoverToAsyncMs`** ([#261](https://github.com/Cyoda/cyoda-go/issues/261)). Declared in OpenAPI for Cloud parity; v0.8.0 runtime rejects non-default values with `400 VALIDATION_FAILED`.
- **`retryPolicy` enum** ([#262](https://github.com/Cyoda/cyoda-go/issues/262)). Restricted to `NONE`, `FIXED`, or empty; any other value is rejected with `400 VALIDATION_FAILED`. The retryable flag is surfaced back through inbound dispatch responses.
- **Scheduled-transition shape**. New `schedule: {delayMs, timeoutMs}` object on transitions; mutually exclusive with `manual: true`. Engine runtime not yet implemented — declared workflows accept the shape, but `schedule` transitions are silently skipped during cascade and return `TRANSITION_NOT_FOUND` if fired by name (same wire semantic as `disabled`).
- **Import/export boundary hygiene** ([#257](https://github.com/Cyoda/cyoda-go/issues/257)). Export omits default/empty optional fields (`disabled: false`, empty `processors`, empty `desc`); states with no transitions serialise as `{}`.

The new rules tighten the accepted-input set rather than merely add fields, so this is a meaningful contract change. Strict semver would call that MAJOR. We bump MINOR (1.0 → 1.1) because every shape v0.8.0 accepts that 1.0 also accepted is byte-identical on the wire — the breakage is purely in what's rejected. Calling it MAJOR would invite repeated MAJOR bumps every release we close another silent-no-op gap, devaluing the MAJOR signal. Clients see a clean diagnosis either way: payloads stamped "1.0" are rejected with `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (1.0 retired from `SupportedSchemaRanges`), so the error is explicit rather than a confusing `VALIDATION_FAILED`.

`SupportedSchemaRanges` declares `{Major: 1, MinMinor: 1, MaxMinor: 1}`. v0.7.x clients sending `"1.0"` are rejected; regenerate against `"1.1"`. No dual-shape acceptance.

### 1.0 — v0.7.2 initial contract (retired in v0.8.0)

First version. Wire shape matches the `WorkflowConfigurationDto` schema in `api/openapi.yaml` at the v0.7.2 commit. Prior to this version, `WorkflowConfigurationDto.version` was an unvalidated free-form string; values such as `"1"` and `"1.0"` were both accepted but conveyed no contract. Pre-1.0 binary; no migration window. Retired in v0.8.0 in favour of 1.1; v0.8.0 binaries reject `"1.0"` with `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`.
