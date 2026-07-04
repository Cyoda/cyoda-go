---
topic: errors.INVALID_CONDITION
title: "INVALID_CONDITION — request condition could not be parsed"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
---

# errors.INVALID_CONDITION

## NAME

INVALID_CONDITION — a request body condition (AbstractConditionDto) was malformed and could not be parsed.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Endpoints that accept a search-style condition in the request body — grouped statistics and the conditional form of delete-by-model — reject a body whose condition cannot be parsed. The condition type is unrecognised, a nested clause is malformed, or the JSON does not match the expected condition envelope.

To resolve: correct the condition body to a valid `AbstractConditionDto` (see `cyoda help search`).

## SEE ALSO

- errors
- errors.BAD_REQUEST
