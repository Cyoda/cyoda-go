# #598 — every scheduled run has one owner

Milestone v0.9.0.

- Research: `docs/superpowers/research/2026-09-24-598-scheduler-ownership-research.md`.
- **[ruling]** marks a product-owner decision.
- Path prefixes: `SPI/` is `cyoda-go-spi`; `help/` is `cmd/cyoda/help/content/`.

## 1. Summary

- **Ownership.** At most one pnode at a time **claims** a scheduled
  transition's task, and the claiming pnode runs it. That pnode is the task's
  **owner** (§3). The owner is never the user of the run's transaction.
- **Fencing.** Every write the owner makes is checked against its claim token.
- **Liveness.** A pnode proves it is alive with one heartbeat record. Another
  pnode may claim a task only after the owner has stopped heartbeating.
- **No repeats.** A processor not declared `idempotent` is never repeated by the
  platform. Before such a processor is dispatched, the owner writes a mark on
  the task. If the processor reached a compute node and the run did not commit,
  the task becomes **FAILED** and is never run again.
- **Retries.** A failure where no unsafe work was handed off is retried, with a
  growing delay, until the transition's own `timeoutMs` passes **[ruling]**.
- **Lost owners.** A task whose owner is lost `CYODA_SCHEDULER_MAX_LOST_OWNERS`
  (3) times becomes FAILED **[ruling]**.
- **Visibility.** A FAILED task never moves the entity **[ruling]**. It is kept,
  and is visible through `GET /scheduled-tasks`, metrics, logs, and an audit
  event on the entity.
- **Removed:**
  - the scanning coordinator;
  - the round-robin distribution;
  - the scheduler RPC;
  - the redispatch throttle;
  - the expiry grace band.

## 2. Acceptance

The guarantees hold **per life** of a task (§3). An entity write re-arms a task
and starts a new life. From then on, the old run cannot commit, write a mark, or
record an outcome. A callout of the old run that is already in flight is not
stopped. If the application re-arms a task while that callout calls an unsafe
processor, the new life may call the processor again. That repeat is caused by
the application's write **[ruling]**.

| # | Acceptance (#598, per life) | Met by |
|---|---|---|
| A1 | No run starts while another run of the same life is in progress on a live pnode | §6.1, §6.2, §6.3, §6.4, C6 |
| A2 | A processor not declared safe to repeat is never executed again on the platform's initiative | §5.5, C3 |
| A3 | A task that cannot succeed reaches a recorded, visible terminal state | §5.7, §8, §9. **[ruling]** A safe failure with no `timeoutMs` retries without end: it is visible, never terminal |
| A4 | The deciding pnode learns the outcome; an empty or partial cluster view is safe; runs in progress are bounded | §6.1: the claiming pnode runs the task and reads no cluster view; `MAX_RUNS` and a per-tenant cap bound the runs |
| A5 | Cross-backend parity and multi-node coverage | §13 |

The issue's acceptance text is reworded to say "per life" and to carry the A3
ruling.

## 3. Terms

- **Task**: the stored record "fire transition T of entity E at time X". Its id
  hashes (tenant, entity, source state, transition)
  (`internal/domain/workflow/arm.go:27-30`).
- **Arm**: create or replace a task. Every arm draws a new random **arm token**
  and starts a new **life**.
- **Claim**: a pnode takes a task in order to run it. Every claim draws a new
  random **claim token**. Tokens are UUIDs and are never reused.
- **Owner**: the pnode **incarnation** that holds the claim and runs the task.
  An incarnation is a random UUID drawn when the pnode process starts. The
  owner is a processing node, not the user or principal of the run's
  transaction.
- **Run**: one attempt to fire a task, from the claim to its recorded outcome.
- **Hand-off**: `member.Send` returning nil (`internal/grpc/dispatch.go:133-134,
  189`).
- **Unsafe processor**: a processor whose `config.idempotent` is not true
  (`SPI/types.go:244-251`).
- **Criteria and functions** are always repeat-safe
  (`internal/grpc/callout.go:191, 263`). A criterion or function whose
  callbacks trigger unsafe work is unsupported.
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
| Transition no longer scheduled in the selected workflow | removed | `SCHEDULED_TRANSITION_CANCEL` | DEBUG |
| Entity has no transaction id to guard the fire | removed | `SCHEDULED_TRANSITION_CANCEL` | ERROR |
| Entity gone, or moved on | removed | none | DEBUG |
| Safe failure | WAITING, attempts + 1 | none | WARN |
| Unsafe work handed off, run not committed | FAILED `UNSAFE_WORK_NOT_COMPLETED` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Owner lost `MAX_LOST_OWNERS` times | FAILED `OWNER_LOST_REPEATEDLY` | same | ERROR |
| Late after a failed attempt or a lost owner | FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | same | ERROR |
| The run panicked | FAILED `RUN_PANICKED` | same | ERROR, ticket |
| The run committed the entity into another state, then stopped | FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | same | ERROR |

A FAILED task is never claimed. One of these ends it (§7):
- the entity leaves the state → removed, with `SCHEDULED_TRANSITION_CANCEL`;
- the entity is written in the state → a new life;
- the transition stops being scheduled → removed at the next write or import;
- the entity is deleted → removed.

## 5. The run

### 5.1 Before the run

After it claims a task, the owner checks these in order:

1. A mark exists for this life → FAILED `UNSAFE_WORK_NOT_COMPLETED`.
2. `PartialCommit` is set for this life (§5.4) → FAILED
   `STOPPED_AFTER_PARTIAL_COMMIT`.
3. `lostOwners ≥ MAX_LOST_OWNERS` → FAILED `OWNER_LOST_REPEATEDLY`.
4. **Deadline**, when `timeoutMs` is set: `deadline = scheduledTime +
   timeoutMs`, on the owner's clock. The claimed record decides which rule
   applies.
   - `attempts == 0 && lostOwners == 0`: late when `now > deadline`. A late
     task is expired: removed, with `SCHEDULED_TRANSITION_EXPIRE`.
   - Otherwise: late when `now > deadline + RETRY_DELAY`. A late task is
     FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`.
5. Otherwise the owner runs it.

A pnode crash counts as a lost owner. So a task that was running on a crashed
pnode, and has a short `timeoutMs`, ends FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`
rather than expired. This is certain when `timeoutMs + RETRY_DELAY <
STALE_AFTER − HEARTBEAT_INTERVAL`.

### 5.2 The run's transactions

`FireScheduledTransition(ctx, task, claim)` keeps its structure
(`internal/domain/workflow/fire_scheduled.go:89-524`), with these rules.

- **Re-read.** Every segment first re-reads the task with `Get(tenant, id)`.
  That is the first transaction and each segment after a
  `COMMIT_BEFORE_DISPATCH` commit. If the task is gone, or its arm token or
  claim token has changed, the run ends `superseded`.
- **Every commit writes the task row.**
  - The final transaction either removes the task (`RemoveLife(tenant, id,
    armToken)`) or re-arms it: a fired run re-arms through the re-arm step,
    and a self-loop re-arms the same id.
  - Every `COMMIT_BEFORE_DISPATCH` segment commit first calls
    `StampSegment(tenant, id, armToken, claimToken, partial)` as its last
    write (`flushAndCommitSegment`, `engine_processors.go:477-523`). The stamp
    is fenced. A refused stamp stops the segment from committing.
  - Task rows are under first-committer-wins (C1). So a commit whose task was
    reclaimed, re-armed or removed by another transaction since it began fails
    with `spi.ErrConflict`.
- **The run guard belongs to the transaction.** The run registers its guard
  (§5.3) against every transaction it begins: the first one, and the new
  transaction of each `COMMIT_BEFORE_DISPATCH` segment. The engine's segment
  commit (`flushAndCommitSegment`) finds the guard from the id of the
  transaction it commits, not from the context. So every commit of a run's
  transaction checks the run's cancellation and writes the fenced stamp,
  whichever call chain reaches it. The run removes the registrations when it
  ends.
  - The other commit sites need no lookup. The run's own commits in
    `fire_scheduled.go` are the run's by construction. The entity handler
    commits only a transaction that its own request began
    (`commitOwned`, `internal/domain/entity/handler.go:133-140`).
  - A compute-node callback that joined the run's transaction never reaches
    the segment commit: the engine refuses its `COMMIT_BEFORE_DISPATCH`
    processor first, with `409 COMMIT_IN_JOINED_TRANSACTION`
    (`engine_processors.go:312-314`). The stamp's `partial` flag is therefore
    always set by the run's own chain (§5.4).
- **`RemoveLife` removes only the life it names.** It does nothing if this same
  transaction has already replaced or removed the task.
