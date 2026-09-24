# #598 — every scheduled run has one owner

Status: spec v3, 2026-09-24. This version incorporates two independent spec
reviews. Milestone v0.9.0. Branch `feat/598-scheduler-ownership`.

Inputs:
- Research: `docs/superpowers/research/2026-09-24-598-scheduler-ownership-research.md` ("R§n").
- Design brief and its review: `docs/superpowers/research/2026-09-24-598-design-brief.md`.
- Product-owner rulings are marked **[ruling]**.

Path conventions: `SPI/` is `cyoda-go-spi`. `help/` is
`cmd/cyoda/help/content/`. Every other path is in this repository.

## 1. Summary

**Ownership.** At any moment at most one pnode **claims** a scheduled
transition's task, and the pnode that claims it runs it. Every write the owner
makes carries the claim's token, and the store refuses a token that is not
current. Each pnode proves it is alive with one heartbeat record. Another pnode
may claim a task only when the owner has stopped heartbeating.

**Repeats.** The platform never repeats a processor that is not declared
`idempotent`. Before such a processor is dispatched, the owner writes a mark on
the task. A task that carries the mark and did not commit becomes **FAILED** and
is never run again. A run that panics is also FAILED.

**Retries.** A failure where nothing unsafe was handed off is retried, with a
growing delay, until the transition's own `timeoutMs` passes **[ruling]**. A task
that loses its owner `CYODA_SCHEDULER_MAX_LOST_OWNERS` (3) times becomes FAILED
**[ruling]**.

**Visibility.** A FAILED task never moves the entity **[ruling]**. It is kept,
and it is visible through `GET /scheduled-tasks`, metrics, logs and an audit
event on the entity.

**Removed:**
- the scanning coordinator;
- the round-robin distribution;
- the scheduler RPC;
- the redispatch throttle;
- the expiry grace band.

## 2. Acceptance (issue #598)

The guarantees hold **per life** of a task (§3). An entity write that re-arms a
task starts a new life. From that moment the old run cannot commit, write a
mark or record an outcome. If a callout is already in flight, however, the old
run is not stopped. So when the application re-arms a task while its run is
calling an unsafe processor, the new life may call that processor again. That
repeat is caused by the application's write, not by the platform
**[ruling]**. The issue's acceptance text will be reworded to say "per life".

| # | Acceptance | Met by |
|---|---|---|
| A1 | No run of a task's life starts while another run of the same life is in progress on a live pnode | §6.1 claim and first-heartbeat rule; §6.2 liveness; §6.3 self-cancel; §6.4 no give-back of a live run; §6.1 registration order |
| A2 | A processor not declared safe to repeat is never executed again on the platform's initiative | §5.5 mark; §5.4 claim check on every commit; §10.1 life check on every fenced write; §10.2 mark serialised with the claim |
| A3 | A task that cannot succeed reaches a recorded, visible terminal state | §5.7 FAILED; §8 query; §9 telemetry. **[ruling]** A safe failure with no `timeoutMs` retries without end. It stays visible (status, attempts, last error) but never reaches a terminal state. The acceptance text will be reworded to say so. |
| A4 | The pnode that decides learns the outcome; an empty or partial cluster view is safe; runs in progress are bounded | §6.1 claim-and-run with no delegation and no cluster view; `MAX_RUNS` and the per-tenant cap |
| A5 | Cross-backend parity and multi-node coverage | §13 |

## 3. Terms

- **Task**: the stored record "fire transition T of entity E at time X". Its id
  is a hash of (tenant, entity, source state, transition)
  (`internal/domain/workflow/arm.go:27-30`), so the same id comes back every
  time the entity returns to that state.
- **Arm**: create or replace a task. Every arm draws a new random **arm token**
  and starts a new **life**.
- **Claim**: a pnode takes a task in order to run it. Every claim draws a new
  random **claim token**. Tokens are UUIDs and are never reused.
- **Owner**: the pnode incarnation that holds a claim.
- **Pnode incarnation**: a random UUID drawn when a pnode process starts.
- **Run**: one attempt to fire a task, from the claim to the recorded outcome.
- **Hand-off**: the moment a callout reaches a compute node (`member.Send`,
  `internal/grpc/dispatch.go:189`).
- **Unsafe processor**: a processor whose `config.idempotent` is not true
  (`SPI/types.go:244-251`).
- **Criteria and functions** are always repeat-safe
  (`internal/grpc/callout.go:191, 263`). A criterion or function whose
  callbacks trigger unsafe work is an unsupported pattern, and the
  documentation says so (§12).
- **Store clock**: the database clock (PostgreSQL `now()`), or the injected
  clock of the memory and SQLite stores.

## 4. Task statuses and endings

```
          arm (entity write) — new life
               │
               ▼
   ┌──────► WAITING ◄───────────────┐
   │           │ claim              │ safe failure / given back
   │           ▼                    │
   │        RUNNING ────────────────┘
   │           ├── fired / declined / expired / cancelled ──► task removed (+ audit)
   │           └── any FAILED reason ────────────────────────► FAILED (kept)
   └── entity write in the source state (from any status)
```

| Ending | Task afterwards | Audit on the entity | Log |
|---|---|---|---|
| Fired | removed; re-armed if the entity is back in the source state | `SCHEDULED_TRANSITION_FIRE` | DEBUG |
| Criterion false | removed | `TRANSITION_NOT_MATCH_CRITERION` | DEBUG |
| Late on the first attempt, no lost owner | removed | `SCHEDULED_TRANSITION_EXPIRE` | INFO |
| Transition gone from the workflow; entity has no transaction id | removed | `SCHEDULED_TRANSITION_CANCEL` | as today |
| Entity gone, or moved on | removed | none (as today) | DEBUG |
| Safe failure (§5.6) | WAITING, attempts + 1, next-attempt time | none | WARN |
| Unsafe work handed off, run not committed | FAILED `UNSAFE_WORK_NOT_COMPLETED` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Owner lost `MAX_LOST_OWNERS` times | FAILED `OWNER_LOST_REPEATEDLY` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Late after a failed attempt or a lost owner | FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| The run panicked | FAILED `RUN_PANICKED` | `SCHEDULED_TRANSITION_FAIL` | ERROR with ticket |

A FAILED task is never claimed. Only an entity write or a workflow import ends
it (§7):
- the entity leaves the state → removed, with `SCHEDULED_TRANSITION_CANCEL`;
- the entity is written in the state → a new life;
- the transition is no longer a scheduled transition of that state → removed
  at the entity's next write, or at the workflow import;
- the entity is deleted → removed.

## 5. The run

### 5.1 Before the run

After it claims a task (§6.1), the owner decides from the claimed record, in
this order:

1. **Mark.** The mark is set for this life → FAILED
   `UNSAFE_WORK_NOT_COMPLETED` (§5.7).
2. **Lost owners.** `lostOwners ≥ MAX_LOST_OWNERS` → FAILED
   `OWNER_LOST_REPEATEDLY`.
