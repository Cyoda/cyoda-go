---
topic: errors.EPOCH_MISMATCH
title: "EPOCH_MISMATCH — the storage shard changed owner during the transaction"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
---

# errors.EPOCH_MISMATCH

## NAME

EPOCH_MISMATCH — the storage backend refused the transaction because the shard it ran on moved to another owner while it ran.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `yes`.

## DESCRIPTION

A storage backend that partitions its data into shards, each owned by one process at a time, refuses a transaction whose shard changes owner before the transaction completes. The backend rolls the transaction back, so nothing of it is applied, and the same request can succeed on a fresh attempt.

The stock storage backends (`memory`, `sqlite`, `postgres`) do not partition data into owned shards and never answer this code.

Retryable. Frequent recurrence means that shard ownership in the storage backend moves often, for example during repeated node restarts.

## SEE ALSO

- errors
- errors.CONFLICT
