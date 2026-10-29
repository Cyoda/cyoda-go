# Controllable Platform Clock Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A test-only platform clock that the platform operator can set (past or future), run at a rate from 0.1 to 100, and move forward with step-through firing of scheduled transitions, while production keeps today's time sources.

**Architecture:** One `spi.Clock` interface (`Now() time.Time`) is injected wherever business time is read. Production injects real time; postgres keeps its database clock. With `CYODA_CLOCK_CONTROL_ENABLED=true` the host builds a test clock (an in-memory setting `S`, `R`, `k`). The setting is spread by gossip and handed to the storage plugin through `spi.WithClock`, and `POST /platform/clock` sets it, changes the rate, or runs a move.

**Tech Stack:** Go 1.27.3, `log/slog`, hashicorp/memberlist (existing gossip), pgx/PostgreSQL, sqlite, OpenAPI codegen (`go generate ./api`), gRPC CloudEvents.

**Spec:** `docs/superpowers/specs/2026-10-26-611-platform-clock-design.md` (read it first). Research: `docs/superpowers/research/2026-10-26-611-platform-clock-research.md`.

## Global Constraints

- **Feature switch:** `CYODA_CLOCK_CONTROL_ENABLED`, bool, default `false`.
- **Production is unchanged:** with the feature off, no new store reads, locks, checks or background work. The gossip topic and state exchange are active only with the feature on. Postgres stamp functions take NULL and use `clock_timestamp()` exactly as today.
- **Rate:** 0.1 to 100 inclusive. There is no frozen clock.
- **Input times:** RFC 3339, from 1970-01-01T00:00:00Z to 9999-12-31T23:59:59Z.
- **Restart check tolerance:** 1 s. **Set-back check tolerance:** the call's own duration × rate.
- **Route:** `GET /platform/clock` (any authenticated caller), `POST /platform/clock` (platform operator, `auth.OperatorGuard`). gRPC is read-only: `PlatformClockGetRequest` / `PlatformClockResponse`.
- **New error codes:** `CLOCK_CONTROL_DISABLED` (403) and `CLOCK_BEFORE_RECORDED_TIME` (409, the detail names the latest recorded time).
- **Startup WARN when on:** "platform clock control is enabled: test feature, not for production; the clock is instance-wide and affects every tenant".
- **Stays on real time:** auth (JWT, keys, M2M dates, trusted keys), callout deadlines, gossip, keep-alives, heartbeats, watchdogs, model-cache lease, member `ConnectedAt`, metrics, v1 UUIDs.
- **No issue IDs** (`#NNN`) in code, comments, logs, OpenAPI or help text.
- **TDD per task**, Gate 1. Use `make test` while iterating; never add `-count=1`; never run `make test-full` or `make race` locally (CI runs them).
- **SPI changes** follow `../cyoda-go-spi/MAINTAINING.md`: a PR into SPI `main`, a pseudo-pin in all four `go.mod` files in one commit, no tag.
- **One clock type in the host.** `scheduler.Clock`, `scheduler.NewRealClock` and `workflow.WithScheduledClock(func() time.Time)` are deleted, not kept beside it. Exit check: `grep -rn 'NewRealClock\|WithScheduledClock(' --include='*.go' .` returns only the new option.

## Review Focus

1. **A move on a store with no scheduled tasks at all** (the common seeding case) must finish at once at `T`. It must not wait, and it must not fail on a store that reports no tasks. → Task 10, `TestMove_NoTasks_SetsTargetImmediately`.
2. **`GET /platform/clock` on a deployment with the feature off** must answer real time, rate 1 and `controllable: false`. It must never 5xx. → Task 11, e2e `TestPlatformClock_Get_FeatureOff`.
3. **`time` equal to now together with `rate`** must apply both in one change. It must not be a 409 or a move. → Task 10, `TestChange_TimeEqualNowWithRate_AppliesBoth`.
4. **An `advanceByMs` that would pass year 9999** must be 400 before anything changes. → Task 11, validation table row `advanceByMs_past_9999`.
5. **A past-start deployment's async search** must keep its results through the snapshot TTL measured in platform time. The job must not be reclaimed before its first heartbeat. → Tasks 4–6 conformance and Task 16 e2e `TestPlatformClock_PastStart_AsyncSearchSurvives`.

---

## Stream A: SPI (repository `../cyoda-go-spi`)

### Task 1: `spi.Clock`, `WithClock`, `SchedulerActivity`, search-job liveness contract

**Files:**
- Modify: `../cyoda-go-spi/plugin.go` (factory options, `:51-83`)
- Modify: `../cyoda-go-spi/persistence.go` (`ScheduledTaskStore`, `:137`)
- Modify: `../cyoda-go-spi/types.go` (report type)
- Modify: `../cyoda-go-spi/search_store.go:47-50,124-126,188-202` (doc contract)
- Test: `../cyoda-go-spi/plugin_test.go`

