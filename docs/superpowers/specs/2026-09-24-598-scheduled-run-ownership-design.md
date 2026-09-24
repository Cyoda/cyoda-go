# #598 — every scheduled run has one owner

Status: spec, 2026-09-24. Milestone v0.9.0. Branch `feat/598-scheduler-ownership`.

Inputs:
- Research: `docs/superpowers/research/2026-09-24-598-scheduler-ownership-research.md` ("R§n").
- Design brief and its independent review: `docs/superpowers/research/2026-09-24-598-design-brief.md`.
- Product-owner rulings, marked **[ruling]**.

`SPI/` is `cyoda-go-spi`. Paths without a prefix are in this repository.

## 1. Summary

A scheduled transition's task is **claimed** by exactly one pnode at a time.
The pnode that claims a task runs it. Every write the owner makes carries the
claim's token, and the store refuses a token that is not current. Each pnode
proves it is alive with one heartbeat record. A task whose owner stopped
heartbeating may be claimed by another pnode.

The platform never repeats a processor that is not declared `idempotent`. Before
such a processor is handed to a compute node, the owner writes a mark. A task
that carries the mark and did not commit is **FAILED** and is never run again.

A failure where nothing unsafe was handed off is retried, with a growing delay,
until the transition's own `timeoutMs` passes **[ruling]**. A task whose owner
dies repeatedly is FAILED after `CYODA_SCHEDULER_MAX_LOST_OWNERS` (3)
**[ruling]**.

A FAILED task never moves the entity **[ruling]**. It is kept, and is visible
through `GET /scheduled-tasks`, metrics, logs and an audit event on the entity.

The scanning coordinator, the round-robin distribution, the scheduler RPC, the
redispatch throttle and the expiry grace band are deleted.

## 2. Acceptance (issue #598) and where each point is met

| # | Acceptance | Met by |
|---|---|---|
| A1 | No run starts while another run of the same task is in progress on a live pnode | §5.2 claim; §6.2 liveness; §6.3 self-cancel |
| A2 | A processor not declared safe to repeat is never executed again on the platform's initiative | §5.5 unsafe mark; §5.4 claim check on every commit |
| A3 | A task that cannot succeed reaches a recorded, visible terminal state | §5.7 FAILED; §8 query; §9 telemetry. **[ruling]** A safe failure with no `timeoutMs` retries without end. It is visible in the query (status, attempts, last error), but not terminal. The issue's acceptance text will be reworded to say so. |
| A4 | The deciding pnode learns the outcome; an empty or partial cluster view is safe; runs in progress are bounded | §6.1: the pnode that claims runs the task, there is no delegation, no cluster view is read, and at most `MAX_RUNS` runs are in progress |
| A5 | Cross-backend parity and multi-node coverage | §13 |

## 3. Terms

- **Task**: the stored row "fire transition T of entity E at time X". Its id is
  a hash of (tenant, entity, source state, transition) (`internal/domain/workflow/arm.go:27-30`),
  so the same id comes back each time the entity returns to that state.
- **Arm**: create or replace a task. Every arm gets a new random **arm token**.
  A task's "life" runs from one arm to the next.
- **Claim**: a pnode taking a task to run it. Every claim gets a new random
  **claim token**. Tokens are UUIDs and are never reused. That makes an old
  token unable to match a later claim, even across lives of the same task id.
- **Owner**: the pnode incarnation that holds a claim.
- **Pnode incarnation**: a random UUID a pnode process draws when it starts. A
  restarted pnode is a new incarnation.
- **Run**: one attempt to fire a task, from claim to commit or failure.
- **Hand-off**: the moment a callout is given to a compute node
  (`member.Send`, `internal/grpc/dispatch.go:189`).
- **Unsafe processor**: a processor whose `config.idempotent` is not true
  (`SPI/types.go:244-251`). Criteria and functions are never unsafe.
- **Store clock**: the database's clock (PostgreSQL `now()`), or the store's
  injected clock on memory and SQLite.

## 4. Task statuses and endings

```
          arm (entity write)
               │
               ▼
   ┌──────► WAITING ◄───────────────┐
   │           │ claim (due, or     │ attempt failed (safe) / given back
   │           │ owner lost)        │
   │           ▼                    │
   │        RUNNING ────────────────┘
   │           │
   │           ├── fired / declined / expired / cancelled ──► row removed (+ audit)
   │           └── cannot be repeated / lost too often /
   │               expired after failed attempts ──────────► FAILED (kept)
   │
   └── re-arm by an entity write (from any status, including FAILED)
```

| Ending | Task afterwards | Audit on the entity | Log |
|---|---|---|---|
| Fired | removed, or re-armed if the entity is back in the source state | `SCHEDULED_TRANSITION_FIRE` | DEBUG |
| Criterion false | removed | `TRANSITION_NOT_MATCH_CRITERION` | DEBUG |
| Too late, no failed attempt and no lost owner | removed | `SCHEDULED_TRANSITION_EXPIRE` | INFO |
| Transition gone from the workflow; entity carries no transaction id | removed | `SCHEDULED_TRANSITION_CANCEL` | as today |
| Entity gone, or moved on | removed | none (as today) | DEBUG |
| Safe failure (§5.6) | WAITING, attempts + 1, next-attempt time set | none | WARN |
| Unsafe work handed off, run not committed | FAILED `UNSAFE_WORK_NOT_COMPLETED` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Owner lost `MAX_LOST_OWNERS` times | FAILED `OWNER_LOST_REPEATEDLY` | `SCHEDULED_TRANSITION_FAIL` | ERROR |
| Too late after a failed attempt or a lost owner | FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | `SCHEDULED_TRANSITION_FAIL` | ERROR |