- **A processor may write its own fired entity** through a joined callback and
  return no mutations (`help/workflows.md:185-192`). Before its final persist,
  the run re-reads the fired entity inside its transaction.
  - If this transaction deleted it: the run removes its life, skips the re-arm
    and the persist, and commits. The outcome is `cancelled`.
  - If this transaction last wrote it: the run persists with
    `CompareAndSave(entity, <this transaction's id>)`, so the engine's result
    is the last write, as in an ordinary transition.
  - A write by another transaction is not visible under the snapshot and still
    fails at commit.
- **Order at the end.** The run removes or re-arms its own task first, then runs
  the re-arm step for the final state (`fire_scheduled.go:470-489`).
- **Deciding "superseded".** If a run's transaction fails with `ErrStaleClaim`
  or `ErrConflict`, the run re-reads its task with a read that does not join
  the transaction.
  - Life or claim changed → `superseded`, and nothing is recorded.
  - Otherwise → an ordinary failure, handled by §5.6.
  - A failed re-read is retried as §5.6 retries.
- **Removed from the fire path**, because the claim covers them:
  - the pre-transaction origin read (the claimed record carries `ArmedBy`);
  - the `ArmedBy` verify-or-abort (`:215-217`);
  - the re-armed-into-the-future guard (`:250-255`);
  - the tenant-mismatch guard (`:195-203`);
  - the grace band (`:50-63, 276-295`).

**Why this fences the data without a claim check inside the entity
transaction.** Three things together:
- the re-read at the start of each segment;
- the task-row write in every commit (C1);
- the task-row lock that a commit holds until it lands (C6, §6.3).

The mark (§5.5) is what protects A2.

### 5.3 Context and cancellation

- **The run context** derives from the scheduler's run context. The system
  identity is attached to it, as `common.SystemUserContext` builds it.
- **The run guard** travels on that context, and is registered against each
  of the run's transactions (§5.2). It carries:
  - tenant, task id, arm token and claim token;
  - the store;
  - the run's cancellation;
  - a flag saying the fired transition has changed the state (for
    `PartialCommit`).
- **When the run is cancelled.** It is cancelled when the pnode cancels itself
  (§6.3), when a panic latch fires (§6.5), or at shutdown step 3 (§6.4). A run
  with an unsafe callout in flight at step 3 is not cancelled; §6.4 bounds it.
- **Shutdown signals on the guard.** The guard also carries a "no new unsafe
  dispatch" signal, closed at shutdown step 1, and a record of each unsafe
  callout in flight and when the oldest one started.
- **Callouts see the cancellation.** Every callout of a guarded run carries it:
  - processors, in every segment and in both `COMMIT_BEFORE_DISPATCH` branches
    (`engine_processors.go:368, 400`);
  - criteria;
  - the re-arm step's `schedule.function` callouts.

  Cancellation cuts a callout that is in flight
  (`internal/grpc/dispatch.go:270-277`). Transactions still begin with
  `WithoutCancel` (`:411, 538`).
- **Checkpoints.** The engine checks the run's cancellation from the guard at
  three points: before each processor dispatch, before each cascade step, and
  before each entity-transaction commit. It does not commit a cancelled run.
  The bookkeeping writes of §5.6 and §6.4 are exempt from this check.
- **Commits are shielded.** Every entity-transaction commit uses
  `common.ShieldedCommitWithBudget`. That covers each segment commit and every
  `Commit` in `fire_scheduled.go` (`:173, 232, 246, 288, 337, 418, 440, 523`).

### 5.4 Partial commit

A `COMMIT_BEFORE_DISPATCH` processor in a cascade step commits the entity in
that step's state (`engine.go:848`, `engine_processors.go:477-523`). The stamp
of that segment sets `PartialCommit`, and so does the stamp of every later
segment. This covers a cascade that loops back into the source state. A segment
of the fired transition itself, with the entity still in the source state, does
not set it.

If the run then stops, the task ends FAILED `STOPPED_AFTER_PARTIAL_COMMIT`:
- a live run records this itself;
- after a crash, the next claim records it (§5.1, step 2).

A run that fails after a segment of the fired transition itself follows the
ordinary rules. A safe failure is retried from that committed state.

### 5.5 The unsafe mark

This applies at every dispatch site of an unsafe processor under a run guard
(`engine_processors.go:230, 269, 368, 400`).

**Before every unsafe dispatch** the engine calls `MarkUnsafe(tenant, id,
armToken, claimToken)`, even when this run already holds a mark. For the same
claim the call is idempotent. It is also the check that stops a superseded run
from sending more unsafe work.

| Result | Engine |
|---|---|
| accepted | dispatch |
| `ErrStaleClaim` | do not dispatch; the run is `superseded` |
| `ErrMarkedByAnotherClaim` | do not dispatch; FAILED `UNSAFE_WORK_NOT_COMPLETED` |
| `ErrTaskBusy` | do not dispatch; safe failure |
| any other error | do not dispatch; safe failure recorded with `RecordAttempt{ClearOwnMark}` |

**"Unsafe work reached a compute node"** is one fact that the run keeps in
memory.
- It is set before every unsafe dispatch.
- It is reset only when that dispatch returns the **`NotHandedOff` proof**, and
  only if the fact was false before that dispatch.

So a panic, a success, or an error without the proof leaves the fact set.

**`NotHandedOff`** is an error value. Only the callout coordinator attaches it,
and only when both of these hold:
- no try had `member.Send` return nil;
- no hand-over to a peer got past `StageNotConnected`
  (`peer_router.go:154-167`), unless the peer's authenticated answer was
  `no_handoff` (`a.Failure.Kind == NoHandOff`, `handover.go:389-391`).

A `no_handoff` answer counts as proof only for a callout that is not
repeat-safe. For such a callout the peer stops on any failure other than
`NoHandOff` (`run_local.go:147`, `handover.go:115, 245`). The coordinator's
exits before any try also carry the proof: `ResolveAnswerLimit` and the
criterion parse failure (`internal/callout/entry.go:30-32`).

For this, the callout layer needs these changes:
- `LocalResult` gains a `HandedOff` bit. Every return path after `Send`
  returned nil sets it: the answer branches from `dispatch.go:207` on, and the
  cancellation at `:270-280`. The branch at `:189-205` is a failed `Send`,
  before the hand-off, and does not set it. `run_local.go:137-140` carries the
  bit.
- The coordinator keeps a sticky flag across all passes. It records each
  hand-over's outcome before the early return at `coordinator.go:279`. It
  attaches `NotHandedOff` whenever the flag is false, including on its exits
  before any try (`ResolveAnswerLimit`, `:120-123`).
- A cancellation error still satisfies `errors.Is(err, context.Canceled)`.

**Absence of the proof counts as handed off.** The engine looks for the proof
with `errors.As`. Every other error is fail-closed. That includes:
- a savepoint error that replaces the dispatch error
  (`engine_processors.go:279-285`);
- a failure after a successful dispatch in the same step (`:239, :411, :419,
  :429`);
- `internal/testing/localproc`.

**Where the mark lives.** The mark belongs to the life, and every later claim of
that life sees it. The run removes it only through
`RecordAttempt{ClearOwnMark}` (§5.6). If the pnode dies first, the mark stays.

**Coverage of the mark:**
- A #254 hand-over to a peer is covered, because the owner marks before it
  dispatches.
- Callbacks inside an unsafe processor are covered by its mark.
- Callbacks inside an `idempotent` processor are covered by its declaration.
- `ASYNC_NEW_TX` processors are marked the same way. If the run commits, the
  task is completed.

**The callback anti-pattern.** A joined callback that wrote the fired entity
holds the task row in the run's transaction.
- A later `MarkUnsafe` then gets `ErrTaskBusy`, which is a safe failure.
- In a segmented run, the next stamp refuses and the segment rolls back. The
  non-joining re-read (§5.2) then classifies the result.

In both cases the run never hangs, never repeats unsafe work, and never loops
without the attempt being counted.

### 5.6 Recording the outcome

A run whose final transaction did not commit always records its outcome with a
fenced write. The write is issued only after the run's open segment has been
rolled back, so it never waits on the run's own row lock.
- **Its context.** The write runs on `context.WithoutCancel(runCtx)`, with a
  10 s timeout per attempt. It never inherits the run's cancellation.
- **Errors are retried by default**: after 1 s, then doubling, up to
  `HEARTBEAT_INTERVAL`, until the write is accepted or refused, or until the
  shutdown deadline of §6.4. This covers every infrastructure condition:
  outage, failover, timeout, pool exhaustion, conflict. Each failed attempt is
  logged at WARN, at most once a minute per task.
- **A refusal** (`ErrStaleClaim`) means the run was superseded.
- **The node latches** (§6.5) only on a deterministic rejection by the store:
  an error that satisfies `errors.Is(err, spi.ErrStoreRejected)`. This is a new
  SPI marker. Every store sets it, and a conformance case covers it. PostgreSQL
  sets it for SQLSTATE classes 22, 23 and 42.
  - The latch is logged at ERROR with a ticket.
  - The task stays RUNNING under the latched owner, visible in the query, the
    metrics and `/readyz`.
  - Every other error is retried without limit, as above.
  - Retrying can take a long time under lock waits or pool exhaustion. That
    shows in the WARN lines and in `cyoda.scheduler.bookkeeping.retries`.

