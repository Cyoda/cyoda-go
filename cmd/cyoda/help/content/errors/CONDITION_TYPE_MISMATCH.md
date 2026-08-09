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

CONDITION_TYPE_MISMATCH — a search condition's operand parses into none of the field's declared DataTypes.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Validation is parse-based: a comparison or range operand is rejected only when it parses into none of the field's declared DataTypes. For example `"abc"` against a DOUBLE field is rejected — it is not a number. A numeric-looking string against a polymorphic `[INTEGER, STRING]` field is accepted (it parses as STRING).

An operator must also apply to the field's type: string and pattern operators require a text field; ordering and range operators require an ordered type (number, text, timestamp). `IS_NULL`/`NOT_NULL` carry no operand-type constraint. A field with no declared types, and paths not present in the schema, carry no constraint here; an unknown field path is instead rejected by a separate validation pass with `INVALID_FIELD_PATH`.

Temporal meta fields (`creationDate`, `lastUpdateTime`) follow the same rule: a comparison/range operand must parse into a temporal type. A coarse operand (e.g. a bare year, or an offset-less date-time) upscales and is accepted; only an operand that parses into no temporal type is this error.

Both `/search` and the grouped-statistics endpoint (`POST /api/entity/stats/{entityName}/{modelVersion}/query`) enforce this check.

Correct the operand so it denotes a value of one of the target field's declared DataTypes.

## SEE ALSO

- errors
- errors.BAD_REQUEST
- errors.INVALID_FIELD_PATH
- errors.VALIDATION_FAILED
