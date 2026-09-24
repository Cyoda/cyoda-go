# #598 — every scheduled run has one owner

Status: spec v6, 2026-09-24. Versions v1 to v3 went through three independent
reviews. Their findings clustered in the claim check inside the entity
transaction, so v4 re-derived that part (§10.0). A fourth review of v4 found
narrower mechanical defects, and v5 fixes them: segment stamps (§5.2), a
re-checked claim (§10.2), the watchdog margin (§6.3), lock timeouts (§10.2),
and the SQLite ordering and marks (§10.3). A targeted review of v5 then led to
v6. A single `HandedOff` fact is now the hand-off proof (§5.5). Cancellation
reaches every callout, and every commit checks it (§5.3). `PartialCommit`
replaces the state comparison (§5.2), and "superseded" is decided outside the
run's own transaction (§5.2). One product decision is open: D1 in §7.

Milestone v0.9.0. Branch `feat/598-scheduler-ownership`.

Inputs:
- Research: `docs/superpowers/research/2026-09-24-598-scheduler-ownership-research.md` ("R§n").
- Design brief and its review: `docs/superpowers/research/2026-09-24-598-design-brief.md`.
- Product-owner rulings are marked **[ruling]**.

Paths: `SPI/` is `cyoda-go-spi`. `help/` is `cmd/cyoda/help/content/`.

## 1. Summary

At any time, at most one pnode **claims** a scheduled transition's task, and the
pnode that claims it runs it. The claim carries a token. The store refuses
bookkeeping written under a token that is no longer current. Each pnode proves
it is alive with one heartbeat record. Another pnode may claim a task only
after its owner has stopped heartbeating.

The platform never repeats a processor that is not declared `idempotent`.
Before it dispatches such a processor, the owner writes a mark on the task. A
task that carries the mark and did not commit becomes **FAILED** and is never
run again. A run that panics is also FAILED. So is a run that stopped after it
had committed the entity into another state.

A failure where nothing unsafe was handed off is retried, with a growing delay,
until the transition's own `timeoutMs` passes **[ruling]**. A task that loses
its owner `CYODA_SCHEDULER_MAX_LOST_OWNERS` (3) times becomes FAILED
**[ruling]**.

A FAILED task never moves the entity **[ruling]**. It is kept, and it is visible
through `GET /scheduled-tasks`, metrics, logs and an audit event on the entity.

The scanning coordinator, the round-robin distribution, the scheduler RPC, the
redispatch throttle and the expiry grace band are deleted.

## 2. Acceptance (issue #598)

The guarantees hold **per life** of a task (§3). An entity write that re-arms a
task starts a new life. From then on, the old run can no longer commit, write a
mark or record an outcome. If a callout of the old run is already in flight, it
is not stopped. So if the application re-arms a task while its run is calling
an unsafe processor, the new life may call that processor again. That repeat is
caused by the application's write **[ruling]**. The issue's acceptance text
will be reworded to say "per life".

| # | Acceptance | Met by |
|---|---|---|
| A1 | No run of a life starts while another run of the same life is in progress on a live pnode | §6.1 claim and heartbeat rules; §6.2 liveness; §6.3 self-cancel; §6.4 no give-back of a live run |
| A2 | A processor not declared safe to repeat is never executed again on the platform's initiative | §5.5 mark, serialised with the claim (C3) |
| A3 | A task that cannot succeed reaches a recorded, visible terminal state | §5.7; §8; §9. **[ruling]** A safe failure with no `timeoutMs` retries without end. It stays visible, but it never becomes terminal. The acceptance text will be reworded. |
| A4 | The deciding pnode learns the outcome; an empty or partial view is safe; runs in progress are bounded | §6.1: the claiming pnode runs the task itself and reads no cluster view; `MAX_RUNS` and a per-tenant cap bound the runs |
| A5 | Cross-backend parity and multi-node coverage | §13 |

## 3. Terms

- **Task**: the stored record "fire transition T of entity E at time X". Its id
  is a hash of (tenant, entity, source state, transition)
  (`internal/domain/workflow/arm.go:27-30`).
- **Arm**: create or replace a task. Every arm draws a new random **arm token**
  and starts a new **life**.
- **Claim**: a pnode takes a task to run it. Every claim draws a new random
  **claim token**. Tokens are UUIDs and are never reused.
- **Owner**: the pnode incarnation that holds a claim. A **pnode incarnation**
  is a random UUID drawn when a pnode process starts.
- **Run**: one attempt to fire a task, from the claim to its recorded outcome.
- **Hand-off**: a callout reaching a compute node (`member.Send`,
  `internal/grpc/dispatch.go:189`).
- **Unsafe processor**: a processor whose `config.idempotent` is not true
  (`SPI/types.go:244-251`).
- **Criteria and functions are always repeat-safe**
  (`internal/grpc/callout.go:191, 263`). A criterion or function whose
  callbacks trigger unsafe work is an unsupported pattern.
- **Store clock**: PostgreSQL `now()`, or the injected clock of the memory and
  SQLite stores.

## 4. Statuses and endings

```
          arm (entity write) — new life
               ▼
   ┌──────► WAITING ◄──────────── safe failure / given back
   │           │ claim                     ▲
   │           ▼                           │
   │        RUNNING ───────────────────────┘
   │           ├── fired / declined / expired / cancelled ──► task removed (+ audit)
   │           └── any FAILED reason ────────────────────────► FAILED (kept)
   └── entity write in the source state (from any status)
```

| Ending | Task afterwards | Audit on the entity | Log |
|---|---|---|---|
| Fired | removed; re-armed if the entity is back in the source state | `SCHEDULED_TRANSITION_FIRE` | DEBUG |
| Criterion false | removed | `TRANSITION_NOT_MATCH_CRITERION` | DEBUG |
| Late on the first attempt, no lost owner | removed | `SCHEDULED_TRANSITION_EXPIRE` | INFO |
| Transition gone; entity has no transaction id | removed | `SCHEDULED_TRANSITION_CANCEL` | as today |
| Entity gone, or moved on | removed | none (as today) | DEBUG |
| Safe failure | WAITING, attempts + 1 | none | WARN |
| Unsafe work handed off, run not committed | FAILED `UNSAFE_WORK_NOT_COMPLETED` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Owner lost `MAX_LOST_OWNERS` times | FAILED `OWNER_LOST_REPEATEDLY` | same | ERROR |
| Late after a failed attempt or a lost owner | FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | same | ERROR |
| The run panicked | FAILED `RUN_PANICKED` | same | ERROR, ticket |
| The run committed the entity into another state, then failed | FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | same | ERROR |

A FAILED task is never claimed. It ends in one of these ways (§7):
- the entity leaves the state (`SCHEDULED_TRANSITION_CANCEL`);
- the entity is written in the state (a new life);
- the transition stops being scheduled, at the entity's next write or at a
  workflow import;
- the entity is deleted.

## 5. The run

### 5.1 Before the run

After the claim (§6.1), the owner checks these in order:

1. **Mark set for this life** → FAILED `UNSAFE_WORK_NOT_COMPLETED`.
2. **`PartialCommit` set for this life** (§5.2) → FAILED
   `STOPPED_AFTER_PARTIAL_COMMIT`. An earlier run of this life committed the
   entity mid-cascade and did not finish. That run may have been cut by a
   crash.