The first matching row applies:

| The run | Bookkeeping |
|---|---|
| panicked | `Fail(RUN_PANICKED)` |
| `PartialCommit` set by this run | `Fail(STOPPED_AFTER_PARTIAL_COMMIT)` |
| holds a mark; unsafe work reached a compute node | `Fail(UNSAFE_WORK_NOT_COMPLETED)` |
| cut by the shutdown drain; no unsafe work reached a compute node | `RecordAttempt{NotCounted, NextAttemptTime: now, ClearOwnMark}` |
| holds a mark, or its last `MarkUnsafe` failed with an error other than a refusal; no unsafe work reached a compute node | `RecordAttempt{Error, ClearOwnMark}` |
| any other failure | `RecordAttempt{Error, NextAttemptTime}` |

`RecordAttempt`:
- sets the task to WAITING;
- adds 1 to `attempts`, unless `NotCounted`;
- records the error (§5.8);
- clears the claim;
- with `ClearOwnMark`, removes this claim's mark in the same atomic write.

```
delay = RETRY_DELAY × 2^(attempts−1), saturating at RETRY_DELAY_MAX
next  = now + delay;  with timeoutMs: next = min(next, deadline)
```

If the deadline has already passed when a **counted** attempt is recorded, the
owner calls `Fail(EXPIRED_AFTER_FAILED_ATTEMPTS)` instead. A `NotCounted`
attempt is always recorded as such, and the next claim decides with §5.1. So a
first attempt cut by a deploy after its deadline is expired, not FAILED. The WARN log line is
written after `RecordAttempt` is accepted. A criterion that evaluates to false
declines the task; that is not a failure.

### 5.7 FAILED

`Fail` and the `SCHEDULED_TRANSITION_FAIL` audit event are written in one
transaction.
- The audit data is `{transition, sourceState, reason, attempts, lostOwners}`.
- The commit is identified by (task id, arm token). That is where the planned
  notification feature publishes "timer failed".

### 5.8 The recorded error (Gate 3)

`lastError` is visible to tenant users, so it passes an allow-list:

The first matching row applies:

| Error | Recorded as |
|---|---|
| a cancellation of the run (shutdown, self-cancel), even when wrapped in a `CalloutFailure` | `CANCELLED: the run was stopped by the scheduler` — logged at WARN, no ticket |
| `spi.ErrConflict` | `CONFLICT: a concurrent write changed the entity or its task` — logged at WARN, no ticket |
| a `*contract.CalloutFailure` | its `Message`: `CODE: detail` when the failure has a code, plain client-safe text when it has none (`internal/contract/callout.go:78-84`); for `MemberFailed`, the compute node's own message |
| a `*common.AppError` of the Operational level, any status | its `Message` (already `CODE: detail`) |
| anything else | `internal error [ticket: <uuid>]`, with the full error logged at ERROR |

`classifyWorkflowError` is not used for this. Its catch-all carries
`err.Error()` in a 400 (`internal/domain/entity/service.go:2840`), and the fire
path wraps store errors in plain `fmt.Errorf` (`fire_scheduled.go:107, 118,
168, 234`).

The text is sanitised before it is stored: invalid UTF-8 and NUL characters are
replaced, and it is cut at a character boundary to at most 1 024 bytes. Every
backend stores the same text.

## 6. The scheduler service

`internal/scheduler` is one claim loop per pnode, with a heartbeat goroutine and
a watchdog.

### 6.1 Claiming

**When the loop may claim at all.** Only after the first `Heartbeat` has
succeeded, and never while heartbeats are failing (§6.3) or while the node is
latched (§6.5).

**Lost-owner claims** (`AllowLostOwner`) take a task from a stale owner. The
loop makes them only after its own heartbeats have succeeded without a gap for
at least `STALE_AFTER`. Otherwise, after a database outage, the first pnode
back would claim every other pnode's tasks as lost.

**When it claims.** Every `CYODA_SCHEDULER_SCAN_INTERVAL` (1 s). It also claims
at once when a run slot frees and the previous claim filled every slot.

```
ClaimDue(ClaimRequest{Owner, NowMs, StaleAfter, Limit,
                      TenantInProgress map[TenantID]int, PerTenantLimit, AllowLostOwner})
```

**Limits:**
- `Limit` = `CYODA_SCHEDULER_MAX_RUNS` (8) minus the runs in progress. With
  nothing free, the loop does not call.
- `PerTenantLimit` = `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (4) runs per tenant
  on this pnode.
- Tenants take turns. Within a tenant, tasks are claimed in `nextAttemptTime`
  order.

**Claimable tasks:**
- WAITING, with `nextAttemptTime ≤ NowMs` on the pnode clock. That is the
  clock that set `scheduledTime` at arm.
- With `AllowLostOwner`: RUNNING, where the owner's liveness record is missing
  or older than `StaleAfter` by the store clock. The claim adds 1 to
  `lostOwners`.

**One task per entity.** A task is not claimed while another task of the same
entity is RUNNING. Only one task per entity is claimed per call. The store
enforces this against concurrent callers too (§10.2).

A claimed task becomes RUNNING, with this owner and a new claim token drawn by
the store.

**Registration.** The loop adds each returned task to its set of live runs
before it does anything else, including the next `GiveBackIdle`. A task leaves
the set only after its outcome is accepted or refused, or at shutdown step 5
if its run has ended without that. Only the loop goroutine
calls `ClaimDue` and `GiveBackIdle`. If a claim's reply is lost, the tasks it
claimed are not in the set, so the next `GiveBackIdle` returns them.

**Self-heal.** Every tick calls `GiveBackIdle(owner, keep = live claim tokens)`.
A task this incarnation holds RUNNING without a live run goes back to WAITING,
uncounted.

### 6.2 Liveness

- **Heartbeat.** A dedicated goroutine calls `Heartbeat(incarnation)` every
  `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (15 s), from start until shutdown step
  5. `Heartbeat` is an upsert, so a pnode whose record was swept during a long
  outage recreates it. The store stamps it with the store clock. Each call has
  a budget of `min(10 s, HEARTBEAT_INTERVAL)`, covering both the connection
  acquire and the statement. So heartbeats start at a fixed interval.
- **Stale after.** `CYODA_SCHEDULER_STALE_AFTER` defaults to 2 min and is
  validated in §6.3.
  - It must be the same on every pnode of a cluster: a reclaimer's value
    meets the owner's watchdog.
  - During a rolling change of it, fencing still holds, but the watchdog
    margin is not guaranteed.
- **Hung runs.** A pnode that keeps heartbeating keeps its tasks, even if a run
  is hung. Liveness is not progress, as in #509.
- **Cleanup.** The claim loop removes the liveness record of a dead incarnation
  after 10 × `STALE_AFTER`, once no task references it.

### 6.3 Watchdog and self-cancel

**The watchdog** is a goroutine separate from the heartbeat.
- Before each heartbeat call acquires its connection, it records the moment on
  the monotonic clock. The store's stamp is never earlier than that moment.
- After a success, it arms a timer for `recorded moment + W`, where:
  ```
  W = STALE_AFTER − CommitBudget − 10 s
  ```
  - `CommitBudget` is 30 s (`internal/common/rollback.go:36`);
  - the 10 s is slack for clock rate and scheduling.

**When the timer fires**, the pnode cancels every run in progress (outcome
`self_cancelled`) and makes no claim until a heartbeat succeeds.

**Why no other pnode can reclaim while this owner still commits:**
- after the timer fires, no new commit of the run starts, because every commit
  checks the cancellation (§5.3);
- a commit already under way holds its task-row lock until it lands (§5.2);
- a claim skips a locked row (C6).

`CommitBudget` inside `W` gives an in-flight commit the time to land normally,
before the store could consider the owner stale.

**Validation:** `STALE_AFTER ≥ CommitBudget + 10 s slack + 10 s heartbeat
budget + 3 × HEARTBEAT_INTERVAL`. The heartbeat interval is measured start to
start. With this, `W` exceeds two intervals plus one heartbeat's budget, with
a full interval to spare. So a single failed or slow heartbeat never
self-cancels a pnode.

A frozen VM, whose monotonic clock stops, is not caught by the watchdog. For
unsafe work, the mark covers it (C3). Its commits are fenced (C1, C6).

### 6.4 Shutdown

On a signal, the scheduler runs these steps **before** the servers drain
(`cmd/cyoda/run.go:89-195`):

