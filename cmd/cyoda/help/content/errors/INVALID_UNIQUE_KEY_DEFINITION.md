---
topic: errors.INVALID_UNIQUE_KEY_DEFINITION
title: "INVALID_UNIQUE_KEY_DEFINITION — unique key definition is structurally invalid"
stability: stable
see_also:
  - errors
  - errors.INVALID_UNIQUE_KEY
  - errors.COMPOSITE_KEY_UNSUPPORTED
---

# errors.INVALID_UNIQUE_KEY_DEFINITION

## NAME

INVALID_UNIQUE_KEY_DEFINITION — a model's unique key definition is structurally invalid and cannot be used.

## SYNOPSIS

HTTP: `422` `Unprocessable Entity`. Retryable: `no`.

## DESCRIPTION

Each unique key on a model must have a non-empty list of field paths, a unique name within the model, and field paths that resolve to existing, non-ambiguous fields in the model schema. This error is returned by model administration only: declaring unique keys (`PUT /api/model/{entityName}/{modelVersion}/unique-keys`) that violate one of these requirements, or importing sample data into an `UNLOCKED` model whose merge would put an object or array at a keyed field. An entity write is never answered with it: a write that puts a non-scalar value in a keyed field is refused with `INVALID_UNIQUE_KEY` before it extends the model, and processor-returned data that would do so fails the transition with `400 WORKFLOW_FAILED` (`cyoda help models`).

To resolve: correct the unique key definition — ensure every key has a name, at least one field path, no duplicate key names within the model, and that all referenced field paths exist in the model schema as scalar fields — or import sample data that keeps every keyed field scalar.

## SEE ALSO

- errors
- errors.INVALID_UNIQUE_KEY
- errors.COMPOSITE_KEY_UNSUPPORTED
