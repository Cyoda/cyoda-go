---
topic: errors.INVALID_FIELD_PATH
title: "INVALID_FIELD_PATH — search condition references unknown field path"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
  - errors.CONDITION_TYPE_MISMATCH
  - search
---

# errors.INVALID_FIELD_PATH

## NAME

INVALID_FIELD_PATH — a search condition references one or more JSONPath field paths that are absent from the target model's locked schema.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no` (unless the model schema is then extended via re-import; the request is then valid against the new locked schema).

## DESCRIPTION

Before executing a search, the server validates that every data-field path referenced by the condition (e.g. `$.price`, `$.profile.email`) resolves against the target model's locked schema. Lifecycle paths (`state`, `previousTransition`, etc.) and meta paths (`$._meta.*`) bypass this check.

If any referenced path is unknown, the server performs at most one bounded `RefreshAndGet` against the model store to recover from a stale cached schema. If the path is still unknown after the refresh, the request is rejected with HTTP 400 and `errorCode: "INVALID_FIELD_PATH"`. The response detail names every offending path so clients can correct the request without round-tripping to the support team.

Programmatic clients should branch on `errorCode == "INVALID_FIELD_PATH"` (not on HTTP 400) to distinguish unknown-field-path errors from other 400s such as `BAD_REQUEST` (malformed JSON) or `CONDITION_TYPE_MISMATCH` (incompatible value type).

Common causes:

- The condition references a field that has not been declared in the model schema.
- The model has been re-imported with a different shape and the client's condition uses an old field name.
- The path is misspelled (e.g. `$.Name` vs `$.name`).

To resolve: verify the field path against the model's schema (`GET /api/model/.../export`), or extend the model schema and re-lock it before retrying.

## SEE ALSO

- errors
- errors.BAD_REQUEST
- errors.CONDITION_TYPE_MISMATCH
- search
