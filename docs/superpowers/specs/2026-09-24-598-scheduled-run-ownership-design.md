# #598 — every scheduled run has one owner

Status: spec v2, 2026-09-24, revised after independent spec review. Milestone
v0.9.0. Branch `feat/598-scheduler-ownership`.

Inputs:
- Research: `docs/superpowers/research/2026-09-24-598-scheduler-ownership-research.md` ("R§n").
- Design brief and its independent review: `docs/superpowers/research/2026-09-24-598-design-brief.md`.
- Product-owner rulings, marked **[ruling]**.

`SPI/` is `cyoda-go-spi`. Paths without a prefix are in this repository.

## 1. Summary

At any time, one pnode at most **claims** a scheduled transition's task, and
that pnode runs it. Every write the owner makes carries the claim's token. The
store refuses a token that is not current. Each pnode proves it is alive with
one heartbeat record. A task whose owner stopped heartbeating may be claimed by
another pnode.

The platform never repeats a processor that is not declared `idempotent`.
Before such a processor is dispatched, the owner writes a mark on the task. A
task that carries the mark and did not commit is **FAILED**, and is never run
again.

A failure where nothing unsafe was handed off is retried, with a growing delay,
until the transition's own `timeoutMs` passes **[ruling]**. A task whose owner
is lost repeatedly is FAILED after `CYODA_SCHEDULER_MAX_LOST_OWNERS` (3) losses
**[ruling]**.

A FAILED task never moves the entity **[ruling]**. It is kept, and is visible
through `GET /scheduled-tasks`, metrics, logs and an audit event on the entity.

Deleted: the scanning coordinator, the round-robin distribution, the scheduler
RPC, the redispatch throttle and the expiry grace band.

## 2. Acceptance (issue #598) and where each point is met

A task's **life** runs from one arm to the next (§3). The guarantees hold per
life. An entity write that re-arms a task starts a new life. From then on, the
old run can no longer commit or write a mark, but it is not stopped mid-callout.
If the application re-arms a task while its run is still calling an unsafe
processor, the new life may call that processor again. That repeat is caused by
the application's write, not by the platform **[ruling]**. The issue's
acceptance text will be reworded to say "per life".

| # | Acceptance | Met by |
|---|---|---|
| A1 | No run of a task's life starts while another run of the same life is in progress on a live pnode | §6.1 claim; §6.2 liveness and first-heartbeat rule; §6.3 self-cancel; §6.4 no give-back of a live run |
| A2 | A processor not declared safe to repeat is never executed again on the platform's initiative | §5.5 unsafe mark; §5.4 claim check on every commit; §10.2 mark serialised with the claim |
| A3 | A task that cannot succeed reaches a recorded, visible terminal state | §5.7 FAILED; §8 query; §9 telemetry. **[ruling]** A safe failure with no `timeoutMs` retries without end. The query shows it (status, attempts, last error), but it never reaches a final state. The acceptance text will be reworded. |
| A4 | The deciding pnode learns the outcome; an empty or partial cluster view is safe; runs in progress are bounded | §6.1: the claiming pnode runs the task itself, with no delegation and no cluster view read; at most `MAX_RUNS` runs at a time |
| A5 | Cross-backend parity and multi-node coverage | §13 |

## 3. Terms

- **Task**: the stored record "fire transition T of entity E at time X". Its
  id is a hash of (tenant, entity, source state, transition)
  (`internal/domain/workflow/arm.go:27-30`). The same id therefore returns every
  time the entity returns to that state.
- **Arm**: create or replace a task. Every arm draws a new random **arm token**
  and starts a new **life** of the task.
- **Claim**: a pnode takes a task in order to run it. Every claim draws a new
  random **claim token**. Tokens are UUIDs and are never reused, so an old
  token cannot match a later claim, even across lives of the same task id.
- **Owner**: the pnode incarnation that holds a claim.
- **Pnode incarnation**: a random UUID drawn when a pnode process starts. A
  restarted pnode is a new incarnation.
- **Run**: one attempt to fire a task, from claim to recorded outcome.
- **Hand-off**: the moment a callout reaches a compute node
  (`member.Send`, `internal/grpc/dispatch.go:189`).
- **Unsafe processor**: a processor whose `config.idempotent` is not true
  (`SPI/types.go:244-251`). Criteria and functions are never unsafe. They are
  always repeat-safe (`internal/grpc/callout.go:191, 263`), and that
  declaration covers whatever their callbacks do.
- **Store clock**: the database clock (PostgreSQL `now()`), or the injected
  clock of the memory and SQLite stores.

## 4. Task statuses and endings

```
          arm (entity write) — new life
               │
               ▼
   ┌──────► WAITING ◄───────────────┐
   │           │ claim (due, or     │ safe failure / given back
   │           │ owner lost)        │
   │           ▼                    │
   │        RUNNING ────────────────┘
   │           │
   │           ├── fired / declined / expired / cancelled ──► task removed (+ audit)
   │           └── unsafe not completed / owner lost too often /
   │               expired after failed attempts ──────────► FAILED (kept)
   │
   └── entity write in the source state (from any status, FAILED included)
```

| Ending | Task afterwards | Audit on the entity | Log |
|---|---|---|---|
| Fired | removed, or re-armed if the entity is back in the source state | `SCHEDULED_TRANSITION_FIRE` | DEBUG |
| Criterion false | removed | `TRANSITION_NOT_MATCH_CRITERION` | DEBUG |
| Too late on the first attempt, with no lost owner | removed | `SCHEDULED_TRANSITION_EXPIRE` | INFO |
| Transition gone from the workflow; entity carries no transaction id | removed | `SCHEDULED_TRANSITION_CANCEL` | as today |
| Entity gone, or moved on | removed | none (as today) | DEBUG |
| Safe failure (§5.6) | WAITING, attempts + 1, next-attempt time | none | WARN |
| Unsafe work handed off, run not committed | FAILED `UNSAFE_WORK_NOT_COMPLETED` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Owner lost `MAX_LOST_OWNERS` times | FAILED `OWNER_LOST_REPEATEDLY` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Past the deadline after a failed attempt or a lost owner | FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | `SCHEDULED_TRANSITION_FAIL` | ERROR |

A FAILED task is never claimed. Only an entity write ends it (§7):
- the entity leaves the state → the task is removed with `SCHEDULED_TRANSITION_CANCEL`;
- the entity is written in the state → a new life;
- the entity is deleted → the task is removed.

## 5. The run

### 5.1 Before the run

After it claims a task (§6.1), the owner decides from the claimed record, in
this order:

1. **Mark.** The unsafe mark is set for this life → FAILED
   `UNSAFE_WORK_NOT_COMPLETED` (§5.7).
2. **Lost owners.** `lostOwners ≥ MAX_LOST_OWNERS` → FAILED
   `OWNER_LOST_REPEATEDLY`.