3. **Deadline.** When `timeoutMs` is set, `deadline = scheduledTime +
   timeoutMs`, and the task is **late** when:
   - it has never failed and never lost an owner (`attempts == 0 &&
     lostOwners == 0`), and `now > deadline` — today's rule, on the owner's
     clock; or
   - otherwise, `now > deadline + RETRY_DELAY`. A retry scheduled at or before
     the deadline therefore still runs when it is claimed up to one retry delay
     late, and never later than that.

   A late task that never failed and never lost an owner is expired: it is
   removed in a transaction with `SCHEDULED_TRANSITION_EXPIRE`, as today, but
   now fenced. Any other late task is FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`.
4. Otherwise, **run** (§5.2).

**A pnode crash counts as a lost owner.** So when a pnode dies, each task it was
running whose `timeoutMs` is shorter than `STALE_AFTER` ends as
`EXPIRED_AFTER_FAILED_ATTEMPTS`. This is intended: the failure stays visible.

**The grace band is deleted.** The 100 ms band (`fire_scheduled.go:50-63,
276-295`) and `CYODA_SCHEDULER_EXPIRY_GRACE` existed only so that two pnodes
judging the same task at once could not decide differently.

### 5.2 The run's transaction

`FireScheduledTransition(ctx, task, claim)` keeps its structure
(`internal/domain/workflow/fire_scheduled.go:89-524`). These are the changes:

- **Re-read.** The in-transaction re-read is `Get(tenant, id)`. If the task is
  gone, or its arm token or claim token no longer matches this claim, the run
  ends with outcome `superseded` and still records it (§5.6).
- **Deleted**, because the claim covers them:
  - the pre-transaction origin read (the claimed record carries `ArmedBy`);
  - the `ArmedBy` verify-or-abort (`:215-217`);
  - the re-armed-into-the-future guard (`:250-255`);
  - the tenant-mismatch guard (`:195-203`), which existed only for the
    scheduler RPC;
  - the grace-band drop.
- **Every removal of the task** on the fire path (`Delete(id)` today) becomes
  `Complete(tenant, id, armToken, claimToken)` inside the transaction (§5.4).
- **Re-arm at the end** (`reconcileScheduledTasks`, `:487`) runs with this task
  excluded, as today. The fire path completes its own task **before** it
  re-arms. So a transition or cascade that returns to the source state arms the
  same id as a new life, after the old life is completed (C3).

### 5.3 The run's context and cancellation

- **Context.** The run executes on a context derived from the scheduler's run
  context, not from `context.Background()` (`internal/scheduler/executor.go:44-45`).
  The system identity is attached to it the way `common.SystemUserContext`
  builds it today.
- **Run guard.** The context carries a run guard: tenant, task id, arm token,
  claim token, and the store.
- **Cancelled when:** the shutdown drain ends (§6.4), or the pnode cancels
  itself (§6.3).
- **Cancellation cuts in-flight callouts.** The answer wait ends on context
  cancellation (`internal/grpc/dispatch.go:194-200, 270-277`). A run whose
  unsafe callout is cut therefore holds a mark and becomes FAILED. The shutdown
  drain (§6.4) is the window in which such callouts can still finish.
- **Segments after a `COMMIT_BEFORE_DISPATCH` commit** begin with
  `context.WithoutCancel(ctx)` (`engine_processors.go:388, 403, 530`), so that
  no commit is ever cut off halfway. When a run guard is present, those
  segments must still stop:
  - The engine checks the run's cancellation at every step boundary: before
    each processor dispatch, before each cascade step, and before the final
    persist. A cancelled run stops at that point, with the cancellation as its
    error.
  - The dispatch context of a `startNewTxOnDispatch=false` callout (`:388`)
    carries the run's cancellation, like every other callout of the run.
  - Commits keep `WithoutCancel`.

### 5.4 Every commit checks the claim

- **The final commit.** `Complete(tenant, id, armToken, claimToken)` is staged
  in the run's transaction. The transaction commits only if the claim is still
  current. The task's life is then removed, unless this same transaction has
  already ended it (C5, §10.1). Otherwise the commit fails and the run is
  `superseded`.
- **Intermediate commits.** A `COMMIT_BEFORE_DISPATCH` processor commits TX_pre
  in the middle of a run (`engine_processors.go:469-515`). When a run guard is
  present, `flushAndCommitSegment` first stages `CheckClaim(tenant, id,
  armToken, claimToken)`. A superseded owner therefore cannot commit TX_pre.
- **Dependency on #599.** A compute-node callback that joins the run's
  transaction can reach a `COMMIT_BEFORE_DISPATCH` processor and commit the
  run's transaction without this check. A2 is complete only once #599 refuses
  that case. **#599 lands before, or together with, #598.**

### 5.5 The unsafe mark

In `executeProcessors`, when a run guard is present and the processor is
unsafe:

1. **Before dispatch.** At every call site of `extProc.DispatchProcessor`
   (`engine_processors.go:230, 269, 360, 392`), unless this run already holds a
   mark, the engine calls `MarkUnsafe(tenant, id, armToken, claimToken)`:
   - **accepted** → dispatch;
   - **`ErrStaleClaim`** → do not dispatch; the run is `superseded`;
   - **`ErrMarkedByAnotherClaim`** → do not dispatch; FAILED
     `UNSAFE_WORK_NOT_COMPLETED` under this claim. An earlier owner of this life
     marked it, so its outcome is unknown.
   - **any other error** → do not dispatch; call `ClearUnsafe`:
     - if `ClearUnsafe` is accepted, this is a safe failure (§5.6);
     - if `ClearUnsafe` also fails, the run is treated as holding a mark
       (FAILED). A mark may have been written, and failing the task is closed.
2. **After a failed dispatch.** The engine checks whether the error proves that
   nothing was handed off. It is proof only when:
   - the error is a `*contract.CalloutFailure` (`internal/contract/callout.go:77-95`)
     whose `Kind` is `NoHandOff` and whose `Attempts` are all `NoHandOff`; or
   - the error is `contract.ErrNoMatchingMember` with no attempts.

   If there is proof, and the mark was written for this processor (no earlier
   unsafe processor in the run was handed off), the engine calls `ClearUnsafe`.
   A `Terminal` failure is not proof, even before sending. That is the safe
   side, and it is intended.
3. **Otherwise the mark stays.** The task then becomes FAILED and is not
   repeated.

Scope and coverage:
- **Per life.** The mark belongs to the life (the arm token). Every later
  claim of the same life sees it. A re-arm starts a new life without a mark.
- **Hand-over.** The pnode-to-pnode protocol does not change. The owner marks
  before it dispatches, so a #254 hand-over to a peer pnode is covered.
- **Callbacks.** Callbacks inside an unsafe processor are covered by its mark.
  Callbacks inside an `idempotent` processor are covered by the author's
  declaration (`SPI/types.go:244-251`).
- **`ASYNC_NEW_TX`** processors (`:269`) are marked the same way. Their own
  failure does not fail the run (`:175-183`). If the run then commits, the task
  is completed. If the run fails later, the task is FAILED.

**Effect on today's commonest failure.** An unsafe processor followed by a
failing re-arm `schedule.function` (`executor.go:49-55`) now ends FAILED.
Today it repeats the unsafe processor every 30 s. The release notes say so.

### 5.6 After the run: the outcome is always recorded

A run whose transaction did not commit always ends with a fenced bookkeeping
write. No path skips it.
- **Transient store errors** are retried (1 s, doubling, up to
  `HEARTBEAT_INTERVAL`) until the write is accepted or refused.
- **At shutdown** the retries stop at the deadline of §6.4 step 3.
- **A refusal** (`ErrStaleClaim`) means the run was superseded, and nothing
  more is done.

| The run | Bookkeeping |
|---|---|
| holds a mark | `Fail(UNSAFE_WORK_NOT_COMPLETED)` (§5.7) |
| panicked (§6.5) | `Fail(RUN_PANICKED)` |
| ended by the shutdown drain, no mark | `RecordAttempt{NotCounted: true, NextAttemptTime: now}` |
| self-cancelled or failed with an error, no mark | `RecordAttempt{Error, NextAttemptTime}` (safe failure) |
| superseded | the same write as its cause would give; the store refuses it |

`RecordAttempt` sets the task to WAITING, adds 1 to `attempts` unless
`NotCounted`, records the error (§5.8), and clears the claim.

```
delay = RETRY_DELAY × 2^(attempts−1), saturating at RETRY_DELAY_MAX   // attempts after +1
next  = now + delay
if timeoutMs set: next = min(next, deadline)
```

- **Past the deadline.** If the deadline has already passed when the attempt
  is recorded, the task becomes FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`
  instead.
