---
topic: errors.CONFLICT
title: "CONFLICT — another write committed first"
stability: stable
see_also:
  - errors
  - errors.EPOCH_MISMATCH
  - auth.clients
---

# errors.CONFLICT

## NAME

CONFLICT — the write lost a race: another transaction committed a change to the same entity, or to one of its scheduled tasks, after this write began; or, on `PUT /clients/{clientId}/secret`, another reset of the same client won.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `yes`, except on a request that joined a transaction (below).

## DESCRIPTION

When a write commits, the server checks that nothing it wrote was changed by another transaction that committed after it began. Two kinds of change are checked:

- **The entity.** Another client or a workflow changed the entity first.
- **A scheduled task of the entity.** The scheduler changed one of the entity's scheduled tasks first. The scheduler changes a task when it claims it, stamps a segment of its run, records an attempt, marks it failed, or gives it back. A write that arms or cancels the entity's scheduled transitions writes those tasks, and so can race the scheduler.

Both are normal outcomes under concurrent load.

On PostgreSQL two transactions that commit at the same time can meet PostgreSQL's deadlock detection when one of them claims a unique-key value that the other frees in the same commit. One of them is answered this retryable 409; the other commits, or is answered `409 UNIQUE_VIOLATION` if a value it claims is still held. A retry is checked against the committed claims. Commits that only claim the same values, and free none of them, do not deadlock: claims are inserted in a fixed order, so the later commit waits for the earlier one.

A schema extension never causes this answer. Two writes that extend the same model both succeed: each extension commits on its own and never conflicts with another (`cyoda help models`, SCHEMA EXTENSION).

A race can also be lost before the commit, while the request is still running. The answer is still 409 — never a `500`, a `400 WORKFLOW_FAILED` or a `412 ENTITY_MODIFIED`:

- **A processor's callback write.** A processor's joined callback writes another entity, or a scheduled task, and loses the race. The request answers 409, with or without `If-Match`. When the processor then fails, for any reason, the lost race is the answer, not the processor's failure. This holds for `SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX` and `COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch`, on every storage backend. Under `ASYNC_NEW_TX` the race stays lost when its savepoint is undone: the request answers 409 and the request's transaction does not commit.
- **The engine's write before a callout.** Before each callout that carries the transaction token, the engine writes the entity the workflow runs for into the open transaction (not committed) when a read of it in the transaction finds nothing (a create) or the stored data or state differ from the engine's copy. When another transaction committed a change to that entity after the request read it, the request answers 409 on every backend. On PostgreSQL the write itself is refused; on memory and SQLite the write is kept in the transaction and its commit is refused. A collection request fails whole, and the item is not listed as a per-item failure.
- **A transaction already aborted (PostgreSQL).** On PostgreSQL the statement that loses the race aborts the transaction, and every later statement in it is refused. Those refusals are the same lost race, and answer 409.
- **`If-Match`.** `If-Match` states the version the request starts from, and the server checks it once, when the request's transition starts. `412 ENTITY_MODIFIED` answers only that check: the entity was already at another version when the request read it. A write to the entity later in the same transaction, such as the engine's write of the entity before a callout, is the request's own and does not break it. A change that another transaction commits after the request's read is a lost race, and answers 409. The one exception is described in `errors.ENTITY_MODIFIED`: a change made during the callout of a `COMMIT_BEFORE_DISPATCH` processor, after the request's first segment committed.

How each operation handles a race with the scheduler:

- **Entity delete, when the request owns its transaction, and workflow import, which always does.** The server retries up to 3 times before it answers 409. A workflow import that answers 409 has already saved its workflows; only the removal of the tasks of transitions no longer scheduled is left.
- **Batched delete (`transactionSize`).** The whole request does not answer 409 for a task race. A batch that still conflicts lists its other ids in `idToError` with this code (an entity gone or changed keeps its own entry), and the other batches run.
- **A request that joined an open transaction (`X-Tx-Token`).** The server does not retry it. When the transaction has lost a race, a joined request that fails answers 409 with `retryable` false, and keeps nothing but a schema extension it made: a retry inside the same transaction cannot succeed. The callback is not what is retried — the operation that owns the transaction answers the retryable 409, and its client runs it again as a new transaction. A workflow import never joins one: it is refused with `400 MODEL_ADMIN_IN_JOINED_TRANSACTION` if it carries the token.
- **Two callbacks of one transaction (location-independent backends only).** A storage plugin that supplies the transaction coordination itself may run the callbacks of one transaction at the same time. Of two that wrote the same entity from the same version, one keeps nothing but a schema extension it made and answers 409 with `retryable` true: send it again in the same transaction. The in-tree backends serve the callbacks of one transaction one at a time and never give this answer.

**A client secret reset (`PUT /clients/{clientId}/secret`).** Of two resets of one client at the same moment, on one node or on two, exactly one wins; the other answers 409 and changes nothing. A reset also answers 409 when the client was deleted and created again under the same id while the reset ran. Retryable: reset again if you still want a new secret. See `auth.clients` (STORAGE AND CONSISTENCY).

Retry the whole read-modify-write cycle with the current entity state. Replaying the original write without re-reading produces stale data. A retried workflow import saves the same workflows again and then removes the tasks.

On the gRPC entity operations this error is `code` `CLIENT_ERROR`, with a message that starts with `CONFLICT:` and `retryable` true.

## SEE ALSO

- errors
- errors.EPOCH_MISMATCH
- auth.clients