1. Stop claiming.
2. Wait up to `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (20 s) for the runs in progress.
   The compute-node streams and callback routes are still open.
3. Cancel every remaining run **except** one whose unsafe processor has been
   handed off and is still in flight. Cutting that run would turn a routine
   deploy into a FAILED task.
   - Its callout may finish, or reach its own deadline
     (`internal/callout/coordinator.go:148-151`).
   - After that, the run may go on with safe dispatches (criteria, functions,
     `idempotent` processors), cascade steps and commits, until the step-4
     bound.
   - It dispatches no **new** unsafe processor: reaching one counts as cut
     (§5.3).
   - From step 1 on, no run starts a new unsafe dispatch. Reaching one counts
     as cut.
4. Wait for every run to record its outcome (§5.6), up to: the longest
   remaining callout deadline + `CommitBudget` + 15 s.
5. As the loop's last act, `GiveBackIdle(owner, keep = live claim tokens)` hands
   back the claims whose run ended without a recorded outcome. A run still live
   is not given back: its task is reclaimed after `STALE_AFTER`, as a lost
   owner. The heartbeat goroutine then stops and has fully exited before
   `RetireOwner` runs, and `RetireOwner` runs only if no run is live.
6. The server drains start.

When a server fails, the same sequence runs from `a.Shutdown()` after the
servers stop.

**Grace period.**
- A pnode with no unsafe callout in flight exits within about 20 + 30 + 15 s,
  plus the existing tail of about 25 s (`help/run.md:287`).
- An unsafe callout can only be in flight if it started before step 1, because
  no new unsafe dispatch starts after the signal. A pnode with one in flight
  can take up to:

  ```
  callout deadline + CommitBudget + 15 s + 25 s
  callout deadline = (1 + CYODA_RETRY_FIXED_NUM_RETRIES) × answer limit
                     + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE
  ```

  At the defaults that is 155 + 70 = 225 s. At the maximum answer limit
  (`CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`) it is 275 + 70 = 345 s.
- The Helm chart sets `terminationGracePeriodSeconds: 360`. `help/run.md` and
  the chart README give the formula, so an operator who raises those settings
  can raise the grace period too. The step-2 drain overlaps the callout
  deadline and adds nothing to the bound.

### 6.5 Panics

Every new goroutine recovers panics and latches the node unhealthy. This is the
existing permanent latch (`docs/ARCHITECTURE.md:382-393`).

- **A panicking run** records `Fail(RUN_PANICKED)` with a ticket and is not
  retried. Its state is unverified, and running it again on another pnode
  would spread the problem.
  - A panicked run is never given back.
  - If its `Fail` has not landed when the process exits, its task is
    reclaimed after `STALE_AFTER` as a lost owner, and `MAX_LOST_OWNERS`
    bounds any repeat.
- **A latched pnode** stops claiming. Unless the heartbeat itself panicked, it
  keeps heartbeating, so its runs still in progress are not taken over.
- **A panic in the loop, the heartbeat or the watchdog** latches the node, and
  the recovery itself cancels every run in progress. Its tasks are then
  reclaimed by other pnodes after `STALE_AFTER` if the heartbeat has stopped.

### 6.6 Removed

- `internal/scheduler/coordinator.go` and `distribution.go`, and
  `ClusterExecutor`.
- `internal/cluster/scheduler_rpc.go`, with the
  `/internal/dispatch/scheduled-task` route.
- `DispatchForwardTimeout` (`internal/cluster/config.go:25-27`).
- `Config.RedispatchBackoff` and `BatchSize`.
- Their wiring in `app/app.go:586, 600-657, 810-827`, and their tests,
  including `app/config_dispatch_test.go` and the forward-timeout rows of
  `app/config_registry_binding_test.go`.

## 7. Entity writes, workflow imports and tasks

- **Arm.** `reconcileScheduledTasks` → `ReconcileForEntity`. Every armed task
  starts a new life, whatever the status it replaces:
  - WAITING, with `nextAttemptTime = scheduledTime`;
  - attempts and lost owners 0, errors cleared, `PartialCommit` false;
  - a new arm token, and no claim.
- **Cancel.** Reconcile removes every task of the entity that is not in the new
  arm set, and records `SCHEDULED_TRANSITION_CANCEL`. The firing task is
  excluded from that audit.
- **When reconcile does nothing.** It returns early only when no workflow of the
  entity's model has a scheduled transition (`arm.go:96-98`). That flag is
  computed when workflows are loaded (V3).
- **Workflow import** first saves the workflows (`internal/domain/workflow/handler.go:369`,
  outside any transaction). Then, in its own transaction, it removes the
  model's tasks whose (source state, transition) is not scheduled in any of the
  model's workflows: `DeleteForModel(tenant, name, version, keep)`.
  - Saving first means a failed removal never loses a timer that is still
    scheduled.
  - A removal that conflicts is retried up to 3 times. It is idempotent.
  - After that, the import answers `409` (retryable). A retried import saves
    the same workflows and retries the removal.
- **Entity delete** removes the entity's tasks in the same transaction, on every
  path, and records no audit event:

  | Path | Removal |
  |---|---|
  | `Handler.DeleteEntity` (`internal/domain/entity/service.go:643`) | `DeleteForEntities(tenant, [id])` |
  | `DeleteEntitiesConditional` (`:1192`), single-transaction loop (`:1301`) and `deleteBatched` (`:1427`) | `DeleteForEntities` with the ids actually deleted |
  | `DeleteAllEntities` (`:765`, also reached from `:1217`) | `DeleteForModel(tenant, model, version, keep = none)` |

  **Server-side retry on a conflict.** On an owned path the store cannot tell a
  task-row conflict from a conflict on the entity itself, so the retry covers
  any `spi.ErrConflict`. A delete that races an ordinary update succeeds on
  retry, against the entity as it now is.

  | Delete path | Retry |
  |---|---|
  | single delete, delete-all fast path and the conditional single-transaction loop, when the handler owns the transaction | the whole call, up to 3 times; the rollback makes the re-run idempotent, and no audit event is duplicated. A conflict that persists → 409 |
  | `deleteBatched` | each batch (`deleteOneBatch`), up to 3 times, against its re-checked version baseline. A conflict that persists goes into that batch's `IDToError`, as batch conflicts do today (`service.go:1690-1716`); it never becomes 409 |
  | any path that joined a transaction already on the context (`beginScope` not owned, `service.go:645, 772, 1230`) | none; the conflict surfaces at the outer transaction's commit |

  The gRPC doors reach the same functions (`internal/grpc/entity.go:200, 501`).
- **Client writes carry no claim.** If a client write commits first, the
  owner's next task-row write fails (C1), and the owner is superseded.
- **A client write can get a retryable 409 when it races the scheduler.** C1
  works both ways. A client write conflicts if the scheduler changed one of
  its task rows after the write began. An update then fails with `409`
  (retryable). A delete or an import retries on the server first (above), and
  fails with 409 only if the conflict persists. The scheduler changes a task row when it claims, stamps a segment,
  records an attempt, fails a task, or gives one back. This is the same 409 a
  client gets when it races the timer firing. It is documented and tested on
  every door (§13). The OpenAPI spec declares it wherever it is not already
  declared: `deleteSingleEntity` and `importEntityModelWorkflow`.
  `help/errors/CONFLICT.md` is widened to cover a race with the scheduler as
  well as a concurrent entity change.

## 8. `GET /scheduled-tasks`

**HTTP only.** It is an operator's view that no compute node needs, like the
audit trail. The gRPC waiver is in §13.

**Access.** Any authenticated user of the tenant, as for the audit trail. The
tenant comes from the token.

| Parameter | Type | Rule |
|---|---|---|
| `status` | repeatable: `WAITING`, `RUNNING`, `FAILED` | unknown value → 400 |
| `modelName` | string, 1–256, valid UTF-8, no NUL | otherwise → 400 |
| `modelVersion` | integer ≥ 1 | only together with `modelName` |
| `entityId` | UUID | |
| `cursor` | opaque, ≤ 256 characters | invalid → 400; never echoed |
| `limit` | integer 1–1000, default 20 | out of range → 400 (not clamped) |

**Order.** Results are sorted by `(scheduledTime, taskId)`, ascending. The
cursor is versioned base64url JSON, decoded strictly like the audit cursor
(`internal/domain/audit/cursor.go:65-114`).

**Response 200:**
```
{ "items": [ScheduledTaskDto], "pagination": { "hasNext", "nextCursor" } }
```

`ScheduledTaskDto` (typed but open, ADR 0003). Node ids and tokens are never
returned.

| Field | Type | Present |
|---|---|---|
| `taskId` | string (a hash) | always |
| `entityId` | string, uuid | always |
| `modelName`, `modelVersion` | string, integer | always |
| `sourceState`, `transition` | string | always |
| `status` | string (open) | always |
| `scheduledTime`, `armedTime` | date-time | always |
| `expiresTime` | date-time | when `timeoutMs` is set |
| `attempts`, `lostOwners` | integer | always |
| `nextAttemptTime` | date-time | WAITING |
| `lastAttemptTime`, `lastError` | date-time, string | after a failed attempt |
| `failureReason`, `failedTime` | string (open), date-time | FAILED |
| `armedBy` | `{id, kind}` | when known |

**Errors:**

| Status | Code | When |
|---|---|---|
| 200 | — | success, including an empty list |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName`; `modelVersion` not an integer ≥ 1; `entityId` not a UUID; `modelName` empty, too long, invalid UTF-8 or with NUL; `limit` not an integer or outside 1–1000; invalid `cursor` |
| 401 | `UNAUTHORIZED` | no token, or an invalid one |
| 500 | `SERVER_ERROR` | internal failure; generic message and a ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no 403, because no role is required. There is no 404: an unknown
model or entity, or another tenant's, returns an empty list.