**Interfaces:**
- Produces:
  - `type Clock interface { Now() time.Time }`
  - `func WithClock(c Clock) FactoryOption`
  - `func (c FactoryConfig) Clock() Clock`, which returns nil when no clock was supplied
  - `type SchedulerActivityReport struct { EarliestWaitingMs int64; HasWaiting bool; Running bool }`
  - `ScheduledTaskStore.SchedulerActivity(ctx context.Context) (SchedulerActivityReport, error)`, across all tenants
- Contract text:
  - `SearchJob.HeartbeatTime` is stamped by `CreateJob` from the store's physical clock and is never nil after `CreateJob`.
  - `ClaimStale` measures staleness from `HeartbeatTime` only.
  - A plugin given `WithClock` takes every business stamp from it (submit time, SnapshotTime, consistency time, audit times, `GetSubmitTime` retention, search reaping cutoff) and keeps its own physical clock for liveness.

- [ ] **Step 1: Write the failing test** `TestWithClock_RoundTrip`: `ApplyFactoryOptions([]FactoryOption{WithClock(c)}).Clock() == c`, and `ApplyFactoryOptions(nil).Clock() == nil`.
- [ ] **Step 2:** `cd ../cyoda-go-spi && go test ./ -run TestWithClock` → FAIL (undefined).
- [ ] **Step 3: Implement** the types, the option, the interface method and the doc-comment contract changes listed above.
- [ ] **Step 4:** `go test ./... && go vet ./...` → PASS.
- [ ] **Step 5: Commit** `feat(spi)!: Clock factory option, SchedulerActivity, heartbeat stamped at CreateJob`.

### Task 2: spitest hook and conformance cases

