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
  "version": "1.5",
  "name": "my-workflow",
  "initialState": "ready",
  "states": { "ready": {} }
}
```

## SEMANTICS

- **MAJOR** bumps when a payload valid under the previous MAJOR is no longer valid (or vice-versa) — removing a field, renaming, changing semantics, making an optional field required.
- **MINOR** bumps for additive, backward-compatible changes — a new optional field, a new enum value in an existing string-enum, a new condition operator. **This is the common case.**

Multiple MAJORs may be accepted concurrently during a deprecation window. Within a MAJOR, the server accepts any MINOR in its declared `[minMinor, maxMinor]` range.

Current contract: **1.5**, which added `idempotent` to a processor's `config` and `retryPolicy` to `schedule.function` (dual-shape: 1.1 through 1.4 remain accepted). The same release validates `retryPolicy` on a `function`-type criterion and bounds `responseTimeoutMs` on every callout by the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`; both rules apply to an import under any schema version. Because the bound is a server setting, a workflow exported from one deployment can be refused by another with a lower bound. 1.4 added the `NOT` group operator. Not every behaviour change bumps this contract — see `docs/workflow-schema-versioning.md` for the rationale behind each decision.

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
  "current": "1.5",
  "supported": [
    { "major": 1, "minMinor": 1, "maxMinor": 5 }
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
test "$current" = "1.5" || { echo "schema drift"; exit 1; }
```
