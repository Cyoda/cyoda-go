---
topic: errors.DISPATCH_TIMEOUT
title: "DISPATCH_TIMEOUT — dispatch to compute member timed out"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_FORWARD_FAILED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - grpc
---

# errors.DISPATCH_TIMEOUT

## NAME

DISPATCH_TIMEOUT — the dispatcher waited longer than the configured timeout for a compute member to accept and complete a task.

## SYNOPSIS

HTTP: `503` `Service Unavailable`.

## DESCRIPTION

A workflow processor, criteria evaluation, or function callout was dispatched to a compute member but the response did not arrive within the callout's own `responseTimeoutMs` (a field on the processor, criteria, or function config; default `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, `30000` ms) — not any cluster forwarding timeout.

The error message names which of two phases timed out:

- **member not draining** — the compute member had stopped reading its stream, so the request could not even be handed to it.
- **no response** — the member took the request but did not answer in time.

A member that is not draining is evicted within `CYODA_KEEPALIVE_TIMEOUT` seconds; dispatches to it after that return `errors.COMPUTE_MEMBER_DISCONNECTED` and route to another member instead.

Retryable. Completion on the remote node is not guaranteed; retries must be idempotent or carry an idempotency key.

If timeouts recur, check compute member load and network latency. `CYODA_DISPATCH_WAIT_TIMEOUT` and `CYODA_DISPATCH_FORWARD_TIMEOUT` govern cross-node forwarding between cluster nodes, not this timeout — see `cyoda help config cluster`.

## SEE ALSO

- errors
- errors.DISPATCH_FORWARD_FAILED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- errors.COMPUTE_MEMBER_DISCONNECTED
- grpc