A FAILED task is never claimed. It ends only through an entity write (§7):
- the entity leaves the state: the task is removed, with `SCHEDULED_TRANSITION_CANCEL`;
- the entity is written in the state: the task is re-armed as a new life;
- the entity is deleted: the task is removed.

## 5. The run

### 5.1 Before the run

After it claims a task (§6.1), the owner decides in this order, from the claimed
row alone:

1. The unsafe mark is set for this life → FAILED `UNSAFE_WORK_NOT_COMPLETED` (§5.7).
2. `lostOwners ≥ MAX_LOST_OWNERS` → FAILED `OWNER_LOST_REPEATEDLY`.
3. Lateness. `lateness = now − scheduledTime`, measured by the owner's clock.
   `timeoutMs` is set and `lateness > timeoutMs`:
   - `attempts == 0 && lostOwners == 0` → expired. The row is removed in a
     transaction with `SCHEDULED_TRANSITION_EXPIRE`. This is the existing
     expiry, now fenced.
   - otherwise → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`.
4. Otherwise, run (§5.2).

A pnode crash counts as a lost owner. So when a pnode dies, every task it was
running whose `timeoutMs` is shorter than `STALE_AFTER` ends as
`EXPIRED_AFTER_FAILED_ATTEMPTS`. That is intended: the timer did not fire as the
author meant, and the failure stays visible.

The 100 ms grace band (`fire_scheduled.go:50-63, 276-295`) and
`CYODA_SCHEDULER_EXPIRY_GRACE` are deleted. They existed only so that two pnodes
judging the same task at the same moment could not decide differently. Only
one owner now decides, and only a decision under a current claim token can
commit.

### 5.2 The run's transaction

`FireScheduledTransition(ctx, task, claim)` keeps its structure
(`internal/domain/workflow/fire_scheduled.go:89-524`), with these changes:

- The in-transaction re-read is `Get(tenant, id)`. If the row is gone, or its
  claim token is no longer this claim's, the run stops. It writes no
  bookkeeping, because the task has been re-armed, re-claimed or removed by
  someone else. Outcome `superseded`.
- The following are **deleted**, because the claim check above covers them:
  - the pre-transaction read that seeds the origin — the claimed row carries
    `ArmedBy`, so the seed comes from it;
  - the `ArmedBy` verify-or-abort (`:215-217`);
  - the re-armed-into-the-future guard (`:250-255`);
  - the tenant-mismatch guard (`:195-203`) — it existed only for the scheduler
    RPC, which is deleted;
  - the grace-band drop.
- Every place that removed the task with `Delete(id)` now calls
  `Complete(tenant, id, claimToken)` inside the transaction (§5.4).
- The re-arm step at the end (`reconcileScheduledTasks`, `:487`) is called with
  this task excluded, as today. The fire path completes its own task
  **before** calling it. So when the transition or cascade ends back in the
  source state (a scheduled self-loop, or a guarded cascade back to it), the
  same id is re-armed as a new life after the old one is completed. Stores
  apply staged operations in order (§10.3).

### 5.3 Where the run's context comes from

The run executes on a context derived from the scheduler's run context. That
context is cancelled when:
- the pnode stops (§6.4);
- the pnode fences itself (§6.3).

It carries a **run guard**: tenant, task id, arm token, claim token, and access
to the store. It no longer derives from `context.Background()`
(`internal/scheduler/executor.go:44-45`). The system identity is still built the
way `common.SystemUserContext` builds it, attached to that derived context.

### 5.4 Every commit checks the claim

- **The final commit.** `Complete(tenant, id, claimToken)` is staged in the
  run's transaction and removes the row only if the claim token is current.
  Otherwise the commit fails and the run is `superseded`.
- **Intermediate commits.** A `COMMIT_BEFORE_DISPATCH` processor commits
  TX_pre in the middle of a run (`engine_processors.go:469-515`). When the
  context carries a run guard, `flushAndCommitSegment` first stages
  `CheckClaim(tenant, id, claimToken)` in the segment's transaction. A
  superseded owner therefore cannot commit TX_pre data.
- **What this depends on.** A compute-node callback that joins the run's
  transaction can reach a `COMMIT_BEFORE_DISPATCH` processor and commit the
  run's transaction without this check (#599). A2 and the claim check are
  complete only once #599 refuses that case. **#599 lands before or with
  #598** (§14).

### 5.5 The unsafe mark

In `executeProcessors`, when the context carries a run guard and the processor
is unsafe:

1. **Before** the processor is dispatched (every call site of
   `extProc.DispatchProcessor`: `engine_processors.go:230, 269, 360, 392`), the
   engine calls `MarkUnsafe(tenant, id, armToken, claimToken)`. The call is
   made unless this run already holds a mark. If the store refuses it (claim
   not current), the processor is not dispatched and the run is `superseded`.
2. **After** the dispatch fails, the engine checks whether the failure proves
   that nothing was handed off. It is proof only when the error is a
   `*contract.CalloutFailure` (`internal/contract/callout.go:77-95`) whose
   `Kind` is `NoHandOff` and whose `Attempts` are all `NoHandOff`, or the
   error is `ErrNoMatchingMember` with no attempts. If there is proof, and the
   mark was written for this processor (no earlier unsafe processor in the run
   was handed off), the engine calls `ClearUnsafe(tenant, id, armToken,
   claimToken)`.
3. In every other case the mark stays. That includes the case where
   `ClearUnsafe` fails, or the pnode dies before it runs. The task then
   becomes FAILED rather than being repeated.

The mark belongs to the task's life (the arm token), not to one claim. Every
later claim of the same life sees it. A re-arm starts a new life without a
mark.

No change to the pnode-to-pnode protocol. A #254 hand-over to a peer pnode is
covered, because the owner writes the mark before it dispatches.

Callbacks that run other workflows inside an unsafe processor are covered by
that processor's mark. Callbacks inside a processor declared `idempotent` are
covered by its declaration, which the engine takes on trust
(`SPI/types.go:244-251`).

`ASYNC_NEW_TX` processors are dispatched the same way (`:269`) and are marked
the same way. Their own failure is not fatal to the run (`:175-183`), but a
failed unsafe `ASYNC_NEW_TX` processor that was handed off leaves the mark set.
If the run then commits, the task is completed and the mark goes with it. If
the run fails later, the task is FAILED.

### 5.6 A run that fails (safe failure)

A run fails with an error, and its transaction is rolled back. The owner then
decides from what it knows in memory:

- The run holds a mark → FAILED `UNSAFE_WORK_NOT_COMPLETED` (§5.7).
- The run was `superseded` → nothing is written.
- Otherwise it is a safe failure: `RecordAttempt(tenant, id, claimToken,
  Attempt{Error, NextAttemptTime})`. The task goes back to WAITING,
  `attempts + 1`, the error is recorded (§5.8), and the claim is cleared.

Next-attempt time:
```
delay = min(RETRY_DELAY × 2^(attempts−1), RETRY_DELAY_MAX)   // attempts after +1
next  = now + delay
if timeoutMs set: next = min(next, scheduledTime + timeoutMs)
```
If the deadline (`scheduledTime + timeoutMs`) has already passed when the
attempt is recorded, the task is FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` at once
instead. So the last attempt is made at the deadline, and a failure after that
is final.

