---
topic: errors.COMMIT_IN_JOINED_TRANSACTION
title: "COMMIT_IN_JOINED_TRANSACTION — a callback reached a processor that would commit the transaction it joined"
stability: stable
see_also:
  - errors
  - workflows
  - errors.CALLOUT_SUPERSEDED
  - errors.WORKFLOW_FAILED
---

# errors.COMMIT_IN_JOINED_TRANSACTION

## NAME

COMMIT_IN_JOINED_TRANSACTION — a write made under a transaction token ran a workflow that reached a `COMMIT_BEFORE_DISPATCH` processor, and was refused.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

A compute member's callback — a request carrying a transaction token — runs in the transaction of the operation that called the member out. It joins that transaction; it does not own it. Only the operation that began a transaction commits it.

A `COMMIT_BEFORE_DISPATCH` processor commits the transaction it runs in before it is dispatched. When the workflow a callback's write runs — a create, an update or a transition, one entity or a collection — reaches such a processor, committing there would commit the calling operation's transaction part-way through that operation, without it knowing. The write is refused instead, before anything is written: the processor is not dispatched, nothing is flushed, and the joined transaction stays open and uncommitted. Both values of `startNewTxOnDispatch` are refused, because both commit the transaction first.

What happens to the joined transaction is the calling operation's decision, as with any failed callback: the member reports the failure in its answer, and the operation fails or carries on according to its processor's execution mode.

The message names the workflow and the processor. Not retryable: the same callback reaches the same processor again. Either change the processor's `executionMode` — `SYNC` runs it inside the joined transaction — or make the write outside the callback, without the transaction token, as an operation of its own. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

The import does not reject the combination: whether a workflow is reached from a callback is decided at run time.

## SEE ALSO

- errors
- workflows
- errors.CALLOUT_SUPERSEDED
- errors.WORKFLOW_FAILED
