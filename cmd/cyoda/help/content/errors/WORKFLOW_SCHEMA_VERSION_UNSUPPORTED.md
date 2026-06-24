---
topic: errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED
title: "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED — workflow schema version not accepted"
stability: stable
see_also:
  - errors
  - workflows.schema-version
  - errors.WORKFLOW_NOT_FOUND
  - errors.VALIDATION_FAILED
---

# errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED

## NAME

WORKFLOW_SCHEMA_VERSION_UNSUPPORTED — the workflow definition uses a schema version that this server does not accept.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Every workflow definition must declare a schema version in `MAJOR.MINOR` format (for example `"version": "1.1"`). This error is returned when the import request contains a version string that does not match any supported schema version.

Common causes:

- The `"version"` field is a bare integer (e.g. `"1"`) instead of `MAJOR.MINOR` (e.g. `"1.1"`).
- The `"version"` field refers to a future or deprecated schema revision not supported by this server build. v0.8.0 retires the `1.0` minor used by release/v0.7.x; payloads stamped `"1.0"` are rejected and must be regenerated against `1.1`.
- The `"version"` field is missing from one or more workflow objects in the import payload.

Correct the `"version"` field in every workflow object within the `workflows` array to use `"1.1"` (or the current supported version), then resubmit the import request.

Discover the current supported set via `cyoda help workflows schema-version versions` or `GET /help/workflows/schema-version/versions`.

## SEE ALSO

- errors
- workflows.schema-version
- errors.WORKFLOW_NOT_FOUND
- errors.VALIDATION_FAILED
