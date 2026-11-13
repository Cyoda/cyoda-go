---
topic: errors.MODEL_NOT_LOCKED
title: "MODEL_NOT_LOCKED — model must be locked before writing entities or extending its schema"
stability: stable
see_also:
  - errors
  - errors.MODEL_NOT_FOUND
  - errors.CONFLICT
  - models
---

# errors.MODEL_NOT_LOCKED

## NAME

MODEL_NOT_LOCKED — the entity model exists but is not `LOCKED`, which is required before entities of that type can be written or the model's schema extended.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

A model is `UNLOCKED` or `LOCKED`. Entities are written only against a `LOCKED` model, and only a `LOCKED` model is extended. Two cases answer this code:

- **The write names an unlocked model.** An entity create or collection create whose model is `UNLOCKED` is refused before anything is written.
- **The model was unlocked during the write.** A write that extends the model's schema (its `changeLevel` permits a field the model does not have) commits the extension before its own entity changes. If the model was unlocked after the write read it and before the extension commits, the extension is refused and the write answers this code. None of the write's entity changes is kept; an extension the same write made earlier (for example, by an earlier processor of its transition) stays, as every extension does. Unlocking needs zero committed entities, so this happens only when a model is unlocked while a write to it is in progress — for example during the compute-node callout of a transition on an entity that is not committed yet. It applies to every write that can extend: entity create and update, collection create and update, and data a workflow processor returns, over HTTP and gRPC.

This is a model-lifecycle conflict, not a conflict between writes: two writes that extend the same model never answer it, nor `409 CONFLICT`. See `cyoda help models` (SCHEMA EXTENSION).

Not retryable as is: lock the model (`PUT /api/model/{entityName}/{modelVersion}/lock`) and send the write again.

## SEE ALSO

- errors
- errors.MODEL_NOT_FOUND
- errors.CONFLICT
- models