A criterion that evaluates to false is not a failure. It declines the task, as
today.

### 5.7 FAILED

`Fail(tenant, id, claimToken, reason, error)` and the
`SCHEDULED_TRANSITION_FAIL` audit event are written in one small transaction
that commits. The audit event's data is `{transition, sourceState, reason,
attempts, lostOwners}`. The row is kept with its status, reason, error and
failed time. The row stays in place, and the commit identifies it uniquely and
permanently by (task id, arm token). That commit is the point from which the
planned notification feature can publish a "timer failed" event later.

### 5.8 The recorded error (Gate 3)

The error is shown to tenant users (§8), so it follows the 4xx/5xx rule:

- A `*contract.CalloutFailure` for `MemberFailed`: the compute node's own
  message, which is tenant-owned (`callout.go:82`).
- Any other error that classifies as a 4xx `*common.AppError`: its code and
  message.
- Anything else (storage, engine, a 5xx): `internal error [ticket: <uuid>]`.
  The full error is logged at ERROR under that ticket.
- The text is truncated to 1 024 bytes.

## 6. The scheduler service

`internal/scheduler` is rewritten around one loop per pnode.

### 6.1 Claiming

- The loop wakes every `CYODA_SCHEDULER_SCAN_INTERVAL` (1 s).
- It also wakes when a run slot frees, if the previous claim filled every
  free slot. So throughput is bounded by run time, not by the scan interval.
- On each wake it calls:
  ```
  ClaimDue(ClaimRequest{Owner: incarnation, NowMs: clock.Now(), StaleAfter, Limit: free slots})
  ```
- Free slots = `CYODA_SCHEDULER_MAX_RUNS` (8) minus runs in progress. With
  none free, it does not call.
- Every claimed task is run on its own goroutine: §5.1 decision, then the run.

`ClaimDue` is one atomic store operation (§10.1). A task is claimable when:
- it is WAITING and `nextAttemptTime ≤ NowMs` (pnode clock, the same clock
  domain that set `scheduledTime` at arm); or
- it is RUNNING and its owner's liveness record is missing, or older than
  `StaleAfter` by the store clock. Such a claim adds 1 to `lostOwners`, in the
  same step.

The claimed row gets status RUNNING, a new claim token and the owner. Claims
are ordered by `nextAttemptTime`.

### 6.2 Liveness

- Each pnode writes `Heartbeat(incarnation)` every
  `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (15 s), from start to stop, whether or
  not it has runs. The store stamps the time with the store clock.
- `CYODA_SCHEDULER_STALE_AFTER` (1 min) must be ≥ 4 × the interval. Startup
  fails otherwise, as `CYODA_SEARCH_JOB_STALE_AFTER` does
  (`app/config.go:861-886`).
