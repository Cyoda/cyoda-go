# A controllable platform clock (#611)

## What this is

The controllable clock is a test feature. A deployment that turns it on can:
- start the platform's clock at a configured time, in the past or the future;
- run it at a configured rate: frozen, real speed, faster or slower;
- later move it forward through an operator API.

When the clock moves forward, every scheduled transition that comes due on the
way fires at its own due time, in due order.

Every part of the platform that records or compares a business time reads this
one clock. Applications read the same clock from the platform, so the times
they record agree with the platform's own.

The clock is instance-wide, so by definition it affects every tenant.

Research notes, with the code citations this design rests on:
`docs/superpowers/research/2026-10-26-611-platform-clock-research.md`.

## Why

An application on cyoda-go needs tests, demonstrations and example data that
span days, months or years. Today it can move only the time it owns. The
platform stamps revisions, audit events and scheduled fires on real time.

The first user, CTCC, seeds collateral data over several business days. After
such a seed, the application says a pledge was submitted on day 3, while the
platform's audit event for the same submission says "a minute ago". For a
treasury system the audit trail is the record it has to defend, so the two must
agree.

## Terms

- **Real time:** the physical clock the store already uses for its stamps.
  - postgres: the database server's `clock_timestamp()`.
  - memory and sqlite: the plugin's `Clock` (Go `time.Now()` in production).
- **Platform time:** the time the platform reads for business purposes.
- **Clock line:** how platform time follows real time. It has three values:
  - a start time `S`;
  - the real time `R` at which `S` was set;
  - a rate `k`.

  Platform time at real time `r` is `S + k × (r − R)`. With `k = 1` it runs at
  real speed; with `k = 0` it is frozen; with `k = 60`, one real minute is one
  platform hour.
- **Version:** a number stored with the line. It increases on every change of
  the line, and is 0 for a store whose line was never set.
