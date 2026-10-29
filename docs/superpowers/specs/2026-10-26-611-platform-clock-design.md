# A controllable platform clock (#611)

## What this is

A test feature. On a deployment that turns it on, the platform operator can
set the platform's clock to a time in the past or the future, run it faster
or slower, and move it forward. Every part of cyoda-go that reads "now" for a
business purpose reads this one clock. Applications read the same clock from
the platform, so the times they record agree with the platform's own.

The clock is instance-wide, so by definition it affects every tenant.

Production does not use it. A deployment without the feature runs today's
code paths and takes its times from today's sources.

Research notes: `docs/superpowers/research/2026-10-26-611-platform-clock-research.md`.

## Why

An application on cyoda-go needs tests, demonstrations and example data that
span days, months or years. Today it can move only the time it owns. The
platform stamps revisions, audit events and scheduled fires on real time.

The first user, CTCC, seeds collateral data over several business days. After
such a seed, the application says a pledge was submitted on day 3, while the
platform's audit event for the same submission says "a minute ago". The audit
trail is the record a treasury system has to defend, so the two must agree.

## Terms

- **Real time:** the physical clock of the machine or database.
- **Platform time:** the time cyoda-go reads for business purposes.
- **Clock:** the one object in a cyoda-go process that answers "what is the
  platform time now?" (`Now() time.Time`). It is injected into every
  component that reads business time.
- **Clock setting:** a start time `S`, the real time `R` at which `S`
  applied, and a rate `k`. A test clock answers `S + k × (real now − R)`.
  - `k = 1` runs at real speed.
  - `k = 2` makes one real minute two platform minutes.
- **Business time:** a time that is stored, returned to a client, sent to a
  compute node, or compared with stored data. Examples: revision times, audit
  event times, consistency times, scheduled-transition due times, model update
  dates, search job times.
- **Operational time:** a time used only to measure elapsed real time.
  Examples: timeouts, deadlines, leases, heartbeats, token and key validity,
  keep-alives, metrics.
