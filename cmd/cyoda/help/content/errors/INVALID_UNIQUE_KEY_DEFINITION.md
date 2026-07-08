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

Each unique key on a model must have a non-empty list of field paths, a unique name within the model, and field paths that resolve to existing, non-ambiguous fields in the model schema. This error is returned when a submitted model definition violates one of these structural requirements.

To resolve: correct the unique key definition — ensure every key has a name, at least one field path, no duplicate key names within the model, and that all referenced field paths exist in the model schema.

## SEE ALSO

- errors
- errors.INVALID_UNIQUE_KEY
- errors.COMPOSITE_KEY_UNSUPPORTED