- **Floor:** the existing value in each store that never goes back (#649). It
  moves to each submit time, SnapshotTime and consistency time the store
  issues.
- **Platform now:** `max(line(real now), floor)`.
- **Business time:** a time that is stored, returned to a client, sent to a
  compute node, or compared with stored data. Examples: revision times, audit
  event times, consistency times, scheduled-transition due times, model update
  dates, search job times.
- **Operational time:** a time used only to measure elapsed real time.
  Examples: timeouts, deadlines, leases, heartbeats, retention of internal
  records, token and key validity, metrics.
- **Pending task:** a scheduled task in WAITING or RUNNING.
- **Idle:** no scheduled task is RUNNING.
- **Quiet:** idle, and no WAITING task has `NextAttemptTime ≤ platform now`.
- **Jump:** a change that moves platform time forward (a move step or a
  move's finish at a later time).
- **Re-anchor:** a change that keeps platform time where it is and changes
  only the rate (including the freeze that starts a move).
- **Move:** an operator request that sets the clock to a later time.

## Goals

1. **One platform clock for the whole cluster,** held by the store and
   computed by the store.
2. **A configured start.** A deployment can start the clock at any time from
   1970-01-01 to 9999-12-31, and at any rate of zero or more. Both are set in
   configuration and applied once to a store whose clock was never set.
3. **Forward only.** After that, the clock only goes forward. An operator can
   move it forward and change its rate.
4. **Moves fire what comes due.** A move fires every scheduled transition that
   comes due on the way, each at its own due time, in due order across all
   tenants. This includes transitions armed by those fires.
5. **Business time reads the platform clock** everywhere in cyoda-go and in
   the storage plugins. Operational times stay on real time.
6. **One line per transaction.** A transaction reads and stamps with one clock
   line from beginning to end.
7. **Readable.** Applications and compute nodes can read the platform time.
8. **Restricted.** Only a platform operator can change the clock, and only on
   a deployment that turned the control on.

## Non-goals

- Application-supplied valid time on writes (#293). That is about when a fact
  was true. This issue is about the time the platform itself reads.
- Setting the clock back through the API. The only way to start in the past
  is the start setting, applied to a store whose clock was never set. This
  matches Stripe's test clocks, which are set when created and then only move
  forward.
- Automatic time skipping when the system is idle (Temporal's model). Moves
  are explicit.
- A move that runs in the background with a status resource (Stripe's
  `advancing` → `ready`). A move request returns when the move is complete,
  and can be repeated to continue it.
- Per-tenant clocks.

## Anti-goals

The design has failed, even if it works, if any of these hold:

- **Mixed lines:** a commit carries a time from a clock line other than the
  one its transaction read from.
- **Stale nodes:** a node writes a revision or audit time from an old line
  after a change has returned success.
- **Jumped tasks:** a scheduled transition in the skipped time is recorded at
  the move's target time, or as expired, because the clock jumped over it.
- **Failed fires:** a scheduled fire is refused or failed because the clock
  changed while it ran.
- **Operational damage:** a clock change makes a key or token valid or
  invalid, makes a lease or heartbeat look dead, or deletes internal records
  early.
- **A changed default:** a deployment that never changes the clock behaves
  differently from today.
- **Divergence:** two backends give different answers to the same sequence of
  clock requests.

## The clock line and its rules

1. **The store holds the line.** `S`, `R`, `k`, the version and the state of an
   unfinished move (see "Moving the clock") are stored durably with the floor.
   - **postgres:** a table plus non-transactional sequences; see "Plugin
     changes".
   - **sqlite:** a table, cached in the transaction manager under the mutex
     that guards the floor.
   - **memory:** fields under that mutex. Not durable, like everything else in
     the memory store.
   - **A store whose line was never set** has the identity line (platform time
     equals real time), version 0, and no stored row.
2. **Arithmetic is exact.**
   - Times are integer microseconds since 1970.
   - The rate is a decimal with at most six fractional digits, held as an
     integer in parts per million.
   - Platform time is `S + floor((r − R) × k_ppm / 1 000 000)`, computed without
     overflow: numeric in SQL, 128-bit in Go.
   - If the physical clock reads earlier than `R`, the elapsed time counts as 0.
3. **Every stamp follows the line.** A store computes `line(real now)` wherever
   it reads its physical clock for a business time today:
   - submit time: `max(line(now), floor + 1 µs)`;
   - SnapshotTime: `max(line(now), floor)`;
   - consistency time: `max(line(now), floor)`.

   The floor rules do not change. With `k = 0`, successive commits still get
   distinct, increasing times one microsecond apart, so a frozen clock creeps
   forward one microsecond per commit.
4. **One line per transaction.**
   - A transaction takes the line in force at its Begin. Every platform-time
     read inside it uses that line, and so does its commit stamp.
   - If the line's version changed between Begin and Commit, the commit is
     refused with `spi.ErrConflict`. That is the existing retryable conflict,
     answered as 409 like any other transaction conflict
     (`internal/common/errors.go:218-219`).
   - The refusal covers every case where a transaction read the clock before a
     change and would commit after it: an arm whose due time came from the old
     line, a criterion that judged a date against it, and a stamp that would
     mix two lines.
   - A write outside a transaction stamps with the current line and is never
     refused by this rule. Such a write never arms a task: every arm runs in
     the engine's transaction (`reconcileScheduledTasks` is called with it at
     `internal/domain/workflow/engine.go:354,451,534,561` and
     `fire_scheduled.go:321`).
5. **Changes happen only when the scheduler allows.** The store applies a
   change (other than the initial setting) as one atomic operation:
   - a jump only when quiet, so no due task is jumped;
   - a re-anchor only when idle, because it moves no task's due time
     relative to the clock.

   Otherwise it changes nothing and reports "busy".
   - Because of this, no scheduled fire is ever in flight across a change, so
     rule 4 never refuses a fire. Only client transactions can straddle a
     change, and they get the retryable conflict.
   - **Why this holds.**
     - A fire's transactions begin only after the claim that made its task
       RUNNING has committed (`fire_scheduled.go:146` runs after the claim).
       So a fire whose transaction began before the change has a RUNNING task
       the check can see.
     - A claim cannot be in flight across the check: every claim holds a
       shared claim lock for its whole transaction, and a change holds that
       lock exclusively from its check until the new line is in force. So
       every claim commits either before the check (its task is RUNNING and
       visible) or after the change (its fire begins on the new line). On
       memory and sqlite, the lock the change already takes (`entityMu`, the
       commit gate) is the one `ClaimDue` holds; postgres gets a dedicated
       advisory key (see "Plugin changes").
     - Lost-owner re-claims take RUNNING rows whatever their
       `NextAttemptTime` (`plugins/postgres/scheduled_task_store.go:461-466`).
       That is why quiet tests "no RUNNING" directly.
   - **Claim pause (for a clock that runs).** A busy deployment on a running
     clock may never be idle by itself. A re-anchor on a running clock (a rate
     change, or a move's freeze) therefore first sets a claim pause in the
     store, which `ClaimDue` honours (it claims nothing while the pause is
     set). The re-anchor then waits until idle, and clears the pause in the
     same operation that applies it.
     - The pause is a lease in real time (30 s, renewed by the waiting
       request), so a crashed requester cannot stop the scheduler for long.
       The request passes a duration; the store computes the end on its own
       real clock.
     - The pause never applies to lost-owner claims (claims of RUNNING rows
       whose owner is dead). Those are the only way such a task leaves
       RUNNING, so pausing them would block idle forever.
     - WAITING tasks already due at that moment are not jumped: on a running
       clock they are late by the scan latency anyway, and they fire next, at
       the new line's time.
     - The pause matters only for liveness. Correctness rests on the atomic
       check.
     - On a frozen clock (every step after the first), no pause is needed: once
       the due tasks have fired, nothing else comes due.
6. **Platform now.**
   - A read outside a transaction asks the store and returns
     `max(line(real now), floor)` on the current line.
   - A read inside a transaction is computed in the process from the
     transaction's line, so it needs no store call and cannot fail. It is not
     raised to the floor.
     - The real-time input is the store's real time at Begin, plus the
       process's monotonic time elapsed since Begin.
     - postgres: Begin's first statement returns `clock_timestamp()` with the
       line.
     - memory and sqlite: the plugin's physical clock.
     - So the host's own clock never enters the line, and host-to-database
       skew is never multiplied by the rate.
     - The process starts its monotonic timer when the Begin result
       arrives, so network latency only makes in-transaction readings
       earlier. An in-transaction reading is then not later than the commit
       stamp, up to the difference in rate between the two physical clocks
       (multiplied by `k`), or a step back of the store's clock. Nothing
       orders events by comparing an in-transaction reading with a stamp.
7. **Initial setting (deployment start).**
   - `CYODA_CLOCK_START` and `CYODA_CLOCK_RATE` are applied once, when the
     store's version is 0.
   - The start must not be earlier than the floor. On a store that holds
     nothing, the floor is zero, so any start from 1970 on is accepted.
   - The store must have no pending scheduled task; otherwise the initial
     setting is refused (startup error). A start later than pending due times
     would jump them, and a start applied while other nodes fire would refuse
     those fires.
   - The line becomes `S` = the start (or platform now if only a rate is
     given), `R` = real now, `k` = the rate, version 1.
8. **Forward only.**
   - Every change after the initial setting sets `S` to a time not earlier
     than platform now, and `R` to real now.
   - A rate change alone re-anchors at platform now, so platform time never
     jumps.
   - A change is refused if the version is not the one the caller expects.
9. **The line belongs to the data.**
   - Changes are durable and stay in force whether or not the control is on.
   - A store whose clock was changed never returns to real time by itself,
     because that could put new times behind recorded ones.
   - If platform time is behind real time, a move to the current real time
     with rate 1 puts it back on real time, up to the length of that request.
   - If platform time is ahead of real time, it never returns.
   - Production uses a store whose clock was never changed.
10. **Range.**
    - Input times are limited to 1970-01-01T00:00:00Z to 9999-12-31T23:59:59Z
      (400 at the API; a startup error for the settings).
    - A stamp that would pass 9999-12-31 at run time fails the operation
      (5xx). It is not clamped.
    - The rate has no upper bound. A high rate reaches that limit quickly (at
      1 000 000 in about three real days), and the store is then unusable. The
      help topic says so.

## Moving the clock

A move to a target `T` later than platform now steps the clock from one due
time to the next. The node that received the request drives the loop. Every
node's scheduler fires the tasks, as it does today.

**The step operation.** Each step is one atomic store operation,
`step(T, expectVersion)`:

1. **Settle in-flight commits.** Exclude new stamps, and wait for commits that
   already took a stamp to finish (see "Plugin changes" for each backend).
2. **Check for quiet.** If any task is RUNNING, or any WAITING task has
   `NextAttemptTime ≤ platform now`, answer **busy** and change nothing.
   Otherwise read `e`, the earliest `NextAttemptTime` of any WAITING task
   across all tenants.
3. **Step to the next due time.** If `e ≤ T`, set `S = e` and `k = 0`, and
   record the unfinished move. Answer **stepped**.
4. **Finish.** Otherwise (no pending task, or `e > T`):
   - set `S = max(T, platform now)`. A step never refuses a target that has
     fallen behind platform now while the move waited. `CLOCK_BEFORE_NOW` is
     checked once, against the request, before the move starts;
   - set `k` to the final rate;
   - clear the unfinished move.

   Answer **finished**.

**The unfinished move** (stored with the line) holds:
- the target `T`;
- the final rate: the request's rate if it gave one, otherwise the rate in
  force before the first step.

**The loop.**
- **Freeze first.** On a running clock, the move first freezes it: a
  re-anchor to rate 0 at platform now, under the claim pause, applied when
  idle (rule 5). It records the unfinished move and clears the pause.
  - Tasks already due at that moment fire next, at the frozen time.
  - Then the steps run on the frozen clock, where quiet comes by itself once
    the due tasks have fired.
- On **busy**, the move wakes this node's scheduler, waits briefly, and steps
  again.
- On **stepped**, it wakes the scheduler and steps again.
- On **finished**, it returns.
- Each step expects the version that the move's previous step produced.
  Another change in between makes the move stop with 409 `CLOCK_CHANGED`.

How this meets the goals:

- **Due order across all tenants.** Each step goes to the earliest pending due
  time, and only when nothing earlier is due or running. Tasks due at the same
  instant fire together, as they would in real time.
- **Each fire at its own due time.** From the first step on, the clock is
  frozen at the due time while the scheduler claims and fires. A commit is
  stamped at that time plus one microsecond per earlier commit.
- **Time limits behave as in real time.** No platform time passes between a
  task coming due and its claim. The scan interval and callout latency cost
  real time only.
- **No jumped tasks.** A step reads `e` after settling in-flight commits and
  while excluding new stamps:
  - every arm already committed is in `e`;
  - a transaction that armed before the step and commits after it is refused
    by rule 4, and the client retries it on the new line.

  A fire's own arms and retries commit in the fire's transaction or its
  bookkeeping, so they are in the next step's `e`.
- **No failed fires.** Steps happen only when quiet (rule 5).
- **Never backward.** Every write is forward-only (rule 8).

**A latent scheduler defect this exposes.** Retry bookkeeping sets the next
attempt to `min(now + delay, deadline)` and fails a task only when
`now > deadline` (`internal/scheduler/bookkeeping.go:54-60`).
- On a frozen clock at `now = deadline`, a failing task is re-armed at `now`,
  re-claimed at once, and re-fired forever, so the move stays busy.
- This change fixes it test-first: the task fails with
  `EXPIRED_AFTER_FAILED_ATTEMPTS` when `now ≥ deadline`, and the clamp
  applies only when `deadline > now`.
  - The existing test `internal/scheduler/bookkeeping_test.go:99` ("at the
    deadline the attempt is still recorded") asserts the old behaviour and
    is inverted.
  - `cmd/cyoda/help/content/workflows.md:412` changes from "after the
    deadline" to "at or after the deadline".
  - On a real-time clock the difference is a single millisecond. A first
    attempt at `now == deadline` still runs, because `preRunDecision`
    expires only on `now > deadline` (`fire_scheduled.go:66`).

**Interruption.**
- Each step commits on its own. If the request is cancelled or the node stops,
  the clock stays frozen at the last step, the unfinished move stays stored,
  and the claim pause expires on its own.
- `GET /platform/clock` shows the move (`move.target`, `move.rate`).
- **While a move is unfinished, every POST continues it:**
  - the target is the request's target if given, otherwise the stored one;
  - the final rate is the request's rate if given, otherwise the stored one;
  - an `advanceByMs` counts from platform now, which is the frozen
    intermediate time.

  So `{}` is not valid (the body needs a member), but `{"rate": 1}` continues
  the stored move and finishes it at rate 1.

**Cost.** One step per distinct due time in the window, including retries and
schedules that re-arm themselves.
- Each step costs up to one scheduler scan interval on other nodes (default
  1 s), plus the time to fire.
- A task that keeps failing with no time limit retries at most every
  `CYODA_SCHEDULER_RETRY_DELAY_MAX` (default 15 min) of platform time, so a
  one-year move with one such task takes about 35,000 steps.
- These are the fires that would happen in that year. The help topic says so.
- The move logs progress at INFO, at most once every 10 s.

**Waiting has no limit.** A move waits while a due task is pending: when no
scheduler runs anywhere (`CYODA_SCHEDULER_ENABLED=false` on every node), or
when a RUNNING task is held by a node that is alive but stuck. The caller
decides how long to wait and can cancel. Failing closed here means not moving
past an unfired task.

**What a move does not wait for.** A move does not wait for work an
application starts outside a fire's transaction, for example a compute node
that later calls cyoda-go on its own. Such a transaction is an ordinary
client transaction under rule 4.

**A store without scheduled tasks** has no pending tasks; its steps finish at
once.

**Other changes.** Unless a move is unfinished (then every POST continues
it), a rate-only change and a target equal to platform now are re-anchors:
- they are applied when idle, using the claim pause on a running clock;
- they wait and retry while busy;
- they expect the version read at the start of the request.

## API

### HTTP

`GET /platform/clock`
- Any authenticated caller, any tenant.
- 200 `PlatformClockDto`:
  - `now` (date-time): platform now;
  - `rate` (number);
  - `move` (object, present only while a move is unfinished): `target`
    (date-time) and `rate` (number);
  - `controllable` (boolean): whether this node accepts changes.

`POST /platform/clock`
- Platform operator only (`auth.OperatorGuard`).
- Body `PlatformClockChangeDto`, read strictly like every typed body (#678):
  - `time` (date-time, optional): the target;
  - `advanceByMs` (integer > 0, optional): the target is platform now +
    `advanceByMs`;
  - `rate` (number ≥ 0, at most six fractional digits, optional).
- At most one of `time` and `advanceByMs`, and at least one member.
- What happens:
  - A target later than platform now runs a move.
  - A target equal to platform now, or a rate alone, is a re-anchor, applied
    when idle.
  - While a move is unfinished, every POST continues it.
- 200 `PlatformClockDto` when the change is complete.

Errors, in the order they are checked:

| Status | Code | When | GET | POST |
|---|---|---|---|---|
| 400 | existing query-parameter refusal | undeclared query parameter | ✓ | ✓ |
| 401 | — | missing or invalid credentials | ✓ | ✓ |
| 403 | existing operator refusal | caller is not a platform operator, or acts on behalf of a user | — | ✓ |
| 403 | `CLOCK_CONTROL_DISABLED` | `CYODA_CLOCK_CONTROL_ENABLED` is false on this node | — | ✓ |
| 400 | existing strict-body / validation codes | see the list below | — | ✓ |
| 409 | `CLOCK_BEFORE_NOW` | `time` earlier than platform now; the detail gives platform now | — | ✓ |
| 409 | `CLOCK_CHANGED` | another request changed the clock during this one | — | ✓ |
| 503 | existing store-unavailable | store unreachable | ✓ | ✓ |
| 500 | generic + ticket | anything else | ✓ | ✓ |

The 400 validation refusals are:
- a malformed body, an unknown member, or no member;
- both `time` and `advanceByMs`;
- `advanceByMs ≤ 0`;
- a `rate` that is negative, not finite, has more than six fractional digits
  (judged on the decimal text, not on a float), or whose parts-per-million
  value overflows 64 bits;
- a `time` that is not RFC 3339, before 1970, or after 9999-12-31;
- an `advanceByMs` that overflows or reaches past 9999-12-31.

Response components (`QueryParameterRefused`, `Unauthorized`, `Forbidden`,
`ServiceUnavailable`, `InternalServerError`) are reused as
`GET /entity/consistency-time` uses them (`api/openapi.yaml:982-1008`).

Rule 4 lets the transaction-conflict 409 occur on any operation that commits
a transaction. The build checks each such HTTP and gRPC operation and adds the
conflict response where it is not yet documented.

### gRPC

- Read only. CloudEvent types `PlatformClockGetRequest` and
  `PlatformClockResponse` (`now`, `rate`, `move`, `controllable`), next to
  `EntityConsistencyTimeGetRequest` (`internal/grpc/cloudevent_types.go:84-85`).
  Each has a published JSON schema (#626).
- Changing the clock stays HTTP-only, like the other operator controls.
- Error classes: unauthenticated, store unavailable, internal.

### Platform time on callout requests

- **What carries it:** every callout request to a compute node (processor,
  criterion and schedule-function requests) carries the standard CloudEvents
  `time` attribute.
- **Its value:** platform time read from the calling transaction's line (rule
  6). The read needs no store call, so it cannot race with a joined callback
  that uses the same connection during dispatch.
- **What does not carry it:** other CloudEvents keep their current form.
  Member greet and keep-alive events, gRPC API responses and error responses
  must not depend on the store being reachable.
- **Cloud:** it sets this attribute from its own JVM clock on its events
  (`~/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas/grpc/ProtobufMessageConverter.kt:153`).
  cyoda-go sets none today (`internal/grpc/cloudevent.go:23-29`).
- **Why it matters:** this is how a schedule function learns the platform's
  "now". A relative `fireAfterMs` is already resolved against the arm time
  (`internal/domain/workflow/schedule_function.go:65`).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CYODA_CLOCK_CONTROL_ENABLED` | `false` | Allows `POST /platform/clock` on this node, and the two settings below. |
| `CYODA_CLOCK_START` | unset | RFC 3339, 1970-01-01 to 9999-12-31. Initial platform time for a store whose clock was never set. |
| `CYODA_CLOCK_RATE` | unset | Number ≥ 0, at most six fractional digits. Initial rate for such a store; with no start, it applies from platform now. |

At startup:
- **Settings without the control:** start or rate set while the control is
  off is a configuration error, and startup fails.
- **Control on:** startup logs a WARN: "platform clock control is enabled:
  test feature, not for production; the clock is instance-wide and affects
  every tenant".
- **Applying the initial setting.** If start or rate is set, the node applies
  the initial setting right after it obtains the transaction manager
  (`app/app.go`, after `factory.TransactionManager`, today near line 238). That
  is before the auth loop, the search reapers, the scheduler and the listeners
  start.
  - The node reads the clock state first. If the version is not 0, it skips
    with INFO "platform clock already set; CYODA_CLOCK_START/RATE not
    applied". This is the normal case on a restart, and on every node but the
    first in a cluster.
  - Otherwise it applies the initial setting, expecting version 0.
    - If another node won the race, the version check fails, and the node
      skips as above.
    - If the start is earlier than the floor (the store already holds later
      data), startup fails with an error that names the floor.
    - If the store holds a pending scheduled task, startup fails.
- **Moved clock:** if the stored line is not the identity line, an INFO line
  logs platform now and the rate, whatever the settings.

Rules for the build:
- **Nothing before the initial setting** may begin a transaction or issue a
  time on the store. The fresh-store tests below check this on every backend.
- **cassandra:** its `Start` (`app/app.go:224-234`) runs before
  `TransactionManager` and starts recovery, heartbeat and reaper loops. It
  must load the line before any of them issues a business time. The
  cassandra issue states the order its plugin needs.

Documentation (Gate 4):
- a new help topic `cmd/cyoda/help/content/config/clock.md`, `README.md`, and
  `DefaultConfig()`;
- the control is independent of `CYODA_IAM_MODE` and `CYODA_REQUIRE_JWT`,
  because a staging deployment with real authentication may need a test clock;
- mock IAM lets any tenant's admin act as platform operator
  (`internal/auth/operator_guard.go:30`), as on every operator route. The help
  topic says so.

All nodes of a cluster should share the clock settings, as they share other
configuration. A node with the control off refuses changes. Reads are the same
on every node.

## Storage interface (SPI) changes

On `spi.TransactionManager`, which owns the floor and the stamps:

```go
// ClockRate is a rate in parts per million (1 000 000 = real speed).
type ClockRate int64

// ClockMove is an unfinished move.
type ClockMove struct {
    Target    time.Time
    FinalRate ClockRate
}

// ClockState is the platform clock as the store holds it. Outside a
// transaction Now is platform now on the current line; inside one it is on
// the transaction's line.
type ClockState struct {
    Now     time.Time
    Rate    ClockRate
    Move    *ClockMove
    Version int64 // 0 for a store whose clock was never set
}

// ClockChange is one atomic change. Exactly one kind:
//   Initial: version must be 0; Start (or platform now) must not be below
//            the floor; no pending task may exist; Rate optional.
//   Step:     a jump: a move step towards Target with final rate Rate (nil:
//             the stored move's final rate, else the current rate). Quiet.
//   Reanchor: keep platform now, set Rate (required); when Move is set,
//             record it as the unfinished move (a move's freeze). Idle.
type ClockChange struct {
    Kind          ClockChangeKind
    Start         *time.Time // Initial
    Target        time.Time  // Step
    Rate          *ClockRate
    Move          *ClockMove // Reanchor: the move this freeze starts
    ExpectVersion int64
}

type ClockOutcome int // Busy, Stepped, Finished, Applied

PlatformClock(ctx context.Context) (ClockState, error)
ChangePlatformClock(ctx context.Context, c ClockChange) (ClockState, ClockOutcome, error)
// PauseClaims sets or renews the claim pause for d of the store's real
// time; ClaimDue claims nothing but lost-owner RUNNING rows while it holds.
// A change clears it.
PauseClaims(ctx context.Context, d time.Duration) error

var ErrClockBehindFloor = errors.New("clock start earlier than recorded time")
var ErrClockBeforeNow  = errors.New("clock target earlier than platform now")
var ErrClockChanged    = errors.New("platform clock changed concurrently")
```

- `ErrClockBehindFloor` and `ErrClockBeforeNow` are wrapped with the time they
  violate, so the caller can name it.
- For `Initial`, the version check comes first. On a store already set,
  `ErrClockChanged` is the answer, never `ErrClockBehindFloor`. This matters
  on every restart of a store that started in the past.
- Rule 4 becomes part of the `Commit` contract: `ErrConflict` when the line's
  version changed since Begin.
- The store needs cross-tenant knowledge of pending tasks for the quiet check.
  That is plugin-internal: the transaction manager and the scheduled-task
  store live in the same plugin. A plugin without scheduled tasks treats every
  check as quiet.

Breaking change for plugins: an out-of-tree plugin does not compile against
the new SPI until it implements these methods.

The SPI follows `MAINTAINING.md`:
- a PR into SPI `main`;
- a pseudo-pin in all four `go.mod` files, with no tag in the middle of the
  milestone;
- `COMPATIBILITY.md` updated with the pin.

The tracing wrapper (`internal/observability/tx_tracing.go`) forwards the new
methods.

### spitest

**Harness additions:**
- **`FreshFactory` (optional):** returns a factory on a new, empty store,
  with its own `Now` and `AdvanceClock`.
  - Clock cases need it, because the suite otherwise shares one factory
    (`spitest/spitest.go:19-23`), and a change on a shared store affects every
    later case.
  - A harness without it skips the clock cases, and says so.
- **`PeerFactory` (optional):** a second factory on the same store. Only
  stores shared between processes (postgres, cassandra) can offer it; sqlite
  takes a file lock and memory is per process.
- **`Harness.Now`** reports platform now. The postgres harness reads
  `CURRENT_TIMESTAMP` today (`plugins/postgres/conformance_test.go:165-174`).

**Conformance cases, each on a fresh store:**
- **The line is followed.**
  - Stamps, SnapshotTime, consistency time and `PlatformClock().Now` follow
    it: a start years in the past, and rates 0, 1 and 2.5, with real time
    driven by `AdvanceClock`.
  - The arithmetic is exact. Each plugin tests its line computation with an
    explicit real time `r` (postgres: the SQL line function called with `r`)
    against shared Go cases. The results must be the same on every backend.
- **Initial setting:**
  - a start in the past on a fresh store is accepted;
  - a second Initial gets `ErrClockChanged`;
  - Initial on a set store whose floor is above the start gets
    `ErrClockChanged`, not `ErrClockBehindFloor`;
  - a start below the floor on a version-0 store with data gets
    `ErrClockBehindFloor`.
- **Changes:**
  - a stale version gets `ErrClockChanged`;
  - a step whose target has fallen behind platform now finishes at platform
    now;
  - the claim pause stops `ClaimDue` until it expires or a change clears it;
  - a rate-only change does not jump the time, including on a frozen clock
    whose floor has crept ahead of the line.
- **Quiet rule and steps:**
  - a RUNNING task, or a due WAITING task, in either of two tenants gives
    Busy;
  - a step lands on the earliest WAITING due time;
  - a FAILED task is ignored;
  - the initial setting is refused while a task is pending.
- **Postgres only:** a commit that took its stamp before a step and becomes
  visible after it is counted in that step's `e`. The SPI has no way to pause
  inside a commit, so this is a postgres test that holds a commit marker on a
  raw connection. memory and sqlite make a commit visible under the same
  lock, so for them the case cannot arise.
- **Rule 4:**
  - a transaction that began before a change and commits after it gets
    `ErrConflict`;
  - in-transaction `PlatformClock().Now` stays on the Begin line after the
    change;
  - a transaction that began after the change commits on the new line;
  - a write outside a transaction after a change is never refused.
- **With `PeerFactory`:** a change through one factory is used by the next
  commit and the next read through the other.
- **Durability:** the line and the unfinished move survive reopening the store
  (sqlite, postgres).
- **Operational behaviour does not follow the line.** After a one-year move:
  - a running search job is not reclaimed as stale;
  - a scheduler owner is not swept;
  - a submit time recorded before the move stays readable through
    `GetSubmitTime`.

  On a store started years in the past, finished search jobs are not reaped
  early.

## Plugin changes

### All plugins

Business times use the line; operational times use real time. Every place that
compares a business time with a physical clock changes so that both sides are
in the same domain:

- **`GetSubmitTime` retention.**
  - Sites: memory `plugins/memory/txmanager.go:1193`; sqlite
    `plugins/sqlite/txmanager.go:986`, `:998-1001`; postgres
    `plugins/postgres/transaction_manager.go:354`, `:543-560`.
  - Change: keep entries by real age. Record the real insertion time beside
    each entry. In postgres this is a new `submit_times` column, and the
    pruning index `idx_submit_times_pruning` moves to it.
- **Search job liveness and retention.**
  - Sites: postgres `plugins/postgres/search_store.go:587`, `:630`; memory and
    sqlite `search_store.go`.
  - Change: staleness and reaping use real times recorded by the store. A
    job's `CreateTime` and `FinishTime` stay the platform times the host
    supplies, for display.
- **Scheduler owner liveness** stays on real time. In memory and sqlite it now
  reads the physical clock, not the line.

### memory and sqlite

- They keep their `Clock` as the physical source. Tests advance it as today,
  and the line is applied on top.
- A transaction records the whole line (`S`, `R`, `k`, version) at Begin,
  under the floor mutex. Today sqlite reads the clock for SnapshotTime before
  taking the mutex (`plugins/sqlite/txmanager.go:755`, mutex at `:796-800`);
  the line and the physical reading move inside it.
- Commit compares the version under the mutex.
- A change takes the locks in the same order as Commit, so that it excludes
  both new stamps and commits that have stamped but are not yet visible:
  - **memory:** Commit stamps under `m.mu` inside the `factory.entityMu`
    section, and rows become visible when `entityMu` is released
    (`plugins/memory/txmanager.go:854-867`, stamp at `:662-674`). Scheduled
    tasks are read under `entityMu` (`scheduled_task_claims.go`). So a change
    takes `entityMu` for writing, then `m.mu`.
  - **sqlite:** a change takes the commit gate, then the floor mutex, as
    Commit does.

  Under that exclusion it runs the quiet check, then writes. The claim pause
  is a field that `ClaimDue` reads under the same lock it already holds.
- sqlite persists a consistency-time high-water mark of C + 1 s
  (`txmanager.go:1363`, `:1395-1399`). After a restart, a frozen clock can be
  up to 1 s further on. This is documented.

### postgres

A transaction's snapshot is fixed by its first statement
(`transaction_manager.go:163`, `:171`), and `cyoda_stamp` runs inside the
transaction at commit (`:415`). So a table read at commit sees the line as it
was at Begin. The design uses this on purpose and adds a check outside MVCC.

- **Storage:**
  - a single-row table holds `S`, `R`, `k`, the unfinished move and the
    version;
  - a sequence `cyoda_clock_version` mirrors the version outside MVCC, as the
    sequence `cyoda_stamp_floor` does for the floor.
- **Begin:** the existing first statement (`set_config`, `:171`) also returns
  the line row (or nothing, for the identity line) and `clock_timestamp()`, at
  no extra round trip. The host keeps both with the transaction and computes
  in-transaction platform time from them (rule 6). The clock table is not
  under row-level security; it holds no tenant data, and this statement reads
  it whatever `app.current_tenant` is.
- **Stamp lock order:** `cyoda_stamp` today takes the commit's marker lock,
  then (0,0) (`000016:47`, then `:49`). It changes to: take (0,0), then the
  marker, stamp, release (0,0).
  - A marker then always means "already stamped", and no marker holder ever
    waits for (0,0). A change can hold (0,0) and wait on markers without
    deadlock.
  - `cyoda_consistency_time` stays correct. It scans markers only after it
    releases (0,0), and every commit stamped at or below its C took its marker
    while holding (0,0), so before C was read.
- **Stamp:** `cyoda_stamp` receives the transaction's line version. Under
  lock (0,0) it compares that version with the sequence:
  - **Equal:** stamp with that line, from the snapshot row.
  - **Transaction lower:** the line changed after Begin. Raise SQLSTATE
    40001. The plugin already classifies that as `spi.ErrConflict`
    (`consistency_time.go:141-148`, `transaction_manager.go:1012-1013`).
  - **Transaction higher:** a change committed its row, and its process
    stopped before `setval`. The lock proves no change is in progress, so set
    the sequence to the row's version and stamp.
  
  Any stamp failure already closes the pooled connection
  (`consistency_time.go:151-160`). Rule 4 refusals therefore cost one
  connection each; this is acceptable for a test feature.
- **Writes outside a transaction and consistency time:**
  - these run in READ COMMITTED, already under (0,0);
  - a statement there sees the latest committed row;
  - they apply the same repair (row ahead of sequence → set sequence) and use
    the current line.
- **`PlatformClock` reads take no lock.** The scheduler reads on every scan
  on every node, and `GET /platform/clock`, model and search-job times read
  too. A plain READ COMMITTED read of the row, the version sequence and the
  floor sequence answers.
  - Only when the row is ahead of the sequence (the repair case) does the read
    take (0,0). Under it, the repair re-reads the row and the sequence and
    raises the sequence with `setval(greatest(...))`; it never lowers it.
    A read that meets a change between its row commit and its `setval`
    therefore waits for the change and repairs nothing.
  - A read that sees the old row with the new sequence returns the old line
    for an instant that overlapped the change; that is correct for it, and a
    claim using it only claims less.
  - A read made during a change sees either the old line or the new one, and
    both are correct for that instant.
- **Change:**
  All steps run on one dedicated connection.
  1. Take the claim key exclusively (a dedicated advisory key; every claim
     transaction takes it shared at its start, as a transaction lock), then
     lock (0,0), both as session locks.
  2. Wait for every tenant's in-flight commit marker. After the stamp
     reorder, these are exactly the commits that have stamped but are not yet
     visible. It is the marker scan that `cyoda_consistency_time` performs for
     one tenant (`000016:72-81`), across all tenants. The wait is bounded at
     500 ms: on timeout the change releases both locks and answers busy, so
     commits waiting for (0,0) (`lock_timeout` 2000 ms, `000016:43`) never
     time out into 503 because of a change.
  3. Run the quiet check: a cross-tenant query of scheduled tasks. The
     scheduler tables are not under row-level security, as `ClaimDue`'s
     cross-tenant reads show. A partial index on
     `(next_attempt_time) WHERE status IN ('WAITING','RUNNING')` makes both
     "any RUNNING" and "earliest WAITING" one index probe.
  4. In a READ COMMITTED transaction, check the expected version and the
     forward rule, update the row, and commit.
  5. `setval` the version sequence.
  6. Release both locks.
- **Failure rules for a change:**
  - any error while the session lock is held closes the connection, so the
    lock is released;
  - `setval` is never run when the commit's outcome is in doubt, so the
    sequence is never ahead of a committed row;
  - a commit that did happen is reconciled by the repair rule.

  A leaked lock (0,0) would stall every commit. A sequence ahead of the rows
  would refuse every commit.
- **Provisional dates:** `entity_store.go:242`, `:774` read
  `CURRENT_TIMESTAMP` for provisional `CreationDate`, `LastModifiedDate` and
  `valid_time`. Reads inside the same transaction see these values (`:614`)
  before the commit stamp replaces them.
  - They take platform time from the transaction's line instead, passed as a
    parameter.
  - `wall_clock_time` (`clock_timestamp()`, `:239`, `:373`) stays physical.
    It is written and never read.
- **Claim pause:** a sequence holding the pause's end, in real-time
  microseconds of the database clock. The claim query's WAITING branch adds
  "and the pause has ended by `clock_timestamp()`"; its lost-owner branch does
  not. The pause is operational time.
- **Claim key:** `claimDue` takes the dedicated advisory key shared at the
  start of its transaction. It is never a table lock: a client transaction
  holding a row lock on `scheduled_tasks` while waiting for (0,0) would then
  deadlock with a change.
- **Migration:** a new migration adds the table, the sequences and the partial
  index, and changes `cyoda_stamp`, `cyoda_consistency_time` and the claim
  query. With no row, both use the
  identity line, so a store that never uses the clock stamps exactly as today.
- **No cache:** postgres needs no host-side cache. A change is in force for
  every node as soon as the sequence moves, and a transaction that straddles it
  is refused.
- **Tests:** a postgres-specific test covers the repair path (row committed,
  sequence not moved).

### cassandra (separate repository)

It needs an issue that lands before its v0.9.0 bump, covering:
- the HLC's physical part follows the line;
- transaction ids and audit `event_time` timeuuids come from platform time,
  because recovery turns the floor into a timeuuid bound
  (`internal/tx/tx_recovery.go:330-337`);
- `period_bucket` follows platform time;
- rule 4;
- the atomic change with the quiet check (quiet at once while scheduled tasks
  are not implemented);
- loading the line in `Start`;
- the SPI methods;
- a `FreshFactory` for its conformance harness, and the fresh-stack parity
  fixture interface. Without them its clock scenarios are skipped.

## Host (root module) changes

A small package `internal/platformclock` wraps the SPI for the host:
- `Now(ctx)`: computed in the process from the transaction's line when the
  context carries a transaction; a store read otherwise;
- `State(ctx)`;
- `Init(ctx, settings)`, used at startup;
- `Change(ctx, request)`: runs the move loop or a rate change, and wakes the
  local scheduler.

One instance per process, built in `app/app.go` from the transaction manager.
The scheduler gains a wake signal; it has none today.

Business-time sites move to `platformclock.Now(ctx)`:

| Site | Today | After |
|---|---|---|
| scheduler claim (`internal/scheduler/service.go:607`) | `scheduler.Clock` | store read on every scan, made before `claimRequest` takes `s.mu` (no store I/O under that mutex); on failure the scan is skipped. The claimed tasks carry this `NowMs` to `preRunDecision` |
| scheduler bookkeeping (`service.go:690`; `:939` derives from it) | `scheduler.Clock` | store read; on failure, the existing bookkeeping-failure path |
| engine arm, audit timestamps (`internal/domain/workflow/engine.go:173,236,1110`, `arm.go:115`, `fire_scheduled.go:186`) | `WithScheduledClock(func() time.Time)` | the transaction's line; no store call, cannot fail |
| fire decision `preRunDecision` (`fire_scheduled.go:91`, before the fire's Begin at `:146`) | same | the claim's `NowMs`, passed with the task. A RUNNING task blocks every change (rule 5), so the line cannot have changed since the claim; no store read, cannot fail |
| entity pre-commit `CreationDate`/`LastModifiedDate` (`internal/domain/entity/service.go:282,2015,2271,2528`) | `time.Now()` | the transaction's line |
| postgres provisional dates (`plugins/postgres/entity_store.go:242,774`) | `CURRENT_TIMESTAMP` | the transaction's line, passed in |
| model `UpdateDate` (`internal/domain/model/service.go:195`) | `time.Now()` | store read; failure fails the operation |
| search job `CreateTime` and finish times (`internal/domain/search/service.go`, `reaper.go`) | `time.Now()` | store read, or the transaction's line inside one; failure fails the operation |
| callout CloudEvent `time` (`internal/grpc/cloudevent.go`, callout dispatch) | not set | the transaction's line |

The engine's own-transaction audit writes (`recordEventInOwnTx`,
`internal/domain/workflow/engine.go:1141-1170`) retry once on `ErrConflict`.
Rule 4 can refuse such a write if it straddles a change, and the audit trail
must not lose the event.

Model `UpdateDate` and search-job times are read outside a transaction and
saved shortly after. A change in that window leaves the old line's time on
them. These are display dates, not revision or audit times. The window is one
request long.

These stay on real time, by decision:
- **Credentials.** Everything under `internal/auth`, and the key and
  trusted-key adapters in `internal/domain/account`, including M2M client
  dates and key `validFrom` defaults. A clock change must never change which
  credential is valid.
- **Operational timing.** Callout deadlines, gossip, heartbeats, watchdogs,
  the model-cache lease, member `ConnectedAt`, and metrics.
- **v1 UUIDs** (`internal/common/uuid.go:21`). They are identifiers. The
  audit listing uses an event id's embedded time only to order events whose
  other keys are equal (`internal/domain/audit/events.go:144`), and real
  creation order is correct there.

## Security

- **Who can change it.** Changing the clock needs a platform operator
  (`OperatorGuard`, which refuses on-behalf-of tokens) on a node with the
  control on. The control is off by default, and its settings are refused
  without it.
- **Instance-wide by design.** The clock affects every tenant.
  - Any tenant reads it, and it holds no tenant data.
  - On a deployment with the control on, a tenant can see how the clock
    moves: a frozen clock creeps by one microsecond per commit of any tenant,
    and a move stops at each due time. That is part of what the test feature
    is.
  - The startup WARN and the help topic say so.
- **No credential, key or token validity reads platform time.**
- **Input validation.**
  - The body is read strictly. The rate must be finite and non-negative, with
    at most six fractional digits. Times are RFC 3339 within 1970–9999.
  - Problem details name the refused member.
  - `CLOCK_BEFORE_NOW` names platform now, which any caller can read anyway.
  - The startup floor error goes to the operator's log only.

## Documentation

- **New help topic `cyoda help clock`:**
  - the clock line;
  - starting in the past through the settings;
  - moves, interruption and cost;
  - the conflict rule for transactions that straddle a change;
  - returning to real time;
  - "the clock belongs to the data";
  - retries and time limits at high rates;
  - the rate limit at year 9999;
  - the instance-wide effect.
- **Existing topics:**
  - `cyoda help config clock` for the three settings;
  - `cloudevents.md`: the `time` attribute on callout requests is platform
    time;
  - `scheduled-tasks.md`: a move fires due tasks in order, at their due
    times;
  - `crud.md`: point-in-time "now" and the consistency time follow the clock.
- **OpenAPI:** both operations and their DTOs, and the conflict response on
  every committing operation. **gRPC:** schemas for the two new types.
- **`docs/cloud-parity/platform-clock.md`** (Gate 7): the endpoints, gRPC
  types, settings, the callout `time` attribute, and rule 4. Cloud has no
  controllable clock today. `ClockCtrl` is replaceable only by tests
  (`~/dev/cyoda-platform/core-libs/core/.../clocks/ClockCtrl.java:33`).
- **A CaaS ticket** in CP for Cloud to follow.
- **CHANGELOG:**
  - `### Added` for the feature;
  - `### Breaking` for plugin authors (the new SPI methods and the `Commit`
    rule).

## Coverage matrix

**Layers:**
- **U:** unit.
- **E:** HTTP e2e on postgres in `internal/e2e`. Each clock test uses its own
  database, through the scheduler harness pattern
  (`internal/e2e/scheduler_harness_test.go:23-28`).
- **P:** cross-backend parity in `e2e/parity`.
  - It runs through a new optional fixture interface that starts a stack on a
    fresh store with given settings. `BackendFixture` has none today
    (`e2e/parity/fixture.go:19-57`).
  - The in-tree fixtures implement it.
- **G:** gRPC in `internal/grpc`.
- **C:** spitest conformance on every plugin.

| Scenario | U | E | P | G | C |
|---|---|---|---|---|---|
| Line arithmetic, exact; re-anchor on a rate change, including a frozen clock with the floor ahead | ✓ | | | | ✓ |
| Fresh deployment with `CYODA_CLOCK_START` years in the past: the first revision, audit event, consistency time and `GET /platform/clock` carry that time | | ✓ | ✓ | | ✓ |
| Start setting on a store holding later data → startup error; start setting on an already-set store (floor above the start) → skipped | ✓ | ✓ | | | ✓ |
| Start or rate set with the control off → startup error | ✓ | | | | |
| Advance by one day (#611 "done when"): new revision time, audit time, a transition due in that day fired at its due time, point-in-time "now" answers at the new time, `GET /platform/clock` returns it | | ✓ | ✓ | | |
| Move order: transitions of three entities in two tenants, due at different times, fire in due order, each at its due time; a fire that arms another transition inside the window is reached in its place | ✓ | ✓ | | | |
| A transition with a time limit shorter than the move fires and is not expired, at rate 60 | | ✓ | | | |
| A client transaction that arms a transition and straddles a step → 409; retried, it arms on the new line, and the move reaches its due time | | ✓ | | | ✓ |
| Interrupted move: the clock stays frozen at a step; `move` shown on GET; a POST with `time` = `move.target` continues and applies the stored rate | ✓ | ✓ | | | ✓ |
| Frozen clock: commits get increasing distinct times; nothing comes due until a move | | ✓ | ✓ | | ✓ |
| A transaction that straddles a change → 409 conflict, over HTTP and over gRPC | | ✓ | ✓ | ✓ | ✓ |
| A write outside a transaction right after a change is not refused | | | | | ✓ |
| `CLOCK_BEFORE_NOW` with platform now in the detail | ✓ | ✓ | | | ✓ |
| `CLOCK_CHANGED` from two concurrent changes (isolated e2e). Built deterministically: both moves wait busy on a due task with the scheduler off, then the scheduler is started; one wins, the other answers 409 | ✓ | ✓ | | | ✓ |
| Move on a running clock under steady scheduled load: the claim pause lets it start | | ✓ | | | ✓ |
| Frozen clock at a task's deadline, fire fails: the task fails `EXPIRED_AFTER_FAILED_ATTEMPTS`, no busy loop | ✓ | ✓ | | | |
| Move waiting with no scheduler running; the client cancels; the clock stays at the last step, and the pause expires | | ✓ | | | |
| A rate-only POST during an unfinished move continues and finishes it at that rate | ✓ | ✓ | | | |
| Initial setting with a pending task → startup error | ✓ | ✓ | | | ✓ |
| Run-time stamp past 9999-12-31 → 5xx (store started near the limit, rate high) | ✓ | | | | ✓ |
| `preRunDecision` uses the claim's `NowMs` | ✓ | | | | |
| Claim pause does not block a lost-owner reclaim; a re-anchor waiting on a dead owner's RUNNING task completes after the reclaimed fire | | | | | ✓ |
| postgres: a claim racing a re-anchor's idle check commits before the check or fires on the new line (claim key) | ✓ | | | | |
| postgres: a change that cannot settle markers within 500 ms answers busy; concurrent commits are not refused with 503 | ✓ | | | | |
| Control off → 403 `CLOCK_CONTROL_DISABLED`; GET still works | | ✓ | | | |
| Non-operator → 403; on-behalf-of token → 403 | | ✓ | | | |
| Each 400 refusal in the validation list, and the undeclared query parameter on GET and POST | ✓ | ✓ | | | |
| 401 on both operations | | ✓ | | | |
| 503 store unavailable | ✓ | waived: no e2e stops the database mid-test; unit test at the handler | | | |
| 500 internal | ✓ | waived: no e2e can force an internal fault; unit test at the handler | | | |
| Line and unfinished move survive restart (sqlite, postgres) | | ✓ | | | ✓ |
| postgres repair path: row committed, sequence not moved | ✓ | | | | |
| Multi-node: a change through node A is used by the next commit and the next GET on node B | | ✓ | | | ✓ (PeerFactory) |
| Operational times unaffected after a one-year move and on a store started years back (search job not reclaimed or reaped, owner not swept, submit time readable, token valid) | ✓ | ✓ | | | ✓ |
| Callout `time` attribute carries platform time; a schedule function's absolute fire time computed from it is due at the right platform time | ✓ | ✓ | | | |
| gRPC `PlatformClockGetRequest`: success, unauthenticated, store unavailable, internal | | | | ✓ | |
| Every committing operation documents the conflict 409 (OpenAPI check) | ✓ | | | | |

## Rollout

1. **SPI PR:** types, methods, errors, the `Commit` rule, and the spitest
   hooks and cases. Then the pseudo-pin in all four `go.mod` files, and
   `COMPATIBILITY.md`.
2. **cyoda-go PR into `release/v0.9.0`:** plugins, host, API, configuration,
   docs and the parity doc.
3. **The cassandra issue** (before the cassandra v0.9.0 bump) **and the CaaS
   ticket.**

## Left to the builder

- Exact table, column and sequence names, the migration number, and the Go
  shape of `internal/platformclock`.
- How the move waits after "busy" (poll interval), and the scheduler's wake
  signal. The bound is at most one scan interval per step.
- How the cross-tenant quiet check reads pending tasks in each plugin.
- The wording of log lines and help topics.

## Calls made in this document (not discussed with Paul)

- **The quiet rule:**
  - a jump waits until no scheduled task is due or running; a re-anchor waits
    until none is running;
  - so fires never straddle a change;
  - on a running clock, a real-time claim pause (30 s lease) lets a busy
    system reach that moment.
  - During a move the clock is frozen from the first step on, and the final
    rate is applied at the end.
  - While a move is unfinished, every POST continues it.
- **Two fixes to existing code, made test-first:**
  - the scheduler's deadline busy loop;
  - the postgres stamp's lock order, now (0,0) before the marker.
- **Rule 4 (one line per transaction):** a client transaction that straddles
  a clock change is refused with the existing retryable 409. Reads inside a
  transaction are computed in the process from the line taken at Begin.
- **The move request** is synchronous. An interrupted move stays recorded,
  frozen, and any later POST continues it. Stripe uses a background job
  instead.
- **The API:**
  - `advanceByMs` is in milliseconds, like other durations in the API
    (`fireAfterMs`);
  - the rate has at most six fractional digits;
  - error codes: 403 `CLOCK_CONTROL_DISABLED`, 409 `CLOCK_BEFORE_NOW`, 409
    `CLOCK_CHANGED`;
  - the route is `/platform/clock` for reading and changing;
  - changes are HTTP-only; gRPC can only read.
- **The `time` attribute** is on callout requests only.
- **Limits:** times are limited to 1970-01-01 to 9999-12-31, and there is no
  upper bound on the rate.
- **Moves wait** for pending due tasks without a time limit.
- **Real time stays** for IAM dates (M2M client dates, key defaults) and for
  v1 UUIDs.
- **Settings:** the control is independent of the authentication mode, and
  the start or rate settings with the control off are a startup error.
