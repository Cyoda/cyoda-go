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

A `COMMIT_BEFORE_DISPATCH` processor commits the transaction it runs in before it is dispatched. When the workflow a callback's write runs — a create, an update or a transition, one entity or a collection — reaches such a processor, committing there would commit the calling operation's transaction part-way through that operation, without it knowing. The write is refused instead, at that processor: the processor is not dispatched, the joined transaction is neither flushed nor committed, and it stays open. What the refused workflow had already done in it — the audit events it recorded, and the effects of any earlier processor or transition, including their own callouts — stays in the joined transaction for the calling operation to keep or discard. Both values of `startNewTxOnDispatch` are refused, because both commit the transaction first.

What happens to the joined transaction is the calling operation's decision, as with any failed callback: the member reports the failure in its answer, and the operation fails or carries on according to its processor's execution mode.

The message names the workflow and the processor. Not retryable: the same callback reaches the same processor again. The fix is in the compute node: make that write as an independent request, without the transaction token, so it runs in a transaction of its own — the `COMMIT_BEFORE_DISPATCH` processor then commits that transaction, as designed — and let the calling processor handle the write's outcome as the business requires. Changing the inner processor's `executionMode` to `SYNC` also removes the refusal, but changes what that workflow promises (its pre-callout state is no longer durable before the callout), so do it only if that promise is not needed. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

The import does not reject the combination: whether a workflow is reached from a callback is decided at run time.

## SEE ALSO

- errors
- workflows
- errors.CALLOUT_SUPERSEDED
- errors.WORKFLOW_FAILED
