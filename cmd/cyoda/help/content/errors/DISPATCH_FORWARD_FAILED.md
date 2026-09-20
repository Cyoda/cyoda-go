---
topic: errors.DISPATCH_FORWARD_FAILED
title: "DISPATCH_FORWARD_FAILED — inter-node dispatch forwarding error"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_TIMEOUT
  - errors.COMPUTE_MEMBER_DISCONNECTED
---

# errors.DISPATCH_FORWARD_FAILED

## NAME

DISPATCH_FORWARD_FAILED — a callout was handed over to another node and the answer was lost.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The node that owns the transaction handed a processor, criterion or function callout to another node and did not get an answer it can trust: no reply within the time allowed, a connection that broke after it was opened, a status other than `2xx` (from the node itself or from anything between the two), or a reply that does not authenticate — which includes node clocks more than 30 seconds apart.

**The other node may have given the work to a compute node, and the compute node may have run it.** A node that could not be *connected to* does not produce this error — nothing left the owner, no try is counted, and the next node is asked. This error means the connection was opened and what happened afterwards is unknown. It therefore counts as one try. A criterion, a function, or a processor declared `idempotent` is then given to another compute node; any other processor stops here, the operation fails with this error, and its transaction is rolled back.

Retryable, as far as cyoda's own state is concerned — see `errors.DISPATCH_TIMEOUT` for what `retryable` does and does not promise about effects outside cyoda. Persistent occurrences point at the network between nodes, at a proxy or sidecar answering `502`–`504` for a node that is down, or at clock skew.

## SEE ALSO

- errors
- errors.DISPATCH_TIMEOUT
- errors.COMPUTE_MEMBER_DISCONNECTED