- **No overflow.** The multiplication saturates, so it never overflows.
- **Log line.** The WARN line of a safe failure is written only after
  `RecordAttempt` is accepted.
- **Declines are not failures.** A criterion that evaluates to false declines
  the task, as today.

### 5.7 FAILED

`Fail(tenant, id, armToken, claimToken, reason, error, atMs)` and the
`SCHEDULED_TRANSITION_FAIL` audit event are written together, in one small
transaction that commits.
- **Audit data:** `{transition, sourceState, reason, attempts, lostOwners}`.
- **Kept on the task:** status, reason, error and failed time.
- **Future notification.** The commit is identified uniquely and stably by
  (task id, arm token). It is where the planned notification feature can later
  publish "timer failed".

### 5.8 The recorded error (Gate 3)

The error is shown to tenant users (§8). It is filtered through an
**allow-list**, not through a classifier with a catch-all.
`classifyWorkflowError` is unsuitable: its catch-all is a 400 that carries
`err.Error()` (`internal/domain/entity/service.go:2872`), and the fire path
wraps store errors in plain `fmt.Errorf` (`fire_scheduled.go:107, 118, 168,
234`). What is shown:

- A `*contract.CalloutFailure` of kind `MemberFailed` → the compute node's own
  message, which is tenant-owned (`internal/contract/callout.go:84`).
- A `*common.AppError` found with `errors.As` whose status is below 500 → its
  code and message.
- `contract.ErrNoMatchingMember` → its code `NO_COMPUTE_MEMBER_FOR_TAG` and
  message.
- Anything else → `internal error [ticket: <uuid>]`. The full error is logged
  at ERROR under that ticket.

The text is truncated to 1 024 bytes.

## 6. The scheduler service

`internal/scheduler` is rewritten as one loop per pnode, plus a heartbeat
goroutine and a liveness watchdog.

### 6.1 Claiming

**When the loop may claim:**
- only after its first `Heartbeat` has succeeded;
- after a self-cancel, only once heartbeats succeed again (§6.3);
- a **lost-owner claim** — taking a task whose owner stopped heartbeating —
  only after this pnode's own heartbeats have succeeded without a gap for at
  least `STALE_AFTER`. After a database outage every pnode's heartbeat is
  stale at once. This rule stops the first pnode back from taking every other
  pnode's tasks and counting them as lost owners.
- never while the node is latched unhealthy (§6.5).

**When it wakes:**
- every `CYODA_SCHEDULER_SCAN_INTERVAL` (1 s);
- when a run slot frees, if the previous claim filled every free slot.

**The claim.** `ClaimDue(ClaimRequest{Owner, NowMs: clock.Now(), StaleAfter,
Limit: free slots, PerTenantLimit, AllowLostOwner})`.
- Free slots = `CYODA_SCHEDULER_MAX_RUNS` (8) minus the runs in progress. With
  no free slot, the loop does not call.
- `PerTenantLimit` = `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (default 4) minus
  that tenant's runs in progress on this pnode. The store applies it per
  tenant in the same statement.
- Within the limit, tasks are taken in turn across tenants, ordered by each
  tenant's `nextAttemptTime`. One tenant's backlog cannot fill every slot.

**A task is claimable when:**
- it is WAITING and `nextAttemptTime ≤ NowMs`, on the pnode clock — the clock
  that set `scheduledTime` at arm; or
- `AllowLostOwner` is set, and the task is RUNNING and its owner's liveness
  record is missing or older than `StaleAfter` by the store clock. That claim
  adds 1 to `lostOwners` in the same step.

**In addition**, no other task of the same entity may be RUNNING. Tasks of one
entity are therefore run one at a time. That removes the races between sibling
tasks: two runs writing the same entity, and one sibling removing another's
record inside a transaction.

A claimed task becomes RUNNING, with a new claim token and this owner.

**Registration order (A1).** The loop keeps the set of its live runs. A claimed
task enters the set before the claim call returns to the loop. A run leaves the
set only after its bookkeeping is accepted or refused. `ClaimDue` and
`GiveBackIdle` are called only from the loop goroutine, so they never run at
the same time.

**Self-heal.** On every tick, `GiveBackIdle(owner, keep = live claim tokens)`
gives back every task this incarnation holds RUNNING whose claim token is not
in the set. It is not counted. A mark belongs to the life, so a given-back task
that carries a mark still becomes FAILED on its next claim.

### 6.2 Liveness

- **Heartbeat.** A dedicated goroutine calls `Heartbeat(incarnation)` every
  `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (15 s), from start to stop, runs or no
  runs. The time is stamped with the store clock.
- **Stale period.** `CYODA_SCHEDULER_STALE_AFTER` (1 min) must be ≥ 4 × the
  interval. Startup fails otherwise, as it does for
  `CYODA_SEARCH_JOB_STALE_AFTER` (`app/config.go:861-886`).
- **A slow run keeps its task.** So does a run that hangs while its pnode
  heartbeats: liveness is not progress, the same position as #509.
- **Dead incarnations.** The claim loop removes their liveness records once a
  minute, after 10 × `STALE_AFTER` without a heartbeat, once no task
  references them.

### 6.3 Self-cancel

A **watchdog** goroutine, separate from the heartbeat so that a hung heartbeat
cannot block it, reads the time at which the last successful heartbeat was
**sent**, on the monotonic clock. If that time is more than `STALE_AFTER −
HEARTBEAT_INTERVAL` ago, the pnode:
- cancels every run in progress (outcome `self_cancelled`);
- stops claiming until a heartbeat succeeds again.

With clocks running at the same rate, this happens before any other pnode may
take the tasks. A frozen VM whose monotonic clock does not advance is covered
for unsafe work by the mark (C4).

### 6.4 Shutdown

Today `cmd/cyoda/run.go` drains the HTTP, admin and gRPC servers concurrently in
an errgroup once the context ends (`:117-171`), and stops the scheduler only
afterwards, in `a.Shutdown()`. The new order **on a signal** is:

