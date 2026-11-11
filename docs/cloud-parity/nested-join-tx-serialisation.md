# Nested joined-transaction serialisation — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's transaction-join model. cyoda-go is the authoritative implementation.

## Behaviour

A compute-node callback that runs inside a transaction `T` (a routed
processor/criterion dispatch that joined `T` via its tx-token) may itself drive
a further transition on `T` whose own SYNC processor drives yet another joined
write on `T`. This **depth-2+ cascade in a single transaction** must complete and
commit atomically — the whole nested chain succeeds or rolls back together.

## Invariant Cloud must uphold

On a backend whose transaction lives in one node's memory or connection
(memory, SQLite, PostgreSQL), access to a transaction's shared buffer /
connection is serialised by a **per-transaction exclusive gate** keyed by tx
id. The gate covers **every** joined request, not only entity writes: a joined
read, a joined search, the model load and model extension of a joined create,
and every non-entity handler behind the callback door all hold it from before
the request's first store operation until the request's scope has ended. Two
joined requests at once — a read alongside a write, or two reads — are
otherwise unsafe on every such backend: `concurrent map writes` on memory and
SQLite, `conn busy` on PostgreSQL. The owner's own chain does not take the
gate: while a callout of the owner is out, the owner is blocked in it, and it
seals the transaction — waits for the callbacks in progress and refuses every
later one — before its final writes and its commit or rollback. See
`callback-scopes.md`.

The right to touch the transaction is checked when the gate is taken and again
when the request's scope ends; whoever takes the callout's work over waits
until the earlier member's requests in progress have ended, and those are
discarded. See `callout-failover.md` §6.

Every joined holder of the gate MUST **give the gate up for the duration of
any blocking external dispatch** (every processor dispatch — SYNC /
ASYNC_SAME_TX / ASYNC_NEW_TX — and FUNCTION criterion and schedule-function
call-out) and take it back before touching the buffer again. Where the dispatch
is bracketed by buffer work (e.g. an ASYNC_NEW_TX savepoint create/rollback),
the release spans only the call-out, not the surrounding buffer ops. The
dispatch window touches no local buffer but can re-enter with a descendant
joined callback on the same tx; holding the gate across it is a hold-and-wait
that deadlocks the transaction until the dispatch timeout.

While a holder is paused in its dispatch, the gate admits **only that
dispatch's callbacks** (its descendants). A sibling callback of the same
transaction is refused at once with `503 TRANSACTION_BUSY`, retryable, and one
already queued is refused when the pause begins. This keeps each callback's
writes, with those nested under it, contiguous, so that undoing one callback's
savepoint undoes exactly its own work and its descendants', never a sibling's.
The paused holder takes the gate back in its turn, without a cap, and is
never refused.

Releasing during a remote dispatch is safe because only the dispatch's own
callbacks can run while it is out. The workflow does write before a dispatch —
the engine records the start of the state machine (`STATE_MACHINE_START`) and
the processing pause (`PAUSE_FOR_PROCESSING`) before it dispatches the
processor — but those writes
belong to the paused holder's scope, below every write its descendants make,
and no sibling can interleave with them.

## Failure mode this prevents

Without the release-across-dispatch rule, a 2-deep same-transaction cascade
(a manual/automated transition whose SYNC processor drives a cross-entity
transition that *itself* has a SYNC processor) hangs for the dispatch timeout and
then fails `WORKFLOW_FAILED`. Callers were forced to break the join (run the
inner transition in its own transaction), sacrificing cross-entity atomicity.