3. **`lostOwners ≥ MAX_LOST_OWNERS`** → FAILED `OWNER_LOST_REPEATEDLY`.
4. **Deadline.** When `timeoutMs` is set, the deadline is `scheduledTime +
   timeoutMs`. The task is **late** in two cases:
   - `attempts == 0 && lostOwners == 0` and `now > deadline`. This is today's
     rule, measured on the owner's clock.
   - Otherwise, `now > deadline + RETRY_DELAY`.
  - The claimed record, not the claim, decides which case applies.

   A late task with no failed attempt and no lost owner is expired: it is
   removed with `SCHEDULED_TRANSITION_EXPIRE`, as today. Any other late task
   is FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`.
5. Otherwise, **run** it.

A pnode crash counts as a lost owner. So when a pnode dies, a task it was
running can end as `EXPIRED_AFTER_FAILED_ATTEMPTS` instead of being expired
silently. That happens when the reclaim comes after `deadline + RETRY_DELAY`,
which is certain when `timeoutMs + RETRY_DELAY < STALE_AFTER −
HEARTBEAT_INTERVAL`. That is intended.

The grace band (`fire_scheduled.go:50-63, 276-295`) and
`CYODA_SCHEDULER_EXPIRY_GRACE` are deleted. Only one owner decides now.

### 5.2 The run's transactions

`FireScheduledTransition(ctx, task, claim)` keeps its structure
(`internal/domain/workflow/fire_scheduled.go:89-524`), with these changes:

- **Re-read at the start of every segment.** Every segment of the run re-reads
  the task with `Get(tenant, id)` before anything else: the first transaction,
  and each segment after a `COMMIT_BEFORE_DISPATCH` commit. If the task is
  gone, or its arm token or claim token no longer matches, the run ends
  `superseded`.
- **"Superseded" is decided outside the run's transaction.** A run whose
  transaction fails with `ErrStaleClaim` or `ErrConflict` re-reads the task
  with a read that does not join. If its life and claim are still current, the
  failure came from inside its own transaction, for example the callback
  anti-pattern of §5.5. It is then a counted safe failure, recorded with
  `RecordAttempt`. Only a changed life or claim means `superseded`. So no
  failure is left unrecorded, and none turns into a give-back loop.
- **The run's final transaction always writes its own task row.**
  - A fired run removes the task through the re-arm step, or re-arms it for a
    self-loop.
  - Every other ending removes the task with `RemoveLife(tenant, id,
    armToken)`, which today is `Delete(id)`.
  - Task rows are under first-committer-wins on every backend (C1). So if the
    task was reclaimed, re-armed or removed by another transaction after this
    one began, the commit fails with `spi.ErrConflict`. The run then ends
    `superseded`.
- **`RemoveLife` removes only the life it names.** If this same transaction
  has already replaced or removed the task — a joined callback wrote the
  entity — the call does nothing. That is the ordinary-transition outcome for
  a processor that writes the entity it is processing, which the workflow docs
  advise against (`help/workflows.md:185-192`).
- **Deleted**, because the claim covers them:
  - the pre-transaction origin read (the claimed record carries `ArmedBy`);
  - the `ArmedBy` verify-or-abort (`:215-217`);
  - the re-armed-into-the-future guard (`:250-255`);
  - the tenant-mismatch guard (`:195-203`), which served only the scheduler
    RPC;
  - the grace-band drop.
- **Order at the end.** The fire path removes or re-arms its own task first,
  then runs the re-arm step for the final state, as today (`:470-489`). A
  self-loop therefore ends with the same id armed as a new life.

- **Every intermediate segment commit is stamped.** When a run guard is present,
  `flushAndCommitSegment` (`engine_processors.go:469-515`) does one thing
  before it commits a `COMMIT_BEFORE_DISPATCH` segment. As the last write of
  that segment it calls `StampSegment(tenant, id, armToken, claimToken,
  committedState)`, which joins the transaction. The stamp:
  - writes the task row, with both tokens in its condition. If no row matches,
    the result is `ErrStaleClaim` and the segment does not commit;
  - sets `PartialCommit` when the fired transition has already changed the
  entity's state before this segment. That covers a `COMMIT_BEFORE_DISPATCH`
  processor in a cascade step, including a cascade that loops back into the
  source state. A segment of the fired transition itself (the entity still
  in the source state) does not set it.

  The stamp checks that the segment's anchor entity is the fired entity; a
  segment of any other entity is not stamped. So every commit of a run writes
  its own task row, and C1 fences each one
  against a reclaim or a re-arm. The row lock is released at the segment's
  commit, before the processor is dispatched, so it cannot cause
  `ErrTaskBusy`.

**Why no other claim check is needed.** A segmented run's intermediate
flushes and its final write use a plain `Save` (`fire_scheduled.go:498-507`;
`flushAndCommitSegment` with `applyIfMatch` false). Each TX_post applies its
processor's result with a compare-and-save against TX_pre
(`engine_processors.go:421`). Three things fence the commits:
- the re-read at the start of each segment;
- the task-row write of every commit, through the stamp or the final write
  (C1);
- the watchdog margin (§6.3). Every commit of a replaced owner lands before
  another pnode can reclaim.

A2 does not depend on commits. It depends on the mark (§5.5).

### 5.3 The run's context and cancellation

- The run executes on a context derived from the scheduler's run context, not
  from `context.Background()` (`internal/scheduler/executor.go:44-45`). The
  system identity is attached to it as `common.SystemUserContext` builds it.
  The context carries a **run guard**: tenant, task id, arm token, claim token
  and the store.
- **The context is cancelled** when the shutdown drain ends (§6.4) or when the
  pnode cancels itself (§6.3). Cancellation cuts in-flight callouts
  (`internal/grpc/dispatch.go:194-200, 270-277`). A run whose unsafe callout
  was cut holds a mark, and becomes FAILED.
- **Segments after a `COMMIT_BEFORE_DISPATCH` commit** begin with
  `context.WithoutCancel(ctx)` (`engine_processors.go:388, 403, 530`), so a
  commit is never cut halfway. When a run guard is present, the engine
  therefore also checks the run's cancellation:
  - before each processor dispatch;
  - before each cascade step;
  - before the final persist.

  **Every callout of a guarded run carries the run's cancellation**: processors
  in every segment, both `COMMIT_BEFORE_DISPATCH` branches (`:360, :388`),
  criteria, and the re-arm step's `schedule.function` callouts. Transactions
  keep being begun with `WithoutCancel`.
- **Every commit of a guarded run** — each segment commit, and every
  `Commit` in `fire_scheduled.go` (`:173, 232, 246, 288, 337, 418, 440, 523`) —
  first checks the run's cancellation. It does not commit a cancelled run. It
  then commits shielded, with `CommitBudget` (`common.ShieldedCommitWithBudget`).
  The check runs immediately before the commit, after the segment's stamp.
  Only a commit that was already under way when the cancellation came can still
  land, and it lands within `CommitBudget`.

### 5.4 A run that stops after committing into another state

A `COMMIT_BEFORE_DISPATCH` processor inside a cascade step commits the entity
in that step's state (`engine.go:848` sets the state before
`cascadeAutomated`; `engine_processors.go:469-515`). If the run fails after
that, the entity is no longer in the source state, and the cascade did not
finish.

Suppose a run fails after it has committed a segment that set `PartialCommit`
(§5.2).

- **If the run is still alive**, it records FAILED
  `STOPPED_AFTER_PARTIAL_COMMIT` itself.
- **If the run died** (a crash, or a kill before the outcome was recorded), the
  next claim finds the stamp and records the same outcome (§5.1, step 2). The
  task is therefore never removed silently as "moved on".

A segment of the fired transition itself (a `COMMIT_BEFORE_DISPATCH` processor
on that transition, with the entity still in the source state) does not set
`PartialCommit`. The ordinary rules apply to it: a safe failure is retried from
that state, and a marked run is FAILED.

### 5.5 The unsafe mark

In `executeProcessors`, when the run guard is present and the processor is
unsafe, the following happens at each dispatch site (`engine_processors.go:230,
269, 360, 392`).

**Before dispatch**, unless this run already holds a mark, the engine calls
`MarkUnsafe(tenant, id, armToken, claimToken)`:

| Result | What the engine does |
|---|---|
| accepted | dispatch |
| `ErrStaleClaim` | do not dispatch; the run is `superseded` |
| `ErrMarkedByAnotherClaim` | do not dispatch; FAILED `UNSAFE_WORK_NOT_COMPLETED` (an earlier owner of this life marked it, and its outcome is unknown) |
| `ErrTaskBusy` (an open transaction has written or staged a write to the task row, §10.2, §10.3) | do not dispatch; safe failure |
| any other error | do not dispatch; the run ends as a counted safe failure with `RecordAttempt{Error, ClearOwnMark: true}` (`NotCounted` when the cause is a shutdown cancellation), which removes any mark written under this claim in the same step. A mark whose write did commit is therefore removed. Nothing was dispatched. `RecordAttempt` is retried as in §5.6 |

**After a failed dispatch** the mark is cleared only if the callout proves that
nothing was handed off.

**The proof is one fact: `HandedOff`.** The callout layer reports it on every
error it returns, including a cancellation. It is **true** when any try called
`member.Send` (`internal/grpc/dispatch.go:189`), or when a hand-over to a peer
pnode began. It stays true even when that try was abandoned because the context
was cancelled. That case matters: today such a try returns `ctx.Err()` after
`Send` (`dispatch.go:277`), and the coordinator records it only as "abandoned"
in its stats (`internal/callout/coordinator.go:199-203, 279`).

The changes this needs:
- every return path in `dispatch.go` after `Send` reports "handed off";
- the coordinator carries the fact onto the error it returns.

On cancellation, the error still satisfies `errors.Is(err, context.Canceled)`,
so existing callers are unaffected (V9). The proof is **not** read from the
attempt list or from `CalloutFailureKind`, whose zero value is `NoHandOff`
(`internal/contract/callout.go:21`).

When `HandedOff` is false, and no earlier unsafe processor of the run was handed
off, the engine calls `ClearUnsafe`. **Otherwise the mark stays.** A shutdown or
self-cancel therefore clears the mark of a processor that was never sent, and
keeps the mark of one that was.

How the mark applies:
- It belongs to the life: every later claim of that life sees it. A re-arm
  starts a new life, which has no mark.
- The pnode-to-pnode protocol does not change. The owner marks before it
  dispatches, so a #254 hand-over to a peer is covered.
- Callbacks inside an unsafe processor are covered by its mark. Callbacks
  inside an `idempotent` processor are covered by its declaration.
- `ASYNC_NEW_TX` processors (`:269`) are marked the same way. If the run
  commits, the task is completed. If it fails later, the task is FAILED.

**`ErrTaskBusy` and the anti-pattern.** If a joined callback wrote the fired
entity earlier in the run — the anti-pattern above — the run's own transaction
holds the task row. A later unsafe processor then gets `ErrTaskBusy`. The
attempt fails safely and is retried, and the pattern is visible in the query.
In a segmented run, the callback's re-arm makes the same segment's stamp refuse.
The segment rolls back, which also undoes the re-arm. The non-joining re-read
(§5.2) finds the life unchanged, so the run records a counted safe failure.
The pattern never hangs, never loops uncounted and never repeats unsafe work,
and it stays visible. The workflow docs name it as unsupported for scheduled runs.

**Effect on today's most common failure.** An unsafe processor followed by a
failing re-arm `schedule.function` (`executor.go:49-55`) now ends FAILED.
Today it repeats the unsafe processor every 30 s.

### 5.6 After the run: the outcome is always recorded

A run whose final transaction did not commit always ends with a fenced
bookkeeping write. Transient store errors are retried: after 1 s, then doubling
up to `HEARTBEAT_INTERVAL`, until the write is accepted or refused. At shutdown
the retries stop at the deadline of §6.4. A refusal means the run was
superseded.

| The run | Bookkeeping |
|---|---|
| holds a mark | `Fail(UNSAFE_WORK_NOT_COMPLETED)` |
| panicked | `Fail(RUN_PANICKED)` |
| §5.4 applies | `Fail(STOPPED_AFTER_PARTIAL_COMMIT)` |
| was cut by the shutdown drain, no mark | `RecordAttempt{NotCounted, NextAttemptTime: now}` |
| self-cancelled or failed, no mark | `RecordAttempt{Error, NextAttemptTime}` |
| superseded | the write its cause gives; the store refuses it |

`RecordAttempt` sets the task to WAITING. It adds 1 to `attempts` (not for
`NotCounted`), records the error (§5.8) and clears the claim. The retry delay
is:

```
delay = RETRY_DELAY × 2^(attempts−1), saturating at RETRY_DELAY_MAX
next  = now + delay;  if timeoutMs set: next = min(next, deadline)
```

- **Deadline already passed.** If the deadline has passed when the outcome is
  recorded, the owner calls `Fail(EXPIRED_AFTER_FAILED_ATTEMPTS)` instead.
  That includes a `NotCounted` shutdown attempt.
- **Log line.** The WARN line is written only after `RecordAttempt` is
  accepted.
- **Declines.** A criterion that evaluates to false declines the task, as
  today. That is not a failure.

### 5.7 FAILED

`Fail(tenant, id, armToken, claimToken, reason, error, atMs)` and the
`SCHEDULED_TRANSITION_FAIL` audit event are written in one small transaction.
The first statement of that transaction is the fenced update, so it reads the
latest committed row. The audit data is `{transition, sourceState, reason,
attempts, lostOwners}`.

The committed change is identified uniquely and stably by (task id, arm token).
That is where the planned notification feature can publish "timer failed".

### 5.8 The recorded error (Gate 3)

The error is shown to tenant users (§8), so it passes an **allow-list**.
`classifyWorkflowError` cannot be used here: its catch-all is a 400 carrying
`err.Error()` (`internal/domain/entity/service.go:2871`), and the fire path wraps
store errors in plain `fmt.Errorf` (`fire_scheduled.go:107, 118, 168, 234`).
What is shown:

- A `MemberFailed` `*contract.CalloutFailure` → the compute node's message,
  which the tenant owns (`internal/contract/callout.go:84`).
- A `*common.AppError` (via `errors.As`) with status < 500 → its code and
  message.
- `contract.ErrNoMatchingMember` → `NO_COMPUTE_MEMBER_FOR_TAG` and its
  message.
- Anything else → `internal error [ticket: <uuid>]`, with the full error
  logged at ERROR under that ticket.

The text is truncated to 1 024 bytes.

## 6. The scheduler service

`internal/scheduler` becomes one claim loop per pnode, plus a heartbeat
goroutine and a watchdog.

### 6.1 Claiming

The loop claims only when all of these hold:
- the pnode's first `Heartbeat` has succeeded, and heartbeats are succeeding
  again after any self-cancel;
- the node is not latched unhealthy (§6.5).

It may take a task from a lost owner (a **lost-owner claim**) only after its own
heartbeats have succeeded without a gap for at least `STALE_AFTER`. After a
database outage, every pnode's heartbeat is stale at once. This rule stops the
first pnode back from taking all the others' tasks and counting them as lost
owners.

The loop wakes every `CYODA_SCHEDULER_SCAN_INTERVAL` (1 s), and as soon as a
run slot frees if the previous claim filled every free slot.

**The call:**
```
ClaimDue(ClaimRequest{Owner, NowMs, StaleAfter, Limit, TenantInProgress map[TenantID]int,
                      PerTenantLimit, AllowLostOwner})
