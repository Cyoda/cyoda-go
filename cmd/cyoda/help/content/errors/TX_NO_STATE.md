---
topic: errors.TX_NO_STATE
title: "TX_NO_STATE — no transaction state record found"
stability: stable
see_also:
  - errors
  - errors.TX_REQUIRED
  - errors.TX_COORDINATOR_NOT_CONFIGURED
  - errors.TRANSACTION_NOT_FOUND
---

# errors.TX_NO_STATE

## NAME

TX_NO_STATE — no state record exists for the referenced transaction.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Reserved. Not currently emitted by any cyoda-go code path.

In the current model, transaction outcomes are tracked in-memory on the owning
node for a bounded TTL window. There is no persistent two-phase-commit state
log tracking prepare/commit/abort phases. A transaction that has expired from
the in-memory window is reported as `TRANSACTION_NOT_FOUND`.

This error code is reserved for a future capability involving persistent
per-transaction state records. If you receive it from a current cyoda-go
release, it indicates an unexpected condition; raise a support ticket.

## SEE ALSO

- errors
- errors.TX_REQUIRED
- errors.TX_COORDINATOR_NOT_CONFIGURED
- errors.TRANSACTION_NOT_FOUND
