# A callback never commits the transaction it joined

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A compute node's callback — a request carrying a transaction token — runs in
the transaction of the operation that called the compute node out. It joins
that transaction; it does not own it. **Only the operation that began a
transaction commits it.** No step a callback causes may commit the joined
transaction, directly or through a workflow the callback's write runs.

The one workflow step that commits the transaction it runs in is a
`COMMIT_BEFORE_DISPATCH` processor (both values of `startNewTxOnDispatch`
commit first). When the workflow a callback's write runs reaches one, the
write is refused:

- **When:** at the processor, before the segment is flushed, before any
  commit, and before the processor is dispatched. Earlier processors and
  earlier automated transitions of the same workflow have run — their callouts
  were sent. Their effects, and the audit events the refused workflow recorded
  (its start, the processing pause, and a failed result for the refused
  processor), are discarded with the refused write: a callback is
  all-or-nothing (`callback-scopes.md`).
- **Answer:** `409`, error code `COMMIT_IN_JOINED_TRANSACTION`, not retryable.
  The detail names the workflow and the processor. Over gRPC the code is the
  message prefix of the `CLIENT_ERROR` envelope.
- **Doors:** every entity write a callback can make that runs the workflow
  engine. On HTTP: create, create collection, update (loopback or transition,
  PUT or PATCH), update collection. On gRPC: `EntityManage` (create, update,
  patch, transition) and `EntityManageCollection` (create, update).
- **Joined transaction afterwards:** open, uncommitted, and rollback-only:
  the refusal is a failed write that is not retryable. The owner fails with
  `409 TRANSACTION_ROLLBACK_ONLY`; under an `ASYNC_NEW_TX` processor that
  processor fails instead and the owner goes on.
- **Batch update isolation:** the refusal is never isolated as a per-item
  precondition failure, even when the item carries an `ifMatch`; the whole
  request fails.
- **Remedy the error points to:** the compute node makes that write as an
  independent request, without the transaction token, so the
  `COMMIT_BEFORE_DISPATCH` processor commits a transaction of its own.
- **Import:** not rejected. Whether a workflow is reached from a callback is a
  run-time fact.

## Cloud today

Cloud has no `COMMIT_BEFORE_DISPATCH` execution mode (no occurrence of
`COMMIT_BEFORE_DISPATCH` or `startNewTxOnDispatch` in the Cloud tree), so the
refusal has nothing to guard there yet. When Cloud adopts the mode it adopts
this refusal with it, with the same status, code and timing. The general rule
— a joined request never commits its owner's transaction — applies to every
other commit path Cloud has.
