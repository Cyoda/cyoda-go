---
topic: errors.MODEL_ALREADY_LOCKED
title: "MODEL_ALREADY_LOCKED — model is already in the LOCKED state"
stability: stable
see_also:
  - errors
  - errors.MODEL_NOT_FOUND
  - errors.MODEL_NOT_LOCKED
  - errors.CONFLICT
---

# errors.MODEL_ALREADY_LOCKED

## NAME

MODEL_ALREADY_LOCKED — an admin operation requested the model be in the `UNLOCKED` state, but the model is already `LOCKED`.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

Returned by any admin operation that requires the model be in the `UNLOCKED` state, including:

- `DELETE /model/{name}/{version}` — deleting a `LOCKED` model is rejected; unlock first.
- `POST /model/{name}/{version}/lock` — the relock is rejected because the model is already locked.
- `PUT /model/{name}/{version}/unique-keys` — setting unique keys on a `LOCKED` model is rejected.
- Re-importing a model whose existing descriptor is already `LOCKED`.

The problem-detail body carries `entityName` and `entityVersion` on every emit; the relock and delete branches additionally set `expectedState` (always `UNLOCKED`) and `actualState` (always `LOCKED`) so callers can branch on the precondition without scraping the message string.

Not retryable. To proceed, either accept the existing lock or unlock the model first via `POST /model/{name}/{version}/unlock`.

## SEE ALSO

- errors
- errors.MODEL_NOT_FOUND
- errors.MODEL_NOT_LOCKED
- errors.CONFLICT