**Files:**
- Modify: `../cyoda-go-spi/spitest/spitest.go` (`Harness`, `:19-60`)
- Create: `../cyoda-go-spi/spitest/clock.go`
- Modify: `../cyoda-go-spi/spitest/asyncsearch.go:455-466` (`Claim/NilHeartbeatBaseline` → replaced)
- Create: `../cyoda-go-spi/spitest/scheduler_activity.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:
  - `Harness.ClockFactory func(c spi.Clock) (spi.StoreFactory, func())`. It is optional and builds a factory on a fresh store with `WithClock(c)`; the returned func closes it. When nil, the `Clock/*` cases skip with the reason "harness has no ClockFactory".
  - The subtest groups `Clock/*` and `ScheduledTasks/SchedulerActivity/*`.

- [ ] **Step 1: Write the cases.** Each uses a `fakeClock` it controls, set far from real time (2015-03-01T00:00:00Z).
  - `Clock/SubmitTimeFollowsClock`: after a commit, `GetSubmitTime(tx)` equals `fake.Now()` to within the store's floor step (1 µs). The entity's `Meta.CreationDate` and its audit event timestamp equal that value.
  - `Clock/ConsistencyTimeFollowsClock`: `ConsistencyTime(ctx)` is ≥ `fake.Now()` and less than `fake.Now()+1s`.
  - `Clock/FloorRefusesNothingBackward`: commit at fake T; set fake to T−1h; the next commit's submit time is greater than the first.
  - `Clock/SubmitTimeRetainedUnderPastClock`: commit, advance the harness's physical clock by 2 h, then `GetSubmitTime(tx)` still answers. This shows retention runs on the business clock, not the physical one.
  - `Clock/ReapUsesClock`: with fake at 2015-03-01, a job finished at fake time is not removed by `ReapExpired(1h)`; after `fake.Advance(61m)` it is.
  - `Clock/OperationalStaysPhysical`: with fake set 1 year ahead, a running search job created and heartbeated is not returned by `ClaimStale(staleAfter=1m)`, and a scheduler owner that just heartbeated survives `SweepOwners(1m)`.
  - `AsyncSearch/Claim/HeartbeatStampedAtCreate` replaces `NilHeartbeatBaseline`: after `CreateJob`, `GetJob(...).HeartbeatTime != nil`, and the job is not claimed until `AdvanceClock(staleAfter+1ms)`.
  - `ScheduledTasks/SchedulerActivity/Empty`: `HasWaiting == false`, `Running == false`.
  - `ScheduledTasks/SchedulerActivity/EarliestAcrossTenants`: WAITING tasks at 3000 ms in tenant A and 2000 ms in tenant B give `EarliestWaitingMs == 2000`; after `ClaimDue` takes one, `Running == true`; a FAILED task is ignored.
- [ ] **Step 2:** `go test ./spitest/...` compiles. The cases have no plugin to run against here; they run in Tasks 4–6.
- [ ] **Step 3: Commit** `test(spitest): clock, scheduler-activity and heartbeat-at-create conformance`.
- [ ] **Step 4:** Push the SPI branch and open a PR into SPI `main`. Then, in cyoda-go, pseudo-pin all four `go.mod` files to that commit in one commit (`make repin-plugins`), add the `go.work` use line uncommitted, and update `COMPATIBILITY.md`. Commit `build: pin cyoda-go-spi with platform clock support`.

## Stream B: in-tree plugins (one task each, parallel after Task 2)

### Task 3: Shared plugin test helper

**Files:**
- Create: `internal/testing/fakeclock/fakeclock.go`. It is used by the root-module tests; each plugin keeps its own copy because it is a separate module.

**Interfaces:**
- Produces: `fakeclock.New(t time.Time) *Clock` with methods `Now() time.Time`, `Set(time.Time)` and `Advance(time.Duration)`. It is safe for concurrent use.

- [ ] **Step 1: Write the failing test** `TestFakeClock_SetAdvance`.
- [ ] **Step 2:** run it, FAIL.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** run it, PASS.
- [ ] **Step 5:** commit.

### Task 4: memory plugin

**Files:**
- Modify: `plugins/memory/store_factory.go` (add a business clock field, default the physical clock; read `spi.ApplyFactoryOptions(opts).Clock()` in `plugin.go:25`, which today ignores `opts`)
- Modify: `plugins/memory/txmanager.go:662-674,744-755,1005-1006,1193,1293-1302` (business clock)
- Modify: `plugins/memory/search_store.go:376-395` (stamp `HeartbeatTime` at `CreateJob` from the physical clock; remove the `CreateTime` fallback; reaping cutoff on the business clock)
- Modify: `plugins/memory/scheduled_task_claims.go` (`SchedulerActivity` under `entityMu`; owner liveness stays on the physical clock)
- Modify: `plugins/memory/conformance_test.go` (set `ClockFactory`)

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces: none new beyond the SPI.

- [ ] **Step 1:** Set `Harness.ClockFactory` in `conformance_test.go`. Run `cd plugins/memory && go test ./...` → `Clock/*`, `SchedulerActivity/*` and `HeartbeatStampedAtCreate` FAIL.
- [ ] **Step 2:** Implement the changes listed under Files.
- [ ] **Step 3:** `go test ./...` → PASS, including the existing suite unchanged.
- [ ] **Step 4:** Commit `feat(memory): business clock from spi.WithClock; heartbeat stamped at CreateJob; SchedulerActivity`.

### Task 5: sqlite plugin

**Files:**
- Modify: `plugins/sqlite/plugin.go:38` and `store_factory.go` (forward `opts`; business clock field)
- Modify: `plugins/sqlite/txmanager.go:666-674,755,797-807,950,986,998-1001,1232-1250,1379-1404`
- Modify: `plugins/sqlite/search_store.go:334,373,408,418,445` (heartbeat at create; no fallback; reap cutoff on the business clock)
- Modify: `plugins/sqlite/scheduled_task_claims.go` (`SchedulerActivity` under the commit gate)
- Create: `plugins/sqlite/migrations/000012_drop_schema_ext_created_at.up.sql` and `.down.sql` (drop `model_schema_extensions.created_at`, `000001:149`)
- Modify: `plugins/sqlite/conformance_test.go`

- [ ] **Step 1:** Set `ClockFactory` → the cases FAIL.
- [ ] **Step 2:** Add a migration test `TestMigration_SchemaExtensionsHasNoCreatedAt` (`PRAGMA table_info`) → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `cd plugins/sqlite && go test ./...` → PASS.
- [ ] **Step 5:** Commit.

### Task 6: postgres plugin

**Files:**
- Create: `plugins/postgres/migrations/000017_platform_clock.up.sql` and `.down.sql`:
  - re-create `cyoda_stamp(tenant_key int4, at_us bigint DEFAULT NULL)` and `cyoda_consistency_time(tenant_key int4, wait_budget_ms bigint, at_us bigint DEFAULT NULL)`, using `coalesce(at_us, <clock_timestamp µs>)` where `000016:50` and `:66` read the clock (the wait-budget deadline stays on `clock_timestamp()`);
  - drop the `DEFAULT now()` on `search_jobs.created_at`;
  - drop `model_schema_extensions.created_at`.
- Modify: `plugins/postgres/plugin.go:37` (read `Clock()`), `transaction_manager.go:354,415,543-560`, `entity_store.go:242,298-310,360,372-380,432,774,836`, `consistency_time.go:225`, `search_store.go:109,217-240,587,630`, `scheduled_task_store.go` (`SchedulerActivity`)
- Modify: `plugins/postgres/conformance_test.go:144-197` (set `ClockFactory`; correct the stale comment at `:146-150`, since commit stamping replaced the `CURRENT_TIMESTAMP` mechanism)

**Interfaces:**
- Consumes: Tasks 1–2.
- Rules:
  - Without a clock: pass NULL `at_us`. Placeholder columns `entities.creation_date`, `entities.last_modified` and `entity_versions.transaction_time` are written explicitly from the `SELECT CURRENT_TIMESTAMP` value, the same value the defaults give today.
  - With a clock: pass `clock.Now()` µs as `at_us`; the placeholder select uses it; pruning and the reap cutoff use `clock.Now()`.
  - `CreateJob` writes `heartbeat_time = now()`. `ClaimStale` drops `COALESCE(heartbeat_time, created_at)` for `heartbeat_time`.
  - The comment at the `wall_clock_time` write (`entity_store.go:360`) is replaced with: physical moment of insertion, kept for debugging and analysis; nothing reads it; under a test clock it keeps physical time and so differs from the business times.

- [ ] **Step 1:** Set `ClockFactory` → the cases FAIL.
- [ ] **Step 2: Write the failing test** `TestClock_PlaceholderDatesInsideTx` in `plugins/postgres/platform_clock_test.go`: with a fake clock at 2015-03-01, save an entity inside a transaction and read it back in the same transaction; `Meta.CreationDate` and `LastModifiedDate` are within 1 s of 2015-03-01.
- [ ] **Step 3: Write the failing test** `TestNoClock_StampFunctionsUnchanged`: without a clock, a commit's stamp is within 1 s of `SELECT clock_timestamp()`. This pins the production branch.
- [ ] **Step 4:** Implement the migration and the Go changes.
- [ ] **Step 5:** `cd plugins/postgres && go test ./...` → PASS. Run `make test` at the root → PASS (parity unaffected).
- [ ] **Step 6:** Commit `feat(postgres): optional stamp time for a test clock; heartbeat stamped at CreateJob; drop dead defaults`.

## Stream C: host

### Task 7: `internal/platformclock` clock types

**Files:**
- Create: `internal/platformclock/clock.go`, `internal/platformclock/setting.go`
- Test: `internal/platformclock/clock_test.go`

**Interfaces:**
- Produces:
  - `func Real() spi.Clock`, which returns `time.Now()`
  - `type Setting struct { StartUnixMicro, AnchorUnixMicro, RatePPM int64; Version uint64; Writer string }`
  - `func (a Setting) Newer(b Setting) bool`: compares `(Version, Writer)` lexicographically
  - `type Test struct` (unexported fields), built with `func NewTest(nodeID string, src func() time.Time) *Test`. `src` defaults to `time.Now` and must return values with monotonic readings.
  - `func (c *Test) Now() time.Time`: computes `S + k×(elapsed since R)`. `R` is converted once to a monotonic anchor when a setting is applied; elapsed time below 0 counts as 0.
  - `func (c *Test) Setting() Setting`
  - `func (c *Test) Apply(s Setting) bool`: takes `s` only if `s.Newer(current)`
  - `func (c *Test) Set(at *time.Time, ratePPM *int64) Setting`: a local change. `nil` for `at` keeps now; `nil` for the rate keeps the rate. It bumps the version, sets `Writer = nodeID` and returns the new setting.
  - Constants `MinRatePPM = 100_000`, `MaxRatePPM = 100_000_000`.
- The identity setting before any change is `Start = R = construction time`, `RatePPM = 1_000_000`, `Version = 0`.

- [ ] **Step 1: Write failing tests:**
  - `TestTest_IdentityTracksSource`;
  - `TestTest_SetPast_RunsAtRate` (rate 2: after 10 s of source time, `Now` = start + 20 s);
  - `TestTest_RateChangeDoesNotJump`;
  - `TestTest_ApplyIgnoresOlder`;
  - `TestTest_ApplyConvertsAnchor` (a setting made on another clock at the same wall time gives the same `Now` to within 1 ms);
  - `TestSetting_NewerTieBreakByWriter`.
- [ ] **Step 2:** `go test ./internal/platformclock/...` → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit.

### Task 8: Inject the clock everywhere business time is read

**Files:**
- Delete: `internal/scheduler/clock.go`; change `scheduler.Deps.Clock` to `spi.Clock` (`service.go:33-45`)
- Modify: `internal/scheduler/service.go:607` (read the clock before `claimRequest` takes `s.mu`), `:690`
- Modify: `internal/domain/workflow/engine.go:155-158,173,213-219,236,1110`. Replace `WithScheduledClock(func() time.Time)` with `WithClock(spi.Clock)`. `arm.go:115` and `fire_scheduled.go:91,186` use `e.clock.Now()`.
- Modify: `internal/domain/entity/handler.go:89` (`New(..., clock spi.Clock)`) and `service.go:282,2015,2271,2528`
- Modify: `internal/domain/model/handler.go:24` (`New(factory, clock)`) and `service.go:195`
- Modify: `internal/domain/search/service.go:303` (`NewSearchService(..., clock)`) and `:967,1183,1189,1208,1216,1228,1315,1496`; `reaper.go:258,292`
- Modify: `app/app.go:495-521,543-544` and the search wiring. For now pass `platformclock.Real()` everywhere (Task 14 switches it).
- Tests: existing tests that construct these types get `platformclock.Real()` or a `fakeclock`.

**Interfaces:**
- Consumes: Task 7 (`platformclock.Real`), Task 3 (`fakeclock`).
- Produces: the constructor signatures above. Task 14 relies on them.

- [ ] **Step 1: Write failing tests**, each driving its site with a `fakeclock` at 2015-03-01:
  - `TestEngine_AuditTimestampFromClock` (workflow);
  - `TestArm_ScheduledTimeFromClock` (arm: `ScheduledTime == clock + DelayMs`);
  - `TestScheduler_ClaimNowFromClock` (scheduler: `ClaimRequest.NowMs` equals the fake);
  - `TestModelUpdateDateFromClock`;
  - `TestSearchJobCreateTimeFromClock`;
  - `TestEntityPreCommitDatesFromClock`.
- [ ] **Step 2:** Run each package's tests → FAIL.
- [ ] **Step 3:** Implement. Delete `scheduler.NewRealClock` and the function-typed option.
- [ ] **Step 4:** The exit check from Global Constraints returns nothing old. Then `grep -rn 'time\.Now()' internal/domain/entity/service.go internal/domain/model/service.go internal/domain/search/service.go internal/domain/search/reaper.go` → empty.
- [ ] **Step 5:** `make test` → PASS.
- [ ] **Step 6:** Commit `refactor: one injected clock for every business time read`.

### Task 9: Gossip carriage of the setting

**Files:**
- Modify: `internal/cluster/registry/gossip.go:552-606`. Add `func (g *Gossip) SetStateExchange(local func() []byte, merge func([]byte))`. `LocalState` and `MergeRemoteState` call these when set, and return nil or do nothing otherwise (as today).
- Create: `internal/platformclock/sync.go`
- Test: `internal/platformclock/sync_test.go`, `internal/cluster/registry/gossip_state_test.go`

**Interfaces:**
- Consumes: Task 7, `spi.ClusterBroadcaster` (`Broadcast(topic, payload)`, `Subscribe(topic, handler)`).
- Produces:
  - `const Topic = "platform.clock"`
  - `func Attach(c *Test, b spi.ClusterBroadcaster, ex StateExchanger)`, with `type StateExchanger interface{ SetStateExchange(local func() []byte, merge func([]byte)) }`. It subscribes, applies newer settings, and registers the exchange.
  - `func Publish(c *Test, b spi.ClusterBroadcaster, s Setting)`
  - The encoding is JSON of `Setting`. A payload that does not decode is dropped with a WARN.

- [ ] **Step 1: Write failing tests:**
  - `TestSync_BroadcastAppliesNewer` and `TestSync_IgnoresOlderAndGarbage`, with an in-memory broadcaster fake;
  - `TestGossip_StateExchangeOnJoin`: two real `registry.Gossip` instances on loopback; the first holds a setting; after the second joins, its `Test.Setting()` equals it before `Register` returns;
  - `TestGossip_NoStateExchangeByDefault`: `LocalState` returns nil.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit.

### Task 10: Clock service: set, move, rate

**Files:**
- Create: `internal/platformclock/service.go`
- Test: `internal/platformclock/service_test.go`

**Interfaces:**
- Consumes: Tasks 7 and 9, `spi.TransactionManager.ConsistencyTime(ctx)`, `spi.ScheduledTaskStore.SchedulerActivity(ctx)`.
- Produces:
  - `type Change struct { At *time.Time; AdvanceBy *time.Duration; RatePPM *int64 }`
  - `type State struct { Now time.Time; RatePPM int64; Controllable bool }`
  - `func NewService(c *Test, tm spi.TransactionManager, tasks func(ctx context.Context) (spi.ScheduledTaskStore, error), publish func(Setting), poll time.Duration) *Service`
  - `func (s *Service) State() State`
  - `func (s *Service) Change(ctx context.Context, ch Change) (State, error)`
  - `var ErrBeforeRecordedTime` (wrapped with the latest recorded time; `errors.As` into `*BeforeRecordedTimeError{Latest time.Time}`)
  - `func StartupCheck(ctx context.Context, tm spi.TransactionManager, now time.Time) error`: calls `ConsistencyTime` with tenant `PLATFORM` in the context, and errors if the result is after `now + 1s`.
- Behaviour (spec, "Changing the clock"):
  - **Set back** (`At` ≤ now): apply locally, call `ConsistencyTime` (tenant `PLATFORM`), and if `C > At + elapsed×rate`, restore the previous setting and return `ErrBeforeRecordedTime{Latest: C}`. Otherwise publish.
  - **Move** (target > now): the loop from the spec. It polls every `poll`; it sets the clock only forward (`if now < e`, `if now < T`); it applies `RatePPM` at the end; it publishes every change; it stops on `ctx.Done()`, leaving the clock where it is.
  - **Rate only:** re-anchor at now and publish.
  - `At` and `RatePPM` together, with `At` ≤ now: one change.

- [ ] **Step 1: Write failing tests**, with a fake `TransactionManager` and `ScheduledTaskStore` driven by a fake clock and a fake scheduler that "fires" a task by removing it after a poll:
  - `TestMove_NoTasks_SetsTargetImmediately`;
  - `TestMove_StepsThroughDueTimesInOrder`: tasks at +1h and +3h, and the +1h fire arms one at +2h; the clock values set are exactly [+1h, +2h, +3h, T];
  - `TestMove_NeverSetsBackward`: the fake scheduler advances the source past `T` while firing the last task; no setting with `Start < previous Now` is published;
  - `TestMove_WaitsWhileRunning`;
  - `TestMove_CancelLeavesClock`;
  - `TestSetBack_RefusedWhenRecordedLater` (`ConsistencyTime` returns later; the setting is restored and nothing is published);
  - `TestSetBack_FreshStoreAccepted`;
  - `TestChange_TimeEqualNowWithRate_AppliesBoth`;
  - `TestRateOnly_NoJump`;
  - `TestStartupCheck_AheadFails` and `TestStartupCheck_BehindPasses`.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit.

### Task 11: HTTP API

**Files:**
- Modify: `api/openapi.yaml`:
  - operations `getPlatformClock` and `changePlatformClock` at `/platform/clock`;
  - schemas `PlatformClockDto` (`now` date-time, `rate` number, `controllable` boolean) and `PlatformClockChangeDto` (`time` date-time, `advanceByMs` int64 minimum 1, `rate` number 0.1–100);
  - responses reuse `QueryParameterRefused`, `Unauthorized`, `Forbidden`, `ServiceUnavailable`, `InternalServerError` as `/entity/consistency-time` does (`:982-1008`), plus 409.
  - Then run `go generate ./api`.
- Modify: `internal/common/error_codes.go`: add `ErrCodeClockControlDisabled = "CLOCK_CONTROL_DISABLED"` and `ErrCodeClockBeforeRecordedTime = "CLOCK_BEFORE_RECORDED_TIME"` to the constants and to the known-codes set (`:340-350`).
- Create: `cmd/cyoda/help/content/errors/CLOCK_CONTROL_DISABLED.md` and `CLOCK_BEFORE_RECORDED_TIME.md`. `TestErrCode_Parity` enforces them.
- Create: `internal/platformclock/handler.go`
- Modify: `app/app.go` (server field wiring, near `:543-551`)
- Test: `internal/platformclock/handler_test.go`, `internal/e2e/platform_clock_api_test.go`

**Interfaces:**
- Consumes: Task 10.
- Produces: `func NewHandler(svc *Service, guard auth.OperatorGuard, enabled bool) *Handler`, which implements the generated server methods.
- Order of checks (spec error table):
  1. query-parameter refusal;
  2. 401;
  3. operator 403;
  4. 403 `CLOCK_CONTROL_DISABLED`;
  5. 400 validation;
  6. 409 `CLOCK_BEFORE_RECORDED_TIME`;
  7. 503;
  8. 500.
- Without the feature, `GET` answers `platformclock.Real()` time, rate 1 and `controllable: false`.

- [ ] **Step 1: Write failing handler unit tests:**
  - one table row per 400 refusal: `malformed`, `unknown_member`, `empty`, `both_time_and_advance`, `advance_zero`, `advance_overflow`, `advanceByMs_past_9999`, `rate_below`, `rate_above`, `time_not_rfc3339`, `time_before_1970`, `time_after_9999`;
  - `TestHandler_503_StoreFails` and `TestHandler_500_Internal`, with a fake service.
- [ ] **Step 2: Write failing e2e tests** on an isolated stack (`newStackOn` in `internal/e2e/scheduler_harness_test.go:118`, configure `cfg.ClockControlEnabled = true`):
  - `TestPlatformClock_Get_FeatureOff`;
  - `TestPlatformClock_Post_Disabled403`;
  - `TestPlatformClock_Post_NonOperator403`;
  - `TestPlatformClock_Post_OnBehalfOf403`;
  - `TestPlatformClock_Unauthenticated401` (GET and POST);
  - `TestPlatformClock_QueryParamRefused400` (GET and POST);
  - one e2e per 400 class (a representative row of each);
  - `TestPlatformClock_BeforeRecordedTime409` (write an entity, then POST a `time` one hour earlier; the detail contains the latest time).
  
  The 503 and 500 e2e cells are waived as in the spec matrix.
- [ ] **Step 3:** FAIL.
- [ ] **Step 4:** Implement.
- [ ] **Step 5:** `go test ./internal/platformclock/...` PASS. `go test -timeout 30m ./internal/e2e/ -run PlatformClock` PASS.
- [ ] **Step 6:** Commit.

### Task 12: gRPC read

**Files:**
- Modify: `internal/grpc/cloudevent_types.go:84-85` (add `PlatformClockGetRequest` and `PlatformClockResponse`)
- Modify: `api/grpc/events/types.go`
- Create: `docs/cyoda/schema/platform/PlatformClockGetRequest.json` and `PlatformClockResponse.json`; add both to `docs/cyoda/schema/common/CloudEventType.json`
- Modify: the handler, alongside `EntityConsistencyTimeGetRequest` in `internal/grpc/search.go:~130-150`
- Test: `internal/grpc/platform_clock_test.go`

- [ ] **Step 1: Write failing tests** asserting the envelope (`Success`, `Error.Code`):
  - `TestPlatformClock_GRPC_Success` (`now`, `rate`, `controllable`);
  - `TestPlatformClock_GRPC_Unauthenticated`;
  - the existing schema-coverage test (every CloudEvent type has a published schema) fails until the schemas exist.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit.

### Task 13: Platform time on callout requests

**Files:**
- Modify: `internal/grpc/dispatch.go:161`. `ProcessorDispatcher` gets the clock (`NewProcessorDispatcher(..., clock spi.Clock)`, `:39`), and the request CloudEvent gets `Time: timestamppb.New(clock.Now())`. `NewCloudEvent` stays as it is for every other event.
- Modify: `cmd/compute-test-client/catalog.go:303`. The test client computes absolute `fireAt` from the request's `time` attribute.
- Test: `internal/grpc/dispatch_time_test.go`; e2e in Task 16.

- [ ] **Step 1: Write the failing test** `TestDispatch_CalloutCarriesClockTime` (fake clock at 2015-03-01; the request event's `time` equals it).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS.
- [ ] **Step 5:** Commit.

### Task 14: Configuration, wiring and startup

**Files:**
- Modify: `app/config.go` (`ClockControlEnabled bool` from `CYODA_CLOCK_CONTROL_ENABLED`, default false, in `DefaultConfig()`), the config registry binding (`app/config_registry_binding_test.go` lists every variable)
- Modify: `app/app.go`:
  - **with the feature on:** build `platformclock.NewTest(nodeID, nil)`; pass it to the plugin via `spi.WithClock` (`:176-186`) and to every constructor from Task 8; in cluster mode call `Attach` before gossip `Register`, and move `Register` (`:436`) ahead of the search reapers (`:426`); log the startup WARN; with no peer setting after `Register` (`Setting().Version == 0`), run `StartupCheck` and exit on error as the other startup failures do;
  - **with the feature off:** `platformclock.Real()` everywhere, and none of the above.
- Create: `cmd/cyoda/help/content/config/clock.md`; modify `README.md` (configuration table)
- Test: `app/clock_wiring_test.go`

- [ ] **Step 1: Write failing tests:**
  - `TestClockWiring_OffUsesReal` (a deployment built with defaults stamps entities within 1 s of real time, and `LocalState` returns nil);
  - `TestClockWiring_OnSetsPluginClock`;
  - the config-registry binding test rows for the new variable;
  - the help test that requires every `CYODA_*` variable to be documented (fails until `config/clock.md` exists).

  Moving gossip `Register` ahead of the search reapers is an ordering property with no observable test short of a test hook in production code, which the project does not add. It is checked in code review, and the commit message records the TDD waiver.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** Implement, with the help topic and the README row.
- [ ] **Step 4:** PASS; `make test` PASS.
- [ ] **Step 5:** Commit.

### Task 15: Help and reference documentation

**Files:**
- Create: `cmd/cyoda/help/content/clock.md` (`cyoda help clock`, contents per the spec's "Documentation" section, including every restriction and the instance-wide effect)
- Modify: `cmd/cyoda/help/content/cloudevents.md` (the `time` attribute on callouts), `scheduled-tasks.md` (moves fire at due times; due-order limit of scan interval × rate), `crud.md` (point-in-time "now" follows the clock)
- Create: `docs/cloud-parity/platform-clock.md`; add a row in `docs/cloud-parity/README.md`
- Modify: `CHANGELOG.md` (`### Added`; `### Breaking` for plugin authors: `SchedulerActivity`, and `CreateJob` stamping `HeartbeatTime`)

- [ ] **Step 1:** Write the topics. Keep them compact: the actionable core plus a pointer. This task is documentation; there is no failing test to write first.
- [ ] **Step 2:** `go test ./cmd/cyoda/help/...` PASS; `go run ./cmd/cyoda help clock` renders the topic.
- [ ] **Step 3:** Commit.

### Task 16: Behaviour e2e

**Files:**
- Create: `internal/e2e/platform_clock_test.go`. Each test uses its own `schedDB` stack with the feature on and the compute test client where a schedule or processor is needed.

- [ ] **Step 1: Write the tests:**
  - `TestPlatformClock_PastStart_FirstRecordsCarryIt`: POST `time` 2015-03-01T09:00Z on a fresh stack; create an entity; its `creationDate`, its audit event time, `GET /entity/consistency-time` and `GET /platform/clock` are all within 5 s of 2015-03-01T09:00Z.
  - `TestPlatformClock_AdvanceOneDay` (the issue's "done when"):
    - start at 2015-03-01T09:00Z; create an entity whose workflow arms a transition at +3 h;
    - POST `advanceByMs: 86400000`;
    - then: a new revision is dated about day 2 09:00; the audit event for the scheduled transition is dated within 2 s of day 1 12:00; a point-in-time read with no `pointInTime` answers the new state; `GET` returns about day 2 09:00.
  - `TestPlatformClock_MoveOrderAcrossTenants`: three entities in two tenants with due times +1h, +2h, +4h, where the +2h fire arms a +3h one; audit times in due order, each within 2 s of its due time.
  - `TestPlatformClock_TimeLimitShorterThanMove_Fires`: a schedule with a 1 h time limit due at +2 h; move +1 day; the transition fired and is not `EXPIRE`d.
  - `TestPlatformClock_RateTwo`: POST `rate: 2`; after about 2 s of real time, `GET` has advanced about 4 s; a rate change does not jump the time.
  - `TestPlatformClock_PastStart_AsyncSearchSurvives`: past start; submit an async search; wait past one reaper interval (configure a short `CYODA_SEARCH_REAP_INTERVAL` on the stack); the results are still retrievable; `GET` transitions by `transactionId` answers.
  - `TestPlatformClock_InterruptedMove`: the stack runs with `CYODA_SCHEDULER_ENABLED=false`; a task is due at +1 h inside a +1 day window; POST the move with a 2 s client timeout; afterwards `GET` reports a time within 2 s of +1 h (the step the move reached and waits at). Continuing after an interruption is covered by `TestMove_CancelLeavesClock` in Task 10.
  - `TestPlatformClock_PlaceholderDatesInsideTx`: under a past clock, a client transaction saves and reads back an entity; the dates are platform time.
  - `TestPlatformClock_CalloutTimeAttribute`: a schedule function returns an absolute fire time computed from the request's `time` + 1 h; after a move of +2 h it has fired at that time.
  - `TestPlatformClock_TwoNodes_SettingPropagates`: two nodes in cluster mode on one database, built the way `internal/e2e/async_cancel_multinode_test.go` builds its nodes; POST on A; within 2 s, `GET` on B matches. Start a third node afterwards; its first `GET` reports the same setting.
  - `TestPlatformClock_StartupCheck_FunctionOnRealStore`: write with a clock set one day ahead; `platformclock.StartupCheck` returns an error with the real time; with a clock set in the past it returns nil. The process-exit path is waived, as in the spec.
- [ ] **Step 2:** Run against the code from Tasks 1–15: `go test -timeout 30m ./internal/e2e/ -run PlatformClock`. If a test is green at once, check it would have been red without the feature (temporarily stub the service's move to a jump), and record that in the commit message.
- [ ] **Step 3:** Fix what fails.
- [ ] **Step 4:** Commit.

### Task 17: Cross-backend parity

**Files:**
- Modify: `e2e/parity/fixture.go:19-57`. Add the optional interface `FreshStackFixture { NewFreshStack(t *testing.T, env map[string]string) (baseURL string, token string, stop func()) }`.
- Modify: the in-tree fixtures (memory, sqlite, postgres) to implement it.
- Create: `e2e/parity/platform_clock.go` with the scenarios. Register them in `e2e/parity/registry.go`; a scenario skips when the fixture lacks the interface.

- [ ] **Step 1: Write the scenarios:**
  - `PlatformClockPastStart` (as the Task 16 first test);
  - `PlatformClockAdvanceOneDay` (as the "done when" test);
  - `PlatformClockBeforeRecordedTime409`;
  - `PlatformClockPastStartAsyncSearch`.
- [ ] **Step 2:** `make test` → they fail until the fixtures implement the interface.
- [ ] **Step 3:** Implement the fixtures.
- [ ] **Step 4:** `make test` → PASS on all three backends.
- [ ] **Step 5:** Commit.

### Task 18: Follow-up tickets

- [ ] **Step 1:** Open an issue in `Cyoda/cyoda-go-cassandra`. The body is the spec's "cassandra" bullet list, plus: implement `SchedulerActivity` (none while scheduled tasks are unimplemented), the heartbeat-at-create contract, `ClockFactory` in its conformance harness, and `FreshStackFixture` in its parity fixture. It must land before its v0.9.0 bump.
- [ ] **Step 2:** Create the CaaS ticket in CP (project CP, `[CaaS]` title prefix, component CaaS) for the endpoints, the gRPC types, the callout `time` attribute and the configuration, pointing to `docs/cloud-parity/platform-clock.md`.
- [ ] **Step 3:** Record both links in the PR body and in tracker #651's row 11.

## Final verification (before the PR)

- [ ] `make test` and `go vet ./...` at the root; `go test ./...` and `go vet ./...` in each `plugins/*`. All green.
- [ ] The exit checks from Global Constraints return nothing old.
- [ ] Code review, then the cyoda-go security audit, both on the exact PR head. Then push and wait for CI green on that head.
