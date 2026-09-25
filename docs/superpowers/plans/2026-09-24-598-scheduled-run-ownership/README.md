# Scheduled-run ownership — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each scheduled run has one owner. A processor that is not declared
`idempotent` is never repeated by the platform. Safe failures are retried
until the transition's `timeoutMs` expires. A task that cannot succeed ends
FAILED, stays visible through `GET /scheduled-tasks`, and never moves its
entity.

**Architecture:**
- **Claiming.** Every pnode claims due tasks from the store and runs them
  itself. Every write is fenced by a claim token. Liveness comes from a
  per-pnode heartbeat, and a watchdog cancels runs before the pnode can go
  stale.
- **The mark.** A durable mark is written before each unsafe dispatch, and
  positive callout proof clears it. With the mark, no platform-initiated
  repeat is possible.
- **Commit fence.** Task rows are under first-committer-wins on every backend,
  and every commit of a run writes its own task row. That fences commits
  without a claim check inside the entity transaction.
- **Removed:** the coordinator, the distribution, the scheduler RPC, the
  throttle and the grace band.

**Tech Stack:** Go 1.26, `log/slog`, pgx v5 (PostgreSQL REPEATABLE READ entity
transactions, READ COMMITTED scheduler pool), SQLite, OpenTelemetry metrics,
testcontainers-go.

**Spec:** `docs/superpowers/specs/2026-09-24-598-scheduled-run-ownership-design.md`
(it governs). Evidence: `docs/superpowers/research/2026-09-24-598-scheduler-ownership-research.md`.
Executors read the spec sections their task names.

## Global Constraints

- **TDD is mandatory.** A task's RED is observed before its GREEN
  (`.claude/rules/tdd.md`).
- **Verification tiers.**
  - One package while iterating: `go test ./path/...`.
  - `make test` per merged stream.
  - `make test-full` and `go vet ./...` at the end.
  - `make race` once before the PR.
  - Never add `-count=1` or `-v`. Run `make preflight` first; Docker is
    required.
- **Fail closed** (`.claude/rules/correctness-over-availability.md`). Multi-node
  is the primary target (`.claude/rules/multi-node-primary.md`).
- **A backend that diverges from the others is a bug.** Every backend-agnostic
  behaviour has a `spitest` case or a parity scenario.
- **Go conventions:**
  - use `log/slog` only;
  - wrap errors as `fmt.Errorf("failed to X: %w", err)`;
  - `uuid.UUID`, not string, for tokens and incarnations;
  - put `defer Unlock()` on the line after every `Lock()`;
  - put no test hooks in production code.
- **Security (Gate 3):**
  - never log a token or secret;
  - every tenant-facing store method filters on the tenant;
  - `lastError` passes the allow-list of spec §5.8;
  - a 5xx carries a generic message and a ticket.
- **No issue numbers** in code, comments, logs, errors, help or OpenAPI.
- **Deleting the old path is part of the task that replaces it.** The spec
  §15 exit checks must print nothing at the end.
- **SPI mid-milestone:** stream S works on a branch of
  `../../cyoda-go-spi` (the local checkout at
  `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`), cut from `origin/main`.
  cyoda-go uses it through a local `go work edit -use` line that is never
  committed. At the end, the SPI PR merges into the SPI's `main`. cyoda-go
  then pins that pseudo-version and runs `make repin-plugins` as a new commit.
  No tag. `COMPATIBILITY.md` is updated in the same change.
