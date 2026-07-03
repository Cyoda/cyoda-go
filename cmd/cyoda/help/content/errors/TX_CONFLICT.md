---
topic: errors.TX_CONFLICT
title: "TX_CONFLICT — transaction-level conflict"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
  - errors.EPOCH_MISMATCH
  - errors.IDEMPOTENCY_CONFLICT
---

# errors.TX_CONFLICT

## NAME

TX_CONFLICT — the operation was aborted because of a transaction-level conflict.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `yes`.

## DESCRIPTION

Reserved. Not currently emitted by any cyoda-go code path.

Storage-layer serialization failures (e.g., PostgreSQL `40001`/`40P01`) surface
as `CONFLICT` (not `TX_CONFLICT`) in the current implementation. `TX_CONFLICT`
is reserved to distinguish a future transaction-level conflict signal from the
existing entity-level `CONFLICT` code — for example, a conflict detected at
the transaction boundary rather than at an individual entity write.

Retryable. The full transaction — including any reads performed inside it —
must be restarted from the beginning.

## SEE ALSO

- errors
- errors.CONFLICT
- errors.EPOCH_MISMATCH
- errors.IDEMPOTENCY_CONFLICT
