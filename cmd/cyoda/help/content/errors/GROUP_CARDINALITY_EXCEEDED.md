---
topic: errors.GROUP_CARDINALITY_EXCEEDED
title: "GROUP_CARDINALITY_EXCEEDED — too many distinct group buckets"
stability: stable
see_also:
  - errors
  - crud
  - config
  - errors.INVALID_LIMIT
---

# errors.GROUP_CARDINALITY_EXCEEDED

## NAME

GROUP_CARDINALITY_EXCEEDED — the grouped-statistics query produced more distinct group keys than the configured ceiling allows.

## SYNOPSIS

HTTP: `422` `Unprocessable Entity`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. `CYODA_STATS_GROUP_MAX` (default `10000`) bounds the number of distinct buckets the endpoint will build. The bound is enforced on both execution paths — the backend's native GROUP BY pushdown and the in-process streaming tally — so the same query is rejected identically on every storage backend.

No partial result is returned: the buckets accumulated so far are a prefix of the answer, not the answer, and returning them would be indistinguishable from a complete result.

Retrying the same request gives the same outcome. Narrow the population with a more selective `condition`, drop a `groupBy` dimension (high-cardinality payload fields such as identifiers are the usual cause), or have an operator raise `CYODA_STATS_GROUP_MAX`. Note that `limit` does not help: it caps the buckets *returned* after the full set has been built, so the ceiling is reached first — a `limit` above the ceiling is itself rejected with `INVALID_LIMIT`.

## SEE ALSO

- errors
- crud
- config
- errors.INVALID_LIMIT
