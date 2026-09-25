# #598 research — who owns a scheduled run

Status: research, 2026-09-24. Facts only; the design comes after. Every claim
cites the source it was read from. "Not verified" is said where that is so.

Paths: `SPI/` is the pinned `cyoda-go-spi`
(`v0.8.5-0.20260923200233-8aa2258b26cb`). `CASS/` is `../cyoda-go-cassandra`
(the commercial backend). `PLAT/` is `~/dev/cyoda-platform` and `CLOUD/` is
`~/dev/cyoda` (Cyoda Cloud). Everything else is this repository.

## 1. Terms

- **Task** — the stored row that says "fire transition T of entity E at time
  X". One per (tenant, entity, source state, transition); the id is a hash of
  those four (`internal/domain/workflow/arm.go:27-30`).
- **Arm** — create or replace a task. **Fire** — run the transition the task
  names. **Run** — one attempt to fire a task, from start to commit or
  rollback.
- **pnode** — a cyoda-go processing node. **cnode** — a compute node that runs
  processors and criteria for a workflow.
- **Processor side effect** — anything a processor does outside cyoda (an
  e-mail, a payment call). cyoda cannot undo it by rolling back.

## 2. How a scheduled transition works today

### 2.1 Arming and cancelling

- A task is written by `reconcileScheduledTasks` after every settled write of
  an entity whose workflow has a scheduled transition: create, manual
  transition, update, and the scheduled fire itself
  (`internal/domain/workflow/arm.go:95-209`; call sites `engine.go:365, 461,
  563`, `fire_scheduled.go:487`).
- It is written in the same transaction as the entity
  (`SPI/persistence.go:45-47`). Memory and SQLite stage the write and apply it
  at commit; PostgreSQL writes straight into the open transaction
  (`plugins/memory/scheduled_task_store.go:75-91`,
  `plugins/sqlite/txmanager.go:549-557`,
  `plugins/postgres/scheduled_task_store.go:12-20`).
- Arming replaces the whole row, including the attempt counter and the
  redispatch time (`plugins/postgres/scheduled_task_store.go:29-51`). Every
  settled write in the source state therefore restarts the timer.
- Leaving the source state deletes the task and records
  `SCHEDULED_TRANSITION_CANCEL` (`arm.go:187-195`).
- Deleting the entity does not touch its tasks. The task is removed when it
  comes due: the fire finds no entity and deletes it without an audit event
  (`fire_scheduled.go:220-235`).

### 2.2 Scanning and dispatch

- One pnode scans. It is the pnode with the lowest node id in its own view of
  the cluster. With an empty view, every pnode decides it is that pnode
  (`internal/scheduler/coordinator.go:18-21`).
- Each scan (default every 1 s) reads up to 100 due tasks
  (`internal/scheduler/service.go:168`). For each one it first writes
  "ignore me for 30 s" (`MarkRedispatch`), then hands the task to a pnode
  picked round-robin and moves on without waiting (`service.go:174-214`).
- The code states that the 30 s is "a plain best-effort throttle, not a
  lease: it offers no exclusivity guarantee" (`service.go:42-47`), and that
  there is no back-pressure (`service.go:193-197`).
- The round-robin choice does not look at the task (`distribution.go:34`).
  Delegation exists to spread load, not because another pnode is better
  placed.
- A delegated task goes to the peer over an internal HTTP call
  (`POST /internal/dispatch/scheduled-task`, `internal/cluster/scheduler_rpc.go:32`).
  The calling pnode gives up after 30 s (`CYODA_DISPATCH_FORWARD_TIMEOUT`,
  `app/app.go:608`). The receiving pnode runs the fire on a context that
  derives from `context.Background()`, not from the request
  (`scheduler_rpc.go:352`), so the run carries on after the caller has given
  up. The caller uses the answer only for a log line (`scheduler_rpc.go:129-132`).

### 2.3 The run

`FireScheduledTransition` (`internal/domain/workflow/fire_scheduled.go:89-524`)
opens a transaction, re-reads the task and the entity, and checks in turn:
task gone, tenant mismatch, entity gone, entity left the source state, task
re-armed into the future, too late (`timeoutMs` plus a 100 ms grace band),
transition no longer in the workflow. Then it fires the transition: criterion,
processors, the automatic cascade, re-arming of the new state's timers. It
persists the entity with a compare-and-save against the transaction id it read
at the start (`fire_scheduled.go:383, 511`) and commits. The commit deletes the
task (`fire_scheduled.go:487`, through the re-arm step).

Outcomes:

