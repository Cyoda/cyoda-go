---
topic: errors.CONFLICT
title: "CONFLICT — another write committed first"
stability: stable
see_also:
  - errors
  - errors.TX_CONFLICT
  - errors.IDEMPOTENCY_CONFLICT
  - errors.EPOCH_MISMATCH
---

# errors.CONFLICT

## NAME

CONFLICT — the write lost a race: another transaction committed a change to the same entity, or to one of its scheduled tasks, after this write began.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `yes`.

## DESCRIPTION

When a write commits, the server checks that nothing it wrote was changed by another transaction that committed after it began. Two kinds of change are checked:

- **The entity.** Another client or a workflow changed the entity first.
- **A scheduled task of the entity.** The scheduler changed one of the entity's scheduled tasks first. The scheduler changes a task when it claims it, records an attempt, marks it failed, or gives it back. A write that arms or cancels the entity's scheduled transitions writes those tasks, and so can race the scheduler.

Both are normal outcomes under concurrent load.

How each operation handles a race with the scheduler:

- **Entity delete and workflow import.** The server retries up to 3 times before it answers 409.
- **Batched delete (`transactionSize`).** The whole request does not answer 409 for a task race. A batch that still conflicts lists its ids in `idToError` with this code, and the other batches run.
- **Request that joined an open transaction (`X-Tx-Token`).** The server does not retry it. The transaction's owner gets the conflict.

Retry the whole read-modify-write cycle with the current entity state. Replaying the original write without re-reading produces stale data. A retried workflow import saves the same workflows again and then removes the tasks.

On the gRPC entity operations this error is `code` `CLIENT_ERROR`, with a message that starts with `CONFLICT:` and `retryable` true.

## SEE ALSO

- errors
- errors.TX_CONFLICT
- errors.IDEMPOTENCY_CONFLICT
- errors.EPOCH_MISMATCH
