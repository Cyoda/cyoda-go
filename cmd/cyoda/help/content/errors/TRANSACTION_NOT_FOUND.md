---
topic: errors.TRANSACTION_NOT_FOUND
title: "TRANSACTION_NOT_FOUND — transaction ID does not exist"
stability: stable
see_also:
  - errors
  - errors.TRANSACTION_EXPIRED
  - errors.TRANSACTION_NODE_UNAVAILABLE
  - errors.CALLOUT_SUPERSEDED
---

# errors.TRANSACTION_NOT_FOUND

## NAME

TRANSACTION_NOT_FOUND — no transaction with the given ID exists on this node.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

The transaction ID supplied in the request does not correspond to an active transaction. The transaction may have been committed, rolled back, expired, or may never have existed. Also occurs when a request is mis-routed to a node that never opened the transaction.

Not retryable. Transaction state (committed, rolled back, expired) determines whether the transaction existed.

## SEE ALSO

- errors
- errors.TRANSACTION_EXPIRED
- errors.TRANSACTION_NODE_UNAVAILABLE
- errors.CALLOUT_SUPERSEDED