3. **Deadline.** When `timeoutMs` is set, `deadline = scheduledTime +
   timeoutMs`. The task is **late** when either:
   - it has never failed and never lost an owner (`attempts == 0 &&
     lostOwners == 0`), and `now > deadline` — this is today's rule on the
     owner's clock; or
   - otherwise, its due time (`nextAttemptTime`, §5.6) is later than the
     deadline, or the claim is a lost-owner claim and `now > deadline`.

   A retry that the failure path scheduled at or before the deadline is
   therefore run, even when the scan picks it up a little after the deadline.

   A late task that never failed and never lost an owner is expired: removed
   in a transaction with `SCHEDULED_TRANSITION_EXPIRE`, as today, now fenced.
   Any other late task is FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`.
4. Otherwise, **run** (§5.2).

**A pnode crash counts as a lost owner.** So when a pnode dies, each task it
was running whose `timeoutMs` is shorter than `STALE_AFTER` ends as
`EXPIRED_AFTER_FAILED_ATTEMPTS`. This is intended: the timer did not fire as the
author meant, and the failure stays visible.

**The grace band is deleted.** The 100 ms band (`fire_scheduled.go:50-63,
276-295`) and `CYODA_SCHEDULER_EXPIRY_GRACE` existed only so that two pnodes
judging the same task at the same moment could not decide differently. Now one
owner decides, and only a decision made under a current claim can commit.

### 5.2 The run's transaction

`FireScheduledTransition(ctx, task, claim)` keeps its structure
(`internal/domain/workflow/fire_scheduled.go:89-524`), with these changes:

- The in-transaction re-read is `Get(tenant, id)`. If the task is gone, or its
  arm token or claim token is no longer this claim's, the run ends with outcome
  `superseded`.
- **Deleted**, because the claim covers them:
  - the pre-transaction read that seeds the origin — the claimed record
    already carries `ArmedBy`;
  - the `ArmedBy` verify-or-abort (`:215-217`);
  - the re-armed-into-the-future guard (`:250-255`);
  - the tenant-mismatch guard (`:195-203`), which existed only for the
    scheduler RPC;
  - the grace-band drop.
- **Every removal of the task** by the fire path (`Delete(id)` today) becomes
  `Complete(tenant, id, armToken, claimToken)` inside the transaction (§5.4).
- **Re-arming at the end** (`reconcileScheduledTasks`, `:487`) runs with this
  task excluded, as today. The fire path completes its own task **before** it
  calls it. So when the transition or cascade returns to the source state (a
  scheduled self-loop, or a guarded cascade back), the same id is armed as a
  new life after the old life is completed (contract C3, §10.1).

### 5.3 The run's context

The run executes on a context derived from the scheduler's run context. That
context is cancelled when the pnode stops (§6.4) or fences itself (§6.3). It no
longer derives from `context.Background()`
(`internal/scheduler/executor.go:44-45`). The system identity is attached to it
the way `common.SystemUserContext` builds it today.

The context carries a **run guard**: tenant, task id, arm token, claim token,
and the store.

**Cancellation must also reach the segments after a `COMMIT_BEFORE_DISPATCH`
commit.** Those segments begin with `context.WithoutCancel(ctx)`
(`engine_processors.go:388, 403, 530`) so that a commit is never cut off
halfway. Two rules keep that and still stop the run:

- When the run guard is present, the engine checks the run's cancellation at
  every step boundary after a commit: before each processor dispatch, before
  each cascade step, and before the final persist. A cancelled run stops there
  with the cancellation as its error.
- The dispatch context for a `startNewTxOnDispatch=false` callout (`:388`)
  carries the run's cancellation. A callout in flight is not cut off.

### 5.4 Every commit checks the claim

- **Final commit.** `Complete(tenant, id, armToken, claimToken)` is staged in
  the run's transaction. It removes the task only if the claim is current.
  Otherwise the commit fails, and the run is `superseded`.
- **Intermediate commits.** A `COMMIT_BEFORE_DISPATCH` processor commits
  TX_pre in the middle of a run (`engine_processors.go:469-515`). When the run
  guard is present, `flushAndCommitSegment` first stages `CheckClaim(tenant,
  id, armToken, claimToken)` in that segment. A superseded owner therefore
  cannot commit TX_pre.
- **Dependency.** A compute-node callback that joins the run's transaction can
  reach a `COMMIT_BEFORE_DISPATCH` processor and commit the run's transaction
  without this check (#599). A2 and the claim check are complete only once #599
  refuses that. **#599 lands before or with #598** (§14).

### 5.5 The unsafe mark

In `executeProcessors`, when the run guard is present and the processor is
unsafe:

1. **Before dispatch.** At every call site of `extProc.DispatchProcessor`
   (`engine_processors.go:230, 269, 360, 392`), the engine calls
   `MarkUnsafe(tenant, id, armToken, claimToken)`, unless this run already
   holds a mark. The call has three outcomes:
   - accepted → dispatch;
   - refused (`ErrStaleClaim`) → the processor is not dispatched and the run
     is `superseded`;
   - any other error → the processor is not dispatched, the run fails, and the
     run is treated as holding a mark. The mark may have been written. Failing
     the task is closed and safe.
2. **After a failed dispatch.** The engine checks whether the error proves
   that nothing was handed off. It is proof only when:
   - the error is a `*contract.CalloutFailure` (`internal/contract/callout.go:77-95`)
     whose `Kind` is `NoHandOff` and whose `Attempts` are all `NoHandOff`; or
   - the error is `contract.ErrNoMatchingMember` with no attempts.

   If there is proof, and the mark was written for this processor (no earlier
   unsafe processor in the run was handed off), the engine calls
   `ClearUnsafe(tenant, id, armToken, claimToken)`. A `Terminal` failure is
   not proof, even when it happened before sending. That is intended: the
   engine cannot see where it happened, and treating it as handed off is the
   safe side.
3. **Otherwise the mark stays.** That includes a failed `ClearUnsafe`, and a
   pnode that dies before `ClearUnsafe` runs. The task then becomes FAILED and
   is not repeated.

**Scope of the mark.** The mark belongs to the task's life (the arm token), not
to one claim, so every later claim of the same life sees it. A re-arm starts a
new life with no mark.

**No protocol change.** The pnode-to-pnode protocol does not change. The owner
writes the mark before it dispatches, so a #254 hand-over to a peer pnode is
covered.

**Nested work.** Callbacks inside an unsafe processor are covered by that
processor's mark. Callbacks inside a processor declared `idempotent`, a
criterion or a function are covered by their declaration, which the engine
takes on trust (`SPI/types.go:244-251`).

**`ASYNC_NEW_TX`** processors are dispatched at `:269` and are marked in the
same way. Their own failure is not fatal to the run (`:175-183`). So a failed
unsafe `ASYNC_NEW_TX` processor that was handed off leaves the mark:
- if the run then commits, the task is completed;
- if the run fails later, the task is FAILED.

### 5.6 After the run: the outcome is always recorded

A run whose transaction did not commit **always** ends with a fenced
bookkeeping write. There is no "write nothing" path. Transient store errors are
retried (1 s, doubling, up to `HEARTBEAT_INTERVAL`) until the store accepts or
refuses the write, or until the pnode stops. A refusal (`ErrStaleClaim`)
means the run was superseded, and nothing more is done.

| The run | Bookkeeping |
|---|---|
| holds a mark | `Fail(UNSAFE_WORK_NOT_COMPLETED)` (§5.7) |
| was cancelled by shutdown (§6.4) and holds no mark | `RecordAttempt{NotCounted: true, NextAttemptTime: now}` |
| was cancelled by self-cancel (§6.3), or failed with an error, and holds no mark | `RecordAttempt{Error, NextAttemptTime}` (safe failure) |
| panicked | see §6.5 |

`RecordAttempt` sets the task WAITING, adds 1 to `attempts` unless
`NotCounted`, records the error (§5.8) and clears the claim.

**Next-attempt time:**

```
delay = RETRY_DELAY × 2^(attempts−1), saturating at RETRY_DELAY_MAX   // attempts after +1
next  = now + delay
if timeoutMs set: next = min(next, deadline)
```

If the deadline has already passed when the attempt is recorded, the task is
FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` instead. So the last attempt is made at
the deadline (§5.1 runs it), and a failure after that is final. The
multiplication saturates, so it cannot overflow however often a task without a
`timeoutMs` has failed.