- A slow run keeps its task. A run that hangs while its pnode heartbeats also
  keeps its task: liveness is not progress, the same position as #509. Every
  callout in a run is bounded (R§2.7).

### 6.3 Self-cancel when the heartbeat fails

The pnode records, on its monotonic clock, when it **sent** its last heartbeat
that succeeded. If that moment is more than `STALE_AFTER − HEARTBEAT_INTERVAL`
ago:
- the pnode cancels every run in progress (outcome `self_cancelled`);
- it makes no claims until a heartbeat succeeds again.

This happens before any other pnode may take its tasks, provided clocks run at
the same rate. The cancelled runs then try to record their attempt, fenced. If
the tasks were taken meanwhile, the store refuses, and nothing is lost.

The store must not let entity transactions starve heartbeats and claims of
connections. That is a contract requirement (§10.1). On PostgreSQL it is met by
a small dedicated pool for scheduler bookkeeping (§10.2).

### 6.4 Shutdown

New order in `cmd/cyoda/run.go`, on a signal, before gRPC and HTTP stop:

1. The scheduler stops claiming.
2. It waits up to `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (20 s) for runs in
   progress. Compute-node streams and callback routes are still open during
   this wait.
3. It cancels the runs still in progress. Each records its outcome as in §5.6,
   fenced: a run that holds a mark becomes FAILED, and a safe run becomes
   WAITING with no attempt counted.
4. `GiveBack(incarnation)`: every task still RUNNING under this incarnation
   becomes WAITING. `lostOwners` is not counted, and `nextAttemptTime` is left
   unchanged, so the task can be claimed at once.
5. `RetireOwner(incarnation)` removes the liveness record.
6. The existing sequence continues: gRPC, then HTTP, then `a.Shutdown()`.

A cancelled safe run is recorded with `Attempt{NotCounted: true}`. The error
is recorded, `attempts` is not increased, and the next attempt time is now.

The Helm chart's `terminationGracePeriodSeconds` must exceed this drain plus
the existing drains (verification V6).

### 6.5 What is deleted in `internal/scheduler` and `internal/cluster`

- `coordinator.go`, `distribution.go`, `ClusterExecutor` and
  `internal/cluster/scheduler_rpc.go`, with the `/internal/dispatch/scheduled-task`
  route and `SchedulerRPCClient`.
- `Config.RedispatchBackoff` and `BatchSize`.
- Their wiring in `app/app.go:600-657, 810-827`, and their tests.

## 7. Entity writes and tasks

- **Arm** (`reconcileScheduledTasks` → `ReconcileForEntity`):
  - An armed task always starts a new life: WAITING, `nextAttemptTime =
    scheduledTime`, attempts and lostOwners 0, errors cleared, a new arm
    token, no claim.
  - This applies whatever the old row's status was. A RUNNING owner is fenced
    out, because its claim token is gone. A FAILED task is replaced. If the
    unsafe processor then runs again, it is because the application wrote the
    entity.
- **Cancel on leaving the state**: as today. It removes the row whatever its
  status, and records `SCHEDULED_TRANSITION_CANCEL`.
- **Entity delete removes the entity's tasks, in the same transaction, on every
  path:**
  - `DeleteEntity` (`internal/domain/entity/service.go:656`) →
    `DeleteForEntities(tenant, [id])`;
  - `DeleteEntitiesConditional` (`:1205`), per batch →
    `DeleteForEntities(tenant, ids)`;
  - `DeleteAllEntities` (`:778`), the fast path that lists no ids →
    `DeleteForModel(tenant, model, version)`.

  The gRPC doors reach the same functions (`internal/grpc/entity.go:200, 484`).
  No audit event: the entity's history ends with its deletion.

## 8. The task query — `GET /scheduled-tasks`

HTTP only. The query is an operator's view that no compute node needs, the same
reasoning as the audit trail, which has no gRPC door. The coverage matrix
records this as a waiver (§13).

**Access.** Any authenticated user of the tenant, as for the audit trail
(`internal/domain/audit/handler.go` checks no role). The tenant comes from the
token and never from a parameter.

**Parameters** (all optional):

| Name | Type | Rule |
|---|---|---|
| `status` | repeatable; `WAITING`, `RUNNING`, `FAILED` | an unknown value → 400 |
| `modelName` | string, 1–256 | |
| `modelVersion` | integer ≥ 1 | only with `modelName`; alone → 400 |
| `entityId` | UUID | not a UUID → 400 |
| `transition` | string, 1–256 | |
| `cursor` | opaque string, max 256 | invalid → 400, value not echoed |
| `limit` | integer 1–1000, default 20 | outside the range → 400 (rejected, not clamped: fail closed, as direct search and `INVALID_LIMIT` do) |

**Order**: `(scheduledTime, taskId)` ascending, a total order. The cursor is
versioned base64url JSON encoding that position. It is decoded strictly, like
the audit cursor (`internal/domain/audit/cursor.go:65-114`).

**Response 200**:
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

No node ids, claim tokens or arm tokens are returned; they are internal.

**Error table:**

| Status | Code | When |
|---|---|---|
| 200 | — | success, including an empty list |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName`; `modelVersion` not an integer ≥ 1; `entityId` not a UUID; `modelName`/`transition` empty or too long; `limit` outside 1–1000 or not an integer; invalid `cursor` |
| 401 | `UNAUTHORIZED` | no or invalid token |
| 500 | `SERVER_ERROR` | internal failure; generic message and ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no 403 (no role is required) and no 404: an unknown model or entity
gives an empty list. That includes another tenant's entity, so the query cannot
reveal whether another tenant's data exists.

