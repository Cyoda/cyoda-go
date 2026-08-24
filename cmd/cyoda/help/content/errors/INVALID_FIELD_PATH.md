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

INVALID_FIELD_PATH — a condition's `jsonPath` is not valid JSON Path syntax, or names a field absent from the target model's locked schema.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no` (unless the model schema is then extended via re-import; the request is then valid against the new locked schema).

## DESCRIPTION

Two checks emit this code.

**1. Syntax.** A condition's `jsonPath` is JSON Path nomenclature, so the `$.` leader is required:

```
jsonPath = "$." segment ( "." segment )*
segment  = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
```

`$.amount` and `$.address.city` are paths. A bare `amount` is **not** one and is rejected — it is not a tolerated alias. So are an empty path, an empty or trailing segment (`$..a`, `$.a.`), bracket-quoted property access (`$['x']`, `$.['x']` — use dotted access instead), and any character outside the segment set.

An array-subscripted path (`$.tags[*].name`, `$.arr[0]`) is valid JSON Path and is **accepted**; it cannot be pushed into the storage query, so it is evaluated in memory.

This check is syntactic and runs on every search-shaped surface regardless of whether a schema is loaded: `/search` (sync and async), conditional delete, and the `condition` of a grouped-statistics query.

**2. Schema.** Before executing a search, the server validates that every data-field path referenced by the condition (e.g. `$.price`, `$.profile.email`) resolves against the target model's locked schema. Lifecycle paths (`state`, `previousTransition`, etc.) bypass this check.

A path that is a **pure container** — a known structural interior with substructure (an object with child fields) but no scalar observation of its own — is also rejected this way when compared with a scalar operator: a container has no scalar value to compare against, so the client must navigate to a leaf sub-path. `IS_NULL`/`NOT_NULL` are exempt (they test presence, not a value) and remain valid on a container path. A path observed as **both** an object and a bare scalar across different entities is not a pure container — it is searchable via its scalar type, and the object-valued entities remain reachable through their child leaves.

If any referenced path is unknown, the server performs at most one bounded `RefreshAndGet` against the model store to recover from a stale cached schema. If the path is still unknown after the refresh, the request is rejected with HTTP 400 and `errorCode: "INVALID_FIELD_PATH"`. The response detail names every offending path so clients can correct the request without round-tripping to the support team.

A condition referencing an unrecognized lifecycle/meta filter field (not one of the known `state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave`/`previousTransition`, `transactionId`, `id` fields) is rejected the same way, on both `/search` and the grouped-statistics endpoint (`POST /api/entity/stats/{entityName}/{modelVersion}/query`). Grouped statistics does not have data-model schema access, so it enforces this meta-field check but not the schema-backed data-field-path check described above.

Programmatic clients should branch on `errorCode == "INVALID_FIELD_PATH"` (not on HTTP 400) to distinguish unknown-field-path errors from other 400s such as `BAD_REQUEST` (malformed JSON) or `CONDITION_TYPE_MISMATCH` (incompatible value type).

Common causes:

- The `$.` leader is missing (`amount` instead of `$.amount`).
- Bracket-quoted access (`$['amount']`) instead of dotted access (`$.amount`).
- The condition references a field that has not been declared in the model schema.
- The model has been re-imported with a different shape and the client's condition uses an old field name.
- The path is misspelled (e.g. `$.Name` vs `$.name`).

To resolve: write the path as JSON Path with the `$.` leader, then verify the field against the model's schema (`GET /api/model/.../export`), or extend the model schema and re-lock it before retrying.

The same grammar applies to a grouped-statistics `groupBy` entry and aggregation `field`, but those report `INVALID_GROUP_BY_PATH` / `INVALID_AGGREGATION_FIELD` instead, and additionally reject array subscripts (a group key must be a single scalar). The reserved `groupBy` token `state` is a token, not a path, and needs no leader.

## SEE ALSO

- errors
- errors.BAD_REQUEST
- errors.CONDITION_TYPE_MISMATCH
- search