```
- `Limit` is the free slots: `CYODA_SCHEDULER_MAX_RUNS` (8) minus the runs in
  progress. With no free slot, the loop does not call.
- A tenant may have at most `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (4) runs in
  progress on this pnode. The store counts `TenantInProgress` against that
  cap.
- Tasks are taken in turn across tenants, in each tenant's `nextAttemptTime`
  order.

**Claimable** means:
- WAITING, and `nextAttemptTime ≤ NowMs` on the pnode clock (the clock that
  set `scheduledTime` at arm); or
- with `AllowLostOwner`: RUNNING, and the owner's liveness record is missing
  or older than `StaleAfter` by the store clock. That claim adds 1 to
  `lostOwners` in the same step.

**One task per entity at a time.** A task is not claimed while another task of
the same entity is RUNNING. At most one task per entity is claimed per call.
The store enforces this against concurrent callers too (§10.2). This removes
the races between the sibling tasks of one entity.

A claimed task becomes RUNNING, with a new claim token and this owner.

**Registration order (A1):**
- The loop records each claimed task in its set of live runs before it
  returns from the claim.
- A run leaves the set only after its outcome is accepted or refused.
- `ClaimDue` and `GiveBackIdle` are called only from the loop goroutine.

**Self-heal.** On every tick, `GiveBackIdle(owner, keep = live claim tokens)`
gives back each task this incarnation holds RUNNING that has no live run. This
is not counted as a lost owner. The mark belongs to the life, so a given-back
task that carries a mark becomes FAILED on its next claim.

### 6.2 Liveness

- **Heartbeat.** A dedicated goroutine writes `Heartbeat(incarnation)` every
  `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (15 s), from start to stop. The store
  stamps it with the store clock.
- **Stale period.** `CYODA_SCHEDULER_STALE_AFTER` (2 min). It is validated against the watchdog margin (§6.3).
- **Slow runs.** A slow run keeps its task, and so does a hung run whose pnode
  still heartbeats (as in #509).
- **Cleanup.** The claim loop removes the liveness records of dead
  incarnations after 10 × `STALE_AFTER`, once no task references them.

### 6.3 Self-cancel

**The watchdog.** A goroutine separate from the heartbeat. For each heartbeat
call it records, on the monotonic clock, the moment **before** the call started
to acquire its connection. Each call has a 10 s budget, covering both the
acquire and the statement. The store's stamp is therefore never earlier than
the recorded moment.

**The rule.** The watchdog arms a timer for `last success + W`, where
`W = STALE_AFTER − CommitBudget − 10 s` and "10 s" is slack for clock rate and
scheduling. Each successful heartbeat re-arms the timer. When it fires, the
pnode:
- cancels every run in progress (outcome `self_cancelled`);
- makes no claims until a heartbeat succeeds again.

`CommitBudget` is the shielded commit budget, 30 s
(`internal/common/reqtimeout.go:83-88`).

**Why that margin.** A cancelled run's shielded commit then lands no later than
`W + CommitBudget = STALE_AFTER − 10 s` after the recorded moment. Another
pnode may reclaim only `STALE_AFTER` after the store's stamp, which is later.
So the old owner's last write always comes before a reclaim. That is A1 on
live pnodes.

**Validation.** `STALE_AFTER ≥ CommitBudget + 10 s + 2 × HEARTBEAT_INTERVAL`,
so the default `STALE_AFTER` becomes 2 min.

A frozen VM whose monotonic clock does not advance is not stopped by the
watchdog. For unsafe work it is covered by C3. Its commits are fenced by C1.

### 6.4 Shutdown

Today the HTTP, admin and gRPC servers drain concurrently in an errgroup
(`cmd/cyoda/run.go:95-171`), and the scheduler stops afterwards, in
`a.Shutdown()`. New order on a signal:

1. The scheduler stops claiming.
2. It waits up to `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (20 s) for the runs in
   progress. Compute-node streams and callback routes are still open.
