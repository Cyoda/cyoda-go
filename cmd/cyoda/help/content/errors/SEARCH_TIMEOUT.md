---
topic: errors.SEARCH_TIMEOUT
title: "SEARCH_TIMEOUT — search timeout expired before results collected"
stability: stable
see_also:
  - errors
  - errors.SEARCH_SHARD_TIMEOUT
---

# errors.SEARCH_TIMEOUT

## NAME

SEARCH_TIMEOUT — the client-supplied `timeoutMillis` expired before the search result set was collected.

## SYNOPSIS

HTTP: `408` `Request Timeout`. Retryable: `yes`.

## DESCRIPTION

Fires when the client-supplied `timeoutMillis` elapses before the search result set was fully collected. No partial results are returned.

## SEE ALSO

- errors
- errors.SEARCH_SHARD_TIMEOUT
