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

Endpoints that accept a search-style condition in the request body — sync and async search, grouped statistics, and the conditional form of delete-by-model — reject a body whose condition cannot be parsed or is otherwise structurally invalid. The condition type is unrecognised, a nested clause is malformed, the JSON does not match the expected condition envelope, an `operatorType` is not one of the canonical operators, a `MATCHES_PATTERN` regex is malformed, or a `BETWEEN`/`BETWEEN_INCLUSIVE` operator's value is not a two-element array.

A `function` clause at any depth is also rejected here: it is a workflow/transition-criterion shape the engine dispatches to a compute member, and search has no dispatcher for it. Move the predicate into a workflow or transition `criterion`, or express the search with `simple`/`lifecycle`/`group`/`array` clauses.

To resolve: correct the condition body to a valid `AbstractConditionDto` (see `cyoda help search`).

## SEE ALSO

- errors
- errors.BAD_REQUEST