- **Stage files explicitly.** Never use `git add -A`, because `go.work` must
  stay clean.
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **Settings, with their exact names and defaults:**

  | Setting | Default |
  |---|---|
  | `CYODA_SCHEDULER_ENABLED` | `true` |
  | `CYODA_SCHEDULER_SCAN_INTERVAL` | `1s` |
  | `CYODA_SCHEDULER_MAX_RUNS` | `8` |
  | `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | `4` |
  | `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | `15s` |
  | `CYODA_SCHEDULER_STALE_AFTER` | `2m` (≥ 50 s + 3 × heartbeat) |
  | `CYODA_SCHEDULER_MAX_LOST_OWNERS` | `3` |
  | `CYODA_SCHEDULER_RETRY_DELAY` | `30s` |
  | `CYODA_SCHEDULER_RETRY_DELAY_MAX` | `15m` |
  | `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | `20s` |
  | `CYODA_POSTGRES_SCHEDULER_CONNS` | `10` |

  Removed: `CYODA_SCHEDULER_DISTRIBUTION`, `…_COORDINATOR`,
  `…_REDISPATCH_BACKOFF`, `…_BATCH_SIZE`, `…_EXPIRY_GRACE`,
  `CYODA_DISPATCH_FORWARD_TIMEOUT`.
- **No new error codes.** The existing `CONFLICT`, `BAD_REQUEST`,
  `UNAUTHORIZED`, `SERVER_ERROR` and `STORAGE_UNAVAILABLE` are used. The new
  audit event is `SCHEDULED_TRANSITION_FAIL`. The workflow schema version is
  not bumped.

## Resuming this work in a new session

1. **Read the memory.** `project_598_scheduler_redesign.md` holds the state
   and the rulings.
2. **Where the work is.**
   - Worktree: `.worktrees/598-scheduler-ownership` (inside the main
     checkout).
   - Branch: `feat/598-scheduler-ownership`. Its commits are **local only**
     (not pushed).
   - Spec: `docs/superpowers/specs/2026-09-24-598-scheduled-run-ownership-design.md`.
   - Plan: this directory.
   - Research, design brief and the retry-policy brief:
     `docs/superpowers/research/2026-09-2*-598-*`.
3. **Bring the branch up to date:** `git fetch origin && git rebase
   origin/release/v0.9.0`. Until execution starts the branch holds only
   documents. After a rebase, re-check the line numbers that sections E and W
   cite in files the new commits changed.
4. **Start execution:** wave 1 (S, K, Q-1), with the SPI branch cut in
   `../../cyoda-go-spi` as S-1 describes.

## Review Focus

These are inputs the spec implies but no single task's tests exercise. Each has
a test in the task that owns it.

1. **A compute node's error message with NUL or multi-byte text over 1 024
   bytes.** It is stored intact after sanitising, on every backend, and the
   task never sticks. Owner: R-3 plus the S suite.
2. **A client write to an entity whose task row the scheduler is changing.**
   The client gets a retryable 409, or the operation succeeds after the
   server retries. There is never a 500 and never a lost timer. Owners: W-2,
   W-3 and T-9.
3. **A workflow whose processor writes the entity being fired, through a
   callback, followed by an unsafe processor.** There is no hang and no
   repeat, and the failure stays visible. Owner: E-4.
4. **A pnode that loses the database for longer than `STALE_AFTER` and comes
   back.** It self-cancels, recreates its liveness record, and makes no
   lost-owner claims until one stale period of clean heartbeats has passed.
   Owners: R-6 and T-6.
5. **A rolling deploy while an unsafe processor is in flight.** The callout is
   not cut, no new unsafe dispatch starts, and the task does not end FAILED.
   Owners: R-9 and T-8.

## Sections

| File | Stream | What it delivers |
|---|---|---|
| `S-spi.md` | S | SPI types, interface, errors, audit constant, the `spitest` ScheduledTasks suite |
| `BM-memory.md` | BM | memory store: C1 on task rows, C2 overlay, C6, marks, owners, claim |
| `BQ-sqlite.md` | BQ | sqlite migration and store; sequence-number first-committer-wins |
| `BP-postgres.md` | BP | postgres migration, scheduler pool, the §10.2 statements |
| `K-callout-proof.md` | K | `LocalResult.HandedOff`, the coordinator's sticky flag, `NoHandOffProof` |
| `E-engine.md` | E | run guard, the new fire path, mark, stamp, cancellation checkpoints, reconcile semantics, the model-level flag |
| `R-runner.md` | R | scheduler service (claim loop, heartbeat, watchdog, bookkeeping, shutdown, panics, metrics), config, app and run.go wiring, deletions |
| `W-writes.md` | W | entity delete paths and their server-side retry, workflow-import cleanup, OpenAPI 409 cells, gRPC |
| `Q-query.md` | Q | `GET /scheduled-tasks`: OpenAPI, handler, help topic |
| `T-scenarios.md` | T | e2e, parity and multi-node rows of the spec §13 matrix not owned by a stream |
| `D-docs.md` | D | help, README, ARCHITECTURE, cloud parity, chart, CHANGELOG, COMPATIBILITY, schema-versioning, cross-repo notes |
| `interfaces.md` | — | the names every stream uses; binding |

Each section ends with a `## Stream interface summary` and `## Open points`.