### 8.1 Changed error cells on existing endpoints

| Endpoint (HTTP) | Status | Code | When | Declared today |
|---|---|---|---|---|
| `deleteSingleEntity` | 409 | `CONFLICT` (retryable) | a task-row conflict persists after 3 server retries | no — added |
| `deleteEntities` (conditional loop and delete-all fast path) | 409 | `CONFLICT` (retryable) | same | yes |
| `deleteEntities` (batched) | 200 | — | a persistent conflict is reported per id in the batch result, not as 409 | — |
| `updateSingle`, `updateSingleWithLoopback`, `updateCollection` | 409 | `CONFLICT` (retryable) | same, for a task row the update re-arms or cancels | yes |
| `importEntityModelWorkflow` | 409 | `CONFLICT` (retryable) | the task removal still conflicts after 3 retries (§7) | no — added |

The gRPC entity doors (`internal/grpc/entity.go`) return the same condition as
`CLIENT_ERROR` with `CONFLICT` in the message and `retryable`, as they already
do for an entity conflict.

## 9. Telemetry and logs

| Instrument | Type | Attributes |
|---|---|---|
| `cyoda.scheduler.runs` | counter | `outcome`: fired, declined, expired, cancelled, attempt_failed, failed, superseded, self_cancelled, shutdown_cancelled, panicked |
| `cyoda.scheduler.run.duration` | histogram, s | `outcome` |
| `cyoda.scheduler.runs.in_progress` | up-down counter | — |
| `cyoda.scheduler.claims` | counter | `reason`: due, owner_lost |
| `cyoda.scheduler.heartbeat.failures` | counter | — |
| `cyoda.scheduler.bookkeeping.retries` | counter | — |

- **Instruments** come from `observability.Meter()` and carry no tenant
  attribute.
- **Span:** `scheduler.run`, with the outcome.
- **Logs:**
  - INFO `scheduler started` with the pnode's `incarnation`, once at start;
  - WARN on each safe failure;
  - ERROR on FAILED, with the reason and a ticket;
  - WARN on self-cancel;
  - ERROR on panic.

## 10. Storage contract (SPI)

### 10.1 `ScheduledTaskStore`

```go
type ScheduledTaskStatus string        // WAITING | RUNNING | FAILED
type ScheduledTaskFailureReason string // UNSAFE_WORK_NOT_COMPLETED | OWNER_LOST_REPEATEDLY |
                                       // EXPIRED_AFTER_FAILED_ATTEMPTS | RUN_PANICKED | STOPPED_AFTER_PARTIAL_COMMIT
type ScheduledTask struct {
    // ID, TenantID, Type, ScheduledTime, TimeoutMs, EntityID, ModelName,
    // ModelVersion, Transition, SourceState, ArmedAt, ArmedBy
    Status               ScheduledTaskStatus
    ArmToken             uuid.UUID  // drawn by the store on every arm
    NextAttemptTime      int64      // unix ms; = ScheduledTime on arm
    Attempts, LostOwners int
    LastAttemptTime      *int64
    LastError            string
    FailureReason        ScheduledTaskFailureReason
    FailedTime           *int64
    PartialCommit        bool
    Claim                *TaskClaim // RUNNING only: {Token, Owner uuid.UUID}
    UnsafeMarked         bool       // read-only: a mark exists for this life
    ClaimedFromLostOwner bool       // read-only, ClaimDue results only: taken from a stale or missing owner
}
```

- **Removed:** `RedispatchAfter`, `AttemptCount`, `Upsert`, `ScanDue`,
  `MarkRedispatch`, `Delete`.
- **Fenced** means the call is accepted only if the task's current arm token and
  claim token are the ones given. Otherwise, or when the task is missing, the
  result is `spi.ErrStaleClaim`.
- **Never joins** means the method ignores any transaction on `ctx` and commits
  on its own. That is how a mark survives the run's rollback.

| Method | Transaction | Contract |
|---|---|---|
| `ReconcileForEntity(req)` | joins | arms `req.Arm`, each as a new life; removes every other task of the entity; returns the removed tasks |
| `RemoveLife(tenant, id, armToken)` | joins | removes the task if its current life is `armToken`; otherwise does nothing |
| `StampSegment(tenant, id, armToken, claimToken, partial)` | joins | fenced; writes the task row; sets `PartialCommit` when `partial` |
| `DeleteForEntities(tenant, ids)` | joins | removes those entities' tasks |
| `DeleteForModel(tenant, name, version, keep)` | joins | removes the model's tasks, except those whose (state, transition) `keep` retains |
| `Get(tenant, id)` | may join | tenant-scoped; sees the transaction's staged operations (C2) |
| `Query(tenant, filter, cursor, limit)` | never joins | tenant-scoped page (§8) |
| `ClaimDue(req)` | never joins | atomic; disjoint across callers; at most one RUNNING task per entity; cross-tenant (§6.1) |
| `Heartbeat`, `RetireOwner`, `SweepOwners` | never join | owner liveness, on the store clock |
| `GiveBackIdle(owner, keep)` | never joins | RUNNING under `owner` and not in `keep` → WAITING; not counted |
| `MarkUnsafe(tenant, id, armToken, claimToken)` | never joins | fenced; serialised with `ClaimDue` (C3); idempotent for the same claim; `ErrMarkedByAnotherClaim` if another claim of the life marked it; `ErrTaskBusy` (C6) |
| `RecordAttempt(tenant, id, armToken, claimToken, Attempt)` | never joins | fenced; WAITING; claim cleared; with `ClearOwnMark`, also removes this claim's mark |
| `Fail(tenant, id, armToken, claimToken, reason, error, atMs)` | joins the §5.7 transaction | fenced; FAILED; claim cleared |
| `SweepMarks()` | never joins | removes the marks of ended lives |

- **New errors:** `ErrMarkedByAnotherClaim`, `ErrTaskBusy`, and the marker `ErrStoreRejected` (a deterministic rejection by the store).
- `ErrStaleClaim`'s doc comment (`SPI/errors.go:155`) covers both stores.

**Clauses:**
- **C1. First-committer-wins covers task rows on every backend.** A transaction
  that writes a task row fails if another transaction committed a write to
  that row after this one began. The other transaction may be joining or not.
- **C2. A joining read sees its own staged writes.** A read by a joining call
  sees the operations staged earlier in the same transaction.
- **C3. A mark and a claim serialise.** When `MarkUnsafe` and `ClaimDue` race on
  one task, either the mark is refused or the claim returns `UnsafeMarked`.
- **C4. Heartbeats and claims have their own connections.** Entity transactions
  cannot starve them.
- **C5. Refusals are recognisable.**
  - A C1 refusal satisfies `errors.Is(err, spi.ErrConflict)` whether it is
    raised by a statement or by the commit. On PostgreSQL that is SQLSTATE
    40001 or 40P01.
  - A fenced refusal is `spi.ErrStaleClaim`.
- **C6. A task row written by an open transaction is not claimable** until that
  transaction ends. `MarkUnsafe` answers `ErrTaskBusy` for such a row.
- **C7. Audit events roll back with their transaction** on every backend. An
  event recorded in a transaction that rolls back is not kept.

**Further rules the conformance suite pins:**
- `RemoveLife` counts as a C1 write even when it removes nothing.
- `Query` orders ids byte-wise.
- In `ClaimDue`, tenants take turns: each tenant's first task comes before any
  tenant's second.
- A `Cancel` id is removed and not reported.
- `GiveBackIdle` leaves the task claimable at once.
- `Fail` leaves `LastAttemptTime` unchanged and always overwrites `LastError`.
- `RecordAttempt` may answer `ErrTaskBusy`, and the caller retries.
- A joining write whose tenant differs from the transaction's is refused.
- Error text with a NUL, invalid UTF-8 or more than 1 024 bytes is refused with
  `ErrStoreRejected`. On PostgreSQL a `CHECK` constraint enforces it.

**Conformance.** The cases live in `SPI/spitest`. They cover every method, every
refusal and every clause, including:
- a mark survives the rollback of the transaction on `ctx`;
- after a re-arm, every fenced write of the old life is refused;
- two due siblings produce one claim;
- a row written by an open transaction is not claimed.

A backend whose store answers "not implemented" skips them.

### 10.2 PostgreSQL

A new migration.

