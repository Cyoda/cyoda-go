---
topic: errors.DISPATCH_FORWARD_FAILED
title: "DISPATCH_FORWARD_FAILED — inter-node dispatch forwarding error"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_TIMEOUT
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.CALLOUT_FAILED
---

# errors.DISPATCH_FORWARD_FAILED

## NAME

DISPATCH_FORWARD_FAILED — a callout was handed over to another cluster node and no usable answer came back.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The node that owns a request hands a callout over to another cluster node when that node has a matching compute member. This code means the connection to that node was opened and then no usable answer arrived: no reply within the owner's wait, a broken connection, a non-`2xx` status, or an answer that does not authenticate. **The work may have been given to a compute member there, and may have run.** A node that cannot be connected to at all is not this error — the next node is asked, and no try is used.

Because the work may have run, the callout moves on to another node only if it is a criterion, a function, or a processor whose configuration declares `idempotent: true`. This code reaches the client when the lost answer was the only try made; when more than one try failed the code is `errors.CALLOUT_FAILED`, where a lost answer appears as `member<->`.

The message never names the other node or its address.

Retryable. `retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor, a criterion, or a schedule function, the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. (An `ASYNC_NEW_TX` processor's failure never reaches the client: the operation continues.) A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile. Persistent failures indicate inter-node network or peer node health issues; pnode clocks more than 30 s apart show up as this error, too.

## SEE ALSO

- errors
- errors.DISPATCH_TIMEOUT
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.CALLOUT_FAILED
