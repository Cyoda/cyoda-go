# Schema extensions commit on their own and never conflict

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A model with a `changeLevel` grows as entity writes bring new structure the
level permits: the write computes an additive delta and extends the model
before its own entity changes commit. This applies to every write that can
extend — entity create and update, collection create and update, and data a
workflow processor returns — over HTTP and gRPC.

- **Commits on its own.** The extension is committed in a transaction of its
  own, never in the write's transaction (owned or joined). When the extension
  call returns, it is durable, whatever then happens to the write.
- **Visible on return.** When the write returns `2xx`, the node that ran it
  serves the new schema. A write carrying a transaction token runs on the
  transaction's owner. Another node serves it once the model cache
  invalidation broadcast arrives, or when the cache lease
  (`CYODA_MODEL_CACHE_LEASE`) ends if the broadcast is lost. Others may see
  the extension before the write returns.
- **Idempotent.** An extension the schema already holds writes nothing; a
  retry of a request whose outcome is unknown is safe.
- **Never a conflict.** Two writes extending the same model never fail
  because of each other: no `409 CONFLICT`, no retry-budget error.
  Contention is a wait that lasts until the request ends.
- **Loss-free.** Every extension whose call returned is in the model's
  schema.
- **Locked models only.** An extension of a model that is not `LOCKED` is
  refused, answered `409 MODEL_NOT_LOCKED` (an existing code). A write
  reaches it only when the model is unlocked while the write is in progress,
  e.g. during a compute-node callout on an uncommitted entity.
- **Model reads ignore the transaction.** A model read returns the latest
  committed model whatever transaction the request holds, so a criterion
  later in the same request sees a field the request just added.

## Side effects clients see

1. A new field can be visible to other clients (model export, search path
   validation, criteria) before the write that brought it commits; a query
   on it finds nothing from that write until it commits.
2. A write that fails after its extension — a workflow or processor error,
   a unique-key violation, a conflict, a client cancel, a crash — leaves the
   field in the model. Removing it takes unlock (zero entities), delete and
   a fresh import.
3. An extension made by a joined compute-node callback stays if the owner's
   transaction rolls back, or if the callback's scope is discarded (a failed
   `ASYNC_NEW_TX` processor discards its callbacks' entity writes, not their
   extensions).
4. Writers that bring different types for one field leave the model
   admitting both.
5. A failed request may still have extended the model; retrying is safe.
6. On another node, until the broadcast arrives, the older schema is in
   use: a search or criterion on an unknown path reads the model once more
   and normally finds the field; a query that does not refuse can miss the
   new field or a widened type; a write, PATCH or validation there can be
   refused as if the field did not exist, or extend again (a no-op).
7. A `changeLevel` change on a locked model takes effect on each node as its
   model cache picks it up; set the level before locking.
8. A storage outage during the extension of processor-returned data answers
   the retryable `503 STORAGE_UNAVAILABLE`.

## No negative path cache

A search, conditional delete or criterion path check that names a path the
node's cached schema does not hold reads the model from storage once, then
refuses with `400 INVALID_FIELD_PATH` if the path is still unknown. No
refusal is remembered between requests.

## Alignment consequence for Cloud

Cloud must never answer a conflict or retry-exhaustion error because of a
concurrent extension of the same model, must keep an extension when the
write that made it fails, and must answer `409 MODEL_NOT_LOCKED` for an
extension of a model that is not locked.

## Test coverage (cyoda-go)

- SPI conformance: the `spitest` model-extension suite — visible on return,
  kept when the caller's transaction rolls back, `Get` ignoring the caller's
  transaction, a delta the schema holds writing nothing, concurrent
  extenders, `ErrModelNotLocked`, a missing model, an empty delta.
