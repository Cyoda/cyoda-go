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

HTTP: `503` `Service Unavailable`. Retryable: `yes` when the request was not sent to the owner, `no` otherwise. The answer's `retryable` flag says which.

## DESCRIPTION

A transaction lives on the node that began it, and a request made under its token — a compute member's callback — is forwarded to that node, over HTTP or gRPC. This error answers the request when the forward fails. It is retryable only with proof that nothing was sent:

- **The request was not sent** (retryable). The owner is not alive — gossip marks it dead, or the token names a node the directory does not list — or the peer address guard refused its address, or the connection to it could not be opened (HTTP: the dial failed and no connection was held for the request; gRPC: the call failed before any of its headers went out). Nothing ran on the owner, so sending the same request again is safe. When the owner is in fact gone, the transaction is gone with it, and a retry meets the same answer.
- **The request may have reached the owner** (not retryable). The connection failed after it was opened, or the owner did not begin to answer within `CYODA_PROXY_TIMEOUT` (HTTP: its response headers; gRPC: a unary call's response, or a streamed call's first message). The owner may be alive and may have run the request: **the outcome of a write is unknown**. Do not assume it failed, and do not send it again: a retry could repeat a write the owner kept.

When the owner stays unreachable, the compute member reports its callout as failed; the operation the callout belongs to then fails, and the client that made that operation starts it again.

## SEE ALSO

- errors
- errors.TRANSACTION_EXPIRED
- errors.TRANSACTION_NOT_FOUND
