---
topic: errors.DISPATCH_TIMEOUT
title: "DISPATCH_TIMEOUT — dispatch to compute member timed out"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_FORWARD_FAILED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.CALLOUT_FAILED
  - grpc
---

# errors.DISPATCH_TIMEOUT

## NAME

DISPATCH_TIMEOUT — a compute member did not take, or did not answer, a processor, criterion or function callout within the callout's answer limit.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The answer limit is the callout's own `responseTimeoutMs` (a field on the processor, criterion or function config), or `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` when that is not set.

The error message names which of two phases ran out of time:

- **member not draining** — the compute member had stopped reading its stream, so the request could not even be handed to it. The work provably never left the node, and another compute member is tried, whatever the callout is.
- **no response** — the member took the request but did not answer in time. The work may have run. Another member is tried only for a criterion, a function, or a processor whose configuration declares `idempotent: true`.

A message that says **cut off at the callout deadline** is the same failure, caused by the limit on the time one callout may take as a whole rather than by one member's answer limit.

This code reaches the client when it was the only try made — `retryPolicy: NONE`, a processor that is not idempotent, or no other compute member to try. When more than one try failed the code is `errors.CALLOUT_FAILED`, which lists them.

A member that is not draining is evicted within `CYODA_KEEPALIVE_TIMEOUT` seconds.

Retryable. `retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor, a criterion, or a schedule function, the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. (An `ASYNC_NEW_TX` processor's failure never reaches the client: the operation continues.) A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile.

If timeouts recur, check compute member load and network latency. `CYODA_DISPATCH_WAIT_TIMEOUT` is how long a callout waits for a compute member to exist; it is not this limit — see `cyoda help config cluster`.

## SEE ALSO

- errors
- errors.DISPATCH_FORWARD_FAILED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.CALLOUT_FAILED
- grpc
