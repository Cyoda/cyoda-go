---
topic: errors.INCOMPATIBLE_TYPE
title: "INCOMPATIBLE_TYPE — entity payload value does not match the model's declared DataType"
stability: stable
see_also:
  - errors
  - errors.VALIDATION_FAILED
  - errors.BAD_REQUEST
  - crud
---

# errors.INCOMPATIBLE_TYPE

## NAME

INCOMPATIBLE_TYPE — an entity create or update payload carried a leaf value whose inferred DataType is not assignable to the schema's declared DataType for that path.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Returned by `POST /entity/{format}/{name}/{version}` and the entity-update surfaces when a leaf field's value cannot be coerced into the model's declared DataType (e.g. submitting `"abc"` against an `INTEGER` field, or `13.111` against an `INTEGER` field on a model whose `changeLevel` is empty so type widening is not in scope).

Equivalent to Cloud's `FoundIncompatibleTypeWithEntityModelException`.

The problem-detail body carries the structured fields below in `properties`, so SDKs can branch on the precondition without scraping the message string:

- `entityName` — model name (e.g. `"nobel-prize"`)
- `entityVersion` — model version as a string (e.g. `"1"`)
- `fieldPath` — dotted path of the offending leaf (e.g. `"price"`, `"address.zip"`)
- `expectedType` — array of declared DataType names for that path (e.g. `["INTEGER"]`); usually one entry, more than one when the schema's TypeSet has been widened by a prior extension
- `actualType` — the DataType inferred from the supplied value (e.g. `"DOUBLE"`, `"STRING"`)

Not retryable. The caller must either correct the payload (cast/format the value into the declared type) or, if the model's `changeLevel` permits, request a schema extension that widens the field's TypeSet.

This code is distinct from:

- `errors.CONDITION_TYPE_MISMATCH` — search-side equivalent, raised when a search condition's literal value does not match the field's locked DataType.
- `errors.VALIDATION_FAILED` — payload holds a value whose KIND the field does not declare (an object where a scalar is declared, say). That is a kind mismatch, not a type mismatch: there is no `DataType` to report for an object or an array, so it carries no `expectedType`/`actualType`.
- `errors.VALIDATION_FAILED` — generic validation failure that is not a leaf type mismatch (e.g. a missing required field, a structural shape mismatch).

## SEE ALSO

- errors
- errors.VALIDATION_FAILED
- errors.BAD_REQUEST
- crud
