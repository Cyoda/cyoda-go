# Platform Clock Confirmation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A platform clock change answers 200 only when every node of the cluster holds the new setting, the 200 lists those nodes, and any node that does not confirm fails the change with 503 `CLOCK_NOT_CONFIRMED`.

**Architecture:** The handling node installs a fresh setting, then pushes it in parallel to every other gossip member over the authenticated peer channel (`POST /internal/dispatch/clock`, sealed like a callout hand-over). Each peer applies it (newest version wins) and echoes the full setting it then holds; only an exact echo confirms. Every step of a move is confirmed before the move waits for that step's tasks, and every change ends with a setting it installed itself. Gossip keeps only the join-time state exchange; the broadcast topic is removed.

**Tech Stack:** Go 1.27.3, `log/slog`, hashicorp/memberlist v0.6.0, the AEAD peer channel in `internal/cluster/dispatch`, OpenAPI codegen (`go generate ./api`), testcontainers e2e, cross-backend parity.

**Spec:** `docs/superpowers/specs/2026-10-26-611-platform-clock-design.md`. This plan builds what commits `2c2d4f30` and `470dbed3` added to it (`git show 2c2d4f30 470dbed3`): sections "A change is pushed and confirmed", "The peer channel", "Joining nodes: the join exchange only", the 200/503 rows of "API", the `CYODA_PROXY_TIMEOUT` paragraph of "Configuration", production-impact items 10 and 11, the "Peer route" and "SSRF" security bullets, the documentation bullets, and the coverage-matrix rows from "Cluster: a change through node A …" to "`CYODA_PROXY_TIMEOUT` ≤ 0 fails config validation". Everything else in the spec is already built.

## Global Constraints

