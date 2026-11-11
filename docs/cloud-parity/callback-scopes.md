# Callback scopes: each callback all-or-nothing

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A compute node's callback — a request carrying a transaction token — joins the
transaction of the operation that called the compute node out.

- **All-or-nothing.** Each callback runs in a scope of its own. Either all its
  writes become part of the transaction, or none do. A `2xx` is sent only for a
  callback whose writes were kept; any error answer means none were.
- **Kept is relative to the scope above.** A kept callback is still undone with
  the part of the transaction above it: the transaction's rollback, or a failed
  `ASYNC_NEW_TX` processor the callback ran under.
- **A retryable failure** keeps nothing and leaves the transaction usable. The
  compute node may send the callback again under the same token.
- **A failed write that is not retryable** keeps nothing and makes the part of
  the transaction it ran in rollback-only:
  - The callout the write was made under is stopped.
  - The operation that owns the transaction fails with `409
    TRANSACTION_ROLLBACK_ONLY`, not retryable, whatever the compute node
    answers. The message names the code the failed callback was answered with.
    Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.
  - When the transaction has also lost a write race, the owner gets the
    retryable `409 CONFLICT` instead: on a new transaction the failed write may
    succeed.
  - Under an `ASYNC_NEW_TX` processor the mark stops at the processor. The
    processor fails (non-fatal, recorded in its `STATE_PROCESS_RESULT`), its
    scope is discarded with everything its callbacks wrote, and the operation
    continues.
  - A later callback under the marked part is refused before it runs, reads
    included, with `409 TRANSACTION_ROLLBACK_ONLY` — or `410
    CALLOUT_SUPERSEDED` once the owner has ended the stopped callout.
- **A failed read** that answered a `4xx` keeps its scope (its reads stay in the
  transaction's commit-time validation) and marks nothing. A read that failed
  with a `5xx` keeps nothing and marks nothing.
- **Requests outside the transaction** — messages, async search jobs and their
  cancellation, and the administration routes (clients, signing and trusted
  keys, the platform clock) — are admitted and refused under a token as any
  callback is, but their effect is not in the transaction: they keep their own
  answer and never mark. On PostgreSQL a message saved under a token is now
  written outside the transaction, as on memory and SQLite.
- **The owner's commit and rollback** refuse every later callback (`410
  CALLOUT_SUPERSEDED`). A callback still in progress is never kept after them:
  a serialising backend waits until it has ended first; a location-independent
  backend does not wait, and refuses its completion (`410
  CALLOUT_SUPERSEDED`, nothing kept). The outcome is the same either way.

## Answers that change

| Answer | Before | After |
|---|---|---|
| A callback still in progress when its callout advanced (the work went to another compute node) or ended | its own answer; writes kept | `410 CALLOUT_SUPERSEDED`; writes discarded |
| A failed callback write, not retryable | its own answer; the owner could commit what it had staged | its own answer; nothing kept; owner `409 TRANSACTION_ROLLBACK_ONLY` |
| A failed callback, retryable | its own answer; the owner could commit what it had staged | its own answer; nothing kept; the transaction goes on |
| A callback in a transaction that lost a write race | `409 CONFLICT`, retryable | `409 CONFLICT`, **not** retryable (the owner keeps the retryable `409`) |
| A callback whose PostgreSQL session ended under it (connection lost, idle-in-transaction ceiling) | `503 STORAGE_UNAVAILABLE`, retryable | `404 TRANSACTION_NOT_FOUND`, not retryable (the owner keeps the retryable `503`) |
| `503 TRANSACTION_NODE_UNAVAILABLE`, the request was not sent to the owner (not live, address refused, connection not opened) | not retryable | **retryable** |
| `503 TRANSACTION_NODE_UNAVAILABLE`, the request may have reached the owner (failure after the connection opened, `CYODA_PROXY_TIMEOUT`) | not retryable | not retryable: the outcome of a write is unknown |
| A callback write that reaches a `COMMIT_BEFORE_DISPATCH` processor | `409 COMMIT_IN_JOINED_TRANSACTION`; what the workflow did before stayed in the transaction | the same answer; nothing kept; owner `409 TRANSACTION_ROLLBACK_ONLY` |
| A joined server-streaming gRPC call that fails | the frames produced before the error, then the error | the error alone |

## New error codes

- `TRANSACTION_ROLLBACK_ONLY` — `409`, not retryable. To the owner: it may not
  commit. To a callback: it may not run.
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
transaction, it adopts this contract whole: all-or-nothing callbacks, the
rollback-only rule with its `409 TRANSACTION_ROLLBACK_ONLY`, the `ASYNC_NEW_TX`
boundary, and the answers in the table above. Whether it then serialises the
callbacks of one transaction or runs them at the same time decides which of the
two concurrency rules above it follows: `TRANSACTION_BUSY` for the first, the
retryable `409 CONFLICT` between two callbacks for the second.
