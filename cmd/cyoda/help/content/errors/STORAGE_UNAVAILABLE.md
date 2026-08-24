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

STORAGE_UNAVAILABLE — the storage layer could not supply a connection, or the transaction the request was running in is gone.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

Raised in three cases, all transient:

- The connection pool could not supply a connection within `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (default `10s`). Writes, and reads that need a second connection while your transaction already holds one (a point-in-time read or an async-search submit issued inside a transaction), fail fast here rather than queueing behind a saturated pool.
- An operation found its transaction already aborted because the connection sat idle inside it for longer than `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (default `5m`). The usual cause is a workflow processor whose `responseTimeoutMs` exceeds that ceiling.
- The database connection went away underneath the operation — the session was terminated, or the network dropped it.

Retryable. The same request may well succeed on a second attempt. Repeated occurrences mean the pool is undersized for the offered load, a workflow holds transactions open across a callout longer than the ceiling allows, or the link to the database is unstable.

A statement cancelled by `CYODA_POSTGRES_STATEMENT_TIMEOUT` is **not** reported here. Re-running it would exceed the same ceiling again, so it is a `500` with a ticket rather than a retryable `503`; the server log names the setting that fired.

See `cyoda help config database` for the pool and ceiling settings.

## SEE ALSO

- errors
- errors.CONFLICT
- config.database