## 9. Telemetry and logs

Instruments are created on `observability.Meter()`. They carry no tenant
attribute.

| Name | Type | Attributes | Meaning |
|---|---|---|---|
| `cyoda.scheduler.runs` | counter | `outcome`: fired, declined, expired, cancelled, attempt_failed, failed, superseded, self_cancelled, given_back | one per ended run or decision |
| `cyoda.scheduler.run.duration` | histogram, s | `outcome` | claim to end |
| `cyoda.scheduler.runs.in_progress` | up-down counter | — | |
| `cyoda.scheduler.claims` | counter | `reason`: due, owner_lost | |
| `cyoda.scheduler.heartbeat.failures` | counter | — | |

- **Span:** `scheduler.run`, with `outcome`, recording errors as other spans do.
- **Logs:**
  - WARN per safe failure, with task id, entity id, transition, attempt and next attempt time;
  - ERROR per FAILED task, with the reason and the ticket when there is one;
  - WARN per self-cancel.

All are documented in `help/content/telemetry.md`.

## 10. Storage contract (SPI)

### 10.1 `ScheduledTaskStore`

```go
type ScheduledTaskStatus string // "WAITING" | "RUNNING" | "FAILED"
type ScheduledTaskFailureReason string // "UNSAFE_WORK_NOT_COMPLETED" | "OWNER_LOST_REPEATEDLY" | "EXPIRED_AFTER_FAILED_ATTEMPTS"

type ScheduledTask struct {
    // unchanged: ID, TenantID, Type, ScheduledTime, TimeoutMs, EntityID,
    // ModelName, ModelVersion, Transition, SourceState, ArmedAt, ArmedBy
    Status          ScheduledTaskStatus
    ArmToken        uuid.UUID  // new on every arm; set by the store
    NextAttemptTime int64      // unix ms; = ScheduledTime on arm
    Attempts        int
    LostOwners      int
    LastAttemptTime *int64
    LastError       string
    FailureReason   ScheduledTaskFailureReason
    FailedTime      *int64
    Claim           *TaskClaim // RUNNING only
    UnsafeMarked    bool       // read-only: a mark exists for this ArmToken
}
type TaskClaim struct{ Token, Owner uuid.UUID }
```

Removed: `RedispatchAfter`, `AttemptCount`, `ScanDue`, `MarkRedispatch`,
`Delete`.

| Method | In the entity tx? | Contract |
|---|---|---|
| `ReconcileForEntity(req)` | yes | as today; every armed task starts a new life (§7) |
| `Complete(tenant, id, claimToken)` | yes | removes the row if its claim token is current; else `ErrStaleClaim` when the transaction commits, or earlier |
| `CheckClaim(tenant, id, claimToken)` | yes | fails the transaction with `ErrStaleClaim` if the claim is not current when it commits |
| `DeleteForEntities(tenant, ids)` | yes | removes every task of those entities |
| `DeleteForModel(tenant, name, version)` | yes | removes every task of that model version |
| `Get(tenant, id)` | may join | tenant-scoped; `found=false` for another tenant's id |
| `Query(tenant, filter, cursor, limit)` | no | tenant-scoped page in the order of §8 |
| `ClaimDue(req)` | no | atomic; disjoint across concurrent callers; cross-tenant; §6.1 |
| `Heartbeat(owner)` | no | upsert liveness, store clock |
| `RetireOwner(owner)` | no | remove liveness |
| `GiveBack(owner)` | no | every RUNNING task of `owner` becomes WAITING; not counted |
| `MarkUnsafe(tenant, id, armToken, claimToken)` | no | records the mark if the claim is current; else `ErrStaleClaim`; idempotent |
| `ClearUnsafe(tenant, id, armToken, claimToken)` | no | removes the mark if the claim is current; else `ErrStaleClaim` |
| `RecordAttempt(tenant, id, claimToken, Attempt)` | no | fenced; WAITING, claim cleared; `attempts+1` unless `NotCounted` |
| `Fail(tenant, id, claimToken, reason, error, atMs)` | may join | fenced; FAILED, claim cleared |

Refusals return `spi.ErrStaleClaim`, which already exists for async search
(`SPI/errors.go`). A missing row is also refused as stale.

The contract also requires:
- **(C1)** Nothing a pnode writes while a run is in progress — heartbeat, mark,
  clear — may be a row that the run's transaction or an entity write
  transaction updates or deletes. Otherwise a snapshot-isolation store fails
  those transactions (R§3).
- **(C2)** `Heartbeat` and `ClaimDue` must not be starved of connections by
  entity transactions.
- **(C3)** Staged operations in one transaction apply in the order they were
  staged. `Complete(X)` followed by an arm of X leaves the new life.

The conformance cases move from `SPI/scheduled_task_store_conformance.go` into
`SPI/spitest`, next to async search, and cover every method, refusal and
contract clause. A backend that returns "not implemented" for the store (the
commercial backend today) skips them.

