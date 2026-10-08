---
topic: errors.CONSISTENCY_TIME_UNAVAILABLE
title: "CONSISTENCY_TIME_UNAVAILABLE — the store could not provide a consistency time in time"
stability: stable
see_also:
  - errors
  - errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME
  - errors.STORAGE_UNAVAILABLE
  - crud
---

# errors.CONSISTENCY_TIME_UNAVAILABLE

## NAME

CONSISTENCY_TIME_UNAVAILABLE — the store could not certify a consistency time within its wait budget.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

To give a consistency time, the store waits for your tenant's saves that are in their commit phase to finish. That normally takes milliseconds. When a save stays in its commit phase longer than the store's wait budget, the request is refused rather than answered with an instant that could still change. The budget is 10 seconds on postgres (or `CYODA_POSTGRES_STATEMENT_TIMEOUT` when lower) and up to the engine's 11-second store-call deadline on sqlite; memory never waits. A read returns nothing; an async search submit creates no job.

Raised by `GET /api/entity/consistency-time`, by an async search submitted without `pointInTime`, and by any read with a `pointInTime` that is not already known to be at or before the consistency time. No `Retry-After` header is sent; retry with back-off, for example starting at 100 ms and doubling. Repeated occurrences mean a commit is stalling — for example a node that died mid-commit; on postgres such a session ends within seconds.

## SEE ALSO

- errors
- errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME
- errors.STORAGE_UNAVAILABLE
- crud
