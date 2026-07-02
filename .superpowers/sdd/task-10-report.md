# Task 10 Report — e2e: SYNC callback atomicity + read-your-writes (postgres)

Feature #287 (compute-node callbacks join the originating transaction).

## What was built

A **callback-capable in-process compute member** running against a **full, separate
cyoda-go stack** (real Postgres, HTTP + gRPC). Unlike the `localproc`
`ExternalProcessingService` the other workflow E2E tests use, this harness exercises
the **real gRPC dispatch path** so the engine actually mints and transmits the signed
`cyodatxtoken`. The member echoes that token as `X-Tx-Token` on HTTP callbacks into the
same node, driving `TxJoin` middleware → `JoinFromToken` → participate. This is the gap
the skipped `TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten` pointed at.

Why a second app instance (not the global `testApp`): `TestMain` wires
`cfg.ExternalProcessing = procSvc` (localproc), which bypasses gRPC entirely — no token is
ever minted. The callback path *requires* `cfg.ExternalProcessing == nil` so the real gRPC
`ProcessorDispatcher` is selected. The harness stands up its own stack (own JWT key + OAuth,
own httptest server, own gRPC listener) sharing the package Postgres testcontainer (the
`CYODA_POSTGRES_*` env vars set by `TestMain` are still live; golang-migrate re-migration is
a no-op). Callbacks go over HTTP because the gRPC `EntityManage` surface has **no joined
read** RPC, whereas Task 9's HTTP `TxJoin` middleware wraps *all* entity routes (create AND
get/search) — giving both joined writes and joined reads uniformly.

## Reusable harness entry points (for Tasks 11–14)

File: `internal/e2e/callback_harness_test.go`

- `newCallbackHarness(t) *callbackHarness` — full stack + connected gRPC member; self-tears-down via `t.Cleanup`.
- `(*callbackHarness).RegisterProc(name string, fn callbackProc)` — register a processor implemented on the member.
- `(*callbackHarness).SetupModelWithWorkflow(t, entityName, workflowJSON)` — import+lock model, import workflow.
- `(*callbackHarness).CreateEntity(t, name, version, payload) (id, status, body)` — client-facing POST (no token).
- `(*callbackHarness).GetEntityState(t, id) (state, httpStatus)` — returns `("", 404)` for a rolled-back/absent entity.
- `(*callbackHarness).GetEntityData(t, id) map[string]any`
- `(*callbackHarness).DoAuth(t, method, path, body, txToken) *http.Response` — authed request; `txToken != ""` echoes `X-Tx-Token`.
- `callbackProc func(rc *reqCtx) (applyData map[string]any, err error)` — processor body; runs on the member goroutine (do NOT `t.Fatal` inside — record into test-owned state). Returning an error fails the transition (rolls back a SYNC T).
- `reqCtx` — per-request context handed to a processor. Fields: `token`, `requestID`, `entityID`, `entityData` (attached uncommitted primary data), `entityMeta`. Methods:
  - `(*reqCtx).CreateEntity(name, version, payload) (callbackResult, error)` — token-echoing POST (joined write).
  - `(*reqCtx).GetEntity(id) (callbackResult, error)` — token-echoing GET (joined read-your-writes).
- Helpers: `cloneData`, `entityDataField`; constant `secondaryWorkflow` (a no-processor `NONE→STORED` workflow to avoid nested-processor gate contention).

## Tests (file: `internal/e2e/callback_txjoin_test.go`)

1. **`TestCallback_SyncWrite_AtomicWithTransition`** — SYNC processor callback CREATEs a secondary entity.
   - Success branch: primary commits ACTIVE, secondary durable (STORED), lineage (`secondaryId`) recorded.
   - Failure branch: processor creates the secondary then errors → primary POST 400 `WORKFLOW_FAILED`, and the secondary GET is **404** (rolled back atomically with T). *This is the atomicity proof.*
2. **`TestCallback_SyncRead_SeesUncommittedCascadeWrite`** — callback creates a secondary in T, then joined-GETs it back and observes the uncommitted row + marker; the observation is echoed into the primary's committed data and asserted after commit.

## TDD evidence

