---
topic: errors.SEARCH_QUEUE_FULL
title: "SEARCH_QUEUE_FULL — async-search submission was refused for capacity"
stability: stable
see_also:
  - errors
  - errors.SEARCH_SHARD_TIMEOUT
  - errors.STORAGE_UNAVAILABLE
  - config
---

# errors.SEARCH_QUEUE_FULL

## NAME

SEARCH_QUEUE_FULL — the node refused an async-search submission because its capacity for one is already committed.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

`POST /api/search/async/{entityName}/{modelVersion}` runs on a bounded worker pool: a fixed number of workers drain a fixed-capacity queue. Submission fails fast with this error rather than blocking the request or spawning an unbounded goroutine per submission. The rejected submit leaves nothing behind — no job row, no id to poll.

Two guards raise it, both retryable, both meaning "back off and retry":

- **Node capacity** — no free worker and the submit queue at capacity. Sized by `CYODA_SEARCH_ASYNC_WORKERS` (default `8`) and `CYODA_SEARCH_ASYNC_QUEUE` (default `256`).
- **Tenant share** — your tenant already holds its full share of this node's in-flight jobs. Sized by `CYODA_SEARCH_ASYNC_MAX_PER_TENANT` (default: the worker count; `0` disables the cap). This bound keeps one tenant from consuming the whole pool.

The queue drains as in-flight jobs complete; a client that backs off and retries will typically succeed. Frequent occurrences mean sustained submission volume exceeds what the pool is sized to absorb — raise `CYODA_SEARCH_ASYNC_QUEUE` for burst tolerance, or `CYODA_SEARCH_ASYNC_WORKERS` for sustained throughput (bounded by the storage backend's connection budget: each running job holds a scan connection plus, per chunk, a save connection).

See `cyoda help config` (Search internals) for the variables.

## SEE ALSO

- errors
- errors.SEARCH_SHARD_TIMEOUT
- errors.STORAGE_UNAVAILABLE
- config
