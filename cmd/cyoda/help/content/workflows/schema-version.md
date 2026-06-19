---
topic: workflows.schema-version
title: "workflows schema-version — wire-format contract for workflow import"
stability: stable
see_also:
  - workflows
  - errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED
  - openapi
---

# workflows schema-version

## NAME

workflows schema-version — semver `MAJOR.MINOR` contract identifying the workflow-import DTO shape that a workflow definition was authored against.

## SYNOPSIS

Every `WorkflowConfigurationDto` carries a `version` field. The server validates it strictly on import and stamps the current contract version on every workflow it exports.

```json
{
  "version": "1.0",
  "name": "my-workflow",
  "initialState": "ready",
  "states": { "ready": {} }
}
```

## SEMANTICS

- **MAJOR** bumps when a payload valid under the previous MAJOR is no longer valid (or vice-versa) — removing a field, renaming, changing semantics, making an optional field required.
- **MINOR** bumps for additive, backward-compatible changes — a new optional field, a new enum value in an existing string-enum, a new condition operator. **This is the common case.**

Multiple MAJORs may be accepted concurrently during a deprecation window. Within a MAJOR, the server accepts any MINOR in its declared `[minMinor, maxMinor]` range.

## DISCOVERY

Authoritative discovery is via the `versions` action:

```
cyoda help workflows schema-version versions
```

HTTP mirror:

```
GET /help/workflows/schema-version/versions
```

Both emit the same structured JSON:

```json
{
  "current": "1.0",
  "supported": [
    { "major": 1, "minMinor": 0, "maxMinor": 0 }
  ]
}
```

## VALIDATION ERRORS

On import, an unsupported or malformed `version` returns HTTP 400 with `errorCode: "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED"`. The message body distinguishes:

- **Malformed** (`"x"`, `"1"`, `"1.0.0"`, leading zeros) — not in `MAJOR.MINOR` form.
- **Major unsupported** — the major version is not in any supported range.
- **Minor too new** — the major matches but the minor exceeds this server's `maxMinor`. Upgrade cyoda-go, or regenerate the file against an older schema.
- **Minor too old** — the major matches but the minor is below the server's `minMinor` (deprecation window). Re-author the file against a supported MINOR.

## EXAMPLE: PINNING

Pin your authoring tools and CI to the schema version they were tested against:

```bash
# in a CI step
current=$(curl -s $CYODA_HOST/api/help/workflows/schema-version/versions | jq -r .current)
test "$current" = "1.0" || { echo "schema drift"; exit 1; }
```