3. It cancels the remaining runs (§5.3) and waits up to 15 s more for each run
   to end and record its outcome. A cut unsafe callout ends FAILED.
4. The claim loop, which is the only caller of `GiveBackIdle` (§6.1), calls
   `GiveBackIdle(owner, keep = live claim tokens)` as its last act. This hands
   back only the claims whose run has ended but whose outcome did not reach the
   store. A run
   still live after step 3 is never given back. Its task stays RUNNING and is
   reclaimed after `STALE_AFTER` as a lost owner, once the process has exited.
   Then `RetireOwner` runs, if no run is still live.
5. The server drains start, as today.

If a server fails rather than a signal arriving, the same sequence runs from
`a.Shutdown()` after the servers stop, as a best effort.

From signal to exit this takes about 20 + 15 + 25 s (the existing tail,
`help/run.md:287`). The Helm chart sets `terminationGracePeriodSeconds: 60`.

### 6.5 Panics

Every new goroutine recovers panics and latches the node unhealthy. This is the
existing latch (`docs/ARCHITECTURE.md:382-393`): it is permanent, and an
operator replaces the node.

- **A panicking run** records `Fail(RUN_PANICKED)` with a ticket, and is not
  retried. A panic means state that nothing has verified, and running the task
  again on another pnode would spread it.
- **A latched pnode stops claiming.** It keeps heartbeating, so its runs still
  in progress are not taken over.
- **A panicking loop, heartbeat or watchdog** latches the node, and the
  recovery itself cancels every run in progress. It does not rely on the
  watchdog, which may be the goroutine that panicked. The runs record their
  outcomes as in §5.6.

### 6.6 Deleted

- `internal/scheduler/coordinator.go` and `distribution.go`.
- `ClusterExecutor`.
- `internal/cluster/scheduler_rpc.go`, with the
  `/internal/dispatch/scheduled-task` route and `SchedulerRPCClient`.
- `internal/cluster/config.go:25-27` (`DispatchForwardTimeout`).
- `Config.RedispatchBackoff` and `BatchSize`.
- Their wiring (`app/app.go:586, 600-657, 810-827`) and their tests, including
  `app/config_dispatch_test.go` and the forward-timeout rows of
  `app/config_registry_binding_test.go`.

## 7. Entity writes, workflow imports and tasks

- **Arm** (`reconcileScheduledTasks` → `ReconcileForEntity`). Every armed task
  starts a new life: WAITING, `nextAttemptTime = scheduledTime`, attempts and
  lost owners at 0, errors cleared, `PartialCommit` false, a new arm token, no
  claim. This happens whatever the old status was.
- **Cancel.** Reconcile removes every task of the entity that is not in the
  new arm set. That includes a task of the same state whose transition is no
  longer scheduled. Each removal records `SCHEDULED_TRANSITION_CANCEL`,
  except for the task that is firing.
- **Which writes reconcile.**
  - Today reconcile returns early when the selected workflow has no scheduled
    transition (`arm.go:96-98`). Now it returns early only when **no workflow
    of the model** has one. That flag is computed when workflows are loaded,
    so writes to models without schedules cost nothing extra (V7).
  - **Workflow import** removes, in its transaction, the model's tasks whose
    (source state, transition) is not a scheduled transition in any of the
    model's workflows (`internal/domain/workflow/handler.go:174`).
- **Entity delete removes the entity's tasks in the same transaction, on every
  path.**
  - `Handler.DeleteEntity` (`internal/domain/entity/service.go:656`) →
    `DeleteForEntities(tenant, [id])`.
  - `Handler.DeleteEntitiesConditional` (`:1205`), both in its
    single-transaction loop (`:1314`) and in each batch of `deleteBatched`
    (`:1697`) → `DeleteForEntities(tenant, ids actually deleted)`.
  - `Handler.DeleteAllEntities` (`:778`, also reached from `:1230`) →
    `DeleteForModel(tenant, model, version)`.
  - The gRPC doors reach the same functions (`internal/grpc/entity.go:200,
    484`).
- **These writes carry no claim.** Arm, cancel, import cleanup and delete are
  the application's decisions. If one commits first, the running owner's next
  task-row write fails (C1), and the owner is superseded.

- **D1 — decision for the product owner.** C1 works in both directions. A
  client write, a delete or a workflow import can fail with a retryable 409 if
  the scheduler changed that task row after the write began. The scheduler
  changes it by claiming the task, stamping a segment, recording an attempt,
  failing the task, or giving it back.
  - **When it happens:** a client writes an entity at the moment its timer is
    claimed or finishes an attempt. The client transaction must be open across
    that moment. That takes milliseconds for a plain update, and longer when
    the update runs processors.
  - **What a client sees:** the 409 a client already gets today when it races
    the timer *firing* on the same entity. It is retryable.
  - **New:** it also arises on memory and SQLite, which are made consistent
    with PostgreSQL. On PostgreSQL it already happens today through
    `MarkRedispatch`.
  - **Recommendation:** accept it, document it, and test it on every door.
  - **The alternative:** move the scheduler's bookkeeping to a separate row
    that client writes never touch. That is the v2/v3 design. It needs a claim
    check inside the entity transaction to fence the run's commits, and that is
    where three review rounds found their defects (§10.0).
  - **Coverage** (§13): an isolated E test per door (update, delete,
    conditional delete, delete-all, workflow import), plus gRPC.

## 8. The task query — `GET /scheduled-tasks`

**HTTP only.** This is an operator's view that no compute node needs. The
audit trail has no gRPC door for the same reason. The waiver is recorded in
§13.

**Access.** Any authenticated user of the tenant, as for the audit trail
(`internal/domain/audit/handler.go` checks no role). The tenant comes from the
token.

**Parameters** (all optional):

| Name | Type | Rule |
|---|---|---|
| `status` | repeatable; `WAITING`, `RUNNING`, `FAILED` | unknown value → 400 |
| `modelName` | string, 1–256 | |
| `modelVersion` | integer ≥ 1 | only with `modelName`; given alone → 400 |
| `entityId` | UUID | not a UUID → 400 |
| `cursor` | opaque, at most 256 characters | invalid → 400; the value is not echoed |
| `limit` | integer 1–1000, default 20 | out of range → 400 |

An out-of-range `limit` is rejected, not clamped. That is fail-closed, as in
direct search, `INVALID_LIMIT` and the transaction window. The audit endpoint's
clamp (`audit/handler.go:59-61`) is the exception, and is not copied here.

**Order and cursor.** Results are ordered by `(scheduledTime, taskId)`,
ascending. The cursor is versioned base64url JSON, decoded strictly like the
audit cursor (`internal/domain/audit/cursor.go:65-114`).

**Response 200:**
```
{ "items": [ScheduledTaskDto], "pagination": { "hasNext", "nextCursor" } }
```

`ScheduledTaskDto` (typed but open, ADR 0003):

| Field | Type | Present |
|---|---|---|
| `taskId` | string (a hash, not a UUID) | always |
| `entityId` | string, uuid | always |
| `modelName`, `modelVersion` | string, integer | always |
| `sourceState`, `transition` | string | always |
| `status` | string (open) | always |
| `scheduledTime` | date-time | always |
| `expiresTime` | date-time | when `timeoutMs` is set |
| `attempts`, `lostOwners` | integer | always |
| `nextAttemptTime` | date-time | when WAITING |
| `lastAttemptTime`, `lastError` | date-time, string | after a failed attempt |
| `failureReason` | string (open) | when FAILED |
| `failedTime` | date-time | when FAILED |
| `armedTime` | date-time | always |
| `armedBy` | `{id, kind}` | when known |

Node ids and tokens are not returned.

**Errors:**

