---
topic: errors.SCAN_BUDGET_EXHAUSTED
title: "SCAN_BUDGET_EXHAUSTED — residual scan exceeded the backend's row budget"
stability: stable
see_also:
  - errors
  - errors.SEARCH_RESULT_LIMIT
  - errors.CONDITION_TYPE_MISMATCH
---

# errors.SCAN_BUDGET_EXHAUSTED

## NAME

SCAN_BUDGET_EXHAUSTED — a search or grouped-stats condition could not be pushed down to storage, and evaluating it as a residual filter examined more rows than the backend's configured scan budget.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Some conditions are not indexable — for example a regex or a wildcard path match — so the backend must scan candidate rows and post-filter them in the engine instead of pushing the predicate to storage. When the number of rows scanned this way exceeds the configured scan-budget limit, the request fails fast instead of running unbounded.

Not retryable with the same parameters. Narrow the query with an indexable predicate (equality or range on a stored field), add a further selective condition to shrink the candidate set, or raise the backend's scan-budget configuration if the broad scan is intentional.

## SEE ALSO

- errors
- errors.SEARCH_RESULT_LIMIT
- errors.CONDITION_TYPE_MISMATCH