**Table `scheduled_tasks`:**
- **Drops:** `redispatch_after`, `attempt_count`.
- **Adds:** `arm_token`, `status`, `next_attempt_time`, `attempts`,
  `lost_owners`, `last_attempt_time`, `last_error`, `failure_reason`,
  `failed_time`, `partial_commit`, `claim_token`, `claim_owner`.
- **Indexes:**
  - `(next_attempt_time) WHERE status='WAITING'`
  - `(claim_owner) WHERE status='RUNNING'`
  - `UNIQUE (tenant_id, entity_id) WHERE status='RUNNING'`
  - `(tenant_id, scheduled_time, id)`
  - `(tenant_id, model_name, model_version)`
  - `(tenant_id, entity_id)`
- `scheduled_tasks_due_idx` is dropped.

**Other tables:**
- `scheduled_task_marks`: `(task_id, arm_token) PK, claim_token`.
- `scheduler_owners`: `(owner PK, heartbeat_at)`.

**Writes inside the entity transaction.** The task store writes task rows
straight into the open transaction, which runs at REPEATABLE READ. C1 and C6
follow from PostgreSQL itself. SQLSTATE 40001 and 40P01 map to `ErrConflict`
(`plugins/postgres/transaction_manager.go:189`,
`classifying_querier.go:20-21`). 40P01 can arise because a run segment writes
the entity before the task row, while a client write reconciles tasks before it
saves the entity (`engine.go:365`).

**Scheduler pool (C4).** `CYODA_POSTGRES_SCHEDULER_CONNS` (10, sized for `MAX_RUNS` 8 plus the claim loop and async search) connections,
using READ COMMITTED, with:
- `statement_timeout` 30 s;
- `idle_in_transaction_session_timeout` 10 s;
- `lock_timeout` 2 s;
- a 5 s acquire timeout.

What runs on it:
- every never-joining method except `Query`, which uses the main pool;
- the async-search heartbeat and claim (`plugins/postgres/search_store.go`).

`Heartbeat` has one extra connection of its own. The existing
`cyoda.storage.pool.connections` gauge gains a `pool` attribute (`main`,
`scheduler`, `heartbeat`). A `lock_not_available` (55P03)
error is retried by the caller with the §5.6 backoff. For `MarkUnsafe` it
means `ErrTaskBusy`.

**`ClaimDue`** is one transaction:
1. **Rank the candidates** in a subquery:
   - which rows qualify: WAITING and due; with `AllowLostOwner`, also RUNNING
     with a stale or missing owner;
   - exclude a row if its entity has another RUNNING task (`NOT EXISTS`);
   - one row per entity: `DISTINCT ON (tenant_id, entity_id)`;
   - per-tenant limits: `row_number() OVER (PARTITION BY tenant_id …)`.
2. **Lock them:** `SELECT … FOR UPDATE SKIP LOCKED` over the ranked ids,
   ordered by id. The `WHERE` repeats the row's own claim condition. The
   ranking must sit in a subquery, because `FOR UPDATE` cannot share a query
   level with a window function.
3. **Claim them:** `UPDATE … WHERE id = ANY($locked) AND <the full claim
   condition> RETURNING`. This is a new statement, so it sees claims that
   other pnodes committed after step 1. It is what closes that race.
4. **Read the marks** of the claimed rows, while the row locks are held (C3).

A unique violation or a 40P01 means another pnode claimed a sibling. The
transaction rolls back and claims nothing this tick. It is logged at DEBUG.

**`MarkUnsafe`** is one transaction:
1. `SELECT … FROM scheduled_tasks WHERE id, tenant_id, arm_token, claim_token
   FOR SHARE NOWAIT`:
   - no row → `ErrStaleClaim`;
   - 55P03 → `ErrTaskBusy`.
2. `INSERT … ON CONFLICT (task_id, arm_token) DO NOTHING RETURNING claim_token`:
   - the same claim token already there → accepted;
   - another claim token → `ErrMarkedByAnotherClaim`.

How this serialises with `ClaimDue` (C3):
- if a claim commits first, step 1 finds no row;
- if a claim is still in progress, step 1 gets 55P03;
- if the mark holds its share lock first, the claim skips the row, and the next
  scan sees the mark.

**`RecordAttempt`, `Fail`** are conditional statements on the tokens. Zero rows
affected → `ErrStaleClaim`. **`GiveBackIdle`** is a conditional statement on the
owner. Zero rows is its normal no-op.

**`DeleteForEntities`, `DeleteForModel`** are ordinary deletes inside the entity
transaction. A task row that another transaction changed after the snapshot
raises 40001, which maps to `ErrConflict`. The server-side retry of §7 then
applies.

**Tenant scoping.**
- Every tenant-facing method filters on `tenant_id`.
- `ClaimDue`, `GiveBackIdle`, the owner methods and the sweepers are
  cross-tenant, and no API reaches them.
- The tables stay outside row-level security. The migration comment at
  `000004_scheduled_tasks.up.sql:5-14` claims that every write carries a tenant
  predicate. It is corrected.

### 10.3 Memory and SQLite

Both run a single pnode, and both meet the same contract.

- **SQLite durability.** A new SQLite migration adds the task columns of §10.2
  and durable `scheduled_task_marks` and `scheduler_owners` tables. A mark
  survives a process restart, so a restart with a mark set ends FAILED. It is
  never re-run.

- **C1.**
  - Task-row keys join the write set and the commit's conflict check, next to
    entity ids (`plugins/memory/txmanager.go:488-513`,
    `plugins/sqlite/txmanager.go:517-535`).
  - A never-joining write that changes a task row appends a committed-log
    entry, under the commit gate.
  - The check orders by a sequence number. Memory already does
    (`plugins/memory/txmanager.go:499-503`). SQLite's submit-time comparison
    (`plugins/sqlite/txmanager.go:427-431, 525`) is replaced, because with a
    frozen clock it would make a run conflict with its own claim (V1).
- **C2.** A joining read overlays the transaction's staged operations on the
  committed state. The comment at `fire_scheduled.go:476-486` is rewritten to
  say this, and to state that PostgreSQL writes task rows at once.
- **C6.** Staged removals are expanded to task ids when they are staged. An
  open transaction's staged write to a task row makes that row unclaimable, and
  gives `ErrTaskBusy`.
- **Marks** are kept apart from task rows. They never append to the committed
  log.
- **C7.** Audit events are recorded inside the transaction and discarded on
  rollback.
- **SQLite's conflict check** orders by a sequence number for entities as well
  as task rows. With a frozen clock, a submit-time comparison gives false
  conflicts to entity writes too.
- **A write staged into a transaction that has already committed** is refused,
  as entity saves are.
- **Never-joining methods** apply at once, under the store's lock:
  - memory: `entityMu`;
  - SQLite: the writer connection, with the claim following
    `plugins/sqlite/search_store.go:404-496`.

  One task per entity, and C3, are checked under that lock.

## 11. Configuration (Gate 4)

| Variable | Default | Rule | Change |
|---|---|---|---|
| `CYODA_SCHEDULER_ENABLED` | true | | kept |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | 1s | > 0 | validation added |
| `CYODA_SCHEDULER_MAX_RUNS` | 8 | ≥ 1 | new |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | 4 | 1..MAX_RUNS | new |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | 15s | > 0 | new |
| `CYODA_SCHEDULER_STALE_AFTER` | 2m | ≥ 50 s + 3 × heartbeat (§6.3) | new |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | 3 | ≥ 1 | new |
| `CYODA_SCHEDULER_RETRY_DELAY` | 30s | > 0 | new |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | 15m | ≥ retry delay | new |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | 20s | ≥ 0 | new |
| `CYODA_POSTGRES_SCHEDULER_CONNS` | 10 | ≥ 2 | new (postgres plugin) |
| `CYODA_SCHEDULER_DISTRIBUTION`, `…_COORDINATOR`, `…_REDISPATCH_BACKOFF`, `…_BATCH_SIZE`, `…_EXPIRY_GRACE`, `CYODA_DISPATCH_FORWARD_TIMEOUT` | — | | removed |

Startup fails on an invalid value.

Where each change is made:
- `app/config.go`: `DefaultConfig` and `Validate`, at `:176, :181, :468,
  :474-482, :945-946`.
- `cmd/cyoda/help/config_registry.go`: `:80, :131-137`.
- Help topics: `help/config/scheduler.md`, `cluster.md`, `database.md`, and
  `help/config.md:33`.
- `README.md`: `:224-236, :251`.
- The postgres plugin: `plugins/postgres/config.go`, `plugin.go`, `doc.go`.
- `docs/plugins/POSTGRES.md`.

## 12. Documentation and other repositories

- **`help/workflows.md`, SCHEDULED TRANSITIONS:**
  - one owner per run;
  - retry until `timeoutMs`;
  - the FAILED reasons and `SCHEDULED_TRANSITION_FAIL`;
  - what entity writes, deletes and imports do to tasks;
  - one task per entity at a time;
  - the client 409;
  - the callback anti-pattern;
  - criteria and functions must not trigger unsafe work.
