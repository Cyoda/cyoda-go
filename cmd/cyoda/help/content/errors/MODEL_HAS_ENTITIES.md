---
topic: errors.MODEL_HAS_ENTITIES
title: "MODEL_HAS_ENTITIES — operation blocked because entities of this model exist"
stability: stable
see_also:
  - errors
  - errors.MODEL_ALREADY_LOCKED
  - errors.MODEL_ALREADY_UNLOCKED
  - errors.CONFLICT
---

# errors.MODEL_HAS_ENTITIES

## NAME

MODEL_HAS_ENTITIES — an unlock or delete request was rejected because at least one entity of the target model still exists.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

Both `POST /model/{name}/{version}/unlock` and `DELETE /model/{name}/{version}` require zero entities of the target model. The cardinality check guards against silent loss of data and against schema drift on a model whose lifecycle is presumed frozen.

The `entityCount` property in the problem-detail body reports how many entities currently exist for the model on the responding tenant.

Not retryable in the protocol sense. The caller must remove the offending entities (e.g. via condition-delete on the entity surface) and re-issue the lifecycle request.

The unlock check counts committed entities and is not atomic with the unlock: a write still in progress when the unlock runs can commit afterwards, leaving an `UNLOCKED` model with entities, which delete then refuses with this code. Stop writes to a model before unlocking it (see `cyoda help models`).

## SEE ALSO

- errors
- errors.MODEL_ALREADY_LOCKED
- errors.MODEL_ALREADY_UNLOCKED
- errors.CONFLICT
