---
topic: errors.CLOCK_BEFORE_RECORDED_TIME
title: "CLOCK_BEFORE_RECORDED_TIME — the requested time is earlier than a time the store has recorded"
stability: stable
see_also:
  - errors
  - errors.CLOCK_CONTROL_DISABLED
---

# errors.CLOCK_BEFORE_RECORDED_TIME

## NAME

CLOCK_BEFORE_RECORDED_TIME — a request to set the platform clock named a time earlier than the latest time the store has recorded.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no` (as sent).

## DESCRIPTION

`POST /api/platform/clock` with a `time` not later than now sets the clock there. The store keeps history in time order: setting the clock before a time it has already recorded would stamp new writes earlier than existing ones. The request is refused and the clock stays where it was.

The problem's detail names the latest recorded time, and the problem's `properties.latestRecordedTime` carries it as an RFC 3339 timestamp. Ask for that time or a later one, or start over with a fresh store to go further back. A `time` later than now is a move forward and is never refused with this code.

## SEE ALSO

- errors
- errors.CLOCK_CONTROL_DISABLED
