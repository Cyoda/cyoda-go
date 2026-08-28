---
topic: errors.VALIDATION_FAILED
title: "VALIDATION_FAILED — payload fails model schema validation"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
---

# errors.VALIDATION_FAILED

## NAME

VALIDATION_FAILED — the request payload is structurally valid JSON but fails the model's schema or workflow validation rules.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Unlike `BAD_REQUEST` (which covers parse failures, bad parameters, and unstorable bytes), this error is returned when the payload parsed and then failed against the registered model. On an entity write that means:

- a field the model does not declare (`unexpected field not present in model`)
- a value whose JSON kind the field does not declare (`expected scalar, got array`)
- a change the model's `changeLevel` does not permit
- a field name the wire `jsonPath` grammar cannot address

It is also returned by the model import for sample data that is neither a document nor a collection of documents, and by the workflow import for a structural violation. The error detail names the offending path or key.

A leaf value whose DataType is not assignable carries the more specific `INCOMPATIBLE_TYPE` instead, with `fieldPath`, `expectedType` and `actualType` in `properties`. Missing fields are NOT an error: a model describes the structure it has observed, not a set of required fields.

Correct the payload to match the model, or extend the model — export it and compare.

## SEE ALSO

- errors
- errors.BAD_REQUEST