| What happens | Task afterwards | Audit on the entity |
|---|---|---|
| Fired | deleted | `SCHEDULED_TRANSITION_FIRE` |
| Criterion false | deleted, not retried (`fire_scheduled.go:430-440`; documented as one-shot, `help/content/workflows.md` "Firing") | `TRANSITION_NOT_MATCH_CRITERION` |
| Too late | deleted | `SCHEDULED_TRANSITION_EXPIRE` |
| Any other failure | **left in place** (`fire_scheduled.go:443-445`) | none |

A failure leaves one log line at ERROR (`internal/scheduler/executor.go:57`,
`scheduler_rpc.go:356`). No audit event, metric or API records it.

### 2.4 Processor modes inside a run

(`internal/domain/workflow/engine_processors.go`)

- `SYNC` and `ASYNC_SAME_TX` run inside the run's transaction.
- `ASYNC_NEW_TX` runs under a savepoint of the same transaction, and its own
  failure is not fatal (`:244-294`, `:175-183`). It commits nothing on its own.
- `COMMIT_BEFORE_DISPATCH` **commits** the work done so far ("TX_pre") before
  the processor is called, then continues in a new transaction (`:306-451`,
  `:477-523`). The entity's state name only changes after all processors
  (`engine.go:831-848`), so after TX_pre the entity is stored with new data
  and a new transaction id, **still in the source state**. The task is not
  touched until the last transaction. If the run fails after TX_pre, the task
  survives and the next dispatch fires again from the entity as TX_pre left
  it (`fire_scheduled_test.go:1422` exercises the no-leak part).
- The help text already requires a `COMMIT_BEFORE_DISPATCH` processor to be
  safe to repeat, because a *client* that retries re-runs it
  (`help/content/workflows.md:182`). For a scheduled run there is no client;
  the scheduler is the one that repeats.

### 2.5 What goes wrong (the defects #598 lists), confirmed

1. **A run longer than 30 s is started again while it is still running.**
   Nothing marks a task as "being run". After 30 s the scan sees it as due and
   dispatches it again (`service.go:174-181`). Both runs execute every
   processor. Only one can commit: the other loses the entity compare-and-save
   (`fire_scheduled.go:511-515`) and rolls back. Side effects outside cyoda
   happen twice.
2. **A failed run is repeated every 30 s**, all processors included, until the
   transition's `timeoutMs` expires it — never, if it declares none
   (`fire_scheduled.go:276-295`, the gate only applies with a `TimeoutMs`).
3. **No attempt limit.** `AttemptCount` is written by `MarkRedispatch` and read
   by nothing: `grep -rn AttemptCount internal app cmd | grep -v _test.go`
   returns nothing.
4. **A delegated run outlives its delegation** (§2.2).
5. **An empty cluster view means "I scan"** (`coordinator.go:18-21`).
6. **No back-pressure** (`service.go:193-197`).
7. **Processor declarations are ignored.** The `idempotent` declaration from
   #254 (`SPI/types.go:244-251`) is read in one place only,
   `internal/callout/entry.go:17`, for handing a callout to another cnode. The
   scheduler never reads it.

Three further defects found during this research:

8. **After a `COMMIT_BEFORE_DISPATCH` commit, a failed run is repeated from the
   partly changed entity** (§2.4), processors before the commit included.
9. **"Was a row deleted?" is unreliable on memory and SQLite.** `Delete`
   answers from committed state, read separately from the staged delete, so
   two concurrent deletes both answer "yes"
   (`plugins/memory/scheduled_task_store.go:97-114`,
   `plugins/sqlite/scheduled_task_store.go:137-151`). PostgreSQL answers from
   the row count of the delete itself and is correct
   (`plugins/postgres/scheduled_task_store.go:68-74`). The expiry audit event
   relies on this answer (`fire_scheduled.go:282-285`). The cross-backend
   parity doc accepts duplicate audit events on memory/SQLite
   (`docs/cloud-parity/scheduled-transitions.md` §8); under the rule that a
   backend differing from the others is a bug, it is one.
10. **A wrong comment.** `fire_scheduled.go:482-486` says every backend stages
    task writes until commit; PostgreSQL does not (§2.1).

PostgreSQL's `Delete` filters on the task id only
(`plugins/postgres/scheduled_task_store.go:69`), although the migration comment
says writes carry a tenant predicate
(`plugins/postgres/migrations/000004_scheduled_tasks.up.sql:5-14`). The id
hashes the tenant in (`arm.go:27-30`), the only callers pass the id of a row
they read under a tenant check (`fire_scheduled.go:195-203`), and the task
table is not reachable from any API. So no cross-tenant path was found. The
comment is still wrong.

### 2.6 The contract we publish today

`docs/cloud-parity/scheduled-transitions.md` §7 promises "exactly-once fire,
state-correct always" and "at-least-once dispatch, not at-most-once",
explained as "the same at-least-once processor-idempotency contract the engine
already documents for ordinary transitions". The help topic for
`CYODA_SCHEDULER_REDISPATCH_BACKOFF` tells users to "size this value above the
longest scheduled transition you expect, or make the processors of scheduled
transitions safe to repeat" (`help/content/config/scheduler.md:25`). #598's
acceptance criteria contradict both: no second run while one is live, and no
repeat of a processor that is not declared safe to repeat. The parity doc must
change (Gate 7).

