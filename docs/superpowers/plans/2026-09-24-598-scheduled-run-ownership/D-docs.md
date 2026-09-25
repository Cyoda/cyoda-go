# Stream D — documentation, chart, changelog and cross-repo notes

Spec: `docs/superpowers/specs/2026-09-24-598-scheduled-run-ownership-design.md`.
D owns the documentation files of §11, all of §12 that no code stream owns, and
the Gate 4 and Gate 7 obligations. Every sentence about behaviour comes from
spec §1–§10. Where a text names an identifier, the task greps for it before the
commit; if the grep is empty, the sentence is corrected to what the code has.

Documentation tasks have no RED/GREEN cycle. Each has a **Verify** step instead:
the guard tests that exist for the file, and greps that prove a false sentence
is gone. The chart task has a real guard: two new steps in
`.github/workflows/helm-chart-ci.yml`, run locally first.

## Guards that exist

Found by reading `cmd/cyoda/help/*_test.go`, `app/config_registry_binding_test.go`
and `.github/workflows/helm-chart-ci.yml`.

| Guard | What it pins | Run |
|---|---|---|
| `TestContentMarkdownSubsetLinter` (`help_test.go:628`) | no pipe table, no indented (nested) bullet, no blockquote, no HTML block in help content (`renderer/linter.go:30-75`) | `go test ./cmd/cyoda/help/...` |
| `TestSeeAlsoResolution` (`help_test.go:656`) | every `see_also` entry resolves to a topic — `scheduled-tasks` may be listed only after Q's topic file exists | same |
| `TestConfig_EnvVarCoverage` (`help_test.go:489`) | every `CYODA_*` in non-test Go source under `cmd`, `app`, `plugins`, `internal` is named in `config.md` or `config/*.md` | same |
| `TestConfigAll_Complete` (`config_registry_test.go:74`) | every such variable is in the registry (R's and BP's work; D-1/D-2 run it) | same |
| `TestDefaultTree_ConfigClusterSubtopic` (`help_test.go:927`) | `CYODA_DISPATCH_WAIT_TIMEOUT` stays in `config.cluster` | same |
| `TestHelpContent_NoIssueIDs`, `TestSource_NoIssueNumbers` | no `#NNN` in help content | same |
| `TestHelpContent_CrossReferencesUseAWorkingInvocation` | `cyoda help a b`, never `cyoda help a.b`, in help, `README.md`, `api/openapi.yaml`, `api/generated.go` | same |
| `TestRunHelp_NoDuplicateSeeAlso`, `TestRunHelp_SeeAlsoUsesCLISyntax` | SEE ALSO rendering | same |
| `TestRootConfigVars_MatchDefaults` (`app/config_registry_binding_test.go:184`) | registry defaults equal `DefaultConfig()` | `go test ./app/ -run TestRootConfigVars_MatchDefaults` |
| `helm-chart-ci.yml` — lint, template, kubeconform, schema-rejection steps | the chart renders and its schema refuses bad values | the same commands, locally (D-5) |

No test reads `docs/ARCHITECTURE.md`, `docs/cloud-parity/*`, `docs/CONSISTENCY.md`,
`docs/plugins/POSTGRES.md`, `CHANGELOG.md` or `COMPATIBILITY.md`. Their
verification is greps and one read-through.

**Why D does not extend `TestRetiredSettings_Absent`.** Adding the six removed
names to its list (`config_registry_test.go:173`) would put them in a tracked
file, and the spec §15 exit check greps every tracked file except migrations,
plans, specs, release notes and `CHANGELOG.md`. The exit check is the guard for
the removed names.

## Vocabulary

Help, README, CHANGELOG and the cloud-parity file say **node** for a cyoda-go
process and **compute member** for a customer's compute program, as
`docs/cloud-parity/callout-failover.md` does. `pnode` and `cnode` stay inside
`docs/superpowers/`. A stored scheduled transition is a **task**. "Claim",
"owner", "run", "life" and "mark" are used as spec §3 defines them, and only in
`ARCHITECTURE.md` and the cloud-parity file; user-facing help says "owner" and
"claim" but not "life", "arm token" or "mark".

## Ownership of spec §11 and §12 — every item

The other sections are not written yet (`ls` of the plan directory shows
`README.md` and `interfaces.md` only). Rows for other streams follow the plan
README's stream table.

| Item | Owner | Where |
|---|---|---|
| `app/config.go`, `cmd/cyoda/help/config_registry.go` | R | — |
| `plugins/postgres/config.go`, `plugin.go`, `doc.go` | BP | — |
| `help/config/scheduler.md`, `help/config/cluster.md`, `help/config.md:33`, `README.md:224-236, :251` | **D** | D-1 |
| `help/config/database.md`, `docs/plugins/POSTGRES.md` | **D** | D-2 |
| `help/workflows.md` SCHEDULED TRANSITIONS, `idempotent` (`:202`), the selection paragraph `:478`, `:483`, ERRORS, `see_also` | **D** | D-3 |
| `idempotent` description in `api/openapi.yaml` (`:10513-10523`), `docs/workflow-schema-versioning.md` "When NOT to bump" | **D** | D-4 |
| new help topic `scheduled-tasks` and its `topLevelTopicsV061` entry | Q | — |
| `help/run.md` SHUTDOWN TIMING and SIGNALS; `help/helm.md`; chart templates, values, schema, `Chart.yaml`, chart README; `helm-chart-ci.yml` guard | **D** | D-5 |
| `help/telemetry.md` | **D** | D-6 |
| `api/openapi.yaml`: new operation and DTOs, `SCHEDULED_TRANSITION_FAIL` in the audit enum (`:11742`) | Q (Q-1) | — |
| `api/openapi.yaml`: 409 on `deleteSingleEntity` and `importEntityModelWorkflow` | W | — |
| `help/errors/CONFLICT.md` | W | — (D-3 and D-10 point at it) |
| SPI items (`SMEventScheduledTransitionFailed`, new errors, `ErrStaleClaim` comment, the `TransitionSchedule` sentence) | S | — |
| `docs/ARCHITECTURE.md` `:94, :230, :382-393, :772, :940, :943, :1359, :1486-1506, :1550-1553, :1992, :2248` | **D** | D-7 |
| code comment `internal/domain/search/reaper.go:16-22` | **D** | D-7 step 6 |
| `docs/cloud-parity/scheduled-transitions.md` and its README row | **D** | D-8 |
| `COMPATIBILITY.md`: SPI pin, chart version | **D** | D-9 |
| `CHANGELOG.md` `### Breaking` and the assembly of `[Unreleased]` | **D** | D-10 (last) |
| cyoda-go-cassandra#68 comment; CaaS ticket | **D** writes, lead posts | D-11 |
| migration comment `plugins/postgres/migrations/000004_scheduled_tasks.up.sql:5-14` | BP | — (spec §10.2 "It is corrected") |
| *Not in §11/§12, false once this lands:* `docs/cloud-parity/tenant-id-grammar.md:70` ("scheduler RPC payloads"); `docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis-playbook.md:59` (row F6: round-robin share, `distribution.go`); `docs/CONSISTENCY.md` §1 (says nothing of task rows); `help/run.md:264` SIGNALS; the `[Unreleased]` entries that name `CYODA_DISPATCH_FORWARD_TIMEOUT` and the scheduled-transition forward (`CHANGELOG.md:178-185, :196-200, :265-268, :675-682`) | **D** | D-7, D-5, D-10 |
| *Wrong today, found on the way:* `COMPATIBILITY.md:35` names the SPI pin `…7a75d2d335ee` while every `go.mod` pins `…8aa2258b26cb`, and names `ErrTxInFlight`, which does not exist (the sentinel is `ErrTxNotCommitted`, SPI `errors.go:115`); `COMPATIBILITY.md:79` "Currently pinned: `cyoda-go-spi v0.8.4`"; `COMPATIBILITY.md:87-117` "SPI work in flight" describes `ErrEntityModelMismatch` as unpushed, and it is pinned (`internal/domain/entity/service.go:2710`) | **D** | D-9 |
| *Wrong today:* `docs/plugins/POSTGRES.md:246-256` schema table has no `scheduled_tasks` row (the table exists since migration `000004`) | **D** | D-2 |
| *Stale:* `deploy/helm/cyoda/Chart.yaml` `artifacthub.io/changes` describes the `0.7.0` change | **D** | D-5 |

## Order

- D-1 runs directly after R's config task, and D-2 directly after BP's config
  task. From those commits until D-1/D-2 land, `TestConfig_EnvVarCoverage`
  fails; see Open point 1.
- D-3, D-6 and D-8 wait for E, R, W and Q (Q's `scheduled-tasks.md` exists, for
  `TestSeeAlsoResolution`).
- D-4 waits for Q-1 (same file) and E.
- D-5 waits for R's `run.go` wiring (the shutdown order it documents).
- D-7 waits for every code stream.
- D-9 runs after the wave-6 pin commit and after D-5.
- D-10 is the last commit of the plan that touches `CHANGELOG.md`.
- D-11 is text the lead posts after the PR merges.

---

### Task D-1: scheduler settings — help topics and README

> **Dropped (README C-D1).** R-10 writes this text; use the text below as R-10's reference if R-10's own wording is missing a point, but do not execute this task.

**Spec:** §6.1–§6.4, §11.

**Needs:** R's config task (`app/config.go`, `cmd/cyoda/help/config_registry.go`,
`app.SchedulerConfig` fields of `interfaces.md`).

**Files:**
- Modify: `cmd/cyoda/help/content/config/scheduler.md` (whole file)
- Modify: `cmd/cyoda/help/content/config.md:33`
- Modify: `cmd/cyoda/help/content/config/cluster.md:32`
- Modify: `README.md:224-236`, `:251`

- [ ] **Step 1: Check the names and defaults R registered**

```
grep -n 'CYODA_SCHEDULER_\|CYODA_DISPATCH_FORWARD' cmd/cyoda/help/config_registry.go
```

Expected: exactly `ENABLED`, `SCAN_INTERVAL`, `MAX_RUNS`, `MAX_RUNS_PER_TENANT`,
`HEARTBEAT_INTERVAL`, `STALE_AFTER`, `MAX_LOST_OWNERS`, `RETRY_DELAY`,
`RETRY_DELAY_MAX`, `SHUTDOWN_DRAIN`, with the defaults of the plan README's
settings table, and no `CYODA_DISPATCH_FORWARD_TIMEOUT`. If a default differs,
stop and tell the lead: the text below follows the spec.

- [ ] **Step 2: Replace `config/scheduler.md` whole**

```markdown
---
topic: config.scheduler
title: "cyoda scheduled-transition scheduler configuration"
stability: stable
see_also:
  - config
  - config.database
  - workflows
  - scheduled-tasks
  - run
---

# config.scheduler

## NAME

config.scheduler — claiming, liveness, retry and shutdown settings of the scheduled-transition scheduler.

## DESCRIPTION

Every node claims due scheduled transitions from storage and runs them itself. A claimed task has one owner at a time. A node proves it is alive with a heartbeat, and another node takes over its tasks only after those heartbeats have stopped for `CYODA_SCHEDULER_STALE_AFTER`. What a run does, and when a task ends `FAILED`, is in `cyoda help workflows` (SCHEDULED TRANSITIONS). `cyoda help scheduled-tasks` lists the tasks.

- `CYODA_SCHEDULER_ENABLED` (bool, default: `true`) — run the scheduler on this node. With `false` the node claims and runs no scheduled transition; entity writes still arm them, and a node with the scheduler enabled runs them.
- `CYODA_SCHEDULER_SCAN_INTERVAL` (duration, default: `1s`) — how often the node claims due tasks. It also claims at once when a run ends and the previous claim had filled every slot. Must be `> 0`.
- `CYODA_SCHEDULER_MAX_RUNS` (int, default: `8`) — the most scheduled runs in progress on this node at once. Must be `>= 1`.
- `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (int, default: `4`) — the most runs of one tenant on this node at once. Tenants take turns. Must be from `1` to `CYODA_SCHEDULER_MAX_RUNS`.
- `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (duration, default: `15s`) — how often the node writes its liveness record. Must be `> 0`.
- `CYODA_SCHEDULER_STALE_AFTER` (duration, default: `2m`) — how long a node's heartbeats must have stopped before another node may take over its tasks. It is also how long the tasks of a crashed node wait. Must be at least `50s + 3 × CYODA_SCHEDULER_HEARTBEAT_INTERVAL`. Set the same value on every node of a cluster.
- `CYODA_SCHEDULER_MAX_LOST_OWNERS` (int, default: `3`) — how many times a task may lose its owner, to a node that crashed or stopped heartbeating while it ran the task, before the task ends `FAILED` (`OWNER_LOST_REPEATEDLY`). Must be `>= 1`.
- `CYODA_SCHEDULER_RETRY_DELAY` (duration, default: `30s`) — the delay before the first retry of a run that failed safely. It doubles on each further failure. Must be `> 0`.
- `CYODA_SCHEDULER_RETRY_DELAY_MAX` (duration, default: `15m`) — the most the retry delay grows to. Must be `>= CYODA_SCHEDULER_RETRY_DELAY`.
- `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (duration, default: `20s`) — on shutdown, how long the node waits for its runs in progress before it cancels them. A run whose processor that is not declared `idempotent` is in flight on a compute member is not cancelled. Must be `>= 0`. See `cyoda help run` (SHUTDOWN TIMING).

Startup fails on an invalid value.

**When a node loses its heartbeat.** If the node's own heartbeats keep failing for `CYODA_SCHEDULER_STALE_AFTER` minus 40 seconds, it cancels its runs in progress and claims nothing until a heartbeat succeeds. So no other node takes a task over while its owner can still commit. A node that comes back from a storage outage takes over no other node's tasks until its own heartbeats have succeeded for `CYODA_SCHEDULER_STALE_AFTER` without a gap.

On PostgreSQL the scheduler has a pool of its own, sized by `CYODA_POSTGRES_SCHEDULER_CONNS`; see `cyoda help config database`.

## SEE ALSO