## Order of work

An arrow means "must land first".

```
wave 1   S (SPI, in ../cyoda-go-spi)  ·  K (callout proof)  ·  Q-1 (OpenAPI + generated types)
wave 2   BM · BQ · BP  (each needs S; parallel worktrees)
wave 3   E (needs S, K)  ·  W (needs S, BM/BQ/BP)
wave 4   R (needs E, S, BP pool)  ·  Q-2… (needs S, BM/BQ/BP)
wave 5   T (needs R, W, Q)  ·  D (needs everything it documents)
wave 6   SPI PR merged → pin + make repin-plugins · exit checks (spec §15) · make test-full · go vet ./... · make race
         · fresh-context whole-branch code review · security audit (security-auditor) · PR against release/v0.9.0
```

## Corrections

Each correction below is binding. Where it contradicts a section's text, the
correction governs. Each item names the tasks it changes.

- **C-E1: callback writes to the fired entity.** A run must support a
  processor that writes its own fired entity through a joined callback. E-7
  (rewritten) makes the final persist re-read the entity inside the
  transaction:
  - if this transaction deleted it: commit without re-creating it, outcome
    `cancelled`;
  - if this transaction wrote it: `CompareAndSave(entity, finalTxID)`.

  Spec §5.2 is updated to match.
- **C-G1: the run guard has one shape.** E-5 and R-6/R-9 use exactly:
  ```go
  type RunGuard struct {
      Ref         spi.TaskRef
      Store       spi.ScheduledTaskStore
      Done        <-chan struct{} // self-cancel, panic latch, shutdown step 3
      NoNewUnsafe <-chan struct{} // closed at shutdown step 1; nil = never
      Unsafe      *UnsafeFlight   // brackets each unsafe dispatch
  }
  type UnsafeFlight struct{ /* mutex, count, oldest start */ }
  func (f *UnsafeFlight) Begin()
  func (f *UnsafeFlight) End()
  func (f *UnsafeFlight) Since() (time.Time, bool) // oldest in-flight start; false when none
  ```
  - At each unsafe dispatch site, in this order:
    1. `Unsafe.Begin()`;
    2. if `NoNewUnsafe` is closed, end the run as cut, with an error that
       satisfies `errors.Is(err, context.Canceled)`, and call `Unsafe.End()`;
    3. check `Done`;
    4. `MarkUnsafe`;
    5. dispatch;
    6. `Unsafe.End()`.
  - `(*RunGuard).UnsafeInFlight() bool` stays, as a thin wrapper over
    `Unsafe.Since()`. E-5 implements `UnsafeFlight` in place of its atomic
    counter, and brackets every unsafe dispatch with `Begin`/`End`.
  - The accessor is exported as `workflow.RunGuardFrom(ctx) *RunGuard`,
    because R's tests read the guard from the context their fake `Firer`
    receives. R's name `Draining` is `NoNewUnsafe`.
- **C-S1: `ClaimedFromLostOwner`.** `spi.ScheduledTask` gains
  `ClaimedFromLostOwner bool`. It is read-only and set only on a `ClaimDue`
  result, when that claim took the task from a stale or missing owner.
  - S adds it with a `spitest` case.
  - BM, BQ and BP set it.
  - R reads it for `cyoda.scheduler.claims{reason}`.
- **C-S2: the suite entry point.** It is `runScheduledTasksSuite(t, h, tracker)`.
  A backend whose `ScheduledTaskStore` returns `errors.ErrUnsupported` skips
  the group.
- **C-S3: deterministic rejections.** Error text with a NUL, invalid UTF-8, or
  more than 1 024 bytes is rejected with `spi.ErrStoreRejected` on every
  backend.
  - BP adds `CHECK (octet_length(last_error) <= 1024)` in migration 000014.
  - BM and BQ validate the text themselves.
  - The engine and the scheduler always sanitise the text first (R-3), so this
    only guards against a defect.
- **C-S4: audit rollback.** S adds a conformance case: an audit event recorded
  in a transaction that rolls back is not kept. BM-6 and BQ-7 make it pass.
