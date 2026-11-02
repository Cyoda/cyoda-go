# Cluster Status, Node Lifecycle and gRPC Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One operator call, `GET /platform/cluster`, lists every node in the gossip directory with its reachability, lifecycle state, database reachability, version and clock setting and says whether the cluster is operational. It comes with lifecycle states in gossip metadata, a storage SPI `Ping`, `/readyz` failing on drain, token-free `grpc.health.v1` `Check` and a member-count metric.

**Architecture:** A health latch type replaces `*atomic.Bool` and notifies when it trips. A lifecycle holder derives `STARTING`/`NORMAL`/`DRAINING`/`UNHEALTHY` from three one-way facts and publishes each change through one gossip metadata builder. After the feat-611 rebase, a new sealed peer route (`POST /internal/dispatch/status`) answers each node's own status. A `clusterstatus` service asks every directory member in parallel through `HTTPForwarder`, builds entries, judges `operational` and runs the `waitForNodes` rounds. The HTTP handler serves it under the operator guard.

**Tech Stack:** Go 1.27.3, `log/slog`, hashicorp/memberlist, OpenTelemetry metrics, grpc-go v1.83.2 (`google.golang.org/grpc/health/grpc_health_v1`), oapi-codegen (`go generate ./api`), pgx, sqlite, testcontainers e2e.

**Spec:** `docs/superpowers/specs/2026-11-01-696-cluster-status-design.md` (read it first). The peer channel it reuses is in `docs/superpowers/specs/2026-10-26-611-platform-clock-design.md`, "The peer channel", and its plan is `docs/superpowers/plans/2026-11-01-611-clock-confirmation.md` on `feat-611-platform-clock`.

## Global Constraints