- config
- config.database
- workflows
- scheduled-tasks
- run
```

The 40 seconds are spec §6.3's `CommitBudget` (30 s) plus 10 s slack.

- [ ] **Step 3: `config.md:33`**

Replace the line with:

```markdown
- `config.scheduler` — scheduled-transition claiming, liveness, retries and shutdown drain
```

- [ ] **Step 4: `config/cluster.md`** — delete the `CYODA_DISPATCH_FORWARD_TIMEOUT`
bullet (`:32`) whole. Nothing replaces it.

- [ ] **Step 5: `README.md` "Scheduled transitions" (`:224-236`)** — replace the
section body (heading kept) with:

```markdown
A workflow transition with a `schedule` fires on its own after a delay. The delay is a static `delayMs`, or a `function` callout that computes the firing time (and an optional expiry) per entity when the transition is armed. Every node claims due transitions and runs them itself, and a task has one owner at a time. A processor that is not declared `idempotent` is never run twice by the scheduler. A failure that is safe to repeat is retried until the transition's `timeoutMs` passes. A task that cannot succeed ends `FAILED`, never moves its entity, and is listed by `GET /api/scheduled-tasks`. See `cyoda help workflows`, `cyoda help scheduled-tasks` and `cyoda help config scheduler`.

| Env var | Default | Effect |
|---------|---------|--------|
| `CYODA_SCHEDULER_ENABLED` | `true` | Run the scheduler on this node. |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | `1s` | How often the node claims due tasks. |
| `CYODA_SCHEDULER_MAX_RUNS` | `8` | Most runs in progress on this node. |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | `4` | Most runs of one tenant on this node; at most `MAX_RUNS`. |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | `15s` | How often the node proves it is alive. |
| `CYODA_SCHEDULER_STALE_AFTER` | `2m` | How long heartbeats must stop before another node takes the tasks over. At least `50s + 3 × heartbeat`; the same on every node. |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | `3` | Lost owners after which a task ends `FAILED`. |
| `CYODA_SCHEDULER_RETRY_DELAY` | `30s` | Delay before the first retry; doubles on each failure. |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | `15m` | Upper bound on the retry delay. |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | `20s` | How long shutdown waits for the runs in progress. |
| `CYODA_POSTGRES_SCHEDULER_CONNS` | `10` | PostgreSQL only: connections kept for the scheduler and the async-search heartbeat. At least `2`. |
```

- [ ] **Step 6: `README.md` "Compute-node callouts"** — delete the
`CYODA_DISPATCH_FORWARD_TIMEOUT` row (`:251`).

- [ ] **Step 7: Verify**

```
go test ./cmd/cyoda/help/...
    TestContentMarkdownSubsetLinter, TestSeeAlsoResolution (needs Q's scheduled-tasks.md),
    TestConfig_EnvVarCoverage, TestConfigAll_Complete, TestDefaultTree_ConfigClusterSubtopic,
    TestHelpContent_CrossReferencesUseAWorkingInvocation, TestHelpContent_NoIssueIDs → PASS
go test ./app/ -run TestRootConfigVars_MatchDefaults → PASS
git grep -n -e CYODA_SCHEDULER_DISTRIBUTION -e CYODA_SCHEDULER_COORDINATOR -e CYODA_SCHEDULER_REDISPATCH_BACKOFF \
  -e CYODA_SCHEDULER_BATCH_SIZE -e CYODA_SCHEDULER_EXPIRY_GRACE -e CYODA_DISPATCH_FORWARD_TIMEOUT \
  -- README.md cmd/cyoda/help/content → no output
grep -n 'scheduled-tasks' api/openapi.yaml | head -1 → the path Q added; if it is not `/scheduled-tasks`, correct the README sentence
```

If `TestSeeAlsoResolution` fails only on `scheduled-tasks` because Q has not
landed, drop that entry from `see_also` and SEE ALSO, commit, and add it back in
D-3.

- [ ] **Step 8: Commit**

```
git add cmd/cyoda/help/content/config/scheduler.md cmd/cyoda/help/content/config.md \
        cmd/cyoda/help/content/config/cluster.md README.md
git commit -m "docs(help): scheduler settings — claiming, liveness, retries, shutdown drain" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-2: the scheduler pool — `config/database.md` and `docs/plugins/POSTGRES.md`

> **Dropped (README C-D1).** BP-1 writes `config/database.md` and BP-6 writes `POSTGRES.md`, including the missing `scheduled_tasks` schema row below. Do not execute this task.

**Spec:** §10.2 (scheduler pool, tables), §11.

**Needs:** BP's config task (`CYODA_POSTGRES_SCHEDULER_CONNS` in
`plugins/postgres/config.go` and `plugin.go`) and BP's migration.

**Files:**
- Modify: `cmd/cyoda/help/content/config/database.md` — PostgreSQL list, after the `CYODA_POSTGRES_MIN_CONNS` bullet
- Modify: `docs/plugins/POSTGRES.md` — §Concurrency model (end), schema table (`:246-256`), configuration table (`:316-325`)

- [ ] **Step 1: Check what BP built**

```
grep -n 'SCHEDULER_CONNS' plugins/postgres/*.go
grep -n 'statement_timeout\|lock_timeout\|idle_in_transaction' plugins/postgres/*sched*.go plugins/postgres/pool*.go
ls plugins/postgres/migrations | tail -2
```

Expected: default `10`, minimum `2`; the scheduler pool sets `statement_timeout`
30 s, `idle_in_transaction_session_timeout` 10 s, `lock_timeout` 2 s, a 5 s
acquire timeout; one extra connection for the heartbeat. If any figure differs,
the text follows the code.

- [ ] **Step 2: `config/database.md`** — insert after the `CYODA_POSTGRES_MIN_CONNS` bullet:

```markdown
- `CYODA_POSTGRES_SCHEDULER_CONNS` — size of a second pool, apart from `CYODA_POSTGRES_MAX_CONNS`, for the scheduler's claims, heartbeats and outcome writes and for the async-search heartbeat and claim (default: `10`; at least `2`). Entity transactions never use it, so a saturated main pool cannot starve a heartbeat. One more connection is kept for the scheduler heartbeat alone. Its statements run under fixed limits: 30 seconds per statement, 2 seconds per lock wait, 5 seconds to get a connection. Allow `CYODA_POSTGRES_MAX_CONNS + CYODA_POSTGRES_SCHEDULER_CONNS + 1` connections per node in the database's `max_connections`.
```

- [ ] **Step 3: `POSTGRES.md` configuration table** — add after the
`CYODA_POSTGRES_MIN_CONNS` row:

```markdown
| `CYODA_POSTGRES_SCHEDULER_CONNS` | `10` | Size of the scheduler pool, a second `pgxpool.Pool` beside the main one, at `READ COMMITTED`. It serves every scheduled-task method that does not join an entity transaction (except `Query`, which uses the main pool) and the async-search heartbeat and claim. At least `2`. The heartbeat has one further connection of its own. |
```

- [ ] **Step 4: `POSTGRES.md` §Concurrency model** — add at the end of the section:

```markdown
**Scheduled tasks.** Task rows written by a joining `ScheduledTaskStore` method
go straight into the open entity transaction, at `REPEATABLE READ`, so
first-committer-wins covers them as it covers entities, and a row written by an
open transaction cannot be claimed until that transaction ends. Every other task
method runs on the scheduler pool (`CYODA_POSTGRES_SCHEDULER_CONNS`) at
`READ COMMITTED`, with `statement_timeout` 30 s,
`idle_in_transaction_session_timeout` 10 s and `lock_timeout` 2 s. `ClaimDue`
ranks due rows, locks them with `FOR UPDATE SKIP LOCKED` and claims them in one
transaction; a partial unique index allows one `RUNNING` task per entity.
`MarkUnsafe` takes a `FOR SHARE NOWAIT` lock on the task row, so a mark and a
claim of the same task never both succeed. A lock wait that times out
(`55P03`) is retried by the caller; for `MarkUnsafe` it means the task is busy.
```

- [ ] **Step 5: `POSTGRES.md` schema table** — add three rows after `search_job_results`:

```markdown
| `scheduled_tasks` | Scheduled-transition tasks: status, claim, attempts, last error | `id` (with `tenant_id` indexed) |
| `scheduled_task_marks` | One mark per task life, written before a processor not declared `idempotent` is dispatched | `(task_id, arm_token)` |
| `scheduler_owners` | Liveness of each node's scheduler, stamped with `now()` | `owner` |
```

These three tables are outside row-level security; every tenant-facing
statement carries `tenant_id`. Add that sentence directly under the table.

- [ ] **Step 6: Verify**

```
go test ./cmd/cyoda/help/...   TestConfig_EnvVarCoverage, TestConfigAll_Complete, TestContentMarkdownSubsetLinter → PASS
(cd plugins/postgres && go test -run 'Config' ./...)   the plugin's ConfigVars default test → PASS
grep -n 'scheduled_task_marks\|scheduler_owners' plugins/postgres/migrations/*.up.sql → the BP migration creates both
```

- [ ] **Step 7: Commit**

```
git add cmd/cyoda/help/content/config/database.md docs/plugins/POSTGRES.md
git commit -m "docs(postgres): the scheduler pool and the scheduled-task tables" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-3: `workflows.md` — what a scheduled run promises

**Spec:** §1, §2, §4, §5.1, §5.4–§5.7, §6.1, §6.4, §7, §8.1, §12.

**Needs:** E (the fire path, the mark, reconcile removing every other task),
R (retries, FAILED, shutdown), W (delete and import removal, the 409), Q
(`scheduled-tasks.md`), S (`SCHEDULED_TRANSITION_FAIL`).

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md` — front matter `see_also`; the
  `idempotent` bullet (`:202`); SCHEDULED TRANSITIONS *Fail-closed* paragraph
  (`:339-345`) and **Engine behaviour** (`:358-408`); workflow-selection
  paragraphs (`:478`, `:483`); ERRORS (`:579-590`); SEE ALSO (`:669-687`)

- [ ] **Step 1: Check the identifiers the text names**

```
grep -rn 'SCHEDULED_TRANSITION_FAIL\|UNSAFE_WORK_NOT_COMPLETED\|OWNER_LOST_REPEATEDLY\|EXPIRED_AFTER_FAILED_ATTEMPTS\|RUN_PANICKED\|STOPPED_AFTER_PARTIAL_COMMIT' \
  $(go list -m -f '{{.Dir}}' github.com/cyoda-platform/cyoda-go-spi)/types.go
ls cmd/cyoda/help/content/scheduled-tasks.md cmd/cyoda/help/content/errors/CONFLICT.md
```

All six constants and both files must exist.

- [ ] **Step 2: the `idempotent` bullet (`:202`)** — append to the end of the bullet:

```markdown
 The same declaration governs a scheduled run: the scheduler never runs a processor twice unless it is `idempotent` (see **SCHEDULED TRANSITIONS**).
```

- [ ] **Step 3: *Fail-closed* (`:339-345`)** — append after the sentence ending
"(see that error topic)." :

```markdown
When the arm happens inside the scheduler's own run — the fired transition's cascade ends in a state with a `function`-timed schedule — the same failure fails that run safely: nothing commits, and the task is retried (see **Retries** below).
```

- [ ] **Step 4: Replace the Engine behaviour block** — from the line
`**Engine behaviour (applies to both timing modes).** A scheduled` down to the
blank line before `**One-shot vs. polling.**` — with:

