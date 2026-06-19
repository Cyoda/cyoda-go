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

### 1.0 — initial contract

First version. Wire shape matches the `WorkflowConfigurationDto` schema in `api/openapi.yaml` at this commit. Prior to this version, `WorkflowConfigurationDto.version` was an unvalidated free-form string; values such as `"1"` and `"1.0"` were both accepted but conveyed no contract. Pre-1.0 binary; no migration window.
