# One version per entity per transaction; commit-time unique keys; callback reads

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows. Cloud ticket: CP-3997.

## Callback reads return the current entity

Before every callout that carries the transaction token, other than a `COMMIT_BEFORE_DISPATCH` processor's
(its first segment has just committed the entity), the engine reads the
entity the workflow runs for in the open transaction. It writes the entity into
that transaction (not committed) when a read of it in the transaction finds nothing (a create), or
when the stored data bytes or state differ from the engine's copy; a change to
the metadata alone does not count. On PostgreSQL the stored data is the JSONB
rendering of what was written, so data the engine took from a request or a
processor usually differs in its bytes and is written again. The write commits
or rolls back with the transaction: at the end of the operation that began the
transaction, or, when a `COMMIT_BEFORE_DISPATCH` processor follows, at that processor's
segment boundary. A later write of the entity in the same transaction replaces
the pending version, so the transaction records one version of the entity. A
callback read under the token returns the entity as the workflow has it now:
the request's data, the state changes so far and the data earlier processors
returned. On a create the entity is found; during a processor it is in its
source state. On PostgreSQL the transaction holds the entity's row lock from
its first write of the entity until the transaction ends, or until a rollback
to a savepoint undoes that write, so another writer of
the entity, and another transaction that read it, wait (at the write or the
commit) until then. A transaction that has not written the entity holds no row
lock on it while a callout runs: a manual transition that sends no data, for example, has not
written it before its first processor's callout.

## A callout payload carries the callout's transaction id

While a workflow runs, the entity carries the id of the transaction it runs
in: the request's, a scheduled fire's, or after a `COMMIT_BEFORE_DISPATCH`
boundary the new transaction's. A callout that carries a `transactionId`
carries the same id in `payload.meta.transactionId`, whether or not the
transaction has written the entity yet, and a `lifecycle` criterion on
`transactionId` sees that id. A callout without a `transactionId` (a
`COMMIT_BEFORE_DISPATCH` processor with `startNewTxOnDispatch: false`, or a
function criterion evaluated for a read) carries the `transactionId` of the
stored version it describes: the version committed before the callout, or the
version the read returned. A callback read under the token returns the stored entity,
so before the transaction has written the entity its `meta.transactionId` is
the last committed version's.

## A callout never writes the entity it runs for

A processor, a function criterion and a schedule function never write or delete
the entity they run for, with or without the token; a processor changes that
entity only through the data it returns. That holds also for a change a nested
processor (of another entity a callback creates or updates) works out for the
entity: it is passed back up and returned by the processor the entity's
transition called, never written to the entity. Writing other entities through
the token stays supported, except an entity whose cascade is running in the same transaction, at any
depth: the entity is passing from one state to the next, so such a write moves
it out from under the running transition, and it starts that entity's workflow
again inside its own cascade, which can loop without end. The engine no longer
adopts such a write.

## One version per entity per transaction

- Every backend records at most one version of an entity per transaction. A
  later write of an entity the transaction already wrote replaces the pending
  version. The version has the last write's content and attribution, change
  type `DELETED` if the last write is a delete, else `CREATED` for the entity's
  first version and `UPDATED` otherwise, and the previous version number + 1.
- Create then delete in one transaction records one `DELETED` tombstone.
  Delete then recreate records one version.
- A savepoint rollback restores each entity's pending version to what it was at
  the savepoint, including "none". A savepoint rollback of a create frees the
  id's model.
- `GET /entity/{id}?transactionId=X` returns X's final entity and reads
  committed versions only: `404 ENTITY_NOT_FOUND` before X commits, also inside
  X. `/changes` and the `EntityChange` audit events carry one entry per
  transaction.
- An auto-commit write is its own transaction and version. It ignores a
  caller-supplied transaction id and change type. A read of a pending version
  in the transaction carries the store-assigned metadata.
- An entity's model is immutable within a transaction, also after a
  same-transaction delete.

## Unique keys are checked at commit

At commit the store reads the unique-key definitions of the model of each
entity whose last write in the transaction is not a delete (its own stored copy, never definitions a caller passes; the model's
state is not checked),
releases the claims of every entity the transaction
wrote, computes the claims of the final entities and inserts them. Swaps,
chains of moves and free-after-claim commit. A value claimed twice, or held by
an entity outside the transaction, is `409 UNIQUE_VIOLATION`; a partial
composite key is `422 INVALID_UNIQUE_KEY`. A joined callback's duplicate or
partial key (from its processors' returned data) is found at the owner's
commit, not on the callback request; a partial key in the callback's own body
stays `422` at the callback. On PostgreSQL a deadlock (`40P01`) between two
crossing commits is a retryable `409 CONFLICT`. A scheduled fire that breaks a
key fails at commit like any run: nothing the refused transaction wrote is kept,
and a segment an earlier `COMMIT_BEFORE_DISPATCH` boundary committed stays
committed.

## The model rules are the API's

An entity is created only in a `LOCKED` model; a model is unlocked only when it
has no entities; a `LOCKED` model is never deleted, re-imported or given new
unique-key definitions. The services check these when a request starts: an entity create
answers `409 MODEL_NOT_LOCKED`, unlock counts the entities and answers `409
MODEL_HAS_ENTITIES`, and import, unique-key change and delete answer `409
MODEL_ALREADY_LOCKED`. Storage adds no check of its own. Model administration
runs from one place, not while writes to the model are in flight. `DELETE /model/...` no
longer answers `MODEL_HAS_ENTITIES`: a model with entities is `LOCKED`, and
delete answers `MODEL_ALREADY_LOCKED`.

## Scheduled fire

A fire seals its transaction after its schedule functions run, so their
callbacks are admitted (before, `410`). A fire refused at commit on a unique key
records the doors' text (for example `UNIQUE_VIOLATION: ...`) in the
task's `lastError`; it is logged at warn level only and mints no ticket.

## What Cloud changes

Everything above, on every storage path: the callout payload's
`meta.transactionId` rule, one version per entity per
transaction, commit-time unique-key checks reading the stored model's
unique-key definitions,
delete no longer answering `MODEL_HAS_ENTITIES`, and the no-self-write rule
for compute nodes.