1. The scheduler stops claiming.
2. It waits up to `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (20 s) for the runs in
   progress. Compute-node streams and callback routes are still open during the
   wait.
3. It cancels the runs still in progress (§5.3) and waits, for up to 10 s more,
   until each has recorded its outcome (§5.6). A cut unsafe callout ends as
   FAILED.
4. `GiveBackIdle(owner, keep = ∅)` hands back any claim whose bookkeeping did
   not reach the store within step 3. Then `RetireOwner(owner)`.
5. The server drains start, as today.

When the group ends because a server failed rather than on a signal, the same
sequence runs from `a.Shutdown()`, after the servers, as a best effort.

**Worst case and grace period.** Signal to exit is about 20 + 10 + 25 s (the
existing tail, `help/run.md:287`), about 55 s. The Helm chart sets
`terminationGracePeriodSeconds: 60`. If SIGKILL arrives before step 4, the
tasks stay RUNNING and are reclaimed after `STALE_AFTER` as lost owners. That
is correct, because they were not handed back.

### 6.5 Panics

Every new goroutine recovers panics and latches the node unhealthy. That is the
existing latch (`docs/ARCHITECTURE.md:382-393`). It is permanent and needs an
operator to replace the node.

- **A panicking run** records `Fail(RUN_PANICKED)` with a ticket and does not
  retry. A panic means state that nothing has verified. Running the task again
  on another pnode would spread it.
- **A latched pnode stops claiming.** It keeps heartbeating, so its runs still
  in progress are not taken over. It gives back nothing it is not running.
  Today peer work still reaches a latched node, but the scheduler is now fully
  local, so it can stop.
- **A panicking claim loop, heartbeat or watchdog** latches the node. The
  others stop claiming, and the runs cancel themselves through the watchdog
  (§6.3).

### 6.6 Deleted

- `internal/scheduler/coordinator.go` and `distribution.go`.
- `ClusterExecutor`, and `internal/cluster/scheduler_rpc.go` with the
  `/internal/dispatch/scheduled-task` route and `SchedulerRPCClient`.
- `internal/cluster/config.go:25-27` (`DispatchForwardTimeout`).
- `Config.RedispatchBackoff` and `BatchSize`.
- Their wiring (`app/app.go:600-657, 810-827`) and their tests, including
  `app/config_dispatch_test.go` and the forward-timeout rows of
  `app/config_registry_binding_test.go`.

## 7. Entity writes, workflow imports and tasks

- **Arm** (`reconcileScheduledTasks` → `ReconcileForEntity`). Every armed task
  starts a new life: WAITING, `nextAttemptTime = scheduledTime`, attempts and
  lostOwners 0, errors cleared, a new arm token, no claim. This holds whatever
  the old life's status was. A RUNNING owner is fenced out. A FAILED task is
  replaced.
- **Cancel.** Reconcile removes **every task of the entity that is not in the
  new arm set**, not only the tasks of other states. That includes a task of
  the same state whose transition is no longer scheduled. Each removal records
  `SCHEDULED_TRANSITION_CANCEL`, except the firing task (as today).
- **Which writes reconcile.**
  - Today reconcile returns at once when the selected workflow has no
    scheduled transition (`arm.go:96-98`). Now it returns at once only when
    **no workflow of the entity's model** has a scheduled transition. That
    flag is computed when workflows are loaded, so writes to models without
    schedules still cost nothing extra.
  - **Workflow import** removes, in its own transaction, the model's tasks
    whose (source state, transition) is no longer a scheduled transition in
    any of the model's workflows (`internal/domain/workflow/handler.go:174`
    and its service). This covers a model that drops its schedules
    altogether.
- **Entity delete removes the entity's tasks, in the same transaction, on every
  path:**
  - `Handler.DeleteEntity` (`internal/domain/entity/service.go:656`) →
    `DeleteForEntities(tenant, [id])`;
  - `Handler.DeleteEntitiesConditional` (`:1205`), in its single-transaction
    loop (`:1314`) and in each batch of `deleteBatched` (`:1697`) →
    `DeleteForEntities(tenant, ids)`;
  - `Handler.DeleteAllEntities` (`:778`), the fast path that lists no ids and
    is also reached from the conditional path (`:1230`) →
    `DeleteForModel(tenant, model, version)`.

  The gRPC doors reach the same functions (`internal/grpc/entity.go:200,
  484`). No audit event is written.
- **Not fenced.** Arm, cancel, the import cleanup and the `DeleteFor*` removals
  are not fenced by a claim. They are the application's decisions, and they
  fence any owner out.

## 8. The task query — `GET /scheduled-tasks`

**HTTP only.** It is an operator's view that no compute node needs. The audit
trail has no gRPC door for the same reason. The coverage matrix records the
waiver (§13).

**Access.** Any authenticated user of the tenant, as for the audit trail
(`internal/domain/audit/handler.go` checks no role). The tenant comes from the
token.

**Parameters** (all optional):

| Name | Type | Rule |
|---|---|---|
| `status` | repeatable; `WAITING`, `RUNNING`, `FAILED` | unknown → 400 |
| `modelName` | string, 1–256 characters | |
| `modelVersion` | integer ≥ 1 | only with `modelName`; alone → 400 |
| `entityId` | UUID | not a UUID → 400 |
| `cursor` | opaque string, max 256 | invalid → 400; the value is not echoed |
| `limit` | integer 1–1000, default 20 | outside the range → 400 |

**`limit` out of range is rejected, not clamped.** That is fail-closed, as in
direct search, `INVALID_LIMIT` and the transaction window. The audit endpoint's
clamp (`audit/handler.go:59-61`) is the exception, and this endpoint does not
copy it.

**Order.** `(scheduledTime, taskId)` ascending. The cursor is versioned
base64url JSON of that position and is decoded strictly, like the audit cursor
(`internal/domain/audit/cursor.go:65-114`).

**Response 200:** `{ "items": [ScheduledTaskDto], "pagination": { "hasNext", "nextCursor" } }`.

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
| `nextAttemptTime` | date-time | WAITING |
| `lastAttemptTime`, `lastError` | date-time, string | after a failed attempt |
| `failureReason` | string (open) | FAILED |
| `failedTime` | date-time | FAILED |
| `armedTime` | date-time | always |
| `armedBy` | `{id, kind}` | when known |

Node ids, claim tokens and arm tokens are not returned.

**Error table:**

| Status | Code | When |
|---|---|---|
| 200 | — | success, including an empty list |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName`; `modelVersion` not an integer ≥ 1; `entityId` not a UUID; `modelName` empty or too long; `limit` not an integer or outside 1–1000; invalid `cursor` |
| 401 | `UNAUTHORIZED` | no token, or an invalid token |
| 500 | `SERVER_ERROR` | internal failure; generic message and a ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no 403 (no role is required) and no 404. An unknown model or entity,
including another tenant's, returns an empty list.

## 9. Telemetry and logs

Instruments come from `observability.Meter()` and carry no tenant attribute.

| Name | Type | Attributes |
|---|---|---|
| `cyoda.scheduler.runs` | counter | `outcome`: fired, declined, expired, cancelled, attempt_failed, failed, superseded, self_cancelled, shutdown_cancelled, panicked |
| `cyoda.scheduler.run.duration` | histogram, s | `outcome` |
| `cyoda.scheduler.runs.in_progress` | up-down counter | — |
| `cyoda.scheduler.claims` | counter | `reason`: due, owner_lost |
| `cyoda.scheduler.heartbeat.failures` | counter | — |

