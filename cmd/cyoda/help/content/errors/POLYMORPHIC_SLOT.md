---
topic: errors.POLYMORPHIC_SLOT
title: "POLYMORPHIC_SLOT — the write proposes a new kind at a declared path"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
  - errors.VALIDATION_FAILED
  - models
---

# errors.POLYMORPHIC_SLOT

## NAME

POLYMORPHIC_SLOT — the payload would give a declared path a kind the model does not have there, and the schema extension cannot record that.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised on a write against a model with a `changeLevel` set, where the write also proposes a schema change. The payload holds an object, an array, or a scalar at a path the model declares as one of the other two — for example an array where the model declares a scalar. Recording that would make the path a polymorphic slot, which the extension path does not create at any level, `STRUCTURAL` included.

Raising the `changeLevel` does not help: this is not a change-level violation. Send the kind the path declares, or re-establish the model from sample data that covers both kinds while it is `UNLOCKED` — a model can declare more than one kind for a path, and then admits all of them.

On a model with no `changeLevel` the same payload is a plain validation failure (`BAD_REQUEST`, `expected scalar, got array`): nothing is being proposed, the value simply does not fit.

## SEE ALSO

- errors
- errors.BAD_REQUEST
- errors.VALIDATION_FAILED
- models
