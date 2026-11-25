# Callback scopes: each callback all-or-nothing

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A compute node's callback — a request carrying a transaction token — joins the
transaction of the operation that called the compute node out.

- **All-or-nothing.** Each callback runs in a scope of its own. Either all its
  writes become part of the transaction, or none do. A `2xx` is sent only for a
  callback whose writes were kept; any error answer means none were.
- **A schema extension is not a callback write.** It commits on its own and
  stays whatever happens to the callback, its scope or the transaction
  (`schema-extensions.md`). Every "kept", "discarded" and "undone" in this
  file is about the callback's other writes.
- **Kept is relative to the scope above.** A kept callback is still undone with
  the part of the transaction above it: the transaction's rollback, or a failed
  `ASYNC_NEW_TX` processor the callback ran under.
- **A failed callback leaves the decision to the compute node.** It keeps
  nothing (except a read that answered a `4xx` that is not retryable,
  below), and the transaction goes on unless the transaction itself can no
  longer commit: a lost write race, an ended session (table below), or a
  failed discard (last point here).
  - After a retryable answer the compute node may send the callback again
    under the same token.
  - An answer that is not retryable is the compute node's to handle: its
    processor, function criterion or schedule function decides whether to go
    on without that callback or to fail. The operation then does what it does
    with any answer of that processor: a failed `SYNC` processor fails it, a
    failed `ASYNC_NEW_TX` processor does not.
  - When the outcome of a write is unknown — a `503
    TRANSACTION_NODE_UNAVAILABLE` that is not retryable (the request may have
    reached the node that holds the transaction), or no answer at all — the
    compute node cannot tell whether the write was kept, so it must fail the
    callout.
  - A failed callback whose scope cannot be discarded is answered `500
    SERVER_ERROR`, not retryable, and the transaction can then no longer
    commit: its writes may still be part of it.
- **A failed read** that answered a `4xx` that is not retryable keeps its scope
  (its reads stay in the transaction's commit-time validation). A read that
  failed with a retryable answer or a `5xx` keeps nothing.
- **Requests outside the transaction** — messages, async search jobs and their
  cancellation, and the administration routes (clients, signing and trusted
  keys, the platform clock) — are admitted and refused under a token as any
  callback is, but their effect is not in the transaction: they keep their own
  answer. On PostgreSQL a message saved under a token is now
  written outside the transaction, as on memory and SQLite.
- **The owner's commit and rollback** refuse every later callback: with `410
  CALLOUT_SUPERSEDED` while the transaction is still open, and with `404
  TRANSACTION_NOT_FOUND` once it has ended. A callback still in progress is never kept after them:
  a serialising backend waits until it has ended first; a location-independent
  backend does not wait, and refuses its completion (`410
  CALLOUT_SUPERSEDED`, nothing kept but a schema extension). The outcome is the same either way.

## Answers that change

A schema extension a callback made stays in every row.

| Answer | Before | After |
|---|---|---|
| A callback still in progress when its callout advanced (the work went to another compute node) or ended | its own answer; writes kept | `410 CALLOUT_SUPERSEDED`; writes discarded |
| A failed callback, retryable or not | its own answer; the owner could commit what it had staged | its own answer; nothing kept, except that a read answered with a non-retryable `4xx` keeps what it read; the transaction goes on |
| A callback in a transaction that lost a write race | `409 CONFLICT`, retryable | `409 CONFLICT`, **not** retryable (the owner keeps the retryable `409`) |
| A callback whose PostgreSQL session ended under it (connection lost, idle-in-transaction ceiling) | `503 STORAGE_UNAVAILABLE`, retryable | `404 TRANSACTION_NOT_FOUND`, not retryable (the owner keeps the retryable `503`) |
| `503 TRANSACTION_NODE_UNAVAILABLE`, the request was not sent to the owner (not live, address refused, connection not opened) | not retryable | **retryable** |
| `503 TRANSACTION_NODE_UNAVAILABLE`, the request may have reached the owner (failure after the connection opened, `CYODA_PROXY_TIMEOUT`) | not retryable | not retryable: the outcome of a write is unknown |
| A callback write that reaches a `COMMIT_BEFORE_DISPATCH` processor | `409 COMMIT_IN_JOINED_TRANSACTION`; what the workflow did before stayed in the transaction | the same answer; nothing kept; the transaction goes on |
| A joined server-streaming gRPC call that fails | the frames produced before the error, then the error | the error alone |

## New error code

- `TRANSACTION_BUSY` — `503`, retryable. A callback refused while another
  callback of the same transaction is paused in a callout of its own (below).

## Concurrency of callbacks

Callbacks of one transaction share it. How they run depends on the storage
backend; the outcome of a given sequence of callbacks does not.

- **Serialising backends** (memory, SQLite, PostgreSQL). The callbacks of one
  transaction are served one at a time, in the order they arrive, and a
  callback's writes, with those of the callbacks nested under it, stay
  contiguous. A callback whose write runs a workflow with a callout of its own
  is paused while that callout is out; only that callout's callbacks are
  admitted then. Any other callback of the transaction is refused at once with
  `503 TRANSACTION_BUSY`, retryable, having run nothing; one already queued is
  refused when the pause begins. Compute nodes should send the callbacks of one
  transaction in series.
- **Location-independent backends.** A storage plugin may supply the
  transaction's coordination itself through an optional SPI capability. Its
  transactions are location-independent: a callback is served by the node it
  reaches, and the callbacks of one transaction may run at the same time. Of
  two callbacks that wrote the same entity from the same version, one keeps
  nothing and is answered `409 CONFLICT`, retryable: it may be sent again in
  the same transaction. A serialising backend never gives this answer. No
  in-tree backend supplies the capability.

## What Cloud changes

Cloud's callbacks are separate transactions today, so the scope rules have no
direct counterpart there. When Cloud joins callbacks to the calling operation's
transaction, it adopts this contract whole: all-or-nothing callbacks, a
failed callback that leaves the transaction to go on, the `ASYNC_NEW_TX`
boundary, and the answers in the table above. Whether it then serialises the
callbacks of one transaction or runs them at the same time decides which of the
two concurrency rules above it follows: `TRANSACTION_BUSY` for the first, the
retryable `409 CONFLICT` between two callbacks for the second.

## Changed in 0.9.1

In 0.9.0 a callback write that failed with an answer that was not retryable
made the transaction rollback-only: the owner failed with `409
TRANSACTION_ROLLBACK_ONLY` whatever the compute node answered, later callbacks
in the transaction were refused with that code, and under an `ASYNC_NEW_TX`
processor the processor failed instead and the operation went on. 0.9.1
removes the rule and the
error code. A failed callback keeps nothing in both versions; in 0.9.1 the
transaction goes on and the compute node's processor decides. Cloud builds
neither the rule nor the code.