| Status | Code | When |
|---|---|---|
| 200 | — | success, including an empty list |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName`; `modelVersion` not an integer ≥ 1; `entityId` not a UUID; `modelName` empty or too long; `limit` not an integer or outside 1–1000; invalid `cursor` |
| 401 | `UNAUTHORIZED` | no token, or an invalid one |
| 500 | `SERVER_ERROR` | internal failure; generic message and a ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no 403, because no role is required. There is no 404: an unknown
model or entity returns an empty list, and so does another tenant's entity.

## 9. Telemetry and logs

The instruments come from `observability.Meter()` and carry no tenant.

| Name | Type | Attributes |
|---|---|---|
| `cyoda.scheduler.runs` | counter | `outcome`: fired, declined, expired, cancelled, attempt_failed, failed, superseded, self_cancelled, shutdown_cancelled, panicked |
| `cyoda.scheduler.run.duration` | histogram, s | `outcome` |
| `cyoda.scheduler.runs.in_progress` | up-down counter | — |
| `cyoda.scheduler.claims` | counter | `reason`: due, owner_lost |
| `cyoda.scheduler.heartbeat.failures` | counter | — |

- **Span:** `scheduler.run`, carrying the outcome.
- **Logs:**
  - WARN per safe failure;
  - ERROR per FAILED task, with its reason and ticket;
  - WARN per self-cancel;
  - ERROR per panic.

All of it is documented in `help/telemetry.md`.

## 10. Storage contract (SPI)

### 10.0 What changed from v3, and why

v1–v3 checked the claim inside the entity transaction (`Complete`,
`CheckClaim`, then a separate run-state table and rule C5). Each review found
new holes there under PostgreSQL REPEATABLE READ: self-deadlock, write skew,
fences that did not fence, spurious conflicts.

That check is not needed:
- Data correctness comes from the entity compare-and-save.
- A2 comes from the mark written before dispatch.
- A1 comes from the claim and liveness.

What remains is one uniform rule: **task rows are under first-committer-wins on
every backend (C1)**, and a run's final transaction always writes its own task
row (§5.2). v4 therefore has one task table, a marks table and an owners table,
and no claim check inside entity transactions.

### 10.1 `ScheduledTaskStore`

```go
type ScheduledTaskStatus string        // "WAITING" | "RUNNING" | "FAILED"
type ScheduledTaskFailureReason string // "UNSAFE_WORK_NOT_COMPLETED" | "OWNER_LOST_REPEATEDLY" |
                                       // "EXPIRED_AFTER_FAILED_ATTEMPTS" | "RUN_PANICKED" | "STOPPED_AFTER_PARTIAL_COMMIT"