A criterion that evaluates to false is not a failure. It declines the task, as
today.

**The log line** for a safe failure (WARN) is written after `RecordAttempt` is
accepted, so a superseded run logs nothing at WARN or ERROR.

### 5.7 FAILED

`Fail(tenant, id, armToken, claimToken, reason, error, atMs)` and the
`SCHEDULED_TRANSITION_FAIL` audit event are written in one small transaction
that commits. The audit event's data is `{transition, sourceState, reason,
attempts, lostOwners}`. The task keeps its status, reason, error and failed
time.

That commit, identified uniquely and stably by (task id, arm token), is the
point from which the planned notification feature can later publish a "timer
failed" event.

### 5.8 The recorded error (Gate 3)

The error is shown to tenant users (§8), so it follows the 4xx/5xx rule. The
engine classifies it with the same classifier the entity service uses for
workflow errors (`classifyWorkflowError`,
`internal/domain/entity/service.go:2795`). That classifier moves to a package
both can import, so there is one classification.

- A `*contract.CalloutFailure` of kind `MemberFailed` → the compute node's own
  message, which is tenant-owned (`callout.go:82`).
- A classified 4xx `*common.AppError` → its code and message.
- Anything else (a 5xx: storage, engine) → `internal error [ticket: <uuid>]`.
  The full error is logged at ERROR under that ticket.
- The text is truncated to 1 024 bytes.

## 6. The scheduler service

`internal/scheduler` is rewritten around one loop per pnode.

### 6.1 Claiming

- **No claim before the first heartbeat.** The loop starts claiming only after
  its first `Heartbeat` has succeeded. The same holds after a self-cancel
  (§6.3).
- **When it wakes.** Every `CYODA_SCHEDULER_SCAN_INTERVAL` (1 s). It also wakes
  when a run slot frees, if the previous claim filled every free slot, so
  throughput is bounded by run time and not by the scan interval.
- **The call.** `ClaimDue(ClaimRequest{Owner: incarnation, NowMs: clock.Now(),
  StaleAfter, Limit: free slots})`. Free slots = `CYODA_SCHEDULER_MAX_RUNS` (8)
  minus runs in progress. With no slot free, it does not call.
- **Each claimed task** runs on its own goroutine: the §5.1 decision, then the
  run.
- **Self-heal on every tick.** `GiveBackIdle(incarnation, liveClaimTokens)`
  gives back every task this incarnation holds RUNNING whose claim token is not
  in its set of live runs. This clears a claim left behind when bookkeeping
  never reached the store (for example the pnode stopped retrying at shutdown).
  It is not counted as a lost owner. A mark belongs to the life, so a
  given-back task that holds a mark still becomes FAILED on its next claim.

`ClaimDue` is one atomic store operation (§10). A task is claimable when:

- it is WAITING and `nextAttemptTime ≤ NowMs`, judged on the pnode clock (the
  clock domain that set `scheduledTime` at arm); or
- it is RUNNING and its owner's liveness record is missing, or older than
  `StaleAfter` by the store clock. That claim adds 1 to `lostOwners` in the
  same step.

A claimed task becomes RUNNING with a new claim token and the owner. Claims are
taken in order of `nextAttemptTime`.

### 6.2 Liveness

- **Heartbeat.** Each pnode writes `Heartbeat(incarnation)` every
  `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (15 s), from start to stop, whether or
  not it has runs. The store stamps the time with the store clock.
- **Stale period.** `CYODA_SCHEDULER_STALE_AFTER` (1 min) must be at least 4 ×
  the interval. Startup fails otherwise, as it does for
  `CYODA_SEARCH_JOB_STALE_AFTER` (`app/config.go:861-886`).
- **A slow run keeps its task.** So does a run that hangs while its pnode keeps
  heartbeating. Liveness is not progress, the same position as #509. Every
  callout in a run is bounded (R§2.7).
- **Liveness records of dead incarnations** are removed by the claim loop,
  once a minute, after 10 × `STALE_AFTER` without a heartbeat, when no task
  references them any more.

### 6.3 Self-cancel when the heartbeat fails

The pnode records, on its monotonic clock, when it **sent** its last heartbeat
that succeeded. If that moment is more than `STALE_AFTER − HEARTBEAT_INTERVAL`
ago, the pnode:
- cancels every run in progress (outcome `self_cancelled`);
- stops claiming until a heartbeat succeeds again.

This happens before any other pnode may take its tasks, provided clocks run at
the same rate. A frozen VM whose monotonic clock does not advance is not
covered by this rule. The mark's serialisation with the claim (§10.2) covers
that case for unsafe work.

The store must not let entity transactions starve heartbeats and claims of
connections (contract C2).

### 6.4 Shutdown

`cmd/cyoda/run.go` today drains the HTTP, admin and gRPC servers concurrently,
in an errgroup, once the context ends (`:117-171`). The scheduler is stopped
only afterwards, in `a.Shutdown()`. New order **on a signal**:

