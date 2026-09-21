---
topic: errors.TOO_MANY_JOINED_REQUESTS
title: "TOO_MANY_JOINED_REQUESTS — too many callbacks are already waiting for this transaction"
stability: stable
see_also:
  - errors
  - errors.SEARCH_QUEUE_FULL
  - errors.CALLOUT_SUPERSEDED
  - config
---

# errors.TOO_MANY_JOINED_REQUESTS

## NAME

TOO_MANY_JOINED_REQUESTS — the node refused a request made under a transaction token because that transaction already has as many callbacks waiting as it may have.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

Callbacks of one transaction are served **one at a time**: each holds the transaction for the time the server works on it, so callbacks sent in parallel are served in turn. Every callback waiting its turn holds its whole request in memory until it runs, and it may wait for as long as the callout lasts. The queue behind the callback being served is therefore bounded by `CYODA_CALLOUT_JOINED_MAX_WAITERS` (default `128`), and a callback that arrives when the queue is full is refused rather than parked.

The refused callback touched nothing: it is turned away before its request body is read where the door allows that, and it never reaches the transaction. The callback holding the transaction and the ones already queued are unaffected.

Retryable, and the action is the usual one for a capacity refusal: back off briefly and send the callback again. Firing many callbacks at one transaction at once buys no speed, because they are served in turn either way — the refusal enforces that rather than introducing it. A processor that lets this error escape fails its callout, and the operation is rolled back, so a compute member should handle it rather than propagate it.

Frequent occurrences mean a compute member is submitting callbacks far faster than the server can serve them. Slow the member down, or have an operator raise `CYODA_CALLOUT_JOINED_MAX_WAITERS`, at the cost of memory held on the owning node while the transaction is held.

See `cyoda help config grpc` for the variable.

## SEE ALSO

- errors
- errors.SEARCH_QUEUE_FULL
- errors.CALLOUT_SUPERSEDED
- config
