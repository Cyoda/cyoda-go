---
topic: errors.MODEL_HAS_ENTITIES
title: "MODEL_HAS_ENTITIES — unlock blocked because entities of this model exist"
stability: stable
see_also:
  - errors
  - errors.MODEL_ALREADY_LOCKED
  - errors.MODEL_ALREADY_UNLOCKED
  - errors.CONFLICT
---

# errors.MODEL_HAS_ENTITIES

## NAME

MODEL_HAS_ENTITIES — an unlock request was rejected because at least one entity of the target model still exists.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

`PUT /model/{name}/{version}/unlock` requires zero entities of the target model. The check guards against silent loss of data and against schema drift on a model whose lifecycle is presumed frozen. `DELETE /model/{name}/{version}` never answers this code: a model with entities is always `LOCKED`, and delete refuses a `LOCKED` model with `MODEL_ALREADY_LOCKED`.

The `entityCount` property in the problem-detail body reports how many entities currently exist for the model on the responding tenant.

Not retryable in the protocol sense. The caller must remove the offending entities (e.g. via condition-delete on the entity surface) and re-issue the lifecycle request.

The check counts committed entities, and it is not one step with the unlock: a write still in progress when the unlock runs is not seen, and nothing refuses it later. Stop writes to a model before you unlock it (see `cyoda help models`, LIFECYCLE).

## SEE ALSO

- errors
- errors.MODEL_ALREADY_LOCKED
- errors.MODEL_ALREADY_UNLOCKED
- errors.CONFLICT
