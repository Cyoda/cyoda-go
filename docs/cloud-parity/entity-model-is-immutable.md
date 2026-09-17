# An entity's model reference is immutable — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go. cyoda-go is the authoritative implementation.

It exists for one reason: the change that introduced this rule is otherwise
a bug fix, but it adds a **new wire error code** that Cloud mirrors.

## The contract

**An entity's model reference — its model name and model version — is fixed
when the entity is created and never changes for the life of the entity ID.**
That includes across a delete and recreate: a soft-deleted entity keeps its
row and its history under the same model, so recreating it under a different
model would be exactly the rewrite this rule forbids.

A write that names a model different from the target entity's stored one is
**rejected**; the stored model always wins. It is never accepted with the
reference rewritten, and never accepted with the incoming model ignored.

Enforcement belongs at the point of write in the storage layer — for a
buffered backend, at the point the write is buffered, not deferred to
flush or commit — so that an in-transaction sequence cannot slip past it.

## The error

| Endpoint | Code | Status | When |
|---|---|---|---|
| `POST /entity/{entityName}/{modelVersion}` and the collection / update / patch forms | `ENTITY_MODEL_MISMATCH` | `400` | the save targets an existing entity whose stored model reference differs |

Not retryable. The problem-detail body carries the offending entity's id in
`properties.entityId`.

**No current client request can produce it.** Creating an entity mints a
fresh entity ID, and every update path copies the stored entity's model
reference onto the entity being saved, so nothing a client can send
disagrees with an entity's own model. The code is defence in depth against a
future endpoint, an internal job, a migration or direct storage access — if
it is ever observed, that is a defect in the write path that produced it,
not a condition for a caller to work around.

## Why it is a correctness rule, not hygiene

Allowing the rewrite strands the entity's earlier-model history. A
point-in-time read issued under the original model loses the entity
entirely — silently, with no error — the moment a later write moves it to a
different model, because the read selects by model and the entity's rows no
longer answer under the model they were written with.

It is also load-bearing for how a point-in-time read may be executed. A
backend that resolves a snapshot by enumerating entities and probing each
one's revision (which is what cyoda-go's PostgreSQL backend does) selects
the model on the entity, and is equivalent to a revision-walking form only
because the two can never disagree.

## What a backend must do

1. Compare the incoming model reference against the stored entity's before
   applying a write, and reject a mismatch rather than applying it.
2. Return an error that the engine can classify — in cyoda-go's SPI,
   one satisfying `errors.Is(err, spi.ErrEntityModelMismatch)` — so the
   request answers `400 ENTITY_MODEL_MISMATCH` and not a `500`.
3. Apply the rule on every write entry point that can reach an existing
   entity, including `CompareAndSave` where it does not delegate to `Save`,
   and including the same-transaction create-then-delete-then-recreate
   sequence, where a naive implementation finds no committed row to compare
   against.

The SPI conformance suite (`spitest`) pins all three, so a backend that has
not adopted them fails conformance on its next dependency update rather
than diverging silently.
