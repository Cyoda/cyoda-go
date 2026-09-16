---
topic: errors.ENTITY_MODEL_MISMATCH
title: "ENTITY_MODEL_MISMATCH — a save targeted an existing entity under a different model"
stability: stable
see_also:
  - errors
  - errors.ENTITY_NOT_FOUND
  - errors.ENTITY_MODIFIED
---

# errors.ENTITY_MODEL_MISMATCH

## NAME

ENTITY_MODEL_MISMATCH — a save named an entity ID that already exists under a different model name or model version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

An entity's model is fixed at creation: its model name and model version never change for the life of the entity ID, including across delete and recreate. A save that names an existing entity ID but declares a different model is rejected rather than silently rewriting which model the entity's history belongs to — the stored model always wins.

Allowing the rewrite would strand the entity's earlier-model version history: every point-in-time read issued under the original model would lose the entity entirely, with no error, the moment a later write moved it to a new model.

The `entityId` property in the problem-detail body identifies the entity whose model the save disagreed with.

## RECOVERY

1. **Write to the entity's own model.** Read the entity (`GET /api/entity/{entityId}`) to confirm its current `model.name` / `model.version`, and submit the update against that model rather than the one the failed request declared.
2. **Or create a new entity under the intended model.** If the write is meant to move the data to a different model, create a fresh entity there — with a new entity ID — instead of reusing the old one.

A retry with the same model mismatch will fail again; the request itself has to change.

## SEE ALSO

- errors
- errors.ENTITY_NOT_FOUND
- errors.ENTITY_MODIFIED
