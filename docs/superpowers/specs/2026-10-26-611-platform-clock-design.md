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
2. **Test preconditions.** Three conditions are the test's job. They are
   documented, and the code does not enforce them:
   - the cluster is static during a change: no node starts, stops or joins;
   - the system is idle during a change: no requests or callouts run;
   - one change at a time: the next change starts only after the previous
     call has returned.
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
8. **Confirmed changes.** A change answers 200 only when every node holds the
   new setting. Any node that does not confirm it fails the change with 503
   `CLOCK_NOT_CONFIRMED`. Nothing is rolled back; starting over means
   restarting the cluster with a fresh store.

## Goals

1. One clock object per process, injected wherever business time is read.
   All clock logic is in one type.
2. Production behaves as today: real time, from today's sources.
3. With the feature on, the operator can set the clock to any time not
   earlier than the latest time the store has recorded, change the rate, and
   move it forward.
4. A move fires every scheduled transition that comes due on the way, each at
   its own due time.
5. When a change answers 200, every node of the cluster holds the same clock
   setting.
6. Applications and compute nodes can read the platform time.

## Non-goals

- **Durability.** A full restart returns the clock to real time.
- **Guarantees for activity during a change.** Transactions in flight during
  a change are not protected (decision 2).
- **Rollback.** A change that some node did not confirm is not undone
  (decision 8).
- **Partitions.** A node cut off from gossip but still reaching the database
  is out of scope (decision 1). It is not in the handling node's directory, so
  no change is pushed to it.
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
    machine's wall clock does not jump the platform time. A setting received
    from another node carries `R` as Unix time; the receiving node converts it
    once to a local monotonic anchor.
  - `Now()` never fails and never does I/O.
- **Selection:** the host builds one or the other at startup, from
  `CYODA_CLOCK_CONTROL_ENABLED`, and passes it through constructors to every
  component that reads business time. This is the project's existing
  injection style (`app/app.go`).
- **Existing clock hooks are replaced by it and deleted:**
  - `scheduler.Clock` and `NewRealClock` (`internal/scheduler/clock.go`);
  - the engine's function-typed `WithScheduledClock`
    (`internal/domain/workflow/engine.go:213-219`).

  One clock type is left; no second abstraction for the same thing.

### What reads the clock, and what stays on real time

**Business time, reads the clock:**

| Site | Today |
|---|---|
| scheduler claim and bookkeeping (`internal/scheduler/service.go:607`, `:690`) | `scheduler.Clock`, real |
| engine arm, fire decision, audit timestamps (`internal/domain/workflow/engine.go:173,236,1110`, `arm.go:115`, `fire_scheduled.go:91,186`) | engine clock, real |
| entity pre-commit `CreationDate`/`LastModifiedDate` (`internal/domain/entity/service.go:282,2015,2271,2528`) | `time.Now()` |
| model `UpdateDate` (`internal/domain/model/service.go:195`) | `time.Now()` |
| search job `CreateTime` and finish times (`internal/domain/search/service.go:967,1183,1189,1208,1216,1228,1315,1496`, `reaper.go:258,292`) | `time.Now()` |
| callout request CloudEvent `time` attribute (`internal/grpc/dispatch.go:161`) | not set |
| storage plugins' business stamps (see "Plugins") | plugin clock, or the database clock |

**Operational time, stays on real time:**
- everything under `internal/auth` and the key and trusted-key adapters in
  `internal/domain/account`, including M2M client dates and key `validFrom`
  defaults. A clock change must never change which credential is valid.
  Tokens from an external identity provider carry real times anyway.
- callout deadlines, gossip, keep-alives, heartbeats, watchdogs, the
  model-cache lease, member `ConnectedAt`, metrics;
- v1 UUIDs (`internal/common/uuid.go:22`). They are identifiers; the audit
  listing uses their time only to break ties (`internal/domain/audit/events.go:144`).

The build lists every remaining `time.Now`/`time.Since`/`time.Until` in
non-test code and records each as business or operational. The research notes
have the starting inventory.

### Platform time on callout requests

- **What carries it:** processor, criterion and schedule-function requests to
  compute nodes carry the standard CloudEvents `time` attribute.
- **Its value:** the platform time at which the owning node took the callout
  up. The owner reads its clock once, before the first try, to the
  microsecond; every try sends that value, and a hand-over carries it
  (`atUnixMicro`, required), so a peer sends the owner's time and never its
  own clock's reading. Retries and hand-overs send the same time. A callout
  without one is not sent.
- **Why:** a schedule function that returns an absolute fire time computes it
  on the platform's clock, not the compute node's.
- **Cloud:** it stamps every CloudEvent with its JVM clock at conversion
  (`~/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas/grpc/ProtobufMessageConverter.kt:153`),
  a new value on each try; aligning means one value per callout, kept across
  retries.