### 10.2 PostgreSQL

New migration. There are no production instances, so the existing columns are
simply replaced.

- `scheduled_tasks`:
  - add `status`, `arm_token`, `next_attempt_time`, `attempts`, `lost_owners`,
    `last_attempt_time`, `last_error`, `failure_reason`, `failed_time`,
    `claim_token`, `claim_owner`;
  - drop `redispatch_after`, `attempt_count`;
  - indexes:
    - `(next_attempt_time) WHERE status='WAITING'`
    - `(claim_owner) WHERE status='RUNNING'`
    - `(tenant_id, scheduled_time, id)`
    - `(tenant_id, model_name, model_version)`
    - drop `scheduled_tasks_due_idx`.
- `scheduler_owners(owner uuid PK, heartbeat_at timestamptz)`.
- `scheduled_task_marks(task_id text, arm_token uuid, PRIMARY KEY (task_id, arm_token))`.
  This table is written only outside entity transactions (C1).
  - `MarkUnsafe` inserts in one statement guarded by `EXISTS (SELECT 1 FROM
    scheduled_tasks WHERE id=$1 AND tenant_id=$2 AND claim_token=$3)`.
  - Rows whose `(task_id, arm_token)` no longer matches a task are swept in
    batches by the claim loop, at most once a minute.
- `ClaimDue`: one statement, with a CTE that selects claimable rows
  `FOR UPDATE OF t SKIP LOCKED`, `LEFT JOIN scheduler_owners` for staleness
  (`now() - $stale`), `EXISTS` on marks for `unsafe_marked`, `ORDER BY
  next_attempt_time LIMIT $n`, then `UPDATE … RETURNING`. It follows the
  async-search claim (`plugins/postgres/search_store.go:570-587`).
- `Complete` and `CheckClaim` inside the entity transaction (REPEATABLE READ):
  - `DELETE … WHERE id AND tenant_id AND claim_token`, and
    `SELECT … FOR SHARE` for the check.
  - A re-claim or re-arm committed after the transaction began makes them
    fail with a serialization error. It is mapped to `ErrStaleClaim` for this
    table's statements.
  - A row found without a match is also `ErrStaleClaim`.
- Scheduler bookkeeping (`ClaimDue`, `Heartbeat`, `GiveBack`, `RetireOwner`,
  marks, `RecordAttempt`) runs on a dedicated pool of
  `CYODA_POSTGRES_SCHEDULER_CONNS` (2) connections (C2). `Fail` joins the
  small bookkeeping transaction of §5.7, on the main pool.
- Every statement except `ClaimDue` filters on `tenant_id`. `ClaimDue` is the
  only cross-tenant statement, as `ScanDue` is today. The table stays out of
  row-level security (`000004_scheduled_tasks.up.sql:5-14`), and the migration
  comment is corrected.

### 10.3 Memory and SQLite

Both are single-pnode, but they must satisfy the same contract; a backend that
differs is a bug.

- **Staged operations**, `Complete`, `CheckClaim`, `DeleteFor*` and arms, are
  applied in staging order.
  - **Memory:** the claim-token checks run in the commit's validation step,
    under `entityMu` and before anything is applied
    (`plugins/memory/txmanager.go:530`). **To verify (V1).**
  - **SQLite:** the checks are statements inside the commit's `sqlTx`; an
    error rolls it back (`plugins/sqlite/txmanager.go:885-894`).
- **Non-transactional methods:**
  - Memory: under `entityMu`.
  - SQLite: single conditional statements on the writer connection. The
    claim is a read, then a conditional update per row, as in the async-search
    claim (`plugins/sqlite/search_store.go:404-496`).
- **The store clock** is the injected clock.
- **The "was it removed?" defect** (R§2.5 item 9) disappears, because `Delete`
  is removed. Every remaining removal is fenced, and each reports refusal
  through `ErrStaleClaim`.

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
| `CYODA_POSTGRES_SCHEDULER_CONNS` | 2 | ≥ 1 | new (postgres plugin) |
| `CYODA_SCHEDULER_DISTRIBUTION` | — | | removed |
| `CYODA_SCHEDULER_COORDINATOR` | — | | removed |
| `CYODA_SCHEDULER_REDISPATCH_BACKOFF` | — | | removed |
| `CYODA_SCHEDULER_BATCH_SIZE` | — | | removed |
| `CYODA_SCHEDULER_EXPIRY_GRACE` | — | | removed |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | — | | removed (its only user was the scheduler RPC) |

Each change is made in `app/config.go` (`DefaultConfig`, `Validate`),
`cmd/cyoda/help/config_registry.go`, `help/content/config/scheduler.md` (and
`cluster.md` for the removed variable), `README.md`, and the Helm chart values.
Startup fails on an invalid value.

## 12. Documentation, parity and other repositories

- **`help/content/workflows.md`, "SCHEDULED TRANSITIONS"**:
  - one owner per run;
  - retry until `timeoutMs`;
  - FAILED and its reasons;
  - the new audit event;
  - what an entity write does to a FAILED task.
- **`help/content/workflows.md:182` and the `idempotent` description in
  `api/openapi.yaml`**: the declaration now also governs whether a scheduled
  run may be repeated.
