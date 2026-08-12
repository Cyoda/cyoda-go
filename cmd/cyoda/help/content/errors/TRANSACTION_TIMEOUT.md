---
topic: errors.TRANSACTION_TIMEOUT
title: "TRANSACTION_TIMEOUT — transaction timeout expired before commit"
stability: stable
see_also:
  - errors
  - errors.TRANSACTION_EXPIRED
  - crud
---

# errors.TRANSACTION_TIMEOUT

## NAME

TRANSACTION_TIMEOUT — the client-supplied `transactionTimeoutMillis` expired before the first commit.

## SYNOPSIS

HTTP: `408` `Request Timeout`. Retryable: `yes`.

## DESCRIPTION

Fires when the client-supplied `transactionTimeoutMillis` elapses before the first commit. Nothing was committed.

On multi-commit operations (chunked collections, commit-before-dispatch workflows) the timeout applies only until the first commit — failures after that surface through the per-chunk contract instead.

## SEE ALSO

- errors
- errors.TRANSACTION_EXPIRED
- crud
