---
topic: errors.COMPOSITE_KEY_UNSUPPORTED
title: "COMPOSITE_KEY_UNSUPPORTED — backend does not support composite unique keys"
stability: stable
see_also:
  - errors
  - errors.UNIQUE_VIOLATION
  - errors.INVALID_UNIQUE_KEY_DEFINITION
---

# errors.COMPOSITE_KEY_UNSUPPORTED

## NAME

COMPOSITE_KEY_UNSUPPORTED — the active storage backend does not support composite unique key enforcement.

## SYNOPSIS

HTTP: `422` `Unprocessable Entity`. Retryable: `no`.

## DESCRIPTION

Composite unique key enforcement is an optional capability that storage backends may or may not implement. This error is returned when a model defines one or more unique keys but the backend does not implement the `CompositeUniqueKeyCapable` interface.

To resolve: use a backend that supports composite unique keys, or remove the unique key definitions from the model.

## SEE ALSO

- errors
- errors.UNIQUE_VIOLATION
- errors.INVALID_UNIQUE_KEY_DEFINITION