- **New topic `scheduled-tasks`** for the endpoint, in the shape of `audit.md`,
  added to `topLevelTopicsV061`.
- **`api/openapi.yaml`:**
  - the new operation and DTOs;
  - `SCHEDULED_TRANSITION_FAIL` added to the audit event enum (`:11742`);
  - `go generate ./api`.
- **SPI:**
  - `SMEventScheduledTransitionFailed = "SCHEDULED_TRANSITION_FAIL"`;
  - the stale sentence in `TransitionSchedule`'s doc ("until then, consuming
    engines silently skip scheduled transitions", R) is removed.
- **`docs/cloud-parity/scheduled-transitions.md`, rewritten (Gate 7).** §1 grace
  band, §7 and §8 are replaced by:
  - one owner per run;
  - never repeat unsafe work;
  - retry until `timeoutMs`;
  - the FAILED reasons;
  - the lost-owner cap;
  - the query;
  - entity delete removes tasks.

  A CaaS ticket is filed for Cloud to follow.
- **`docs/ARCHITECTURE.md`**: the scheduler sections are rewritten to present
  tense (`:94, :382-391, :943, :1486-1506, :1550-1553, :1992`).
- **`CHANGELOG.md`, `### Breaking`**: the removed variables, the changed
  processor-repeat semantics, the new FAILED status.
- **`COMPATIBILITY.md`**: the SPI pin bump.
- **SPI workflow, mid-milestone:** an SPI PR into `main`, which cyoda-go
  pseudo-pins; no tag.
- **cyoda-go-cassandra#68** updated to this contract.

## 13. Coverage matrix

Layers:
- **U**: unit tests in the owning package.
- **S**: `spitest` conformance on memory, SQLite and PostgreSQL.
- **E**: `internal/e2e`, on PostgreSQL through the HTTP stack.
- **P**: a cross-backend parity scenario registered in `e2e/parity/registry.go`.
- **M**: multi-node PostgreSQL (`e2e/parity/postgres` multinode).
- **G**: gRPC.

Concurrency and timing cases stay out of **P** (`.claude/rules/test-coverage.md`).

### Endings

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| fired on time | ✓ | | ✓ | ✓ | |
| fired after one safe failure (cnode unavailable, then available) | ✓ | | ✓ | ✓ | |
| scheduled self-loop fires and re-arms the same id as a new life | ✓ | ✓ (C3) | ✓ | ✓ | |
| declined (criterion false) | ✓ | | ✓ | ✓ | |
| expired (late, no failed attempt) | ✓ | | ✓ | ✓ | |
| safe failure: criterion error → WAITING, attempts 1, error recorded | ✓ | | ✓ | ✓ | |
| safe failure: no cnode for the tag (no hand-off) → mark cleared, WAITING | ✓ | | ✓ | ✓ | |
| safe failure: idempotent processor fails → WAITING | ✓ | | ✓ | ✓ | |
| retry delay doubles and is capped; last attempt at the deadline | ✓ | | | | |
| expired after failed attempts → FAILED | ✓ | | ✓ | ✓ | |
| unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never re-run | ✓ | | ✓ | ✓ | |
| later step fails after unsafe hand-off → FAILED | ✓ | | ✓ | ✓ | |
| unsafe `ASYNC_NEW_TX` fails, run commits → completed | ✓ | | ✓ | | |
| CBD: TX_pre committed, later failure, all idempotent → retried from TX_pre state | ✓ | | ✓ | | |
| CBD: unsafe CBD processor → FAILED after failure | ✓ | | ✓ | | |
| FAILED audit event recorded with reason | ✓ | | ✓ | ✓ | |

### Ownership, fencing and liveness

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| concurrent `ClaimDue` calls get disjoint sets | | ✓ | | | ✓ |
| run longer than 3 × heartbeat interval is not claimed by another pnode | | | | | ✓ |
| owner killed without mark → claimed after `STALE_AFTER`, `lostOwners` 1, runs, fires | | | | | ✓ |
| owner killed with mark → FAILED, processor sent once | | | | | ✓ |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | ✓ | ✓ | | | |
| stale claim token refused: `Complete`, `CheckClaim`, `MarkUnsafe`, `ClearUnsafe`, `RecordAttempt`, `Fail` | | ✓ | | | |
| superseded owner sends no unsafe processor (`MarkUnsafe` refused) | ✓ | | ✓ | | |
| superseded owner's CBD TX_pre refused | ✓ | | ✓ | | |
| old owner's token after the same id is re-armed and claimed again is refused (ABA) | ✓ | ✓ | | | |
| re-arm while RUNNING fences the owner; the run is superseded, no bookkeeping | ✓ | ✓ | ✓ | | |
| heartbeat failure → self-cancel before `STALE_AFTER`, no claims until recovery | ✓ | | | | |
| heartbeat and mark writes never fail the run's commit or a client write (C1) | | ✓ | ✓ | | |
| heartbeats not starved with every pool connection in an entity tx (C2) | | | ✓ | | |
| never more than `MAX_RUNS` runs in progress; the next claim waits for a slot | ✓ | | ✓ | | |
| slot freed after a full claim → immediate claim, not the next tick | ✓ | | | | |
| empty cluster view has no effect (no coordinator) | ✓ | | | | ✓ |