- **Other CloudEvents** (keep-alives, API responses, errors) are unchanged.

## The setting across a cluster

The setting is a small value: `S`, `R` (as Unix time), `k`, and a version made
of a counter plus the node id that wrote it (for a total order)
(`Setting`, `internal/platformclock/setting.go:24-30`). Each node holds it in
its test clock, and a node's clock reading follows entirely from it.

Nodes compute the platform time from their own real clocks; there is no
master. That is how NTP works too: it distributes a reference, and each
machine keeps time locally.

### A change is pushed and confirmed

Every kind of change runs the same order: a set, a set-back, a rate-only
change, and each step of a move.

1. **Apply locally.** The handling node installs a new setting in its own
   clock. Every install bumps the version and stamps the handling node as
   writer (`Test.install`, `internal/platformclock/clock.go:157-186`), so
   every change ends with a fresh setting. A set-back first runs its store
   check (`internal/platformclock/service.go:166-181`);
   when the check refuses, the previous setting is restored and nothing is
   pushed.
2. **Push.** The node marks the setting shareable (`Test.share`,
   `internal/platformclock/clock.go:194-205`) and sends it to every other live
   member in parallel, over the peer channel (below).
3. **Hold and echo.** Each receiving node applies the setting
   (`Test.Apply`, `clock.go:96-117`, newest version wins) and answers with the
   full setting it holds afterwards (`Test.Setting`, `clock.go:93-94`). A
   setting equal to the one held is not re-applied, and the echo still matches.
4. **Match.** A node's confirmation counts only when its echo equals, in all
   five fields, the setting the handling node sent.
5. **Wait for all.** The change is confirmed when every other live member has
   confirmed it. The handling node's own scheduler may fire a step's tasks
   before the peers hold that step; this is accepted, because the system is
   idle (decision 2) and nothing else runs in that moment.

**Every change ends by installing and confirming a fresh setting.** A node's
starting setting has version 0, an empty writer, and a start and anchor taken
from that node's own `time.Now()` (`NewTest`, `clock.go:45-62`); each node's
differs, and validation refuses an empty writer (`sync.go:42-43`). So the
handling node never confirms a setting it did not install in this change.
Where a change would otherwise end without a new setting — a move whose last
step made none (`setForward` reports no change,
`internal/platformclock/service.go:220-224`), such as a short advance whose
target the clock has already passed or a move with no due tasks — the
handling node installs a new setting re-anchored at its current platform
time, with the requested rate, or the current one when none is requested
(`Test.set` with no time, `clock.go:119-126`). That setting is the one every
node must confirm. So a 200 always means every node holds the same setting,
and every change bumps the version.

**Every other live member** is every member in the handling node's gossip
directory (`internal/cluster/registry/directory.go:11-29`) except itself. A
member whose metadata does not parse has no usable address and fails the
confirmation. Today `List` skips such a member
(`internal/cluster/registry/gossip.go:438-450`) and `Lookup` reports it not
alive (`gossip.go:416-431`), so the confirmation reads the directory through a
registry method that returns it too.

**Failure.** The change fails with 503 `CLOCK_NOT_CONFIRMED` when any member
does not answer within the bound, refuses (clock control is off on it, or the
setting fails its validation), echoes a different setting, or has no usable
address. The detail names the id of every node that did not confirm. The
forwarder's error text and the peers' URLs go to the log only. Nothing is
rolled back: the handling node and the nodes that confirmed keep the setting.
Starting over means restarting the cluster with a fresh store.

**What a 200 means.** Every node holds the same setting. Readings still differ
by the difference between the hosts' real clocks multiplied by the rate:
10 ms of NTP difference is 1 s at rate 100. The documentation says the nodes
need synchronised clocks.

**Single-node deployments** have no peers and no peer channel. A change is
confirmed once it is applied, and the 200 lists the handling node only, whose
id is `local` (`app/app.go:157`).

### The peer channel

The push reuses the authenticated channel that callout hand-overs use
(`internal/cluster/dispatch`).

- **Route.** A new route, `POST /internal/dispatch/clock`, is registered beside
  the hand-over route (`internal/cluster/dispatch/handler.go:50-53`), at the
  root and outside the context path (`app/app.go:731-734`, `:743-746`), on
  every node in cluster mode, whether clock control is on or off.
- **Authentication.** The request is sealed for its recipient, and the
  associated data binds the method, the path, the timestamp and the recipient's
  node id (`internal/cluster/dispatch/aead_peer_auth.go:92-105`, `:300-301`).
  The answer is sealed for the request it answers (`aead_peer_auth.go:303-311`).
  So a hand-over envelope cannot be replayed to the clock route, nor a clock
  envelope delivered to another node. A request that fails authentication gets
  the bare 403 the hand-over route gives today (`handler.go:75-78`), which the
  sender reads as no confirmation.
