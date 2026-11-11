---
topic: errors.TRANSACTION_ROLLBACK_ONLY
title: "TRANSACTION_ROLLBACK_ONLY — the transaction, or the part of it a request would run in, can only be rolled back"
stability: stable
see_also:
  - errors
  - workflows
  - errors.TRANSACTION_BUSY
  - errors.COMMIT_IN_JOINED_TRANSACTION
---

# errors.TRANSACTION_ROLLBACK_ONLY

## NAME

TRANSACTION_ROLLBACK_ONLY — a write that joined a transaction failed, and what that transaction holds can no longer be kept.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

## DESCRIPTION

A compute member's callback — a request carrying a transaction token — joins the transaction of the operation that called the member out. When a write made that way fails with an answer that retrying cannot change, what that write and its workflow had already done in the transaction is discarded, and the transaction is marked: it can only be rolled back.

Three requests meet this code.

- The operation that began the transaction (the owner), when it tries to commit, or to write, in the marked transaction. The message names the code the failed callback was answered with. The owner's operation fails and is rolled back; nothing is committed.
- A request that joins the transaction under a part that is marked. It is refused at once, reads included, because what it would read or write belongs to a part that can no longer be kept.
- A callback whose own write called a compute member out, when a callback of that inner callout failed a write. Its part is marked: it keeps nothing, and the mark goes on up to the owner.

The mark also stops the callout the failed write was made under before its next try, and the owner then ends that callout. So a compute member that carries on under the same token after a failed write sees this code only until then. After it, its callbacks are answered `410 CALLOUT_SUPERSEDED` while the transaction is open, and `404 TRANSACTION_NOT_FOUND` once the owner has rolled it back.

Under an `ASYNC_NEW_TX` processor the mark stops at that processor: the processor fails, everything its callbacks wrote is discarded, and the operation goes on. When the transaction has also lost a write race, the owner gets the retryable `409 CONFLICT` instead of this code: on a new transaction the failed write may succeed.

Not retryable: sending the same request again meets the same mark. A callback that failed with a retryable answer does not mark the transaction; the transaction goes on.

The fix is in the cause. Read the code the failed callback was answered with, correct the request the compute node sent, and run the whole operation again from its start as a new transaction.

## SEE ALSO

- errors
- workflows
- errors.TRANSACTION_BUSY
- errors.COMMIT_IN_JOINED_TRANSACTION
