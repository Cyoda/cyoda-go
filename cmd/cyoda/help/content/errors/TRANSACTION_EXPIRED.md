---
topic: errors.TRANSACTION_EXPIRED
title: "TRANSACTION_EXPIRED — transaction token has expired"
stability: stable
see_also:
  - errors
  - errors.TRANSACTION_NOT_FOUND
  - errors.TRANSACTION_NODE_UNAVAILABLE
---

# errors.TRANSACTION_EXPIRED

## NAME

TRANSACTION_EXPIRED — the transaction token presented to the proxy is past its expiry time.

## SYNOPSIS

HTTP: `410` `Gone`. Retryable: `no`.

## DESCRIPTION

Transaction tokens are short-lived bearer tokens issued when a transaction is opened. This error fires when the token's `exp` claim is in the past at the time the proxy validates it. The transaction itself may still be active server-side, but the token is no longer valid for routing.

Not retryable with the same token. The original transaction must be committed or rolled back before opening a new one.

## SEE ALSO

- errors
- errors.TRANSACTION_NOT_FOUND
- errors.TRANSACTION_NODE_UNAVAILABLE
