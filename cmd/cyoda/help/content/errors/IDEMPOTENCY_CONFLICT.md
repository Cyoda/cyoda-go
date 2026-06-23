---
topic: errors.IDEMPOTENCY_CONFLICT
title: "IDEMPOTENCY_CONFLICT — duplicate request with conflicting payload"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
  - errors.TX_CONFLICT
---

# errors.IDEMPOTENCY_CONFLICT

## STATUS

**Not yet implemented.** The error code is reserved and documented here for the future contract, but no API handler currently reads the `Idempotency-Key` header or raises this error. Duplicate collection requests will currently pass through without detection. Tracked for implementation or removal.

## NAME

IDEMPOTENCY_CONFLICT — a request with the same idempotency key was already received but its payload differs from the original.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

The idempotency key is supplied via the `Idempotency-Key` HTTP header on collection create and update requests. See `crud` for the request shape.

The server has already processed (or is currently processing) a request with the same idempotency key, but the new request's body or parameters differ from the first one. Idempotency keys protect against duplicate submissions of identical requests; they do not allow modifying the request after the fact.

Not retryable with the same key and a different payload. A distinct operation requires a distinct idempotency key.

## SEE ALSO

- errors
- errors.CONFLICT
- errors.TX_CONFLICT
