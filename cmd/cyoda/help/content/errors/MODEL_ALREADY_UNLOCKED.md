---
topic: errors.MODEL_ALREADY_UNLOCKED
title: "MODEL_ALREADY_UNLOCKED — model is already in the UNLOCKED state"
stability: stable
see_also:
  - errors
  - errors.MODEL_ALREADY_LOCKED
  - errors.MODEL_NOT_LOCKED
  - errors.CONFLICT
---

# errors.MODEL_ALREADY_UNLOCKED

## NAME

MODEL_ALREADY_UNLOCKED — an unlock-transition request was issued against a model that is already `UNLOCKED`. The expected pre-state was `LOCKED`.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

Returned when `PUT /model/{name}/{version}/unlock` is issued against a model whose current state is `UNLOCKED`. Distinct from `MODEL_NOT_LOCKED`, which is reserved for the entity-write-without-lock path on the entity service: this code is the symmetric counterpart of `MODEL_ALREADY_LOCKED` for the admin lifecycle.

The problem-detail body carries `entityName`, `entityVersion`, `expectedState` (always `LOCKED`), and `actualState` (always `UNLOCKED`) so callers can branch on the precondition without scraping the message string.

Not retryable. Lock the model first via `PUT /model/{name}/{version}/lock` if a subsequent unlock is required.

## SEE ALSO

- errors
- errors.MODEL_ALREADY_LOCKED
- errors.MODEL_NOT_LOCKED
- errors.CONFLICT