1. The scheduler stops claiming.
2. It waits up to `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (20 s) for runs in progress.
   Compute-node streams and callback routes are still open during this wait.
3. It cancels the runs still in progress, and **waits until each has ended and
   recorded its outcome** (§5.6). A run past a `COMMIT_BEFORE_DISPATCH` commit
   ends at its next step boundary (§5.3). A callout in flight ends at its own
   deadline.
4. Only then: `GiveBackIdle(incarnation, ∅)` for any claim whose bookkeeping
   did not reach the store, then `RetireOwner(incarnation)`.
5. The server drains start, as today.

When the group ends because a server failed rather than on a signal, the same
sequence runs from `a.Shutdown()`, after the servers. That is best effort,
because the streams are already gone.

A run that is still inside a callout at step 3 can outlast the pod's grace
period. SIGKILL then leaves its task RUNNING. It is reclaimed after
`STALE_AFTER` and counted as a lost owner, which is correct: the pnode did not
hand the task back.

**Grace period.** The Helm chart sets `terminationGracePeriodSeconds: 60`
(`deploy/helm/*/templates/statefulset.yaml`, new value
`terminationGracePeriodSeconds`). The "SHUTDOWN TIMING" section of
`help/content/run.md` is rewritten with the new order and worst case.

### 6.5 Panics

Every new goroutine recovers panics: the claim loop, the heartbeat, and each
run. Each one latches the node unhealthy, as the dispatch goroutine does today
(`internal/scheduler/service.go:200-209`, `docs/ARCHITECTURE.md:382`).

- **A panicking run** records a fenced outcome:
  - `Fail(UNSAFE_WORK_NOT_COMPLETED)` if it holds a mark;
  - otherwise `RecordAttempt{LostOwner: true}`, which counts toward
    `MAX_LOST_OWNERS`.

  The latch takes the pnode out of service, as a crash does, so a panic counts
  as a crash. A task whose run always panics is FAILED `OWNER_LOST_REPEATEDLY`
  after the cap. It does not travel through the whole cluster.
- **A panicking claim loop or heartbeat** latches the node and stops claiming.
  Its runs self-cancel (§6.3).

### 6.6 Deleted

- `internal/scheduler/coordinator.go` and `distribution.go`.
- `ClusterExecutor`, and `internal/cluster/scheduler_rpc.go` with the
  `/internal/dispatch/scheduled-task` route and `SchedulerRPCClient`.
- `Config.RedispatchBackoff` and `BatchSize`.
- Their wiring (`app/app.go:600-657, 810-827`) and their tests.

## 7. Entity writes and tasks

- **Arm** (`reconcileScheduledTasks` → `ReconcileForEntity`). An armed task
  always starts a new life: WAITING, `nextAttemptTime = scheduledTime`,
  attempts and lostOwners 0, errors cleared, a new arm token, no claim. This
  applies whatever status the old life had:
  - a RUNNING owner is fenced out;
  - a FAILED task is replaced, and if the unsafe processor then runs again, it
    is because the application wrote the entity.
- **Cancel on leaving the state**: as today. It removes the task whatever its
  status and records `SCHEDULED_TRANSITION_CANCEL`.
- **Entity delete removes the entity's tasks, in the same transaction, on every
  delete path:**
  - `DeleteEntity` (`internal/domain/entity/service.go:656`) →
    `DeleteForEntities(tenant, [id])`;
  - `DeleteEntitiesConditional` (`:1205`) — in its single-transaction loop
    (`:1314`) and in each batch of `deleteBatched` (`:1697`) →
    `DeleteForEntities(tenant, ids)`;
  - `DeleteAllEntities` (`:778`) — the fast path, which lists no ids, also
    reached from the conditional path (`:1230`) →
    `DeleteForModel(tenant, model, version)`.

  The gRPC doors reach the same functions (`internal/grpc/entity.go:200,
  484`). No audit event is written; the entity's history ends with its
  deletion.
- Arm, cancel and the `DeleteFor*` removals are **not** fenced by a claim.
  They are the application's decision, and they fence any owner out.

## 8. The task query — `GET /scheduled-tasks`

**HTTP only.** This is an operator's view that no compute node needs. The audit
trail has no gRPC door for the same reason. The coverage matrix records the
waiver (§13).

**Access.** Any authenticated user of the tenant, as for the audit trail
(`internal/domain/audit/handler.go` checks no role). The tenant comes from the
token and never from a parameter.

**Parameters** (all optional):

| Name | Type | Rule |
|---|---|---|
| `status` | repeatable; `WAITING`, `RUNNING`, `FAILED` | unknown value → 400 |
| `modelName` | string, 1–256 characters | |
| `modelVersion` | integer ≥ 1 | only together with `modelName`; alone → 400 |
| `entityId` | UUID | not a UUID → 400 |
| `cursor` | opaque string, max 256 | invalid → 400; the value is not echoed |
| `limit` | integer 1–1000, default 20 | outside the range → 400 `BAD_REQUEST`. It is rejected, not clamped: fail closed. |

**Order.** `(scheduledTime, taskId)` ascending, which is a total order. The
cursor is versioned base64url JSON encoding that position, decoded strictly
like the audit cursor (`internal/domain/audit/cursor.go:65-114`).

**Response 200:**
```json
{ "items": [ ScheduledTaskDto ], "pagination": { "hasNext": true, "nextCursor": "..." } }
```

`ScheduledTaskDto` (typed but open, ADR 0003):

| Field | Type | Present |
|---|---|---|
| `taskId` | string (a hash, not a UUID) | always |
| `entityId` | string, uuid | always |
| `modelName`, `modelVersion` | string, integer | always |
| `sourceState`, `transition` | string | always |
| `status` | string (open: `WAITING`, `RUNNING`, `FAILED`) | always |
| `scheduledTime` | date-time | always |
| `expiresTime` | date-time | when `timeoutMs` is set |
| `attempts`, `lostOwners` | integer | always |
| `nextAttemptTime` | date-time | WAITING |
| `lastAttemptTime`, `lastError` | date-time, string | after a failed attempt |
| `failureReason` | string (open: the three reasons) | FAILED |
| `failedTime` | date-time | FAILED |
| `armedTime` | date-time | always |
| `armedBy` | `{id, kind}` | when known |

Node ids, claim tokens and arm tokens are internal and are not returned.

**Error table:**

| Status | Code | When |
|---|---|---|
| 200 | — | success, an empty list included |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName`; `modelVersion` not an integer ≥ 1; `entityId` not a UUID; `modelName` empty or too long; `limit` not an integer or outside 1–1000; invalid `cursor` |
| 401 | `UNAUTHORIZED` | no token, or an invalid token |
| 500 | `SERVER_ERROR` | internal failure; generic message with a ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no 403, because no role is required. There is no 404: an unknown
model or entity, including another tenant's, gives an empty list. The query
therefore does not reveal whether another tenant's data exists.

## 9. Telemetry and logs

Instruments come from `observability.Meter()`. They carry no tenant attribute.

| Name | Type | Attributes | Meaning |
|---|---|---|---|
| `cyoda.scheduler.runs` | counter | `outcome`: fired, declined, expired, cancelled, attempt_failed, failed, superseded, self_cancelled, shutdown_cancelled, panicked | one per ended run or pre-run decision |
| `cyoda.scheduler.run.duration` | histogram, s | `outcome` | claim to recorded outcome |
| `cyoda.scheduler.runs.in_progress` | up-down counter | — | |
| `cyoda.scheduler.claims` | counter | `reason`: due, owner_lost | |
| `cyoda.scheduler.heartbeat.failures` | counter | — | |

- **Span:** `scheduler.run`, with `outcome`, recording errors like the other
  spans.
- **Logs:**
  - WARN per safe failure, with task id, entity id, transition, attempts and
    next attempt;
  - ERROR per FAILED task, with the reason and a ticket when there is one;
  - WARN per self-cancel;
  - ERROR per panic.

All of it is documented in `help/content/telemetry.md`.

## 10. Storage contract (SPI)

### 10.1 `ScheduledTaskStore`

```go
type ScheduledTaskStatus string        // "WAITING" | "RUNNING" | "FAILED"
type ScheduledTaskFailureReason string // "UNSAFE_WORK_NOT_COMPLETED" | "OWNER_LOST_REPEATEDLY" | "EXPIRED_AFTER_FAILED_ATTEMPTS"

type ScheduledTask struct {
    // unchanged: ID, TenantID, Type, ScheduledTime, TimeoutMs, EntityID,
    // ModelName, ModelVersion, Transition, SourceState, ArmedAt, ArmedBy
    Status          ScheduledTaskStatus
    ArmToken        uuid.UUID  // drawn by the store on every arm
    NextAttemptTime int64      // unix ms; = ScheduledTime on arm
    Attempts        int
    LostOwners      int
    LastAttemptTime *int64
    LastError       string
    FailureReason   ScheduledTaskFailureReason
    FailedTime      *int64
    Claim           *TaskClaim // RUNNING only
    UnsafeMarked    bool       // read-only: a mark exists for this ArmToken
    LostOwnerClaim  bool       // read-only, ClaimDue result only: this claim took the task from a lost owner
}
type TaskClaim struct{ Token, Owner uuid.UUID }
```

**Removed:** `RedispatchAfter`, `AttemptCount`, `Upsert`, `ScanDue`,
`MarkRedispatch`, `Delete`.

| Method | Transaction | Contract |
|---|---|---|
| `ReconcileForEntity(req)` | joins the entity tx | as today; every armed task starts a new life (§7) |
| `Complete(tenant, id, armToken, claimToken)` | joins | removes the task if the claim is current; else `ErrStaleClaim`, at the call or at commit |
| `CheckClaim(tenant, id, armToken, claimToken)` | joins | fails the tx with `ErrStaleClaim` if the claim is not current at commit |
| `DeleteForEntities(tenant, ids)` | joins | removes every task of those entities |
| `DeleteForModel(tenant, name, version)` | joins | removes every task of that model version |
| `Fail(tenant, id, armToken, claimToken, reason, error, atMs)` | joins the bookkeeping tx of §5.7 | fenced; FAILED; claim cleared |
| `Get(tenant, id)` | may join | tenant-scoped; `found=false` for another tenant's id |
| `Query(tenant, filter, cursor, limit)` | **never joins** | tenant-scoped page, ordered as in §8 |
| `ClaimDue(req)` | **never joins** | atomic; disjoint across concurrent callers; cross-tenant; §6.1 |
| `Heartbeat(owner)` | **never joins** | upserts liveness with the store clock |
| `RetireOwner(owner)` | **never joins** | removes liveness |
| `SweepOwners(olderThan)` | **never joins** | removes liveness records of dead incarnations with no tasks |
| `GiveBackIdle(owner, keep []claimToken)` | **never joins** | every task RUNNING under `owner` whose claim token is not in `keep` becomes WAITING; not counted |
| `MarkUnsafe(tenant, id, armToken, claimToken)` | **never joins** | records the mark if the claim is current, serialised with `ClaimDue` (C4); idempotent only for the same claim token; a mark written by another claim of the same life → `ErrStaleClaim` |
| `ClearUnsafe(tenant, id, armToken, claimToken)` | **never joins** | removes this claim's mark if the claim is current; else `ErrStaleClaim` |
| `RecordAttempt(tenant, id, armToken, claimToken, Attempt)` | **never joins** | fenced; WAITING; claim cleared; `attempts+1` unless `NotCounted`; `lostOwners+1` if `LostOwner` |

"Never joins" means the method ignores any transaction on `ctx` and commits on
its own. A mark written from inside the run survives the run's rollback. That
is exactly what makes it work. (Today the PostgreSQL store joins whatever
transaction `ctx` carries, `plugins/postgres/scheduled_task_store.go:12-20`.)

Refusals return `spi.ErrStaleClaim`, which already exists. Its doc comment
(`SPI/errors.go:155`) is widened from async search to both stores. A missing
task is also refused as stale.

**Contract clauses:**
- **(C1)** A transaction that writes tasks (arm, cancel, complete, check,
  delete) and a write that never joins may not update the same data. The one
  exception is a claim changing hands (§10.2), which must make a later
  `Complete` or `CheckClaim` fail.
- **(C2)** Heartbeats and claims must not be starved of connections by entity
  transactions.
- **(C3)** Operations staged in one transaction apply in the order they were
  staged. `Complete(X)` followed by an arm of X leaves the new life.
- **(C4)** A `MarkUnsafe` and a `ClaimDue` that race on the same task
  serialise: either the mark is refused, or the claim sees the mark.

The conformance cases move from `SPI/scheduled_task_store_conformance.go` into
`SPI/spitest`, next to async search. They cover every method, every refusal and
every clause, including "a mark survives the rollback of a transaction on
`ctx`". A backend whose store answers "not implemented" (the commercial
backend today) skips them.

### 10.2 PostgreSQL

A new migration. There are no production instances, so existing columns are
simply replaced.

- **`scheduled_tasks` — the arm record.** One row per task id. Written only by
  task-writing transactions (arm, cancel, complete, delete).
  - Columns: today's columns, plus `arm_token uuid`; minus
    `redispatch_after`, `attempt_count`.
  - Indexes:
    - `(tenant_id, scheduled_time, id)` — query order;
    - `(tenant_id, model_name, model_version)` — query filter and
      `DeleteForModel`;
    - keep `(tenant_id, entity_id)`;
    - drop `scheduled_tasks_due_idx`.
- **`scheduled_task_runs` — the run state of one life.**
  `(task_id, arm_token) PK, tenant_id, status, claim_token, claim_owner,
  next_attempt_time, attempts, lost_owners, last_attempt_time, last_error,
  failure_reason, failed_time`.
  - Inserted by the arming transaction, as a new key, so it never conflicts.
  - After that, written only by the never-joining methods.
  - Its `claim_token` changes only when a claim changes hands: a claim, a
    recorded outcome or a give-back. The owner does not write this row while
    its run is in progress.
  - Rows whose `(task_id, arm_token)` no longer matches a task are swept in
    batches by the claim loop, once a minute.
  - Indexes:
    - `(next_attempt_time) WHERE status='WAITING'`
    - `(claim_owner) WHERE status='RUNNING'`
- **`scheduled_task_marks`** — `(task_id, arm_token) PK, claim_token`. Swept
  like the run rows.
- **`scheduler_owners`** — `(owner uuid PK, heartbeat_at timestamptz)`.
- **`ClaimDue`.** One statement, following the async-search claim
  (`plugins/postgres/search_store.go:570-587`):
  - a CTE selects claimable `scheduled_task_runs` rows `FOR UPDATE OF r SKIP
    LOCKED`;
  - `LEFT JOIN scheduler_owners` decides staleness (`now() - $stale`);
  - `EXISTS` on marks gives `unsafe_marked`;
  - `ORDER BY next_attempt_time LIMIT $n`;
  - then `UPDATE … RETURNING`, joined back to `scheduled_tasks` for the
    task's fields.
  - The CTE inner-joins `scheduled_tasks` on `(id, arm_token)`, so a run row
    whose life has ended (an orphan awaiting the sweep) is never claimed.
    `Query` and `Get` use the same join.
- **`MarkUnsafe` (C4).** One short transaction:
  - `SELECT … FROM scheduled_task_runs WHERE task_id, arm_token, claim_token
    FOR SHARE`, which fails if the claim is not current;
  - then `INSERT … ON CONFLICT (task_id, arm_token) DO NOTHING RETURNING
    claim_token`;
  - on conflict, compare the stored claim token: the same token → accepted;
    another token → `ErrStaleClaim`.

  The row lock serialises the mark with `ClaimDue`'s `FOR UPDATE SKIP LOCKED`.
  If the claim comes first, the mark's `FOR SHARE` re-checks the new claim
  token and finds no row. If the mark comes first, the claim skips the locked
  row, and the next scan sees the mark.
- **`Complete` and `CheckClaim`** inside the entity transaction (REPEATABLE
  READ):
  - `SELECT 1 FROM scheduled_task_runs WHERE task_id, arm_token,
    claim_token FOR SHARE`;
  - for `Complete`, also `DELETE FROM scheduled_tasks WHERE id, tenant_id,
    arm_token`.

  A claim changing hands after the transaction began makes the lock fail with
  a serialisation error. For these statements that error is mapped to
  `ErrStaleClaim`. A row found without a match is also `ErrStaleClaim`.
  Because the owner never writes its own run row during a run, a run never
  conflicts with itself. That is C1.
- **Separate tables mean no false conflicts.** Entity transactions write only
  `scheduled_tasks` (and insert new run rows). Bookkeeping, claims and marks
  write only the other tables. So a retrying sibling task never makes an entity
  write fail.
- **Connections (C2).**
  - Never-joining methods run on a dedicated pool of
    `CYODA_POSTGRES_SCHEDULER_CONNS` (3). `Heartbeat` has one connection of its
    own, with a 5 s acquire timeout.
  - The async-search heartbeat and claim statements
    (`plugins/postgres/search_store.go`) move to the same pool. They have the
    same starvation risk on the main pool.
  - `Fail` joins the small bookkeeping transaction on the main pool.
- **Tenant scoping.**
  - Every tenant-facing statement filters on `tenant_id`: `Get`, `Query`,
    `Complete`, `CheckClaim`, `DeleteFor*`, `Fail`, `MarkUnsafe`,
    `ClearUnsafe`, `RecordAttempt`.
  - The cross-tenant statements are the scheduler's own: `ClaimDue`,
    `GiveBackIdle`, `Heartbeat`, `RetireOwner`, `SweepOwners` and the
    sweepers. None of them is reachable from an API.
  - The tables stay out of row-level security, and the migration comment that
    claims otherwise (`000004_scheduled_tasks.up.sql:5-14`) is corrected.

### 10.3 Memory and SQLite

Both run on one pnode only, but they meet the same contract. A backend that
differs from the others is a bug.

- **Staged operations** (arm, cancel, `Complete`, `CheckClaim`, `DeleteFor*`)
  apply in staging order.
  - **Memory:** the claim checks run in the commit's validation step, under
    `entityMu`, before anything is applied
    (`plugins/memory/txmanager.go:530`) — **V1**.
  - **SQLite:** the checks are statements inside the commit's `sqlTx`; an
    error rolls it back (`plugins/sqlite/txmanager.go:885-894`).
- **Never-joining methods** apply immediately:
  - **Memory:** under `entityMu`.
  - **SQLite:** single conditional statements on the writer connection. The
    claim reads first, then updates each row conditionally, like the
    async-search claim (`plugins/sqlite/search_store.go:404-496`).

  Both must ignore any transaction on `ctx`. The memory `stage()` and the
  SQLite store today join one when present.
- **Clock:** the store clock is the injected clock.
- **Removals:** `Delete` is gone, and with it the unreliable "was it removed?"
  answer (R§2.5 item 9). Fenced removals report refusal through
  `ErrStaleClaim`. Arm, cancel and `DeleteFor*` are not fenced (§7).

## 11. Configuration (Gate 4)

| Variable | Default | Rule | Status |
|---|---|---|---|
| `CYODA_SCHEDULER_ENABLED` | true | | kept |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | 1s | > 0 | kept; now validated |
| `CYODA_SCHEDULER_MAX_RUNS` | 8 | ≥ 1 | new |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | 15s | > 0 | new |
| `CYODA_SCHEDULER_STALE_AFTER` | 1m | ≥ 4 × heartbeat interval | new |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | 3 | ≥ 1 | new |
| `CYODA_SCHEDULER_RETRY_DELAY` | 30s | > 0 | new |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | 15m | ≥ retry delay | new |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | 20s | ≥ 0 | new |
| `CYODA_POSTGRES_SCHEDULER_CONNS` | 3 | ≥ 2 | new (postgres plugin) |
| `CYODA_SCHEDULER_DISTRIBUTION` | — | | removed |
| `CYODA_SCHEDULER_COORDINATOR` | — | | removed |
| `CYODA_SCHEDULER_REDISPATCH_BACKOFF` | — | | removed |
| `CYODA_SCHEDULER_BATCH_SIZE` | — | | removed |
| `CYODA_SCHEDULER_EXPIRY_GRACE` | — | | removed |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | — | | removed; its only user was the scheduler RPC (`app/app.go:608`) |

Each change is made in:
- `app/config.go` (`DefaultConfig`, `Validate`; `:468, :474-482, :945-946`);
- `cmd/cyoda/help/config_registry.go` (`:80, :131-137`);
- `help/content/config/scheduler.md`;
- `help/content/config/cluster.md` (the removed forward timeout);
- `help/content/config/database.md` (`CYODA_POSTGRES_SCHEDULER_CONNS`);
- the summary line in `help/content/config.md:33`;
- `README.md` (`:224-236, :251`).

Startup fails on an invalid value. The Helm chart exposes no scheduler
variables today, and this change adds none. It adds
`terminationGracePeriodSeconds` (§6.4).

## 12. Documentation, parity and other repositories

- **`help/content/workflows.md` "SCHEDULED TRANSITIONS":** one owner per run;
  retry until `timeoutMs`; FAILED and its reasons; the new audit event; what
  an entity write does to a FAILED task.
- **`help/content/workflows.md:182` and the `idempotent` description in
  `api/openapi.yaml`:** the declaration now also decides whether a scheduled
  run may be repeated.
  - Only the meaning changes, not the shape, so `WorkflowConfigurationDto`
    needs no schema-version bump (`docs/workflow-schema-versioning.md`). The
    commit message says so.
- **New help topic `scheduled-tasks`** for the endpoint, shaped like
  `audit.md`, and added to `topLevelTopicsV061`.
- **`help/content/run.md`:** "SHUTDOWN TIMING" (§6.4).
- **`help/content/telemetry.md`:** §9.
- **`api/openapi.yaml`:**
  - the new operation and its DTOs;
  - `SCHEDULED_TRANSITION_FAIL` in the audit event enum (`:11742`);
  - `go generate ./api`.
- **SPI:**
  - `SMEventScheduledTransitionFailed = "SCHEDULED_TRANSITION_FAIL"`;
  - the `ErrStaleClaim` doc comment;
  - remove the stale sentence in `TransitionSchedule`'s doc ("until then,
    consuming engines silently skip scheduled transitions").
- **`docs/cloud-parity/scheduled-transitions.md`, rewritten (Gate 7).**
  - §1: the grace band is removed.
  - §5: any write resets the timer, FAILED included.
  - §6: the audit event set, with `SCHEDULED_TRANSITION_FAIL`.
  - §7 and §8 are replaced by: one owner per life; unsafe work is never
    repeated; retry until `timeoutMs`; the FAILED reasons; the lost-owner cap;
    the query; entity delete removes tasks.
  - A CaaS ticket is filed for Cloud to follow.
- **`docs/ARCHITECTURE.md`**, the scheduler passages, rewritten in the present
  tense: `:94, :230, :382-391, :772, :940, :943, :1359, :1486-1506,
  :1550-1553, :1992`.
- **`internal/domain/search/reaper.go:18`:** the comment that names `ScanDue`.
- **`CHANGELOG.md` `### Breaking`:** the removed variables; processors not
  declared `idempotent` are no longer repeated by scheduled runs; the FAILED
  status; the new query.
- **`COMPATIBILITY.md`:** the SPI pin bump.
- **SPI, mid-milestone:** an SPI PR into `main`, pseudo-pinned by cyoda-go; no
  tag.
- **cyoda-go-cassandra#68:** updated to this contract.

## 13. Coverage matrix

Layers:
- **U** — unit tests in the owning package.
- **S** — `spitest` conformance on memory, SQLite and PostgreSQL.
- **E** — `internal/e2e`, on PostgreSQL through the HTTP stack.
- **P** — a cross-backend parity scenario registered in
  `e2e/parity/registry.go`.
- **M** — multi-node PostgreSQL (`e2e/parity/postgres` multinode).

Rules:
- Concurrency and timing cases stay out of **P** (`.claude/rules/test-coverage.md`).
- Parity fixtures that retry set a short `CYODA_SCHEDULER_RETRY_DELAY` in
  `fixtureutil/tuned_env.go` (`:18`, next to the scan interval), and the
  commercial backend's fixture takes it from there.

### Endings

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| fired on time | ✓ | | ✓ | ✓ | |
| fired after one safe failure (compute node unavailable, then available) | ✓ | | ✓ | ✓ | |
| scheduled self-loop fires and re-arms the same id as a new life | ✓ | ✓ (C3) | ✓ | ✓ | |
| declined (criterion false) | ✓ | | ✓ | ✓ | |
| expired (late on the first attempt) | ✓ | | ✓ | ✓ | |
| safe failure: criterion error → WAITING, attempts 1, error recorded | ✓ | | ✓ | ✓ | |
| safe failure: no compute node for the tag → mark cleared, WAITING | ✓ | | ✓ | ✓ | |
| safe failure: idempotent processor fails → WAITING | ✓ | | ✓ | ✓ | |
| retry delay doubles, saturates, and is clamped to the deadline; the attempt at the deadline runs | ✓ | | | | |
| past the deadline after failed attempts → FAILED | ✓ | | ✓ | ✓ | |
| unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never run again | ✓ | | ✓ | ✓ | |
| a later step fails after an unsafe hand-off → FAILED | ✓ | | ✓ | ✓ | |
| `MarkUnsafe` fails with a transient error → not dispatched, FAILED | ✓ | | | | |
| unsafe `ASYNC_NEW_TX` fails, the run commits → completed | ✓ | | ✓ | | |
| CBD: TX_pre committed, later failure, all idempotent → retried from the TX_pre state | ✓ | | ✓ | | |
| CBD: unsafe CBD processor, then a failure → FAILED | ✓ | | ✓ | | |
| CBD: cancellation after TX_pre stops the run at the next step | ✓ | | ✓ | | |
| FAILED audit event recorded with its reason | ✓ | | ✓ | ✓ | |
| a panicking run: node latched, counted as a lost owner; FAILED after the cap | ✓ | | | | |

### Ownership, fencing and liveness

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| concurrent `ClaimDue` calls get disjoint sets | | ✓ | | | ✓ |
| a run longer than 3 × heartbeat interval is not claimed by another pnode | | | | | ✓ |
| owner killed without a mark → claimed after `STALE_AFTER`, `lostOwners` 1, runs, fires | | | | | ✓ |
| owner killed with a mark → FAILED; the processor was sent once | | | | | ✓ |
| owner killed, `timeoutMs` < `STALE_AFTER` → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | | | | | ✓ |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | ✓ | ✓ | | | |
| a stale claim token is refused by `Complete`, `CheckClaim`, `MarkUnsafe`, `ClearUnsafe`, `RecordAttempt`, `Fail` | | ✓ | | | |
| `MarkUnsafe` racing `ClaimDue`: the mark is refused, or the claim sees it (C4) | | ✓ | | | ✓ |
| a mark by another claim of the same life → `MarkUnsafe` refused | | ✓ | | | |
| a mark survives the rollback of the transaction on `ctx` | | ✓ | | | |
| a superseded owner sends no unsafe processor | ✓ | | ✓ | | |
| a superseded owner's CBD TX_pre is refused | ✓ | | ✓ | | |
| the old owner's token after the same id is re-armed and claimed again is refused (ABA) | ✓ | ✓ | | | |
| a re-arm while RUNNING fences the owner; its bookkeeping is refused | ✓ | ✓ | ✓ | | |
| a retrying sibling task never makes the run's commit or a client write fail (C1) | | ✓ | ✓ | | |
| heartbeat failure → self-cancel before `STALE_AFTER`, no claims until it recovers | ✓ | | | | |
| no claim before the first heartbeat | ✓ | | | | |
| heartbeats are not starved when every main-pool connection is in an entity tx (C2) | | | ✓ | | |
| a stuck RUNNING task of this incarnation with no live run is given back (self-heal) | ✓ | ✓ | | | |
| never more than `MAX_RUNS` runs at a time; the next claim waits for a slot | ✓ | | ✓ | | |
| a slot freed after a full claim → immediate claim, not the next tick | ✓ | | | | |
| an empty cluster view has no effect (no coordinator) | ✓ | | | | ✓ |
| dead-incarnation liveness records are swept | | ✓ | | | |

### Shutdown

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| runs finish within the drain; compute-node streams stay open during it | ✓ | | ✓ | | |
| a run cut by the drain without a mark → WAITING, not counted, claimed at once by another pnode | ✓ | | | | ✓ |
| a run cut by the drain with a mark → FAILED | ✓ | | ✓ | | |
| no task is given back while its run is still live (the run past CBD ends first) | ✓ | | | | |
| `GiveBackIdle` does not count; `RetireOwner` removes liveness | | ✓ | | | |

### Entity writes

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| a FAILED task is re-armed by an update in the state (new life, no mark) | ✓ | ✓ | ✓ | ✓ | |
| a FAILED task is cancelled when the entity leaves the state | ✓ | | ✓ | ✓ | |
| deleting one entity removes its tasks (HTTP and gRPC) | ✓ | ✓ | ✓ | ✓ | |
| conditional delete removes tasks: single-tx loop, batched, fast path (HTTP and gRPC) | ✓ | ✓ | ✓ | ✓ | |
| delete-all removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |

### Query (`GET /scheduled-tasks`)

| Scenario | U | S | E | P |
|---|---|---|---|---|
| 200, no filter, paging over several pages | ✓ | ✓ | ✓ | ✓ |
| 200, each filter: status (one and several), model name, model name + version, entity | ✓ | ✓ | ✓ | ✓ |
| 200, empty (unknown model, unknown entity, another tenant's entity) | | | ✓ | |
| 200, a FAILED item shows reason, error, times, attempts | | | ✓ | ✓ |
| an internal error in `lastError` is "internal error [ticket]" only | ✓ | | ✓ | |
| 400 unknown `status` | ✓ | | ✓ | |
| 400 `modelVersion` without `modelName` | ✓ | | ✓ | |
| 400 `modelVersion` not an integer ≥ 1 | ✓ | | ✓ | |
| 400 `entityId` not a UUID | | | ✓ | |
| 400 `modelName` empty or too long | ✓ | | ✓ | |
| 400 `limit` 0, 1001, not an integer | ✓ | | ✓ | |
| 400 invalid cursor (the value is not echoed) | ✓ | | ✓ | |
| 401 no token | | | ✓ | |
| 401 invalid token | | | ✓ | |
| 500 SERVER_ERROR with a ticket (store double) | ✓ | | ✓ | |
| 503 STORAGE_UNAVAILABLE (store double) | ✓ | | ✓ | |
| another tenant's tasks are never returned, under every filter | | ✓ | ✓ | ✓ |

**gRPC (waived).** The query has no gRPC door (§8), and no other gRPC
behaviour changes except the delete doors. Those are covered through the shared
functions above, and in `internal/grpc` by one test per delete door asserting
that the tasks are removed.

### Configuration

| Scenario | U |
|---|---|
| each new variable: its default and its validation failure | ✓ |
| each removed variable is no longer read (exit checks, §15) | ✓ |

## 14. Dependencies and scope

- **#599 lands before or with #598** (§5.4). It needs the v0.9.0 milestone;
  today it has none.
- **Not included:**
  - an API to retry or dismiss a FAILED task — an entity write does both;
  - notifications — §5.7 names the future emission point;
  - reclaiming a pnode that hangs but still heartbeats;
  - #600.

## 15. Exit checks

Each returns nothing, in the root module, `plugins/*` and the SPI, excluding
`docs/plans/`, `docs/superpowers/`, `docs/release-notes/` and `CHANGELOG.md`:

```
grep -rn "RedispatchAfter\|MarkRedispatch\|AttemptCount\|ScanDue" --include='*.go' .
grep -rn "LowestLiveNodeID\|scheduler\.RoundRobin\|SchedulerRPC\|ClusterExecutor\|dispatch/scheduled-task" --include='*.go' .
grep -rn "CYODA_SCHEDULER_DISTRIBUTION\|CYODA_SCHEDULER_COORDINATOR\|CYODA_SCHEDULER_REDISPATCH_BACKOFF\|CYODA_SCHEDULER_BATCH_SIZE\|CYODA_SCHEDULER_EXPIRY_GRACE\|CYODA_DISPATCH_FORWARD_TIMEOUT" . --exclude-dir=docs --exclude=CHANGELOG.md
grep -rn "expiryGrace\|WithExpiryGrace" --include='*.go' .
```

`internal/grpc/selector.go` has its own `RoundRobin` (the compute-member
selector), which stays. That is why the check is qualified.

## 16. Verification points (resolved during planning, before code)

- **V1.** Can the memory commit refuse on the claim check in its validation
  step, before applying anything?
- **V2.** Does any path other than those in §5.2 and §7 write or remove a task?
  Check every caller of the store.
- **V3.** A compute-node callback joined to a scheduled run's transaction
  updates or deletes the fired entity. What does an ordinary (non-scheduled)
  transition do in the same case? The scheduled run must behave the same way.
  - With §5.6 the case is never stuck: the run's own re-arm or delete makes
    `Complete` refuse, the rollback restores the task, `RecordAttempt` is
    accepted, and the task retries until `timeoutMs`.
  - Decide whether that is right, or whether the case is refused as an
    unsupported pattern.
- **V4.** Can the multi-node fixture kill one pnode without a graceful shutdown
  and restart it? If not, the fixture is extended.
- **V5.** The existing scheduled e2e and parity tests that rely on removed
  behaviour (the grace band, the throttle, round-robin) are rewritten against
  this spec, not deleted. `fire_scheduled_concurrency_test.go` (the dual
  coordinator cases) becomes a claim-race test.
- **V6.** Can the new shutdown order live in `runServers`? The signal path runs
  the scheduler drain first; the server-failure path runs it last.
