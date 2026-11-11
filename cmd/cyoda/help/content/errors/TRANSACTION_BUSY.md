---
topic: errors.TRANSACTION_BUSY
title: "TRANSACTION_BUSY — another callback of this transaction is waiting for a callout of its own"
stability: stable
see_also:
  - errors
  - workflows
  - errors.TRANSACTION_ROLLBACK_ONLY
  - errors.TOO_MANY_JOINED_REQUESTS
---

# errors.TRANSACTION_BUSY

## NAME

TRANSACTION_BUSY — a callback was refused because another callback of the same transaction is paused in a callout of its own.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

## DESCRIPTION

On the in-tree backends (memory, SQLite, PostgreSQL) the callbacks of one transaction are served one at a time. A callback runs a write whose workflow may call out to a compute member again. While that inner callout is waiting for its answer, the callback is paused: it lets the transaction go, so that the callbacks of the inner callout can run, and only those. A sibling callback of the same transaction that arrives in that time is refused at once with this code, instead of waiting behind the paused one; so is a sibling already waiting when the pause begins. A callback that belongs to the paused callback's own callout is not a sibling and is admitted. A location-independent backend runs callbacks at the same time and never answers this code.

Nothing of the refused callback ran. Retryable: send it again once the paused callback has been answered.

The fix is in the compute node: send the callbacks of one transaction in series. Parallel callbacks gain nothing on a backend that serves one callback at a time, and on such a backend they only meet this refusal.

## SEE ALSO

- errors
- workflows
- errors.TRANSACTION_ROLLBACK_ONLY
- errors.TOO_MANY_JOINED_REQUESTS
