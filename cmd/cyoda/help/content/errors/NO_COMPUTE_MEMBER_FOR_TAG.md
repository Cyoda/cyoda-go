---
topic: errors.NO_COMPUTE_MEMBER_FOR_TAG
title: "NO_COMPUTE_MEMBER_FOR_TAG — no compute member registered for the required tag"
stability: stable
see_also:
  - errors
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.DISPATCH_TIMEOUT
  - errors.CALLOUT_FAILED
---

# errors.NO_COMPUTE_MEMBER_FOR_TAG

## NAME

NO_COMPUTE_MEMBER_FOR_TAG — no compute member for the required tag and tenant appeared, on any cluster node, within the time a callout waits for one.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

A processor, criterion or function callout goes to a compute member of the caller's tenant that declares at least one of the callout's `calculationNodesTags`. When the node that owns the request has none, and no other cluster node advertises one that can be reached, the callout waits for one to appear — for `CYODA_DISPATCH_WAIT_TIMEOUT` in total (default `5s`; `0` disables waiting), on a single node as in a cluster, whatever the callout's `retryPolicy`. The wait ends the moment a member attaches. If none does, the operation is rejected with this error.

No try was made: nothing was sent to any compute member, and the operation's transaction was rolled back. When a callout did make tries and then ran out of members, the error reports the tries instead (`errors.CALLOUT_FAILED`, or the single try's own code).

Retryable after compute capacity is restored.

## SEE ALSO

- errors
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.DISPATCH_TIMEOUT
- errors.CALLOUT_FAILED