### Shutdown

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| runs finish within the drain; cnode streams still open during it | ✓ | | ✓ | | |
| run cut by the drain, no mark → WAITING, not counted, claimed at once by another pnode | ✓ | | | | ✓ |
| run cut by the drain, with mark → FAILED | ✓ | | ✓ | | |
| `GiveBack` does not count; `RetireOwner` removes liveness | | ✓ | | | |

### Entity writes

| Scenario | U | S | E | P | M |
|---|---|---|---|---|---|
| FAILED task re-armed by an update in the state (new life, mark gone) | ✓ | ✓ | ✓ | ✓ | |
| FAILED task cancelled when the entity leaves the state | ✓ | | ✓ | ✓ | |
| delete one entity removes its tasks (HTTP and gRPC) | ✓ | ✓ | ✓ | ✓ | |
| conditional delete removes tasks (HTTP and gRPC) | ✓ | ✓ | ✓ | ✓ | |
| delete-all removes the model's tasks | ✓ | ✓ | ✓ | ✓ | |

### Query (`GET /scheduled-tasks`)

| Scenario | U | S | E | P |
|---|---|---|---|---|
| 200, no filter, paging over several pages | ✓ | ✓ | ✓ | ✓ |
| 200, each filter: status (one and several), model, model version, entity, transition | ✓ | ✓ | ✓ | ✓ |
| 200, empty (unknown model, unknown entity) | | | ✓ | |
| 200, FAILED item shows reason, error, times, attempts | | | ✓ | ✓ |
| internal error in `lastError` is "internal error [ticket]" only | ✓ | | ✓ | |
| 400 unknown `status` | ✓ | | ✓ | |
| 400 `modelVersion` without `modelName` | ✓ | | ✓ | |
| 400 `modelVersion` not an integer ≥ 1 | ✓ | | ✓ | |
| 400 `entityId` not a UUID | | | ✓ | |
| 400 empty or too-long `modelName`/`transition` | ✓ | | ✓ | |
| 400 `limit` 0, 1001, not an integer | ✓ | | ✓ | |
| 400 invalid cursor (value not echoed) | ✓ | | ✓ | |
| 401 no token | | | ✓ | |
| 500 SERVER_ERROR with ticket (store double) | ✓ | | ✓ | |
| 503 STORAGE_UNAVAILABLE (store double) | ✓ | | ✓ | |
| another tenant's tasks never returned, for every filter | | ✓ | ✓ | ✓ |

**G (gRPC), waived:** the query has no gRPC door (§8), and no other gRPC
behaviour changes. The existing gRPC delete doors are covered through the
shared service functions in the entity-write rows above, and additionally in
`internal/grpc` by one test each asserting that tasks are removed.

### Configuration

| Scenario | U |
|---|---|
| each new variable's default and its validation failure | ✓ |
| each removed variable is no longer read (grep exit check, §15) | ✓ |

## 14. Dependencies and scope

- **#599 lands before or with #598** (§5.4). It needs a milestone; today it
  has none.
- **Not included:**
  - an API to retry or dismiss a FAILED task (an entity write does both);
  - notifications (§5.7 names the future emission point);
  - reclaiming a pnode that hangs but still heartbeats;
  - #600.

## 15. Exit checks (greppable)

Each of these returns nothing outside `docs/plans/`, `docs/superpowers/` and
`CHANGELOG.md`:

```
grep -rn "RedispatchAfter\|MarkRedispatch\|AttemptCount\|ScanDue" --include='*.go' .
grep -rn "LowestLiveNodeID\|RoundRobin\|SchedulerRPC\|scheduled-task\"" --include='*.go' internal app
grep -rn "CYODA_SCHEDULER_DISTRIBUTION\|CYODA_SCHEDULER_COORDINATOR\|CYODA_SCHEDULER_REDISPATCH_BACKOFF\|CYODA_SCHEDULER_BATCH_SIZE\|CYODA_SCHEDULER_EXPIRY_GRACE\|CYODA_DISPATCH_FORWARD_TIMEOUT" .
grep -rn "expiryGrace\|WithExpiryGrace" --include='*.go' .
```

The same checks run in `plugins/*` and in the SPI.

## 16. Verification points (resolved during planning, before code)

- **V1**: Memory commit can refuse on the claim check in its validation step,
  before applying anything.
- **V2**: No path other than those in §5.2 and §7 writes or removes a task.
  Check every caller of the store.
- **V3**: A compute-node callback joined to a scheduled run's transaction that
  updates the fired entity: what does an ordinary (non-scheduled) transition
  do in the same case? The scheduled run must behave the same. If that case
  re-arms the task inside the run, the run is superseded on every attempt.
  Decide whether that outcome is right, or whether the case is refused as an
  unsupported pattern.
- **V4**: The multi-node fixture can kill one pnode (without graceful
  shutdown) and restart it. If it cannot, the fixture is extended.
- **V5**: The existing scheduled e2e and parity tests that rely on removed
  behaviour (the grace band, the redispatch throttle, round-robin) are
  rewritten against this spec, not deleted.
- **V6**: The Helm chart's `terminationGracePeriodSeconds` exceeds the
  scheduler drain plus the gRPC and HTTP drains.