- **Refusals.** A node with clock control off answers with a sealed refusal. A
  node with it on validates the setting as it validates any setting from the
  network (`decode`, `internal/platformclock/sync.go:29-46`) and answers a
  sealed refusal when it fails.
- **Forwarder.** `HTTPForwarder` (`internal/cluster/dispatch/forwarder.go:62-106`)
  gains a call for the clock route. It reuses the peer-address check
  (`forwarder.go:122-130`) and the sealed round trip (`forwarder.go:146-225`).
  The forwarder is built in cluster mode regardless of `ExternalProcessing`;
  today it is built only on the branch where `ExternalProcessing` is unset
  (`app/app.go:495-512`).
- **Parallel, bounded.** All peers are contacted at once. Opening each
  connection is bounded by `CYODA_DISPATCH_CONNECT_TIMEOUT` (the forwarder's
  dialer, `forwarder.go:81-105`). The whole confirmation round is bounded by
  `CYODA_PROXY_TIMEOUT`.
- **Client gone.** The round's context derives from the request. A client that
  leaves cancels it, and the error is marked `ClientGone`
  (`internal/common/errors.go:54-56`), as a move's store calls are
  (`service.go:264-272`).

### Joining nodes: the join exchange only

- **Kept.** A node that joins takes the current setting from the node it joins
  through, in memberlist's join-time push/pull exchange
  (`LocalState`/`MergeRemoteState`, `internal/cluster/registry/gossip.go:552-558`,
  `:625-638`), which `Attach` registers (`internal/platformclock/sync.go:84-104`).
  This is the only remaining use of gossip for the clock.
- **Join only.** The periodic push/pull does not carry clock settings: the
  delegate hands a remote state to the merge only when memberlist's `join`
  flag is true. Today the flag is ignored (`gossip.go:634`). Both sides of a
  join exchange see `join` true (memberlist v0.6.0 `net.go:321`, `:978`,
  `:1052`, `:1315`).
- **Removed.** The broadcast topic `platform.clock` (`sync.go:14-15`), its
  subscription (`sync.go:90`), `Publish` (`sync.go:106-110`) and the publish
  wiring (`app/app.go:597-601`). The direct push replaces them.
- **What is shared.** A node hands a joining peer only a setting it pushed or
  adopted (`clock.go:39-42`, `:207-211`), never a local change still pending,
  such as a set-back awaiting the store's answer, which may yet be undone.
- **Only with the feature on.** `Attach` runs only with clock control on
  (`app/app.go:192-199`); with it off, `LocalState` returns nothing, as today.
- **Before background work.** A node joins and merges the setting before it
  starts any background loop (search reapers, scheduler). Gossip registration
  comes before the search reapers start, in both modes (`app/app.go:444-475`).

## Changing the clock

