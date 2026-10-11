---
topic: errors.CONDITION_TYPE_MISMATCH
title: "CONDITION_TYPE_MISMATCH — search condition operator or value does not fit the field's type"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
  - errors.INVALID_FIELD_PATH
  - errors.VALIDATION_FAILED
---

# errors.CONDITION_TYPE_MISMATCH

## NAME

CONDITION_TYPE_MISMATCH — a search condition's operand parses into none of the field's declared DataTypes, or its operator applies to none of them.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Validation is parse-based: a comparison or range operand is rejected only when it parses into none of the field's declared DataTypes. For example `"abc"` against a DOUBLE field is rejected — it is not a number. A numeric-looking string against a polymorphic `[INTEGER, STRING]` field is accepted (it parses as STRING).

An operator must also apply to at least one of the field's declared types, and a comparison or range operand must parse into one of the types it applies to. String and pattern operators (`IEQUALS`/`INOT_EQUAL` included) require a text type, `STRING` or `CHARACTER`: `ICONTAINS "1"` on a DOUBLE field is this error, because it could only ever match nothing (or, negated, every value). `STARTS_WITH "2024"` on a LOCAL_DATE field is this error too: a date compares by its own type, not as text — use `GREATER_OR_EQUAL "2024"` with `LESS_THAN "2025"`. Ordering operators require a number, text or a date/time, and range operators a number, `STRING` or a date/time with both bounds in the same one: `GREATER_THAN "false"` on a BOOLEAN field is this error. `IS_NULL`/`NOT_NULL` carry no operand-type constraint. A field with no declared types, and paths not present in the schema, carry no constraint here; an unknown field path is instead rejected by a separate validation pass with `INVALID_FIELD_PATH`.

An operand accepted here because it fits at least one of a polymorphic field's declared types is not guaranteed a real comparison against every entity: for an entity whose own stored value is a type family the operand does not fit, `EQUALS` and the other positive comparison operators answer non-match, while `NOT_EQUAL` answers match — see `predicates` for the unsatisfiable-comparison polarity rule. That is an evaluation-time answer about the entity, not a rejection, and is unaffected by this validation.

Temporal meta fields (`creationDate`, `lastUpdateTime`) follow the same rules: a string or pattern operator does not apply to them, and a comparison/range operand must parse into a temporal type. A coarse operand (e.g. a bare year, or an offset-less date-time) upscales and is accepted; only an operand that parses into no temporal type is this error.

Direct and async search, conditional delete and the grouped-statistics endpoint (`POST /api/entity/stats/{entityName}/{modelVersion}/query`) enforce this check. A workflow criterion is checked when it is evaluated; there, a failure aborts the save with `WORKFLOW_FAILED`.

Correct the operand so it denotes a value of one of the target field's declared DataTypes, or use an operator that applies to the field's type.

## SEE ALSO

- errors
- errors.BAD_REQUEST
- errors.INVALID_FIELD_PATH
- errors.VALIDATION_FAILED
