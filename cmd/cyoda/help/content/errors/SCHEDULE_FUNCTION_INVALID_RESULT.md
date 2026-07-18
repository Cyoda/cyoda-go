---
topic: errors.SCHEDULE_FUNCTION_INVALID_RESULT
title: "SCHEDULE_FUNCTION_INVALID_RESULT — scheduled-transition Function returned an unusable result"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_TIMEOUT
  - errors.CONDITION_TYPE_MISMATCH
---

# errors.SCHEDULE_FUNCTION_INVALID_RESULT

## NAME

SCHEDULE_FUNCTION_INVALID_RESULT — a scheduled transition's arm-time Function callout completed, but the engine could not interpret its result as a `Schedule`.

## SYNOPSIS

HTTP: `500` `Internal Server Error`. Retryable: `no`.

## DESCRIPTION

A `TransitionSchedule.function` callout must return `resultKind: "Schedule"` with a value shaped `{fireAt|fireAfterMs, expireAt?|expireAfterMs?}` — exactly one of `fireAt`/`fireAfterMs`, and at most one of `expireAt`/`expireAfterMs`. This error is raised when the compute node returns a different `resultKind`, or a `Schedule` value that is malformed: missing or duplicate fire/expiry fields, an unknown field, or a non-numeric value.

A `fireAt` in the past is not an error — the transition is armed to fire immediately. Only a resolved expiry at or before the resolved fire time is treated as a distinct outcome (the arm is skipped and any prior scheduling for the transition is cancelled), not as this error.

The failure is in the compute node's implementation, not the caller's request — the entity write that triggered arming is rejected so no state change commits against an unschedulable transition. Fix the Function's response shape; do not retry until it is corrected.

## SEE ALSO

- errors
- errors.DISPATCH_TIMEOUT
- errors.CONDITION_TYPE_MISMATCH