type ScheduledTask struct {
    // unchanged: ID, TenantID, Type, ScheduledTime, TimeoutMs, EntityID,
    // ModelName, ModelVersion, Transition, SourceState, ArmedAt, ArmedBy
    Status          ScheduledTaskStatus
    ArmToken        uuid.UUID // drawn by the store on every arm
    NextAttemptTime int64     // unix ms; = ScheduledTime on arm
    Attempts, LostOwners int
    LastAttemptTime *int64
    LastError       string
    FailureReason   ScheduledTaskFailureReason
    FailedTime      *int64
    PartialCommit   bool       // a segment committed after the fired transition changed the state (§5.2)
    Claim           *TaskClaim // RUNNING only: {Token, Owner uuid.UUID}
    UnsafeMarked    bool       // read-only: a mark exists for this life
}
```

**Removed:** `RedispatchAfter`, `AttemptCount`, `Upsert`, `ScanDue`,
`MarkRedispatch`, `Delete`.

**Fenced** means that the call is accepted only if the task's current arm token
and claim token are the given ones. Otherwise it returns `spi.ErrStaleClaim`. A
missing task counts as stale.

| Method | Transaction | Contract |
|---|---|---|
| `ReconcileForEntity(req)` | joins | arms `req.Arm` (each a new life); removes every other task of the entity; returns the removed tasks |
| `RemoveLife(tenant, id, armToken)` | joins | removes the task if its current life is `armToken`; else does nothing |
| `StampSegment(tenant, id, armToken, claimToken, partial bool)` | joins | fenced; writes the task row; sets `PartialCommit` when `partial` (§5.2) |
| `DeleteForEntities(tenant, ids)` | joins | removes those entities' tasks |
| `DeleteForModel(tenant, name, version, keep)` | joins | removes the model's tasks; with `keep`, only those whose (state, transition) is not kept |
| `Get(tenant, id)` | may join | tenant-scoped; sees the transaction's own staged operations (C2) |
| `Query(tenant, filter, cursor, limit)` | never joins; main pool | tenant-scoped page |
| `ClaimDue(req)` | never joins | atomic; disjoint across callers; at most one RUNNING task per entity; cross-tenant; §6.1 |
| `Heartbeat`, `RetireOwner`, `SweepOwners` | never join | the owner's liveness, on the store clock |
| `GiveBackIdle(owner, keep)` | never joins | RUNNING under `owner` and not in `keep` → WAITING; not counted |
| `MarkUnsafe(tenant, id, armToken, claimToken)` | never joins | fenced; serialised with `ClaimDue` (C3); idempotent for the same claim. Errors: `ErrMarkedByAnotherClaim` (a mark by another claim of the life); `ErrTaskBusy` (the row is being changed right now) |
| `ClearUnsafe(tenant, id, armToken, claimToken)` | never joins | fenced; removes this claim's mark |
| `RecordAttempt(tenant, id, armToken, claimToken, Attempt)` | never joins | fenced; WAITING; claim cleared; counts per §5.6; with `ClearOwnMark`, it removes a mark written under this claim in the same step |
| `Fail(tenant, id, armToken, claimToken, reason, error, atMs)` | joins the §5.7 transaction, as its first statement | fenced; FAILED; claim cleared |
| `SweepMarks()` | never joins | removes marks whose life has ended |

"Never joins" means the method ignores any transaction on `ctx` and commits on
its own. That is what lets a mark written during a run survive the run's
rollback. Today the PostgreSQL store (`plugins/postgres/scheduled_task_store.go:12-20`)
and the memory `stage()` join a transaction if there is one.

**New errors:** `ErrMarkedByAnotherClaim` and `ErrTaskBusy`. The
`ErrStaleClaim` doc comment (`SPI/errors.go:155`) is widened to both stores.

**Clauses:**
- **(C1) First-committer-wins covers task rows on every backend.** A
  transaction that writes a task row fails at commit with `spi.ErrConflict` if
  another transaction, joining or not, committed a write to that row after
  this one began. On PostgreSQL this is REPEATABLE READ behaviour (SQLSTATE
  40001 → `ErrConflict`, `plugins/postgres/transaction_manager.go:189`). On
  memory and SQLite, task rows join the committed-log conflict check that
  entities use, and every never-joining write that changes a task row also
  records a committed-log entry.
- **(C2) Reads by a joining call see the operations staged earlier in the same
  transaction.** Today memory and SQLite read committed state
  (`plugins/memory/scheduled_task_store.go:118-127, 165-176`,
  `plugins/sqlite/scheduled_task_store.go:188-191, 243-286`), while PostgreSQL
  reads its own writes. That divergence is fixed, and the comment at
  `fire_scheduled.go:476-486` is rewritten.
- **(C3) A `MarkUnsafe` and a `ClaimDue` that race on one task serialise.**
  Either the mark is refused, or the claim returns `UnsafeMarked`.
- **(C4) Entity transactions must not starve heartbeats and claims of
  connections.**
- **(C5) Conflicts have fixed error values.**
  - A write or commit refused under C1 returns an error for which
    `errors.Is(err, spi.ErrConflict)` holds, both at the statement and at
    commit. PostgreSQL raises the serialisation failure at the writing
    statement, after any lock wait. The task store maps SQLSTATE 40001 on its
    own statements to `ErrConflict`, as the transaction manager already does at
    commit (`plugins/postgres/transaction_manager.go:189`). A deadlock (SQLSTATE
    40P01) is mapped the same way. It can arise because a run segment writes
    the entity before the task row, while a client write reconciles the task
    rows before saving the entity (`engine.go:365`). The shared classifier
    already maps it (`plugins/postgres/classifying_querier.go:20-21`).
  - A fenced refusal returns `spi.ErrStaleClaim`.

The conformance cases move from `SPI/scheduled_task_store_conformance.go` into
`SPI/spitest`. They cover every method, refusal and clause, including:
- a mark survives the rollback of the transaction on `ctx`;
- after a re-arm, every fenced write of the old life is refused;
- two due siblings produce one claim.

A backend whose store returns "not implemented" (the commercial backend today)
skips them.

### 10.2 PostgreSQL

This is a new migration. There are no production instances, so the old columns
are simply replaced.

**Tables:**
- **`scheduled_tasks`**:
  - today's columns, minus `redispatch_after` and `attempt_count`;
  - plus `arm_token`, `status`, `next_attempt_time`, `attempts`,
    `lost_owners`, `last_attempt_time`, `last_error`, `failure_reason`,
    `failed_time`, `claim_token`, `claim_owner`.
  - Indexes:
    - `(next_attempt_time) WHERE status='WAITING'`;
    - `(claim_owner) WHERE status='RUNNING'`;
    - **`UNIQUE (tenant_id, entity_id) WHERE status='RUNNING'`**, the
      database-enforced "one task per entity";
    - `(tenant_id, scheduled_time, id)`;
    - `(tenant_id, model_name, model_version)`;
    - `(tenant_id, entity_id)`, kept;
    - `scheduled_tasks_due_idx` is dropped.
- **`scheduled_task_marks`** `(task_id, arm_token) PK, claim_token`. It is
  written only by never-joining methods and swept by `SweepMarks`.
- **`scheduler_owners`** `(owner PK, heartbeat_at)`.

**The dedicated pool (C4).** `CYODA_POSTGRES_SCHEDULER_CONNS` (3). Its sessions
are set to:
- READ COMMITTED;
- `statement_timeout` 30 s;
- `idle_in_transaction_session_timeout` 10 s;
- **`lock_timeout` 2 s**.

A statement that waits on a task-row lock gives up quickly, and the connection
is released. The lock can belong to an entity transaction that holds the row
for a long run. `lock_not_available` (SQLSTATE 55P03) is retried with the
backoff of §5.6 by the caller. For `MarkUnsafe` it is `ErrTaskBusy`. Acquiring
a connection times out after 5 s.
- It runs every never-joining method except `Query`, which runs on the main
  pool.
- `Heartbeat` has one extra connection of its own, with a 5 s acquire timeout.
- The async-search heartbeat and claim (`plugins/postgres/search_store.go`)
  move to this pool as well.

**`ClaimDue`.** One transaction on the dedicated pool:
1. A subquery ranks the candidates:
   - WAITING and due, or RUNNING with a stale or missing owner when
     `AllowLostOwner`;
   - `NOT EXISTS` for a RUNNING task of the same entity other than itself;
   - `DISTINCT ON (tenant_id, entity_id)`;
   - `row_number() OVER (PARTITION BY tenant_id …)` against the tenant caps.
2. An outer `SELECT … FOR UPDATE SKIP LOCKED` over those ids, ordered by id.
   Its `WHERE` clause repeats the row's own claim condition (status, due time,
   claim token read in step 1). After the lock, PostgreSQL re-evaluates it on
   the latest row version.
   - PostgreSQL does not allow `FOR UPDATE` together with window functions at
     one query level, so the ranking sits in the subquery.
   - SKIP LOCKED after the ranking can yield fewer rows than it could. That is
     accepted.
3. `UPDATE … WHERE id = ANY($locked) AND <the whole claim condition, including
   the sibling test and the owner staleness> RETURNING`. It is a new statement
   with a new snapshot, so it sees a claim that another pnode committed after
   step 1. **Step 3 is what closes the race.** The condition must never be
   removed from it.
4. After the row locks are held, a second statement reads the marks of the
   claimed rows (C3).

Two pnodes that claim two siblings at once collide on the unique index. The
later one gets a unique violation, or a deadlock (SQLSTATE 40P01) if two
claims each hold a sibling the other wants. In both cases its claim
transaction is rolled back and claims nothing this tick. The event is logged at
DEBUG, and the claim is retried on the next tick.

**`MarkUnsafe`.** One short transaction:
1. `SELECT … FROM scheduled_tasks WHERE id, tenant_id, arm_token, claim_token
   FOR SHARE NOWAIT`:
   - no row → `ErrStaleClaim`;
   - lock not available (SQLSTATE 55P03) → `ErrTaskBusy`.
2. `INSERT … ON CONFLICT (task_id, arm_token) DO NOTHING RETURNING
   claim_token`. On conflict: the same token → accepted; another token →
   `ErrMarkedByAnotherClaim`.

How this serialises with a claim:
- A claim that commits first changes the claim token, so step 1 finds no row.
- A claim still in progress holds the row lock, so step 1 returns
  `ErrTaskBusy`.
- A mark that holds its share lock first makes the claim's `SKIP LOCKED` skip
  the row, and the next scan sees the mark.

The run's own transaction does not hold the task row, except in the
anti-pattern of §5.5.

**`ClearUnsafe`, `RecordAttempt`, `GiveBackIdle`, `Fail`.** Conditional
statements, with `WHERE id, tenant_id, arm_token, claim_token` or the owner.
Zero rows means `ErrStaleClaim`.

**Tenant scoping.**
- Every tenant-facing method filters on `tenant_id`.
- The cross-tenant statements are the scheduler's own: `ClaimDue`,
  `GiveBackIdle`, the owner methods and the sweepers.
- The tables stay outside row-level security, and the migration comment that
  says otherwise (`000004_scheduled_tasks.up.sql:5-14`) is corrected.

### 10.3 Memory and SQLite

These backends run on a single pnode, but they meet the same contract.

- **C1.**
  - Task rows get keys in the transaction's write set, next to entity ids, and
    the commit's conflict check covers them (`plugins/memory/txmanager.go:488-513`,
    `plugins/sqlite/txmanager.go:517-535`).
  - A never-joining write that changes a task row (claim, `RecordAttempt`,
    `GiveBackIdle`, `Fail`) appends a committed-log entry carrying that key.
    It does so under the same commit gate that `Commit` holds, so it cannot
    race `Commit`'s check.
  - **Ordering must be strict.** Memory orders by a sequence number
    (`plugins/memory/txmanager.go:499-503`). SQLite today compares submit
    times with `>=` against a snapshot time that can equal the last submit
    time (`plugins/sqlite/txmanager.go:427-431, 525`). With a frozen or
    lagging clock, a run would then conflict with its own claim. SQLite moves
    to a sequence number for this check, as memory has.
  - This is **V1**.
- **Marks are kept apart from task rows** on memory and SQLite as well.
  `MarkUnsafe` and `ClearUnsafe` never write a task row and never append to the
  committed log. Otherwise a run's own mark would make its final commit fail.
- **`ErrTaskBusy`** means: an open transaction has staged a write to this task
  row. Both stores already keep staged operations per transaction
  (`scheduledTaskOps`), and `MarkUnsafe` checks them under the store's lock.
  This matches PostgreSQL, where any open transaction that wrote the row holds
  its lock.
- **C2.** A read by a joining call overlays the transaction's staged
  operations on committed state.
- **Never-joining methods** apply at once and ignore any transaction on `ctx`:
  memory under `entityMu`, SQLite on the writer connection. The claim follows
  the async-search pattern (`plugins/sqlite/search_store.go:404-496`).
- **One task per entity** is checked inside the claim, under the store's lock.
- **Serialisation.** Both backends serialise every never-joining write, so C3
  reduces to checks run under that lock.
- **Staged removals are expanded to task ids** at staging time
  (`DeleteForEntities`, `DeleteForModel`, reconcile cancels). The C1 write set
  and the `ErrTaskBusy` check can then see them.
- **Removed.** `Delete`, and with it the unreliable "was it removed?" answer
  (R§2.5 item 9).

## 11. Configuration (Gate 4)

| Variable | Default | Rule | Status |
|---|---|---|---|
| `CYODA_SCHEDULER_ENABLED` | true | | kept |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | 1s | > 0 | kept, now validated |
| `CYODA_SCHEDULER_MAX_RUNS` | 8 | ≥ 1 | new |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | 4 | 1..MAX_RUNS | new |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | 15s | > 0 | new |
| `CYODA_SCHEDULER_STALE_AFTER` | 2m | ≥ 30 s commit budget + 10 s + 2 × heartbeat (§6.3) | new |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | 3 | ≥ 1 | new |
| `CYODA_SCHEDULER_RETRY_DELAY` | 30s | > 0 | new |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | 15m | ≥ retry delay | new |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | 20s | ≥ 0 | new |
| `CYODA_POSTGRES_SCHEDULER_CONNS` | 3 | ≥ 2 | new (postgres plugin) |
| `CYODA_SCHEDULER_DISTRIBUTION`, `_COORDINATOR`, `_REDISPATCH_BACKOFF`, `_BATCH_SIZE`, `_EXPIRY_GRACE` | — | | removed |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | — | | removed (its only user was the scheduler RPC) |

These are edited in:
- `app/config.go` (`DefaultConfig`, `Validate`, `:176, :181, :468, :474-482, :945-946`);
- `cmd/cyoda/help/config_registry.go` (`:80, :131-137`);
- `help/config/scheduler.md`, `help/config/cluster.md`,
  `help/config/database.md` and `help/config.md:33`;
- `README.md` (`:224-236, :251`);
- `plugins/postgres/config.go`, `plugin.go`, `doc.go`;
- `docs/plugins/POSTGRES.md`.

Startup fails on an invalid value.

## 12. Documentation, parity and other repositories

- **`help/workflows.md`, "SCHEDULED TRANSITIONS":**
  - one owner per run;
  - retry until `timeoutMs`;
  - the FAILED reasons;
  - the new audit event;
  - entity writes and workflow imports;
  - one task per entity at a time;
  - the callback anti-pattern and what it does to a scheduled run (§5.5);
  - criteria and functions must not trigger unsafe work.
- **`help/workflows.md:182`, and the `idempotent` description in
  `api/openapi.yaml`:** the declaration now also governs scheduled runs.
  - No `WorkflowConfigurationDto` schema-version bump.
  - The rationale goes into `docs/workflow-schema-versioning.md` under "When
    NOT to bump": same shape, same accepted values, and a safer engine for an
    unchanged document. The v0.8.4 evaluation-time entries are the precedent.
- **New help topic `scheduled-tasks`**, shaped like `audit.md`, added to
  `topLevelTopicsV061`.
- **Other help topics:** `help/run.md` "SHUTDOWN TIMING"; `help/telemetry.md`;
  `help/helm.md`.
- **`api/openapi.yaml`:**
  - the operation and its DTOs;
  - `SCHEDULED_TRANSITION_FAIL` in the audit enum (`:11742`);
  - `go generate ./api`.
- **SPI:**
  - `SMEventScheduledTransitionFailed`;
  - the new errors;
  - the `ErrStaleClaim` doc comment;
  - remove the stale sentence in `TransitionSchedule`'s doc.
- **Helm chart:**
  - `terminationGracePeriodSeconds` in `templates/statefulset.yaml`,
    `values.yaml`, `values.schema.json` and the chart `README.md`;
  - a chart `version:` bump, with its `COMPATIBILITY.md` entry.
- **`docs/cloud-parity/scheduled-transitions.md`, rewritten (Gate 7):**
  - §1: the grace band is removed.
  - §5: any write resets the timer, FAILED included.
  - §6: the audit set.
  - §7 and §8 are replaced by: one owner per life; unsafe work is never
    repeated; retry until `timeoutMs`; the FAILED reasons; the lost-owner cap;
    one task per entity; the query; entity delete and workflow import.
  - A CaaS ticket is filed.
- **`docs/ARCHITECTURE.md`**: the scheduler passages, in the present tense
  (`:94, :230, :382-393, :772, :940, :943, :1359, :1486-1506, :1550-1553,
  :1992, :2248`).
- **`internal/domain/search/reaper.go:18`**: fix the comment.
- **`CHANGELOG.md` `### Breaking`**: the removed variables, the new
  processor-repeat rule, FAILED, one task per entity, the query.
