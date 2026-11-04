---
topic: errors.TRANSACTION_NODE_UNAVAILABLE
title: "TRANSACTION_NODE_UNAVAILABLE — the node owning the transaction is unreachable"
stability: stable
see_also:
  - errors
  - errors.TRANSACTION_EXPIRED
  - errors.TRANSACTION_NOT_FOUND
---

# errors.TRANSACTION_NODE_UNAVAILABLE

## NAME

TRANSACTION_NODE_UNAVAILABLE — the cluster node that owns the open transaction is not alive or reachable.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

Transaction state is pinned to the node that opened it. If that node crashes or becomes unreachable while the transaction is in progress, subsequent requests using the transaction token are rejected with this error because the proxy cannot forward them to the owner.

Retryable by opening a new transaction. The in-progress transaction is likely lost. Mid-transaction node failure requires the client to restart the transaction from scratch.

## SEE ALSO

- errors
- errors.TRANSACTION_EXPIRED
- errors.TRANSACTION_NOT_FOUND