**RED** (temporarily withheld the `X-Tx-Token` echo so callbacks opened their own tx):
```
callback_txjoin_test.go:160: rolled-back secondary ... is still present (state="STORED", http 200) — callback write was NOT atomic with T
--- FAIL: TestCallback_SyncWrite_AtomicWithTransition
callback_txjoin_test.go:230: joined callback read did NOT see the uncommitted secondary write: readbackStatus=404 ...
--- FAIL: TestCallback_SyncRead_SeesUncommittedCascadeWrite
```
Both invariants correctly break without the join — the tests are load-bearing.

**GREEN** (`go test ./internal/e2e/ -run TestCallback_Sync -v`, Docker up, real Postgres):
```
--- PASS: TestCallback_SyncWrite_AtomicWithTransition (0.30s)
--- PASS: TestCallback_SyncRead_SeesUncommittedCascadeWrite (0.24s)
ok  github.com/cyoda-platform/cyoda-go/internal/e2e
```
Server logs confirm the real path: `member joined` / `member greeted` → `dispatching processor` (gRPC) → callback → failure branch `ENTITY_NOT_FOUND (404)` on the rolled-back secondary.

No regressions: ran alongside `TestWorkflowProc_ProcessorModifiesData` and
`TestWorkflowProc_UpdateWithCBD_DurablyCommitsPostCascadeState` — all PASS.
`go vet ./internal/e2e/` clean; `go build ./...` clean.

## Files changed

- `internal/e2e/callback_harness_test.go` — NEW: callback-capable compute-member harness.
- `internal/e2e/callback_txjoin_test.go` — NEW: the two SYNC callback tests.

## Self-review / concerns

- **No production code changed** — Tasks 1–9 already implement the mechanism; this task is
  the harness + proof, as the brief anticipated. No gap surfaced in the token→join path.
- **Callbacks are HTTP, not gRPC `EntityManage`.** Deliberate: gRPC has no joined-read RPC,
  and the HTTP path (Task 9 middleware) covers reads+writes uniformly. The token still
  originates from the real gRPC calc request (`cyodatxtoken`), so `JoinFromToken`/participate
  is fully exercised. The gRPC-layer (`G`) matrix cell for these scenarios is a separate task
  (`internal/grpc`); this task owns the `E` (running-backend e2e) cell.
- **Second app on the shared DB.** Model names are per-test-unique; migration re-run is a
  no-op (golang-migrate version table). Cluster disabled → single "local" node, so tokens are
  always local-join (no proxy). Multi-node forwarding is a later isolated task.
- The `#27 Task 17` skip (`TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten`)
  is CBD `startNewTxOnDispatch=true` (a different feature/matrix row) — left as-is; the harness
  here is now available should that be revisited.
- Member keep-alive is handled (replies to server keep-alives) so long dispatches won't trip
  the 30s timeout; tests complete in <0.3s regardless.

## Environment

Docker available; Postgres 17 testcontainer started successfully. Real e2e run, not skipped.

---

## Review-fix: bearer access race-safety + harness comments

### Sync approach

Replaced `bearer string` in the struct with `bearerVal atomic.Value` (stores `string`).

- **Writer** (`token()` / `bearerOnce.Do`): `h.bearerVal.Store(h.fetchToken(t))` — atomic store before any goroutine can observe the value.
- **Reader** (`callback()`, runs on the compute-member goroutine): `tok, _ := h.bearerVal.Load().(string)` — atomic load, no bare field read.

`sync/atomic.Value` provides a sequentially-consistent barrier that the race detector can see, unlike the structural happens-before (goroutine creation) that was invisible to it. The `sync.Once` is retained to ensure the fetch runs exactly once.

### Clarifying comments added

1. `callback_harness_test.go` `SetupModelWithWorkflow` (~line 313): `// workflowSampleModel is defined in workflow_test.go`
2. `callback_txjoin_test.go` `_ = failID` (~line 147): `// empty on failure — nothing to look up`

### Race-run result

```
go test -race -run TestCallback_SyncWrite ./internal/e2e/ -count=1 -v -timeout=300s
--- PASS: TestCallback_SyncWrite_AtomicWithTransition (1.59s)
PASS
ok  	github.com/cyoda-platform/cyoda-go/internal/e2e	7.688s
```

No race report. `go build ./...` and `go vet ./internal/e2e/` both clean.