### 2.7 How long a run can legitimately take

A single callout is bounded by `tries × answer limit + patience + hand-over
allowance` (`internal/callout/coordinator.go:148-151`): 4 × 30 s + 5 s + 30 s =
155 s at the defaults, 275 s at the maximum answer limit. A run makes one
callout per criterion, processor, cascaded transition and re-arm function, one
after another. So a legitimate run can last several minutes. No time limit
applies to the run as a whole (`executor.go:44-45`).

## 3. What the code base already has: async search jobs

Design record: `docs/superpowers/specs/2026-09-08-509-async-orphan-reexecute-design.md`.

- **Owner and epoch.** Claiming a job raises its `Epoch`. Every later write by
  the executor carries the epoch it claimed under, and the store refuses a
  mismatch with `ErrStaleClaim` (`SPI/search_store.go:118-121`). A pnode whose
  job was taken is shut out by the store, not by a timer.
- **Liveness.** The executor stamps a heartbeat every 15 s
  (`CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`). A job whose heartbeat is older than
  5 min (`CYODA_SEARCH_JOB_STALE_AFTER`, validated ≥ 4 × the interval,
  `app/config.go:861-886`) may be claimed by any pnode. Staleness is judged
  with the store's clock, not the pnode's.
- **No coordinator.** Every pnode runs the reclaim sweep
  (`app/app.go:484-507`). PostgreSQL makes concurrent claims disjoint with
  `FOR UPDATE SKIP LOCKED` in one statement
  (`plugins/postgres/search_store.go:570-587`).
- **Back-pressure.** A pnode claims at most as many jobs as it has free
  capacity (`internal/domain/search/reaper.go:49-57`).
- **Graceful hand-back.** On shutdown a pnode releases its jobs; a released
  job is claimable at once and the release does not count as a lost executor
  (`SPI/search_store.go:199-211`).
- **Attempt limit.** Only lost executors count (`StaleClaims`). After
  `CYODA_SEARCH_JOB_MAX_ATTEMPTS` (3) the job is failed for good
  (`reaper.go:68-80`). Counting every claim instead was rejected in review: a
  rolling restart would have failed healthy jobs (509 spec D3).
- **Hung but alive is out of scope**: a pnode that heartbeats but makes no
  progress keeps its job ("liveness is not progress", 509 spec §7).
- **All three in-tree backends implement it**, each atomically: memory under
  one lock, SQLite with conditional updates on its single writer connection,
  PostgreSQL with conditional updates and `SKIP LOCKED`
  (`plugins/*/search_store.go`). The shared conformance suite covers it
  (`SPI/spitest/asyncsearch.go`).

What does not carry over as it stands:

- A search job runs no customer code. A scheduled run does, so reclaiming a
  run raises the question the search design never faced: may the processors
  run again?
- Search-job writes are independent of any entity transaction
  (`plugins/postgres/search_store.go:22-27`). The last step of a scheduled run
  is inside the entity's transaction, so ending the claim has to happen in
  that transaction.
- PostgreSQL runs entity transactions at REPEATABLE READ
  (`plugins/postgres/transaction_manager.go:129`). A transaction that updates
  or deletes a row that another transaction changed and committed after it
  started fails with a serialization error. If the heartbeat were written to
  the task row itself, the run's own final delete of that row would fail after
  every heartbeat. **Liveness must therefore live apart from what the run's
  transaction writes.**

## 4. What the code base already has: #254 callout failover

- A processor may declare `idempotent: true` in its `config`; default false;
  criteria and functions are always treated as safe to repeat
  (`SPI/types.go:244-251`, `api/openapi.yaml:10513-10523`,
  `internal/grpc/callout.go:191, 263`).
- The line that matters is the **hand-off**: the moment a callout is given to
  a cnode. Before a hand-off it is always safe to try elsewhere; after it, only
  a callout declared safe to repeat may be tried again
  (`internal/contract/callout.go:50-59`).
- A replaced cnode is shut out by a number that only goes up, checked at
  every point where its callbacks touch the transaction
  (`internal/fence/fence.go`).

## 5. The commercial backend

- **It has no scheduled-task store.** The accessor returns "not implemented"
  (`CASS/internal/factory/factory.go:753-762`); the work is open as
  cyoda-go-cassandra#68, which plans a copy of today's throttle. Scheduled
  transitions do not work on that backend today. Nothing there needs to stay
  compatible.
