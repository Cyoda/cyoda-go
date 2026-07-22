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

SEARCH_RESULT_LIMIT — the search query matched more results than the server-enforced maximum page or result set size.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

The server imposes an upper bound on the number of results returned per page and per job to protect cluster resources. Returned when the request exceeds this limit — either by requesting too large a page size or by the matched result count exceeding the cap.

Not retryable with the same parameters. A smaller `pageSize` or more selective filter conditions reduce the result set below the cap.

## SEE ALSO

- errors
- errors.SEARCH_JOB_NOT_FOUND
- errors.SEARCH_SHARD_TIMEOUT
