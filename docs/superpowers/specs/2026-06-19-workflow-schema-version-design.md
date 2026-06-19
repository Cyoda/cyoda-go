# Workflow Schema Version

## Problem

Applications that author and validate workflow-import resource files have no
way to know which workflow-import contract their file is written against, nor
which contracts a given cyoda-go server will accept. The
`WorkflowConfigurationDto.version` field exists in the OpenAPI spec and is
already required on import, but it is an unvalidated free-form string. In-tree
fixtures use a mix of `"1"` and `"1.0"`, neither of which the server
distinguishes from any other value.

The consequence: when the workflow DTO shape evolves — new optional fields,
new condition operators, new enum values — there is no signal in the wire
format that tells either side what to expect. A file produced against a newer
server is silently accepted by an older one (and may parse without complaint
even when the new feature is silently dropped).

## Goal

Give file-producing applications and file-consuming servers a precise,
discoverable, strictly-validated contract for the workflow-import DTO shape,
independent of cyoda-go binary version and OpenAPI document version.

Specifically:

1. Every workflow declares the contract it was written against.
2. The server validates that declaration against its supported set at import.
3. Both sides discover the server's supported set the same way: via the
   existing `cyoda help` → `GET /help/...` mirror.

## Non-goals

- Versioning the cyoda-go binary or the OpenAPI document. Those have their
  own versions and lifecycles.
- Auto-migrating older payloads to newer payloads. Until there is a v2,
  there is nothing to migrate. v1→v2 conversion (if it is ever wanted) is a
  separate, future design.
- Stamping the cyoda-go binary version on exports. The help payload's
  `version` field already exposes it; baking it into every exported
  workflow conflates two concerns.

## Design

### The wire field

`WorkflowConfigurationDto.version` (per-workflow, already required) is given
real semantics: it identifies the workflow-import DTO contract shape, in
**semver MAJOR.MINOR** form (string).

- Format: `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`. No PATCH — a schema has no
  "bug fix" that doesn't change shape, so PATCH would always be 0; the
  empty slot is just noise.