All changes are made through `POST /platform/clock` by a platform operator, on
a node with `CYODA_CLOCK_CONTROL_ENABLED=true`. The test keeps the cluster
static and the system idle meanwhile, and makes one change at a time
(decision 2). Every change, and every step of a move, is pushed to the other
nodes and confirmed by them before the request goes on (see "A change is
pushed and confirmed").

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
4. Otherwise it pushes the setting and waits until every other node confirms
   it.

On a fresh store the floor is zero, so any start from 1970 on is accepted. The
help topic says to set the start before anything else. Every operation that
obtains a consistency time moves the floor on every backend (a point-in-time
read, an async search), and on memory and sqlite every transaction does
(their snapshot time). Any of these done first raises the earliest allowed
start.

A `time` together with a `rate` sets both in one change.

### Moving forward (step-through)

A target `T` later than now runs this loop on the node that received the
request:

```
loop:
  wait until no scheduled task is RUNNING and none WAITING is due (≤ now)
  e = earliest NextAttemptTime of any WAITING task, all tenants
  if there is none, or e > T:
      if now < T: set the clock to T
      apply the requested rate, if any; done
  if now < e: set the clock to e
```

The clock keeps running between the query and the set, and during each
fire. So a set never moves it back: an intermediate step goes to `e` only if
`e` is still ahead, and the final step goes to `T` only if `T` is still ahead.
If a fire near the end ran past `T`, the move ends with the clock where it is.

- **Each step** is an ordinary change: applied locally, pushed, and confirmed
  by every other node before the move waits for that step's tasks. A step
  that is not confirmed stops the move there with 503 `CLOCK_NOT_CONFIRMED`.
- **The end of a move** installs and confirms a fresh setting when the last
  step made none (`setForward` reports no change,
  `internal/platformclock/service.go:220-224`): re-anchored at the current
  platform time, with the requested rate, and a new version (see "Every
  change ends by installing and confirming a fresh setting"). So every move
  answers 200 only with a final setting of its own, confirmed by every node.
- **The scheduler** on every node then fires what is due, as it does on real
  time. A fire is recorded at `e` plus the scan latency times the rate. The
  default scan interval is 1 s, so at rate 1 a fire is recorded within about
  a second of its due time, as on real time.
- **Fires that arm or retry** add tasks that the next pass finds. So a
  transition armed by an earlier fire inside the window is reached in its
  place.
- **"Wait"** polls the store, so it sees tasks on every node.
- **Due order holds between due times further apart than one scan interval
  times the rate.** Tasks that come due within that span of each other are
  claimed in the same scan and run concurrently (up to `MaxRuns`, 8, and 4 per
  tenant, `app/config.go:457-459`), so their commit order is not their due
  order. That is also how they behave on real time. The help topic says so.
- **One change at a time.** Two changes at once are the test's mistake
  (decision 2), not something the platform detects. A node that already holds
  a newer setting than the one pushed echoes its own, and the change fails
  with 503 `CLOCK_NOT_CONFIRMED`.
- **Interruption.** If the request is cancelled or the node stops, the clock
  stays where the last step put it, running at its rate. Repeating the move
  with the same `time` continues from there; `advanceByMs` counts from the
  clock's reading at the request.
- **Cost:**
  - one step per distinct due time in the window, including retries and
    schedules that re-arm themselves;
  - each step costs one confirmation round, up to one scan interval on the
    other nodes, plus the fire;
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
    and checks the store. The check reads the store's latest recorded time
    (`TransactionManager.LatestRecordedTime`, asked with tenant `PLATFORM` in
    the context). That is the floor, read without raising it; it is zero on
    a fresh store. If it is more than 1 s later than real time, startup
    fails: "the store holds times later than real time; start over with a
    fresh store" (decision 7). The floor is not raised, so the operator's
    first setting in the past is still accepted after the check.
  - The check runs only when no peer supplied a setting after the cluster
    join.
  - The check is a function that returns an error; startup exits on it, as on
    every other startup failure (`app.New` reports failures with
    `os.Exit(1)`).
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
- 200 `PlatformClockChangeResultDto` when complete:
  - `now`, `rate`, `controllable`, as in `PlatformClockDto`;
  - `nodes` (array of strings, required): the ids of every node that holds
    the setting, the handling node included, in ascending order. On a single
    node it is `["local"]`.

  `GET /platform/clock` and the gRPC `PlatformClockResponse` keep
  `PlatformClockDto` unchanged. Spec's call: a new response schema for POST,
  not an optional member, so that GET declares no member it never sends.

Errors, in the order they are checked:

| Status | Code | When | GET | POST |
|---|---|---|---|---|
| 400 | existing query-parameter refusal | undeclared query parameter | ✓ | ✓ |
| 401 | — | missing or invalid credentials | ✓ | ✓ |
| 403 | existing operator refusal | not a platform operator, or on behalf of a user | — | ✓ |
| 403 | `CLOCK_CONTROL_DISABLED` | the feature is off on this node | — | ✓ |
| 400 | existing strict-body / validation codes | malformed body, unknown member, null member, no member, both `time` and `advanceByMs`, `advanceByMs ≤ 0` or moving the clock past 9000, `rate` outside 0.1–100, `time` not RFC 3339 or outside 1970–9000 | — | ✓ |
| 409 | `CLOCK_BEFORE_RECORDED_TIME` | `time` earlier than the latest recorded time; the detail names it | — | ✓ |
| 503 | existing store-unavailable | the store did not answer the consistency-time check or the move's task query | — | ✓ |
| 503 | `CLOCK_NOT_CONFIRMED` | a node did not confirm the change or a move's step: no answer within `CYODA_PROXY_TIMEOUT`, a refusal (clock control off on that node, or the setting fails its validation), an echo that differs, or a member without a usable address; the detail names the node ids; not retryable — start over | — | ✓ |
| 500 | generic + ticket | anything else | ✓ | ✓ |

`GET` makes no store call and contacts no peer, so it has no 503, and no 500
that a request could cause.

`CLOCK_NOT_CONFIRMED` is checked after the local change: a set-back that the
store refuses answers 409 and pushes nothing. A client that leaves during a
confirmation round gets no answer; the error is marked `ClientGone` and the
door logs it at DEBUG.

### gRPC

- Read only. CloudEvent types `PlatformClockGetRequest` and
  `PlatformClockResponse` (`now`, `rate`, `controllable`), next to
  `EntityConsistencyTimeGetRequest` (`internal/grpc/cloudevent_types.go:84-85`).
  Each has a published JSON schema (#626).
- Error classes: unauthenticated. Like every gRPC operation, the read
  requires `ROLE_M2M` (`PermissionDenied` otherwise); HTTP `GET /platform/clock`
  does not.
- Changing the clock is HTTP-only, like the other operator controls.

## Configuration

`CYODA_CLOCK_CONTROL_ENABLED` (bool, default `false`). When true:
- the node builds the test clock and passes it to the storage plugin;
- the node accepts `POST /platform/clock`;
- startup logs a WARN: "platform clock control is enabled: test feature, not
  for production; the clock is instance-wide and affects every tenant".

All nodes of a cluster use the same value.

`CYODA_PROXY_TIMEOUT` (duration, default `30s`, `app/config.go:452`) bounds
each confirmation round. `ValidateDispatch` (`app/config.go:927-941`) refuses
a value that is not greater than zero; today it is not checked. The
`config cluster` help topic (`cmd/cyoda/help/content/config/cluster.md:27`)
says both. `README.md` does not list the variable (`grep -n CYODA_PROXY_TIMEOUT
README.md` is empty) and the default is unchanged, so neither needs a change
(Gate 4).

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
- **`TransactionManager.LatestRecordedTime(ctx) (time.Time, error)`:**
  returns a time at or above the latest submit time, SnapshotTime or
  consistency time the store has issued (sqlite seeds it from a persisted
  high-water mark). It does not raise the floor and does not wait. It returns
  the zero time on a store that has issued none. The restart check uses it.
  It is a new interface method, so it is breaking for out-of-tree plugins.
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

- **Stamp functions.** A new migration (000017) re-creates `cyoda_stamp`
  and `cyoda_consistency_time` (`migrations/000016_consistency_time.up.sql:39-57`,
  `:59-82`) with an optional time argument.
  - NULL (production) uses `clock_timestamp()` exactly as today.
  - A value (test clock) is used instead. The floor rules are unchanged.
- **Placeholder dates.** Placeholder dates fill a row until the commit stamp
  replaces them, and reads inside the same transaction see them
  (`entity_store.go:613-614`), so they must be platform time. They come from
  two places:
  - the `SELECT CURRENT_TIMESTAMP` at `entity_store.go:242`, `:774`;
  - columns whose inserts wrote no value and took the column default: `entities.creation_date` and
    `last_modified` (`entity_store.go:298-310`; `000012_commit_instant.up.sql:20-22`)
    and `entity_versions.transaction_time` (`entity_store.go:372-380`, `:836`;
    `000001:35`).

  With a test clock, the select takes the clock's time, and the inserts write
  these columns explicitly from that value. Without one, the inserts write the
  `CURRENT_TIMESTAMP` value. Migration 000017 drops the placeholder-date
  `DEFAULT`s on `entities.creation_date` and `last_modified` and on
  `entity_versions.transaction_time` and `creation_date`
  (`000017…up.sql:74-77`), because every insert now writes them. The heartbeat
  backfill and `NOT NULL` are at `000017…up.sql:79-80`.
- **Submit-time pruning and search-job reaping.** Today these compare business
  stamps with Go's `time.Now()` (`transaction_manager.go:354`, `:543-560`;
  `search_store.go:587`). With a test clock they use it, so both sides are
  platform time. Without one they are unchanged.
- **Stale reclaim:** see "Search-job liveness" below.
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

### Search-job liveness (all backends, SPI contract)

Stale reclaim measures from `HeartbeatTime`, and from the engine-supplied
`CreateTime` while a job has never heartbeated. That is the SPI contract
(`cyoda-go-spi/search_store.go:47-50`, enforced by spitest
`Claim/NilHeartbeatBaseline`, `spitest/asyncsearch.go:455-466`). It compares a
business time with the store's physical clock on every backend:
- memory `search_store.go:388-390`;
- sqlite `:408`, `:418`;
- postgres `:630`.

With a past clock, every new job looks stale until its first heartbeat, is
reclaimed, and is fenced out.

The contract changes:
- `CreateJob` stamps `HeartbeatTime` from the store's physical clock, and the
  `CreateTime` fallback is removed in all three plugins;
- liveness is then judged on one clock, like every other liveness check;
- the spitest cases change in the SPI PR.

In production this changes the baseline of a never-heartbeated job from the
engine's clock to the store's, a difference within host–store skew.

### cassandra (separate repository)

An issue in `cyoda-go-cassandra`, before its v0.9.0 bump:
- take `spi.WithClock` as the physical part of its hybrid logical clock;
- generate transaction-id and audit timeuuids from it (recovery compares them
  with the floor, `internal/tx/tx_recovery.go:330-337`);
- `period_bucket`;
- the stub `SchedulerActivity`;
- the search-job liveness contract change.

## Production impact

With the feature off, a production deployment differs from today in these
ways and no others.

**Behaviour on existing paths:**
1. **Clock calls.** Business-time sites call `clock.Now()` on an injected
   clock that returns `time.Now()`, instead of calling `time.Now()` directly.
   The scheduler and engine clock hooks are replaced by it.
2. **Callout `time` attribute.** Callout requests carry the CloudEvent `time`
   attribute.
3. **Postgres stamp functions** are re-created by migration 000017 with an
   extra argument, NULL in production. They take the same branch as today.
   A re-created function gets the default `EXECUTE` for `PUBLIC` again, so a
   deployment that revoked it re-applies its revoke.
4. **Postgres inserts** write the placeholder dates explicitly. The values are
   the same as the column defaults give today.
5. **Search-job liveness.** On all backends, a new job's liveness baseline is
   the store's clock at insert, not the engine's `CreateTime`.
6. **Dead and placeholder defaults, and dead columns** are dropped. Migration
   000017 drops the placeholder-date `DEFAULT`s on `entities.creation_date`
   and `last_modified` and on `entity_versions.transaction_time` and
   `creation_date`, because every insert now writes them.
7. **Sqlite snapshot time.** `Begin` reads its snapshot time inside the commit
   gate, so the recorded SnapshotTime can be later by the gate wait.
8. **Search-job heartbeat backfill.** Postgres 000017 and sqlite 000013 set
   `heartbeat_time` from the creation time for existing rows, and 000017 then
   makes `search_jobs.heartbeat_time` `NOT NULL`.
9. **Wording and construction.** The `ErrClientGone` message, logged at DEBUG
   only, reads "the request's client went away". The scheduler reads the
   clock before taking its lock. Every host-side business-time constructor and
   option refuses a nil clock with a panic at startup; a plugin given no clock
   uses its own, as production postgres does. `memory.NewAsyncSearchStore`
   is removed.
10. **Cluster mode wiring.** Every node in cluster mode registers the clock
    route `POST /internal/dispatch/clock` and answers every push with a sealed
    refusal, because clock control is off. The dispatch forwarder is built in
    cluster mode whatever `ExternalProcessing` is; production leaves
    `ExternalProcessing` unset (`app/app.go:495-512`), so production already
    builds it, and only an embedder that sets it sees a difference.
11. **`CYODA_PROXY_TIMEOUT` validation.** A value not greater than zero fails
    startup. The default, `30s`, passes.

**Additions that production does not exercise:**
- `GET /platform/clock` (answers real time, rate 1) and `POST /platform/clock`
  (answers 403 `CLOCK_CONTROL_DISABLED`);
- the gRPC read types, the new error codes, and the OpenAPI and schema entries;
- the SPI methods `SchedulerActivity` and `LatestRecordedTime`, which every
  plugin implements and production never calls.

The state exchange is active only with the feature on, and it merges only
during a join (`join` true); with the feature off nothing is registered on it,
so the join-only rule changes nothing. The broadcast topic is gone.
Gossip registration now happens before the search reapers start, in both
modes, so with the feature off the reapers start after the cluster join.
No new store reads, locks, checks or background work run in production.

## Security

- **Changing the clock** needs a platform operator (`OperatorGuard` refuses
  on-behalf-of tokens) on a node with the feature on. The feature is off by
  default.
- **Instance-wide by design.** The clock is the same for every tenant. It
  holds no tenant data, but while a move runs the stepped clock lands on the due times of scheduled tasks in every tenant, so any caller reading the clock sees those times (not which tenant they belong to). The startup WARN and the help topic say so.
- **Credentials.** No credential, key or token validity reads the platform
  clock.
- **Input validation:**
  - strict body;
  - rate within 0.1–100;
  - times RFC 3339 within 1970–9000, and an advance may not move the clock past 9000;
  - problem details name the refused member.
- **Information in errors.** `CLOCK_BEFORE_RECORDED_TIME` names the latest
  recorded time. Any caller can read that already as a consistency time.
  `CLOCK_NOT_CONFIRMED` names node ids only, to a platform operator. Node
  addresses, peer URLs and the forwarder's error text go to the log, never to
  the response.
- **Peer route.** `POST /internal/dispatch/clock` accepts only an envelope
  sealed with the cluster key for this node and this path (see "The peer
  channel"), the same trust as a callout hand-over: whoever holds the cluster
  key can already hand this node a callout. It is outside the context path and
  outside API authentication, like the hand-over route. Every answer to an
  opened request is sealed. A node with clock control off only refuses. A
  received setting is validated as any setting from the network
  (`sync.go:29-46`); a failed validation is logged without the payload.
- **SSRF.** The push dials only addresses from the gossip directory, through
  the forwarder's peer-address guard (`forwarder.go:122-130`), which refuses
  loopback unless `CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING` is set.

## Documentation

- **New help topic `cyoda help clock`:**
  - what the clock reads;
  - setting a start in the past (first, on a fresh store);
  - moves and their cost;
  - the rate and its effect on time limits and retries;
  - the restrictions: keep the system idle during changes, nodes need
    synchronised clocks, a restart returns to real time or fails on a store
    with later times;
  - the instance-wide effect;
  - a section "Testing with the platform clock":
    - the three preconditions: the cluster is static during a change, the
      system is idle, one change at a time;
    - a 200 means every node holds the setting; the answer lists the nodes;
    - on failure (`503 CLOCK_NOT_CONFIRMED`), start over: restart the cluster
      with a fresh store;
    - set the start in the past before writing data or importing models;
    - retention follows platform time;
    - a move needs a running scheduler and the compute nodes its tasks call;
    - a node that is starting, or shutting down, while a change runs can make
      the change fail;
    - synchronised host clocks are required.
- **New error topic** `cmd/cyoda/help/content/errors/CLOCK_NOT_CONFIRMED.md`,
  and the code in `internal/common/error_codes.go` beside the other clock
  codes (`:167-173`, `:312-313`).
- **Existing topics:**
  - `cyoda help config clock`;
  - `cyoda help config cluster`: `CYODA_PROXY_TIMEOUT` bounds a clock
    confirmation round and must be greater than zero;
  - `cloudevents.md` (the `time` attribute);
  - `scheduled-tasks.md` (moves fire due tasks at their due times);
  - `crud.md` (point-in-time "now" follows the clock).
- **OpenAPI** for both operations; **gRPC** schemas for the two types. POST's
  200 names `PlatformClockChangeResultDto` (today `PlatformClockDto`,
  `api/openapi.yaml:1087-1092`). POST's 503 gets its own description naming
  the store codes and `CLOCK_NOT_CONFIRMED`, in place of the shared
  `ServiceUnavailable` response (`api/openapi.yaml:1115-1116`). Then
  `go generate ./api`.
- **`docs/cloud-parity/platform-clock.md`** (Gate 7), and a CaaS ticket. Cloud
  has no controllable clock (`ClockCtrl` is replaceable only by tests). The
  page states the contract a Cloud cluster follows: a change answers 200 only
  when every node holds the setting, the 200 body lists the node ids
  (`PlatformClockChangeResultDto`), and a node that does not confirm gives
  503 `CLOCK_NOT_CONFIRMED` with no rollback. Its §2 drops "highest version
  wins" as the means of spreading a change; a joining node still takes the
  setting from its peers. How Cloud nodes reach each other is Cloud's own.
- **CHANGELOG:**
  - `### Added` for the feature;
  - `### Breaking` for plugin authors (`SchedulerActivity`,
    `LatestRecordedTime`, and `CreateJob` stamping `HeartbeatTime`).

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
| Move order: transitions of three entities in two tenants, due times spaced wider than scan interval × rate, fire in due order, each within one scan interval of its due time; a transition armed by an earlier fire inside the window fires in its place; a task due exactly at the target does not move the clock back | ✓ | ✓ | | | |
| A transition with a time limit shorter than the move fires and is not expired | | ✓ | | | |
| Rate change does not jump the time; rate 2 runs twice as fast | ✓ | ✓ | | | |
| Plugin stamps follow an injected clock; without it they are as today | | | | | ✓ |
| Operational checks stay on the physical clock with an injected clock far from real time (owner not swept, running search job not reclaimed) | | | | | ✓ |
| With a past clock: an async search's results are not reaped early; `GetSubmitTime` still answers | | ✓ | ✓ | | ✓ |
| `CLOCK_BEFORE_RECORDED_TIME` after a write, with the latest time in the detail | ✓ | ✓ | ✓ | | |
| Interrupted move: the clock stays where the last step put it | | ✓ | | | |
| Repeating an interrupted move continues from where the last step put the clock | ✓ | | | | |
| In-transaction read of a new entity under a test clock returns platform-time placeholder dates (postgres) | | ✓ | | | |
| `SchedulerActivity`: earliest WAITING across two tenants, the RUNNING flag, an empty store | | | | | ✓ |
| Search-job liveness: a new job carries `HeartbeatTime` at insert and is not reclaimed before it is stale on the store's clock | | | | | ✓ |
| Feature off → 403 `CLOCK_CONTROL_DISABLED`; GET returns real time | | ✓ | | | |
| Non-operator → 403; on-behalf-of → 403; 401 on both operations | | ✓ | | | |
| Each 400 cell, including the query-parameter refusal | ✓ | ✓ | | | |
| POST 503 when the store fails the check | ✓ | waived: no e2e stops the database mid-test; handler unit test | | | |
| POST 500 | ✓ | waived: no e2e can force an internal fault; handler unit test | | | |
| Cluster: a change through node A answers 200 only once node B holds it; `nodes` lists every node; B reads the setting at once; a node that joins later takes the setting | ✓ | ✓ | | | |
| Single node: the 200 lists the handling node only (`local`) | ✓ | ✓ | ✓ | | |
| 503 `CLOCK_NOT_CONFIRMED`, unreachable peer: a gossip member whose address serves nothing; the detail names it | ✓ | ✓ | | | |
| 503 `CLOCK_NOT_CONFIRMED`, a peer with clock control off answers a sealed refusal; the detail names it | ✓ | ✓ | | | |
| 503 `CLOCK_NOT_CONFIRMED`, a peer echoes a different setting | ✓ | waived: needs two concurrent changes, which the preconditions exclude; the service test drives a peer that echoes a newer setting | | | |
| 503 `CLOCK_NOT_CONFIRMED`, a member whose metadata does not parse | ✓ | waived: no cyoda-go node advertises such metadata; registry and service unit tests | | | |
| 503 `CLOCK_NOT_CONFIRMED` on a move step: the move stops at that step, the clock stays there | ✓ | | | | |
| A move confirms each step before it waits for that step's tasks, and installs and confirms a fresh final setting when the last step made none | ✓ | | | | |
| A short advance on a fresh cluster (the target already passed when the move ends) answers 200: a fresh setting with a new version is confirmed by every node | ✓ | ✓ | | | |
| A move with no due tasks answers 200 with a fresh setting confirmed by every node | ✓ | ✓ | | | |
| Every change bumps the version and stamps the handling node as writer; the starting setting (version 0, empty writer) is never pushed | ✓ | | | | |
| A set-back refused by the store pushes nothing | ✓ | | | | |
| A client that leaves during a confirmation round: `ClientGone`, logged at DEBUG | ✓ | waived: an e2e cannot read an answer its client has abandoned | | | |
| Peer route: refusal with clock control off; refusal of a setting that fails validation; an envelope sealed for another path or node is refused | ✓ | | | | |
| Periodic push/pull does not merge a setting; the join exchange does | ✓ | | | | |
| `CYODA_PROXY_TIMEOUT` ≤ 0 fails config validation | ✓ | | | | |
| Restart alone on a store with times ahead of real time → the check returns an error; behind → starts on real time | ✓ | waived: startup failure exits the process (`app.New`, `os.Exit(1)`), and e2e runs the app in-process; the check function is unit-tested on a real store | | | |
| Callout `time` attribute carries the clock's time; an absolute schedule-function fire time computed from it is due at the right platform time | ✓ | ✓ | | | |
| Callout `time` attribute is the owner's platform time when it took the callout up: a retry and a hand-over send the same value; a peer sends the hand-over's time, not its own clock's; a hand-over without one is refused | ✓ | | | | |
| Stale reclaim of a never-heartbeated job judged on the database clock | | | | | ✓ |
| gRPC `PlatformClockGetRequest`: success, unauthenticated | | | | ✓ | |

## Rollout

1. **SPI PR:** `Clock`, `WithClock`, `SchedulerActivity`, spitest hook and
   cases. Then the pseudo-pin and `COMPATIBILITY.md`.
2. **cyoda-go PR into `release/v0.9.0`.**
3. **The cassandra issue and the CaaS ticket.**

## Left to the builder

- Package and type names; the peer route's path and the wire encoding of the
  push, its echo and its refusal; the registry method that lists every
  directory member.
- The e2e cluster fixture. `newClockSeed`
  (`internal/e2e/platform_clock_test.go:664-697`) is a gossip-only member whose
  address serves nothing (`:679`), so it fails every cluster change. It either
  serves the clock route, or the cluster test joins through a real node; the
  member that serves nothing becomes the unreachable-peer 503 test.
- The poll interval of the move's wait.
- How `SchedulerActivity` is implemented per plugin.
- The wording of the log lines and help topics.

## Calls made in this document (not discussed with Paul)

- **Carriage.** A change is pushed directly to every node over the peer
  channel and confirmed by each; memberlist's join-time state exchange carries
  the setting to a node that joins. Highest version wins on every node.
- **POST's 200 schema** is a new `PlatformClockChangeResultDto`, so GET and
  gRPC keep `PlatformClockDto`.
- **Setting a past time** is checked against the store's consistency time.
  On memory and sqlite, a read made before the start raises the earliest
  allowed start; the help topic says to set the start first.
- **One change at a time** is documented, not detected. An interrupted move
  leaves the clock where it got to. There is no wake signal for the scheduler;
  each step waits up to one scan interval.
- **A rate change together with a move** applies at the end of the move.
- **The restart check's tolerance** is 1 s.
- **`advanceByMs`** is in milliseconds, like other API durations.
- **The route** is `/platform/clock`. Changes are HTTP-only; gRPC can only
  read.
- **Real time stays** for IAM dates and v1 UUIDs.
- **Search-job liveness:** the SPI contract change described above, on all
  backends.
