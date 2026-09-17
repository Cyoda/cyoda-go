---
topic: errors.ENTITY_MODEL_MISMATCH
title: "ENTITY_MODEL_MISMATCH — a write disagreed with an entity's fixed model"
stability: stable
see_also:
  - errors
  - errors.ENTITY_NOT_FOUND
  - errors.ENTITY_MODIFIED
---

# errors.ENTITY_MODEL_MISMATCH

## NAME

ENTITY_MODEL_MISMATCH — a write named an entity ID that already exists under a different model name or model version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

An entity's model is fixed at creation: its model name and model version never change for the life of the entity ID, including across delete and recreate. A soft-deleted entity keeps its row and its history under that same model, so recreating it under a different model would be exactly the rewrite this invariant forbids. Every storage backend enforces this at the point of write and reports this code rather than silently rewriting which model the entity's history belongs to — the stored model always wins.

Allowing the rewrite would strand the entity's earlier-model version history: every point-in-time read issued under the original model would lose the entity entirely, with no error, the moment a later write moved it to a new model. That is the defect this code exists to prevent.

**This code is not a documented client-facing outcome of any current request.** No HTTP endpoint or gRPC request accepts a model on an entity update — creating an entity always mints a fresh entity ID, and updating one always targets the model the entity already has. There is today no way for an ordinary client request to disagree with an entity's own model, so this code should never appear in a real response.

If you see it anyway, that means some write path — a new endpoint, an internal job, a migration, direct storage-layer access — is constructing a write with a model different from the target entity's. **That is a defect to report** (with the entity ID from the `entityId` property and the request that produced it), not a condition for the caller to work around. The `entityId` property in the problem-detail body identifies the entity whose model the write disagreed with.

## RECOVERY

There is no client-side recovery: no legitimate request should ever produce this response. Report it as a bug, including the `entityId` and the operation that triggered it, so the write path responsible for supplying the wrong model can be identified and fixed.

## SEE ALSO

- errors
- errors.ENTITY_NOT_FOUND
- errors.ENTITY_MODIFIED