- **Route:** `GET /platform/cluster`, OpenAPI tag `Platform Cluster`, operationId `getPlatformCluster`, platform operator only (`auth.OperatorGuard`; on-behalf-of refused). HTTP only.
- **Query:** `waitForNodes` integer 1–1000, optional. `timeoutMs` integer 1–600000, default 30000, refused with 400 when given without `waitForNodes`. The 400 code is `BAD_REQUEST`, and `properties.parameter` names the parameter.
- **Body:** the same `ClusterStatusDto` for 200 and 503, written with `common.WriteJSON`, never `common.WriteError`. The body never carries node addresses, peer URLs, forwarder or database error text, or stack traces.
- **Lifecycle:** precedence `UNHEALTHY` > `DRAINING` > `STARTING` > `NORMAL`. Nothing is undone. cyoda-go sets every transition itself; an embedder calls nothing extra.
- **Peer route:** `POST /internal/dispatch/status`, sealed like every peer route. The request body is empty JSON `{}`. Each ask is bounded by `CYODA_PROXY_TIMEOUT`, and each connect by `CYODA_DISPATCH_CONNECT_TIMEOUT`. The answering node's `Ping` is bounded by half its `CYODA_PROXY_TIMEOUT`.
- **Re-ask interval** of a wait: 1 s.
- **Answer bounds** (builder's call, fixed here): `version` and `writer` at most 256 bytes and valid UTF-8; clock `version` ≤ `math.MaxInt64`; `state` one of the four.
- **Metadata key** for the state: `"st"`.
- **Metric:** `cyoda.cluster.member.count`, `Int64ObservableGauge`, unit `{member}`, attribute `cyoda.cluster.member.state` ∈ `starting|normal|draining|unhealthy`. Cluster mode only.
- **SPI:** `Ping(ctx context.Context) error` on `spi.StoreFactory`. The change follows `../cyoda-go-spi/MAINTAINING.md`: a PR into SPI `main`, a pseudo-pin of merged `main` in all four `go.mod` files in one commit, no tag. While the PR is open, the `go.work` `use ../../../cyoda-go-spi/.worktrees/feat-696-cluster-status` line is added and never committed.
- **No issue IDs** (`#NNN`) in code, comments, logs, OpenAPI or help text.
- **Time reads:** every real-time read added (wait deadline, re-ask timer, Ping bound) carries `// realtime: <reason>` (`.claude/rules/time-reads.md`; `internal/platformclock/time_reads_test.go` enforces it).
- **TDD per task** (Gate 1). Iterate with the touched package's `go test` and `make test`. Never add `-count=1`. Never run `make test-full` or `make race` locally. For e2e, run `go test -timeout 30m ./internal/e2e/ -run '<Name>'`.
- **No test hooks in production code.** Where a property is non-functional, record the waiver in the commit message.

## Review Focus

1. **A peer that accepts the connection and never answers** must not hold a round past `CYODA_PROXY_TIMEOUT`. The other members' entries stay `UP`. → Task 13, `TestRound_HangingPeerBoundedByAskTimeout`.
2. **A sealed answer that is well formed but wrong** (state `"normal"` in lower case, a 10 KB writer, a clock version above `MaxInt64`) makes the member `DOWN`, and none of its text reaches the body. → Task 11, `TestNodeStatus_Validate` table, and Task 13, `TestRound_InvalidAnswerIsDown`.
3. **`waitForNodes` without `timeoutMs`** waits the 30 s default. It does not wait forever or answer at once. → Task 14, `TestHandler_WaitDefaultsTimeout30s`.
4. **A latch trip racing a drain** publishes `UNHEALTHY` last. No observer ever sees a state of lower rank after a higher one. → Task 4, `TestLifecycle_ConcurrentTripAndDrain_MonotoneDelivery`.
5. **`DRAINING` set right before `Deregister`** is still advertised. A pending re-advertise runs before the re-advertiser stops. → Task 6, `TestDeregister_RunsPendingReadvertise`.

## feat-611 interfaces (filled in by Task 9)

Tasks 10–16 use these by the role named here. Task 9 writes the names that landed on `feat-611-platform-clock` into the right-hand column and commits this file.

| Role | Landed name |
|---|---|
| Registry method that lists every directory member, including one whose metadata does not parse | |
| The `HTTPForwarder` that `app.New` builds in cluster mode whatever `ExternalProcessing` is | |
| How the clock route is registered beside the hand-over route (handler type, constructor) | |
| The e2e cluster fixture that joins a node through a real node (if one landed) | |
| The test clock's setting accessor (today `(*platformclock.Test).Setting()`) | |

---

## Phase A — independent of feat-611

### Task 1: SPI `Ping` and its conformance cases (repository `../cyoda-go-spi`)

**Files:**
- Create a worktree: `cd ../cyoda-go-spi && git fetch && git worktree add .worktrees/feat-696-cluster-status -b feat-696-cluster-status origin/main`. All paths below are in it.
- Modify: `persistence.go` (`StoreFactory`, beside `Close`)
- Create: `spitest/ping.go`
- Modify: `spitest/spitest.go` (call the new group from `StoreFactoryConformance`)

**Interfaces:**
- Produces: `StoreFactory.Ping(ctx context.Context) error`, with the doc comment exactly as in the spec ("Ping reports whether the store answers. It returns within ctx and returns an error when ctx is done. After Close it returns an error."). Also the subtests `Ping/Healthy`, `Ping/CancelledContext` and `Ping/AfterClose`.

- [ ] **Step 1: Write the cases** in `spitest/ping.go`:
  - `Ping/Healthy`: `h.Factory.Ping(ctx)` is nil.
  - `Ping/CancelledContext`: with a context already cancelled, `Ping` returns a non-nil error.
  - `Ping/AfterClose`: when `h.ClockFactory == nil`, `t.Skip("harness has no ClockFactory")`. Otherwise `f, closeFn := h.ClockFactory(fixedClock{at: time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)})`, then `f.Ping(ctx)` is nil, `closeFn()`, and `f.Ping(ctx)` is non-nil. Reuse the spitest fixed clock if one exists; otherwise add a three-line one in the file.
- [ ] **Step 2: Add `Ping`** to the interface. Run `go build ./... && go vet ./...` and expect PASS. There is no in-repo implementation; the cases run against the plugins in Task 2.
- [ ] **Step 3: Commit** `feat(spi)!: StoreFactory.Ping and its conformance cases`.
- [ ] **Step 4:** Push the branch and open the PR into SPI `main`, titled `feat(spi)!: StoreFactory.Ping`. Then file the cassandra issue in `cyoda/cyoda-go-cassandra`: "Implement StoreFactory.Ping: a ctx-bounded check on the session that returns an error after Close; the new spitest Ping cases fail until it lands". In cyoda-go, add the uncommitted `go.work` use line from Global Constraints.

### Task 2: `Ping` in the in-tree plugins, the caching factory and the root fakes

**Files:**
- Modify: `plugins/memory/store_factory.go` (a `closed atomic.Bool` that `Close` sets; `Ping`)
- Modify: `plugins/sqlite/store_factory.go` (`Ping` = `f.readDB.PingContext(ctx)`)
- Modify: `plugins/postgres/store_factory.go` (`Ping` = `f.pool.Ping(ctx)`; the scheduler pools are not checked)
- Modify: `internal/cluster/modelcache/factory.go:82` (forward `Ping` as `Close` is forwarded)
- Modify: every root fake the compiler names, including `app/app_close_idempotent_test.go:44`, `internal/cluster/modelcache/factory_test.go:40,190` and `internal/domain/model/service_test.go:136`
- Test: `internal/cluster/modelcache/factory_test.go`; the plugins' `conformance_test.go` already set `ClockFactory`, so the three spitest cases run there

**Interfaces:**
- Consumes: Task 1.
- Produces: `Ping` on every `spi.StoreFactory` in the tree.

- [ ] **Step 1: Write the failing test** `TestCachingStoreFactory_PingForwards`. The inner fake's `Ping` returns a sentinel error and records its ctx. `CachingStoreFactory.Ping(ctx)` returns that sentinel (`errors.Is`) and passes the same ctx.
- [ ] **Step 2:** `go build ./... ` fails (the interface is unimplemented). That failure is the RED for every implementation.
- [ ] **Step 3: Implement** the three plugins, the forwarder and the fakes. Memory returns `ctx.Err()` when ctx is done, and the error `memory: store factory is closed` after `Close`.
- [ ] **Step 4: Run** `go test ./internal/cluster/modelcache/...`, then in each of `plugins/memory`, `plugins/sqlite` and `plugins/postgres` run `go test -run 'Conformance/Ping' ./...`. Expect PASS, with `Ping/AfterClose` run and not skipped.
- [ ] **Step 5: Commit** `feat(storage): StoreFactory.Ping on every backend`.

### Task 3: The health latch type

**Files:**
- Create: `internal/health/latch.go`, `internal/health/latch_test.go`
- Modify (replace `*atomic.Bool` with `*health.Latch`, `Store(false)` with `Trip()`, `!Load()` with `Tripped()`): `app/app.go:94-104,128,787-795,810,850,867`; `internal/grpc/recovery.go:37,50,92-96`; `internal/grpc/server.go:91`; `internal/scheduler/service.go:40-41,1025-1041`; `internal/api/health.go:22`; `internal/api/middleware/recovery.go:36,65`; `internal/domain/search/service.go:210,335,1182`; `internal/domain/search/reaper.go:188`; and every test the compiler names
- Rename the `App` field `healthFlag` to `healthLatch` (`*health.Latch`); later tasks use that name.
- Modify: the comment at `app/app.go:94-103`. It lists the six sites: the search reaper sweep, the gRPC recovery interceptors, the scheduler, the HTTP recovery middleware, the async-search goroutine and the search reaper's second pass.

**Interfaces:**
- Produces, in package `health`:
  - `type Latch struct` (zero value not used; build with `NewLatch`)
  - `func NewLatch() *Latch`
  - `func (l *Latch) Trip()`. The first call marks it tripped and starts each `OnTrip` callback in its own goroutine. Later calls do nothing. On a nil `*Latch` it does nothing.
  - `func (l *Latch) Tripped() bool`, false on a nil `*Latch`
  - `func (l *Latch) OnTrip(f func())`. `f` runs once, in its own goroutine, when the latch trips, or at once in a goroutine when it has already tripped.

- [ ] **Step 1: Write the failing tests** in `latch_test.go`:
  - `TestLatch_TripIsOneWay`: `Tripped()` is false, then true after `Trip()`, and still true after a second `Trip()`.
  - `TestLatch_OnTripNotifiesOnceWithoutBlocking`: a callback that blocks on a channel does not block `Trip()`. It runs exactly once after two `Trip` calls.
  - `TestLatch_OnTripAfterTrip`: a callback registered after the trip still runs.
  - `TestLatch_NilIsSafe`: `var l *Latch; l.Trip(); l.Tripped() == false`.
- [ ] **Step 2:** `go test ./internal/health/` fails with undefined `NewLatch`.
- [ ] **Step 3: Implement** `latch.go`. Then replace the flag at the six sites, the readers (`GET /health`, `ReadinessCheck`, the scheduler's claim gate) and the tests. The nil checks at the sites go, because the latch is nil-safe.
- [ ] **Step 4: Run** `go build ./... && go vet ./...`, then `make test`. Expect PASS. Exit check: `grep -rn 'healthFlag \*atomic.Bool\|HealthFlag \*atomic.Bool\|healthFlag.Store(false)' --include='*.go' .` returns nothing.
- [ ] **Step 5: Commit** `refactor(health): a latch type that notifies when it trips replaces the health flag`.

### Task 4: The lifecycle holder and its transitions in the app

**Files:**
- Create: `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`
- Modify: `app/app.go` (an `a.lifecycle` field built right after the latch at `:128`; `MarkStarted()` as the last step of `New` before `return a`, `:775`; `BeginDraining()` as the first statement of `DrainScheduler` `:921` and of `Shutdown` `:1051`; `Close` gets it through its existing `DrainScheduler` call `:1004`; the accessor `func (a *App) Lifecycle() *lifecycle.Lifecycle`)
- Test: `app/app_lifecycle_test.go`

**Interfaces:**
- Consumes: Task 3 (`*health.Latch`).
- Produces, in package `lifecycle`:
  - `type State string` with `Starting = "STARTING"`, `Normal = "NORMAL"`, `Draining = "DRAINING"` and `Unhealthy = "UNHEALTHY"`
  - `func States() []State` returns `[Starting, Normal, Draining, Unhealthy]`. Counts, the metric and the size check use this order.
  - `func (s State) Valid() bool`
  - `func New(latch *health.Latch) *Lifecycle`. It registers `latch.OnTrip`.
  - `func (l *Lifecycle) State() State` is computed on each read from `latch.Tripped()`, draining and started, by precedence.
  - `func (l *Lifecycle) MarkStarted()`, `func (l *Lifecycle) BeginDraining()`, `func (l *Lifecycle) Draining() bool`
  - `func (l *Lifecycle) OnChange(f func(State))`. `f` is called once at registration with the current state, then once per change of the computed state. Calls are made under the lifecycle's lock, in order, so a later call never carries a state that ranks lower in `States()` order. `f` must not block or call back into the lifecycle.

- [ ] **Step 1: Write the failing tests** in `lifecycle_test.go`:
  - `TestLifecycle_Precedence`: a table over (tripped, draining, started) → state. For example (F,F,F)→STARTING, (F,F,T)→NORMAL, (F,T,F)→DRAINING, (F,T,T)→DRAINING, (T,*,*)→UNHEALTHY.
  - `TestLifecycle_NothingUndone`: `BeginDraining` then `MarkStarted` stays DRAINING. After a trip, every call stays UNHEALTHY.
  - `TestLifecycle_OnChangeDeliversCurrentThenChanges`: register → STARTING; `MarkStarted` → NORMAL; `MarkStarted` again → no call; `BeginDraining` → DRAINING; trip → UNHEALTHY, polled with a 2 s deadline.
  - `TestLifecycle_ConcurrentTripAndDrain_MonotoneDelivery`: 100 iterations. Each starts with a fresh lifecycle, runs `BeginDraining` and `latch.Trip()` in two goroutines at once, and records every delivered state. The rank sequence never decreases, and the last state is UNHEALTHY.
- [ ] **Step 2:** `go test ./internal/lifecycle/` → FAIL (undefined).
- [ ] **Step 3: Implement** `lifecycle.go`.
- [ ] **Step 4: Write the failing app tests** in `app/app_lifecycle_test.go`. Each builds `New(DefaultConfig())` with `ContextPath = ""`:
  - `TestNew_ReturnsNormal`: `a.Lifecycle().State() == lifecycle.Normal`, with no other call.
  - `TestDrainScheduler_SetsDraining`, `TestShutdown_Alone_SetsDraining`, `TestClose_Alone_SetsDraining`: each calls only that method and then sees `Draining`.
  - `TestLatchTrip_SetsUnhealthy`: a panic through `middleware.Recovery(a.healthLatch)` makes the state UNHEALTHY.
- [ ] **Step 5:** Wire the app. Run `go test ./internal/lifecycle/ ./app/ -run 'Lifecycle|ReturnsNormal|SetsDraining|SetsUnhealthy'` → PASS.
- [ ] **Step 6: Commit** `feat(lifecycle): node lifecycle states set by the app on start, drain and latch`.

### Task 5: `/readyz` and gRPC readiness fail while draining

**Files:**
- Modify: `app/app.go:878-915` (`ReadinessCheck`, and its doc comment, which gains the third reason)
- Test: `app/app_readiness_test.go`
- Docs: `cmd/cyoda/help/content/run.md:288-299` (HEALTH PROBES). `/readyz` answers 503 from the start of shutdown. `GET /health` stays on the latch alone, so the two differ while the node drains. Also `cmd/cyoda/help/content/cluster.md:67-72` (OPERATIONS: shrinking and rolling restart now see `/readyz` fail at once on SIGTERM), `cmd/cyoda/help/content/cli/health.md` where it describes when `/readyz` fails, and `docs/ARCHITECTURE.md` `:398`, `:406` and `:2031-2039` (three reasons, not two).

**Interfaces:**
- Consumes: Task 4 (`Lifecycle.Draining()`).
- Produces: `ReadinessCheck()` returns `errors.New("node draining: shutdown has begun")` while draining. The latch reason wins when both hold.

- [ ] **Step 1: Write the failing tests** `TestReadyz_FailsFromDrainSchedulerStart` and `TestHealth_StaysUpWhileDraining`. The first calls `a.DrainScheduler(ctx)`, then `probeReadyz(t, a).Code == 503`, and `a.ReadinessCheck()` contains "draining". The second sends `GET /health` through `a.Handler()` after `DrainScheduler` and expects 200 `{"status":"UP"}`.
- [ ] **Step 2:** Run `go test ./app/ -run 'Readyz_FailsFromDrain|Health_StaysUp'`. The first FAILs (200) and the second PASSes; it pins that `/health` does not change.
- [ ] **Step 3: Implement**, and update the docs listed above.
- [ ] **Step 4:** The same run → PASS. Then run `make test`.
- [ ] **Step 5: Commit** `feat(readiness)!: /readyz fails from the start of shutdown`.

The spec waives the e2e cell and cites the SIGTERM test (`cmd/cyoda/main_sigterm_test.go:19`). That test is left unchanged: an idle scheduler drains in microseconds, so the 503 window cannot be observed from outside the process without flaking. The app-level test drives the real admin handler that `cmd/cyoda` mounts, which is the waived cell's substance.

### Task 6: Gossip metadata carries the state

**Files:**
- Modify: `internal/cluster/registry/gossip.go`. `nodeMeta` gains `State lifecycle.State \`json:"st"\``. Create a `metaBuilder` (it may live in a new `internal/cluster/registry/meta.go`). `marshalMeta(cfg, version, state)`. `checkIdentityFits` measures with `lifecycle.Unhealthy`. `NewGossip` builds its first metadata with `lifecycle.Starting`. `UpdateTags` goes through the builder. Add `(*Gossip).SetState`.
- Modify: `internal/cluster/registry/tags_worker.go:255-275` (`runReadvertiser`: on `<-g.stop`, run a pending poke before it returns)
- Modify: `app/app.go` (right after `mustNewGossip`, `a.lifecycle.OnChange(func(s lifecycle.State) { ... gossipReg.SetState(s) ... })`, logging an error at ERROR)
- Test: `internal/cluster/registry/meta_internal_test.go`, `internal/cluster/registry/gossip_state_test.go` (extend)
- Docs: `cmd/cyoda/help/content/config/cluster.md:20`. Re-measure the identity budget: the framing figure now includes `,"st":"UNHEALTHY"`. Print the new number from the test in Step 1 and write it in. Also `docs/ARCHITECTURE.md:2660`: the metadata holds identity, a list version and the state.

**Interfaces:**
- Consumes: Task 4 (`lifecycle.State`, `States()`).
- Produces:
  - `func (g *Gossip) SetState(s lifecycle.State) error`. It stores new metadata through the builder and pokes `g.readvertise` without blocking. An equal state changes nothing and pokes nothing.
  - `func memberState(meta []byte) (lifecycle.State, bool)` (unexported). It returns false when the metadata does not parse or its state is not `Valid()`. Tasks 7 and 10 use it.
  - `metaBuilder` holds the current `listVersion` and `lifecycle.State` under one mutex and marshals both. Its `setVersion` runs inside `tags.setOwn`'s announce callback, under the tag lock: the lock order is tag store, then builder, and the builder never calls the tag store.

- [ ] **Step 1: Write the failing tests:**
  - `TestCheckIdentityFits_MeasuresLongestState`: an identity that fits with `"st":"NORMAL"` but not with `"st":"UNHEALTHY"` is refused. Build it by padding `NodeID` to the boundary computed from `marshalMeta`. Also assert that `UNHEALTHY` is the longest of `lifecycle.States()`.
  - `TestNewGossip_MetadataStartsStarting`: `parseMeta(g.delegate.NodeMeta(0))` has state STARTING.
  - `TestSetState_PairsWithCurrentTagVersion`: `UpdateTags(A)`, then `SetState(Normal)`, then `UpdateTags(B)`. Each stored metadata carries the newest version and the newest state.
  - `TestSetState_ConcurrentWithUpdateTags`: 200 goroutines alternate `SetState` and `UpdateTags` under `-race`. The final metadata names the final version and the final state.
  - `TestSetState_ReadvertisedToPeer`: two real gossip nodes. B calls `SetState(Draining)`, and A's directory shows B's state DRAINING within 5 s, though `Changed()` did not fire for it.
  - `TestDeregister_RunsPendingReadvertise`: two real nodes. A goroutine samples `memberState` of B in A's directory every 1 ms. B calls `SetState(Draining)` and then `Deregister` at once. The sampler saw DRAINING before B left. Run it `-count=20` on the code before the `runReadvertiser` change, and record that it goes RED. If it never goes RED, drop it and record the review-only waiver in the commit.
  - `TestMemberState`: valid metadata gives (state, true). Metadata with no `st`, an unknown `st` or bad JSON gives ("", false).
- [ ] **Step 2:** `go test ./internal/cluster/registry/ -run 'IdentityFits_MeasuresLongestState|MetadataStartsStarting|SetState|PendingReadvertise|MemberState'` → FAIL.
- [ ] **Step 3: Implement** the builder, `SetState`, `memberState` and the re-advertiser drain. Then wire the app.
- [ ] **Step 4:** The same run with `-race` → PASS. Then `make test`.
- [ ] **Step 5: Commit** `feat(cluster): gossip metadata publishes the node's lifecycle state`.

### Task 7: The member-count metric

**Files:**
- Modify: `internal/cluster/registry/metrics.go` (`newTagMetrics` gains `memberStates func() map[lifecycle.State]int64`; a second observable gauge in the same callback registration, so `close()` unregisters both)
- Modify: `internal/cluster/registry/gossip.go` (`func (g *Gossip) memberStateCounts() map[lifecycle.State]int64` over `g.dir.all()`, self included, through `memberState`; unparseable members are not counted)
- Test: `internal/cluster/registry/metrics_internal_test.go`
- Docs: `cmd/cyoda/help/content/telemetry.md`. Add the metric beside the membership metrics (`:123-124`) and its attribute to ATTRIBUTE VOCABULARY (`:147`). Add the attribute key to `internal/observability/attrs.go` if the vocabulary is mirrored there.

**Interfaces:**
- Consumes: Task 6 (`memberState`).
- Produces: the gauge `cyoda.cluster.member.count`. Every collection observes all four states, observing 0 for a state no member holds, so that a state left behind drops to 0 rather than keeping its last value.

- [ ] **Step 1: Write the failing test** `TestMemberCountMetric`. Use an SDK `ManualReader` meter with a fake directory: self NORMAL, one peer DRAINING, one peer with unparseable metadata. The collected points are `{starting:0, normal:1, draining:1, unhealthy:0}` with unit `{member}`. After `close()`, a collect has no `cyoda.cluster.member.count` point.
- [ ] **Step 2:** Run it → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** Run it → PASS.
- [ ] **Step 5: Commit** `feat(cluster): cyoda.cluster.member.count per lifecycle state`.

### Task 8: `grpc.health.v1` `Check` without a token

**Files:**
- Create: `internal/grpc/health.go` (`healthServer`, which embeds `grpc_health_v1.UnimplementedHealthServer`)
- Modify: `internal/grpc/server.go` (`NewServer` gains `readiness func() error` after `clockState` and panics on nil; `grpc_health_v1.RegisterHealthServer` beside `cyodapb.RegisterCloudEventsServiceServer` `:155`)
- Modify: `internal/grpc/interceptor.go:22-37` and `internal/grpc/role_interceptor.go:15-37`. The unary auth interceptor and `UnaryRequireM2M` pass `info.FullMethod == grpc_health_v1.Health_Check_FullMethodName` through without checking it. Correct the `:15-16` comment.
- Modify: `app/app.go` (pass `a.ReadinessCheck` to `NewServer`); the four tests that call `NewServer`
- Test: `internal/grpc/health_test.go`
- Docs: `cmd/cyoda/help/content/grpc.md`. In **Authentication**, `/grpc.health.v1.Health/Check` is the one method without a token. Under SERVICES (`:53`), add the health service: `Check` for `""` only, `SERVING` while ready, healthy and not draining; `Watch` and `List` are not implemented. Also add one line to `run.md` HEALTH PROBES.

**Interfaces:**
- Consumes: Task 5 (`ReadinessCheck`).
- Produces: `Check(ctx, &HealthCheckRequest{Service: ""})` answers `SERVING` when `readiness()` is nil and `NOT_SERVING` otherwise. Any other service name gets `status.Error(codes.NotFound, "unknown service")`.

- [ ] **Step 1: Write the failing tests** in `health_test.go`. They run against a real `NewServer` on a loopback listener. Its auth fake accepts `Bearer m2m` (roles `ROLE_M2M`) and `Bearer plain` (no roles) and refuses anything else, including no header. `readiness` is a func the test sets.
  - `TestHealthCheck_NoToken_Serving`
  - `TestHealthCheck_NotReady_NotServing`: readiness returns the latch error, then the draining error. Both give `NOT_SERVING`.
  - `TestHealthCheck_OtherService_NotFound`
  - `TestHealthWatch_NoToken_Unauthenticated`, `TestHealthList_PlainToken_PermissionDenied`, `TestHealthList_M2M_Unimplemented`, `TestHealthWatch_M2M_Unimplemented`
  - `TestHealthCheck_Panic_InternalAndLatched`: readiness panics. The call answers `codes.Internal`, the latch is tripped, and the server still answers the next call.
  - `TestHealthExemption_ExactMethodOnly`: a unary call through `UnaryAuthInterceptor` with `FullMethod` `/grpc.health.v1.Health/CheckX` and no token is `Unauthenticated`.
- [ ] **Step 2:** `go test ./internal/grpc/ -run 'Health'` → FAIL.
- [ ] **Step 3: Implement**, then update the docs.
- [ ] **Step 4:** The same run → PASS. Then `make test`.
- [ ] **Step 5: Commit** `feat(grpc): grpc.health.v1 Check without a token`.

---

## Phase B — after the feat-611 confirmation lands

### Task 9: Rebase onto feat-611 and record its interfaces

- [ ] **Step 1:** Confirm that the 611 confirmation plan's last task is committed on `feat-611-platform-clock`: `git -C ../feat-611-platform-clock log --oneline -20` shows its final commits. If it has not landed, stop here and report.
- [ ] **Step 2:** In this worktree, run `git rebase feat-611-platform-clock`. Resolve the conflicts. Expect them in `app/app.go`, `internal/cluster/registry/gossip.go` and `internal/cluster/dispatch/{handler,forwarder}.go`. Keep both sides' behaviour.
- [ ] **Step 3:** `go build ./... && go vet ./...`, then `make test` → PASS.
- [ ] **Step 4:** Fill in the "feat-611 interfaces" table above from the landed code, citing `file:line`. Commit the plan alone: `docs(plan): record the feat-611 interfaces the cluster status read uses`.

### Task 10: The directory listing carries each member's state

**Files:**
- Modify: the registry method in the table's first row, its result type and `registry.Local`'s counterpart if 611 added one
- Test: beside that method's existing tests

**Interfaces:**
- Consumes: Task 6 (`memberState`), Task 9 (the listing method).
- Produces: each listed member carries `State lifecycle.State`. It is empty when the metadata does not parse or names no valid state. The member's id, its address (empty when unusable) and whether its metadata parsed stay as 611 defined them. In single-node mode the one member is `local` and takes its state from the process.

- [ ] **Step 1: Write the failing test** `TestDirectoryMembers_CarryState`: self NORMAL, a peer DRAINING, a peer with unparseable metadata. The states are NORMAL, DRAINING and empty.
- [ ] **Step 2:** Run it → FAIL. **Step 3:** Implement. **Step 4:** Run it → PASS.
- [ ] **Step 5: Commit** `feat(cluster): the directory listing names each member's state`.

### Task 11: A node's own status, and the answer's validation

**Files:**
- Create: `internal/clusterstatus/node.go`, `internal/clusterstatus/node_test.go`

**Interfaces:**
- Consumes: Task 2 (`Ping`), Task 4 (`lifecycle`).
- Produces, in package `clusterstatus`:
  - `type Clock struct { Controllable bool \`json:"controllable"\`; Version uint64 \`json:"version"\`; Writer string \`json:"writer"\` }`
  - `type NodeStatus struct { State lifecycle.State \`json:"state"\`; Database bool \`json:"database"\`; Version string \`json:"version"\`; Clock Clock \`json:"clock"\` }`. This is also the wire encoding of the peer answer.
  - `func (s NodeStatus) Validate() error`, which applies the answer bounds in Global Constraints
  - `type Self struct` and `func NewSelf(lc *lifecycle.Lifecycle, store interface{ Ping(context.Context) error }, version string, clock func() Clock, askTimeout time.Duration) *Self`
  - `func (s *Self) Status(ctx context.Context) NodeStatus`. It calls `Ping` under `context.WithTimeout(ctx, askTimeout/2)`. A failure is logged at WARN with the error and answered as `Database: false`.

- [ ] **Step 1: Write the failing tests:**
  - `TestSelf_Status`: lifecycle NORMAL, Ping nil, version `"v1"`, clock `{true, 7, "node-a"}` give exactly that `NodeStatus` with `Database: true`.
  - `TestSelf_PingBoundIsHalfAskTimeout`: the fake `Ping` records `ctx.Deadline()`. With `askTimeout` 10 s, the deadline is within 100 ms of now + 5 s.
  - `TestSelf_PingFailure_DatabaseFalse_NoTextInStatus`: Ping returns `errors.New("dial tcp 10.0.0.9:5432: secret-dsn")`. `Database` is false, the marshalled `NodeStatus` does not contain `10.0.0.9`, and the WARN log record carries the error.
  - `TestNodeStatus_Validate`: a table. Valid: each of the four states, a 256-byte version. Invalid: state `"normal"`, state `""`, a 257-byte version, a 257-byte writer, a version that is not valid UTF-8, clock version `math.MaxInt64+1`.
- [ ] **Step 2:** `go test ./internal/clusterstatus/` → FAIL. **Step 3:** Implement. **Step 4:** → PASS.
- [ ] **Step 5: Commit** `feat(clusterstatus): a node's own status and the bounds on a peer's answer`.

### Task 12: The status peer route and the forwarder's ask

**Files:**
- Modify: `internal/cluster/dispatch/handler.go`. Register `POST /internal/dispatch/status` where 611 registers the clock route. The handler: `h.auth.Verify(r)`; on an error, a bare 403 as `handleCallout` gives (`:75-78`); otherwise seal `src.Status(r.Context())` for the request's binding. The replay-cache refusal is answered as any auth failure, with a bare 403, because no status is owed to a refused envelope.
- Modify: `internal/cluster/dispatch/forwarder.go` (`AskStatus`)
- Test: `internal/cluster/dispatch/status_test.go`

**Interfaces:**
- Consumes: Task 9 (route registration and forwarder), Task 11 (`NodeStatus`, `*Self`).
- Produces:
  - `type StatusSource interface { Status(ctx context.Context) clusterstatus.NodeStatus }`. `*clusterstatus.Self` satisfies it, and the dispatch handler takes one, the way 611 passes the clock receiver.
  - `func (f *HTTPForwarder) AskStatus(ctx context.Context, peerNodeID, addr string) (clusterstatus.NodeStatus, error)`. It runs `validatePeerAddress` (bounded by the connect timeout, as `ForwardCallout` does), then `f.forward(ctx, peerNodeID, ensureScheme(addr)+"/internal/dispatch/status", struct{}{}, &out)`, then `out.Validate()`. A validation failure is an error.

- [ ] **Step 1: Write the failing tests.** Two `AEADPeerAuth` values on the shared test secret, one per node id, with the handler on an `httptest.Server` and the forwarder using `AllowLoopbackForTesting()`:
  - `TestAskStatus_RoundTrip`: the source's `NodeStatus` comes back equal.
  - `TestStatusRoute_RefusesEnvelopeForAnotherPath`: an envelope sealed for `/internal/dispatch/callout` and posted to the status path gets 403, and the source is not called.
  - `TestStatusRoute_RefusesEnvelopeForAnotherNode`: an envelope sealed for `node-x` and sent to `node-b` gets 403.
  - `TestAskStatus_InvalidAnswerIsError`: the source answers state `"bogus"`, and `AskStatus` returns an error.
  - `TestAskStatus_LoopbackRefusedWithoutOptIn`: without `AllowLoopbackForTesting` the ask fails before connecting.
- [ ] **Step 2:** `go test ./internal/cluster/dispatch/ -run 'Status'` → FAIL. **Step 3:** Implement. **Step 4:** → PASS.
- [ ] **Step 5: Commit** `feat(dispatch): POST /internal/dispatch/status answers a node's own status, sealed`.

### Task 13: The status service: rounds, entries, `operational`, the wait

**Files:**
- Create: `internal/clusterstatus/service.go`, `internal/clusterstatus/service_test.go`

**Interfaces:**
- Consumes: Task 10 (the listing), Task 11, Task 12 (`AskStatus`).
- Produces:
  - `type Member struct { ID, Addr string; State lifecycle.State }`. An empty `Addr` means there is no usable address. The app adapts the Task 10 listing to it.
  - `type Directory interface { Members(ctx context.Context) ([]Member, error); Changed() <-chan struct{} }`
  - `type Asker interface { AskStatus(ctx context.Context, nodeID, addr string) (NodeStatus, error) }`
  - `func InProcess(self *Self) Asker` is the single-node asker, which calls `self.Status` and ignores the address. `func LocalDirectory(lc *lifecycle.Lifecycle) Directory` gives one member, `local`, whose `Changed()` never fires.
  - `type Reachability string` (`"UP"`, `"DOWN"`). `type Entry struct { ID string; Reachability Reachability; State lifecycle.State; Status *NodeStatus }`, where `Status` is nil when DOWN. `type Count struct { Reachability Reachability; State lifecycle.State; Count int }`. `type Report struct { Operational bool; Counts []Count; Nodes []Entry }`.
  - `func NewService(dir Directory, ask Asker, askTimeout time.Duration) *Service`
  - `func (s *Service) Read(ctx context.Context, waitFor int, timeout time.Duration) (Report, error)`. `waitFor == 0` means one round. The only error it returns is a context error wrapped with `common.ClientGone`.

Rules the code must keep (from the spec, "How it works" and "Response"):
- A round lists the members. In parallel, it asks each one that has an address, under `context.WithTimeout(ctx, askTimeout)`. A member with no address, an ask error or a `Validate` failure is `DOWN`, with `State` from the listing. Each ask error is logged at DEBUG with the member id, and the address and error text go to the log only. Entries are sorted by `ID`.
- `Counts`: one item per (reachability, state) present. They are ordered UP before DOWN, then by `lifecycle.States()` order, with the stateless item last.
- `Operational` is true only when there is at least one entry, every entry is `UP` with state `NORMAL` and `Database` true, all `Version`s are equal, all `Clock`s are equal, and, when `waitFor > 0`, at least `waitFor` entries are `UP`/`NORMAL`.
- The wait loop: take `dir.Changed()` before each round. Answer when the round has at least `waitFor` UP/NORMAL entries. Otherwise wait for `Changed()`, a 1 s timer, or the deadline. At the deadline, answer with the last completed round, cancelling any round still running. The first round always completes, whatever the deadline. A done `ctx` returns `common.ClientGone(ctx.Err())`. Deadline and timer are real time, marked `// realtime:`.

- [ ] **Step 1: Write the failing tests.** Use fake `Directory` and `Asker`; the fake `Changed()` channel is controlled by the test.
  - `TestRound_AllUpNormal_Operational`: two members answer NORMAL, the same version and the same clock, giving Operational true and `Counts == [{UP, NORMAL, 2}]`.
  - `TestRound_DownKeepsMetadataState`: one member's ask fails, giving that entry `DOWN` with state STARTING from the listing, `Status == nil` and Operational false. A second case lists the member as NORMAL with a failing ask (built, listeners not yet open): it is `DOWN`/`NORMAL`, and `Read(ctx, 2, 300ms)` over it and one UP/NORMAL member keeps waiting and answers Operational false at the timeout.
  - `TestRound_UnusableAddressIsDownWithoutAsk`: no `Addr` gives DOWN, and the asker is not called for it.
  - `TestRound_StatelessDownMember`: listing state `""` gives DOWN with no state. `Counts` has a stateless item, last.
  - `TestRound_InvalidAnswerIsDown`: the asker returns the validation error, giving DOWN.
  - `TestRound_HangingPeerBoundedByAskTimeout`: with askTimeout 200 ms, a peer blocks until its ctx is done. The round returns within 1 s, the hanging peer is DOWN and the others are UP.
  - `TestOperational_Table`: draining (UP/DRAINING), unhealthy, `Database` false, version mismatch and clock mismatch (`controllable` differs; `version` differs; `writer` differs) are each false. No entries is false.
  - `TestRead_WaitAnswersWhenSecondNodeJoins`: the first round has one member. The test adds a second member and closes `Changed()`. `Read(ctx, 2, 10s)` returns Operational true within 2 s.
  - `TestRead_WaitMetWithExtraDown`: two UP/NORMAL and one DOWN return at once with Operational false.
  - `TestRead_TimeoutAnswersLastCompletedRound`: `waitFor` 3 against two members with timeout 300 ms returns after about 300 ms (no earlier than 300 ms, before 1.5 s) with the two-member round and Operational false.
  - `TestRead_FirstRoundOutlastsTimeout`: timeout 1 ms, with the asker sleeping 200 ms. The report holds the completed first round, all UP.
  - `TestRead_ReasksOnIntervalWithoutChanged`: a member is DRAINING in round 1 and NORMAL from round 2. `Changed()` never fires, and `Read(ctx, 1, 5s)` returns Operational true after about 1 s.
  - `TestRead_ClientGone`: cancel ctx during the wait. The error satisfies `errors.Is(err, common.ErrClientGone)`.
  - `TestInProcess_SingleNode`: `LocalDirectory` plus `InProcess(self)` give one entry `local`, UP/NORMAL, Operational true. `Read(ctx, 2, 300ms)` returns Operational false at about 300 ms.
- [ ] **Step 2:** `go test ./internal/clusterstatus/ -run 'Round|Operational|Read|InProcess'` → FAIL. **Step 3:** Implement. **Step 4:** Run with `-race` → PASS.
- [ ] **Step 5: Commit** `feat(clusterstatus): the cluster status read, its rounds and its wait`.

### Task 14: `GET /platform/cluster`: OpenAPI, handler, wiring, single-node e2e

**Files:**
- Modify: `api/openapi.yaml`. Add the path beside `/platform/clock` (`:1014`), tag `Platform Cluster`, operationId `getPlatformCluster`, and the description "**Authorization Required:** platform operator — `ROLE_ADMIN` in the tenant `PLATFORM`." Add both query parameters (`type: integer`, `format: int32`, with `minimum`/`maximum`; `timeoutMs` `default: 30000`). The responses are `200` and `503`, each its own response with `application/json` `ClusterStatusDto`; `400` with ProblemDetail, whose description lists every refusal from the spec's table; `401` `Unauthorized`; `403` `Forbidden`; and `default` `InternalServerError`. Add the schemas `ClusterStatusDto`, `ClusterStateCountDto`, `ClusterNodeStatusDto` and `ClusterNodeClockDto` with the members, enums and required lists in the spec's tables. `clock.version` is `int64`. Then run `go generate ./api`.
- Create: `internal/clusterstatus/handler.go`, `internal/clusterstatus/handler_test.go`
- Modify: `internal/api/server.go` (`PlatformCluster` field; `GetPlatformCluster` delegate like `GetPlatformClock` `:219-225`), `internal/api/unimplemented.go`
- Modify: `app/app.go`. In cluster mode, build `NewService` over the listing adapter and the 611 forwarder, with `cfg.Cluster.ProxyTimeout`. In single-node mode, use `LocalDirectory` and `InProcess`. `Self` takes `a.storeFactory`, `cfg.Version`, the clock func (with clock control on: `{true, s.Version, s.Writer}` from the test clock's setting; off: `{}`) and `cfg.Cluster.ProxyTimeout`. Pass `Self` to the dispatch handler's status route.
- Create: `internal/e2e/platform_cluster_test.go`
- Modify: `internal/e2e/platform_operator_test.go:16-30` (add `{"get cluster", http.MethodGet, "/platform/cluster", ""}` to `operatorEndpoints`)
- Docs: `cmd/cyoda/help/content/cluster.md`, a new section `CLUSTER STATUS`. It covers the path, auth, both parameters, the body, the `operational` rule, 200/503, the four states and their transitions, and that a node is listed DOWN/NORMAL between `App.New` and its listeners opening. The section names no issue ids. Also `config/cluster.md:27`: `CYODA_PROXY_TIMEOUT` bounds each status ask, and half of it bounds the database check.

**Interfaces:**
- Consumes: Task 13.
- Produces: `func NewHandler(svc *Service, guard auth.OperatorGuard) *Handler` and `func (h *Handler) GetPlatformCluster(w http.ResponseWriter, r *http.Request, params genapi.GetPlatformClusterParams)`.

Handler order: guard; then the parameter checks (`waitForNodes` outside 1–1000; `timeoutMs` outside 1–600000; `timeoutMs` without `waitForNodes`), each answered as `common.Operational(400, common.ErrCodeBadRequest, …)` with `Props = {"parameter": name}`; then `svc.Read`; then 200 or 503 with the body built from the generated DTO types. An error from `Read` goes to `common.WriteError(w, r, common.Internal("failed to read the cluster status", err))`.

- [ ] **Step 1: Write the failing handler tests** (`handler_test.go`, with the service over fakes):
  - `TestHandler_RangeRefusals`: a table over `waitForNodes` 0 and 1001, `timeoutMs` 0 and 600001, and `timeoutMs` alone. Each gives 400 `BAD_REQUEST` with `properties.parameter` naming the parameter, and the service is not called.
  - `TestHandler_WaitDefaultsTimeout30s`: `waitForNodes=2` alone reaches `Read` with timeout 30 s. The fake service records it.
  - `TestHandler_503SameBody`: a non-operational report gives 503 `application/json` (not `problem+json`), and the body decodes as `ClusterStatusDto`.
  - `TestHandler_ReadError_500Ticket`: `Read` returns a plain error, giving 500 with a ticket.
  - `TestHandler_ClientGone_NoTicket`: `Read` returns a ClientGone error, and nothing is logged above DEBUG.
- [ ] **Step 2:** `go test ./internal/clusterstatus/ -run Handler` → FAIL. **Step 3:** Implement and wire. **Step 4:** → PASS. Run `go test ./api/... ./internal/api/...` for the spec-rule tests → PASS.
- [ ] **Step 5: Write the e2e tests** in `platform_cluster_test.go`. They run against the shared single-node server with `operatorRequest`:
  - `TestPlatformCluster_SingleNode_200`: 200, `operational` true, `nodes == [{id:"local", reachability:"UP", state:"NORMAL", database:true, version:<non-empty>, clock:{controllable:false, version:0, writer:""}}]`, `counts == [{UP, NORMAL, 1}]`.
  - `TestPlatformCluster_SingleNode_WaitForTwo_503AtTimeout`: `?waitForNodes=2&timeoutMs=500` gives 503 after ≥ 500 ms with one `local` entry.
  - `TestPlatformCluster_400`: a table over `foo=1` (undeclared), `waitForNodes=abc`, `waitForNodes=2.5`, `waitForNodes=0`, `waitForNodes=1001`, `waitForNodes=99999999999`, `waitForNodes=2&waitForNodes=3`, `timeoutMs=0&waitForNodes=1`, `timeoutMs=600001&waitForNodes=1`, and `timeoutMs=100`. Each is 400 `BAD_REQUEST`, and `properties.parameter` is the parameter name.
  - The 401 and 403 cells (no token, tenant admin, on-behalf-of, PLATFORM without ROLE_ADMIN) come from the `operatorEndpoints` row added above.
- [ ] **Step 6:** `go test -timeout 30m ./internal/e2e/ -run 'PlatformCluster|PlatformOperator'` → PASS. Then `make test`.
- [ ] **Step 7: Commit** `feat(api): GET /platform/cluster lists the cluster's nodes and whether it is operational`.

### Task 15: Cluster e2e

**Files:**
- Modify: `internal/e2e/platform_cluster_test.go`

The fixture: the table's e2e cluster fixture if one landed. Otherwise, `statusClusterNode(t, s *schedDB, key, nodeID, gossipAddr string, seeds []string, configure func(*app.Config)) *callbackHarness`, modelled on `clockClusterNode` (`platform_clock_test.go:700-717`) without clock control. The first node binds gossip on `127.0.0.1:<freeLoopbackPort>`, and the others seed through it, so no outside member is in the directory. A bare gossip member with `NodeAddr` `http://127.0.0.1:1` (as `newClockSeed` builds) stands for a member whose address serves nothing. Every read goes to node A with `a.platformToken(t)`.

- [ ] **Step 1: Write the tests:**
  - `TestPlatformClusterMN_TwoNodes_200`: two entries, both UP/NORMAL, `operational` true. A's own entry is UP, which proves its peer route serves.
  - `TestPlatformClusterMN_DeadAddress_503`: a bare member joins. 503, that member DOWN with state `STARTING`, and the raw body does not contain `127.0.0.1` or `http://`.
  - `TestPlatformClusterMN_LeftNodeAbsent`: B runs `Shutdown()` and `Close()`. A's read is polled (5 s) until it lists only A, with 200.
  - `TestPlatformClusterMN_Draining_503`: `b.app.DrainScheduler(ctx)` gives 503 with B UP/DRAINING.
  - `TestPlatformClusterMN_VersionMismatch_503`: B has `cfg.Version = "other"`, giving 503 with both UP/NORMAL.
  - `TestPlatformClusterMN_ClockMismatch_503`: B has `cfg.ClockControlEnabled = true`, giving 503 with B's `clock.controllable` true and A's false.
  - `TestPlatformClusterMN_WaitForTwo_200OnJoin`: start A, then begin `?waitForNodes=2&timeoutMs=60000` in a goroutine, then start B. The answer is 200 with two entries.
  - `TestPlatformClusterMN_WaitMetWithExtraDown_503`: A, B and a bare member with `?waitForNodes=2` give 503 at once (well under the timeout), with two UP/NORMAL and one DOWN.
  - `TestPlatformClusterMN_WaitNotMet_503LastRound`: A alone with `?waitForNodes=2&timeoutMs=1000` gives 503 after ≥ 1 s with A's entry UP.
- [ ] **Step 2:** `go test -timeout 30m ./internal/e2e/ -run 'PlatformClusterMN'` → PASS. These are new behaviour on code that Tasks 12–14 already drove RED in unit tests. A test that passes on its first run is checked once against a deliberately broken service (for example `Operational` forced true), to show it can fail; that check is not committed.
- [ ] **Step 3: Commit** `test(e2e): cluster status across nodes`.

### Task 16: Parity scenario

**Files:**
- Create: `e2e/parity/platform_cluster.go`
- Modify: `e2e/parity/registry.go` (`{"PlatformClusterStatus", RunPlatformClusterStatus}` beside the platform clock rows `:482-485`; header count +1), `e2e/parity/registry_count_test.go` (`wantParityScenarioCount` +1 from its value at that point)
- Modify: `e2e/parity/client` if it has no generic GET helper for an operator path

**Interfaces:**
- Produces: `func RunPlatformClusterStatus(t *testing.T, fixture BackendFixture)`.

- [ ] **Step 1: Write the scenario.** With `fixture.PlatformOperator(t)`, `GET /platform/cluster` gives 200 and `operational` true. Every node is UP/NORMAL with `database` true and the same `version`, and the counts sum to `len(nodes)`. It does not assert the id `local`, because a fixture may run in cluster mode. With a tenant admin token it gives 403 `FORBIDDEN`.
- [ ] **Step 2:** `make test` (the parity suites run uncached) → PASS on memory, sqlite and postgres.
- [ ] **Step 3: Commit** `test(parity): the cluster status read on every backend`.

### Task 17: CHANGELOG, cloud parity, ARCHITECTURE audit

**Files:**
- Modify: `CHANGELOG.md` `[Unreleased]`. Under `### Breaking`: `StoreFactory.Ping` for plugin authors (see `COMPATIBILITY.md`), and `/readyz` failing from the start of shutdown. Under `### Added`: `GET /platform/cluster`, lifecycle states in gossip metadata, `grpc.health.v1` `Check` without a token, and `cyoda.cluster.member.count`.
- Create: `docs/cloud-parity/cluster-status.md`. It covers the read's contract (route, auth, parameters and ranges, body, `operational`, 200/503 and 400/401/403), the four states and their precedence, gRPC health (`Check` only, `""` only, no token), and readiness on drain.
- Modify: `docs/cloud-parity/README.md` (a row for it)
- Modify: `docs/ARCHITECTURE.md`. Audit the sections on health, readiness, gossip metadata, peer routes and gRPC services, and write each as it now is, in the present tense. Add the status peer route beside the hand-over and clock routes, the latch type, and the lifecycle.

- [ ] **Step 1:** Write the entries. Exit check: `grep -n '#[0-9]\{3\}' docs/cloud-parity/cluster-status.md cmd/cyoda/help/content/cluster.md cmd/cyoda/help/content/grpc.md` returns nothing.
- [ ] **Step 2:** `go test ./cmd/cyoda/...` (the help-tree tests) → PASS.
- [ ] **Step 3: Commit** `docs: cluster status, lifecycle states, gRPC health and readiness on drain`.

### Task 18: SPI pin and compatibility (blocked on the SPI PR merging)

Until the SPI PR from Task 1 is merged, the earlier tasks build against the uncommitted `go.work` line. This task runs once it has merged and before the cyoda-go PR is opened.

**Files:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`, and each `go.sum`
- Modify: `COMPATIBILITY.md` (the new SPI pseudo-version; out-of-tree plugins must implement `StoreFactory.Ping`; the cassandra issue from Task 1)

- [ ] **Step 1:** Remove the `go.work` SPI line. Pin the merged SPI `main` commit in all four `go.mod` files (`go get github.com/cyoda-platform/cyoda-go-spi@<merged-sha>` in each module, then `go mod tidy`). Run `make check-spi-pin-sync` → PASS.
- [ ] **Step 2:** Push the branch, then run `make repin-plugins` (it pins the plugin submodules to the pushed HEAD).
- [ ] **Step 3:** Update `COMPATIBILITY.md`. Run `make test` → PASS.
- [ ] **Step 4: Commit** `chore(deps): pin cyoda-go-spi with StoreFactory.Ping` (one commit with the four `go.mod` files and `COMPATIBILITY.md`).
- [ ] **Step 5:** File the CaaS ticket in Jira project CP, titled `[CaaS] Cluster status read, lifecycle states and gRPC health`, component `CaaS`, linking `docs/cloud-parity/cluster-status.md`.

### Task 19: Verification

- [ ] **Step 1:** `make preflight`, then `go vet ./...`, then `make test`, with the full log written to a file. All PASS.
- [ ] **Step 2:** Exit checks: `grep -rn 'atomic.Bool' --include='*.go' app internal | grep -i health` is empty. `grep -rn 'Health_Check_FullMethodName' --include='*.go' internal/grpc` shows only the two interceptors and the tests.
- [ ] **Step 3:** Hand on to the workflow gates: whole-branch `requesting-code-review` with a fresh-context reviewer, then `cyoda-go-security-audit` on the exact PR head, then the PR. CI green on that head is "done".
