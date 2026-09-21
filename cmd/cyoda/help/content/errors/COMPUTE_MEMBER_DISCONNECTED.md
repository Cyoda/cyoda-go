---
topic: errors.COMPUTE_MEMBER_DISCONNECTED
title: "COMPUTE_MEMBER_DISCONNECTED — compute member dropped from the cluster"
stability: stable
see_also:
  - errors
  - errors.CLUSTER_NODE_NOT_REGISTERED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.DISPATCH_TIMEOUT
  - errors.CALLOUT_FAILED
  - grpc
---

# errors.COMPUTE_MEMBER_DISCONNECTED

## NAME

COMPUTE_MEMBER_DISCONNECTED — the compute member tried for a processor, criterion or function callout went away.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The compute member chosen for a callout disconnected — before it was handed the work, or after it and before it answered. In the second case the work may or may not have been carried out.

The server also raises this code when it evicts a member itself — after `CYODA_KEEPALIVE_TIMEOUT` seconds of inbound silence, or when one write to the member has stalled that long — and every callout in flight on that member fails with this code. It is also the outcome when a member leaves in the instant between being chosen and being handed the request.

Before the hand-off, another compute member is always tried. After it, another member is tried only for a criterion, a function, or a processor whose configuration declares `idempotent: true`. This code reaches the client when it was the only try made; when more than one try failed the code is `errors.CALLOUT_FAILED`.

Retryable. `retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor, a criterion, or a schedule function, the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. (An `ASYNC_NEW_TX` processor's failure never reaches the client: the operation continues.) A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile. Persistent failures indicate insufficient compute capacity for the required tags.

## SEE ALSO

- errors
- errors.CLUSTER_NODE_NOT_REGISTERED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- errors.DISPATCH_TIMEOUT
- errors.CALLOUT_FAILED
- grpc
