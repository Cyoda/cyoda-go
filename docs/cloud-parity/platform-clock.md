# The platform clock — Cloud twin-alignment spec

CaaS ticket: [CP-3992](https://cyoda1.atlassian.net/browse/CP-3992)

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's controllable platform clock. cyoda-go is the authoritative
implementation. Cloud has no controllable clock today: its clock control is
replaceable only by tests.

The clock is a test feature. On a deployment that turns it on
(`CYODA_CLOCK_CONTROL_ENABLED`, default off), the platform operator sets the
platform's clock to a past or future time, changes its rate, or moves it
forward. Every business time reads this one clock. The clock is instance-wide
and affects every tenant. A deployment without the feature takes its times
from today's sources.

## 1. What reads the clock

**Business time, platform clock:** revision and entity dates, audit event
times, consistency times, model update dates, search-job times,
scheduled-transition arm, due and fire times, the point-in-time "now", and the
CloudEvent `time` attribute on processor, criterion and schedule-function
requests (so a schedule function computes an absolute fire time on the
platform's clock).

**Operational time, real time:** token and key validity, timeouts and
deadlines, leases, heartbeats, keep-alives, metrics.

## 2. The setting

A start time `S`, the real time `R` at which it applied, and a rate `k` from
0.1 to 100 (no frozen clock). Platform time is `S + k * (real now - R)`. A
cluster shares one setting; the highest version (a counter, then the writing
node id) wins; a node that joins takes it from its peers.

## 3. API

| Operation | Contract |
|---|---|
| `GET /platform/clock` | Any authenticated caller. `PlatformClockDto`: `now`, `rate`, `controllable`. Without the feature: real time, rate 1. gRPC read: `PlatformClockGetRequest` / `PlatformClockResponse` |
| `POST /platform/clock` | Platform operator only. Body `PlatformClockChangeDto`: `time`, `advanceByMs` (> 0), `rate`; at most one of `time` and `advanceByMs`, at least one member. `200 PlatformClockDto` when complete |

| Status | Code | When |
|---|---|---|
| 403 | `CLOCK_CONTROL_DISABLED` | the feature is off |
| 400 | strict-body and validation codes | malformed body, unknown member, null member, no member, both `time` and `advanceByMs`, `advanceByMs <= 0`, `advanceByMs` that moves the clock past the year 9999, `rate` outside 0.1 to 100 or with more than six fractional digits, `rate` alone once the clock has run past the year 9999, `time` not RFC 3339 or outside 1970 to 9999 |
| 409 | `CLOCK_BEFORE_RECORDED_TIME` | `time` earlier than the latest time the store has recorded; the detail names it, and `properties.latestRecordedTime` carries it as an RFC 3339 timestamp |
| 503 | store unavailable | the store did not answer the check or the move's task query |

## 4. Behaviour

- **Set a time not later than now.** Allowed only if the store has recorded no
  later time (checked against the consistency time); otherwise the previous
  setting stays and the answer is `409`.
- **Move forward.** A move steps through scheduled transitions: each task that
  comes due on the way fires at its own due time, in due order for due times
  further apart than one scan interval times the rate. The move waits while a
  due task is pending or running. One change at a time; an interrupted move
  leaves the clock where it got to.
- **Rate change** re-anchors at the current platform time, so time does not
  jump.
- **Restart.** A node with no peer holding a setting starts on real time and
  fails startup if the store's latest recorded time is more than 1 s later.

## 5. What Cloud does

Cloud has no such control; this is a cyoda-go test feature. Cloud adopts the
CloudEvent `time` attribute behaviour (it already sets it) and the
`PlatformClockGetRequest` read if it wants twin test tooling. Plugin authors
must implement `ScheduledTaskStore.SchedulerActivity` and
`TransactionManager.LatestRecordedTime`, and judge a never-heartbeated search
job's liveness from `HeartbeatTime`, which `CreateJob` now stamps.
