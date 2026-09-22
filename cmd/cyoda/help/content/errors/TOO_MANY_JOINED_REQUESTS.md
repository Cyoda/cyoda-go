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

Callbacks of one transaction are served **one at a time**: each holds the transaction for the time the server works on it, so callbacks sent in parallel are served in turn. The queue behind the callback being served is bounded by `CYODA_CALLOUT_JOINED_MAX_WAITERS` (default `128`), and a callback that arrives when the queue is full is refused rather than parked. The message does not carry the figure — see the rule below.

That setting bounds how many callbacks may queue, not how many bytes they hold. Every callback waiting its turn holds its whole request in memory until it runs, and it may wait for as long as the callout lasts; that request is capped at a fixed 10 MiB, which no setting moves, and the queue as a whole has no byte ceiling at all. The cap is also a reading rather than a reservation, so callbacks that arrive together can all pass it and buffer their requests before taking their places.

The refused callback changes nothing: it is turned away before its request body is read where the door allows that, and otherwise before it takes the transaction, so it reads and writes nothing and leaves no trace. The callback holding the transaction and the ones already queued are unaffected, and the transaction itself carries on.

Retryable, and the action is the usual one for a capacity refusal: back off briefly and send the callback again. Firing many callbacks at one transaction at once buys no speed, because they are served in turn either way — the refusal enforces that rather than introducing it. A processor that lets this error escape fails its callout, and the operation is rolled back, so a compute member should handle it rather than propagate it.

The message withholds the cap deliberately, under the rule this server's refusals follow: a refusal names the figure the caller can work within, and withholds one that only reads how loaded the node is. Knowing the cap would change nothing a caller does — back off and retry is the same action at any value — while the number itself is operator information. `JOINED_RESPONSE_TOO_LARGE` names its ceiling for the opposite reason: a caller can page a read under a byte budget.

Frequent occurrences mean a compute member is submitting callbacks far faster than the server can serve them. Slow the member down, or have an operator raise `CYODA_CALLOUT_JOINED_MAX_WAITERS`, at the cost of memory held on the owning node while the transaction is held.

See `cyoda help config grpc` for the variable.

## SEE ALSO

- errors
- errors.SEARCH_QUEUE_FULL
- errors.CALLOUT_SUPERSEDED
- config