- **`idempotent`** (`help/workflows.md:182` and its `api/openapi.yaml`
  description): the declaration also governs scheduled runs.
  - No schema-version bump.
  - The rationale goes into `docs/workflow-schema-versioning.md` under "When
    NOT to bump".
- **New help topic `scheduled-tasks`**, shaped like `audit.md`, added to
  `topLevelTopicsV061`.
- **Other help topics:**
  - `help/run.md`, SHUTDOWN TIMING;
  - `help/telemetry.md`;
  - `help/helm.md`.
- **`api/openapi.yaml`:**
  - the new operation and its DTOs;
  - `SCHEDULED_TRANSITION_FAIL` in the audit enum (`:11742`);
  - a 409 response on `deleteSingleEntity` and `importEntityModelWorkflow`
    (§8.1);
  - then `go generate ./api`.
- **SPI:**
  - `SMEventScheduledTransitionFailed`;
  - the new errors;
  - the `ErrStaleClaim` doc comment;
  - remove the "consuming engines silently skip scheduled transitions"
    sentence from `TransitionSchedule`.
- **Helm chart:**
  - `terminationGracePeriodSeconds`, in `templates/statefulset.yaml`,
    `values.yaml`, `values.schema.json` and the chart `README.md`;
  - a chart `version:` bump.
- **`docs/cloud-parity/scheduled-transitions.md`** (Gate 7) is rewritten to
  this contract. A CaaS ticket is filed.
- **`docs/ARCHITECTURE.md`:** the scheduler passages, at `:94, :230, :382-393,
  :772, :940, :943, :1359, :1486-1506, :1550-1553, :1992, :2248`.
- **Code comment:** `internal/domain/search/reaper.go:18`.
- **`CHANGELOG.md` `### Breaking`:**
  - the removed variables;
  - processors not declared `idempotent` are not repeated;
  - the FAILED status;
  - one task per entity;
  - the client 409, including the new 409 cells of §8.1;
  - the query;
  - `terminationGracePeriodSeconds` 360 in the chart, and its formula.
- **`COMPATIBILITY.md`:** the SPI pin and the chart version.
- **`help/errors/CONFLICT.md`:** a 409 can also mean the request raced the
  scheduler.
- **SPI:** a PR into `main`, pseudo-pinned by cyoda-go.
- **cyoda-go-cassandra#68:** updated to this contract, including C6.

## 13. Coverage matrix

Layers:
- **U** — unit.
- **S** — `spitest`, on memory, SQLite and PostgreSQL.
- **E** — `internal/e2e`, over HTTP on PostgreSQL.
- **P** — a parity scenario in `e2e/parity/registry.go`.
- **M** — multi-node PostgreSQL. The fixture kills nodes
  (`e2e/parity/postgres/async_node_crash_test.go:48-61`).
- **1** — a single-node SQLite restart.

Rules:
- Concurrency cases stay out of **P**.
- Parity and multi-node fixtures set short `RETRY_DELAY`, `HEARTBEAT_INTERVAL`
  and `STALE_AFTER` in `e2e/parity/fixtureutil/tuned_env.go`.

### Endings

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| fired on time | ✓ | | ✓ | ✓ | |
| fired after one safe failure (compute node down, then up) | ✓ | | ✓ | ✓ | |
| self-loop fires and re-arms the same id as a new life | ✓ | ✓ | ✓ | ✓ | |
| declined | ✓ | | ✓ | ✓ | |
| expired, late on the first attempt | ✓ | | ✓ | ✓ | |
| criterion error → WAITING, attempts 1, error recorded | ✓ | | ✓ | ✓ | |
| no compute node → `NotHandedOff`, mark cleared, WAITING | ✓ | | ✓ | ✓ | |
| idempotent processor fails → WAITING | ✓ | | ✓ | ✓ | |
| retry delay doubles, saturates, clamps to the deadline; runs up to `RETRY_DELAY` past it; later → FAILED | ✓ | | | | |
| late after failed attempts → FAILED | ✓ | | ✓ | ✓ | |
| unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never re-run | ✓ | | ✓ | ✓ | |
| a later step fails after an unsafe hand-off → FAILED | ✓ | | ✓ | ✓ | |
| failure after a successful unsafe dispatch in the same step → FAILED | ✓ | | ✓ | | |
| savepoint error replaces the dispatch error → FAILED | ✓ | | | | |
| cancelled before `Send` returned nil → WAITING | ✓ | | | | |
| cancelled after `Send` returned nil → FAILED | ✓ | | | | |
| hand-over past `StageNotConnected` without `no_handoff` → FAILED | ✓ | | | | |
| database outage during `MarkUnsafe` → `RecordAttempt{ClearOwnMark}` after recovery, WAITING | ✓ | ✓ | | | |
| `RecordAttempt{ClearOwnMark}` retried in an outage; pnode dies first → FAILED at the next claim | ✓ | ✓ | | | |
| `ErrMarkedByAnotherClaim` → FAILED | ✓ | ✓ | | | |
| `ErrTaskBusy` → safe failure | ✓ | ✓ | | | |
| unsafe `ASYNC_NEW_TX` fails, the run commits → completed | ✓ | | ✓ | | |
| CBD on the fired transition, later failure, all idempotent → retried from the TX_pre state | ✓ | | ✓ | | |
| CBD in a cascade step, later failure → FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | ✓ | | ✓ | | |
| cascade looping back into the source state with a CBD step sets `PartialCommit` | ✓ | | ✓ | | |
| owner killed after a cascade-step commit → next claim FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | ✓ | ✓ | | | M |
| cancellation after TX_pre stops the run at the next step | ✓ | | ✓ | | |
| joined callback writes the fired entity, no unsafe processor follows → same outcome as an ordinary transition | ✓ | ✓ | ✓ | | |
| joined callback writes the fired entity, then an unsafe processor → `ErrTaskBusy`, safe failure, no hang (every backend) | ✓ | ✓ | ✓ | | |
| joined callback writes the fired entity in a segmented run → stamp refused, re-read classifies | ✓ | ✓ | ✓ | | |
| a segment commit reached on a context without the run guard is still stamped and cancellation-checked (guard found by transaction id) | ✓ | | | | |
| joined callback deletes the fired entity → the run commits | ✓ | ✓ | ✓ | | |
| `SCHEDULED_TRANSITION_FAIL` recorded with its reason | ✓ | | ✓ | ✓ | |
| panicking run → FAILED `RUN_PANICKED`, node latched, claims stop | ✓ | | | | |
| `lastError` of a non-sentinel store error is "internal error [ticket]" only | ✓ | | ✓ | | |
| `lastError` of a `MemberFailed` message, a callout timeout (`Code: Message`), an Operational `AppError`, `NO_COMPUTE_MEMBER_FOR_TAG` | ✓ | | ✓ | | |
| `lastError` over 1 024 bytes with multi-byte characters and a NUL → cut at a character boundary, stored on every backend | ✓ | ✓ | | | |
| a bookkeeping write retried through an outage; accepted after recovery | ✓ | ✓ | | | |
| `spi.ErrStoreRejected` → ERROR with ticket, node latched (every backend sets the marker) | ✓ | ✓ | | | |
| a bookkeeping write blocked by a lock or an exhausted pool is retried without latching | ✓ | | | | |
| bookkeeping after a self-cancel does not inherit the run's cancellation | ✓ | | | | |
| `lastError` for a cancelled run and for a conflict: fixed texts, WARN, no ticket | ✓ | | ✓ | | |
| fire-time CANCEL: transition no longer scheduled; entity with no transaction id | ✓ | | ✓ | ✓ | |