- **Floor:** the existing value in each store that never goes back (#649). A
  commit stamp is never lower than it.

## Decisions taken in the design discussion

These were decided with Paul and are not reopened here:

1. **A test feature.** It needs no durability and no protection against
   network partitions or other high-availability concerns. After a hiccup, a
   test starts over.
2. **Idle during changes.** Keeping the system idle while the clock changes
   is the test's job. It is a documented restriction, not something the code
   enforces.
3. **Operator-run.** The platform operator runs the clock through the API.
   There is no configured start time.
4. **Rate limits.** The rate is between 0.1 and 100. There is no frozen
   clock. Anything other than 1 is not recommended.
5. **Production keeps the database clock** (option A). On postgres, commit
   stamps and consistency times come from the database server's clock, as
   today: one physical clock for every stamp in the cluster. Only a test
   clock passes its own time into the stamp SQL. The original design defines
   transaction time as "the database's view of commit time"
   (cyoda-light-go `docs/ARCHITECTURE.md:366`). With node clocks as the
   source, a node with a wrong clock would push every stamp in the cluster
   forward for good, through the floor.
6. **Moves step through due times.** A move fires each scheduled transition
   that comes due on the way at its own due time, in due order (see "Moving
   the clock").
7. **Restart.** A node that starts with no peer holding a clock setting
   starts on real time. If the store holds times later than real time, startup
   fails, because the test must start over.

## Goals

1. One clock object per process, injected wherever business time is read.
   All clock logic is in one type.
2. Production behaves as today: real time, from today's sources.
3. With the feature on, the operator can set the clock to any time not
   earlier than the latest time the store has recorded, change the rate, and
   move it forward.
4. A move fires every scheduled transition that comes due on the way, each at
   its own due time.
5. Every node of a cluster uses the same clock setting.
6. Applications and compute nodes can read the platform time.

## Non-goals

- **Durability.** A full restart returns the clock to real time.
- **Guarantees for activity during a change.** Transactions in flight during
  a change are not protected (decision 2).
- **Frozen clocks.**
- **Application-supplied valid time (#293).**

## The clock

```go
// Clock answers the platform time. It is injected wherever business time
// is read.
type Clock interface {
    Now() time.Time
}
```

- **Production clock:** `time.Now()`.
- **Test clock:** holds the setting in memory, swapped atomically, and
  computes `S + k × (time.Now() − R)`.
  - Elapsed time is measured on Go's monotonic clock, so a step in the
    machine's wall clock does not jump the platform time.
  - `Now()` never fails and never does I/O.
- **Selection:** the host builds one or the other at startup, from
  `CYODA_CLOCK_CONTROL_ENABLED`, and passes it through constructors to every
  component that reads business time. This is the project's existing
  injection style (`app/app.go`).
- **Existing clock hooks are rewired to it:**
  - `scheduler.Clock` (`internal/scheduler/clock.go`);
  - the engine's `WithScheduledClock` (`internal/domain/workflow/engine.go:213-219`).
  
  The scheduler keeps its own interface. It is the same method set, so the
  clock satisfies it.

### What reads the clock, and what stays on real time

**Business time, reads the clock:**

| Site | Today |
|---|---|
| scheduler claim and bookkeeping (`internal/scheduler/service.go:607`, `:690`) | `scheduler.Clock`, real |
| engine arm, fire decision, audit timestamps (`internal/domain/workflow/engine.go:173,236,1110`, `arm.go:115`, `fire_scheduled.go:91,186`) | engine clock, real |
| entity pre-commit `CreationDate`/`LastModifiedDate` (`internal/domain/entity/service.go:282,2015,2271,2528`) | `time.Now()` |
| model `UpdateDate` (`internal/domain/model/service.go:195`) | `time.Now()` |
| search job `CreateTime` and finish times (`internal/domain/search/service.go:967,1183,1189,1208,1216,1228,1315,1496`, `reaper.go:258,292`) | `time.Now()` |
| callout request CloudEvent `time` attribute (callout dispatch, `internal/grpc/cloudevent.go`) | not set |
| storage plugins' business stamps (see "Plugins") | plugin clock, or the database clock |

**Operational time, stays on real time:**
- everything under `internal/auth` and the key and trusted-key adapters in
  `internal/domain/account`, including M2M client dates and key `validFrom`
  defaults. A clock change must never change which credential is valid.
  Tokens from an external identity provider carry real times anyway.
- callout deadlines, gossip, keep-alives, heartbeats, watchdogs, the
  model-cache lease, member `ConnectedAt`, metrics;
- v1 UUIDs (`internal/common/uuid.go:21`). They are identifiers; the audit
  listing uses their time only to break ties (`internal/domain/audit/events.go:144`).

The build lists every remaining `time.Now`/`time.Since`/`time.Until` in
non-test code and records each as business or operational. The research notes
have the starting inventory.

### Platform time on callout requests

- **What carries it:** processor, criterion and schedule-function requests to
  compute nodes carry the standard CloudEvents `time` attribute.
- **Its value:** the clock's time when the request is built.
- **Why:** a schedule function that returns an absolute fire time computes it
  on the platform's clock, not the compute node's.
- **Cloud:** it sets this attribute from its JVM clock
  (`~/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas/grpc/ProtobufMessageConverter.kt:153`).
- **Other CloudEvents** (keep-alives, API responses, errors) are unchanged.

## The setting across a cluster

The setting is a small value: `S`, `R` (as Unix time), `k`, and a version made
of a counter plus the node id that wrote it (for a total order). Each node
holds it in its test clock.

- **Changes.** The node that handles a change applies it locally and
  broadcasts it on a gossip topic (`Gossip.Broadcast`,
  `internal/cluster/registry/gossip_broadcast.go`). Every node keeps the
  highest version it has seen.
- **Joining nodes.** A node that joins receives the setting in memberlist's
  join-time state exchange (`LocalState`/`MergeRemoteState`; today they are
  unused, `internal/cluster/registry/gossip.go:605-606`). The periodic
  push-pull of that exchange repairs a broadcast a node missed.
- **Single-node deployments** have no gossip; the setting is local.
- **Lag.** A node learns of a change within gossip latency, usually well
  under a second. Under decision 2 the test waits for the change to return
  and keeps idle meanwhile, so lag is not guarded.
- **Real clock differences between nodes** show up multiplied by the rate:
  10 ms of NTP difference is 1 s at rate 100. The documentation says the
  nodes need synchronised clocks.

Nodes compute the platform time from their own real clocks; there is no
master. That is how NTP works too: it distributes a reference, and each
machine keeps time locally.

## Changing the clock

All changes are made through `POST /platform/clock` by a platform operator, on
a node with `CYODA_CLOCK_CONTROL_ENABLED=true`. The test keeps the system idle
meanwhile (decision 2).

### Setting a time not later than now (start in the past)

The operator sets `S = T`. This is allowed only if no time later than `T` is
recorded in the store:

1. The node applies `T` to its local clock.
2. It asks the store for its consistency time. On every backend that is
   `max(clock, floor)`, and with the test clock it is computed from the clock's
   time (see "Plugins").
3. If the answer is later than `T` plus a small tolerance (the time the call
   took, times the rate), the store holds a later time. The node restores its
   previous setting and answers 409 `CLOCK_BEFORE_RECORDED_TIME`, naming the
   latest recorded time.
4. Otherwise it broadcasts the setting.

On a fresh store the floor is zero, so any start from 1970 on is accepted. The
help topic says to set the start before anything else: on memory and sqlite,
even a read moves the floor (their snapshot time), so a read made first raises
the earliest allowed start.

### Moving forward (step-through)

A target `T` later than now runs this loop on the node that received the
request:

```
loop:
  wait until no scheduled task is RUNNING and none WAITING is due (≤ now)
  e = earliest NextAttemptTime of any WAITING task, all tenants
  if there is none, or e > T:
      set the clock to T (and the requested rate, if any); done
  set the clock to e
  wake this node's scheduler
```

- **Each step** is an ordinary change: applied locally and broadcast.
- **The scheduler** on every node then fires what is due, as it does on real
  time. A fire is recorded at `e` plus the scan latency times the rate. The
  default scan interval is 1 s, so at rate 1 a fire is recorded within about
  a second of its due time, as on real time.
- **Fires that arm or retry** add tasks that the next pass finds. So a
  transition armed by an earlier fire inside the window is reached in its
  place.
- **"Wait"** polls the store, so it sees tasks on every node.
- **Concurrency.** The loop remembers the version of its own last change. If
  it sees a version it did not write, another change was made, and it stops
  with 409 `CLOCK_CHANGED`.
- **Interruption.** If the request is cancelled or the node stops, the clock
  stays where the last step put it, running at its rate. Repeating the move
  continues from there.
- **Cost:**
  - one step per distinct due time in the window, including retries and
    schedules that re-arm themselves;
  - each step costs up to one scan interval on the other nodes, plus the
    fire;
  - a move waits as long as a due task stays pending, for example when no
    node runs a scheduler. The caller decides how long to wait.

  The help topic states all of this.

**Why not just jump.** A jump is what a system does after a sleep: everything
that came due fires afterwards, recorded at the new time.
- A task with a time limit shorter than the jump is recorded as expired.
- A transition armed by such a late fire lands after the target, so it does
  not happen in the window at all.

The audit trail then disagrees with the schedule, which is what #611 is about.

### Changing the rate

A rate change re-anchors at the current platform time (`S` = now, `R` = real
now), so the time does not jump. It can be combined with a move. In that case
the move runs at the current rate and the new rate applies at the end.

### Restart

- **A node that starts with clock control on:**
  - first joins the cluster and takes the setting from its peers if any holds
    one;
  - if none does (a single node, or a full restart), it starts on real time
    and checks the store. If the store's consistency time is more than 1 s
    later than real time, startup fails: "the store holds times later than
    real time; start over with a fresh store" (decision 7).
- **Data behind real time** (a past start was used before the restart) does
  not fail the start: real time is simply later, as after a sleep.
- **Clock control off:** a node does none of this, and production startup is
  unchanged.

## API

### HTTP

`GET /platform/clock`
- Any authenticated caller, any tenant.
- 200 `PlatformClockDto`:
  - `now` (date-time): the node's clock;
  - `rate` (number);
  - `controllable` (boolean).
- Without the feature it returns real time and rate 1.

`POST /platform/clock`
- Platform operator only (`auth.OperatorGuard`).
- Body `PlatformClockChangeDto`, read strictly (#678):
  - `time` (date-time, optional);
  - `advanceByMs` (integer > 0, optional): the target is now + `advanceByMs`;
  - `rate` (number from 0.1 to 100, optional).
- At most one of `time` and `advanceByMs`, and at least one member.
- What happens:
  - `time` not later than now: a set (see above);
  - a later target: a move;
  - `rate` alone: a rate change.
- 200 `PlatformClockDto` when complete.

Errors, in the order they are checked:

| Status | Code | When | GET | POST |
|---|---|---|---|---|
| 400 | existing query-parameter refusal | undeclared query parameter | ✓ | ✓ |
| 401 | — | missing or invalid credentials | ✓ | ✓ |
| 403 | existing operator refusal | not a platform operator, or on behalf of a user | — | ✓ |
| 403 | `CLOCK_CONTROL_DISABLED` | the feature is off on this node | — | ✓ |
| 400 | existing strict-body / validation codes | malformed body, unknown member, no member, both `time` and `advanceByMs`, `advanceByMs ≤ 0` or overflowing, `rate` outside 0.1–100, `time` not RFC 3339 or outside 1970–9999 | — | ✓ |
| 409 | `CLOCK_BEFORE_RECORDED_TIME` | `time` earlier than the latest recorded time; the detail names it | — | ✓ |
| 409 | `CLOCK_CHANGED` | another change was made during a move | — | ✓ |
| 503 | existing store-unavailable | the store did not answer the consistency-time check or the move's task query | — | ✓ |
| 500 | generic + ticket | anything else | ✓ | ✓ |

`GET` makes no store call, so it has no 503.

### gRPC

- Read only. CloudEvent types `PlatformClockGetRequest` and
  `PlatformClockResponse` (`now`, `rate`, `controllable`), next to
  `EntityConsistencyTimeGetRequest` (`internal/grpc/cloudevent_types.go:84-85`).
  Each has a published JSON schema (#626).
- Error classes: unauthenticated, internal.
- Changing the clock is HTTP-only, like the other operator controls.

## Configuration

`CYODA_CLOCK_CONTROL_ENABLED` (bool, default `false`). When true:
- the node builds the test clock and passes it to the storage plugin;
- the node accepts `POST /platform/clock`;
- startup logs a WARN: "platform clock control is enabled: test feature, not
  for production; the clock is instance-wide and affects every tenant".

All nodes of a cluster use the same value.

Documented in a new help topic `cmd/cyoda/help/content/config/clock.md`, in
`README.md` and in `DefaultConfig()` (Gate 4). Mock IAM lets any tenant's
admin act as platform operator (`internal/auth/operator_guard.go:30`), as on
every operator route. The help topic says so.

## Storage plugins

### SPI (cyoda-go-spi)

- **`spi.Clock`:** the interface above, plus a factory option
  `spi.WithClock(Clock)` beside `WithClusterBroadcaster` (`plugin.go:51-72`).
  - It is additive: a plugin that ignores the option builds and runs as
    today.
  - A plugin given a clock takes every business stamp from it, and keeps its
    own physical clock for operational time.
- **`ScheduledTaskStore.SchedulerActivity(ctx)`:** returns the earliest
  `NextAttemptTime` of any WAITING task and whether any task is RUNNING,
  across all tenants. The move needs it; today `Query` is per tenant. This is
  a new interface method, so it is breaking for out-of-tree plugins. A store
  without scheduled tasks reports none.
- **Process:** an SPI PR into `main`, a pseudo-pin in all four `go.mod` files
  (`MAINTAINING.md`), and `COMPATIBILITY.md`.

### memory and sqlite

Today one `Clock` serves both business stamps and operational checks
(`plugins/memory/clock.go`, `plugins/sqlite/clock.go`; tests drive it through
`TestClock`). The change:

- **Two roles.** The plugin keeps that clock as its physical, operational
  clock and adds a business clock. Without the option, the business clock is
  that same physical clock, so production and existing tests behave as today.
- **Business clock reads:**
  - submit time, SnapshotTime, consistency time;
  - audit event times;
  - `GetSubmitTime` retention (memory `txmanager.go:1193`, sqlite `:986`,
    `:998-1001`), because the entries are business stamps;
  - search-job reaping against `finished_at`.
- **Physical clock reads:**
  - scheduler owner liveness;
  - search-job heartbeats and stale reclaim.

### postgres

Production keeps the database clock (decision 5). With a test clock:

- **Stamp functions.** `cyoda_stamp` and `cyoda_consistency_time`
  (`migrations/000016_consistency_time.up.sql:39-84`) take an optional time
  argument.
  - NULL (production) uses `clock_timestamp()` exactly as today.
  - A value (test clock) is used instead. The floor rules are unchanged.
- **Placeholder dates.** The `SELECT CURRENT_TIMESTAMP` that fills placeholder
  dates before the commit stamp (`entity_store.go:242`, `:774`) takes the test
  clock's time when one is given. Reads inside the same transaction see the
  placeholder, so it must be platform time.
- **Submit-time pruning and search-job reaping.** Today these compare business
  stamps with Go's `time.Now()` (`transaction_manager.go:354`, `:543-560`;
  `search_store.go:587`). With a test clock they use it, so both sides are
  platform time. Without one they are unchanged.
- **Stale reclaim's fallback** compares the engine-supplied `created_at` with
  the database's `now()` (`search_store.go:630`), mixing two clocks today.
  - The insert writes `heartbeat_time = now()` instead, and the fallback is
    removed. Liveness is then judged on the database clock alone, as for every
    other liveness check.
  - This is a fix to production code (Gate 6); its effect there is within
    host–database skew.
- **Dead defaults** (agreed):
  - drop the unused `DEFAULT now()` on `search_jobs.created_at`
    (`000001:106`; the insert always supplies it, `search_store.go:109`);
  - remove `model_schema_extensions.created_at` (postgres `000001:182`, sqlite
    `000001:149`), which nothing writes or reads.
- **`wall_clock_time`** stays. A comment at the column's write
  (`entity_store.go:360`, whose current text "to match _meta value" is stale)
  says three things:
  - it records the physical moment of insertion, for debugging and analysis
    (cyoda-light-go `docs/plans/2026-03-27-postgresql-storage-design.md` §5);
  - nothing reads it;
  - under a test clock it keeps physical time, so it differs from the
    business times.
- **A stale comment** at `conformance_test.go:146-150` calls `CURRENT_TIMESTAMP`
  "load-bearing for correctness (tx-stable timestamps)". Commit stamping
  replaced that; the comment is corrected.

### cassandra (separate repository)

An issue in `cyoda-go-cassandra`, before its v0.9.0 bump:
- take `spi.WithClock` as the physical part of its hybrid logical clock;
- generate transaction-id and audit timeuuids from it (recovery compares them
  with the floor, `internal/tx/tx_recovery.go:330-337`);
- `period_bucket`;
- the stub `SchedulerActivity`.

## Production impact

With the feature off, these are the only differences from today:

1. **Clock calls.** Business-time sites call `clock.Now()` on an injected
   clock that returns `time.Now()`, instead of calling `time.Now()` directly.
2. **Callout `time` attribute.** Callout requests carry the CloudEvent `time`
   attribute.
3. **Postgres stamp functions** take an extra argument, NULL in production.
   They take the same branch as today.
4. **Stale reclaim** judges a never-heartbeated search job from its insert
   time on the database clock.
5. **Dead defaults and columns** are dropped.

No new store reads, locks, checks or background work run in production.

## Security

- **Changing the clock** needs a platform operator (`OperatorGuard` refuses
  on-behalf-of tokens) on a node with the feature on. The feature is off by
  default.
- **Instance-wide by design.** The clock is the same for every tenant. It
  holds no tenant data. The startup WARN and the help topic say so.
- **Credentials.** No credential, key or token validity reads the platform
  clock.
- **Input validation:**
  - strict body;
  - rate within 0.1–100;
  - times RFC 3339 within 1970–9999;
  - problem details name the refused member.
- **Information in errors.** `CLOCK_BEFORE_RECORDED_TIME` names the latest
  recorded time. Any caller can read that already as a consistency time.

## Documentation

- **New help topic `cyoda help clock`:**
  - what the clock reads;
  - setting a start in the past (first, on a fresh store);
  - moves and their cost;
  - the rate and its effect on time limits and retries;
  - the restrictions: keep the system idle during changes, nodes need
    synchronised clocks, a restart returns to real time or fails on a store
    with later times;
  - the instance-wide effect.
- **Existing topics:**
  - `cyoda help config clock`;
  - `cloudevents.md` (the `time` attribute);
  - `scheduled-tasks.md` (moves fire due tasks at their due times);
  - `crud.md` (point-in-time "now" follows the clock).
- **OpenAPI** for both operations; **gRPC** schemas for the two types.
- **`docs/cloud-parity/platform-clock.md`** (Gate 7), and a CaaS ticket. Cloud
  has no controllable clock (`ClockCtrl` is replaceable only by tests).
- **CHANGELOG:**
  - `### Added` for the feature;
  - `### Breaking` for plugin authors (`SchedulerActivity`).

## Coverage matrix

**Layers:**
- **U:** unit.
- **E:** HTTP e2e in `internal/e2e`, each clock test on its own stack and
  database (the pattern of `internal/e2e/scheduler_harness_test.go:23-28`),
  with the feature on.
- **P:** cross-backend parity. It needs a new optional fixture interface that
  starts a stack on a fresh store with given settings; `BackendFixture` has
  none today (`e2e/parity/fixture.go:19-57`). The in-tree fixtures implement
  it.
- **G:** gRPC.
- **C:** spitest conformance, through a new optional harness hook that builds
  a factory with `spi.WithClock`.

| Scenario | U | E | P | G | C |
|---|---|---|---|---|---|
| Test clock arithmetic: rate, re-anchor, monotonic elapsed | ✓ | | | | |
| Fresh deployment: set the clock years into the past; the first revision, audit event, consistency time and `GET /platform/clock` carry that time | | ✓ | ✓ | | ✓ |
| Advance by one day (#611 "done when"): new revision time, audit time, a transition due in that day fired at its due time, point-in-time "now" answers at the new time, `GET` returns it | | ✓ | ✓ | | |
| Move order: transitions of three entities in two tenants fire in due order, each within one scan interval of its due time; a transition armed by an earlier fire inside the window fires in its place | ✓ | ✓ | | | |
| A transition with a time limit shorter than the move fires and is not expired | | ✓ | | | |
| Rate change does not jump the time; rate 2 runs twice as fast | ✓ | ✓ | | | |
| Plugin stamps follow an injected clock; without it they are as today | | | | | ✓ |
| Operational checks stay on the physical clock with an injected clock far from real time (owner not swept, running search job not reclaimed) | | | | | ✓ |
| With a past clock: an async search's results are not reaped early; `GetSubmitTime` still answers | | ✓ | ✓ | | ✓ |
| `CLOCK_BEFORE_RECORDED_TIME` after a write, with the latest time in the detail | ✓ | ✓ | | | |
| `CLOCK_CHANGED` when a second change arrives during a move (scheduler off, so the move waits) | | ✓ | | | |
| Feature off → 403 `CLOCK_CONTROL_DISABLED`; GET returns real time | | ✓ | | | |
| Non-operator → 403; on-behalf-of → 403; 401 on both operations | | ✓ | | | |
| Each 400 cell, including the query-parameter refusal | ✓ | ✓ | | | |
| 503 when the store fails the check | ✓ | waived: no e2e stops the database mid-test; handler unit test | | | |
| 500 | ✓ | waived: no e2e can force an internal fault; handler unit test | | | |
| Cluster: a change through node A reaches node B; a node that joins later takes the setting | ✓ | ✓ | | | |
| Restart alone on a store with times ahead of real time → startup fails; behind → starts on real time | ✓ | ✓ | | | |
| Callout `time` attribute carries the clock's time; an absolute schedule-function fire time computed from it is due at the right platform time | ✓ | ✓ | | | |
| Stale reclaim of a never-heartbeated job judged on the database clock | | | | | ✓ |
| gRPC `PlatformClockGetRequest`: success, unauthenticated, internal | | | | ✓ | |

## Rollout

1. **SPI PR:** `Clock`, `WithClock`, `SchedulerActivity`, spitest hook and
   cases. Then the pseudo-pin and `COMPATIBILITY.md`.
2. **cyoda-go PR into `release/v0.9.0`.**
3. **The cassandra issue and the CaaS ticket.**

## Left to the builder

- Package and type names; the gossip topic name and the encoding of the
  setting.
- The poll interval of the move's wait, and the scheduler's wake signal.
- How `SchedulerActivity` is implemented per plugin.
- The wording of the log lines and help topics.

## Calls made in this document (not discussed with Paul)

- **Gossip carriers.** The setting rides a gossip topic for changes and
  memberlist's join-time state exchange for nodes that join. Highest version
  wins.
- **Setting a past time** is checked against the store's consistency time.
  On memory and sqlite, a read made before the start raises the earliest
  allowed start; the help topic says to set the start first.
- **A move stops** with `CLOCK_CHANGED` if another change appears. An
  interrupted move leaves the clock where it got to.
- **A rate change together with a move** applies at the end of the move.
- **The restart check's tolerance** is 1 s.
- **`advanceByMs`** is in milliseconds, like other API durations.
- **The route** is `/platform/clock`. Changes are HTTP-only; gRPC can only
  read.
- **Real time stays** for IAM dates and v1 UUIDs.
- **Stale reclaim.** The insert writes `heartbeat_time`, removing the
  two-clock fallback in production.
