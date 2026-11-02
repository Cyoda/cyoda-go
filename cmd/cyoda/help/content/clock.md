---
topic: clock
title: "clock — the controllable platform clock (a test feature)"
stability: experimental
version_added: 0.9.0
see_also:
  - config.clock
  - config.cluster
  - errors.CLOCK_NOT_CONFIRMED
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

`POST` needs a platform operator and `CYODA_CLOCK_CONTROL_ENABLED=true` (see `cyoda help config clock`); without it the answer is `403 CLOCK_CONTROL_DISABLED`. `GET` answers any authenticated caller, and over gRPC there is `PlatformClockGetRequest`. A change takes `time` or `advanceByMs` (not both), and optionally `rate`. A successful `POST` answers `200` with `now`, `rate`, `controllable` and `nodes`, the ids of the nodes that hold the new setting (`["local"]` on a single node). A `time` later than now is a move (see below), not only a set in the past.

## DESCRIPTION

**Test feature, not for production.** The clock is instance-wide: one setting applies to every tenant. While a move runs, the clock lands on the due times of scheduled tasks in every tenant, so any caller reading the clock then sees those times (not which tenant they belong to).

**What it affects.** Business times: entity and revision dates, audit event times, consistency times, model update dates, search-job times, scheduled-transition due times, the point-in-time "now", and the `time` attribute of callout requests (the owner's platform time when it took the callout up, kept across retries; see `cyoda help cloudevents`), so a schedule function can compute an absolute fire time on the platform's clock. **What stays on real time:** token and key validity, timeouts and deadlines, leases, heartbeats and keep-alives.

**Starting in the past.** Set the start first, on a fresh store. A time earlier than the latest time the store has recorded is refused with `409 CLOCK_BEFORE_RECORDED_TIME`, naming the latest recorded time. Any earlier activity (a write, a point-in-time read, a search) raises the earliest allowed start. Model import does not, but it records its date, so set the start before importing models. The check records the time it is made at, so setting the same time again can be refused.

**Range.** `time` lies in the years 1970 to 9000, and `advanceByMs` may not move the clock past the end of 9000.

**Moving forward.** A move does not jump. It steps through the scheduled transitions that come due on the way, and each fires at its own due time, within about one scan interval (`CYODA_SCHEDULER_SCAN_INTERVAL`) times the rate. Due order holds for due times further apart than that; closer ones run together. The cost is one step per distinct due time, including retries and schedules that re-arm themselves. A move waits while a due task is pending (for example when no node runs a scheduler), so allow for that when you wait for the answer. A move that is interrupted leaves the clock where it got to. To continue, repeat it with the same `time`; `advanceByMs` counts from the clock's current time, so read `GET /platform/clock` first.

**Rate.** From 0.1 to 100; 1 is recommended. A rate other than 1 makes retries and time limits pass faster or slower in platform time, and multiplies any difference between node clocks. Retention follows platform time as well: after a move, the next reap drops async search results, and later commits drop recorded submit times, that are past their retention at the new time, and a rate above 1 shortens retention in real time. A `rate` sent with a later `time` or with `advanceByMs` takes effect when the move finishes.

**Restrictions.**
- Keep the system idle while the clock changes; nothing guards activity during a change. See `TESTING WITH THE PLATFORM CLOCK`.
- Cluster nodes need synchronised real clocks, and all use the same value of `CYODA_CLOCK_CONTROL_ENABLED`.
- A node that restarts while other nodes run takes their setting. Only a full restart (no node left running, or a single-node deployment) returns the clock to real time, and that startup fails if the store holds times more than 1 s later than real time: start over with a fresh store.
- Mock IAM lets any admin act as platform operator.

## TESTING WITH THE PLATFORM CLOCK

The clock is for tests on a cluster you control. A change is only correct under three preconditions, which nothing enforces:

- The cluster is static during a change: no node starts, stops or joins.
- The system is idle.
- One change at a time: send the next only after the previous call has returned.

Every change, and every step of a move, is pushed to every node of the cluster, and each node must echo exactly that setting. A `200` therefore means every node holds the setting, and the answer lists the nodes in `nodes`. If a node does not confirm within `CYODA_PROXY_TIMEOUT`, the answer is `503 CLOCK_NOT_CONFIRMED`, which names the nodes. Nothing is rolled back, so the cluster may hold different clocks; start over by restarting the cluster with a fresh store (see `cyoda help errors CLOCK_NOT_CONFIRMED`).

Advice for a test run:

- Set a start in the past first, before you write any data or import any model: see **Starting in the past**.
- Allow for retention after a move and at a rate above 1: see **Rate**.
- A move needs a running scheduler and the compute nodes that its scheduled tasks call; without them it waits.
- A node that starts or shuts down while a change runs can make the change fail, or the change can miss it: a node that joins after the change has read the list of nodes is not pushed the setting and is not in `nodes`. Check `nodes` against the nodes you run.
- Synchronise the real clocks of the cluster nodes and give every node the same `CYODA_CLOCK_CONTROL_ENABLED`: see **Restrictions**.

## SEE ALSO

- config.clock
- config.cluster
- errors.CLOCK_NOT_CONFIRMED
- scheduled-tasks
- workflows
- crud
- openapi
