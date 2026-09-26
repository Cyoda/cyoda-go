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
- **A scheduled task of the entity.** The scheduler changed one of the entity's scheduled tasks first. The scheduler changes a task when it claims it, stamps a segment of its run, records an attempt, marks it failed, or gives it back. A write that arms or cancels the entity's scheduled transitions writes those tasks, and so can race the scheduler.

Both are normal outcomes under concurrent load.

A race can also be lost before the commit, while the request is still running. The answer is still 409 — never a `500`, a `400 WORKFLOW_FAILED` or a `412 ENTITY_MODIFIED`:

- **A processor's callback write.** A processor's joined callback writes the entity, or one of its scheduled tasks, and loses the race. When the processor then fails, for any reason, the lost race is the answer, not the processor's failure. This holds for `SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX` and `COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch`, on every storage backend. Under `ASYNC_NEW_TX` the race stays lost when its savepoint is undone: the request answers 409 and nothing commits.
- **A transaction already aborted (PostgreSQL).** On PostgreSQL the statement that loses the race aborts the transaction, and every later statement in it is refused. Those refusals are the same lost race, and answer 409.
- **`If-Match`.** `412 ENTITY_MODIFIED` answers only the request's own precondition: the entity is no longer at the version the request names. When the transaction had already lost a race before the compare ran, the precondition was never evaluated, and the answer is 409.

How each operation handles a race with the scheduler:

- **Entity delete and workflow import, when the request owns its transaction.** The server retries up to 3 times before it answers 409. A workflow import that answers 409 has already saved its workflows; only the removal of the tasks of transitions no longer scheduled is left.
- **Batched delete (`transactionSize`).** The whole request does not answer 409 for a task race. A batch that still conflicts lists its ids in `idToError` with this code, and the other batches run.
- **Request that joined an open transaction (`X-Tx-Token`).** The server does not retry it. The request can answer 409 itself, and the transaction's owner then cannot commit.

Retry the whole read-modify-write cycle with the current entity state. Replaying the original write without re-reading produces stale data. A retried workflow import saves the same workflows again and then removes the tasks.

On the gRPC entity operations this error is `code` `CLIENT_ERROR`, with a message that starts with `CONFLICT:` and `retryable` true.

## SEE ALSO

- errors
- errors.TX_CONFLICT
- errors.IDEMPOTENCY_CONFLICT
- errors.EPOCH_MISMATCH