```markdown
**Engine behaviour (applies to both timing modes).** A scheduled transition is driven by a background scheduler, independently of cascade evaluation and of any other API call touching the entity. Each armed transition is stored as a **task**.

- **Arming.** On every write that leaves the entity in the transition's source state — the initial entry AND every subsequent settled write (an ordinary in-place data update or a self-loop) — the transition is (re-)armed with a freshly computed scheduled time. Static mode sets it to `now + delayMs`; function mode **invokes the callout** synchronously, inside that write's transaction, and each call fully replaces the previous scheduling decision. Every arm starts the task afresh: its attempts, lost owners and last error are cleared, and a `FAILED` task is `WAITING` again.
- **Settled-interval reset.** Because arming happens on *every* settled write, an entity written more often than its scheduled interval never reaches the fire. Authors relying on "escalate N after entry" semantics must account for this: routine touch-writes on a busy entity postpone the fire indefinitely (and, in function mode, make a callout on each such write).
- **One owner per run.** Every node claims due tasks from storage and runs them itself. A claimed task is `RUNNING` under one node, and every write that node makes for the run is checked against its claim. A node proves it is alive with a heartbeat; another node takes over its tasks only after the heartbeats have stopped for `CYODA_SCHEDULER_STALE_AFTER` (default `2m`). A node that is alive keeps its tasks, even when a run hangs. See `cyoda help config scheduler`.
- **One task per entity at a time.** While one scheduled transition of an entity runs, no other scheduled transition of the same entity is claimed. It runs after the first one ends.
- **Firing.** When the scheduled time is due, the owner evaluates the transition's criterion. A `true` (or absent) criterion fires the transition normally (processors run, state advances, `TRANSITION_MAKE` is recorded) and the task is removed. A `false` criterion **declines** the transition — the entity stays in its current state, and the task is removed and not retried (`TRANSITION_NOT_MATCH_CRITERION`). See "One-shot vs. polling" below for how to model a retry. A criterion that cannot be evaluated — its compute member is down, for example — is a failure, not `false`.
- **Retries.** A run that fails when nothing unsafe was handed to a compute member is retried: a criterion or function error, no compute member for the tag, a conflict with a concurrent write, a storage error, a failed `idempotent` processor, or a processor that provably never reached a compute member. The task goes back to `WAITING` with its attempts and last error, and is tried again after `CYODA_SCHEDULER_RETRY_DELAY` (default `30s`), doubling on each failure up to `CYODA_SCHEDULER_RETRY_DELAY_MAX` (default `15m`). With `timeoutMs`, retries stop at the deadline (below). Without `timeoutMs`, a failing task is retried without end and stays visible as `WAITING`.
- **Lateness / expiry (`timeoutMs`).** `timeoutMs` — set directly on a static schedule, or derived from a function schedule's expiry — sets the task's deadline: scheduled time + `timeoutMs`, on the owner's clock. A task picked up after its deadline on its first attempt is **expired**: it is removed without evaluating the criterion (`SCHEDULED_TRANSITION_EXPIRE`), the transition never fires and the entity stays put. A task that has already failed an attempt or lost an owner ends `FAILED` (`EXPIRED_AFTER_FAILED_ATTEMPTS`) instead: when an attempt fails after the deadline, or when it is picked up more than `CYODA_SCHEDULER_RETRY_DELAY` past the deadline. Retry delays are cut to the deadline, so the last retry comes at or before it. A node crash counts as a lost owner, so a task that was running on a crashed node, with a `timeoutMs` shorter than `CYODA_SCHEDULER_STALE_AFTER`, ends `FAILED` rather than expired. No `timeoutMs` (no expiry) means no deadline.
- **A processor is never repeated unless it is `idempotent`.** Before the owner sends a processor that is not declared `idempotent` to a compute member, it marks the task. If that processor may have reached a compute member and the run does not commit — the member failed or did not answer, a later step failed, or the owner crashed — the task ends `FAILED` (`UNSAFE_WORK_NOT_COMPLETED`) and the scheduler never runs it again. Declare `idempotent: true` only on a processor that is safe to repeat everywhere it reaches (see **Repeating a processor** above); its failures are then retried like any other safe failure.
- **Partial commit.** A `COMMIT_BEFORE_DISPATCH` processor in a cascade step after the fired transition commits the entity in that step's state. If the run stops after such a commit, the task ends `FAILED` (`STOPPED_AFTER_PARTIAL_COMMIT`): the entity has already moved, and running the transition again from its source state would be wrong. A `COMMIT_BEFORE_DISPATCH` processor of the fired transition itself commits the entity while it is still in the source state; a safe failure after it is retried from that committed state.
- **`FAILED`.** The reasons are `UNSAFE_WORK_NOT_COMPLETED`, `STOPPED_AFTER_PARTIAL_COMMIT`, `EXPIRED_AFTER_FAILED_ATTEMPTS`, `OWNER_LOST_REPEATEDLY` (the task lost its owner `CYODA_SCHEDULER_MAX_LOST_OWNERS` times, default `3`) and `RUN_PANICKED` (the run hit an internal error; the node is taken out of service, see `cyoda help run`). A `FAILED` task never moves the entity and is never claimed again. It is kept, and it is visible in `GET /scheduled-tasks` (see `cyoda help scheduled-tasks`), in the metrics (see `cyoda help telemetry`), in the server log at ERROR with a ticket, and as a `SCHEDULED_TRANSITION_FAIL` audit event on the entity. It ends when the entity is written in the source state (the task is armed afresh), when the entity leaves that state (`SCHEDULED_TRANSITION_CANCEL`), when a workflow import stops scheduling the transition, or when the entity is deleted. There is no separate retry or dismiss operation: an entity write does both.
- **Entity writes, deletes and imports.** An entity write re-arms or removes the entity's tasks in its own transaction. A task whose transition the selected workflow does not schedule from the entity's current state is removed and recorded as `SCHEDULED_TRANSITION_CANCEL`. Deleting an entity — singly, by condition, or all of a model's — removes its tasks with no audit event. A workflow import saves the workflows first, then removes the model's tasks whose transition none of the model's workflows schedules any more.
- **A write can answer `409` when it races the scheduler.** The scheduler writes a task when it claims it, commits part of a run, records an attempt, ends it `FAILED`, or hands it back. A client write that re-arms or removes a task the scheduler wrote after the write began fails with a retryable `409 CONFLICT` — the same answer as a write that races the fire itself. A delete or a workflow import first retries on the server, three times, and answers `409` only if the conflict persists; a batched conditional delete reports it for the entity in its result instead. A repeated import saves the same workflows and completes the removal. See `cyoda help errors CONFLICT`.
- **Explicitly firing a scheduled transition by name still returns** HTTP 400 `TRANSITION_NOT_FOUND`, with the message `transition "X" in state "Y" is scheduled and fires automatically; it is not manually fireable`. Same code returned when a transition is `disabled: true` — same semantic: "the transition exists but is not currently dispatchable from the caller's POV." The entity remains in the source state. To allow early firing, give the state an ordinary manual transition alongside the scheduled one.
- **Audit trail.** Arming, firing, expiry, cancellation and failure each emit a dedicated event: `SCHEDULED_TRANSITION_ARM`, `SCHEDULED_TRANSITION_FIRE` (alongside the ordinary `TRANSITION_MAKE`), `SCHEDULED_TRANSITION_EXPIRE`, `SCHEDULED_TRANSITION_CANCEL`, `SCHEDULED_TRANSITION_FAIL`. `FAIL` carries the transition, the source state, the reason, the attempts and the lost owners. A loopback that re-arms the same state emits only `ARM`, not `CANCEL`. `CANCEL` means the entity left the source state, or the workflow selected for the entity does not schedule the transition from that state — found at a write (see *Workflow-level selection*) or when the task came due. A task whose entity carries no transaction id to guard the fire is also cancelled, and logged at ERROR. A task past its deadline on its first attempt records `EXPIRE`, not `CANCEL`, even when it is obsolete too: lateness is decided from the stored task and the clock before the workflow is consulted. A safe failure records no audit event; it shows in the task's attempts and last error, and in the log at WARN.
- **Callbacks in a scheduled run.** A processor's callbacks join the run's transaction, as in any transition. A callback that writes the entity being fired (see *a processor must not save the entity it is processing for* above) also holds the task. When a processor that is not `idempotent`, or a `COMMIT_BEFORE_DISPATCH` step, follows in the same run, the run cannot mark or commit the task, so the attempt fails safely — and every retry fails the same way. The run never hangs and never repeats unsafe work.
- **Criteria and functions must not trigger unsafe work.** A criterion and a `schedule.function` are treated as safe to repeat and are not marked. One whose callbacks trigger a processor that is not `idempotent` is not supported: that processor can run twice.
- **Shutdown.** A node that shuts down stops claiming, waits `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (default `20s`) for its runs, then hands back the ones that sent nothing unsafe: another node claims them at once, and the attempt is not counted. A run whose processor that is not `idempotent` is in flight is allowed to finish. See `cyoda help run` (SHUTDOWN TIMING).
```

- [ ] **Step 5: workflow-level selection (`:478`)** — replace the paragraph
"A pending scheduled task the newly selected workflow no longer declares …" with:

```markdown
A pending scheduled task that the newly selected workflow does not schedule from the entity's current state is removed by the write that caused the re-bind, in that write's transaction, and recorded as `SCHEDULED_TRANSITION_CANCEL`.
```

and in the bullet at `:483` replace "and can silently retire a scheduled
transition that was acting as a time-based control." with "and can retire a
scheduled transition that was acting as a time-based control; the retirement is
recorded as `SCHEDULED_TRANSITION_CANCEL`, and nothing else reports it."

- [ ] **Step 6: ERRORS (`:579-590`)** — add after the `errors.VALIDATION_FAILED` line:

```markdown
- `errors.CONFLICT` — `409` — workflow import: removing the tasks of scheduled transitions no workflow schedules any more still conflicted with the scheduler after three tries; retryable, and repeating the same import completes it
```

- [ ] **Step 7: `see_also` (front matter) and SEE ALSO** — add, in both lists,
after `errors.SCHEDULE_FUNCTION_INVALID_RESULT`:

```
scheduled-tasks
config.scheduler
errors.CONFLICT
```

(front matter entries indented `  - ` as the others; SEE ALSO entries `- `).
If D-1 dropped `scheduled-tasks` from `config/scheduler.md`, add it back there now.

- [ ] **Step 8: Verify**

```
go test ./cmd/cyoda/help/...   TestContentMarkdownSubsetLinter, TestSeeAlsoResolution, TestRunHelp_NoDuplicateSeeAlso,
                               TestRunHelp_SeeAlsoUsesCLISyntax, TestHelpContent_CrossReferencesUseAWorkingInvocation,
                               TestHelpContent_NoIssueIDs, TestHelpTopics_ConditionTypeParity → PASS
grep -n 'grace band\|picked up more than `timeoutMs`\|is \*\*not\*\* cancelled by the write\|silently retire' \
  cmd/cyoda/help/content/workflows.md → no output
grep -c 'SCHEDULED_TRANSITION_FAIL' cmd/cyoda/help/content/workflows.md → 2
go run ./cmd/cyoda help workflows | sed -n '/SCHEDULED TRANSITIONS/,/CRITERIA/p' → read it once, top to bottom
```

- [ ] **Step 9: Commit**

```
git add cmd/cyoda/help/content/workflows.md cmd/cyoda/help/content/config/scheduler.md
git commit -m "docs(help): a scheduled run has one owner; FAILED, retries, the 409 and the callback anti-pattern" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-4: `idempotent` in the OpenAPI spec, and why the schema version stays

**Spec:** §12 (`idempotent`, no schema-version bump).

**Needs:** Q-1 (same file; rebase on it), E.

**Files:**
- Modify: `api/openapi.yaml:10513-10523` (the `idempotent` property of the processor config)
- Modify (generated): `api/generated.go`
- Modify: `docs/workflow-schema-versioning.md` — "When NOT to bump" list (`:43-48`) and a new entry after "Exported schedule omits `delayMs` when function-driven (v0.9.0)" (`:143-163`)

- [ ] **Step 1: the description** — replace the `description: |` block of
`idempotent` with:

```yaml
          description: |
            The workflow author's declaration that running this processor
            again is safe — for cyoda and for every system the processor
            touches. When true, the work may be given to another compute
            member after a member that received it went silent or dropped
            its connection, and a scheduled transition whose run failed
            after this processor was sent is retried. When false (the
            default) neither happens, because the first member may have
            acted: such a scheduled run ends FAILED with the reason
            UNSAFE_WORK_NOT_COMPLETED and is not run again. Criteria and
            functions are always treated as safe to repeat and carry no
            such field.
```

- [ ] **Step 2: Regenerate** — `go generate ./api`. Only the comment on the
`Idempotent` field of the generated type changes: `git diff --stat api/generated.go`
shows one small hunk.

- [ ] **Step 3: "When NOT to bump" list** — add as the last bullet of the list
at `:43-48`:

```markdown
- Widening what the engine does with a value of a field the DTO already carries, when the field's shape, default, validation and export stay the same. The version contract scopes what an import accepts and what an export emits, not every runtime consequence of a value.
```

- [ ] **Step 4: New entry** — after the "Exported schedule omits `delayMs`…"
entry, before `## Required commit-/PR-time checks`:

```markdown
### `idempotent` also governs scheduled runs (v0.9.0)

A processor's `config.idempotent`, added in 1.5, now also decides what the
scheduler does when a scheduled run fails after sending that processor to a
compute member. With `true` the run is retried. With `false`, the default, the
task ends `FAILED` (`UNSAFE_WORK_NOT_COMPLETED`) and is not run again. Before,
the scheduler could start such a run a second time while the first was still in
progress, whatever the field said.

The field, its type, its default, its validation and its export are unchanged;
every document 1.5 accepted is accepted and exported byte-identically. What
changes is what the engine does with a value it already read — the §"When NOT to
bump" "widening what the engine does with a value" case. The declaration's own
text already said "running this again is safe", which is the property the
scheduler now relies on. No `CurrentSchemaVersion` or `SupportedSchemaRanges`
change.
```

- [ ] **Step 5: Verify**

```
go build ./... && go vet ./api/...
go test ./api/... ./cmd/cyoda/help/...   TestHelpContent_CrossReferencesUseAWorkingInvocation (reads openapi.yaml and generated.go) → PASS
go test ./internal/domain/workflow/ -run 'TestOpenAPIWorkflowVersionContract|TestCurrentSchemaVersionIsSupported' → PASS
git diff api/openapi.yaml | grep '^[-+] ' | grep -v description → only the description lines changed
```

- [ ] **Step 6: Commit**

```
git add api/openapi.yaml api/generated.go docs/workflow-schema-versioning.md
git commit -m "docs(api): idempotent governs scheduled runs too; no schema-version bump" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-5: shutdown timing — `run.md`, the Helm chart, `helm.md`

**Spec:** §6.4 (steps 1–6, grace period), §12 (Helm chart).

**Needs:** R's `run.go` wiring (the scheduler drain before the server drains, on
the signal path; the same steps from `a.Shutdown()` on the server-failure path).

**Files:**
- Modify: `cmd/cyoda/help/content/run.md` — SIGNALS (`:264`), SHUTDOWN TIMING (`:283-289`)
- Modify: `deploy/helm/cyoda/templates/statefulset.yaml` (pod spec)
- Modify: `deploy/helm/cyoda/values.yaml` (after `affinity: {}`)
- Modify: `deploy/helm/cyoda/values.schema.json` (`properties`)
- Modify: `deploy/helm/cyoda/Chart.yaml` (`version:`, `artifacthub.io/changes`)
- Modify: `deploy/helm/cyoda/README.md` (new subsection after "Scale to 3 replicas (cluster mode)")
- Modify: `cmd/cyoda/help/content/helm.md` — VALUES (after `affinity`), CRDS / OBJECTS (StatefulSet line)
- Modify: `.github/workflows/helm-chart-ci.yml` (two guard steps)

- [ ] **Step 1: Check the order R wired**

```
grep -n 'Drain\|scheduler' cmd/cyoda/run.go app/app.go | head -20
```

The scheduler's `Drain` must run before the HTTP, admin and gRPC drains on the
signal path. If it does not, stop: the text below would be false.

- [ ] **Step 2: Chart guards first (the failing check)** — add two steps to
`helm-lint-and-validate` in `.github/workflows/helm-chart-ci.yml`, after
"helm template — default (Gateway API, replicas=1)":

```yaml
      - name: helm template — pods get the shutdown grace period
        run: |
          grep -q "terminationGracePeriodSeconds: 360" /tmp/default.yaml

      - name: helm template — schema rejects a zero grace period
        run: |
          set +e
          output=$(helm template cyoda deploy/helm/cyoda \
            --set postgres.existingSecret=test-dsn \
            --set jwt.existingSecret=test-jwt \
            --set cluster.hmacSecret.existingSecret=test-hmac \
            --set monitoring.metricsBearer.existingSecret=test-metrics-bearer \
            --set gateway.parentRefs[0].name=test-gw \
            --set gateway.http.hostnames[0]=cyoda.example.com \
            --set gateway.grpc.hostnames[0]=grpc.cyoda.example.com \
            --set terminationGracePeriodSeconds=0 \
            2>&1)
          status=$?
          set -e
          if [ "$status" -eq 0 ] || ! echo "$output" | grep -q "/terminationGracePeriodSeconds"; then
            echo "FAIL: expected schema rejection of terminationGracePeriodSeconds=0; got:"
            echo "$output"
            exit 1
          fi
          echo "PASS: schema rejected a zero grace period."
