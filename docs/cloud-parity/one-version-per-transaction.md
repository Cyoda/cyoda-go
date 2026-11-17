# One version per entity per transaction; commit-time unique keys; callback reads

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Callback reads return the current anchor

The entity a workflow runs for (the anchor) is saved into the transaction
before every callout that carries the transaction token. A callback read under
the token returns the anchor as the workflow has it now: the request's data,
the state changes so far and the data earlier processors returned. On a create
the anchor is found; during a processor it is in its source state. On
PostgreSQL the callout holds the anchor's row lock, so another writer of the
anchor, and another transaction that read it, wait (at the write or the commit)
until the workflow ends.

## A callout never writes the entity it runs for

A processor, a function criterion and a schedule function never write or delete
the anchor, with or without the token; a processor changes the anchor only
through the data it returns. Writing other entities through the token stays
supported, except an entity whose cascade is running in the same
transaction, at any depth: such a write starts that entity's workflow again
inside its own cascade and can loop without end. The engine does not detect a
breach and no longer adopts such a write.

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
  X. `/changes` and the audit history carry one entry per transaction.
- An auto-commit write is its own transaction and version. It ignores a
  caller-supplied transaction id and change type. A read of a pending version
  in the transaction carries the store-assigned metadata.
- An entity's model is immutable within a transaction, also after a
  same-transaction delete.

## Unique keys are checked at commit

At commit the store reads the unique keys of each saved entity's model (its own
stored copy, never keys a caller passes; the model's state is not checked),
releases the claims of every entity the transaction
wrote, computes the claims of the final entities and inserts them. Swaps,
chains of moves and free-after-claim commit. A value claimed twice, or held by
an entity outside the transaction, is `409 UNIQUE_VIOLATION`; a partial
composite key is `422 INVALID_UNIQUE_KEY`. A joined callback's duplicate or
partial key (from its processors' returned data) is found at the owner's
commit, not on the callback request; a partial key in the callback's own body
stays `422` at the callback. On PostgreSQL a deadlock (`40P01`) between two
crossing commits is a retryable `409 CONFLICT`. A scheduled fire that breaks a
key fails at commit like any run; the entity is unchanged.

## The model rules are the API's

Entities exist only in a `LOCKED` model; a model is unlocked only when it has
no entities; a `LOCKED` model is never deleted, re-imported or given new
unique keys. The services check these when a request starts: an entity create
answers `409 MODEL_NOT_LOCKED`, unlock counts the entities and answers `409
MODEL_HAS_ENTITIES`, and import, unique-key change and delete answer `409
MODEL_ALREADY_LOCKED`. Storage adds no check of its own. The rules hold when
model administration runs from one place and not while writes to the model are
in flight. `DELETE /model/...` no longer answers `MODEL_HAS_ENTITIES`: a model
with entities is `LOCKED`.

## Scheduled fire

A fire seals its transaction after its schedule functions run, so their
callbacks are admitted (before, `410`). A fire refused at commit on a unique key
records the doors' text (for example `UNIQUE_VIOLATION: ...`) in the
task's `lastError`; it is logged at warn level only and mints no ticket.

## What Cloud changes

Everything above, on every storage path: one version per entity per
transaction, commit-time unique-key checks reading the stored model's keys,
delete no longer answering `MODEL_HAS_ENTITIES`, and the no-self-write rule
for compute nodes.