- **C-S5: rules S added to the interface doc.** They bind every backend:
  - `RemoveLife` is a C1 write even when it removes nothing;
  - `Query` orders ids byte-wise (PostgreSQL uses `COLLATE "C"`);
  - "tenants take turns" means each tenant's first task comes before any
    tenant's second;
  - a `Cancel` id is removed and not reported;
  - `GiveBackIdle` leaves the task claimable at once;
  - `Fail` leaves `LastAttemptTime` unchanged and always overwrites
    `LastError`;
  - `RecordAttempt` may return `ErrTaskBusy`, and the scheduler retries it;
  - a joining write whose tenant differs from the transaction's is refused.
- **C-W1: the retry helper.**
  - It lives in `internal/common`: `common.TaskConflictRetries = 3` and
    `common.RetryOnTaskConflict(ctx context.Context, owned bool, op func() error) error`.
    `entity` imports `workflow`, so workflow import could not reach it in
    `entity`.
  - On owned delete paths the retry covers any `spi.ErrConflict`, whether on
    the entity or on a task row. A delete that races an ordinary update
    succeeds on retry.
  - Spec §7 is updated.
- **C-K1: no-hand-off proof.** A peer's `no_handoff` answer counts as proof
  only for a callout that is not repeat-safe. The coordinator also attaches
  the proof on the criterion parse failure (`entry.go:30-32`). Spec §5.5 is
  updated.
- **C-Q1: `modelName` validation.** A `modelName` with invalid UTF-8 or NUL is
  a 400 `BAD_REQUEST`. Spec §8 is updated.
- **C-R1: the exempt run at shutdown.** §6.4 governs. A run whose unsafe
  callout is in flight at step 3 is never cancelled, and step 4 bounds it.
  Spec §5.3 is aligned.
- **C-R2: startup log line.** R logs `scheduler started` at INFO with
  `incarnation=<uuid>`. T maps claims to pnodes by this line.
- **C-R3: shutdown calls are safe to repeat.**
  - `App.Shutdown` is idempotent; T's shutdown tests call it twice.
  - `Stop` is the full `Drain`, and does nothing after a `Drain`.
- **C-R4: no test bypass for `STALE_AFTER`.** Its floor (≥ 50 s + 3 ×
  heartbeat) stands in tests too.
  - T's lost-owner multi-node scenarios share killed nodes where possible and
    run in parallel.
  - No test hook is added.
- **C-R5: where `RunReport.FailReason` sits.** It is checked right after the
  panic row of §5.6.
- **C-R6: pool gauge.** The existing `cyoda.storage.pool.connections` gauge
  gains a `pool` attribute (`main`, `scheduler`, `heartbeat`), and BP-2
  reports the two new pools through it. `telemetry.md` documents this (D-6).
- **C-T1: build order for e2e.** R-11 and T-2 are executed as one task. T-2's
  rewrites of the tests that rely on removed behaviour land in the same commit
  as R-11's caller fixes, so `go vet ./...` never sees a broken `internal/e2e`.
- **C-T2: unit cells of the entity-write rows.** The U cells of "FAILED task
  re-armed by an update", "FAILED task cancelled when the entity leaves the
  state" and "a task whose transition is no longer scheduled is removed at the
  next write" belong to E-2's reconcile tests.
- **C-T3: waivers.** Two cells are waived:
  - the WARN-without-ticket log half of the cancelled/conflict `lastError` row
    — R-3's `recordedError` unit test covers it;
  - the "no transaction id" half of fire-time CANCEL in parity — no API can
    create that state.
- **C-D1: help text for settings.** R-10 owns `config/scheduler.md`,
  `config.md`, `config/cluster.md` and the README scheduler rows. BP-1 owns
  `config/database.md`, and BP-6 owns `docs/plugins/POSTGRES.md`, including the
  missing `scheduled_tasks` row in its schema table.
  - D-1 and D-2 are dropped; D keeps only what those tasks do not write.
  - `TestConfig_EnvVarCoverage` stays green because the text lands in the same
    commit as the setting.
- **C-D2: chart version.** D-5 bumps the chart `version:` with the template
  change. `appVersion` changes at the release cut.
- **C-X1: exit checks.** They also exclude `COMPATIBILITY.md`, whose release
  rows are history. Spec §15 is updated.
- **C-E2: a failed re-read.** A failed non-joining re-read in the engine
  reports `OutcomeFailed`. The scheduler's fenced bookkeeping then settles
  superseded against failed.