```

Run both locally against the unchanged chart and see them fail:

```
helm template cyoda deploy/helm/cyoda --set postgres.existingSecret=test-dsn --set jwt.existingSecret=test-jwt \
  --set cluster.hmacSecret.existingSecret=test-hmac --set monitoring.metricsBearer.existingSecret=test-metrics-bearer \
  --set 'gateway.parentRefs[0].name=test-gw' --set 'gateway.http.hostnames[0]=cyoda.example.com' \
  --set 'gateway.grpc.hostnames[0]=grpc.cyoda.example.com' > /tmp/default.yaml
grep -q "terminationGracePeriodSeconds: 360" /tmp/default.yaml; echo $?        → 1
(same command) --set terminationGracePeriodSeconds=0; echo $?                  → 0 (accepted: the schema allows extra keys)
```

- [ ] **Step 3: `values.yaml`** — add after `affinity: {}`:

```yaml
# -- Seconds Kubernetes waits after SIGTERM before it kills a cyoda pod.
# A node first drains its scheduled runs. A run whose processor that is not
# declared idempotent is in flight on a compute member is allowed to finish,
# which can take a whole callout deadline. The worst case is
#   max(CYODA_SCHEDULER_SHUTDOWN_DRAIN, callout deadline) + 70s
# where
#   callout deadline = (1 + CYODA_RETRY_FIXED_NUM_RETRIES)
#                        × CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS
#                      + CYODA_DISPATCH_WAIT_TIMEOUT
#                      + CYODA_CALLOUT_HANDOVER_ALLOWANCE
# At the binary's defaults: max(20s, 4 × 60s + 5s + 30s) + 70s = 345s.
# Raise this when you raise any of those settings. See `cyoda help run`.
terminationGracePeriodSeconds: 360
```

- [ ] **Step 4: `values.schema.json`** — add to `properties` (alphabetical
position is not required; put it after `affinity`):

```json
    "terminationGracePeriodSeconds": {
      "type": "integer",
      "minimum": 1
    },
```

- [ ] **Step 5: `statefulset.yaml`** — in the pod `spec:`, after
`automountServiceAccountToken: false`:

```yaml
      terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
```

- [ ] **Step 6: `Chart.yaml`** — `version: 0.8.4` → `version: 0.9.0`
(`appVersion` is not touched; `bump-chart-appversion.yml` moves it at the
release). Replace the `artifacthub.io/changes` list with:

```yaml
  artifacthub.io/changes: |
    - terminationGracePeriodSeconds defaults to 360, so a node can finish its scheduled runs before it is killed
```

- [ ] **Step 7: Chart `README.md`** — insert after the "Scale to 3 replicas
(cluster mode)" subsection:

````markdown
### Shutdown grace period

The chart sets `terminationGracePeriodSeconds: 360`. On `SIGTERM` a node first
drains its scheduled runs, and a run whose processor that is not declared
`idempotent` is in flight on a compute member is allowed to finish. The worst
case from `SIGTERM` to exit is

```
max(CYODA_SCHEDULER_SHUTDOWN_DRAIN, callout deadline) + 70s
callout deadline = (1 + CYODA_RETRY_FIXED_NUM_RETRIES) × CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS
                   + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE
```

— 345 s at the binary's defaults. If you raise any of those settings through
`extraEnv`, raise `terminationGracePeriodSeconds` to match. A pod killed before
it has recorded its runs' outcomes leaves its tasks to another node, which takes
them over after `CYODA_SCHEDULER_STALE_AFTER` (default `2m`). See
`cyoda help run` (SHUTDOWN TIMING).
````

- [ ] **Step 8: `run.md` SIGNALS (`:264`)** — replace the `SIGINT` bullet with:

```markdown
- `SIGINT` (Ctrl+C) — triggers graceful shutdown. The scheduler drains its runs first; then the HTTP, admin and gRPC servers drain in-flight requests within a 10-second deadline, and the storage backend is closed. The process exits with code 0. See SHUTDOWN TIMING.
```

- [ ] **Step 9: `run.md` SHUTDOWN TIMING (`:283-289`)** — replace the section
body (heading kept) with:

```markdown
On the first signal the scheduler stops first, while compute members and their callbacks can still reach the node:

1. It stops claiming scheduled transitions. From here on no run sends a new processor that is not declared `idempotent`.
2. It waits up to `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (default `20s`) for the runs in progress.
3. It cancels the runs still going, except a run whose processor that is not `idempotent` is in flight on a compute member. That callout may finish or reach its own deadline, and the run may then carry on with safe steps and commit.
4. It waits for every run to record its outcome: at most the longest remaining callout deadline plus 45 seconds.
5. It hands back the tasks whose runs ended without an outcome, and stops its heartbeat.

A run cut at step 3 that had sent nothing unsafe is claimed again at once, by any node, and the attempt is not counted. When a server fails instead, the same steps run after the servers have stopped.

The server drains follow. Their graceful shutdown deadline is **10 seconds**, applied separately to the HTTP server, the admin server and the gRPC server; the three drain concurrently, so the server drains take about 10 seconds in total, not 30. The value is not configurable. A gRPC drain that outlives its deadline is cut off with a hard stop.

Three steps follow the server drains, in order: `app.Shutdown()` gives in-flight async search jobs up to 5 seconds to finish before releasing them for another node to reclaim, and takes the node out of the cluster; `app.Close()` releases backend resources (database connection pools), with no timeout of its own; and the telemetry flush gets up to 10 seconds. Everything after the scheduler therefore takes about 25 seconds at worst, plus the cluster leave and the storage close. An idle node exits in well under a second.

The worst case from signal to exit:

- With no processor that is not `idempotent` in flight: `CYODA_SCHEDULER_SHUTDOWN_DRAIN` + 70 seconds — 90 s at the defaults.
- With one in flight: max(`CYODA_SCHEDULER_SHUTDOWN_DRAIN`, callout deadline) + 70 seconds. The callout deadline is `(1 + CYODA_RETRY_FIXED_NUM_RETRIES) × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE`: 225 s in all at the defaults, and 345 s when the answer limit is `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (default `60000`).

The 70 seconds are the 30-second commit budget, 15 seconds of slack and the 25 seconds above.

In Kubernetes, the pod `terminationGracePeriodSeconds` must cover the worst case, or the kubelet sends `SIGKILL` before the node has recorded its runs' outcomes; another node then takes those tasks over after `CYODA_SCHEDULER_STALE_AFTER`, as lost owners. The Helm chart sets `360`. Raise it when you raise the tries, the answer limit, the wait or the hand-over allowance (see `cyoda help helm`).
```

- [ ] **Step 10: `helm.md`** — VALUES, after the `affinity` entry:

```markdown
**`terminationGracePeriodSeconds`** — integer — default `360`
Seconds Kubernetes waits after `SIGTERM` before it kills a pod. It must cover the node's worst-case shutdown, `max(CYODA_SCHEDULER_SHUTDOWN_DRAIN, callout deadline) + 70` seconds, which is 345 s at the binary's defaults with the maximum answer limit. Raise it when you raise `CYODA_RETRY_FIXED_NUM_RETRIES`, `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`, `CYODA_DISPATCH_WAIT_TIMEOUT` or `CYODA_CALLOUT_HANDOVER_ALLOWANCE`. Must be `>= 1`. See `cyoda help run` (SHUTDOWN TIMING).
```

and in CRDS / OBJECTS, in the `StatefulSet` bullet, after
"`updateStrategy: RollingUpdate`.", insert "`terminationGracePeriodSeconds`
from values (default `360`)."

- [ ] **Step 11: Verify**

```
helm lint deploy/helm/cyoda --set postgres.existingSecret=lint-placeholder --set jwt.existingSecret=lint-placeholder \
  --set cluster.hmacSecret.existingSecret=lint-placeholder --set monitoring.metricsBearer.existingSecret=lint-placeholder → 0 failed
(the two commands of step 2) → grep exits 0; the zero value is refused naming /terminationGracePeriodSeconds
kubeconform -strict -kubernetes-version 1.31.0 -skip HTTPRoute,GRPCRoute,ServiceMonitor /tmp/default.yaml → valid
go test ./cmd/cyoda/help/...   TestContentMarkdownSubsetLinter, TestHelpContent_CrossReferencesUseAWorkingInvocation → PASS
grep -n 'stops the scheduler' cmd/cyoda/help/content/run.md → no output
```

The numbered list in step 9 is at column 0, which the markdown linter accepts;
it refuses only indented bullets.

- [ ] **Step 12: Commit**

```
git add cmd/cyoda/help/content/run.md cmd/cyoda/help/content/helm.md \
        deploy/helm/cyoda/templates/statefulset.yaml deploy/helm/cyoda/values.yaml \
        deploy/helm/cyoda/values.schema.json deploy/helm/cyoda/Chart.yaml deploy/helm/cyoda/README.md \
        .github/workflows/helm-chart-ci.yml
git commit -m "feat(helm): a 360 s grace period so a node can finish its scheduled runs; shutdown timing documented" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-6: `telemetry.md` — the scheduler instruments

**Spec:** §9.

**Needs:** R's metrics task.

**Files:**
- Modify: `cmd/cyoda/help/content/telemetry.md` — after the cluster membership
  metrics (`:121-125`), and ATTRIBUTE VOCABULARY (`:150-151`)

- [ ] **Step 1: Check R's instruments**

```
grep -rn '"cyoda.scheduler\.' internal/scheduler/*.go | grep -v _test
grep -rn 'observability.Meter()' app/app.go | grep -i sched
```

Six names, as `interfaces.md` § Metrics lists, created from
`observability.Meter()` with no `CYODA_OTEL_ENABLED` condition. The meter
provider always carries the Prometheus exporter (`docs/ARCHITECTURE.md` §11), so
they are on `/metrics` whatever that setting is. If R made them conditional,
change "regardless of `CYODA_OTEL_ENABLED`" below to "when `CYODA_OTEL_ENABLED=true`".

- [ ] **Step 2: Insert** after the `cyoda.cluster.tags.lists_outstanding` bullet:

```markdown
Scheduler metrics are exposed on every node that runs the scheduler (`CYODA_SCHEDULER_ENABLED=true`), regardless of `CYODA_OTEL_ENABLED`:

- `cyoda.scheduler.runs` — `Int64Counter` — scheduled runs that ended; labeled by `outcome`: `fired`, `declined`, `expired`, `cancelled`, `attempt_failed` (a safe failure, to be retried), `failed` (the task ended `FAILED`), `superseded` (an entity write or another claim replaced the run), `self_cancelled` (the node's own heartbeats failed), `shutdown_cancelled`, `panicked`. Alarm on `failed` and `panicked`
- `cyoda.scheduler.run.duration` — `Float64Histogram`, unit `s` — duration of one run, from claim to recorded outcome; labeled by `outcome`
- `cyoda.scheduler.runs.in_progress` — `Int64UpDownCounter` — runs in progress on this node; at most `CYODA_SCHEDULER_MAX_RUNS`
- `cyoda.scheduler.claims` — `Int64Counter` — tasks claimed; labeled by `reason`: `due`, or `owner_lost` (taken over from a node whose heartbeats stopped). A steady `owner_lost` rate points at nodes that crash or lose the database
- `cyoda.scheduler.heartbeat.failures` — `Int64Counter` — heartbeats that failed. Failures that last make the node cancel its runs (`self_cancelled`)
- `cyoda.scheduler.bookkeeping.retries` — `Int64Counter` — retried writes of a run's outcome. A rising count means the database is refusing or blocking them; each retry is also logged at WARN

No `cyoda.scheduler.*` metric carries a tenant, a task, an entity or a node id. Each run has a `scheduler.run` span carrying its outcome. The tasks behind a `failed` count are listed by `GET /scheduled-tasks?status=FAILED` (see `cyoda help scheduled-tasks`).
```

- [ ] **Step 3: ATTRIBUTE VOCABULARY** — replace the `outcome` line with:

```markdown
- `outcome` — outcome label of `cyoda.callout.tries`, `cyoda.callout.handovers`, `cyoda.callout.superseded`, `cyoda.scheduler.runs` and `cyoda.scheduler.run.duration`; a closed set for each
```

and add after it:

```markdown
- `reason` — claim label of `cyoda.scheduler.claims` (`due` or `owner_lost`)
```

- [ ] **Step 4: Verify**

```
go test ./cmd/cyoda/help/...   TestContentMarkdownSubsetLinter, TestSeeAlsoResolution → PASS
for m in runs run.duration runs.in_progress claims heartbeat.failures bookkeeping.retries; do
  grep -q "\"cyoda.scheduler.$m\"" internal/scheduler/*.go || echo "missing $m"; done → no output
```

- [ ] **Step 5: Commit**

```
git add cmd/cyoda/help/content/telemetry.md
git commit -m "docs(help): the scheduler's six instruments" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-7: `ARCHITECTURE.md` and the other reference documents

**Spec:** §1–§10, §12 (`ARCHITECTURE.md` lines, `reaper.go:18`).

**Needs:** every code stream (S, BM, BQ, BP, K, E, R, W, Q).

**Files:**
- Modify: `docs/ARCHITECTURE.md` — `:94`, `:230`, `:382`, `:391`, `:772-773`, `:940`, `:943`, `:1359`, `:1506`, `:1550-1553`, `:1896-1914`, `:1992`, `:2248`; new §4.8; §11; §14.4; §14.6
- Modify: `docs/CONSISTENCY.md` §1 (end)
- Modify: `docs/cloud-parity/tenant-id-grammar.md:70`
- Modify: `docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis-playbook.md:59` (row F6)
- Modify: `internal/domain/search/reaper.go:16-22` (comment only)

`ARCHITECTURE.md` is a reference: present tense, no history. Audit the whole
document on this touch: after the edits, the `grep` in step 8 must print
nothing.

- [ ] **Step 1: Point edits**

| Line | Replace | With |
|---|---|---|
| `:94` | `Scheduled-transition dispatch loop` | `Scheduled-transition claim loop, heartbeat and watchdog (§4.8)` |
| `:230` | `peer dispatch bodies, scheduler payloads, gossip envelopes,` | `peer dispatch bodies, gossip envelopes,` |
| `:382` | `, and the scheduler's dispatch goroutine.` | `, and the scheduler's goroutines — the claim loop, each run, the heartbeat and the watchdog.` |
| `:382` | `a log line for the reaper and for a scheduled fire, the latter with no caller to answer)` | `a log line for the reaper, and a task ended FAILED with RUN_PANICKED for a scheduled run, which has no caller to answer)` |
| `:382` | the last sentence, from `That is why the scheduler site latches too` to the end of the paragraph | `A scheduled run is engine work like any request, so its panic latches the node too, and its task is not retried: running it again on another node would spread the problem (§4.8).` |
| `:391` | `tx-affinity proxying, cluster dispatch and the peer scheduler RPC all keep reaching the node, and the scheduler's round-robin distribution does not read node liveness, so it retains its share of every scan.` | `tx-affinity proxying and callout hand-overs keep reaching the node. The node's own scheduler stops claiming when the flag latches, but keeps heartbeating, so the runs it has in progress are not taken over while they may still commit.` |
| `:772-773` | `The scheduler's peer RPC signs its requests and answers the same way. Both bodies are JSON encoded` | `The hand-over's request and answer bodies are JSON encoded` |
| `:940` | `the hand-over transport, the scheduler's peer RPC, the HTTP reverse proxy` | `the hand-over transport, the HTTP reverse proxy` |
| `:943` | `` `CYODA_DISPATCH_FORWARD_TIMEOUT` bounds the scheduler's peer RPC only.`` | (delete the sentence) |
| `:1359` | `(precedent: `` `ScheduledTaskStore.ScanDue` ``)` | `(as are `` `ScheduledTaskStore.ClaimDue` `` and the scheduler's owner methods)` |
| `:1506` | item 4 whole | `4. **`FireScheduledTransition(ctx, task, maxLostOwners, retryDelay)`** -- Runs one claimed scheduled task (§4.8). Returns a `RunReport` rather than an `*EngineResult`: the caller is the scheduler, which records the outcome.` |
| `:1992` | the `CYODA_DISPATCH_FORWARD_TIMEOUT` row | (delete the row) |
| `:2248` | `` `CYODA_DISPATCH_CONNECT_TIMEOUT` (2s) bounds opening the connection only; `CYODA_DISPATCH_FORWARD_TIMEOUT` (30s) bounds the scheduler's peer RPC, not this.`` | `` `CYODA_DISPATCH_CONNECT_TIMEOUT` (2s) bounds opening the connection only.`` |