- **Span:** `scheduler.run`, with its outcome.
- **Logs:**
  - WARN per safe failure;
  - ERROR per FAILED task, with its reason and ticket;
  - WARN per self-cancel;
  - ERROR per panic.

All of this is documented in `help/telemetry.md`.

## 10. Storage contract (SPI)

### 10.1 `ScheduledTaskStore`

```go
type ScheduledTaskStatus string        // "WAITING" | "RUNNING" | "FAILED"
type ScheduledTaskFailureReason string // "UNSAFE_WORK_NOT_COMPLETED" | "OWNER_LOST_REPEATEDLY" | "EXPIRED_AFTER_FAILED_ATTEMPTS" | "RUN_PANICKED"

type ScheduledTask struct {
    // unchanged: ID, TenantID, Type, ScheduledTime, TimeoutMs, EntityID,
    // ModelName, ModelVersion, Transition, SourceState, ArmedAt, ArmedBy
    Status          ScheduledTaskStatus
    ArmToken        uuid.UUID // drawn by the store on every arm
    NextAttemptTime int64     // unix ms; = ScheduledTime on arm
    Attempts        int
    LostOwners      int
    LastAttemptTime *int64
    LastError       string
    FailureReason   ScheduledTaskFailureReason
    FailedTime      *int64
    Claim           *TaskClaim // RUNNING only
    UnsafeMarked    bool       // read-only: a mark exists for this life
}
type TaskClaim struct{ Token, Owner uuid.UUID }
```

**Removed:** `RedispatchAfter`, `AttemptCount`, `Upsert`, `ScanDue`,
`MarkRedispatch`, `Delete`.

**The fence.** A method marked *fenced* is accepted only if the task's current
life is `armToken` **and** that life's claim is `claimToken`. Otherwise it
returns `spi.ErrStaleClaim`. A missing task is also stale. So a re-arm makes
every fenced write of the old life fail.

| Method | Transaction | Contract |
|---|---|---|
| `ReconcileForEntity(req)` | joins the entity tx | arms `req.Arm` (each a new life); removes every other task of the entity; returns the removed tasks |
| `DeleteForEntities(tenant, ids)` | joins | removes every task of those entities |
| `DeleteForModel(tenant, name, version, keep func)` | joins | removes the model's tasks; with `keep`, only those whose (state, transition) is not kept (import cleanup) |
| `Complete(tenant, id, armToken, claimToken)` | joins | fenced on the claim (C5); removes the life |
| `CheckClaim(tenant, id, armToken, claimToken)` | joins | fenced; fails the tx at commit if the claim has changed |
| `Fail(tenant, id, armToken, claimToken, reason, error, atMs)` | joins the §5.7 tx | fenced; FAILED; claim cleared |
| `Get(tenant, id)` | may join | tenant-scoped; `found=false` for another tenant's id |
| `Query(tenant, filter, cursor, limit)` | never joins | tenant-scoped page, in §8 order |
| `ClaimDue(req)` | never joins | atomic; disjoint across callers; cross-tenant; §6.1 |
| `Heartbeat(owner)` | never joins | upserts liveness, stamped with the store clock |
| `RetireOwner(owner)` | never joins | removes liveness |
| `SweepOwners(olderThan)` | never joins | removes the liveness records of dead incarnations that no task references |
| `GiveBackIdle(owner, keep)` | never joins | every task RUNNING under `owner` whose claim token is not in `keep` becomes WAITING; not counted |
| `MarkUnsafe(tenant, id, armToken, claimToken)` | never joins | fenced; serialised with `ClaimDue` (C4); idempotent for the same claim; a mark by another claim of the same life → `ErrMarkedByAnotherClaim` |
| `ClearUnsafe(tenant, id, armToken, claimToken)` | never joins | fenced; removes this claim's mark |
| `RecordAttempt(tenant, id, armToken, claimToken, Attempt)` | never joins | fenced; WAITING; claim cleared; `attempts+1` unless `NotCounted` |

"Never joins" means the method ignores any transaction on `ctx` and commits on
its own. That is what lets a mark written during a run survive the run's
rollback. Today the PostgreSQL store joins whatever transaction `ctx` carries
(`plugins/postgres/scheduled_task_store.go:12-20`), and so does the memory
`stage()`.

Errors:
- `spi.ErrStaleClaim` already exists; its doc comment (`SPI/errors.go:155`)
  widens to both stores.
- `spi.ErrMarkedByAnotherClaim` is new.

**Clauses:**
- **(C1)** The joining methods and the never-joining methods never update the
  same stored data. The one exception is a claim changing hands (claim,
  recorded outcome, give-back). That change must make a later `Complete`,
  `CheckClaim` or `Fail` under the old claim fail.
- **(C2)** Entity transactions must not starve heartbeats and claims of
  connections.
- **(C3)** Operations staged in one transaction apply, **and are checked**, in
  staging order. Each check sees the result of the operations staged before it
  in the same transaction. Example: `Complete(X)` followed by an arm of X
  leaves the new life.
- **(C4)** A `MarkUnsafe` and a `ClaimDue` that race on one task serialise:
  either the mark is refused, or the claim returns `UnsafeMarked`.
- **(C5)** `Complete` is accepted when the claim is current. If the life was
  already ended or replaced **by operations staged earlier in the same
  transaction** (a joined callback re-armed or deleted the entity), `Complete`
  succeeds and leaves them in place. If it was ended or replaced by another
  transaction, `Complete` fails. This settles V3 of spec v2: a callback that
  updates or deletes the fired entity inside the run does what it would do in
  an ordinary transition. The run is not failed for it.

The conformance cases move from `SPI/scheduled_task_store_conformance.go` into
`SPI/spitest`, next to async search. They cover every method, every refusal and
every clause, including "a mark survives the rollback of the transaction on
`ctx`" and "after a re-arm, the old claim's `MarkUnsafe`, `RecordAttempt`,
`Fail`, `Complete` and `CheckClaim` are refused". A backend whose store answers
"not implemented" (the commercial backend today) skips them.

### 10.2 PostgreSQL

A new migration. There are no production instances, so existing columns are
simply replaced.

**Tables:**
- **`scheduled_tasks`** — the arm record, one row per task id.
  - Written only by joining transactions.
  - Today's columns, plus `arm_token uuid`, minus `redispatch_after` and
    `attempt_count`.
  - Indexes: `(tenant_id, scheduled_time, id)`, `(tenant_id, model_name,
    model_version)`, `(tenant_id, entity_id)` (kept);
    `scheduled_tasks_due_idx` is dropped.
- **`scheduled_task_runs`** — the run state of one life.
  - Columns: `(task_id, arm_token) PK, tenant_id, entity_id, status,
    claim_token, claim_owner, next_attempt_time, attempts, lost_owners,
    last_attempt_time, last_error, failure_reason, failed_time`.
  - The arming transaction inserts the row as a new key, so the insert never
    conflicts.
  - After that, only the never-joining methods and `Fail` write it, and they
    write it only when a claim changes hands. The owner never writes its own
    run row while its run is in progress, so the run never conflicts with
    itself.
  - Rows whose life has ended are swept in batches by the claim loop, once a
    minute. This costs one extra small row per arm, which is the price of C1.
  - Indexes: `(next_attempt_time) WHERE status='WAITING'`,
    `(claim_owner) WHERE status='RUNNING'`, `(entity_id) WHERE
    status='RUNNING'`.
