---
topic: errors.UNIQUE_VIOLATION
title: "UNIQUE_VIOLATION — composite unique key constraint violated"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
  - errors.INVALID_UNIQUE_KEY
---

# errors.UNIQUE_VIOLATION

## NAME

UNIQUE_VIOLATION — a write was rejected because it would create a duplicate entry for a declared composite unique key.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

The entity payload contains field values that collide with an existing entity's composite unique key. Unlike an optimistic-concurrency CONFLICT (which is retryable), a unique-key violation is a permanent data constraint — retrying the same payload without changing the key field values will produce the same result.

To resolve: change the values of the fields that form the unique key so they no longer duplicate an existing entity.

On write-time backends (PostgreSQL) this can also fire within one transaction when a value is claimed before its holder is freed; free-before-claim is portable (see `cyoda help models`).

## SEE ALSO

- errors
- errors.CONFLICT
- errors.INVALID_UNIQUE_KEY
