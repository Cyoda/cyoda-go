---
topic: errors.MODEL_NOT_FOUND
title: "MODEL_NOT_FOUND — entity model does not exist"
stability: stable
see_also:
  - errors
  - crud
  - search
  - errors.ENTITY_NOT_FOUND
  - errors.MODEL_NOT_LOCKED
---

# errors.MODEL_NOT_FOUND

## NAME

MODEL_NOT_FOUND — the referenced entity model (schema) does not exist.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

The entity type or model name specified in the request does not exist in the tenant's model registry. Occurs on write paths (creating entities with an unknown type, importing data that references a missing model, performing model lifecycle transitions on a model ID that does not exist) and on read paths (list, stats, grouped-stats, and search operations that reference an unregistered model).

Not retryable. Register the model before issuing any operation that references it.

## SEE ALSO

- errors
- errors.ENTITY_NOT_FOUND
- errors.MODEL_NOT_LOCKED
