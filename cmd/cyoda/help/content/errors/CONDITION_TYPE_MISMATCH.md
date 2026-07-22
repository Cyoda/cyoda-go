---
topic: errors.CONDITION_TYPE_MISMATCH
title: "CONDITION_TYPE_MISMATCH — search condition value type incompatible with field"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
  - errors.INVALID_FIELD_PATH
  - errors.VALIDATION_FAILED
---

# errors.CONDITION_TYPE_MISMATCH

## NAME

CONDITION_TYPE_MISMATCH — a search condition value's JSON type is incompatible with the field's locked DataType in the model schema.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Each field in a locked model has an inferred DataType (e.g. INTEGER, DOUBLE, BOOLEAN). When a search condition references a numeric or boolean field, the condition's value must be type-compatible with that field. For example, submitting a string value `"abc"` against a DOUBLE field is rejected.

String fields are not strictly enforced — any comparison value (numeric or string) is accepted to support lexicographic and coerced comparisons. Conditions that reference field paths not present in the model schema are rejected by a separate pre-execution validation pass with `INVALID_FIELD_PATH`; the type-checker itself has no opinion on unknown paths.

Lifecycle/meta conditions (e.g. `state`, `creationDate`, `lastUpdateTime`) are also type-checked: temporal fields (`creationDate`, `lastUpdateTime`) only accept ordering/equality/BETWEEN/null-presence operators with an offset-bearing RFC3339 operand — a string-shaped operator such as `CONTAINS` or a non-date operand against a temporal field is this same error.

IS_NULL and NOT_NULL operators bypass type checking entirely. Null values are compatible with any field type.

Both `/search` and the grouped-statistics endpoint (`POST /api/entity/stats/{entityName}/{modelVersion}/query`) enforce this check.

Correct the condition value so that its type matches the target field's declared DataType.

## SEE ALSO

- errors
- errors.BAD_REQUEST
- errors.INVALID_FIELD_PATH
- errors.VALIDATION_FAILED
