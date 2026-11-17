---
topic: errors.MODEL_NOT_LOCKED
title: "MODEL_NOT_LOCKED — model must be locked before entities are created in it"
stability: stable
see_also:
  - errors
  - errors.MODEL_NOT_FOUND
  - errors.CONFLICT
  - models
---

# errors.MODEL_NOT_LOCKED

## NAME

MODEL_NOT_LOCKED — the entity model exists but is not `LOCKED`, which is required before entities of that type can be created.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

A model is `UNLOCKED` or `LOCKED`, and an entity is created only in a `LOCKED` model. An entity create or collection create, over HTTP or gRPC, whose model is not `LOCKED` is refused with this code before anything is written. In a collection create, one item whose model is not `LOCKED` refuses the whole request.

A node that reads the model as not `LOCKED` reads it again from storage before it answers this code, so a model locked a moment ago is not refused because the node has not yet seen the lock. See `cyoda help models` (LIFECYCLE).

Not retryable as is: lock the model (`PUT /api/model/{entityName}/{modelVersion}/lock`) and send the create again.

## SEE ALSO

- errors
- errors.MODEL_NOT_FOUND
- errors.CONFLICT
- models
