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

TRANSACTION_NODE_UNAVAILABLE — a request made under a transaction token could not be completed on the cluster node that owns the transaction: that node is not alive, cannot be reached, or did not begin to answer within `CYODA_PROXY_TIMEOUT`.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `no`.

## DESCRIPTION

A transaction lives on the node that began it, and a request made under its token — a compute member's callback — is forwarded to that node, over HTTP or gRPC. This error answers the request when the forward fails:

- **The owner is not alive** (gossip marks it dead, or the token names a node the directory does not list). The transaction is lost with its node.
- **The owner cannot be reached, or does not begin to answer within `CYODA_PROXY_TIMEOUT`** (HTTP: its response headers; gRPC: a unary call's response, or a streamed call's first message). The owner may be alive and may have run the request: **the outcome of a write is unknown**. Do not assume it failed.

Not retryable: the same request cannot be known to succeed on a fresh attempt. The compute member reports its callout as failed; the operation the callout belongs to then fails, and the client that made that operation starts it again.

## SEE ALSO

- errors
- errors.TRANSACTION_EXPIRED
- errors.TRANSACTION_NOT_FOUND