- **`scheduled_task_marks`** — `(task_id, arm_token) PK, claim_token`.
- **`scheduler_owners`** — `(owner uuid PK, heartbeat_at timestamptz)`.

**The dedicated pool** (C2), `CYODA_POSTGRES_SCHEDULER_CONNS` (3):
- runs every never-joining method;
- sessions are pinned to READ COMMITTED, with `statement_timeout` 30 s and
  `idle_in_transaction_session_timeout` 10 s, so a pnode that freezes inside a
  short transaction cannot hold a lock for long;
- `Heartbeat` has its own extra connection with a 5 s acquire timeout. That
  connection is not counted in the pool size.
- The async-search heartbeat and claim (`plugins/postgres/search_store.go`)
  move to this pool too, because they face the same starvation risk on the
  main pool.

**`ClaimDue`** — one transaction on the dedicated pool, following the
async-search claim (`plugins/postgres/search_store.go:570-587`):
1. A CTE selects the claimable run rows, inner-joined with `scheduled_tasks` on
   `(task_id = id, arm_token)`, so an ended life is never claimed.
   - It keeps `NOT EXISTS` for a RUNNING row of the same `entity_id`.
   - It `LEFT JOIN`s `scheduler_owners` for staleness.
   - It ranks with `row_number() OVER (PARTITION BY tenant_id ORDER BY
     next_attempt_time)`, orders by (rank, `next_attempt_time`) and applies
     the tenant limit and `LIMIT $n`.
   - It locks with `FOR UPDATE OF r SKIP LOCKED`.
   - It is followed by `UPDATE … RETURNING`.
2. A **second statement**, after the row locks are held, reads the marks of
   the claimed rows. At READ COMMITTED it sees any mark committed before the
   locks were taken (C4).

**`MarkUnsafe`** — one short transaction on the dedicated pool:
1. `SELECT … FROM scheduled_tasks WHERE id, tenant_id, arm_token FOR SHARE`
   and `SELECT … FROM scheduled_task_runs WHERE task_id, arm_token,
   claim_token FOR SHARE`. If either finds nothing, the answer is
   `ErrStaleClaim`.
2. `INSERT … ON CONFLICT (task_id, arm_token) DO NOTHING RETURNING
   claim_token`. On conflict, a mark with the same claim token → accepted; a
   mark with another token → `ErrMarkedByAnotherClaim`.

The run-row lock serialises the mark with `ClaimDue`'s row lock. The arm-row
share lock waits for an arming transaction that is in flight and then
re-checks the life.

**`ClearUnsafe`, `RecordAttempt`, `GiveBackIdle`** — conditional statements on
the dedicated pool, joined to `scheduled_tasks` for the life check.

**`Complete`, `CheckClaim`, `Fail`** — these run inside a REPEATABLE READ
transaction.
- Each takes `SELECT … FROM scheduled_task_runs WHERE task_id, arm_token,
  claim_token FOR SHARE`.
- `Complete` then runs `DELETE FROM scheduled_tasks WHERE id, tenant_id,
  arm_token`. A row already ended or replaced by this same transaction matches
  nothing, which is accepted (C5). A row changed by another committed
  transaction raises a serialisation error.
- `Fail` then updates the run row.
- A serialisation error (SQLSTATE 40001) on these statements is mapped to
  `ErrStaleClaim`.

**No false conflicts.** Entity transactions write `scheduled_tasks` and insert
new run rows. Bookkeeping writes only run rows and marks. Siblings of one
entity never run at the same time (§6.1).

**Tenant scoping.**
- The tenant-facing methods filter on `tenant_id`: `Get`, `Query`, `Complete`,
  `CheckClaim`, `Fail`, `DeleteFor*`, `MarkUnsafe`, `ClearUnsafe`,
  `RecordAttempt`.
- The scheduler's own statements are cross-tenant: `ClaimDue`,
  `GiveBackIdle`, `Heartbeat`, `RetireOwner`, `SweepOwners` and the sweepers.
  No API reaches them.
- The tables stay out of row-level security. The migration comment that says
  otherwise (`000004_scheduled_tasks.up.sql:5-14`) is corrected.

### 10.3 Memory and SQLite

Both are single-pnode, and both meet the same contract. A backend that behaves
differently is a bug.

- **Same logical model.** The arm record and the run state of each life are
  kept apart, keyed as in PostgreSQL. The fence and the tests then behave the
  same way.
- **Staged operations** apply, and are checked, in staging order (C3):
  - **Memory:** each check is evaluated against committed state plus the
    operations staged before it, in the commit's validation step, under
    `entityMu`, before anything is applied (`plugins/memory/txmanager.go:530`).
  - **SQLite:** checks are statements inside the commit's `sqlTx`, in order.
    An error rolls the transaction back (`plugins/sqlite/txmanager.go:885-894`).
- **Never-joining methods** apply immediately and ignore any transaction on
  `ctx`:
  - memory, under `entityMu`;
  - SQLite, as conditional statements on the writer connection, following the
    async-search claim (`plugins/sqlite/search_store.go:404-496`).
- **Clock.** The store clock is the injected clock.
- **Removals.** `Delete` is gone, and with it the unreliable "was it removed?"
  answer (R§2.5 item 9).

## 11. Configuration (Gate 4)

| Variable | Default | Rule | Status |
|---|---|---|---|
| `CYODA_SCHEDULER_ENABLED` | true | | kept |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | 1s | > 0 | kept; now validated |
| `CYODA_SCHEDULER_MAX_RUNS` | 8 | ≥ 1 | new |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | 4 | 1 ≤ n ≤ MAX_RUNS | new |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | 15s | > 0 | new |
| `CYODA_SCHEDULER_STALE_AFTER` | 1m | ≥ 4 × heartbeat | new |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | 3 | ≥ 1 | new |
| `CYODA_SCHEDULER_RETRY_DELAY` | 30s | > 0 | new |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | 15m | ≥ retry delay | new |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | 20s | ≥ 0 | new |
| `CYODA_POSTGRES_SCHEDULER_CONNS` | 3 | ≥ 2 | new (postgres plugin) |
| `CYODA_SCHEDULER_DISTRIBUTION`, `CYODA_SCHEDULER_COORDINATOR`, `CYODA_SCHEDULER_REDISPATCH_BACKOFF`, `CYODA_SCHEDULER_BATCH_SIZE`, `CYODA_SCHEDULER_EXPIRY_GRACE` | — | | removed |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | — | | removed (its only user was the scheduler RPC) |

**Where each change is made:**
- `app/config.go`: `DefaultConfig`, `Validate`, `:468, :474-482, :945-946`.
- `cmd/cyoda/help/config_registry.go`: `:80, :131-137`.
- `help/config/scheduler.md`.
- `help/config/cluster.md` (the forward timeout).
- `help/config/database.md` (the pool variable).
- `help/config.md:33`.
- `README.md` (`:224-236, :251`).
- The postgres plugin: `plugins/postgres/config.go`, `plugin.go` and `doc.go`,
  and `docs/plugins/POSTGRES.md`.

Startup fails on an invalid value.

## 12. Documentation, parity and other repositories

