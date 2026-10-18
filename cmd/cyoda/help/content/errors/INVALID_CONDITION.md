---
topic: errors.INVALID_CONDITION
title: "INVALID_CONDITION — request condition is malformed or unevaluable"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
  - errors.CONDITION_TYPE_MISMATCH
---

# errors.INVALID_CONDITION

## NAME

INVALID_CONDITION — a request body condition (`QueryConditionDto`) was malformed, or is well-formed but carries a leaf no backend can evaluate.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Endpoints that accept a search-style condition in the request body — sync and async search, grouped statistics, and the conditional form of delete-by-model — reject a body whose condition cannot be parsed or is otherwise structurally invalid. The condition type is unrecognised, a nested clause is malformed, the JSON does not match the expected condition envelope, an `operatorType` is not one of the canonical operators, an operand is missing or `null` on an operator other than `IS_NULL`/`NOT_NULL` (every other operator takes one), a `LIKE` or `MATCHES_PATTERN` operand is not a valid pattern, a `BETWEEN`/`BETWEEN_INCLUSIVE` operator's value is not a two-element array of two non-null scalar bounds, an operand is an array on an operator other than `BETWEEN`/`BETWEEN_INCLUSIVE` (no other operator takes a set of values), a `group` clause's `operator` is not `AND`, `OR`, or `NOT`, a `NOT` group's `conditions` does not hold exactly one entry (zero, or two or more), a clause writes its operator twice (two of `operatorType`, `operator`, `operation`), a member name is not one its clause declares (another letter case included) or is written twice (in grouped statistics a duplicate inside `condition` is found while the body is decoded, so it is `MALFORMED_REQUEST`), an `array` clause has no `operatorType` or tests no position (`values` missing, empty or all `null`), or the condition tests more than 10,000 entries. An `array` clause counts one entry per member of `values`, `null` members included, a `function` clause counts one plus the entries of its pre-check, and every other clause counts one; the limit bounds how far a condition expands, because an `array` clause expands into one clause per entry; it does not bound the total cost of a request.

A `function` clause at any depth is also rejected here: it is a workflow/transition-criterion shape the engine dispatches to a compute member, and search has no dispatcher for it. Move the predicate into a workflow or transition `criterion`, or express the search with `simple`/`lifecycle`/`group`/`array` clauses.

A **well-formed** condition also lands here when a leaf cannot be evaluated at all. The commonest cause is a comparison against a field the model declares no type for: the type check treats an undeclared field as carrying no constraint and accepts it, and the evaluator then has nothing to compare against. The response reads `condition contains a leaf the backend cannot evaluate`. This is not a parse failure — the body is syntactically correct, so re-checking the JSON will not help. Check that the `jsonPath` names a field the model actually declares, and that the model has observed data for it. A leaf whose operator applies to none of a field's declared types, or whose comparison or range operand parses into none of the types the operator applies to, is the distinct `CONDITION_TYPE_MISMATCH` instead.

To resolve: correct the condition body to a valid `QueryConditionDto` (see `cyoda help search`), or correct the `jsonPath` to a declared field.

## SEE ALSO

- errors
- errors.CONDITION_TYPE_MISMATCH
- errors.BAD_REQUEST
