---
topic: errors.CALLOUT_FAILED
title: "CALLOUT_FAILED — every try of a processor, criterion or function callout failed"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_TIMEOUT
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.DISPATCH_FORWARD_FAILED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - workflows
  - config.grpc
---

# errors.CALLOUT_FAILED

## NAME

CALLOUT_FAILED — a processor, criterion or function callout was tried on more than one compute member, and no try produced an answer.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

A callout is tried on one compute member after another: until one answers, until a failure forbids another try, or until the tries its `retryPolicy` allows are used (`NONE`: one try; `FIXED` or unset: one try plus `CYODA_RETRY_FIXED_NUM_RETRIES`). This code is returned when more than one try failed and nothing more could be done — every try was used, or no further compute member appeared within `CYODA_DISPATCH_WAIT_TIMEOUT`, or the time the callout may take ran out.

The message lists the failed tries:

```
CALLOUT_FAILED: the callout could not be completed, got 3 failures: [member<5f0c…>: DISPATCH_TIMEOUT: processor dispatch timed out after 30000ms: no response (2 times)], [member<->: DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed]
```

- The count is the number of failed tries. Identical entries are written once, with `(k times)`.
- `member<id>` is the id of the compute member that was tried — one of the caller's own tenant's connections, the id the member was given when it joined. `member<->` is a try handed over to another cluster node whose answer was lost, so that the member is not known.
- Each entry carries the error code the try would have had on its own.

When exactly one try was made and failed, that try's own code is returned instead (`errors.DISPATCH_TIMEOUT`, `errors.COMPUTE_MEMBER_DISCONNECTED`, `errors.DISPATCH_FORWARD_FAILED`), not this one. When no try could be made at all, the code is `errors.NO_COMPUTE_MEMBER_FOR_TAG`. A compute member that answered with a failure of its own is never replaced by another: that is `errors.WORKFLOW_FAILED`.

A callout reaches a second compute member only where that is safe for Cyoda's own state: always when the work provably never left the node, and after the work was handed to a member only for a criterion, a function, or a processor whose configuration declares `idempotent: true`.

Retryable, and `retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile.

## SEE ALSO

- errors
- errors.DISPATCH_TIMEOUT
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.DISPATCH_FORWARD_FAILED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- workflows
- config.grpc