- **`COMPATIBILITY.md`**: the SPI pin and the chart.
- **SPI release:** an SPI PR into `main`, which cyoda-go pseudo-pins. No tag.
- **cyoda-go-cassandra#68**: updated to this contract.

## 13. Coverage matrix

**Layers:**
- **U** — unit.
- **S** — `spitest` on memory, SQLite and PostgreSQL.
- **E** — `internal/e2e` over HTTP on PostgreSQL.
- **P** — a parity scenario registered in `e2e/parity/registry.go`.
- **M** — multi-node PostgreSQL. The fixture already kills a node
  (`e2e/parity/postgres/async_node_crash_test.go:48-61`).
- **1** — the single-node SQLite restart test.

**Rules:**
- Concurrency cases stay out of P.
- Timing cases may be in P, like today's `FiresOnTime` and
  `ExpiryElapsedExpiresNoFire`.
- The fixtures set short `RETRY_DELAY`, `HEARTBEAT_INTERVAL` and `STALE_AFTER`
  in `e2e/parity/fixtureutil/tuned_env.go` (`:18`).

### Endings

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| fired on time | ✓ | | ✓ | ✓ | |
| fired after one safe failure (compute node down, then up) | ✓ | | ✓ | ✓ | |
| a self-loop fires and re-arms the same id as a new life | ✓ | ✓ | ✓ | ✓ | |
| declined | ✓ | | ✓ | ✓ | |
| expired, late on the first attempt | ✓ | | ✓ | ✓ | |
| safe failure: criterion error → WAITING, attempts 1, error recorded | ✓ | | ✓ | ✓ | |
| safe failure: no compute node → mark cleared, WAITING | ✓ | | ✓ | ✓ | |
| safe failure: an idempotent processor fails → WAITING | ✓ | | ✓ | ✓ | |
| retry delay doubles, saturates and is clamped; runs up to `RETRY_DELAY` past the deadline; later → FAILED | ✓ | | | | |
| late after failed attempts → FAILED | ✓ | | ✓ | ✓ | |
| an unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never re-run | ✓ | | ✓ | ✓ | |
| a later step fails after an unsafe hand-off → FAILED | ✓ | | ✓ | ✓ | |
| database outage during `MarkUnsafe` → `RecordAttempt{ClearOwnMark}` after recovery, WAITING, not FAILED | ✓ | ✓ | | | |
| `ErrMarkedByAnotherClaim` → FAILED under the current claim | ✓ | ✓ | | | |
| `ErrTaskBusy` → not dispatched, safe failure | ✓ | ✓ | | | |
| an unsafe `ASYNC_NEW_TX` processor fails and the run commits → completed | ✓ | | ✓ | | |
| CBD on the fired transition: TX_pre committed, then a failure, all idempotent → retried from the TX_pre state | ✓ | | ✓ | | |
| CBD in a cascade step, then a failure → FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | ✓ | | ✓ | | |
| owner killed after a cascade-step segment commit → the next claim records FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | ✓ | ✓ | | | M |
| a replaced owner's segment commit is refused by its stamp (C1) | ✓ | ✓ | ✓ | | |
| cancelled before `Send` → `HandedOff` false, mark cleared, safe failure | ✓ | | | | |
| cancelled after `Send` (an earlier try was not handed off) → `HandedOff` true, mark stays, FAILED | ✓ | | | | |
| a callout abandoned mid hand-over reports `HandedOff` true | ✓ | | | | |
| CBD: cancellation after TX_pre stops the run at the next step | ✓ | | ✓ | | |
| a joined callback writes the fired entity; no unsafe processor follows → same outcome as an ordinary transition | ✓ | ✓ | ✓ | | |
| a joined callback writes the fired entity, then an unsafe processor → `ErrTaskBusy`, safe failure, no hang | ✓ | | ✓ | | |
| the same on memory and SQLite: `ErrTaskBusy` from a staged write, same outcome as PostgreSQL | | ✓ | | | |
| a client re-arm in flight when an unsafe processor is about to be marked → `ErrTaskBusy` on every backend | | ✓ | | | |
| a joined callback writes the fired entity inside a segmented run → superseded at the next segment | ✓ | | ✓ | | |
| a joined callback deletes the fired entity → the run commits | ✓ | ✓ | ✓ | | |
| FAILED audit event recorded with its reason | ✓ | | ✓ | ✓ | |
| a panicking run → FAILED `RUN_PANICKED`; node latched; claims stop | ✓ | | | | |
| `lastError` for a non-sentinel store error shows only "internal error [ticket]" | ✓ | | ✓ | | |

