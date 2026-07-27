---
topic: errors.SEARCH_RESULT_LIMIT
title: "SEARCH_RESULT_LIMIT — search result set exceeds the allowed limit"
stability: stable
see_also:
  - errors
  - errors.SCAN_BUDGET_EXHAUSTED
  - errors.SEARCH_JOB_NOT_FOUND
  - errors.SEARCH_SHARD_TIMEOUT
---

# errors.SEARCH_RESULT_LIMIT

## NAME

SEARCH_RESULT_LIMIT — the search query's matched entity count exceeded the requested `limit`, a cap on the matched set, not a page size.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Direct (synchronous) search is bounded-or-fail: `limit` caps the matched result set rather than paging it. When more entities match than the limit allows, the request is rejected — it never returns a truncated prefix, because a partial result would be indistinguishable from a complete one.

Not retryable with the same parameters. Narrow the condition, raise `limit` (up to the documented maximum), or use async search, which snapshots the full result set and pages over it.

## SEE ALSO

- errors
- errors.SEARCH_JOB_NOT_FOUND
- errors.SEARCH_SHARD_TIMEOUT
