---
topic: errors.JOINED_RESPONSE_TOO_LARGE
title: "JOINED_RESPONSE_TOO_LARGE — a callback's answer is larger than it may hold under the transaction"
stability: stable
see_also:
  - errors
  - errors.CALLOUT_SUPERSEDED
  - errors.SEARCH_RESULT_LIMIT
  - config
---

# errors.JOINED_RESPONSE_TOO_LARGE

## NAME

JOINED_RESPONSE_TOO_LARGE — the answer to a request made under a transaction token is larger than the server may hold while it holds the transaction.

## SYNOPSIS

HTTP: `413` `Content Too Large`. Retryable: `no`.

## DESCRIPTION

A compute member's callback — a request carrying a transaction token — runs while it holds its transaction, and its answer is built in memory and sent only once the transaction has been let go of. Everything else waiting on that transaction, including the end of the callout itself, waits behind those bytes, so the answer has a ceiling: `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES` (default `10485760`, 10 MiB). The message carries the configured figure.

The whole answer is discarded when it passes the ceiling. Nothing is sent in part: a collection cut short would be a wrong answer given as an available one, and the caller could not tell the two apart. The same applies on all three doors — the HTTP callback door and the two gRPC ones, the unary door and the server-streaming door, where the frames of a chunked collection are counted together.

The ceiling is on the answer only. A callback's *request* body has its own, separate cap — a fixed 10 MiB, which no setting moves — and a body over it is refused with `413` `BAD_REQUEST` before the transaction is touched.

The message carries the configured figure under the rule this server's refusals follow: a refusal names the figure the caller can work within, and withholds one that only reads how loaded the node is. A byte ceiling is the first kind — a caller pages its read to fit under it. `TOO_MANY_JOINED_REQUESTS` withholds its queue cap for the opposite reason: no caller can act on it.

Not retryable: the same request builds the same answer again. Ask for less — `pageSize` and `pageNumber` on a get-all, `limit` on a search — and make several callbacks instead of one. If the deployment's compute members legitimately read more than this in one callback, an operator raises `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES`; the cost is memory held on the owning node while the transaction is held.

See `cyoda help config grpc` for the variable. Over gRPC the code is the message prefix of the error envelope.

## SEE ALSO

- errors
- errors.CALLOUT_SUPERSEDED
- errors.SEARCH_RESULT_LIMIT
- config
