---
topic: clock
title: "clock — the controllable platform clock (a test feature)"
stability: experimental
version_added: 0.9.0
see_also:
  - config.clock
  - scheduled-tasks
  - workflows
  - crud
  - openapi
---

# clock

## NAME

clock — set, move and speed up the platform's clock for tests and demonstrations.

## SYNOPSIS

```
GET  /api/platform/clock
POST /api/platform/clock      {"time": "2024-01-02T09:00:00Z"}
POST /api/platform/clock      {"advanceByMs": 86400000}
POST /api/platform/clock      {"rate": 1}
```

`POST` needs a platform operator and `CYODA_CLOCK_CONTROL_ENABLED=true` (see `cyoda help config clock`); without it the answer is `403 CLOCK_CONTROL_DISABLED`. `GET` answers any authenticated caller, and over gRPC there is `PlatformClockGetRequest`. A change takes `time` or `advanceByMs` (not both), and optionally `rate`. A `time` later than now is a move (see below), not only a set in the past.

## DESCRIPTION

**Test feature, not for production.** The clock is instance-wide: one setting applies to every tenant. While a move runs, the clock lands on the due times of scheduled tasks in every tenant, so any caller reading the clock then sees those times (not which tenant they belong to).

**What it affects.** Business times: entity and revision dates, audit event times, consistency times, model update dates, search-job times, scheduled-transition due times, the point-in-time "now", and the `time` attribute of callout requests, so a schedule function can compute an absolute fire time on the platform's clock. **What stays on real time:** token and key validity, timeouts and deadlines, leases, heartbeats and keep-alives.

**Starting in the past.** Set the start first, on a fresh store. A time earlier than the latest time the store has recorded is refused with `409 CLOCK_BEFORE_RECORDED_TIME`, naming the latest recorded time. Any earlier activity (a write, a point-in-time read, a search) raises the earliest allowed start. Model import does not, but it records its date, so set the start before importing models. The check records the time it is made at, so setting the same time again can be refused.

**Moving forward.** A move does not jump. It steps through the scheduled transitions that come due on the way, and each fires at its own due time, within about one scan interval (`CYODA_SCHEDULER_SCAN_INTERVAL`) times the rate. Due order holds for due times further apart than that; closer ones run together. The cost is one step per distinct due time, including retries and schedules that re-arm themselves. A move waits while a due task is pending (for example when no node runs a scheduler), so allow for that when you wait for the answer. Make one change at a time. A move that is interrupted leaves the clock where it got to. To continue, repeat it with the same `time`; `advanceByMs` counts from the clock's current time, so read `GET /platform/clock` first.

**Rate.** From 0.1 to 100; 1 is recommended. A rate other than 1 makes retries and time limits pass faster or slower in platform time, and multiplies any difference between node clocks. Retention follows platform time as well: a move reaps async search results and recorded submit times that are past their retention at the new time, and a rate above 1 shortens retention in real time. A `rate` sent with a later `time` or with `advanceByMs` takes effect when the move finishes. A clock that runs past the year 9999 makes reads fail and refuses a `rate` change; start a new store.

**Restrictions.**
- Keep the system idle while the clock changes; nothing guards activity during a change.
- Cluster nodes need synchronised real clocks, and all use the same value of `CYODA_CLOCK_CONTROL_ENABLED`.
- A node that restarts while other nodes run takes their setting. Only a full restart (no node left running, or a single-node deployment) returns the clock to real time, and that startup fails if the store holds times more than 1 s later than real time: start over with a fresh store.
- Mock IAM lets any admin act as platform operator.

## SEE ALSO

- config.clock
- scheduled-tasks
- workflows
- crud
- openapi
