---
topic: errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME
title: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME — the requested instant is later than the consistency time"
stability: stable
see_also:
  - errors
  - errors.CONSISTENCY_TIME_UNAVAILABLE
  - crud
  - search
---

# errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME

## NAME

POINT_IN_TIME_AFTER_CONSISTENCY_TIME — a read asked for an instant later than the consistency time.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no` (as sent).

## DESCRIPTION

A read with `pointInTime` returns the data as at that instant, and that answer never changes afterwards. That holds only for an instant at or before the **consistency time**: the instant up to which every save is final. A later instant could still gain saves, so the read is refused instead of answered. An instant at or before the store's clock is never refused: the store waits for the saves still committing. Only an instant later than the store's clock is refused — a future instant, or one from a client clock running ahead of the store's.

The problem's `properties.consistencyTime` carries the current consistency time. Read at that instant or earlier. To read "as of now" with a stable answer, take the consistency time from `GET /api/entity/consistency-time` and pass it as `pointInTime`; a read at that value is never refused with this code (a `503` is still possible). For a genuinely future instant, wait until the consistency time reaches it and re-issue the read. Over gRPC the message text carries the consistency time, since the envelope has no properties; or call `EntityConsistencyTimeGetRequest`.

## SEE ALSO

- errors
- errors.CONSISTENCY_TIME_UNAVAILABLE
- crud
- search
