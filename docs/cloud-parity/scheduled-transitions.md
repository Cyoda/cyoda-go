# Scheduled-transition runtime — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's scheduled-transition runtime. cyoda-go is the authoritative
implementation.

Words used here. A **node** is one platform process. A **compute member** is a
customer's compute program attached to a node. A **task** is the stored record
"fire transition T of entity E at time X". To **arm** a task is to create or
replace it; every arm starts a new **life** of the task. A node **claims** a
task to run it, and the claiming node is its **owner** until the run's outcome
is recorded. A **run** is one attempt, from the claim to the recorded outcome. A
**hand-off** is the moment a callout's work is on a compute member's connection
(`callout-failover.md` §1). An **unsafe processor** is a processor whose
`config.idempotent` is not `true`.

Sections 1 to 14 are the visible contract. Section 15 says what cyoda-go does
internally to meet it, and section 16 what that leaves Cloud to decide and what
is still open in cyoda-go.

## 1. Timing

A transition's `schedule` carries exactly one of two timing sources:

- **`delayMs`** — `scheduledTime = arm time + delayMs`.
- **`function`** — an arm-time callout computes `scheduledTime` and optionally
  an expiry per entity (§14).

`timeoutMs` — set on a static schedule, or derived from a function's expiry —
gives the task a **deadline**: `scheduledTime + timeoutMs`. No `timeoutMs`
means no deadline. The owner judges the deadline on its own clock when it runs
the task; by the one-owner rule (§3) no other node decides the same task at the
same time.

A workflow import refuses, with `400 VALIDATION_FAILED`:

- a `timeoutMs` below 0;
- a `delayMs` below 1 on a static schedule (the schedule then has neither
  timing source), and a negative `delayMs` beside a `function`;
- a schedule with both timing sources or neither;
- a transition that is both `manual` and scheduled.

Every armed `scheduledTime`, and `scheduledTime + timeoutMs` when there is a
deadline, lies between the Unix epoch (0 ms) and 9999-12-31T23:59:59.999Z
(253402300799999 ms), computed without integer overflow. A static schedule
whose arm lands outside that range fails the entity write with a `500
SERVER_ERROR` and a ticket: the value comes from the workflow, not from the
request. A function result outside it fails the write as §14 says.

## 2. Arming, cancelling and removing — atomic with the write

Every entity write, in its own transaction:

1. arms a task for each scheduled transition of the entity's current state in
   the workflow selected for the entity. Every arm is a new life: status
   `WAITING`, next attempt at `scheduledTime`, attempts, lost owners and last
   error cleared, and any `FAILED` status gone;
2. removes every other task of the entity, recording
   `SCHEDULED_TRANSITION_CANCEL` for each. That covers a task whose source state
   the entity left, and a task the newly selected workflow does not schedule
   from the current state (a re-bind by workflow selection). A loopback that
   re-arms the same transition records `SCHEDULED_TRANSITION_ARM` only.

A scheduled transition is one that has a `schedule` and is neither `manual`
nor `disabled`. Arm, fire and import clean-up use this one rule.

Deleting an entity — one entity, by condition, or all entities of a model —
removes its tasks in the same transaction, with no audit event. A workflow
import saves the workflows first. Then it removes the model's tasks whose
(source state, transition) neither a workflow of the model nor the default
workflow schedules any more. It does this in a transaction of its own; a
conflict with the scheduler is retried 3 times on the server, and then the
import answers a retryable `409`. A repeated import completes it. An import
that joins a caller's transaction removes the tasks in that transaction, once,
without a server-side retry.

An entity commit that lands in a scheduled state with no task is a lost fire,
and a task that outlives the reason it was armed is a phantom timer. The
atomicity above prevents both.

## 3. One owner per run

- At most one node claims a task at a time, and the claiming node runs it.
- Every write the owner makes for the run — each commit, the recorded outcome —
  is accepted only if the task's life and claim are still the ones it claimed.
  A run whose task was re-armed, removed or claimed by another node since it
  began commits nothing.
- A node proves it is alive with a heartbeat record, stamped by the store's
  clock. Another node may claim a `RUNNING` task only after its owner's record
  is older than `STALE_AFTER` (cyoda-go default 2 minutes) or missing; that
  claim counts one **lost owner**.
- A node whose own heartbeats keep failing cancels its runs before it can be
  considered stale, and a commit already under way keeps the task unclaimable
  until it lands. So no other node reclaims a task while its owner can still
  commit.
