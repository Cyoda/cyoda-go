---
topic: errors.CALLOUT_SUPERSEDED
title: "CALLOUT_SUPERSEDED — this compute node was replaced, or its callout has ended"
stability: stable
see_also:
  - errors
  - errors.TRANSACTION_NOT_FOUND
  - errors.TRANSACTION_EXPIRED
  - errors.DISPATCH_TIMEOUT
  - errors.COMPUTE_MEMBER_DISCONNECTED
---

# errors.CALLOUT_SUPERSEDED

## NAME

CALLOUT_SUPERSEDED — a request carrying a transaction token was refused because the processor, criterion or function request it belongs to is no longer this compute node's.

## SYNOPSIS

HTTP: `410` `Gone`. Retryable: `no`.

## DESCRIPTION

A compute node receives a transaction token with each processor, criterion or function request, and presents it on the API requests it makes while it works. The token is valid for that one request on that one compute node. This error fires when the token is presented after cyoda gave the same work to another compute node — because this one did not answer within its answer limit, or its connection dropped — or after the request ended: answered, failed or abandoned. The transaction itself is still open; once it has ended the answer is `TRANSACTION_NOT_FOUND` instead.

The request is refused before it reads or writes anything. A request that was already reading or writing when the work moved on is allowed to finish, and is answered normally.

A compute node that receives this error must stop working on that request: nothing further it sends under that token is accepted, and its answer is discarded. Whatever it did outside cyoda before it was replaced is the application's to reconcile; a processor declared `idempotent` promises that a repeat on another compute node is safe.

Not retryable. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

## SEE ALSO

- errors
- errors.TRANSACTION_NOT_FOUND
- errors.TRANSACTION_EXPIRED
- errors.DISPATCH_TIMEOUT
- errors.COMPUTE_MEMBER_DISCONNECTED
