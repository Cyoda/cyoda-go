---
topic: errors.TX_REQUIRED
title: "TX_REQUIRED — operation must be performed inside a transaction"
stability: stable
see_also:
  - errors
  - errors.TX_CONFLICT
---

# errors.TX_REQUIRED

## NAME

TX_REQUIRED — the requested operation can only be performed within an open transaction but no transaction context was provided.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Reserved. Not currently emitted by any cyoda-go code path.

This error code is reserved for future capabilities where certain operations
explicitly require an open transaction context. In the current model,
transactions are opened automatically per request and the caller does not
supply a pre-existing transaction ID to standard CRUD endpoints.

Not retryable without a transaction context.

## SEE ALSO

- errors
- errors.TX_CONFLICT