- A node that keeps heartbeating keeps its tasks, even when a run hangs.
- At most one task per entity is `RUNNING` at a time.
- A task whose claim reached no run — the claim's reply was lost, or it
  arrived after the node began to drain, latched (§10, §15) or passed its heartbeat
  deadline — is given back to `WAITING` without counting an attempt, and is
  claimable at once.

## 4. The criterion

When a run starts, the criterion is evaluated once:

- `true` (or no criterion) → the transition fires: processors run, state
  advances, `TRANSITION_MAKE` and `SCHEDULED_TRANSITION_FIRE` are recorded, and
  the task is removed (or re-armed, if the cascade ends in the source state
  again).
- `false` → **declined**: `TRANSITION_NOT_MATCH_CRITERION`, the entity stays,
  the task is removed and not retried. Poll-until-condition is expressed in
  the workflow (an unconditional scheduled tick into a state whose ordinary
  transitions carry the condition), never as a timer retry.
- A criterion that cannot be evaluated is a failure (§6), not `false`.

## 5. Explicit fire stays rejected

`PUT /entity/{format}/{entityId}/{transition}` and the gRPC requests that name
a transition return `400 TRANSITION_NOT_FOUND` for a scheduled transition. The
message is `transition "X" in state "Y" is scheduled and fires automatically;
it is not manually fireable`, followed by the code's own text. Model early
firing as an ordinary manual transition beside the scheduled one.

Any write that leaves the entity in the source state re-arms the task, so an
entity written more often than its delay never reaches the fire. This is
intended and must not regress to an entry-time-only timer.

## 6. Failures that are safe to repeat are retried

A run fails **safely** when nothing unsafe may have reached a compute member:
a criterion or function error, no compute member for the tag, a conflict with a
concurrent write, a storage error, an `idempotent` processor's failure, or an
unsafe processor that provably never reached a member (`callout-failover.md`
§1: no hand-off). The task goes back to `WAITING`, `attempts` goes up by one,
and its last error is recorded. The next attempt is after

```
delay = RETRY_DELAY × 2^(attempts−1), at most RETRY_DELAY_MAX   (cyoda-go: 30 s, 15 min)
next  = now + delay; with a deadline, next = min(next, deadline)
```

**[ruling]** With a deadline, retries stop at it (§9). Without one, a failing
task is retried without end; it stays visible (§11) and is never terminal.

## 7. An unsafe processor is never repeated by the platform

Before every dispatch of an unsafe processor, the owner writes a mark on the
task's life, in a write of its own that survives the run's rollback. If the
unsafe processor may have been handed off and the run does not commit — the
member failed or went silent, a later step failed, or the owner died — the
task ends `FAILED` (`UNSAFE_WORK_NOT_COMPLETED`) and is never claimed again.
A later claim that finds the mark ends the task the same way. Only proof that
no hand-off happened lets the run clear the mark and retry.

A processor declared `idempotent: true` is not marked; its failures are retried
(§6). Criteria and functions are repeat-safe by rule and are not marked; one
whose callbacks trigger an unsafe processor is unsupported. cyoda-go does not
detect this; that processor can run twice (§16, open item 4).

A run that committed a `COMMIT_BEFORE_DISPATCH` segment after the fired
transition changed the entity's state, and then stopped, ends `FAILED`
(`STOPPED_AFTER_PARTIAL_COMMIT`). This holds also when the cascade came back to
the source state: the entity has moved, so running the transition again from
its source state would be wrong. A `COMMIT_BEFORE_DISPATCH` step of the fired
transition itself commits the entity still in the source state, and a safe
failure after it is retried from there.

## 8. Callbacks inside a run

A compute member's callback during a run joins the run's transaction, as it
joins a client request's.

- A callback read sees what is stored in that transaction. Today it does not
  see the mutations an earlier processor returned and the engine has not yet
  saved, so a processor bases its own write on the payload of its request.
  This behaviour is under decision (§16, open item 2).
- Processors with transaction-callback access — `SYNC`, `ASYNC_SAME_TX`, and
  `COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch: true` — can write the
  fired entity through a callback. If such a processor returns no mutations,
  the engine keeps that write. A processor that returns mutations overrides the
  callback's write. A callback write that leaves the stored payload unchanged
  cannot be told from no write, so the engine's payload stands. Under
  `ASYNC_NEW_TX` the engine's payload stands.
- A callback that writes the fired entity also holds the task. When a
  processor that is not `idempotent`, or a `COMMIT_BEFORE_DISPATCH` step,
  follows in the same run, the run cannot mark or commit the task, so the
  attempt fails safely — and every retry fails the same way. The run never
  hangs and never repeats unsafe work.
