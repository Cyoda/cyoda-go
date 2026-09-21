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

Access to a transaction's shared buffer / connection is serialised by a
**per-transaction exclusive gate** (a non-reentrant mutex keyed by tx id). The
gate covers **every** joined request, not only entity writes: a joined read, a
joined search, the model load and model extension of a joined create, and
every non-entity handler behind the callback door all take it for their whole
length too, from before the request's first store operation to when its
handler returns. Two joined requests at once — a read alongside a write, or two
reads — are otherwise unsafe on every backend: `concurrent map writes` on
memory and SQLite, `conn busy` on PostgreSQL.

The gate is also the place where a callback from a compute member that was
replaced is refused: the right to touch the transaction is checked again once
the gate is held, and whoever takes the callout's work over waits for the same
gate before proceeding. See `callout-failover.md` §6.

Every holder of that gate — the transaction owner **and** every joined callback — MUST
**release the gate for the duration of any blocking external dispatch** (every
processor dispatch — SYNC / ASYNC_SAME_TX / ASYNC_NEW_TX — and FUNCTION criterion
call-out) and re-acquire it before touching the buffer again. Where the dispatch
is bracketed by buffer work (e.g. an ASYNC_NEW_TX savepoint create/rollback), the
release spans only the call-out, not the surrounding buffer ops. The dispatch
window touches no local buffer but can re-enter with a
descendant joined callback on the same tx; holding the gate across it is a
hold-and-wait that deadlocks the transaction until the dispatch timeout.

This generalises the owner-side rule ("never hold the gate across the engine's
processor dispatch") to **every** gate holder. Releasing during a remote dispatch
is safe: no local buffer write is pending at the dispatch point (state and audit
writes happen only after the processor pipeline returns), so concurrent siblings
on the same tx stay consistent.

## Failure mode this prevents

Without the release-across-dispatch rule, a 2-deep same-transaction cascade
(a manual/automated transition whose SYNC processor drives a cross-entity
transition that *itself* has a SYNC processor) hangs for the dispatch timeout and
then fails `WORKFLOW_FAILED`. Callers were forced to break the join (run the
inner transition in its own transaction), sacrificing cross-entity atomicity.