- **Peer route:** `POST /internal/dispatch/clock`, registered at the root, outside the context path, on every node in cluster mode whether clock control is on or off.
- **Match rule:** a confirmation counts only when the echo equals the sent `Setting` in all five fields (`StartUnixMicro`, `AnchorUnixMicro`, `RatePPM`, `Version`, `Writer`).
- **Round bound:** the whole confirmation round is bounded by `CYODA_PROXY_TIMEOUT` (default `30s`); each connection by `CYODA_DISPATCH_CONNECT_TIMEOUT` (the forwarder's dialer). `CYODA_PROXY_TIMEOUT` ≤ 0 fails startup.
- **Error:** 503 `CLOCK_NOT_CONFIRMED`, not retryable; the detail names the id of every node that did not confirm, and nothing else about them. Peer addresses, URLs and forwarder error text go to the log only. Nothing is rolled back.
- **200 body:** `PlatformClockChangeResultDto` = `now`, `rate`, `controllable` + `nodes` (array of strings, required, ascending, the handling node included). Single node: `["local"]`. `GET /platform/clock` and gRPC `PlatformClockResponse` keep `PlatformClockDto` unchanged.
- **Fresh setting:** every change ends by installing a setting (version bumped, `Writer` = handling node id) and confirming it. A version-0 setting (empty writer) is never pushed.
- **Order:** a set-back refused by the store answers 409 and pushes nothing. A client that leaves during a round gets no answer; the error is marked `common.ClientGone` (logged at DEBUG by the door).
- **Gossip:** only the join-time push/pull exchange carries the setting: `MergeRemoteState` merges only when memberlist's `join` flag is true. The `platform.clock` topic, its subscription, `Publish` and the publish wiring are deleted.
- **Forwarder:** built in cluster mode whatever `cfg.ExternalProcessing` is; the SSRF guard (`validatePeerAddress`) applies to clock pushes.
- **Received setting:** validated with `decode` (`internal/platformclock/sync.go:29-46`); a refusal is logged without the payload.
- **No issue IDs** (`#NNN`) in code, comments, logs, OpenAPI or help text. `log/slog` only. Mutex: `Lock()` followed by `defer Unlock()` (IIFE for early release).
- **Time reads:** no new `time.Now()` in non-test code; a real-time read carries `// realtime: <reason>`.
- **Verification:** `go test` on the touched packages while iterating, `make test` at the end of each task, `go vet ./...`. Never `-count=1`, never `make test-full` or `make race` locally. The e2e package locally: `go test -timeout 30m ./internal/e2e/... -run '<pattern>'` (Docker required, no `-v`).
- Every commit message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A rate-only change in a cluster** is pushed and confirmed like a set or a move; a peer reads the new rate the moment the 200 arrives. → Task 5, `TestPlatformClock_Cluster_ConfirmedByEveryNode` (rate step).
2. **A peer that accepts the connection and never answers** ends the round at the bound with 503 naming it; the request does not hang until the client gives up. → Task 2, `TestClusterConfirm_SilentPeerEndsAtBound`.
3. **The handling node is in its own gossip directory**: it is never pushed to and appears once in `nodes`. → Task 2, `TestClusterConfirm_ListsEveryNodeAscending`.
4. **After a 503 the handling node and the confirming nodes keep the setting** (no rollback), and `GET` shows it. → Task 5, `TestPlatformClock_Cluster_UnreachablePeer503` and `TestPlatformClock_Cluster_PeerClockOff503`.
5. **A move whose intermediate step makes no new setting** (the clock already passed that due time) pushes nothing for that step and still ends with a confirmed setting of its own. → Task 4, the adapted `TestMove_StepSkippedWhenClockPassedDueTime`.

---

### Task 1: Registry — list every directory member; merge clock state only on join

**Files:**
- Modify: `internal/cluster/registry/gossip.go` (`List`/`Lookup` neighbourhood `:416-458`; `MergeRemoteState` `:634-638`)
- Modify: `internal/cluster/registry/gossip_export_test.go`
- Test: `internal/cluster/registry/gossip_badmeta_internal_test.go`, `internal/cluster/registry/gossip_state_test.go`

**Interfaces:**
- Produces:
  - `type Member struct { NodeID string; Addr string }` — `NodeID` is the memberlist name (= `CYODA_NODE_ID`); `Addr` is the HTTP address from the metadata, empty when the metadata does not parse.
  - `func (g *Gossip) Members() []Member` — every member of the directory, this node included, unsorted.
  - `(*gossipDelegate).MergeRemoteState(buf []byte, join bool)` merges only when `join` is true.
  - Test export: `func (g *Gossip) MergeRemoteStateForTest(buf []byte, join bool)`.

- [ ] **Step 1: Write the failing tests**

In `gossip_badmeta_internal_test.go`, `TestGossip_Members_IncludesUnparseableWithoutAddress`: start `startInternalGossip(t, "members-1", …)` and `startRawPeer(t, "members-stranger", []byte("not json"), gossipAddr(r))`, wait for the stranger as in the test above it, then assert `r.Members()` contains `{NodeID: "members-1", Addr: <its NodeAddr>}` and `{NodeID: "members-stranger", Addr: ""}`.

In `gossip_state_test.go`, `TestGossip_MergeOnlyOnJoin`: register a state exchange on a fresh `Gossip` whose merge records calls; `MergeRemoteStateForTest([]byte("x"), false)` → no call; `MergeRemoteStateForTest([]byte("x"), true)` → one call with `"x"`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cluster/registry/ -run 'TestGossip_Members_IncludesUnparseableWithoutAddress|TestGossip_MergeOnlyOnJoin'`
Expected: compile failure (`Members`, `MergeRemoteStateForTest` undefined).

- [ ] **Step 3: Implement**

`Members` reads `g.dir.all()`, parses each meta, logs a WARN for an unparseable one (same fields as `List`) and keeps it with an empty `Addr`. `MergeRemoteState` returns at once when `!join`; update its doc comment and the `gossipDelegate` comment (`:566-571`) to say the merge runs on a join only. `LocalState` is unchanged.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/cluster/registry/`
Expected: PASS (including `TestGossip_StateExchangeOnJoin`).

- [ ] **Step 5: Commit**

```bash
git add internal/cluster/registry/
git commit -m "feat(registry): list every directory member and merge pushed state only on a join"
```

---

### Task 2: The confirmation round and `CLOCK_NOT_CONFIRMED`

**Files:**
- Create: `internal/platformclock/confirm.go`, `internal/platformclock/confirm_test.go`
- Modify: `internal/platformclock/handler.go` (`classify`, `:192-209`), `internal/platformclock/handler_test.go`
- Modify: `internal/common/error_codes.go` (beside `ErrCodeClockBeforeRecordedTime` `:170-173`, and `knownErrorCodes` `:312-313`)
- Create: `cmd/cyoda/help/content/errors/CLOCK_NOT_CONFIRMED.md`
- Modify: `cmd/cyoda/help/content/errors.md` (after `:65`)
- Modify: `api/openapi.yaml` (POST `/platform/clock` 503, `:1115-1116`), then `go generate ./api`

**Interfaces:**
- Produces (package `platformclock`):
  - `type Peer struct { NodeID string; Addr string }` — `Addr` empty: no usable address.
  - `type Push func(ctx context.Context, peer Peer, s Setting) (Setting, error)` — sends `s`, returns the setting the peer holds afterwards.
  - `type Confirm func(ctx context.Context, s Setting) ([]string, error)` — returns the ids of every node holding `s`, the handling node included, ascending.
  - `func SingleNode(self string) Confirm` — always `[]string{self}, nil`.
  - `func Cluster(self string, peers func() []Peer, push Push, bound time.Duration) Confirm` — panics on an empty `self`, nil `peers`, nil `push` or `bound <= 0`.
  - `type NotConfirmedError struct { Nodes []string }` (ascending); `Error()` = `"the platform clock setting was not confirmed by node(s) " + strings.Join(Nodes, ", ")`.
  - `common.ErrCodeClockNotConfirmed = "CLOCK_NOT_CONFIRMED"`.

- [ ] **Step 1: Write the failing tests** (`confirm_test.go`, package `platformclock`)

A recording `Push` fake keyed by node id (echo, error, or block until ctx done). `s := Setting{StartUnixMicro: 1, AnchorUnixMicro: 1, RatePPM: 1_000_000, Version: 3, Writer: "b"}`.

- `TestSingleNode_ListsSelf`: `SingleNode("local")(ctx, s)` → `["local"], nil`.
- `TestClusterConfirm_ListsEveryNodeAscending`: self `"b"`, peers `[{c,addr-c},{b,addr-b},{a,addr-a}]`, both echo `s` → `["a","b","c"]`; the fake saw pushes for `a` and `c` only, never `b`.
- `TestClusterConfirm_PushFails`: `c` returns an error → `*NotConfirmedError{Nodes: ["c"]}`; the error text contains `c` and not `addr-c`.
- `TestClusterConfirm_EchoDiffers`: `a` echoes `s` with `Version: 4` → `NotConfirmedError{["a"]}`.
- `TestClusterConfirm_NoUsableAddress`: peer `{x, ""}` → `NotConfirmedError{["x"]}`, and no push to `x`.
- `TestClusterConfirm_SilentPeerEndsAtBound`: `a` blocks until its ctx ends; `bound = 50*time.Millisecond` → returns within 2 s with `NotConfirmedError{["a"]}`, and `errors.Is(err, common.ErrClientGone)` is false.
- `TestClusterConfirm_PushesInParallel`: `a` and `c` each wait on a barrier both must reach (both echo after it) → confirms within 2 s.
- `TestClusterConfirm_ClientGone`: `a` blocks; cancel the parent ctx after the push starts → `errors.Is(err, common.ErrClientGone)`, and `errors.As(err, new(*NotConfirmedError))` is false.
- `TestCluster_PanicsOnMissingCollaborator`: table over the four refusals.

In `handler_test.go`, `TestHandler_503_NotConfirmed`: fake controller error `&platformclock.NotConfirmedError{Nodes: []string{"node-b", "node-c"}}` → status 503, `errorCode` `CLOCK_NOT_CONFIRMED`, `detail` contains `node-b` and `node-c`, the problem is not marked retryable (assert as `TestHandler_503_StoreFails` reads the problem).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/platformclock/ -run 'SingleNode|ClusterConfirm|Cluster_Panics|NotConfirmed'`
Expected: compile failure (undefined `SingleNode`, `Cluster`, `NotConfirmedError`, `ErrCodeClockNotConfirmed`).

- [ ] **Step 3: Implement `Cluster`**

Derive `round, cancel := context.WithTimeout(ctx, bound)`. Skip the peer whose `NodeID == self`; a peer with empty `Addr` fails without a push. Push to the rest in parallel goroutines under `round`, collect into a mutex-guarded result (IIFE + `defer Unlock`), wait for all. Then: if `ctx.Err() != nil` return `common.ClientGone(fmt.Errorf("platform clock confirmation stopped: %w", ctx.Err()))`. Each failed peer is logged at WARN once: `"platform clock setting not confirmed by a node"`, `pkg platformclock`, `nodeId`, `addr`, `reason` (the push error text, `"echoed a different setting"` with the echo's `version`/`writer`, or `"no usable address"`). Any failure → `&NotConfirmedError{Nodes: sorted}`. Otherwise return self plus the confirmed ids, `sort.Strings`.

- [ ] **Step 4: Error code, classification, help, OpenAPI**

- `ErrCodeClockNotConfirmed` with a doc comment ("returned by POST /platform/clock when a node of the cluster did not confirm the change or a move's step. 503, not retryable; nothing is rolled back"), added to `knownErrorCodes`.
- `classify`: after the `OutOfRangeError` case, `errors.As(err, &nc)` → `common.Operational(http.StatusServiceUnavailable, common.ErrCodeClockNotConfirmed, fmt.Sprintf("the clock change was not confirmed by node(s) %s; the nodes that confirmed keep it and nothing is rolled back: start over by restarting the cluster with a fresh store", strings.Join(nc.Nodes, ", ")))`, not retryable.
- `errors/CLOCK_NOT_CONFIRMED.md`, shaped like `errors/CLOCK_CONTROL_DISABLED.md`: `503 Service Unavailable`, retryable `no`; the four causes from the spec's 503 row; the detail names the nodes; nothing is rolled back; start over by restarting the cluster with a fresh store; the cluster must be static, idle, one change at a time; `see_also` `errors`, `clock`, `config.cluster`. Index line in `errors.md` after `CLOCK_CONTROL_DISABLED`: ``- `errors.CLOCK_NOT_CONFIRMED` — `503` — not retryable — a node of the cluster did not confirm a platform clock change; nothing is rolled back``.
- OpenAPI: replace POST's `"503": $ref ServiceUnavailable` with an inline response, `application/problem+json` `ProblemDetail`, description: `Service unavailable. properties.errorCode is STORAGE_UNAVAILABLE or CONSISTENCY_TIME_UNAVAILABLE when the store did not answer the consistency-time check or the move's task query, and CLOCK_NOT_CONFIRMED when a node of the cluster did not confirm the change or one of a move's steps within CYODA_PROXY_TIMEOUT — it did not answer, refused (clock control is off there, or the setting failed its validation), answered a different setting, or has no usable address. The detail names those nodes. CLOCK_NOT_CONFIRMED is not retryable: nothing is rolled back; start over with a fresh store.` Then `go generate ./api`.

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/platformclock/ ./internal/common/ ./cmd/cyoda/help/... ./api/...`
Expected: PASS (`TestErrCode_Parity` and `TestKnownErrorCode_IsExactlyWhatThisFileDefines` included).

- [ ] **Step 6: Commit**

```bash
git add internal/platformclock/confirm.go internal/platformclock/confirm_test.go internal/platformclock/handler.go internal/platformclock/handler_test.go internal/common/error_codes.go cmd/cyoda/help/content/errors/CLOCK_NOT_CONFIRMED.md cmd/cyoda/help/content/errors.md api/
git commit -m "feat(clock): a confirmation round over every peer, and 503 CLOCK_NOT_CONFIRMED"
```

---

### Task 3: The peer channel — hold a pushed setting, the clock route, the forwarder call

**Files:**
- Modify: `internal/platformclock/sync.go` (add the holder beside `receive`), `internal/platformclock/sync_test.go`
- Create: `internal/cluster/dispatch/clock.go`, `internal/cluster/dispatch/clock_test.go`
- Modify: `internal/cluster/dispatch/handler.go` (`writeSealed` `:153-183` becomes a package function both handlers use)
- Modify: `internal/cluster/dispatch/forwarder.go` (add `PushClock` beside `ForwardCallout` `:119-136`)
- Test: `internal/cluster/dispatch/forwarder_ssrf_test.go`

**Interfaces:**
- Consumes: `platformclock.Setting`, `Test.Apply`, `Test.Setting`, `decode`.
- Produces (package `platformclock`):
  - `var ErrClockControlOff = errors.New("clock control is off on this node")`
  - `type Holder struct { … }`; `func NewHolder(c *Test) *Holder` — `c` nil means clock control is off.
  - `func (h *Holder) Hold(payload []byte) (Setting, error)` — refuses with `ErrClockControlOff` when `c` is nil; a payload that fails `decode` returns `fmt.Errorf("invalid clock setting: %w", err)` and is logged at WARN without the payload (as `receive` does); otherwise `Apply` (adoption logged through `adoptLog`) and return `c.Setting()`.
- Produces (package `dispatch`):
  - `const ClockPath = "/internal/dispatch/clock"`
  - `type ClockHolder interface { Hold(payload []byte) (platformclock.Setting, error) }`
  - `type ClockPushAnswer struct { Held *platformclock.Setting `json:"held,omitempty"`; Refused string `json:"refused,omitempty"` }`
  - `func NewClockHandler(auth PeerAuth, holder ClockHolder) *ClockHandler`; `func (h *ClockHandler) Register(mux *http.ServeMux)` registers `POST /internal/dispatch/clock`.
  - `func (f *HTTPForwarder) PushClock(ctx context.Context, peerNodeID, addr string, s platformclock.Setting) (platformclock.Setting, error)`.

- [ ] **Step 1: Write the failing tests**

`sync_test.go` (black-box), with `validSetting` from the file:
- `TestHolder_HoldsNewerAndEchoesIt`: `NewHolder(c).Hold(json(validSetting(5)))` → returns it; `c.Setting()` equals it.
- `TestHolder_EqualEchoesSame`: hold `s` twice → second answer equals `s`.
- `TestHolder_OlderEchoesHeld`: hold version 5, then version 4 → answer is the version-5 setting.
- `TestHolder_InvalidRefused`: each payload of `TestSync_IgnoresOlderAndGarbage`'s invalid list → error; `c.Setting()` unchanged.
- `TestHolder_ClockOffRefuses`: `NewHolder(nil).Hold(…)` → `errors.Is(err, ErrClockControlOff)`.

`clock_test.go` (package `dispatch`; server = `httptest.NewServer` over a mux with `NewClockHandler(newAEAD(t), holder).Register(mux)`; sender = `NewHTTPForwarder(<auth whose sealing reaches testSelfNodeID>, time.Second).AllowLoopbackForTesting()`, as `forwarder_test.go` pairs them):
- `TestClockPush_PeerHoldsAndEchoes`: `PushClock(ctx, testSelfNodeID, srv.URL, s)` → returns `s`; the holder's clock holds `s`.
- `TestClockPush_ClockOffIsSealedRefusal`: holder `platformclock.NewHolder(nil)` → error containing `clock control is off`; the raw HTTP status was 200 (sealed), checked with a handler-level request.
- `TestClockPush_InvalidSettingIsSealedRefusal`: `s.Writer = ""` → error containing `empty writer`.
- `TestClockRoute_EnvelopeForCalloutPathRefused`: sign a body for path `/internal/dispatch/callout`, POST it to `ClockPath` → bare 403.
- `TestClockRoute_EnvelopeForAnotherNodeRefused`: sign for recipient `"node-other"` → bare 403.
- `TestClockRoute_ReplayCacheFullIsSealedRefusal`: wrap the AEAD in a `PeerAuth` whose `Verify` returns the inner result with `ErrReplayCacheFull` → 200 with a sealed `ClockPushAnswer` whose `Refused` is non-empty; the holder was not called.

`forwarder_ssrf_test.go`, `TestHTTPForwarder_PushClockRejectsLoopback`: a forwarder without `AllowLoopbackForTesting` → `PushClock` to `http://127.0.0.1:1` fails with `*ForwardError` `StageNotConnected`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/platformclock/ ./internal/cluster/dispatch/ -run 'Holder|ClockPush|ClockRoute|PushClock'`
Expected: compile failure (undefined `NewHolder`, `NewClockHandler`, `PushClock`, `ClockPath`).

- [ ] **Step 3: Implement**

- `writeSealed` becomes `func writeSealed(w http.ResponseWriter, auth PeerAuth, binding ResponseBinding, v any, attrs ...any)`; the oversized-answer log line takes `attrs` (the callout handler passes `requestId`, `outcome`). `DispatchHandler` calls it.
- `ClockHandler`: `Verify`; `ErrReplayCacheFull` → WARN log and sealed `ClockPushAnswer{Refused: "the replay cache refused the request"}`; any other error → WARN and `http.Error(w, "forbidden", 403)` (as `handler.go:75-78`). Then `Hold(body)`: error → WARN `"clock push refused"` with `reason` (the holder's authored text), sealed `Refused`; success → sealed `Held`.
- `PushClock`: `validatePeerAddress` under a `connectTimeout` lookup context (as `ForwardCallout`), then `f.forward(ctx, peerNodeID, ensureScheme(addr)+ClockPath, &s, &answer)`. `Refused != ""` → `fmt.Errorf("the peer refused the clock setting: %s", answer.Refused)`; `Held == nil` → `errors.New("the peer's answer carries no clock setting")`; else `*answer.Held`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/platformclock/ ./internal/cluster/dispatch/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platformclock/sync.go internal/platformclock/sync_test.go internal/cluster/dispatch/
git commit -m "feat(dispatch): a sealed peer route that holds a pushed clock setting and echoes it"
```

---

### Task 4: Every change is confirmed and ends with a fresh setting; POST answers the nodes

**Files:**
- Modify: `internal/platformclock/service.go` (`Service`, `NewService` `:62-99`, `Change` `:111-159`, `setBack` `:166-181`, `commit` `:183-187`, `move` `:193-231`)
- Modify: `internal/platformclock/handler.go` (`Controller` `:24-28`, `ChangePlatformClock` `:184-189`, a `changeBody`)
- Modify: `internal/platformclock/service_test.go`, `internal/platformclock/handler_test.go`
- Modify: `api/openapi.yaml` (POST 200 `:1087-1092`; new schema after `PlatformClockDto` `:11172-11188`), then `go generate ./api`
- Modify: `app/app.go` (`:592-608`), `app/clock_wiring_test.go` (`:213`), `internal/cluster/registry/gossip_state_test.go` (`:28-35`)
- Modify: `internal/e2e/platform_clock_api_test.go` (`clockBody` `:17-21`, `TestPlatformClock_Post_RateThenGet` `:175-184`)
- Modify: `e2e/parity/platform_clock.go`, `e2e/parity/registry.go` (`:480-485`), `e2e/parity/fixture.go` (`FreshStackFixture` doc `:60-71`)

**Interfaces:**
- Consumes: `Confirm`, `SingleNode`, `NotConfirmedError` (Task 2).
- Produces:
  - `type ChangeResult struct { State State; Nodes []string }`
  - `func NewService(c *Test, storeTime func(ctx context.Context) (time.Time, error), tasks func(ctx context.Context) (spi.ScheduledTaskStore, error), confirm Confirm, poll time.Duration) *Service` — panics `"platformclock.NewService: nil confirm"`.
  - `func (s *Service) Change(ctx context.Context, ch Change) (ChangeResult, error)`; on error the result carries `s.State()` and nil `Nodes`.
  - `type Controller interface { State() State; Change(ctx context.Context, ch Change) (ChangeResult, error) }`
  - OpenAPI `PlatformClockChangeResultDto` (`now`, `rate`, `controllable` as in `PlatformClockDto`, plus `nodes`: array of string, "The ids of every node that holds the new setting, this node included, in ascending order; `[\"local\"]` on a single node."), all four required.

- [ ] **Step 1: Write the failing unit tests**

In `service_test.go`, replace `publishLog` with `confirmLog` (`sets []Setting`, `hook func(Setting)`, `failAt int` (1-based call that returns `failErr`), `nodes []string`) whose method `confirm(ctx, s) ([]string, error)` records `s`, runs `hook`, and returns `nodes` (default `["n1"]`); rename the fixture field `pub` → `conf` and every use (`starts()` stays). Update `TestNewService_NilCollaboratorPanics` ("a nil confirm"). New tests:
- `TestChange_ReturnsConfirmedNodes`: `conf.nodes = ["a","n1"]`; a rate change → `res.Nodes == ["a","n1"]`.
- `TestChange_NotConfirmed_KeepsSetting`: `conf.failAt = 1`, `failErr = &NotConfirmedError{Nodes: ["a"]}`; a rate change to 2_000_000 → `errors.As` NotConfirmedError, `f.clock.Setting().RatePPM == 2_000_000`.
- `TestMove_StepNotConfirmed_StopsThere`: due `h1, h2`, target `h3`; `conf.failAt = 1` → error is NotConfirmedError; `f.clock.Now()` equals `h1`; exactly one confirm call; `f.sched.polls` does not grow after the failure (record it in the hook).
- `TestMove_ConfirmsEachStepBeforeWaiting`: due `h1, h2`; `f.sched.onPoll` fails the test when `f.clock.Setting() != last confirmed` once a confirm has happened.
- `TestMove_TargetPassed_InstallsFreshFinalSetting`: `f.sched.onPoll = func() { f.src.advance(time.Second) }`; `Change{AdvanceByMs: ptr(int64(1))}` → one confirmed setting, `Version == 1`, `Writer == "n1"`, start within 1 ms of `f.clock.Now()` read right after; `res.Nodes == ["n1"]`.
- `TestChange_EveryConfirmedSettingIsFresh`: a rate change, a set-back to `t0-1h`, a move with due `h1`, and an `AdvanceByMs: 1` whose target is passed → over all confirmed settings `Writer == "n1"`, versions strictly increase from 1, none is version 0.
- `TestSetBack_RefusedWhenRecordedLater` and `TestSetBack_ConsistencyTimeErrorRestores`: assert `len(f.conf.sets) == 0`.
- Adapt `TestMove_DueAtTarget_DoesNotMoveBack`: confirmed starts begin with `target`, and the last confirmed setting starts no earlier than `target+1ms` with a higher version than the first (the fresh final setting).
- Adapt `TestMove_StepSkippedWhenClockPassedDueTime`: its confirm func also checks versions strictly increase and, after `Change`, the last confirmed setting equals `c.Setting()` (Review Focus 5).
- Adapt `TestSetBack_PendingSettingNeverServed`, `TestSetBack_PendingOnFreshClockServesNothing`, `TestMove_RepeatAfterCancelContinues`, `TestMove_SchedulerErrorMidMove` and the remaining `f.pub` uses mechanically.

In `handler_test.go`: `fakeController.Change` returns `ChangeResult{State: f.State(), Nodes: f.nodes}`; `TestHandler_Post_200_ListsNodes`: `f.nodes = []string{"node-a","node-b"}` → body `nodes == ["node-a","node-b"]` and `now`, `rate`, `controllable` as before.

- [ ] **Step 2: Write the failing e2e and parity tests**

- `clockBody` gains `Nodes []string `json:"nodes"``. `TestPlatformClock_Post_RateThenGet`: the POST answer's `Nodes` equals `[]string{"local"}`; the GET answer's `Nodes` is nil.
- `e2e/parity/platform_clock.go`: `clockAnswer` gains `Nodes`; `RunPlatformClockChangeListsNodes(t, fixture)`: on `newClockStack(t, fixture, nil)`, POST `{"time": clockParityStart}`, then `{"rate":2}`, then `{"advanceByMs":1000}` → each answer's `Nodes` equals `["local"]`. Register `{"PlatformClockChangeListsNodes", RunPlatformClockChangeListsNodes}` after `PlatformClockPastStartAsyncSearch`. `FreshStackFixture`'s doc says the stack is a single node (cluster mode off).

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/platformclock/`
Expected: compile failure (`ChangeResult`, `NewService` with `Confirm`).

- [ ] **Step 4: Implement the service**

- `commit(ctx, st Setting) ([]string, error)`: `s.c.share(st)`, then `s.confirm(ctx, st)`.
- Rate only: `commit(ctx, s.c.set(nil, rate))`. Set-back: `commit(ctx, next.setting)` after the store check passes (a refusal returns before any commit).
- `move` returns `([]string, error)`. A step: `if st, ok := s.c.setForward(e, nil); ok { if _, err := s.commit(ctx, st); err != nil { stop } }`. The end: `st, ok := s.c.setForward(target, rate); if !ok { st = s.c.set(nil, rate) }; return s.commit(ctx, st)`. "Stop" logs the existing `"platform clock move stopped"` INFO line through one helper shared with the `awaitIdle` failure (the `cause` attribute only for `ErrClientGone`) and returns the error.
- Update the `Service`, `NewService`, `commit` and `move` doc comments: changes are confirmed, not published.

- [ ] **Step 5: Implement the handler, OpenAPI and call sites**

- `changeBody{Now, Rate, Controllable, Nodes}` with JSON names `now`, `rate`, `controllable`, `nodes`; `ChangePlatformClock` writes it from the `ChangeResult`.
- OpenAPI: POST 200 → `PlatformClockChangeResultDto`, description "The clock after the change, and the nodes that hold it."; `go generate ./api`.
- `app/app.go`: `NewService(testClock, …, confirm, 100*time.Millisecond)` where `confirm := platformclock.SingleNode(a.selfNodeID)`, and in cluster mode an interim `Confirm` that calls `platformclock.Publish(gossipReg, s)` and returns `[]string{a.selfNodeID}` (Task 5 replaces it). Delete the `publish` variable.
- `app/clock_wiring_test.go:213` and `gossip_state_test.go:28-35`: pass `platformclock.SingleNode("clock-probe")` / `SingleNode("st-1")`.

- [ ] **Step 6: Run to verify they pass**

Run: `go test ./internal/platformclock/ ./internal/cluster/registry/ ./app/ ./api/...` then `go test -timeout 30m ./internal/e2e/... -run 'TestPlatformClock'` then `make test`
Expected: PASS; `make test` runs `PlatformClockChangeListsNodes` on memory, sqlite and postgres.

- [ ] **Step 7: Commit**

```bash
git add internal/platformclock/ api/ app/ internal/cluster/registry/gossip_state_test.go internal/e2e/platform_clock_api_test.go e2e/parity/
git commit -m "feat(clock): every change ends with a fresh confirmed setting and POST lists the nodes"
```

---

### Task 5: Cluster carriage — push over the peer channel, join exchange only, `CYODA_PROXY_TIMEOUT` validated

**Files:**
- Modify: `app/app.go` (`:192-199` attach, `:485-512` peer auth and forwarder, `:592-608` clock service, `:727-747` route registration), `app/clock.go`
- Modify: `app/config.go` (`ValidateDispatch` `:928-941`), `app/config_dispatch_test.go`
- Modify: `app/clock_wiring_test.go`
- Modify: `internal/platformclock/sync.go` (delete `topic` `:14-15`, the subscription `:90`, `Publish` `:106-110`; `Attach(c *Test, ex StateExchanger)`), `internal/platformclock/export_test.go`, `internal/platformclock/sync_test.go`, `internal/platformclock/service_test.go` (`nopBroadcaster` `:630-633`), `internal/platformclock/clock.go` (comments `:1-3`, `:39-42`, `:207-208`)
- Modify: `internal/cluster/registry/gossip_state_test.go`
- Modify: `internal/e2e/platform_clock_test.go` (`newClockSeed` `:664-697`, `clockClusterNode` `:700-718`, `TestPlatformClock_TwoNodes_SettingPropagates` `:720-761`)
- Modify: `cmd/cyoda/help/content/config/cluster.md` (`CYODA_PROXY_TIMEOUT`, `:27`)

**Interfaces:**
- Consumes: `registry.Member`, `(*Gossip).Members` (Task 1); `platformclock.Cluster`, `Peer`, `Push` (Task 2); `platformclock.NewHolder`, `dispatch.NewClockHandler`, `(*HTTPForwarder).PushClock` (Task 3); `NewService(…, confirm, …)` (Task 4).
- Produces (package `app`, `app/clock.go`):
  - `func clockPeers(g *registry.Gossip) func() []platformclock.Peer`
  - `func clockPush(f *clusterdispatch.HTTPForwarder) platformclock.Push`
  - `func Attach(c *Test, ex StateExchanger)` (package `platformclock`).

- [ ] **Step 1: Write the failing tests**

`app/config_dispatch_test.go`: `validDispatchConfig` sets `ProxyTimeout: 30*time.Second`; add cases `{"zero proxy timeout", ProxyTimeout = 0, "CYODA_PROXY_TIMEOUT"}` and `{"negative proxy timeout", -time.Second, …}`.

`app/clock_wiring_test.go`, `TestClockWiring_ForwarderAndClockRouteInClusterMode` (source pin, as `TestClockWiring_StartupOrder`): `clusterdispatch.NewHTTPForwarder(` occurs once and before `if cfg.ExternalProcessing != nil`; `clusterdispatch.NewClockHandler(` occurs once; `platformclock.Publish(` occurs zero times. In `TestClockWiring_StartupOrder` the probe attaches with `platformclock.Attach(held, probe)`.

`internal/e2e/platform_clock_test.go` — the fixture first:
- `type clockSeed struct { gossip, nodeID string; clock *platformclock.Test }`; `newClockSeed(t, serve bool) clockSeed`. With `serve`, the seed runs `httptest.NewServer` over a mux with `dispatch.NewClockHandler(<AEAD for its nodeID over clusterHMACSecret32, 30 s skew>, platformclock.NewHolder(clock)).Register(mux)`, and advertises that server's URL as `NodeAddr`. Without it, `NodeAddr` stays `http://127.0.0.1:1` (serves nothing). It attaches with today's `platformclock.Attach(clock, g, g)`, so the tests fail on behaviour, not on compilation; Step 3 changes the call to `Attach(clock, g)`.
- `clockClusterNode(t, s, key, nodeID, seed string, clockOn bool)`.
- `sortedIDs(ids ...string) []string` helper.

Then the tests (each node id `clock-<x>-<sfx>`):
- `TestPlatformClock_Cluster_ConfirmedByEveryNode` (replaces `TestPlatformClock_TwoNodes_SettingPropagates`): serving seed S, nodes A and B. `startClockAt(A, clockStart)` answer `Nodes == sortedIDs(S, A, B)`; B's very next `GET` is on the setting (no polling) and `S.clock.Now()` is too. POST `{"rate":2}` on A → `Nodes` all three; B's next `GET` has `Rate == 2` (Review Focus 1). Node C joins through S → its first `GET` has `Rate == 2` and `Now` not before `clockStart`.
- `TestPlatformClock_Cluster_FreshShortAdvanceAndIdleMove`: serving seed S, node A, nothing set. POST `{"advanceByMs":1}` on A → 200, `Nodes == sortedIDs(S, A)`, `S.clock.Setting().Version >= 1` and `.Writer == A's id`. POST `{"advanceByMs":3600000}` → 200, `Nodes` both, S's version higher than before, `A`'s `GET` within 5 s of `S.clock.Now()`.
- `TestPlatformClock_Cluster_UnreachablePeer503`: non-serving seed S, node A. POST `{"rate":2}` on A → 503, `errorCode` `CLOCK_NOT_CONFIRMED`, `detail` contains S's id, body does not contain `127.0.0.1:1`; A's `GET` has `Rate == 2` (Review Focus 4).
- `TestPlatformClock_Cluster_PeerClockOff503`: serving seed S, A clock on, B clock off. POST `{"rate":2}` on A → 503 `CLOCK_NOT_CONFIRMED`, `detail` contains B's id and not S's; `S.clock.Setting().RatePPM == 2_000_000` (Review Focus 4).

`sync_test.go`: replace the broadcast tests with merge tests through `fakeExchange` (`TestSync_MergeAppliesNewer`, `TestSync_MergeIgnoresOlderAndGarbage`, `TestSync_MergeAdoptsStartPastSetLimit`, `TestSync_FreshClockCarriesNothing`); drop `fakeBroadcaster`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./app/ -run 'TestValidateDispatch|TestClockWiring'` and `go test -timeout 30m ./internal/e2e/... -run 'TestPlatformClock_Cluster'`
Expected: `ValidateDispatch` accepts a zero proxy timeout; the source pins fail; the cluster e2e tests fail on `Nodes` (only A listed) and on 200 where 503 is expected (the interim confirm from Task 4).

- [ ] **Step 3: Implement**

- `ValidateDispatch`: `if c.ProxyTimeout <= 0 { return fmt.Errorf("CYODA_PROXY_TIMEOUT must be > 0, got %s", c.ProxyTimeout) }`; update its doc comment.
- `app/app.go`:
  - Attach: `platformclock.Attach(testClock, gossipReg)`; update the comment block above it (no broadcast).
  - Build the forwarder in the `if cfg.Cluster.Enabled` peer-auth block, before `if cfg.ExternalProcessing != nil`, with the existing `DispatchAllowLoopback` opt-in; the `PeerRouter` uses it.
  - Clock service: `confirm := platformclock.SingleNode(a.selfNodeID)`; in cluster mode `platformclock.Cluster(a.selfNodeID, clockPeers(gossipReg), clockPush(forwarder), cfg.Cluster.ProxyTimeout)`. Delete the interim confirm. Update the comment above (`:592-594`).
  - Routes: build the dispatch routes once in cluster mode — `NewDispatchHandler(localDispatcher, peerAuth)` and `NewClockHandler(peerAuth, platformclock.NewHolder(testClock))` (`testClock` nil when clock control is off, so every push is refused) — and register both on whichever mux the context-path branch selects, replacing the two duplicated blocks.
- `app/clock.go`: `clockPeers` maps `g.Members()` to `[]platformclock.Peer`; `clockPush` calls `f.PushClock(ctx, peer.NodeID, peer.Addr, s)`.
- `internal/e2e/platform_clock_test.go`: the seed calls `platformclock.Attach(clock, g)`.
- `platformclock`: delete `topic`, `Publish`, the subscription and the `spi.ClusterBroadcaster` parameter; `Topic` from `export_test.go`; `nopBroadcaster` from `service_test.go`. Package and field comments say "pushed or adopted", not "published"/"broadcast"; the `Attach` comment says the exchange merges on a join only.
- `cluster.md:27`: ``- `CYODA_PROXY_TIMEOUT` (duration, default: `30s`) — request proxy timeout, and the bound on one platform clock confirmation round (every peer contacted in parallel). Must be `> 0`; startup fails otherwise.``

- [ ] **Step 4: Exit checks**

Run: `grep -rn 'platformclock\.Publish\|platformclock\.Topic\|"platform\.clock"' --include='*.go' .` and `grep -rni 'publish\|broadcast' internal/platformclock/`
Expected: both empty.

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/platformclock/ ./internal/cluster/... ./app/` then `go test -timeout 30m ./internal/e2e/... -run 'TestPlatformClock'` then `make test` and `go vet ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add app/ internal/platformclock/ internal/cluster/registry/gossip_state_test.go internal/e2e/platform_clock_test.go cmd/cyoda/help/content/config/cluster.md
git commit -m "feat(clock): push each change to every node over the peer channel; gossip carries the setting only on a join"
```

---

### Task 6: Documentation, cloud parity and changelog

**Files:**
- Modify: `cmd/cyoda/help/content/clock.md` (SYNOPSIS and Restrictions; a new section)
- Modify: `cmd/cyoda/help/content/config/clock.md` (`:24`)
- Modify: `docs/ARCHITECTURE.md` (`:97-98`, `:739-740`, `:783`, the internal dispatch endpoint list `:1044-1050`, `:1212`, `:2336`)
- Modify: `docs/cloud-parity/platform-clock.md` (§2 `:31-36`, §3 `:38-50`), `docs/cloud-parity/README.md` (`:76`)
- Modify: `CHANGELOG.md` (`### Added` platform-clock entry `:1213-1223`; `### Breaking`)

- [ ] **Step 1: Help topics**

`clock.md`: SYNOPSIS says a POST answers `now`, `rate`, `controllable` and `nodes`, and `see_also` gains `errors.CLOCK_NOT_CONFIRMED` and `config.cluster`. Add `## TESTING WITH THE PLATFORM CLOCK` before `## SEE ALSO`, covering exactly the spec's list: the three preconditions (cluster static during a change: no node starts, stops or joins; system idle; one change at a time, the next only after the previous call returned); a 200 means every node holds the setting and the answer lists the nodes; on `503 CLOCK_NOT_CONFIRMED` start over by restarting the cluster with a fresh store; set the start in the past before writing data or importing models; retention follows platform time; a move needs a running scheduler and the compute nodes its tasks call; a node starting or shutting down while a change runs can make it fail; synchronised host clocks are required. Restrictions drop nothing they still state truly; "Keep the system idle" points to the new section.
`config/clock.md:24`: "the nodes share the setting through gossip" becomes "a change is pushed to every node and answers only once every node holds it (`503 CLOCK_NOT_CONFIRMED` otherwise); a node that joins takes the setting from the node it joins through".

Run: `go test ./cmd/cyoda/help/...` — Expected: PASS.

- [ ] **Step 2: ARCHITECTURE.md (present tense, reference not history)**

`:97-98` module line: "its peer push and join-time exchange" instead of "its gossip carriage". `:739-740`: a change is pushed to every node over the peer channel and confirmed before it answers (§4.1). `:783` "Platform clock" paragraph rewritten: the handling node pushes each new setting to every directory member over `POST /internal/dispatch/clock` (sealed for the recipient and the path, as a hand-over), in parallel within `CYODA_PROXY_TIMEOUT`; a peer applies it if newer and echoes the setting it holds; an exact echo confirms; otherwise 503 `CLOCK_NOT_CONFIRMED` names the node; the memberlist push/pull exchange carries the setting only on a join (`join` true), registered only with clock control on; only a pushed or adopted setting is served. The internal dispatch endpoint list and the L7 row add `POST /internal/dispatch/clock`. `:2336`: "nodes share the setting through gossip" → "each change is confirmed by every node".

- [ ] **Step 3: Cloud parity (Gate 7)**

`platform-clock.md` §2: drop "the highest version … wins" as the means of spreading a change; state: a change answers 200 only when every node holds the setting; a node that does not confirm gives 503 `CLOCK_NOT_CONFIRMED` naming it, with no rollback; a node that joins takes the setting from its peers; how Cloud nodes reach each other is Cloud's own. §3: POST row answers `200 PlatformClockChangeResultDto` (`now`, `rate`, `controllable`, `nodes` ascending, the handling node included); error table gains the `503 CLOCK_NOT_CONFIRMED` row from the spec. `README.md:76` summary adds the confirmation and `503 CLOCK_NOT_CONFIRMED`.

- [ ] **Step 4: CHANGELOG**

The `### Added` platform-clock entry gains: "A change answers only once every node of a cluster holds the new setting; the answer lists the nodes, and a node that does not confirm fails the change with `503 CLOCK_NOT_CONFIRMED`." Under `### Breaking`: "`CYODA_PROXY_TIMEOUT` must be greater than zero; startup fails otherwise. It also bounds a platform clock confirmation round."

- [ ] **Step 5: Verify and commit**

Run: `go test ./cmd/cyoda/help/... ./api/...` and `make test` — Expected: PASS.

```bash
git add cmd/cyoda/help/content/ docs/ARCHITECTURE.md docs/cloud-parity/ CHANGELOG.md
git commit -m "docs(clock): testing with the platform clock; a change is confirmed by every node"
```

- [ ] **Step 6: CaaS ticket**

The controller (not a subagent) comments on CP-3992 with the contract from `docs/cloud-parity/platform-clock.md` §2–§3 (200 only when every node holds the setting, `nodes` in the 200 body, 503 `CLOCK_NOT_CONFIRMED` with no rollback), after the PR is pushed. If the Atlassian connector is unavailable, say so to Paul instead of skipping it.