### Ownership, fencing and liveness

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| concurrent `ClaimDue` calls get disjoint sets | | ✓ | | | M |
| two due siblings: one claim per call | | ✓ | | | |
| two due siblings, two pnodes at once: one wins, the other claims nothing that tick | | ✓ | | | M |
| contended claim loop: a task claimed elsewhere between ranking and locking is never re-claimed | | ✓ | | | M |
| per-tenant limit and turn-taking | | ✓ | | | |
| a run longer than 3 × heartbeat interval is not claimed elsewhere | | | | | M |
| owner killed, no mark → claimed after `STALE_AFTER`, `lostOwners` 1, fires | | | | | M |
| owner killed with a mark → FAILED; the processor was sent once | | | | | M |
| owner killed, short `timeoutMs` → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | | | | | M |
| single-node SQLite restart reclaims its own RUNNING tasks as lost owners | | | | | 1 |
| single-node SQLite restart with a mark set → FAILED, never re-run | | | | | 1 |
| a liveness record swept during a long outage is recreated by the next heartbeat | | ✓ | | | M |
| a lost claim reply: the next `GiveBackIdle` returns the claimed tasks | ✓ | ✓ | | | |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | ✓ | ✓ | | | |
| database outage longer than `STALE_AFTER` → no lost-owner claims before a full stale period of healthy heartbeats | ✓ | | | | M |
| every fenced method refuses a stale token | | ✓ | | | |
| after a re-arm, every fenced write of the old life is refused | ✓ | ✓ | ✓ | | |
| a reclaimed or re-armed task makes the old run's commit fail (C1) | ✓ | ✓ | ✓ | | |
| a replaced owner's segment commit is refused by its stamp | ✓ | ✓ | ✓ | | |
| ABA: the old token is refused after a re-arm and a new claim | ✓ | ✓ | | | |
| `MarkUnsafe` racing `ClaimDue` (C3) | | ✓ | | | M |
| a mark survives the rollback of the transaction on `ctx` | | ✓ | | | |
| a joining read sees its own staged writes (C2) | | ✓ | | | |
| a row written by an open transaction is not claimable (C6) | | ✓ | | | |
| a superseded owner sends no unsafe processor | ✓ | | ✓ | | |
| a refusal from inside the run's own transaction goes through §5.6 | ✓ | ✓ | | | |
| a re-arm resets `PartialCommit` | ✓ | ✓ | | | |
| heartbeat failure → self-cancel at `W`; no claims until recovery | ✓ | | | | |
| no new commit after the watchdog fires; an in-flight commit blocks the reclaim until it lands | ✓ | | | | |
| a hung heartbeat does not stop the watchdog | ✓ | | | | |
| a watchdog panic: the latch cancels the runs | ✓ | | | | |
| no claim before the first heartbeat | ✓ | | | | |
| heartbeats are not starved with every main-pool connection busy (C4) | | | ✓ | | |
| async-search heartbeats and claims run on the scheduler pool and are not starved | | | ✓ | | |
| `STALE_AFTER` validation: a single slow or failed heartbeat never self-cancels | ✓ | | | | |
| a scheduler-pool statement blocked on a task-row lock gives up after `lock_timeout` | | ✓ | ✓ | | |
| SQLite: a run never conflicts with its own claim under a frozen clock | | ✓ | | | |
| a RUNNING task with no live run is given back; a live run never is | ✓ | ✓ | | | |
| at most `MAX_RUNS` runs; a freed slot triggers an immediate claim | ✓ | | ✓ | | |
| an empty cluster view has no effect | ✓ | | | | M |
| dead owners and the marks of ended lives are swept | | ✓ | | | |
| §9 instruments are emitted with their attributes | ✓ | | | | |

### Shutdown

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| runs finish within the drain; streams stay open | ✓ | | ✓ | | |
| run cut after the drain, nothing handed off → WAITING, uncounted, claimed at once elsewhere | ✓ | | | | M |
| an unsafe callout in flight at shutdown is not cut; the run continues with safe steps and commits | ✓ | | ✓ | | M |
| after the signal, a run reaching a new unsafe dispatch counts as cut | ✓ | | | | |
| a run with no unsafe callout in flight, cut after the drain, whose unsafe work was handed off earlier → FAILED | ✓ | | ✓ | | |
| a run still live after step 4 is not given back | ✓ | | | | |
| bookkeeping stops at its shutdown deadline | ✓ | | | | |
| `GiveBackIdle` is not counted; `RetireOwner` removes liveness | | ✓ | | | |

### Entity writes and workflow import

| Scenario | U | S | E | P | M/1 |
|---|---|---|---|---|---|
| FAILED task re-armed by an update in the state (new life, no mark) | ✓ | ✓ | ✓ | ✓ | |
| FAILED task cancelled when the entity leaves the state | ✓ | | ✓ | ✓ | |
| a task whose transition is no longer scheduled is removed at the next write | ✓ | ✓ | ✓ | ✓ | |
| a workflow import that drops schedules removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |
| deleting one entity removes its tasks | ✓ | ✓ | ✓ | ✓ | |
| conditional delete removes tasks: single-tx, batched, fast path | ✓ | ✓ | ✓ | ✓ | |
| delete-all removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |
| `DeleteForModel` in tenant A leaves tenant B's tasks | | ✓ | ✓ | | |
| an update racing a claim → retryable 409; a delete or import racing one claim succeeds after the server retry; same on every backend | | ✓ | ✓ (isolated) | | |
| an owned single-transaction delete that conflicts on a task row is retried up to 3 times; a persistent conflict → 409 | ✓ | | ✓ | | |
| batched delete: a conflicting batch retried up to 3 times; a persistent conflict reported per id, never 409 | ✓ | | ✓ | | |
| a delete in a joined transaction is not retried; the outer commit gets the conflict | ✓ | | ✓ | | |
| import: workflows saved before task removal; the removal retried; a persistent conflict → 409; re-import succeeds | ✓ | | ✓ | | |
| gRPC entity doors: tasks removed on delete; 409 on a race | ✓ (`internal/grpc`) | | | | |

### `GET /scheduled-tasks`

| Scenario | U | S | E | P |
|---|---|---|---|---|
| 200, no filter, several pages | ✓ | ✓ | ✓ | ✓ |
| 200, each filter: status (one and several), model name, name and version, entity | ✓ | ✓ | ✓ | ✓ |
| 200 empty: unknown model, unknown entity, another tenant's entity | | | ✓ | |
| 200, a FAILED item with its reason, error, times and attempts | | | ✓ | ✓ |
| 400 unknown `status` | ✓ | | ✓ | |
| 400 `modelVersion` without `modelName` | ✓ | | ✓ | |
| 400 `modelVersion` not an integer ≥ 1 | ✓ | | ✓ | |
| 400 `entityId` not a UUID | | | ✓ | |
| 400 `modelName` empty, too long, invalid UTF-8, or with NUL | ✓ | | ✓ | |
| 400 `limit` 0, 1001, or not an integer | ✓ | | ✓ | |
| 400 invalid cursor, value not echoed | ✓ | | ✓ | |
| 401 no token; 401 invalid token | | | ✓ | |
| 500 `SERVER_ERROR` with a ticket (store double) | ✓ | | ✓ | |
| 503 `STORAGE_UNAVAILABLE` (store double) | ✓ | | ✓ | |
| another tenant's tasks are never returned, under any filter | | ✓ | ✓ | ✓ |

**gRPC waiver.** The query has no gRPC door (§8).

### Configuration

| Scenario | U |
|---|---|
| each new variable: its default and its validation failure | ✓ |
| each removed variable is no longer read (§15) | ✓ |

## 14. Scope

- **Out of scope:**
  - an API to retry or dismiss a FAILED task — an entity write does both;
  - notifications — §5.7 names where they publish;
  - reclaiming a hung pnode that still heartbeats;
  - #600.

## 15. Exit checks

Both commands must print nothing when run from the repository root. They check
tracked files only. The same commands are run inside the SPI.

```
git grep -n -e RedispatchAfter -e RedispatchBackoff -e MarkRedispatch -e AttemptCount -e ScanDue \
  -e redispatch_after -e attempt_count -e LowestLiveNodeID -e 'scheduler\.RoundRobin' -e SchedulerRPC \
  -e ClusterExecutor -e dispatch/scheduled-task -e DispatchForwardTimeout \
  -e CYODA_SCHEDULER_DISTRIBUTION -e CYODA_SCHEDULER_COORDINATOR -e CYODA_SCHEDULER_REDISPATCH_BACKOFF \
  -e CYODA_SCHEDULER_BATCH_SIZE -e CYODA_SCHEDULER_EXPIRY_GRACE -e CYODA_DISPATCH_FORWARD_TIMEOUT \
  -e ExpiryGrace -e expiryGrace \
  -- . ':!*/migrations/*' ':!docs/plans' ':!docs/superpowers' ':!docs/release-notes' ':!CHANGELOG.md' ':!COMPATIBILITY.md'
git grep -n -e '\.Upsert(ctx, task' -e 'sts\.Delete(' -- '*.go'
```

Two exclusions:
- **Migrations.** Applied migrations are immutable and keep the old column
  names.
- **`RoundRobin`.** `internal/grpc/selector.go` has its own `RoundRobin`, which
  stays. So the check qualifies it with `scheduler.`.

## 16. Verification points (resolved in planning)

- **V1.** Memory and SQLite can add task-row keys and never-joining log entries
  to the conflict check without disturbing the entity check.
- **V2.** The existing scheduled tests are rewritten against this spec.
  - The dual-coordinator tests in `fire_scheduled_concurrency_test.go` become
    claim-race tests.
- **V3.** The engine can compute "some workflow of this model schedules a
  transition" without an extra store read per write.
- **V4.** `runServers` can run the scheduler drain before the server drains on
  the signal path.
- **V5.** In `ClaimDue`, the loser of a unique-index wait rolls back within
  `lock_timeout` and claims nothing.
- **V6.** The coordinator can attach `NotHandedOff` exactly as §5.5 defines it.
- **V7.** Every entity-transaction commit of a guarded run writes the task row
  before it commits, through the stamp or the final write.