- [ ] **Step 2: Audit table (`:1550-1553`)** — add after the `SCHEDULED_TRANSITION_CANCEL` row:

```markdown
| `SCHEDULED_TRANSITION_FAIL` | `SMEventScheduledTransitionFailed` | Scheduled task ended FAILED; kept, and never moves the entity |
```

and change the `SCHEDULED_TRANSITION_CANCEL` description to `Scheduled task
removed: the entity left the state, or the selected workflow no longer
schedules the transition`.

- [ ] **Step 3: New §4.8** — insert after §4.7 (before `---` and `## 5. Workflow Engine`):

```markdown
### 4.8 Scheduled Transitions

A scheduled transition is stored as a task in `ScheduledTaskStore`
(`cyoda-go-spi`). `internal/scheduler` runs one claim loop per node, with a
heartbeat goroutine and a watchdog. There is no coordinator: every node claims
due tasks and runs them itself, so the node that decides is the node that runs,
and no node reads a cluster view to schedule.

**Terms.** Every write that arms a task draws a new random *arm token* and
starts a new *life*. Every claim draws a new random *claim token*. The *owner* is
the node incarnation — a UUID drawn when the process starts — that holds the
claim. A *fenced* store call is accepted only if the task's current arm token and
claim token are the ones given; otherwise it returns `spi.ErrStaleClaim`.

**Claiming.** Every `CYODA_SCHEDULER_SCAN_INTERVAL` the loop calls
`ClaimDue` with a limit of `CYODA_SCHEDULER_MAX_RUNS` minus the runs in
progress and a per-tenant limit of `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT`. A
claimable task is `WAITING` and due, or — only once the node's own heartbeats
have run without a gap for `STALE_AFTER` — `RUNNING` under an owner whose
liveness record is missing or older than `STALE_AFTER` by the store clock
(a *lost owner*; `lostOwners` goes up by one). One task per entity is `RUNNING`
at a time. Each tick also calls `GiveBackIdle`, which returns to `WAITING` any
task this owner holds without a live run.

**Fencing.** A run re-reads its task at the start of every segment and ends
`superseded` if the life or the claim changed. Every commit of a run writes the
task row — the final removal or re-arm, or `StampSegment` before a
`COMMIT_BEFORE_DISPATCH` segment commit — and task rows are under
first-committer-wins on every backend (C1). A commit whose task was reclaimed,
re-armed or removed since it began therefore fails with `spi.ErrConflict`. No
claim check is needed inside the entity transaction.

**Liveness and the watchdog.** `Heartbeat` upserts the owner's liveness record,
stamped by the store clock, every `CYODA_SCHEDULER_HEARTBEAT_INTERVAL`. The
watchdog arms a timer at `W = STALE_AFTER − CommitBudget − 10 s` from the moment
before each successful heartbeat acquired its connection. When it fires the node
cancels every run and claims nothing until a heartbeat succeeds. Every commit of
a run checks that cancellation first, and a commit under way holds its task-row
lock, which a claim skips (C6); so no other node can reclaim a task while its
owner still commits. `STALE_AFTER ≥ CommitBudget + 10 s + 10 s + 3 ×
HEARTBEAT_INTERVAL` keeps one slow or failed heartbeat from self-cancelling. A
node that heartbeats keeps its tasks even when a run hangs; liveness is not
progress.

**The unsafe mark.** Before every dispatch of a processor whose
`config.idempotent` is not true, the engine calls `MarkUnsafe`, which never
joins the run's transaction and so survives its rollback. The run also keeps in
memory whether unsafe work may have reached a compute member; only the callout
coordinator's `NotHandedOff` proof clears it. If the run does not commit and
unsafe work may have reached a member, or a later claim finds the mark, the task
ends `FAILED` (`UNSAFE_WORK_NOT_COMPLETED`). Criteria and functions are
repeat-safe by rule and are not marked.

**Outcomes.** A committed run removes its task (fired, declined, expired,
cancelled) or re-arms it. Otherwise the scheduler records the outcome with a
fenced write on `context.WithoutCancel`, retried with backoff until it is
accepted or refused: `RecordAttempt` for a safe failure (back to `WAITING`,
next attempt after `RETRY_DELAY × 2^(attempts−1)`, capped by `RETRY_DELAY_MAX`
and the deadline), or `Fail` with its reason and the `SCHEDULED_TRANSITION_FAIL`
audit event in one transaction. Only a deterministic store rejection
(`spi.ErrStoreRejected`) latches the node. `lastError` is visible to tenant
users and passes an allow-list; anything outside it is recorded as
`internal error [ticket: …]`.

**Entity writes.** `ReconcileForEntity` arms the new arm set, each task as a new
life, and removes every other task of the entity in the write's transaction.
Entity deletes remove the entity's tasks in the same transaction; a workflow
import removes the model's tasks that no workflow schedules, after saving the
workflows. A client write that conflicts with the scheduler on a task row gets
a retryable `409`; deletes and imports retry on the server first.

**Shutdown.** Before the servers drain the scheduler stops claiming, waits
`CYODA_SCHEDULER_SHUTDOWN_DRAIN`, cancels the runs still going except one whose
unsafe callout is in flight, waits for outcomes, gives back what ended without
one, stops the heartbeat and retires the owner. See `help/run.md` SHUTDOWN
TIMING for the bound.

**Storage clauses** every backend meets, pinned by the `ScheduledTasks` suite
of `spitest`: C1 first-committer-wins on task rows; C2 a joining read sees the
transaction's staged writes; C3 a mark and a claim of one task serialise; C4
heartbeats and claims have connections entity transactions cannot starve; C5
refusals are recognisable (`ErrConflict`, `ErrStaleClaim`); C6 a task row
written by an open transaction is not claimable, and `MarkUnsafe` answers
`ErrTaskBusy` for it. PostgreSQL meets C1 and C6 through `REPEATABLE READ` and
row locks, and C4 through its scheduler pool (`CYODA_POSTGRES_SCHEDULER_CONNS`);
memory and SQLite through task-row keys in their commit-time conflict check.

The Cloud-facing contract is `docs/cloud-parity/scheduled-transitions.md`.
```

- [ ] **Step 4: §9 Configuration Reference** — in the PostgreSQL table
(`:1900-1912`) add after `CYODA_POSTGRES_MIN_CONNS`:

```markdown
| `CYODA_POSTGRES_SCHEDULER_CONNS` | `10` | Scheduler pool: scheduled-task claims, heartbeats and outcomes, and the async-search heartbeat and claim; at least `2` (§4.8) |
```

and add a new subsection between `### Cluster` and `### Search`:

```markdown
### Scheduler

| Variable | Default | Description |
|----------|---------|-------------|
| `CYODA_SCHEDULER_ENABLED` | `true` | Run the claim loop on this node |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | `1s` | Claim cadence; `> 0` |
| `CYODA_SCHEDULER_MAX_RUNS` | `8` | Runs in progress per node; `>= 1` |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | `4` | Runs of one tenant per node; `1..MAX_RUNS` |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | `15s` | Liveness write cadence; `> 0` |
| `CYODA_SCHEDULER_STALE_AFTER` | `2m` | Heartbeat silence before a lost-owner claim; `>= 50s + 3 × heartbeat`; the same on every node |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | `3` | Lost owners before FAILED `OWNER_LOST_REPEATEDLY`; `>= 1` |
| `CYODA_SCHEDULER_RETRY_DELAY` | `30s` | First retry delay, doubling; `> 0` |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | `15m` | Retry delay cap; `>= RETRY_DELAY` |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | `20s` | Shutdown wait for runs in progress; `>= 0` |

Startup fails on an invalid value.
```

- [ ] **Step 5: §11, §14.4, §14.6**

§11, after the **Workflow and dispatch** paragraph:

```markdown
**Scheduler:** `cyoda.scheduler.runs` and `cyoda.scheduler.run.duration` (by `outcome`), `cyoda.scheduler.runs.in_progress`, `cyoda.scheduler.claims` (by `reason`), `cyoda.scheduler.heartbeat.failures` and `cyoda.scheduler.bookkeeping.retries`, from `observability.Meter()` and so exposed at `/metrics` regardless of `CYODA_OTEL_ENABLED`; a `scheduler.run` span per run. No tenant attribute.
```

§14.4, a row after **Node crash**:

```markdown
| **Node crash with scheduled runs in progress** | Its tasks stay `RUNNING` under the dead owner. A task whose unsafe processor may have been dispatched carries a mark. | Automatic after `CYODA_SCHEDULER_STALE_AFTER`: another node claims the tasks as lost owners. A marked task ends FAILED `UNSAFE_WORK_NOT_COMPLETED`; the others run again, and after `CYODA_SCHEDULER_MAX_LOST_OWNERS` losses end FAILED. |
```

§14.6, a row after **Max state visits per workflow**:

```markdown
| Scheduled runs per node | 8 (4 per tenant) | Configurable | `CYODA_SCHEDULER_MAX_RUNS`, `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT`. On PostgreSQL each run in progress may hold one main-pool connection for its entity transaction. |
```

- [ ] **Step 6: The other documents**

`docs/CONSISTENCY.md`, end of §1 (before `## 1a.`):

```markdown
**Scheduled-task rows are covered too.** A transaction that writes a task row —
an entity write re-arming or removing its entity's tasks, a delete, a scheduled
run's own commit — fails with `ErrConflict` if another transaction committed a
write to that row after it began. This is what fences a scheduled run's commits
against a reclaim or a re-arm, and why a client write can get `409` when it
races the scheduler. See ARCHITECTURE.md §4.8.
```

`docs/cloud-parity/tenant-id-grammar.md:70`: `Everywhere else — peer dispatch
bodies, scheduler RPC payloads, gossip` → `Everywhere else — peer dispatch
bodies, gossip`.

Playbook row F6 (`:59`): replace "and the scheduler's round-robin ignores node
liveness, so it still takes its share of every scan and opens a transaction and
runs a full cascade for each" with "and its scheduler stops claiming but keeps
heartbeating, so its runs in progress finish rather than being taken over", and
in the evidence column replace "`internal/scheduler/distribution.go`
(`RoundRobin.Pick` — no liveness check)" with "`internal/scheduler` (the claim
loop stops on the latch)".

`internal/domain/search/reaper.go:16-22`, the comment on `StaleClaimBatch`,
becomes:

```go
// StaleClaimBatch caps how many stale jobs a single ReclaimStaleJobs call
// claims. Fixed rather than configurable, it bounds one reaper tick's work
// regardless of how large staleAfter's backlog has grown, so a burst of dead
// jobs cannot turn one tick into an unbounded claim-and-execute loop.
```

- [ ] **Step 7: Check the identifiers §4.8 names**

```
for s in ClaimDue GiveBackIdle StampSegment MarkUnsafe RecordAttempt RetireOwner ReconcileForEntity ErrStaleClaim ErrTaskBusy ErrStoreRejected; do
  grep -q "$s" $(go list -m -f '{{.Dir}}' github.com/cyoda-platform/cyoda-go-spi)/*.go || echo "missing $s"; done → no output
grep -rn 'NotHandedOff\|ProvesNoHandOff' internal/contract/*.go | head -2 → found
grep -n 'func (e \*Engine) FireScheduledTransition' internal/domain/workflow/*.go → the signature of step 1's :1506 row
```

- [ ] **Step 8: Verify**

```
git grep -n -i -e "scheduler's peer RPC" -e 'scheduler rpc' -e 'scheduler payloads' -e 'round-robin distribution' \
  -e 'ScanDue' -e 'DISPATCH_FORWARD_TIMEOUT' -e 'ClusterExecutor' -e 'distribution.go' \
  -- docs/ARCHITECTURE.md docs/CONSISTENCY.md docs/cloud-parity/tenant-id-grammar.md \
     docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis-playbook.md internal/domain/search/reaper.go → no output
go build ./internal/domain/search/ && go vet ./internal/domain/search/
```

Read §4.8 and §3.4 once, top to bottom.

- [ ] **Step 9: Commit**

