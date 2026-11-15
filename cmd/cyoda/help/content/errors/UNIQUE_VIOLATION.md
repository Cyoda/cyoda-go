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

Keys are checked when the transaction commits, on the entities as the transaction leaves them, on every storage backend. A value another entity of the same transaction frees — by being deleted or re-keyed — is free for this one, in either order; only the final entities count. Two entities of one transaction that end with the same value, or an entity that ends with a value an entity outside the transaction holds, refuse the commit with this code. When a request joined another operation's transaction (a compute member's callback), the clash is found at that operation's commit, which answers this code; the callback itself succeeds (see `cyoda help models`, the unique-keys operation).

## SEE ALSO

- errors
- errors.CONFLICT
- errors.INVALID_UNIQUE_KEY