- **`help/workflows.md`, "SCHEDULED TRANSITIONS":**
  - one owner per run;
  - retry until `timeoutMs`;
  - FAILED and its reasons;
  - the new audit event;
  - what an entity write or a workflow import does to a task;
  - siblings run one at a time;
  - a criterion or function that triggers unsafe work through its callbacks
    is unsupported.
- **`help/workflows.md:182` and the `idempotent` description in
  `api/openapi.yaml`:** the declaration now also decides whether a scheduled run
  may be repeated. No `WorkflowConfigurationDto` schema-version bump. The
  rationale is added to `docs/workflow-schema-versioning.md` as a "When NOT to
  bump" entry: the field's shape and its accepted values are unchanged, and the
  change makes the engine safer for an unchanged document. The v0.8.4
  evaluation-time entries are the precedent.
- **New help topic `scheduled-tasks`**, shaped like `audit.md`, added to
  `topLevelTopicsV061`.
- **`help/run.md` "SHUTDOWN TIMING"** is rewritten (§6.4).
- **`help/telemetry.md`**: the instruments of §9.
- **`help/helm.md`**: the grace period.
- **`api/openapi.yaml`:**
  - the operation and its DTOs;
  - `SCHEDULED_TRANSITION_FAIL` in the audit event enum (`:11742`);
  - `go generate ./api`.
- **SPI:**
  - `SMEventScheduledTransitionFailed`;
  - `ErrMarkedByAnotherClaim`;
  - the `ErrStaleClaim` doc comment;
  - the stale sentence in `TransitionSchedule`'s doc is removed.
- **Helm chart** (`deploy/helm/…`):
  - `terminationGracePeriodSeconds` in `templates/statefulset.yaml`,
    `values.yaml`, `values.schema.json` and the chart `README.md`;
  - a chart `version:` bump, which also means a `COMPATIBILITY.md` entry.
- **`docs/cloud-parity/scheduled-transitions.md`**, rewritten (Gate 7):
  - §1: the grace band is removed.
  - §5: any write resets the timer, FAILED included.
  - §6: the audit event set.
  - §7 and §8 are replaced by: one owner per life; unsafe work is never
    repeated; retry until `timeoutMs`; the FAILED reasons; the lost-owner cap;
    siblings one at a time; the query; entity delete and workflow import
    remove tasks.
  - A CaaS ticket is filed.
- **`docs/ARCHITECTURE.md`**, scheduler passages rewritten in the present
  tense: `:94, :230, :382-393, :772, :940, :943, :1359, :1486-1506,
  :1550-1553, :1992, :2248`.
- **`internal/domain/search/reaper.go:18`**: the comment that names `ScanDue`.
- **`CHANGELOG.md` `### Breaking`:**
  - the removed variables;
  - processors not declared `idempotent` are no longer repeated by scheduled
    runs;
  - the FAILED status;
  - siblings run one at a time;
  - the new query.
- **`COMPATIBILITY.md`**: the SPI pin bump and the chart bump.
- **SPI release, mid-milestone:** an SPI PR into `main`, pseudo-pinned by
  cyoda-go; no tag.
- **cyoda-go-cassandra#68**: updated to this contract.

## 13. Coverage matrix

**Layers:**
- **U**: unit.
- **S**: `spitest` conformance on memory, SQLite and PostgreSQL.
- **E**: `internal/e2e` on PostgreSQL through HTTP.
- **P**: a cross-backend parity scenario registered in
  `e2e/parity/registry.go`.
- **M**: multi-node PostgreSQL. The fixture already kills a node
  (`e2e/parity/postgres/async_node_crash_test.go:48-61`).

**Rules:**
- **Concurrency cases** (two pnodes, or races) stay out of **P**
  (`.claude/rules/test-coverage.md`).
- **Timing cases** that need no concurrency (retry, expiry) may be in **P**,
  like the existing `FiresOnTime` and `ExpiryElapsedExpiresNoFire`.
- **Fixtures.** The parity and multi-node fixtures set short
  `RETRY_DELAY`, `HEARTBEAT_INTERVAL` and `STALE_AFTER` in
  `e2e/parity/fixtureutil/tuned_env.go` (`:18`). The commercial backend's
  fixture inherits them.

### Endings

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| fired on time | ✓ | | ✓ | ✓ | |
| fired after one safe failure (compute node unavailable, then available) | ✓ | | ✓ | ✓ | |
| a scheduled self-loop fires and re-arms the same id as a new life | ✓ | ✓ | ✓ | ✓ | |
| declined | ✓ | | ✓ | ✓ | |
| expired (late on the first attempt) | ✓ | | ✓ | ✓ | |
| safe failure: criterion error → WAITING, attempts 1, error recorded | ✓ | | ✓ | ✓ | |
| safe failure: no compute node → mark cleared, WAITING | ✓ | | ✓ | ✓ | |
| safe failure: an idempotent processor fails → WAITING | ✓ | | ✓ | ✓ | |
| retry delay doubles, saturates and is clamped; a retry up to `RETRY_DELAY` past the deadline runs; later → FAILED | ✓ | | | | |
| late after failed attempts → FAILED | ✓ | | ✓ | ✓ | |
| an unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never run again | ✓ | | ✓ | ✓ | |
| a later step fails after an unsafe hand-off → FAILED | ✓ | | ✓ | ✓ | |
| `MarkUnsafe` transient error → not dispatched; cleared → WAITING; clear fails → FAILED | ✓ | | | | |
| `ErrMarkedByAnotherClaim` → FAILED under the current claim | ✓ | ✓ | | | |
| an unsafe `ASYNC_NEW_TX` processor fails and the run commits → completed | ✓ | | ✓ | | |
| CBD: TX_pre committed, later failure, all idempotent → retried from the TX_pre state | ✓ | | ✓ | | |
| CBD: an unsafe CBD processor, then a failure → FAILED | ✓ | | ✓ | | |
| CBD: cancellation after TX_pre stops the run at the next step | ✓ | | ✓ | | |
| a callback inside the run updates the fired entity → same outcome as an ordinary transition (C5) | ✓ | ✓ | ✓ | | |
| a callback inside the run deletes the fired entity → the run commits (C5) | ✓ | ✓ | ✓ | | |
| FAILED audit event recorded with its reason | ✓ | | ✓ | ✓ | |
| a panicking run → FAILED `RUN_PANICKED`, node latched, the latched node stops claiming | ✓ | | | | |
| `lastError` for a non-sentinel store error is "internal error [ticket]" only | ✓ | | ✓ | | |