- Required (unchanged from today's OpenAPI spec).
- Strictly validated at import: unknown → `400
  WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`.
- Stamped to the server's current version on every workflow returned by
  export.

### Semver semantics

- **MAJOR**: a payload valid under the previous MAJOR is no longer valid, or
  vice-versa. Examples: removing a field, renaming a field, making an
  optional field required, changing semantics of an existing field.
- **MINOR**: additive, backward-compatible. Examples: new optional field, new
  enum value in an existing string-enum field, new optional sub-object, new
  condition operator. **This is the common case.**

Both bumps require a one-line entry in `docs/workflow-schema-versioning.md`
explaining what changed and citing the PR.

### Acceptance rule

An incoming `version` parses to `(major, minor)` and is accepted iff that
pair falls inside one of the server's supported ranges. Distinct failure
modes get distinct error messages:

- Malformed (`"x"`, `"1"`, `"1.0.0"`, empty, leading zeros) → falls through
  the parser with `"workflow schema version \"1.0.0\" is not in MAJOR.MINOR form"`.
- `MAJOR` not in any range →
  `"workflow schema major version 2 unsupported on this server; supported majors: [1]"`
- `MAJOR` in a range but `MINOR` above that range's `MaxMinor` →
  `"this server supports workflow schema up to 1.3; payload declares 1.7. Upgrade cyoda-go or regenerate the file against an older schema."`
- `MAJOR` in a range but `MINOR` below that range's `MinMinor` →
  `"workflow schema 1.0 is no longer accepted on this server; minimum supported: 1.2"`

Validation runs per workflow in the request, so a file with mixed-version
entries reports the offender by name.

### Single source of truth

A new file `internal/domain/workflow/schemaversion.go`:

```go
package workflow

// CurrentSchemaVersion is stamped on every exported workflow.
// Bump only per docs/workflow-schema-versioning.md. CurrentSchemaVersion
// MUST be inside one of SupportedSchemaRanges; schemaversion_test.go
// asserts this.
const CurrentSchemaVersion = "1.0"

// SchemaRange is a closed integer interval [MinMinor..MaxMinor] on the
// MINOR axis of a given MAJOR. A range models a single contiguous
// supported window — when a MINOR ages out, raise MinMinor; when an
// older MAJOR is retired, drop its range entirely.
type SchemaRange struct {
    Major    int
    MinMinor int
    MaxMinor int
}

// SupportedSchemaRanges is the closed set of (MAJOR, MINOR) pairs the
// server accepts on import. Today: only 1.0. Adding 1.1 means
// {Major:1, MinMinor:0, MaxMinor:1}. Adding a MAJOR 2 line later means
// appending {Major:2, MinMinor:0, MaxMinor:0} without disturbing the
// existing entry.
var SupportedSchemaRanges = []SchemaRange{
    {Major: 1, MinMinor: 0, MaxMinor: 0},
}

// ParseSchemaVersion parses a MAJOR.MINOR string into integers.
// Rejects anything that doesn't match the format exactly — no leading
// zeros (except single "0"), no whitespace, no PATCH suffix.
func ParseSchemaVersion(s string) (major, minor int, err error) { ... }

// Supports reports whether (major, minor) is inside any supported range.
// On failure, returns one of three sentinel errors so the import handler
// can produce a precise message: ErrSchemaMajorUnsupported,
// ErrSchemaMinorTooNew, ErrSchemaMinorTooOld.
func Supports(major, minor int) error { ... }
```

Consumed by:

1. **Import validation** (`internal/domain/workflow/validate.go`): each
   incoming workflow's `Version` is parsed and checked via `Supports`.
   Sentinel errors map to the four message variants above and to
   `ErrCodeWorkflowSchemaVersionUnsupported`.
2. **Export handler** (`internal/domain/workflow/handler.go`): every
   workflow in the export response has `Version = CurrentSchemaVersion`,
   overriding whatever was stored. Stored data is the source of truth for
   *content*; the wire-format contract version is owned by the serialiser.
3. **Help topic action** (new file `cmd/cyoda/help/workflow_schema.go`):
   emits the JSON below as the `versions` action under a new
   `workflows.schema-version` subtopic (placed under the existing
   `workflows` topic for natural discoverability). Surfaces via CLI
   (`cyoda help workflows schema-version versions`) and HTTP
   (`GET /help/workflows/schema-version/versions`).
4. **HTTP action mirror** (`internal/api/help.go`): the existing
   `RegisterHelpRoutes` mirrors topic *bodies* to HTTP but does not
   dispatch topic *actions* — `GET /help/grpc/proto`, for example, is
   currently 404. This change extends the handler to detect
   `topic + action` paths and invoke the registered handler, with a
   declared `Content-Type` per action. The change is generic — all
   existing actions (`grpc proto`, `grpc json`, `openapi json`,
   `openapi yaml`, `openapi tags`, `cloudevents json`) become reachable
   via HTTP for free, closing a gap between the CLI and HTTP help
   surfaces. The action registry grows a `ContentType` field on each
   entry. No new dedicated REST surface.
5. **OpenAPI consistency test** (new
   `internal/domain/workflow/openapi_consistency_test.go` or extension of
   an existing consistency test): parses `api/openapi.yaml`, asserts the
   `WorkflowConfigurationDto.version` description names the help endpoint
   and that the pattern matches
   `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`. Drift between hand-maintained
   YAML and the Go constant becomes a test failure, not a runtime
   surprise.

### Discovery surfaces

Both surfaces emit the same structured JSON (no string parsing for
consumers):

```json
{
  "current": "1.0",
  "supported": [
    { "major": 1, "minMinor": 0, "maxMinor": 0 }
  ]
}
```

- **CLI:** `cyoda help workflows schema-version versions`
- **HTTP:** `GET /help/workflows/schema-version/versions`

The `workflows.schema-version` topic itself (without the action) returns
markdown explaining the contract, when MAJOR vs MINOR applies, and a
worked example of how a consumer pins its files. The topic is registered
as a subtopic under the existing `workflows` topic.

### OpenAPI changes

In `api/openapi.yaml`:

- `WorkflowConfigurationDto.version`: replace the free-form-string
  description with one that explicitly names the format (`MAJOR.MINOR`)
  and the authoritative discovery endpoint
  (`GET /help/workflow-schema/versions`). Add
  `pattern: "^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"` — same regex the Go
  parser enforces, so the schema, the runtime, and the test fixtures
  cannot drift on shape.
- New error code documented wherever existing error codes are documented
  (`internal/common/error_codes.go` and the relevant help topic /
  errors schema).
- New help topic listed under `cyoda help` root.

The OpenAPI document does NOT enumerate supported values. The spec
describes shape; the server is authoritative on which values are accepted
at any given moment. This is the same posture the project takes for other
runtime-discoverable lists.

### Existing-fixture migration

In-tree fixtures and `default_workflow.json` currently mix `"1"` and `"1.0"`.
Under semver `MAJOR.MINOR`, `"1"` fails the pattern. Bulk-rewrite all such
values to `"1.0"` (the canonical v1 starting point) as part of this change.

Because cyoda-go is pre-1.0 binary, the existing per-workflow `version` field
had no enforced semantics, and there are no known external consumers of the
field, no compatibility shim is warranted.

### Error code

New code in `internal/common/error_codes.go`:

```go
ErrCodeWorkflowSchemaVersionUnsupported = "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED"
```

All four sub-cases (major unsupported, minor too new, minor too old,
malformed) surface this code; the message body distinguishes them. A 4xx
domain error: per the project's error-handling rule, the client configured
the file and should see exactly what went wrong.

## Tests (TDD-driven)

### Unit tests

- `internal/domain/workflow/schemaversion_test.go` (new)
  - `CurrentSchemaVersion` parses and is inside `SupportedSchemaRanges`.
  - `ParseSchemaVersion` table-driven cases:
    `"1.0"` → (1, 0); `"12.345"` → (12, 345); `"0.0"` → (0, 0);
    rejects `""`, `"1"`, `"1.0.0"`, `" 1.0 "`, `"v1.0"`, `"1.x"`,
    `"01.0"`, negatives, non-ASCII digits.
  - `Supports` table: in-range pair → nil; major out of all ranges →
    `ErrSchemaMajorUnsupported`; minor above MaxMinor →
    `ErrSchemaMinorTooNew`; minor below MinMinor →
    `ErrSchemaMinorTooOld`.

- `internal/domain/workflow/validate_import_test.go` (extend)
  - Import body with `version: "2.0"` → 400 with
    `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` and message naming supported
    majors.
  - `version: "1.99"` → 400 with the "too new" message.
  - `version: "1.0.0"` → 400 with the "not in MAJOR.MINOR form" message.
  - Multi-workflow import where the second entry has bad version: error
    names the offending workflow.

- OpenAPI consistency test (extend or new): YAML pattern and description
  for `WorkflowConfigurationDto.version` match the agreed shape.

### E2E tests

- `internal/e2e/workflow_schema_version_test.go` (new)
  - Import with `version: "1.0"` → 200; export → every workflow in
    response carries `version: "1.0"`.
  - Import with `version: "2.0"` → 400 with the new code.
  - `GET /help/workflows/schema-version/versions` → 200 with the
    documented JSON shape and `application/json` content type.
  - `GET /help/workflows/schema-version` → 200 with topic descriptor.
  - One regression check on an existing action mirror, e.g.
    `GET /help/grpc/proto` returns 200 + `text/plain` content (proves
    the action mirror is generic, not workflow-schema-specific).

### Help-subsystem tests

- `cmd/cyoda/help/workflow_schema_test.go` (new): `versions` action returns
  the same JSON shape; topic registration is wired into the help tree.
- `internal/api/help_action_test.go` (new): HTTP action dispatch returns
  registered handlers' output with the declared `Content-Type`. Covers
  each existing action plus the new `versions` action.

## Out of scope (deliberately deferred)

- A `deprecated: true` flag on individual `SchemaRange` entries — only
  meaningful once there's a second MAJOR or a sunset window.
- Version-to-version converters — only one version exists.
- Per-field minimum-version annotations (e.g. "this field requires 1.2+") —
  only meaningful when at least one MINOR exists past `1.0`.
- Stamping the cyoda-go binary version alongside the schema version on
  exports — separate concern, discoverable elsewhere.

## Forward-port plan

This work lands first on a `release/v0.7.x` maintenance branch (cut from the `v0.7.1`
tag at `39e3266`). After it ships, it is forward-ported to `release/v0.8.0`
by `git cherry-pick` or a clean re-merge — the touch surface is small and
disjoint from in-flight v0.8.0 work (workflow domain code, help subsystem,
OpenAPI spec, fixture strings). The forward-port carries the same tests,
same fixture sweep, and a v0.8.0 entry in
`docs/workflow-schema-versioning.md` noting the version it first shipped
in (`1.0` from v0.7.2; no bump from v0.7.2 → v0.8.0 unless a v0.8.0 change
introduces a shape change first).