```
git add docs/ARCHITECTURE.md docs/CONSISTENCY.md docs/cloud-parity/tenant-id-grammar.md \
        docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis-playbook.md \
        internal/domain/search/reaper.go
git commit -m "docs(architecture): scheduled transitions — claim, fence, heartbeat, mark; the RPC and the coordinator gone" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-8: `docs/cloud-parity/scheduled-transitions.md` (Gate 7)

**Spec:** all of §1–§9; §13 for what is tested.

**Needs:** E, R, W, Q (the doc names their behaviour and the query's shape).

**Files:**
- Modify: `docs/cloud-parity/scheduled-transitions.md` (whole file)
- Modify: `docs/cloud-parity/README.md` (the `scheduled-transitions.md` row)

- [ ] **Step 1: Replace the file whole**

````markdown
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

Sections 1 to 13 are the visible contract. Section 14 says what cyoda-go does
internally to meet it, and section 15 what that leaves Cloud to decide.

## 1. Timing

A transition's `schedule` carries exactly one of two timing sources:

- **`delayMs`** — `scheduledTime = arm time + delayMs`.
- **`function`** — an arm-time callout computes `scheduledTime` and optionally
  an expiry per entity (§13).

`timeoutMs` — set on a static schedule, or derived from a function's expiry —
gives the task a **deadline**: `scheduledTime + timeoutMs`. No `timeoutMs`
means no deadline. The owner reads the deadline on its own clock. There is no
grace band: the one-owner rule (§3) means no two nodes decide the same task at
once.

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
   re-arms the same transition records `ARM` only.

Deleting an entity — one entity, by condition, or all entities of a model —
removes its tasks in the same transaction, with no audit event. A workflow
import saves the workflows first and then, in a transaction of its own, removes
the model's tasks whose (source state, transition) none of the model's
workflows schedules any more; it retries that removal three times on a
conflict and then answers a retryable `409`. A repeated import completes it.

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

`PUT …/entity/…/{transition}` and the gRPC manual-transition request return
`400 TRANSITION_NOT_FOUND` for a scheduled transition, with the message
`transition "X" in state "Y" is scheduled and fires automatically; it is not
manually fireable`. Model early firing as an ordinary manual transition beside
the scheduled one.

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

**[ruling]** With a deadline, retries stop at it (§8). Without one, a failing
task is retried without end; it stays visible (§10) and is never terminal.

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
whose callbacks trigger an unsafe processor is unsupported.

A run that committed the entity into another state by a
`COMMIT_BEFORE_DISPATCH` step of its cascade, and then stopped, ends `FAILED`
(`STOPPED_AFTER_PARTIAL_COMMIT`): the entity has moved, so running the
transition again from its source state would be wrong. A
`COMMIT_BEFORE_DISPATCH` step of the fired transition itself leaves the entity
in the source state, and a safe failure after it is retried from there.

## 8. Lateness

The rule depends on the task's history:

| Task picked up | When | Outcome |
|---|---|---|
| first attempt, no lost owner | after the deadline | **expired**: removed, `SCHEDULED_TRANSITION_EXPIRE`, criterion not evaluated |
| after a failed attempt or a lost owner | more than `RETRY_DELAY` after the deadline | `FAILED` `EXPIRED_AFTER_FAILED_ATTEMPTS` |
| any | a counted attempt fails after the deadline | `FAILED` `EXPIRED_AFTER_FAILED_ATTEMPTS` |

A node crash counts as a lost owner, so a task that was running on a crashed
node with a short `timeoutMs` ends `FAILED` rather than expired. A run cut by a
node's shutdown drain, having sent nothing unsafe, is not counted as an attempt
and is due again at once; the next claim decides with the table.

## 9. `FAILED`

| Reason | When |
|---|---|
| `UNSAFE_WORK_NOT_COMPLETED` | §7 |
| `STOPPED_AFTER_PARTIAL_COMMIT` | §7 |
| `EXPIRED_AFTER_FAILED_ATTEMPTS` | §8 |
| `OWNER_LOST_REPEATEDLY` | the task lost its owner `MAX_LOST_OWNERS` times (cyoda-go: 3) |
| `RUN_PANICKED` | the run hit an internal error; the node is taken out of service |

**[ruling]** A `FAILED` task never moves the entity and is never claimed. It is
kept, and it is visible: in `GET /scheduled-tasks` (§10), in the metrics, in
the log at ERROR with a ticket, and as a `SCHEDULED_TRANSITION_FAIL` audit
event on the entity carrying `{transition, sourceState, reason, attempts,
lostOwners}`, written in the same transaction as the status. It ends when the
entity is written in the source state (a new life), leaves the state (removed,
`SCHEDULED_TRANSITION_CANCEL`), stops being scheduled by the workflows (removed
at the next write or import), or is deleted. There is no retry or dismiss API;
an entity write is both.

## 10. `GET /scheduled-tasks`

HTTP only, like the audit trail: an operator's view no compute member needs.
Any authenticated user of the tenant may call it; the tenant comes from the
token. Parameters: `status` (repeatable: `WAITING`, `RUNNING`, `FAILED`),
`modelName` (1–256), `modelVersion` (integer ≥ 1, only with `modelName`),
`entityId` (UUID), `cursor` (opaque, ≤ 256 characters), `limit` (1–1000,
default 20; out of range is `400`, not clamped). Results are ordered by
`(scheduledTime, taskId)` ascending. The response is
`{ "items": [ScheduledTaskDto], "pagination": { "hasNext", "nextCursor" } }`.

`ScheduledTaskDto` is typed but open: `taskId`, `entityId`, `modelName`,
`modelVersion`, `sourceState`, `transition`, `status`, `scheduledTime`,
`armedTime`, `expiresTime` (with a deadline), `attempts`, `lostOwners`,
`nextAttemptTime` (`WAITING`), `lastAttemptTime` and `lastError` (after a failed
attempt), `failureReason` and `failedTime` (`FAILED`), `armedBy` `{id, kind}`
(when known). Node ids and tokens are never returned. `lastError` is at most
1 024 bytes of valid UTF-8 with no NUL, from an allow-list: a compute member's
own message, a coded client-safe message, two fixed texts for a cancelled run
and a conflict, or `internal error [ticket: <uuid>]`.

| Status | Code | When |
|---|---|---|
| 200 | — | success, including an empty list |
| 400 | `BAD_REQUEST` | unknown `status`; `modelVersion` without `modelName` or not an integer ≥ 1; `entityId` not a UUID; `modelName` empty or too long; `limit` not an integer or outside 1–1000; invalid `cursor` (never echoed) |
| 401 | `UNAUTHORIZED` | no token, or an invalid one |
| 500 | `SERVER_ERROR` | internal failure; generic message and a ticket |
| 503 | `STORAGE_UNAVAILABLE` | storage unavailable; retryable |

There is no `403` (no role is required) and no `404`: an unknown model or
entity, or another tenant's, returns an empty list.

## 11. A client write can conflict with the scheduler

The scheduler writes a task row when it claims, commits a run segment, records
an attempt, fails a task or hands it back. A client write that re-arms or
removes a task row the scheduler wrote after the write began gets a retryable
`409 CONFLICT` — the same as a write racing the fire itself.

| Endpoint | Status | When |
|---|---|---|
| `deleteSingleEntity` | `409 CONFLICT` (retryable) | the conflict persists after 3 server-side retries — **new cell** |
| `deleteEntities` (conditional, one transaction; delete-all) | `409 CONFLICT` (retryable) | the same |
| `deleteEntities` (batched) | `200` | a persistent conflict is reported per entity in the batch result |
| `updateSingle`, `updateSingleWithLoopback`, `updateCollection` | `409 CONFLICT` (retryable) | no server-side retry |
| `importEntityModelWorkflow` | `409 CONFLICT` (retryable) | the task removal conflicts after 3 retries — **new cell** |

A delete inside a transaction the caller already holds is not retried; the
conflict surfaces at that transaction's commit. The gRPC entity requests return
the same condition as they return any entity conflict.

## 12. Audit events

| Outcome | Event |
|---|---|
| Armed | `SCHEDULED_TRANSITION_ARM`, on every arm including loopback re-arms |
| Fired | `SCHEDULED_TRANSITION_FIRE` + `TRANSITION_MAKE` |
| Declined | `TRANSITION_NOT_MATCH_CRITERION` |
| Expired | `SCHEDULED_TRANSITION_EXPIRE` |
| Removed while pending (entity left the state, or the workflow no longer schedules it) | `SCHEDULED_TRANSITION_CANCEL` |
| Ended `FAILED` | `SCHEDULED_TRANSITION_FAIL` — **new** |
| Safe failure | none; the task's attempts and last error record it |
| Superseded run, entity gone or moved on | none |
| Entity deleted | none |

A task that comes due while the selected workflow no longer schedules it is
removed with `CANCEL` at that point too; a task past its deadline on its first
attempt records `EXPIRE` first, because lateness is decided before the workflow
is resolved.

## 13. Arm-time `function`

Unchanged from the previous contract. The callout runs synchronously inside the
entity write's transaction and returns `resultKind: "Schedule"` with exactly one
of `fireAt` (absolute unix-ms) or `fireAfterMs` (relative to arm time), and at
most one of `expireAt` or `expireAfterMs` (relative to the resolved fire time).
A fire time in the past is due at once. A resolved expiry at or before the fire
time is **born expired**: the transition is not armed, any pending task for it
is removed, and `SCHEDULED_TRANSITION_EXPIRE` is recorded in the same write,
which still succeeds. A malformed or wrong-kind result fails the write with
`500 SCHEDULE_FUNCTION_INVALID_RESULT`. An unreachable or silent compute member
fails it with the retryable `503` codes of `callout-failover.md` §2.

On the fire path, the cascade can end in a state with its own `function`-timed
schedule. If that arm's callout fails, the run fails safely (§6): nothing
commits, and the task is retried.

## 14. How cyoda-go meets §3 and §7

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
- The callout coordinator attaches a "not handed off" proof only when no try
  reached a member and no hand-over to a peer got past connecting — unless the
  peer's authenticated answer was `no_handoff`.

## 15. What Cloud has to decide

The visible contract to match:

1. One owner per run and the lost-owner rule of §3, however Cloud implements
   ownership.
2. §6 and §7: safe failures retried with a doubling delay until the deadline;
   an unsafe processor never repeated after a possible hand-off.
   `idempotent` means the same on a scheduled run as on any callout.
3. §8's lateness table, and no grace band.
4. The `FAILED` status, its five reasons, and **[ruling]** that it never moves
   the entity; `SCHEDULED_TRANSITION_FAIL` and its data.
5. §2: an arm is a new life that clears a `FAILED` status; a write removes every
   task not in its arm set; deletes and imports remove tasks.
6. `GET /scheduled-tasks` as §10 states it.
7. The two new `409` cells of §11.
8. One `RUNNING` task per entity.

Where Cloud's storage cannot make a task-row write part of the entity
transaction, it needs another way to stop a superseded owner from committing
(§3, second point) and another durable place for the mark (§7). Those are
Cloud's to design; the visible contract above is what must match.
````

- [ ] **Step 2: README row** — replace the `scheduled-transitions.md` row's
second column with:

```markdown
Scheduled-transition runtime: one owner per run (claim, heartbeat, lost owner); safe failures retried until `timeoutMs`; an unsafe processor never repeated, the task ends `FAILED` instead; five `FAILED` reasons and `SCHEDULED_TRANSITION_FAIL`; lateness without a grace band; arm-as-new-life, removal on write, delete and import; `GET /scheduled-tasks`; the new `409` cells on delete and import; arm-time `function`
```

- [ ] **Step 3: Verify**

```
grep -n -i 'grace\|coordinator\|round-robin\|redispatch\|dual' docs/cloud-parity/scheduled-transitions.md → no output
grep -n 'cnode\|pnode' docs/cloud-parity/scheduled-transitions.md → no output
grep -n 'operationId: listScheduledTasks' api/openapi.yaml → found; compare §10's parameter list, bounds and error table with the operation Q wrote, and correct §10 where they differ
grep -n "'409'" api/openapi.yaml | head → W's two new cells exist
```

Read the whole file once.

- [ ] **Step 4: Commit**

```
git add docs/cloud-parity/scheduled-transitions.md docs/cloud-parity/README.md
git commit -m "docs(cloud-parity): scheduled transitions — one owner, no unsafe repeat, FAILED, the query" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-9: `COMPATIBILITY.md` — the SPI pin and the chart

**Spec:** §12 (`COMPATIBILITY.md`); Gate 4.

**Needs:** the wave-6 commit that pins the merged SPI pseudo-version and runs
`make repin-plugins`; D-5 (the chart version).

**Files:**
- Modify: `COMPATIBILITY.md` — the `v0.9.0` matrix row (`:35`), "The SPI pin
  during a milestone" (`:79`), "SPI work in flight for the next release"
  (`:87-117`), "Out-of-tree plugins" (`:168`, `:172-176`), "Helm chart × binary"
  (`:182`), "Operator action required" (after `:202`)

- [ ] **Step 1: Read the pin**

```
go list -m -f '{{.Version}}' github.com/cyoda-platform/cyoda-go-spi
grep -h 'cyoda-go-spi' go.mod plugins/*/go.mod | sort -u
```

All four lines must show one version. Write it wherever `PIN` appears below.

- [ ] **Step 2: Replace the `v0.9.0` row** (`:35`) with:

```markdown
| **`v0.9.0`** (in progress) | `cyoda-go-spi PIN` | same pseudo-version | Mid-milestone, so a pseudo-version of the SPI's `main` and no tag — the tag comes at the release cut ([`MAINTAINING.md`](./MAINTAINING.md)). **Breaking — `ScheduledTaskStore` is replaced.** Removed: `Upsert`, `ScanDue`, `MarkRedispatch`, `Delete`, and `ScheduledTask.RedispatchAfter` / `AttemptCount`. The store now carries ownership: `ScheduledTask` gains `Status`, `ArmToken`, `NextAttemptTime`, `Attempts`, `LostOwners`, `LastAttemptTime`, `LastError`, `FailureReason`, `FailedTime`, `PartialCommit`, `Claim` and `UnsafeMarked`; the methods are `ReconcileForEntity` (arms each task as a new life and removes every other task of the entity), `RemoveLife`, `StampSegment`, `DeleteForEntities`, `DeleteForModel`, `Get`, `Query`, `ClaimDue`, `Heartbeat`, `RetireOwner`, `SweepOwners`, `GiveBackIdle`, `MarkUnsafe`, `RecordAttempt`, `Fail` and `SweepMarks`. Six clauses bind every backend: first-committer-wins on task rows (C1), a joining read sees its staged writes (C2), a mark and a claim serialise (C3), heartbeats and claims cannot be starved by entity transactions (C4), refusals are `ErrConflict` / `ErrStaleClaim` (C5), and a task row written by an open transaction is not claimable (C6). New errors `ErrMarkedByAnotherClaim` and `ErrTaskBusy`, and the marker `ErrStoreRejected`, which every store wraps around a deterministic rejection. New audit constant `SMEventScheduledTransitionFailed` (`SCHEDULED_TRANSITION_FAIL`). The `spitest` `ScheduledTasks` suite replaces `RunScheduledTaskStoreConformance` and covers every method and clause. Additive since `v0.8.4`: `ProcessorConfig.Idempotent *bool` and `ScheduleFunction.RetryPolicy` (a function-driven schedule omits `delayMs` rather than sending a zero); `ErrTxNotCommitted`, which `GetSubmitTime` wraps for a transaction still in flight; `ErrEntityModelMismatch` — an entity's model reference is immutable, pinned by six `Save` / `CompareAndSave` cases; the state machine event id is the store's (`Audit/EventID`) and an entity version number is never reused (`Entity/GetVersionMetadata/RecreateAfterDelete`). **Every node of a cluster must run the same `cyoda-go` version**: the node-to-node wire changed and is sealed per recipient, and an earlier node fires scheduled tasks without claiming them, so a rolling upgrade across versions is not supported. |
```