### Ownership, fencing and liveness

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| concurrent `ClaimDue` calls get disjoint sets | | ✓ | | | ✓ |
| two tasks of one entity are never RUNNING at once | | ✓ | | | ✓ |
| per-tenant limit and turn-taking across tenants | | ✓ | | | |
| a run longer than 3 × the heartbeat interval is not claimed by another pnode | | | | | ✓ |
| owner killed without a mark → claimed after `STALE_AFTER`, `lostOwners` 1, fires | | | | | ✓ |
| owner killed with a mark → FAILED; the processor was sent once | | | | | ✓ |
| owner killed, `timeoutMs` < `STALE_AFTER` → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | | | | | ✓ |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | ✓ | ✓ | | | |
| database outage longer than `STALE_AFTER` → no lost-owner claims before one full stale period of healthy heartbeats | ✓ | | | | ✓ |
| a stale claim is refused by every fenced method | | ✓ | | | |
| after a re-arm the old claim's `MarkUnsafe`, `ClearUnsafe`, `RecordAttempt`, `Fail`, `Complete` and `CheckClaim` are refused | ✓ | ✓ | ✓ | | |
| `MarkUnsafe` racing `ClaimDue` (C4) | | ✓ | | | ✓ |
| a mark survives the rollback of the transaction on `ctx` | | ✓ | | | |
| a superseded owner sends no unsafe processor | ✓ | | ✓ | | |
| a superseded owner's CBD TX_pre is refused | ✓ | | ✓ | | |
| ABA: the old token is refused after the same id is re-armed and claimed again | ✓ | ✓ | | | |
| a retrying sibling never fails a run's commit or a client write (C1) | | ✓ | ✓ | | |
| heartbeat failure → self-cancel before `STALE_AFTER`; no claims until recovery | ✓ | | | | |
| a hung heartbeat does not stop the watchdog | ✓ | | | | |
| no claim before the first heartbeat | ✓ | | | | |
| heartbeats not starved with every main-pool connection in an entity tx (C2) | | | ✓ | | |
| a RUNNING task with no live run is given back (self-heal); a live run is never given back | ✓ | ✓ | | | |
| at most `MAX_RUNS` at once; the next claim waits for a slot | ✓ | | ✓ | | |
| a slot freed after a full claim → immediate claim | ✓ | | | | |
| an empty cluster view has no effect | ✓ | | | | ✓ |
| dead-incarnation liveness records and ended-life rows are swept | | ✓ | | | |

### Shutdown

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| runs finish within the drain; compute-node streams stay open during it | ✓ | | ✓ | | |
| a run cut after the drain, no mark → WAITING, not counted, claimed at once by another pnode | ✓ | | | | ✓ |
| a run cut after the drain, with a mark → FAILED | ✓ | | ✓ | | |
| no task is given back while its run is live (the post-CBD run ends first) | ✓ | | | | |
| bookkeeping during shutdown stops at its deadline | ✓ | | | | |
| `GiveBackIdle` not counted; `RetireOwner` removes liveness | | ✓ | | | |

### Entity writes and workflow import

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| a FAILED task is re-armed by an update in the state (new life, no mark) | ✓ | ✓ | ✓ | ✓ | |
| a FAILED task is cancelled when the entity leaves the state | ✓ | | ✓ | ✓ | |
| a task whose transition is no longer scheduled is removed at the next write | ✓ | ✓ | ✓ | ✓ | |
| a workflow import that drops schedules removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |
| deleting one entity removes its tasks (HTTP and gRPC) | ✓ | ✓ | ✓ | ✓ | |
| conditional delete removes tasks: single-tx loop, batched, fast path (HTTP and gRPC) | ✓ | ✓ | ✓ | ✓ | |
| delete-all removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |
| `DeleteForModel` in tenant A leaves tenant B's tasks of the same model | | ✓ | ✓ | | |

### Query (`GET /scheduled-tasks`)

| Scenario | U | S | E | P |
|---|---|---|---|---|
| 200, no filter, several pages | ✓ | ✓ | ✓ | ✓ |
| 200, each filter: status (one and several), model name, name and version, entity | ✓ | ✓ | ✓ | ✓ |
| 200, empty: unknown model, unknown entity, another tenant's entity | | | ✓ | |
| 200, a FAILED item shows its reason, error, times and attempts | | | ✓ | ✓ |
| 400 unknown `status` | ✓ | | ✓ | |
| 400 `modelVersion` without `modelName` | ✓ | | ✓ | |
| 400 `modelVersion` not an integer ≥ 1 | ✓ | | ✓ | |
| 400 `entityId` not a UUID | | | ✓ | |
| 400 `modelName` empty or too long | ✓ | | ✓ | |
| 400 `limit` 0, 1001, not an integer | ✓ | | ✓ | |
| 400 invalid cursor (value not echoed) | ✓ | | ✓ | |
| 401 no token | | | ✓ | |
| 401 invalid token | | | ✓ | |
| 500 SERVER_ERROR with a ticket (store double) | ✓ | | ✓ | |
| 503 STORAGE_UNAVAILABLE (store double) | ✓ | | ✓ | |
| another tenant's tasks never returned, under any filter | | ✓ | ✓ | ✓ |

**gRPC is waived.** The query has no gRPC door (§8). The only gRPC behaviour
that changes is at the delete doors. `internal/grpc` gets one test per delete
door asserting that the tasks are removed.

### Configuration

| Scenario | U |
|---|---|
| each new variable: its default and its validation failure | ✓ |
| each removed variable is no longer read (exit checks, §15) | ✓ |

## 14. Dependencies and scope

- **#599 lands before, or together with, #598** (§5.4). It needs the v0.9.0
  milestone.
- **Not included:**
  - an API to retry or dismiss a FAILED task (an entity write does both);
  - notifications (§5.7 names the emission point);
  - reclaiming a pnode that hangs but still heartbeats;
  - #600.

## 15. Exit checks

Each check below must return nothing in the root module, in `plugins/*` and in
the SPI. The searches exclude `docs/plans/`, `docs/superpowers/`,
`docs/release-notes/` and `CHANGELOG.md`.

```
grep -rn "RedispatchAfter\|MarkRedispatch\|AttemptCount\|ScanDue" --include='*.go' .
grep -rn "LowestLiveNodeID\|scheduler\.RoundRobin\|SchedulerRPC\|ClusterExecutor\|dispatch/scheduled-task\|DispatchForwardTimeout" --include='*.go' .
grep -rn "CYODA_SCHEDULER_DISTRIBUTION\|CYODA_SCHEDULER_COORDINATOR\|CYODA_SCHEDULER_REDISPATCH_BACKOFF\|CYODA_SCHEDULER_BATCH_SIZE\|CYODA_SCHEDULER_EXPIRY_GRACE\|CYODA_DISPATCH_FORWARD_TIMEOUT" . \
  --exclude-dir=plans --exclude-dir=superpowers --exclude-dir=release-notes --exclude=CHANGELOG.md
grep -rn "expiryGrace\|WithExpiryGrace" --include='*.go' .
```

The second check is qualified with `scheduler.` because
`internal/grpc/selector.go` has its own `RoundRobin` (the compute-member
selector), which stays.

## 16. Verification points (settled during planning, before any code)

**Settled:**
- **V2.** The store's only callers are `arm.go:165`, `fire_scheduled.go:101,
  152`, `scheduler/service.go:161`, and the pass-through at
  `modelcache/factory.go:76`.
- **V3.** Settled by C5.
- **V4.** The multi-node fixture can kill a node
  (`e2e/parity/postgres/async_node_crash_test.go:48-61`).

**Open:**
- **V1.** Memory's validation step can evaluate each check against committed
  state plus the operations staged before it, and refuse before anything is
  applied.
- **V5.** The existing scheduled e2e and parity tests that rely on removed
  behaviour are rewritten against this spec, not deleted.
  `fire_scheduled_concurrency_test.go` becomes a claim-race test.
- **V6.** `runServers` can hold the new order: the scheduler drain first on the
  signal path, and last on the server-failure path.
- **V7.** The engine can compute "does any workflow of this model have a
  scheduled transition" where it resolves the workflow, without an extra store
  read per write.