- A callback that deletes the fired entity ends the run as follows: the delete
  commits, the task is removed, nothing is re-created or re-armed, and no
  `SCHEDULED_TRANSITION_FIRE` is recorded.

## 9. Lateness

The rule depends on the task's history:

| Task picked up | When | Outcome |
|---|---|---|
| first attempt, no lost owner | after the deadline | **expired**: removed, `SCHEDULED_TRANSITION_EXPIRE`, criterion not evaluated |
| after a failed attempt or a lost owner | at most `RETRY_DELAY` after the deadline | runs |
| after a failed attempt or a lost owner | more than `RETRY_DELAY` after the deadline | `FAILED` `EXPIRED_AFTER_FAILED_ATTEMPTS` |
| any | a counted attempt fails after the deadline | `FAILED` `EXPIRED_AFTER_FAILED_ATTEMPTS` |

A node crash counts as a lost owner, so a task that was running on a crashed
node is never expired: it runs, or ends `FAILED`, by the rows above.

At shutdown a node stops claiming, and from then on none of its runs begins a
new unsafe dispatch. It waits `SHUTDOWN_DRAIN` (cyoda-go default 20 s) for its
runs, then cuts every other run and hands back the ones that sent nothing
unsafe and made no partial commit: the attempt is not counted, the task is due
again at once, and its last error is the cancelled-run text (§11). A cut run
that already sent unsafe work or partially committed ends `FAILED`
(`UNSAFE_WORK_NOT_COMPLETED` or `STOPPED_AFTER_PARTIAL_COMMIT`) instead. A run
with a non-idempotent processor callout in flight is not cut, and is allowed
to finish. If a run has not ended by the time the shutdown wait runs out, the
node stops with its claim still held, and another node takes the task over
after `STALE_AFTER`. The next claim decides with the table.

## 10. `FAILED`

| Reason | When |
|---|---|
| `UNSAFE_WORK_NOT_COMPLETED` | §7 |
| `STOPPED_AFTER_PARTIAL_COMMIT` | §7 |
| `EXPIRED_AFTER_FAILED_ATTEMPTS` | §9 |
| `OWNER_LOST_REPEATEDLY` | the task lost its owner `MAX_LOST_OWNERS` times (cyoda-go: 3) |
| `RUN_PANICKED` | the run panicked; the node latches: it reports unhealthy, claims nothing more and cancels its runs in progress |

**[ruling]** A `FAILED` task never moves the entity and is never claimed. It is
kept, and it is visible: in `GET /scheduled-tasks` (§11), in the metrics, in
the log at ERROR with a ticket, and as a `SCHEDULED_TRANSITION_FAIL` audit
event on the entity carrying `{transition, sourceState, reason, attempts,
lostOwners}`, written in the same transaction as the status. When the reply to
the `FAILED` write is lost, its retry is refused as superseded and logged at
DEBUG instead of ERROR; the status and the audit event are still stored. It ends when the
entity is written in the source state (a new life), leaves the state (removed,
`SCHEDULED_TRANSITION_CANCEL`), stops being scheduled by the workflows (removed
at the next write, with `SCHEDULED_TRANSITION_CANCEL`, or at the next import,
without an event), or is deleted. There is no retry or dismiss API; an entity
write is both.

## 11. `GET /scheduled-tasks`

HTTP only, like the audit trail: an operator's view no compute member needs.
Any authenticated user of the tenant may call it; the tenant comes from the
token. Parameters: `status` (repeatable: `WAITING`, `RUNNING`, `FAILED`),
`modelName` (1–256 characters of valid UTF-8, no NUL), `modelVersion` (integer
≥ 1, only with `modelName`), `entityId` (UUID), `cursor` (opaque, ≤ 256
characters), `limit` (1–1000, default 20; out of range is `400`, not clamped).
Filters combine with AND. Results are ordered by `(scheduledTime, taskId)`
ascending. The response is
`{ "items": [ScheduledTaskDto], "pagination": { "hasNext", "nextCursor" } }`.

`ScheduledTaskDto` is typed but open: `taskId`, `entityId`, `modelName`,
`modelVersion`, `sourceState`, `transition`, `status`, `scheduledTime`,
`armedTime`, `expiresTime` (with a deadline), `attempts`, `lostOwners`,
`nextAttemptTime` (`WAITING`), `lastAttemptTime` (after a failed attempt),
`failureReason` and `failedTime` (`FAILED`), `armedBy` `{id, kind}` (when
known). `lastError` pairs with `failedTime` on a `FAILED` task: it is the
failure's text, present even when empty. On every other status it pairs with
`lastAttemptTime` and is present when that is. Node ids and tokens are never
returned.