### Ownership, fencing and liveness

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| concurrent `ClaimDue` calls get disjoint sets | | ✓ | | | M |
| two due siblings: one claim per call | | ✓ | | | |
| two due siblings claimed by two pnodes at once: one wins, the other claims nothing that tick | | ✓ | | | M |
| contended `ClaimDue` loop on PostgreSQL: a task claimed by another pnode between ranking and locking is never re-claimed | | ✓ | | | M |
| a dedicated-pool statement blocked on a task-row lock gives up after `lock_timeout` and frees its connection | | ✓ | ✓ | | |
| SQLite: a run never conflicts with its own claim under a frozen clock | | ✓ | | | |
| per-tenant limit, and turn-taking across tenants | | ✓ | | | |
| a run longer than 3 × the heartbeat interval is not claimed by another pnode | | | | | M |
| owner killed, no mark → claimed after `STALE_AFTER`, `lostOwners` 1, fires | | | | | M |
| owner killed with a mark → FAILED, the processor was sent once | | | | | M |
| owner killed, `timeoutMs` < `STALE_AFTER` → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | | | | | M |
| a single-node SQLite restart reclaims its own RUNNING tasks as lost owners | | | | | 1 |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | ✓ | ✓ | | | |
| database outage longer than `STALE_AFTER` → no lost-owner claims until a full stale period of healthy heartbeats | ✓ | | | | M |
| a stale token is refused by every fenced method | | ✓ | | | |
| after a re-arm, every fenced write of the old life is refused | ✓ | ✓ | ✓ | | |
| a reclaimed or re-armed task makes the old run's final commit fail (C1) | ✓ | ✓ | ✓ | | |
| `MarkUnsafe` racing `ClaimDue` (C3) | | ✓ | | | M |
| a mark survives the rollback of the transaction on `ctx` | | ✓ | | | |
| a joining read sees the transaction's staged operations (C2) | | ✓ | | | |
| a superseded owner sends no unsafe processor | ✓ | | ✓ | | |
| ABA: the old token is refused after re-arm and a new claim | ✓ | ✓ | | | |
| heartbeat failure → self-cancel before `STALE_AFTER`; no claims until recovery | ✓ | | | | |
| a replaced owner's last commit lands before any reclaim: cancellation reaches every callout and every commit checks it (watchdog margin) | ✓ | | | | |
| a cascade that loops back into the source state with a CBD step sets `PartialCommit` | ✓ | | ✓ | | |
| a re-arm resets `PartialCommit` | ✓ | ✓ | | | |
| a stale or conflicting refusal from inside the run's own transaction is a counted safe failure, not superseded | ✓ | ✓ | | | |
| a panicking watchdog: the latch cancels the runs directly | ✓ | | | | |
| a hung heartbeat does not stop the watchdog | ✓ | | | | |
| no claim before the first heartbeat | ✓ | | | | |
| heartbeats are not starved when every main-pool connection is busy (C4) | | | ✓ | | |
| a RUNNING task with no live run is given back; a live run never is | ✓ | ✓ | | | |
| at most `MAX_RUNS`; the next claim waits for a slot; a freed slot triggers an immediate claim | ✓ | | ✓ | | |
| an empty cluster view has no effect | ✓ | | | | M |
| dead-owner records and ended-life marks are swept | | ✓ | | | |
| instruments of §9 are emitted with their attributes | ✓ | | | | |

### Shutdown

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| runs finish within the drain; streams stay open during it | ✓ | | ✓ | | |
| a run cut after the drain with no mark → WAITING, not counted, claimed at once elsewhere | ✓ | | | | M |
| a run cut after the drain with a mark → FAILED | ✓ | | ✓ | | |
| a run still live after step 3 is not given back | ✓ | | | | |
| bookkeeping at shutdown stops at its deadline | ✓ | | | | |
| `GiveBackIdle` is not counted; `RetireOwner` removes liveness | | ✓ | | | |

### Entity writes and workflow import

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| a FAILED task re-armed by an update in the state (new life, no mark) | ✓ | ✓ | ✓ | ✓ | |
| a FAILED task cancelled when the entity leaves the state | ✓ | | ✓ | ✓ | |
| a task whose transition is no longer scheduled is removed at the next write | ✓ | ✓ | ✓ | ✓ | |
| a workflow import that drops schedules removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |
| delete one entity removes its tasks (HTTP, gRPC) | ✓ | ✓ | ✓ | ✓ | |
| conditional delete removes tasks: single-tx loop, batched, fast path (HTTP, gRPC) | ✓ | ✓ | ✓ | ✓ | |
| delete-all removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |
| `DeleteForModel` in tenant A leaves tenant B's tasks alone | | ✓ | ✓ | | |
| a client write racing a task's claim or outcome gets the same result on every backend (C1) | | ✓ | | | |
| D1: a client update, delete, conditional delete, delete-all and workflow import racing a claim → retryable 409 (HTTP, isolated) | | | ✓ | | |
| D1: the same on gRPC entity doors | ✓ (`internal/grpc`) | | | | |

### Query

| Scenario | U | S | E | P |
|---|---|---|---|---|
| 200, no filter, several pages | ✓ | ✓ | ✓ | ✓ |
| 200, each filter: status (one and several), model name, name and version, entity | ✓ | ✓ | ✓ | ✓ |
| 200 empty: unknown model, unknown entity, another tenant's entity | | | ✓ | |
| 200, a FAILED item shows reason, error, times, attempts | | | ✓ | ✓ |
| 400 unknown `status` | ✓ | | ✓ | |
| 400 `modelVersion` without `modelName` | ✓ | | ✓ | |
| 400 `modelVersion` not an integer ≥ 1 | ✓ | | ✓ | |
| 400 `entityId` not a UUID | | | ✓ | |
| 400 `modelName` empty or too long | ✓ | | ✓ | |
| 400 `limit` 0, 1001, not an integer | ✓ | | ✓ | |
| 400 invalid cursor, value not echoed | ✓ | | ✓ | |
| 401 no token; 401 invalid token | | | ✓ | |
| 500 SERVER_ERROR with a ticket (store double) | ✓ | | ✓ | |
| 503 STORAGE_UNAVAILABLE (store double) | ✓ | | ✓ | |
| another tenant's tasks are never returned under any filter | | ✓ | ✓ | ✓ |

**gRPC is waived.** The query has no gRPC door. The only gRPC change is at the
delete doors, and `internal/grpc` gets one test per door asserting that the
tasks are removed.

### Configuration

| Scenario | U |
|---|---|
| each new variable: its default and its validation failure | ✓ |
| each removed variable is no longer read (§15) | ✓ |

## 14. Dependencies and scope

- **#599.** The v4 guarantees no longer depend on the claim check at an
  intermediate commit. #599 still matters on its own: a callback must not
  commit the outer operation's transaction. It is not a blocker for #598.
- **Not included:**
  - an API to retry or dismiss a FAILED task;
  - notifications (§5.7 names where they would publish);
  - reclaiming a hung pnode that still heartbeats;
  - #600.

## 15. Exit checks

Each of these returns nothing in the root module, `plugins/*` and the SPI.
They exclude `docs/plans/`, `docs/superpowers/`, `docs/release-notes/` and
`CHANGELOG.md`.

```
grep -rn "RedispatchAfter\|RedispatchBackoff\|MarkRedispatch\|AttemptCount\|ScanDue\|redispatch_after\|attempt_count" .
grep -rn "LowestLiveNodeID\|scheduler\.RoundRobin\|SchedulerRPC\|ClusterExecutor\|dispatch/scheduled-task\|DispatchForwardTimeout" --include='*.go' .
grep -rn "CYODA_SCHEDULER_DISTRIBUTION\|CYODA_SCHEDULER_COORDINATOR\|CYODA_SCHEDULER_REDISPATCH_BACKOFF\|CYODA_SCHEDULER_BATCH_SIZE\|CYODA_SCHEDULER_EXPIRY_GRACE\|CYODA_DISPATCH_FORWARD_TIMEOUT" . \
  --exclude-dir=plans --exclude-dir=superpowers --exclude-dir=release-notes --exclude=CHANGELOG.md
grep -rn "ExpiryGrace\|expiryGrace" --include='*.go' .
grep -rn "ScheduledTaskStore.*Upsert\|\.Upsert(ctx, task\|sts\.Delete(" --include='*.go' .
```

The `RoundRobin` check is qualified with `scheduler.` because
`internal/grpc/selector.go` has its own `RoundRobin`, which stays.

## 16. Verification points

**Settled:**
- **V2.** The store's callers are `arm.go:165`, `fire_scheduled.go:101, 152`,
  `scheduler/service.go:161`, and the pass-through at
  `internal/cluster/modelcache/factory.go:76`.
- **V4.** The multi-node fixture can kill a node.

**Open (planning, before code):**
- **V1.** Memory and SQLite can put task-row keys into the committed-log
  conflict check, and append log entries for never-joining writes, without
  disturbing the entity conflict check.
- **V5.** The existing scheduled tests that rely on removed behaviour are
  rewritten against this spec, not deleted. The dual-coordinator tests in
  `fire_scheduled_concurrency_test.go` become claim-race tests.
- **V6.** `runServers` can take the new order: the scheduler drain first on
  the signal path, and last on the server-failure path.
- **V7.** The engine can compute "does any workflow of this model schedule a
  transition" without an extra store read per write.
- **V8.** On PostgreSQL, a unique-index wait in `ClaimDue` is bounded by
  `lock_timeout` (2 s). Confirm the loser rolls back and claims nothing that
  tick.
- **V9.** Every return path after `member.Send`, including cancellation,
  and every hand-over that has begun can report `HandedOff`. The flag is
  carried onto the coordinator's error while `errors.Is(err,
  context.Canceled)` still holds for every existing caller (§5.5).
- **V10.** The shielded commit budget (`common.CommitBudget`) is the upper bound
  of every segment commit of a scheduled run, including a segment of the
  final persist (§6.3).
