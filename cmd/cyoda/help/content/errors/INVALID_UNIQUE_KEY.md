---
topic: errors.INVALID_UNIQUE_KEY
title: "INVALID_UNIQUE_KEY — unique key fields are null or invalid"
stability: stable
see_also:
  - errors
  - errors.UNIQUE_VIOLATION
  - errors.INVALID_UNIQUE_KEY_DEFINITION
---

# errors.INVALID_UNIQUE_KEY

## NAME

INVALID_UNIQUE_KEY — the engine could not compute a complete composite unique key because one or more required key fields are null or carry an unsupported value type.

## SYNOPSIS

HTTP: `422` `Unprocessable Entity`. Retryable: `no`.

## DESCRIPTION

A composite unique key requires every declared field to be present and non-null. This error is returned when the entity payload is missing a value for at least one key field, or the value cannot be normalized to a valid claim (for example, an object or array, or a number beyond the allowed precision bound).

A write is checked for this before its data is checked against the model's schema, so a write refused with this error never extends the model (`cyoda help models`, SCHEMA EXTENSION). Data a workflow processor returns is checked when the entity is saved.

To resolve: ensure every field listed in the model's unique key definition has a valid, non-null value in the submitted entity payload.

## SEE ALSO

- errors
- errors.UNIQUE_VIOLATION
- errors.INVALID_UNIQUE_KEY_DEFINITION