- **Shard takeover.** A shard's owner is claimed with a conditional write that
  raises an epoch (`CASS/internal/shard/shard_epoch.go:158-201`). Liveness
  comes from the message broker's consumer-group session; there is no timer of
  its own (`CASS/docs/superpowers/specs/2026-04-06-cassandra-shard-takeover-design.md:661`).
  A slow takeover only logs a warning; being replaced is a separate signal
  (`shard_takeover.go:416-438`). No data write checks the epoch
  (`CASS/docs/adr/0002-...md:96-98`); the old owner is stopped by cancelling
  its shard context.
- **The recovery rule.** "Recovery never re-runs workflows. This is a
  necessary constraint to avoid duplicating processor side effects" (ADR-0002
  `:57`); "recovery must never re-dispatch processors" (takeover spec `:38`).
  Recovery only finishes the commit of work that was already durable.
- **Its async search is on SPI v0.8.3**, which has no heartbeat, epoch or
  attempt limit (`CASS/go.mod:6-7`). A likely defect, from reading only: the
  by-shard index is never updated when a job completes, so a takeover within
  the 24 h TTL can reset a finished job and run it again
  (`CASS/internal/search/store.go:409-451`, `recovery.go:72-126`). To be filed
  on that repository; not part of #598.

## 6. Cyoda Cloud

Source: `PLAT/` (platform library, branch
`feature/CP-3963-3.3-spring-boot-4.1`; the scheduling packages match
`develop`) and `CLOUD/`.

- **Shape.** A scheduled transition is a processor of type `scheduled` with
  `delayMs`, `transition` and optional `timeoutMs`
  (`CLOUD/client/src/main/resources/api/openapi-common.yml:228-245`). When it
  runs it stores a `ScheduledTask` entity
  (`PLAT/.../ScheduledTaskService.java:182-221`).
- **Ownership is by shard, not by task.** ZooKeeper assigns shards to nodes;
  each node's timer handles the tasks of the shards it owns
  (`PLAT/.../ScheduledTasksManager.java`). There is no per-task lease or
  epoch. A status field (`AWAIT → SUBMITTED → FINISHED | EXPIRED |
  CANCELED`) and a durable queue event guard execution
  (`PLAT/.../ScheduledTask.java:43-49`).
- **One attempt.** Any failure of the run marks the task `FINISHED` with
  `actionSuccess=false` and a "FAIL:" message; it is never retried
  (`PLAT/.../ExecuteScheduledTaskActionProcessor.java:30-67`). Only a storage
  error is retried, and that retry can repeat processor side effects
  (`PLAT/.../EventReferenceRunnableTask.java:268-290`).
- **Tasks are kept.** Terminal rows stay, with their own change history
  (`ScheduledTaskService.java:301-373`). Failure is visible only on the task
  row and in logs, not on the target entity. There is no API to list or
  inspect tasks.
- **A later write cancels the timer** by comparing the entity's last
  transaction id at fire time with the one captured when armed
  (`PLAT/.../ScheduleTransitionProcessor.java:113-128`).
- **Likely Cloud defects, from reading only:** a delay or timeout above about
  24.8 days does not parse (`Integer.parseInt`,
  `ScheduleTransitionProcessor.java:72-74`), and that includes Cloud's own
  one-year default timeout (`CLOUD/.../CyodaToCloudWorkflowMapper.kt:34`); the
  recovery scan after a shard hand-off starts at the start time of the most
  recently submitted task and can miss older waiting tasks
  (`ScheduledTaskService.java:137-152`); the docs say scheduled runs execute as
  `system`, the code runs them as the user who armed the task
  (`CLOUD/docs/client-calculation-member-guide.md:465`,
  `ScheduleTransitionProcessor.java:79`). For a Cloud ticket; not part of #598.

## 7. What the facts settle for the design

- A per-task owner with an epoch, a heartbeat judged by the store's clock, and
  claims that any pnode may make, disjoint across pnodes, is already built and
  proven in this code base (§3). It removes the need for a scanning
  coordinator (defect 5) and, if the pnode that claims a task also runs it, for
  delegation (defect 4).
- Back-pressure falls out of "claim only as much as you can run" (§3).
- The rule for repeating processors has a precedent in both directions: #254
  draws the line at the hand-off and trusts `idempotent` (§4); the commercial
  backend never re-runs processors on recovery (§5); Cloud never retries a run
  (§6).
- A terminal failure must be recorded somewhere a user can see. Today the only
  per-entity record users can read is the audit trail
  (`GET /audit/entity/{entityId}`, `api/openapi.yaml:361`).
- The commit of a run must be fenced on the claim inside the entity
  transaction, and on PostgreSQL the heartbeat must not write the rows that
  transaction writes (§3).
- Because the commercial backend has no scheduled-task store yet, the SPI can
  change freely; cyoda-go-cassandra#68 must be updated to the new contract.