- [ ] **Step 3: `:79`** — replace "**Currently pinned: `cyoda-go-spi v0.8.4`.**
That tag is breaking despite being a patch — this module ships breaking changes
on patches (see [Independent version axes](#independent-version-axes)), and its
`### Breaking` changelog section rather than the version component is what flags
it." with:

```markdown
**Currently pinned: the pseudo-version in the `v0.9.0` row.** It is breaking
(`ScheduledTaskStore` is replaced); this module ships breaking changes without a
major version (see [Independent version axes](#independent-version-axes)), and
its `### Breaking` changelog section rather than the version component is what
flags it.
```

- [ ] **Step 4: "SPI work in flight for the next release" (`:87-117`)** — delete
the subsection whole, heading included. Everything it describes is pinned and is
in the `v0.9.0` row.

- [ ] **Step 5: Out-of-tree plugins** — in the `cyoda-go-cassandra` row's Status
cell, replace "The v0.8.4 obligations are listed in the matrix row above." with
"The v0.8.4 and v0.9.0 obligations are listed in the matrix rows above; the
`ScheduledTaskStore` replacement is tracked in cyoda-go-cassandra#68." Replace
the paragraph's last sentence ("Outstanding obligations for the `v0.8.4`
surface — … — are enumerated in the `v0.8.4` matrix row above, each with the
issue tracking it.") with the same sentence followed by: "For `v0.9.0` the
obligation is the new `ScheduledTaskStore`, its six clauses and the `spitest`
`ScheduledTasks` suite."

- [ ] **Step 6: Helm chart × binary** — add above the `0.8.4` row, and drop
"**Current.**" from the `0.8.4` row:

```markdown
| `0.9.0` | `0.8.4` until the release cut, then `0.9.0` | `cyoda-go v0.8.4`, then `v0.9.0` | **Unreleased, not tagged.** Adds `terminationGracePeriodSeconds` (default `360`, schema minimum `1`) so a node can finish its scheduled runs on shutdown; see `cyoda help run` (SHUTDOWN TIMING). `appVersion` moves at the release by `bump-chart-appversion.yml`. |
```

- [ ] **Step 7: Operator action required** — add after the last
`v0.8.4` → next release row:

```markdown
| `v0.8.4` → next release | `version: 0.9.0` (`terminationGracePeriodSeconds: 360`) | **Stop the whole cluster to upgrade; remove six settings.** A node of an earlier version fires scheduled tasks without claiming them, so a mixed cluster can run one twice. `CYODA_SCHEDULER_DISTRIBUTION`, `CYODA_SCHEDULER_COORDINATOR`, `CYODA_SCHEDULER_REDISPATCH_BACKOFF`, `CYODA_SCHEDULER_BATCH_SIZE`, `CYODA_SCHEDULER_EXPIRY_GRACE` and `CYODA_DISPATCH_FORWARD_TIMEOUT` are no longer read. PostgreSQL uses up to `CYODA_POSTGRES_SCHEDULER_CONNS + 1` (default 11) more connections per node: check `max_connections`. Outside the chart, give pods a termination grace period of at least `max(CYODA_SCHEDULER_SHUTDOWN_DRAIN, callout deadline) + 70s` (345 s at the defaults with the maximum answer limit). PostgreSQL and SQLite each apply one more migration, on the scheduler's tables only. |
```

- [ ] **Step 8: Verify**

```
grep -c 'ErrTxInFlight' COMPATIBILITY.md → 0
grep -n 'SPI work in flight' COMPATIBILITY.md → no output
grep -n "$(go list -m -f '{{.Version}}' github.com/cyoda-platform/cyoda-go-spi)" COMPATIBILITY.md → the v0.9.0 row
make check-spi-pin-sync → the four manifests agree
git diff --name-only origin/release/v0.9.0 -- plugins/postgres/migrations plugins/sqlite/migrations
  → one new up/down pair per backend; grep each for CREATE/ALTER/DROP and confirm it names only scheduled_tasks,
    scheduled_task_marks and scheduler_owners (if not, correct the last sentence of step 7)
```

- [ ] **Step 9: Commit**

```
git add COMPATIBILITY.md
git commit -m "docs(compatibility): v0.9.0 SPI pin — ScheduledTaskStore replaced; chart 0.9.0; operator actions" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-10 (LAST): `CHANGELOG.md` `[Unreleased]` — one section, read as one

**Spec:** §12 (`### Breaking` list).

**Needs:** every other task of every stream, D-8 included (the entries cite
`docs/cloud-parity/scheduled-transitions.md`).

**Files:**
- Modify: `CHANGELOG.md` `[Unreleased]` (`### Breaking` `:7`, `### Added` `:230`, `### Fixed` `:492` today)

House style (read `:9-60`): an entry leads with the new contract in bold, says
what it replaces, and ends by naming the help topic or `docs/cloud-parity/*.md`
file. No issue numbers in new entries.

Fragments other streams may have added — **find each (`grep -n 'schedul'
CHANGELOG.md | head -40`) and merge it**: a fragment that says something the
entries below do not is kept and moved next to the entry it belongs to; one that
repeats an entry is deleted.

- [ ] **Step 1: Correct the entries this change makes false**

- `:178-185` ("`CYODA_DISPATCH_FORWARD_TIMEOUT` no longer governs handing a
  callout to another node.") — delete the whole entry. The removal is in step 2,
  and the hand-over bounds are in the Added "Callout failover" entry and in
  "Callout settings".
- `:196-200` ("Every node of a cluster must run this version …") — replace "The
  node-to-node call that delegates a scheduled transition travels in the same
  envelope and changed with it. A mixed-version cluster can therefore neither
  hand a callout over nor forward a scheduled transition:" with "A
  mixed-version cluster cannot hand a callout over:", and after "there is no
  rolling upgrade across this change." add "A node of an earlier version also
  fires scheduled tasks without claiming them, so in a mixed cluster a task can
  run twice."
- `:265-268` (Added, "Callout settings.") — replace
  "`CYODA_DISPATCH_WAIT_TIMEOUT` and `CYODA_DISPATCH_FORWARD_TIMEOUT` are
  validated for the first time (negative, respectively non-positive, values now
  fail startup)." with "`CYODA_DISPATCH_WAIT_TIMEOUT` is validated for the first
  time (a negative value now fails startup)."
- `:675-682` (Fixed, "A node's answer to another node's call could be replaced
  …") — replace "Neither the answer to a handed-over callout nor the answer to
  the call that delegates a scheduled transition was authenticated" with "The
  answer to a handed-over callout was not authenticated, nor was the answer to
  the call that delegated a scheduled transition", and "Both answers are sealed
  for the one request they answer (see Breaking)" with "A hand-over's answer is
  sealed for the one request it answers (see Breaking), and the scheduled-
  transition call no longer exists".

- [ ] **Step 2: `### Breaking`** — add after the "An audit-events cursor from
before this change …" entry:

```markdown
- **Every scheduled run has one owner, and the scheduler never runs a processor
  twice unless it is declared `idempotent`.** Every node claims due scheduled
  transitions from storage and runs them itself. A claimed task has one owner at
  a time, every write of the run is checked against that claim, and another node
  takes a task over only after the owner's heartbeats have stopped for
  `CYODA_SCHEDULER_STALE_AFTER` (default `2m`). Before, one node scanned and
  handed tasks round robin to its peers, and a task still running after 30
  seconds was started a second time, so its processors could run twice. Now,
  before a processor that is not declared `idempotent` is sent to a compute
  member, the owner marks the task; if that processor may have reached a member
  and the run does not commit, the task ends `FAILED` and is not run again. A
  run that failed with nothing unsafe handed off is retried, with a delay that
  doubles from `CYODA_SCHEDULER_RETRY_DELAY` (`30s`) up to
  `CYODA_SCHEDULER_RETRY_DELAY_MAX` (`15m`), until the transition's `timeoutMs`
  passes, or without end when it has none. **A scheduled processor that is safe
  to repeat must now declare `idempotent: true` to be retried.** See
  `cyoda help workflows` and `docs/cloud-parity/scheduled-transitions.md`.

- **A scheduled task can end `FAILED`, and a failed task never moves its
  entity.** The reasons are `UNSAFE_WORK_NOT_COMPLETED`,
  `STOPPED_AFTER_PARTIAL_COMMIT`, `EXPIRED_AFTER_FAILED_ATTEMPTS`,
  `OWNER_LOST_REPEATEDLY` (after `CYODA_SCHEDULER_MAX_LOST_OWNERS`, default `3`)
  and `RUN_PANICKED`. A `FAILED` task is kept, listed by the new
  `GET /scheduled-tasks` (see Added), counted in `cyoda.scheduler.runs`, logged
  at ERROR with a ticket, and recorded on the entity as the new audit event
  `SCHEDULED_TRANSITION_FAIL`. An entity write in the source state arms it
  afresh; leaving the state, a workflow import that stops scheduling it, or
  deleting the entity removes it. Lateness changes with it: a task picked up
  after its deadline on its first attempt is still expired
  (`SCHEDULED_TRANSITION_EXPIRE`), but one that had already failed or lost an
  owner now ends `FAILED` (`EXPIRED_AFTER_FAILED_ATTEMPTS`) instead of being
  dropped, and the grace band above `timeoutMs` is gone. A client that reads the
  audit event type must accept the new value. See
  `docs/cloud-parity/scheduled-transitions.md`.

- **One scheduled transition of an entity runs at a time.** While one of an
  entity's scheduled transitions is running, its others are not claimed; they
  run after it ends.

- **Entity writes, deletes and workflow imports remove scheduled tasks, and can
  answer a retryable `409 CONFLICT` when they race the scheduler.** A write that
  re-binds an entity to a workflow that does not schedule a pending transition
  now removes the task at the write, recorded as `SCHEDULED_TRANSITION_CANCEL`;
  before, the task stayed until it came due. Deleting entities removes their
  tasks in the same transaction, and a workflow import removes the model's tasks
  that no workflow schedules any more. When the scheduler has written one of
  those task rows after the client's write began, the write fails with a
  retryable `409 CONFLICT` — the answer a write racing the fire itself already
  got. Deletes and imports retry on the server three times first; a batched
  conditional delete reports the conflict per entity instead of `409`. Two
  operations can now answer `409` and declare it:
  `DELETE /entity/{entityId}` and
  `POST /model/{entityName}/{modelVersion}/workflow/import`; repeating the same
  import completes it. See `cyoda help errors CONFLICT` and
  `docs/cloud-parity/scheduled-transitions.md`.

- **Scheduler settings are replaced.** No longer read:
  `CYODA_SCHEDULER_DISTRIBUTION`, `CYODA_SCHEDULER_COORDINATOR`,
  `CYODA_SCHEDULER_REDISPATCH_BACKOFF`, `CYODA_SCHEDULER_BATCH_SIZE`,
  `CYODA_SCHEDULER_EXPIRY_GRACE` and `CYODA_DISPATCH_FORWARD_TIMEOUT` (the
  node-to-node call that delegated a scheduled transition is gone). New:
  `CYODA_SCHEDULER_MAX_RUNS` (`8`), `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (`4`),
  `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (`15s`), `CYODA_SCHEDULER_STALE_AFTER`
  (`2m`, at least `50s + 3 ×` the heartbeat interval, the same on every node),
  `CYODA_SCHEDULER_MAX_LOST_OWNERS` (`3`), `CYODA_SCHEDULER_RETRY_DELAY`
  (`30s`), `CYODA_SCHEDULER_RETRY_DELAY_MAX` (`15m`),
  `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (`20s`) and, on PostgreSQL,
  `CYODA_POSTGRES_SCHEDULER_CONNS` (`10`) — a second pool, so a node uses up to
  eleven more connections. `CYODA_SCHEDULER_SCAN_INTERVAL` must now be `> 0`.
  An invalid value fails startup. See `cyoda help config scheduler`.

- **A node can take up to about six minutes to shut down, and the Helm chart
  gives it 360 seconds.** On a signal the scheduler drains its runs before the
  servers drain, and a run whose processor that is not `idempotent` is in
  flight is allowed to finish rather than be cut and end `FAILED`. The worst
  case from signal to exit is `max(CYODA_SCHEDULER_SHUTDOWN_DRAIN, callout
  deadline) + 70s`, where the callout deadline is
  `(1 + CYODA_RETRY_FIXED_NUM_RETRIES) × answer limit +
  CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE` — 345 s at the
  defaults with the maximum answer limit. The chart (`0.9.0`) sets
  `terminationGracePeriodSeconds: 360`, where Kubernetes' default is 30. Raise
  it when you raise those settings. See `cyoda help run`.
```

- [ ] **Step 3: `### Added`** — add after the "Six instruments for callouts …"
entry (merge Q's fragment for the query into the first entry if Q wrote one):

```markdown
- **`GET /scheduled-tasks` lists the tenant's scheduled tasks.** Filters:
  `status` (repeatable: `WAITING`, `RUNNING`, `FAILED`), `modelName` with an
  optional `modelVersion`, and `entityId`; ordered by scheduled time, paged by
  an opaque `cursor` with `limit` 1–1000 (default 20). Each item gives the
  task's status, times, attempts, lost owners, last error and, for a `FAILED`
  task, its reason. Any authenticated user of the tenant may call it; there is
  no gRPC counterpart. See `cyoda help scheduled-tasks`.

- **Six scheduler instruments.** `cyoda.scheduler.runs` and
  `cyoda.scheduler.run.duration` (by `outcome`),
  `cyoda.scheduler.runs.in_progress`, `cyoda.scheduler.claims` (by `reason`:
  `due`, `owner_lost`), `cyoda.scheduler.heartbeat.failures` and
  `cyoda.scheduler.bookkeeping.retries`; and a `scheduler.run` span. None
  carries a tenant. See `cyoda help telemetry`.
```

- [ ] **Step 4: `### Fixed`** — add as the first two entries of the section:

```markdown
- **A scheduled transition could run twice at the same time.** A task still
  running after `CYODA_SCHEDULER_REDISPATCH_BACKOFF` (30 s) was dispatched again,
  to the same or another node, while the first run went on. Only one run could
  commit, but both ran their processors, and a slow callout alone takes 30 s. A
  task now has one owner, and another node takes it over only once the owner
  has stopped heartbeating (see Breaking).

- **A node that had recovered a panic kept its share of scheduled work.** The
  round-robin distribution did not read node health, so a node taken out of the
  Service by `/readyz` still received its share of every scan. A latched node
  now claims nothing, and keeps heartbeating so its runs in progress are not
  taken over while they may still commit.
```

- [ ] **Step 5: Verify**

```
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -n 'forward a scheduled transition\|delegates a scheduled transition' → no output
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -c 'scheduled-transitions.md' → 3
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -n 'cnode\|pnode' → no output
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -c 'CYODA_DISPATCH_FORWARD_TIMEOUT' → 1 (the Breaking list of removed settings)
```

Check spec §12's seven `### Breaking` items one by one against the section: the
removed variables; unsafe processors not repeated; `FAILED`; one task per
entity; the client `409` with the two new cells; the query (Added, referenced
from Breaking); the chart's 360 and its formula. Read the whole `[Unreleased]`
section once, top to bottom, for two entries that say one thing.

- [ ] **Step 6: Commit**

```
git add CHANGELOG.md
git commit -m "docs(changelog): scheduled runs have one owner — one Unreleased section" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D-11: cross-repo notes (the lead posts them)

**Spec:** §12 (cyoda-go-cassandra#68; the CaaS ticket).

**Needs:** the PR merged into `release/v0.9.0`, and the SPI PR merged into the
SPI's `main`. Check both with `gh pr view` before posting: a note must not
describe work that has not landed.

No files, no commit. The lead posts the first text with
`gh issue comment 68 -R cyoda/cyoda-go-cassandra --body-file <file>` (the file
must be non-empty) and files the second through the Rovo connector
(cyoda1.atlassian.net, project CP, component CaaS).

- [ ] **Step 1: Comment for cyoda-go-cassandra#68** (replace `PIN`, `PR` and
`SPI_PR` with the pinned pseudo-version and the two merged PR links)

```markdown
The contract this issue describes is replaced in cyoda-go v0.9.0 (PR, SPI_PR; SPI pinned at `PIN`). The leader scan, round-robin delegation and redispatch throttle are gone. Every node claims tasks from the store and runs them itself, so the store now carries ownership.

**Removed:** `Upsert`, `ScanDue`, `MarkRedispatch`, `Delete`, `ScheduledTask.RedispatchAfter`, `ScheduledTask.AttemptCount`.

**New `ScheduledTask` fields:** `Status` (`WAITING` / `RUNNING` / `FAILED`), `ArmToken` (drawn by the store on every arm), `NextAttemptTime`, `Attempts`, `LostOwners`, `LastAttemptTime`, `LastError` (≤ 1 024 bytes, stored as given), `FailureReason`, `FailedTime`, `PartialCommit`, `Claim {Token, Owner}` (RUNNING only), `UnsafeMarked` (read-only).

**Methods.** "Joins" = part of the entity transaction on `ctx`. "Never joins" = commits on its own, ignoring any transaction on `ctx`. "Fenced" = accepted only with the task's current arm token and claim token, otherwise `ErrStaleClaim`.

- `ReconcileForEntity` (joins): arm each task of the request as a new life (WAITING, counters cleared, new arm token, no claim); remove every other task of the entity; return the removed ones.
- `RemoveLife(tenant, id, armToken)` (joins): remove only if that life is current.
- `StampSegment(ref, partial)` (joins, fenced): write the task row; set `PartialCommit` when asked.
- `DeleteForEntities`, `DeleteForModel(tenant, name, version, keep)` (join).
- `Get` (may join; sees the transaction's staged writes), `Query` (never joins; tenant-scoped page ordered by `(scheduledTime, id)`).
- `ClaimDue(req)` (never joins, cross-tenant): atomic and disjoint across concurrent callers; WAITING and due by the caller's clock, or — with `AllowLostOwner` — RUNNING under an owner whose liveness record is missing or older than `StaleAfter` by the store clock (`LostOwners` + 1); at most one RUNNING task per entity, enforced against concurrent callers; `Limit`, `PerTenantLimit` and `TenantInProgress`; tenants take turns; a new claim token per claim.
- `Heartbeat`, `RetireOwner`, `SweepOwners` (never join): owner liveness, stamped by the store clock; `Heartbeat` is an upsert.
- `GiveBackIdle(owner, keep)` (never joins): RUNNING under `owner` and not in `keep` → WAITING, not counted.
- `MarkUnsafe(ref)` (never joins, fenced): the mark survives the rollback of the transaction on `ctx`; idempotent for the same claim; `ErrMarkedByAnotherClaim` if another claim of the life marked it; `ErrTaskBusy` (C6).
- `RecordAttempt(ref, a)` (never joins, fenced): WAITING, claim cleared, `Attempts` + 1 unless `NotCounted`; with `ClearOwnMark`, remove this claim's mark in the same atomic write.
- `Fail(ref, f)` (joins the transaction that also records `SCHEDULED_TRANSITION_FAIL`, fenced): FAILED, claim cleared.
- `SweepMarks` (never joins): remove the marks of ended lives.

**Clauses — each is a `spitest` `ScheduledTasks` case:**

- C1: first-committer-wins covers task rows. A transaction that writes a task row fails with `ErrConflict` if another transaction — joining or not — committed a write to that row after it began.
- C2: a joining read sees the operations staged earlier in the same transaction.
- C3: `MarkUnsafe` and `ClaimDue` racing on one task: either the mark is refused, or the claim returns `UnsafeMarked`.
- C4: heartbeats and claims cannot be starved by entity transactions.
- C5: a C1 refusal satisfies `errors.Is(err, spi.ErrConflict)` whether raised by a statement or by the commit; a fenced refusal is `spi.ErrStaleClaim`.
- C6: **a task row written by an open transaction is not claimable until that transaction ends, and `MarkUnsafe` answers `ErrTaskBusy` for it.** This is what stops another node reclaiming a task while its owner's commit is under way; without it, fencing does not hold.

Also: every deterministic rejection by the store (constraint, data, syntax class) must satisfy `errors.Is(err, spi.ErrStoreRejected)`; the scheduler latches the node only on that marker and retries everything else.

**Questions for this backend.** C1 and C6 inside the entity transaction under this backend's transaction model; how `ClaimDue`'s one-RUNNING-per-entity rule holds against a concurrent claim on another node; where the mark lives so that it survives the entity transaction's rollback and is seen by the next claim (C3); and the liveness clock (the store's, not the node's). The parity scenarios in `e2e/parity/registry.go` and the `ScheduledTasks` suite reach this backend on its next dependency update. The Cloud-facing statement of the contract is `docs/cloud-parity/scheduled-transitions.md` in cyoda-go.
```

- [ ] **Step 2: CaaS ticket**

Title: `[CaaS] Scheduled transitions: one owner per run, no repeat of non-idempotent processors, FAILED status, GET /scheduled-tasks`

Component: `CaaS`. Body:

```markdown
cyoda-go v0.9.0 changes the scheduled-transition contract. cyoda-go defines the contract; Cloud aligns to it. The full statement is docs/cloud-parity/scheduled-transitions.md in cyoda-go (release/v0.9.0); section 15 lists what Cloud has to match.

What changed for a client:

1. One owner per run. At most one node runs a scheduled task at a time. Another node takes it over only after the owner's heartbeats stop (cyoda-go default 2 minutes); each takeover counts a "lost owner".
2. Retries. A run that fails when nothing unsafe reached a compute member is retried, with a delay doubling from 30 s to 15 min, until the transition's timeoutMs passes; with no timeoutMs, without end.
3. A processor whose config.idempotent is not true is never run twice by the platform. If it may have been handed to a compute member and the run did not commit, the task ends FAILED (UNSAFE_WORK_NOT_COMPLETED).
4. FAILED status with five reasons: UNSAFE_WORK_NOT_COMPLETED, STOPPED_AFTER_PARTIAL_COMMIT, EXPIRED_AFTER_FAILED_ATTEMPTS, OWNER_LOST_REPEATEDLY, RUN_PANICKED. A FAILED task never moves the entity. It is kept and visible; an entity write in the source state arms it afresh. New audit event SCHEDULED_TRANSITION_FAIL with data {transition, sourceState, reason, attempts, lostOwners}.
5. Lateness: a task late on its first attempt is expired as before; one that already failed or lost an owner ends FAILED (EXPIRED_AFTER_FAILED_ATTEMPTS). No grace band.
6. An entity write removes every pending task of the entity that the selected workflow does not schedule from its current state (SCHEDULED_TRANSITION_CANCEL). Entity deletes and workflow imports remove tasks.
7. At most one scheduled task per entity runs at a time.
8. New endpoint GET /scheduled-tasks (HTTP only): filters status, modelName, modelVersion, entityId; cursor paging, limit 1–1000 default 20; errors 400 BAD_REQUEST, 401, 500 SERVER_ERROR, 503 STORAGE_UNAVAILABLE; no 403 or 404.
9. New 409 CONFLICT (retryable) cells: deleteSingleEntity and importEntityModelWorkflow, when a task removal keeps conflicting with the scheduler after three server-side retries.

Please confirm which of 1–9 Cloud already meets, and plan the rest.
```

- [ ] **Step 3:** The lead records the ticket key in the PR description (not in
any shipped file) and tells Paul both notes are posted.

---

## Coverage carried forward (§13)

No §13 row belongs to this stream: documentation carries no scenario. The chart
guard of D-5 is the one new test D adds. Existing tests that pin what D changes
are the help guards listed at the top; D-1, D-3, D-5 and D-6 run them.

## Stream interface summary

D produces nothing another stream compiles against. It relies on these names
existing, by grep in each task's first step, not by signature:

- S: `SMEventScheduledTransitionFailed`, the five failure-reason constants,
  `ErrMarkedByAnotherClaim`, `ErrTaskBusy`, `ErrStoreRejected`, the
  `ScheduledTaskStore` method names of `interfaces.md`.
- R: the ten `CYODA_SCHEDULER_*` names and defaults in `config_registry.go`; the
  six metric names; the scheduler `Drain` before the server drains in
  `cmd/cyoda/run.go`; `FireScheduledTransition`'s signature (E).
- BP: `CYODA_POSTGRES_SCHEDULER_CONNS` (default 10, ≥ 2); the pool limits;
  the migration creating `scheduled_task_marks` and `scheduler_owners`.
- K: `NoHandOffProof` / `ProvesNoHandOff` in `internal/contract`.
- W: the two new `409` cells in `api/openapi.yaml`; `errors/CONFLICT.md`
  widened.
- Q: `cmd/cyoda/help/content/scheduled-tasks.md`; the `listScheduledTasks`
  operation at `/scheduled-tasks`; `SCHEDULED_TRANSITION_FAIL` in the audit enum.

D asks two things of other streams:

- R, BP and W add **no** prose to `README.md`, `ARCHITECTURE.md`,
  `docs/cloud-parity/*` or `CHANGELOG.md` beyond a one-line fragment; D writes
  them. R does not edit the comment at `internal/domain/search/reaper.go:16-22`;
  D-7 does.
- Q-1 adds `SCHEDULED_TRANSITION_FAIL` to the audit enum (`api/openapi.yaml:11742`)
  with the new operation, since it regenerates `api` in wave 1.

## Open points

1. **`TestConfig_EnvVarCoverage` fails between R's config commit and D-1** (and
   between BP's config commit and D-2): it requires every `CYODA_*` in Go source
   to be named in `config/*.md`. Either run D-1 and D-2 directly after those
   commits with no `make test` in between, or fold D-1 steps 2–6 into R's
   commit and D-2 step 2 into BP's. The lead picks; the text does not change.
2. **Chart `version:` 0.9.0 now, `appVersion` at the release.** Spec §12 asks for
   a chart version bump. The chart's convention is that `version:` and
   `appVersion:` move together at each binary release (`COMPATIBILITY.md` chart
   table, `MAINTAINING.md` step 8). D-5 bumps `version:` to `0.9.0` now so the
   template change is versioned, and leaves `appVersion` to
   `bump-chart-appversion.yml`. No chart tag is cut mid-milestone. If Paul
   prefers to bump both only at the release cut, drop the `Chart.yaml` version
   line from D-5 and the chart row from D-9.
3. **Schema minimum for `terminationGracePeriodSeconds` is `1`.** A value below
   the shutdown bound is legal Kubernetes and may be what an operator with
   lowered settings wants, so the schema refuses only `0` and negatives. A
   higher floor (for example 90, the bound with no unsafe callout in flight)
   is possible; it would refuse configurations that are valid for smaller
   settings.
4. **Audit events of a superseded run on memory and SQLite.** The old
   cloud-parity §8 accepted a duplicate `SCHEDULED_TRANSITION_FIRE` on these
   backends under a transient dual coordinator, because their audit store is
   not transaction-scoped (`plugins/memory/sm_audit_store.go:20-37` appends at
   `Record`, outside the transaction). The dual coordinator is gone, so D-8
   drops that section. A run superseded by a client write can still reach
   `Record` before its commit fails. Whether its events stay visible after the
   rollback on memory and SQLite is not stated in the spec; if they do, that is
   a backend divergence from PostgreSQL (a bug, not an accepted edge). The lead
   asks E and BM to confirm before D-8 lands.
5. **`docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis.md:173`**
   says the round-robin keeps a tainted node's share. It is a dated analysis
   of `release/v0.8.2`, not a living reference, so D leaves it; its playbook
   (living, edited by later PRs) is corrected in D-7.
6. **README endpoint path.** D-1 and D-8 write `GET /api/scheduled-tasks`
   (README, with the default context path) and `GET /scheduled-tasks` (help and
   cloud-parity, as the spec). D-1 step 7 greps Q's operation path and corrects
   both if Q chose another path.
