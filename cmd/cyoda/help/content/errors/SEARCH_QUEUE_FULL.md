---
topic: errors.SEARCH_QUEUE_FULL
title: "SEARCH_QUEUE_FULL — the async-search worker pool's queue is full"
stability: stable
see_also:
  - errors
  - errors.SEARCH_SHARD_TIMEOUT
  - errors.STORAGE_UNAVAILABLE
  - config
---

# errors.SEARCH_QUEUE_FULL

## NAME

SEARCH_QUEUE_FULL — the async-search worker pool has no free worker and its submit queue is at capacity.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

`POST /api/search/async/{entityName}/{modelVersion}` runs on a bounded worker pool: a fixed number of workers drain a fixed-capacity queue. Submission fails fast with this error the instant both are exhausted, rather than blocking the request or spawning an unbounded goroutine per submission.

The pool is sized by two variables:

- `CYODA_SEARCH_ASYNC_WORKERS` (default `8`) — number of workers.
- `CYODA_SEARCH_ASYNC_QUEUE` (default `256`) — submit queue capacity beyond the running workers.

Retryable. The queue drains as in-flight jobs complete; a client that backs off and retries will typically succeed. Frequent occurrences mean sustained submission volume exceeds what the pool is sized to absorb — raise `CYODA_SEARCH_ASYNC_QUEUE` for burst tolerance, or `CYODA_SEARCH_ASYNC_WORKERS` for sustained throughput (bounded by the storage backend's connection budget: each running job holds a scan connection plus, per chunk, a save connection).

See `cyoda help config` (Search internals) for the two variables.

## SEE ALSO

- errors
- errors.SEARCH_SHARD_TIMEOUT
- errors.STORAGE_UNAVAILABLE
- config
