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

On storage backends that enforce uniqueness at write time (PostgreSQL), this error can also occur **within a single transaction** when a value is claimed before its current holder is freed — for example a workflow that creates or re-keys an entity onto a value before deleting the entity that still holds it. Free the value first (delete or update the holder), then claim it. Backends that validate at commit (the in-memory and SQLite engines) accept either order; for portable behavior, always free before claiming. See the unique-keys section of `cyoda help models` for details.

## SEE ALSO

- errors
- errors.CONFLICT
- errors.INVALID_UNIQUE_KEY
