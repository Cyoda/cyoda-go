---
topic: errors.SERVER_ERROR
title: "SERVER_ERROR — unexpected internal server error"
stability: stable
see_also:
  - errors
---

# errors.SERVER_ERROR

## NAME

SERVER_ERROR — an unexpected error occurred on the server that prevented the request from being fulfilled.

## SYNOPSIS

HTTP: `500` `Internal Server Error`. Retryable: `yes` (with caution).

## DESCRIPTION

The server encountered an unclassified internal error. The response body contains a `ticket` UUID that correlates with the server-side error log. No internal details are exposed in the response.

Retryable with caution. Simple reads may be retried safely. Writes must be treated as potentially applied — the write outcome must be verified before retrying to avoid duplicates. The `ticket` value identifies the server-side error log entry.

A request made under a transaction token — a compute member's callback — that is answered `500` may have failed in a way the server could not undo. The transaction it joined then cannot commit: the operation that owns the transaction fails, whatever the member answers. Such an answer carries `retryable: false`, because a retry in the same transaction cannot succeed.

## SEE ALSO

- errors