`lastError` is at most 1 024 bytes of valid UTF-8 with no NUL, from an
allow-list: a compute member's own message, a coded client-safe message, two
fixed texts for a cancelled run and a conflict with a concurrent write, or
`internal error [ticket: <uuid>]`. A conflict records the conflict text on
every backend.

| Status | Code | When |
|---|---|---|
| 200 | — | success, including an empty list |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName` or not an integer ≥ 1; `entityId` not a UUID; `modelName` empty, too long, not valid UTF-8 or containing NUL; `limit` not an integer or outside 1–1000; invalid `cursor` (never echoed) |
| 401 | `UNAUTHORIZED` | no token, or an invalid one |
| 500 | `SERVER_ERROR` | internal failure; generic message and a ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no `403` (no role is required) and no `404`: an unknown model or
entity, or another tenant's, returns an empty list.

## 12. A client write can conflict with the scheduler

The scheduler writes a task row when it claims, commits a run segment, records
an attempt, fails a task or hands it back. A client write that re-arms or
removes a task row the scheduler wrote after the write began gets a retryable
`409 CONFLICT` — the same as a write racing the fire itself.

| Endpoint | Status | When |
|---|---|---|
| `deleteSingleEntity` | `409 CONFLICT` (retryable) | the conflict persists after 3 server-side retries |
| `deleteEntities` (conditional, one transaction; delete-all) | `409 CONFLICT` (retryable) | the same |
| `deleteEntities` (batched) | `200` | a persistent conflict is reported per entity in the batch result |
| `updateSingle`, `updateSingleWithLoopback`, `updateCollection` | `409 CONFLICT` (retryable) | no server-side retry |
| `importEntityModelWorkflow` | `409 CONFLICT` (retryable) | the task removal conflicts after 3 retries |

A delete or import that joins a transaction the caller already holds is not
retried on the server; the conflict is the transaction owner's to handle. The
gRPC entity requests return the same condition as they return any entity
conflict.

## 13. Audit events

| Outcome | Event |
|---|---|
| Armed | `SCHEDULED_TRANSITION_ARM`, on every arm including loopback re-arms |
| Fired | `SCHEDULED_TRANSITION_FIRE` + `TRANSITION_MAKE` |
| Declined | `TRANSITION_NOT_MATCH_CRITERION` |
| Expired | `SCHEDULED_TRANSITION_EXPIRE` |
| Removed by a write (the entity left the state, or the selected workflow does not schedule it), or at the fire when the selected workflow no longer schedules it, or when the entity carries no transaction id (logged at ERROR) | `SCHEDULED_TRANSITION_CANCEL` |
| Removed by a workflow import | none |
| Ended `FAILED` | `SCHEDULED_TRANSITION_FAIL` |
| Safe failure | none; the task's attempts and last error record it |
| Superseded run; at the fire, entity gone or moved on; entity deleted by a callback in the run | none |
| Entity deleted | none |

A task that comes due while the selected workflow no longer schedules it is
removed with `SCHEDULED_TRANSITION_CANCEL` at that point too; a task past its
deadline on its first attempt records `SCHEDULED_TRANSITION_EXPIRE` first,
because lateness is decided before the workflow is resolved.

## 14. Arm-time `function`

The callout runs synchronously inside the entity write's transaction and
returns `resultKind: "Schedule"` with exactly one of `fireAt` (absolute unix-ms)
or `fireAfterMs` (relative to arm time), and at most one of `expireAt` or
`expireAfterMs` (relative to the resolved fire time). A fire time in the past
is due at once. A resolved expiry at or before the fire time is **born
expired**: the transition is not armed, any pending task for it is removed, and
`SCHEDULED_TRANSITION_EXPIRE` is recorded in the same write, which still
succeeds.

A malformed or wrong-kind result fails the write with `500
SCHEDULE_FUNCTION_INVALID_RESULT`. That covers a negative `fireAfterMs` or
`expireAfterMs`, and a resolved fire time or expiry outside the range of §1.
An unreachable or silent compute member fails the write with the retryable
`503` codes of `callout-failover.md` §2; a member that answers `success: false`
fails it with `400 WORKFLOW_FAILED`.

On the fire path, the cascade can end in a state with its own `function`-timed
schedule. If that arm's callout fails, the open transaction rolls back; the
task is retried (§6), unless a step already committed
(`STOPPED_AFTER_PARTIAL_COMMIT`, §7) or an unsafe processor may have been
handed off (`UNSAFE_WORK_NOT_COMPLETED`).

## 15. How cyoda-go meets §3 and §7

- Tasks live in `ScheduledTaskStore` (`cyoda-go-spi`). Every arm draws a random
  arm token; every claim a random claim token. The owner's writes are accepted
  only with both current.
- Every commit of a run writes the task row, and task rows are under
  first-committer-wins — the same rule as entities. A commit whose task changed
  since it began fails. A task row written by an open transaction cannot be
  claimed, and a mark cannot be written on it (the store answers "busy").
- A mark and a claim of the same task are serialised: either the mark is
  refused, or the claim sees it.
- Heartbeats and claims have connections of their own, so a saturated entity
  pool cannot starve them.
- A node claims from a lost owner only after its own heartbeats have succeeded
  for `STALE_AFTER` without a gap.
- The owner's callout loop attaches a "not handed off" proof only when no try
  reached a member and no hand-over to a peer got past connecting — unless the
  peer's authenticated answer was `no_handoff` to a callout that is not
  repeat-safe.
- The owner retries an outcome write until the store accepts or refuses it. A
  retry whose earlier try landed with its reply lost is refused as stale: the
  stored outcome is the one the first try wrote, and the run is counted and
  logged as superseded. The retry also stops when shutdown stops waiting for
  outcomes: the claim is kept, and the task is taken over after `STALE_AFTER`.
- A store that rejects an outcome write as invalid latches the node: it reports
  unhealthy, claims nothing more and keeps that run's claim, and its other runs
  go on.
- A panic while a run's outcome is being recorded records no `RUN_PANICKED`.
  The node latches and keeps the claim, so the task stays `RUNNING` while the
  latched node keeps heartbeating.
- Backend coverage: the scheduled-transition and scheduled-function parity
  scenarios run on memory, SQLite and PostgreSQL. The commercial Cassandra
  backend imports only the `externalapi` parity package, so none of those
  scenarios run on it yet.

## 16. What Cloud has to decide

The visible contract to match:

1. One owner per run and the lost-owner rule of §3, however Cloud implements
   ownership.
2. §6 and §7: safe failures retried with a doubling delay until the deadline;
   an unsafe processor never repeated after a possible hand-off, except one
   reached from inside a criterion's or function's callback (§7).
   `idempotent` means the same on a scheduled run as on any callout.
3. §9's lateness table, decided by the task's one owner.
4. The `FAILED` status, its five reasons, and **[ruling]** that it never moves
   the entity; `SCHEDULED_TRANSITION_FAIL` and its data.
5. §2: an arm is a new life that clears a `FAILED` status; a write removes every
   task not in its arm set; deletes and imports remove tasks.
6. §1's import minimums and arm-time range.
7. §8: in the modes it names, a callback write to the fired entity that the
   processor does not override is kept; a callback write followed by an unsafe
   processor or a `COMMIT_BEFORE_DISPATCH` step fails every attempt safely.
8. `GET /scheduled-tasks` as §11 states it.
9. The `409` cells of §12.
10. One `RUNNING` task per entity.

Where Cloud's storage cannot make a task-row write part of the entity
transaction, it needs another way to stop a superseded owner from committing
(§3, second point) and another durable place for the mark (§7). Those are
Cloud's to design; the visible contract above is what must match.

Open in cyoda-go. These are not settled, and Cloud should not build on any of
these answers yet:

1. **Workflow save inside a joined transaction.** On PostgreSQL a workflow
   save that joins a caller's transaction commits or rolls back with it; on
   memory and SQLite it applies at once. The SPI does not say which is right.
   The task removal of §2 is correct under either.
2. **The engine's unsaved payload and callbacks.** §8's first point is today's
   behaviour: a callback does not see the mutations an earlier processor
   returned. Whether the engine should make them visible to a joined callback
   is under decision.
3. **An unguarded scheduled self-loop.** The import's loop check counts a
   scheduled transition as automated, so a scheduled transition back to its
   own state with no criterion is refused with `400 VALIDATION_FAILED`
   ("infinite loop detected"), although the cascade never follows a scheduled
   transition. A criterion on the transition, or `allowCycles: true` on the
   import, passes the check.
4. **An unsafe processor inside a criterion's or function's callback.** Such a
   processor is not detected, so it can run twice (§7). Enforcing the rule —
   refusing it through the run's guard found by transaction id — is under
   decision.
