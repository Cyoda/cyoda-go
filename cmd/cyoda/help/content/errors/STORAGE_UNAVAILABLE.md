---
topic: errors.STORAGE_UNAVAILABLE
title: "STORAGE_UNAVAILABLE — storage could not serve the request in time"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
  - config.database
---

# errors.STORAGE_UNAVAILABLE

## NAME

STORAGE_UNAVAILABLE — the storage layer could not supply a connection, or the transaction was reclaimed by the idle-in-transaction ceiling.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

Raised in two cases, both transient contention:

- The connection pool could not supply a connection within `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (default `10s`). Writes fail fast here rather than queueing behind a saturated pool.
- An operation found its transaction already aborted because the connection sat idle inside it for longer than `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (default `5m`). The usual cause is a workflow processor whose `responseTimeoutMs` exceeds that ceiling.

Retryable. The same request may well succeed on a second attempt. Repeated occurrences mean the pool is undersized for the offered load, or a workflow holds transactions open across a callout longer than the ceiling allows.

See `cyoda help config.database` for the pool and ceiling settings.

## SEE ALSO

- errors
- errors.CONFLICT
- config.database
