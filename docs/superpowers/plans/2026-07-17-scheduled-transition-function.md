# Scheduled-Transition Function Callout — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an application compute a scheduled transition's firing time (and optional expiry) per-entity, via a new generic **Function** callout, while paying down the callout-dispatch duplication and the compute-infra error-taxonomy drift it surfaced.

**Architecture:** Three callout shapes share one dispatch primitive (Processor mutates the entity, Criterion returns a bool, **Function** returns a declared typed value). Scheduling is the first Function consumer: a transition's `schedule.function` is invoked at arm time inside `reconcileScheduledTasks`, returns a composite `Schedule` (fire + optional expiry, absolute or relative), and the resolved time is written to the durable `ScheduledTask` row. Fail-closed: compute-infra unavailability fails the write as a retryable `503`.

**Tech Stack:** Go 1.26+, `log/slog`, CloudEvents-over-gRPC (single `startStreaming` bidi stream), memory/sqlite/postgres SPI stores, testcontainers-go for e2e, kin-openapi validator.

**Design spec:** `docs/superpowers/specs/2026-07-17-scheduled-transition-function-design.md` (section refs `§N` below point there). Issue #419, milestone v0.8.3.

## Global Constraints

- **Correctness over availability, fail closed** (`.claude/rules/correctness-over-availability.md`): a write that cannot arm a valid timer is rejected/rolled back — never committed with a missing/guessed timer.
- **Multi-node is the primary target** (`.claude/rules/multi-node-primary.md`): every callout path must be correct single-node AND cluster.
- **TDD mandatory** (`.claude/rules/tdd.md`): RED → GREEN → REFACTOR; no production code without a failing test.
- **Go conventions:** `log/slog` only; wrap errors `fmt.Errorf("...: %w", err)`; `uuid.UUID` not `string`; 4xx = full domain detail + code, 5xx = generic message + ticket UUID.
- **No issue IDs in shipped artefacts** (code comments, error messages, help topics, OpenAPI): reserve `#419` for commits/PR/this plan.
- **SPI coordinated release** (`MAINTAINING.md`): SPI tag FIRST, then pin-bump root + all 3 `plugins/*/go.mod` identically in one commit; during dev, `go.work` local composition + pseudo-version pin (never a committed `replace`).
- **Fix all deficiencies found; blast radius secondary** — the infra-503 reconciliation (Phase 2) fixes the pre-existing doc/code drift across all four infra codes, not just what the feature touches.
- **New error code → `cmd/cyoda/help/content/errors/<CODE>.md`** (TestErrCode_Parity enforces a strict bijection).
- **OpenAPI typed-but-open:** enumerate properties, never `additionalProperties:false`; reject unknown input server-side.
- **Verify before done** (`.claude/rules/tdd.md`, Gate 5): `go test ./... -v` green (incl. e2e, Docker required) + `go vet ./...`; `make race` once before PR.

## File Structure

**Phase 1 — dispatch unification (cyoda-go, behavior-preserving):**
- `internal/grpc/dispatch.go` — extract `buildEntityPayload` + `dispatchCalloutToMember`; `DispatchProcessor`/`DispatchCriteria` become thin wrappers.
- `internal/cluster/dispatch/{types.go,forwarder.go,handler.go,cluster_dispatcher.go}` — collapse per-type wire/routes into one `DispatchCalloutRequest/Response` + `/internal/dispatch/callout`.
- `internal/observability/dispatch_tracing.go` — kind-labeled metric/span.
- `docs/ARCHITECTURE.md`, `internal/cluster/scheduler_rpc.go` (doc comment) — route references.

**Phase 2 — infra-503 reconciliation (cyoda-go):**
- `internal/domain/entity/service.go` — `classifyWorkflowError` `errors.As` passthrough + `ErrNoMatchingMember`→503 mapping.
- `internal/grpc/dispatch.go`, `internal/cluster/dispatch/cluster_dispatcher.go` — mint the four infra codes as `503` `AppError`s at emit sites.

**Phase 3 — SPI (sibling `cyoda-go-spi`, then pin bump):**
- `cyoda-go-spi/types.go` — `TransitionSchedule.Function`, `ScheduleFunction`; `cyoda-go-spi/persistence.go` — `ReconcileRequest.Cancel`, reconcile godoc; `cyoda-go-spi/…/scheduled_task_store_conformance.go` — born-expired case.
- `go.mod` (root + `plugins/{memory,sqlite,postgres}/go.mod`) — pin bump.

**Phase 4 — Function callout (cyoda-go):**
- `docs/cyoda/schema/processing/EntityFunctionCalculation{Request,Response}.json`; `api/grpc/events/types.go` — generated event structs.
- `internal/grpc/members.go` — `ProcessingResponse.{Result,ResultKind}`; `internal/grpc/streaming.go` — `handleFunctionResponse` + case.
- `internal/contract/processing.go` — `DispatchFunction` + `FunctionResult`; impls in `internal/grpc/dispatch.go`, `internal/cluster/dispatch/…`, `internal/observability/dispatch_tracing.go`, `internal/skeleton/processing.go`, `internal/testing/localproc/localproc.go`.

**Phase 5 — config + validation (cyoda-go):**
- `api/openapi.yaml` + `api/generated.go` — `ScheduleFunctionDto`, `TransitionScheduleDto.function`.
- `internal/domain/workflow/validate.go` — conditional `delayMs`, XOR, function rules.

**Phase 6 — arm wiring (cyoda-go + plugins):**
- `plugins/{memory,sqlite,postgres}/scheduled_task_store.go` — `Cancel`-set delete branch in `ReconcileForEntity`.
- `internal/domain/workflow/arm.go` — invoke `DispatchFunction`, resolve `Schedule`, born-expired cancel, malformed→500.

**Phase 7 — observability:** `internal/scheduler/executor.go` — `slog.Warn`→`slog.Error`.

**Phase 8 — docs:** `cmd/cyoda/help/content/{workflows.md,errors/SCHEDULE_FUNCTION_INVALID_RESULT.md}`, `docs/cloud-parity/scheduled-transitions.md`, `docs/workflow-schema-versioning.md`, `README.md`, `CHANGELOG.md`, `COMPATIBILITY.md`, compute-node Function schema docs.

**Phase 9 — coverage:** `internal/e2e/…`, `e2e/parity/scheduledtransition/…` + `registry.go`, `internal/grpc/…` envelope tests.

**Dependency order:** Phase 1 → 2 (neither needs SPI) → 3 (SPI, before consumers) → 4 → 5 → 6 → 7 → 8 → 9. Phases 1 and 2 each land green independently; 8/9 interleave with their features but are called out separately for the coverage-gap check.

**Baseline before any task:** `go build ./... && go vet ./...` clean; `go test -short ./internal/grpc/... ./internal/cluster/... ./internal/domain/... -v` green. Docker up for e2e tasks.

---

## Phase 1 — Dispatch unification (behavior-preserving; land green before any Function code)

Extract one dispatch primitive so Processor, Criterion, and (later) Function share transport. **No behavior change** — existing tests are the guard. Commit each task.

### Task 1.1: Extract `buildEntityPayload`

**Files:**
- Modify: `internal/grpc/dispatch.go` (the `AttachEntity` block, currently duplicated at ~100-118 in `DispatchProcessor` and ~251-269 in `DispatchCriteria`)
- Test: `internal/grpc/dispatch_test.go`

**Interfaces:**
- Produces: `func buildEntityPayload(entity *spi.Entity) *events.DataPayloadJson`

- [ ] **Step 1: Write the failing test**

```go
// dispatch_test.go
func TestBuildEntityPayload(t *testing.T) {
	e := &spi.Entity{Meta: spi.EntityMeta{
		ID: "e1", State: "S1", TransactionID: "tx1",
		ModelRef:      spi.ModelRef{EntityName: "order", ModelVersion: "3"},
		CreationDate:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		LastModifiedDate: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}, Data: []byte(`{"k":"v"}`)}
	p := buildEntityPayload(e)
	if p.Type != "JSON" {
		t.Fatalf("Type = %q, want JSON", p.Type)
	}
	mk := p.Meta["modelKey"].(map[string]any)
	if mk["version"] != 3 {
		t.Fatalf("version = %v, want int 3", mk["version"])
	}
	if p.Meta["state"] != "S1" {
		t.Fatalf("state = %v", p.Meta["state"])
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`undefined: buildEntityPayload`)

Run: `go test ./internal/grpc/ -run TestBuildEntityPayload -v`

- [ ] **Step 3: Extract the function** (move the exact block verbatim; it is currently identical in both dispatchers)

```go
// buildEntityPayload builds the DataPayloadJson attached to a calc request when
// AttachEntity is set. Shared by every callout shape.
func buildEntityPayload(entity *spi.Entity) *events.DataPayloadJson {
	versionInt := 0
	fmt.Sscanf(entity.Meta.ModelRef.ModelVersion, "%d", &versionInt)
	return &events.DataPayloadJson{
		Type: "JSON",
		Data: json.RawMessage(entity.Data),
		Meta: map[string]any{
			"id": entity.Meta.ID,
			"modelKey": map[string]any{
				"name":    entity.Meta.ModelRef.EntityName,
				"version": versionInt,
			},
			"state":          entity.Meta.State,
			"creationDate":   entity.Meta.CreationDate.Format(time.RFC3339Nano),
			"lastUpdateTime": entity.Meta.LastModifiedDate.Format(time.RFC3339Nano),
			"transactionId":  entity.Meta.TransactionID,
		},
	}
}
```

Replace both inline blocks with `req.Payload = buildEntityPayload(entity)` inside their `if …AttachEntity {` guards.

- [ ] **Step 4: Run tests — expect PASS** (new test + all existing)

Run: `go test ./internal/grpc/ -v`
Expected: PASS, no behavior change.

- [ ] **Step 5: Commit** — `git commit -am "refactor(grpc): extract buildEntityPayload shared by callout shapes"`

### Task 1.2: Extract `dispatchCalloutToMember`

**Files:**
- Modify: `internal/grpc/dispatch.go`
- Test: `internal/grpc/dispatch_test.go`

**Interfaces:**
- Produces: `func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, ceType string, req any, timeoutMs int64, label string) (*ProcessingResponse, error)` — does requestID gen, `NewCloudEvent`+auth+tx-token, `TrackRequest`/`Send`, timeout select, warnings propagation. Returns the raw `*ProcessingResponse` on `Success`, or an error (`ErrNoMatchingMember` is NOT its concern — caller resolves the member first). Timeout returns a `*common.AppError` (Phase 2 makes it 503; here keep the existing `fmt.Errorf` so this task stays behavior-preserving).
- Consumes: `buildEntityPayload` (Task 1.1).

- [ ] **Step 1: Write the failing test** — assert the primitive sends a CE and returns the tracked response.

```go
func TestDispatchCalloutToMember_SuccessAndTimeout(t *testing.T) {
	d, member := newTestDispatcherWithMember(t) // existing test helper; if absent, build via NewProcessorDispatcher + registry.Add
	// success: answer the tracked request
	go respondToNextRequest(member, &ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{}}`)})
	resp, err := d.dispatchCalloutToMember(context.Background(), member, EntityProcessorCalculationRequest, map[string]any{"requestId": "x"}, 1000, "processor")
	if err != nil || !resp.Success {
		t.Fatalf("success path: resp=%v err=%v", resp, err)
	}
	// timeout: nobody answers
	_, err = d.dispatchCalloutToMember(context.Background(), member, EntityProcessorCalculationRequest, map[string]any{"requestId": "y"}, 20, "processor")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`undefined: dispatchCalloutToMember`).

- [ ] **Step 3: Implement the primitive** (lift the shared sequence out of `DispatchProcessor` lines ~120-167):

```go
func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, ceType string, req any, timeoutMs int64, label string) (*ProcessingResponse, error) {
	requestID := requestIDFrom(req) // helper: read the RequestID the caller already set, else mint; simplest: caller sets it and passes it in — see note
	ce, err := NewCloudEvent(ceType, req)
	if err != nil {
		return nil, fmt.Errorf("failed to build %s cloud event: %w", label, err)
	}
	AttachAuthContext(ctx, ce)
	AttachTxToken(ce, d.resolveTxToken(ctx, txIDFromReq(req)))
	slog.Debug("dispatch request", "pkg", "grpc", "requestId", requestID, "payload", logging.PayloadPreview([]byte(ce.GetTextData()), 200))

	ch := member.TrackRequest(requestID)
	if err := member.Send(ce); err != nil {
		slog.Error("failed to send to member", "pkg", "grpc", "memberId", member.ID, "error", err)
		return nil, fmt.Errorf("failed to send %s request: %w", label, err)
	}
	if timeoutMs <= 0 {
		timeoutMs = defaultResponseTimeoutMs
	}
	select {
	case resp := <-ch:
		if resp != nil {
			for _, w := range resp.Warnings {
				common.AddWarning(ctx, fmt.Sprintf("%s %s: %s", label, requestID, w))
			}
		}
		if resp == nil || !resp.Success {
			errMsg := label + " returned failure"
			if resp != nil && resp.Error != "" {
				errMsg = resp.Error
				common.AddError(ctx, fmt.Sprintf("%s: %s", label, errMsg))
			}
			return nil, fmt.Errorf("%s dispatch failed: %s", label, errMsg)
		}
		return resp, nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		slog.Error("dispatch timeout", "pkg", "grpc", "label", label, "requestId", requestID, "timeoutMs", timeoutMs)
		return nil, fmt.Errorf("%s dispatch timed out after %dms", label, timeoutMs)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

**Simplification note:** to avoid reflection, keep requestID/txID resolution in the *wrappers* and change the signature to `dispatchCalloutToMember(ctx, member, ceType, req any, requestID, txID string, timeoutMs int64, label string)`. The wrapper builds `req` (already setting `RequestID`), then passes `requestID`/`txID` explicitly. Use this signature — it is cleaner than reflecting fields out of `req`.

- [ ] **Step 4: Rewrite `DispatchProcessor` and `DispatchCriteria` as wrappers.** Each keeps its own: member resolution (`FindByTags`, `ErrNoMatchingMember`), request-struct build (`buildEntityPayload` on `AttachEntity`), the `dispatchCalloutToMember` call, and its own response parse (`applyProcessorResponse` / read `resp.Matches`,`resp.Reason`). Delete the duplicated transport code from both.

- [ ] **Step 5: Run tests — expect PASS** (all existing grpc + new).

Run: `go test ./internal/grpc/ -v`

- [ ] **Step 6: Commit** — `git commit -am "refactor(grpc): extract dispatchCalloutToMember; processor/criteria ride it"`

### Task 1.3: Collapse the cluster peer wire to one generic route

**Files:**
- Modify: `internal/cluster/dispatch/types.go` (replace `DispatchProcessorRequest/Response` + `DispatchCriteriaRequest/Response` with `DispatchCalloutRequest/Response`)
- Modify: `internal/cluster/dispatch/forwarder.go` (`ForwardProcessor`/`ForwardCriteria` → one `ForwardCallout`; `HTTPForwarder` uses `/internal/dispatch/callout`)
- Modify: `internal/cluster/dispatch/handler.go` (`handleProcessor`/`handleCriteria` → one `handleCallout`; register only `/internal/dispatch/callout`)
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (`DispatchProcessor`/`DispatchCriteria` build a `DispatchCalloutRequest{Kind: …}` and call `ForwardCallout`)
- Test: `internal/cluster/dispatch/*_test.go` (update route/struct references)

**Interfaces:**
- Produces: `type DispatchCalloutRequest struct { Kind string; Entity []byte; EntityMeta …; WorkflowName, TransitionName, TxID string; TenantID spi.TenantID; Tags string; UserID string; Roles []string; TxToken string; Processor *spi.ProcessorDefinition; Criterion json.RawMessage; Target, ProcessorName string; Function *ScheduleFunctionRef }` — union: `Processor` set for `Kind=="processor"`, `Criterion`/`Target`/`ProcessorName` for `Kind=="criteria"`, `Function` for `Kind=="function"` (Phase 4). `type DispatchCalloutResponse struct { Success bool; Error *DispatchError; EntityData []byte; Matches *bool; Reason string; Result json.RawMessage; ResultKind string; Warnings []string }` — union mirroring `ProcessingResponse`.
- Produces: `ForwardCallout(ctx, peer, req DispatchCalloutRequest) (*DispatchCalloutResponse, error)`.

- [ ] **Step 1: Update the failing tests first.** In `internal/cluster/dispatch/*_test.go`, replace references to the per-type structs/routes with the generic ones. Run `go test ./internal/cluster/dispatch/ -v` → FAIL (compile).

- [ ] **Step 2: Introduce the generic structs** in `types.go`; delete the four per-type structs.

- [ ] **Step 3: One forwarder + handler.** `HTTPForwarder.ForwardCallout` POSTs to `peer + "/internal/dispatch/callout"` using the existing generic `forward()` transport (unchanged). `handler.go`: `handleCallout` does `verifyRequest` → unmarshal `DispatchCalloutRequest` → `buildContext`+`WithTxToken` → `switch req.Kind { case "processor": h.local.DispatchProcessor(...); case "criteria": h.local.DispatchCriteria(...) }` → map result into `DispatchCalloutResponse`. Register only `/internal/dispatch/callout`.

- [ ] **Step 4: `ClusterDispatcher` builds generic requests.** `DispatchProcessor`/`DispatchCriteria` set `Kind` and the relevant union fields, call `ForwardCallout`, and map `DispatchCalloutResponse` back. Keep `isNoMatchingMember`, `findPeerWithPolling`, and the owner-tx-token mint unchanged.

- [ ] **Step 5: Run — expect PASS.** `go test ./internal/cluster/... -v`

- [ ] **Step 6: Commit** — `git commit -am "refactor(cluster): collapse peer dispatch to one /internal/dispatch/callout route"`

### Task 1.4: Kind-labeled tracing + route-reference docs

**Files:**
- Modify: `internal/observability/dispatch_tracing.go` (metric attribute driven by a `kind` string, not per-method constants)
- Modify: `docs/ARCHITECTURE.md` (route refs `~599,612-613,678,1606`), `internal/cluster/scheduler_rpc.go:25-26` (doc comment)

- [ ] **Step 1:** Adjust the tracing decorator so `DispatchProcessor`/`DispatchCriteria` pass their kind to a shared `record(ctx, kind, fn)`; existing `typeProcessor`/`typeCriteria` attributes preserved. Run `go test ./internal/observability/ -v` → PASS (add an assertion if a test exists; else this is covered by compilation + no behavior change).
- [ ] **Step 2:** Update the two doc references to name the single `/internal/dispatch/callout` route. (Docs-only; no test.)
- [ ] **Step 3: Commit** — `git commit -am "refactor: kind-labeled dispatch tracing; update route docs"`

---

## Phase 2 — Uniform infra-503 reconciliation (fixes pre-existing doc/code drift; §7)

Mint the four compute-infra codes (`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`, `DISPATCH_FORWARD_FAILED`, `COMPUTE_MEMBER_DISCONNECTED`) as `503` retryable `AppError`s at their emit sites, and pass them through `classifyWorkflowError` via `errors.As`. Applies uniformly to processor + criterion (Function inherits it in Phase 6). All four codes already exist with topics.

### Task 2.1: `classifyWorkflowError` passthrough + no-member 503 mapping

**Files:**
- Modify: `internal/domain/entity/service.go` (`classifyWorkflowError` ~:1937-1951)
- Test: `internal/e2e/` (a running-backend test) or `internal/domain/entity/service_test.go` if a unit seam exists

**Interfaces:**
- Consumes: `grpc.ErrNoMatchingMember`; `common.ErrCodeNoComputeMemberForTag`, `common.Operational`, `.AsRetryable()`.

- [ ] **Step 1: Write the failing e2e test** — a workflow whose processor tags match no member; POST an entity that fires it; assert HTTP `503` + code `NO_COMPUTE_MEMBER_FOR_TAG` + retryable. (New coverage — no existing test asserts 400 here.)

```go
// internal/e2e/dispatch_infra_error_test.go
func TestProcessorNoMember_Returns503(t *testing.T) {
	// import a workflow with an automated transition carrying a processor
	// whose calculationNodesTags match no connected member, POST an entity,
	// assert resp.StatusCode == 503 and body code == "NO_COMPUTE_MEMBER_FOR_TAG".
}
```

- [ ] **Step 2: Run — expect FAIL** (currently 400 `WORKFLOW_FAILED`).

- [ ] **Step 3: Add the passthrough + mapping** at the top of `classifyWorkflowError`:

```go
var appErr *common.AppError
if errors.As(err, &appErr) {
	return appErr // minted 503s (timeout/forward/disconnect) propagate intact
}
if errors.Is(err, grpc.ErrNoMatchingMember) {
	return common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag, err.Error()).AsRetryable()
}
// …existing sentinel checks and default 400 WORKFLOW_FAILED follow…
```

- [ ] **Step 4: Run — expect PASS.** `go test ./internal/e2e/ -run TestProcessorNoMember_Returns503 -v`
- [ ] **Step 5: Commit** — `git commit -am "fix(errors): no-member dispatch failure surfaces 503 NO_COMPUTE_MEMBER_FOR_TAG"`

### Task 2.2: Timeout → 503 `DISPATCH_TIMEOUT` (in the primitive)

**Files:** Modify `internal/grpc/dispatch.go` (`dispatchCalloutToMember` timeout branch); Test `internal/grpc/dispatch_test.go`.

- [ ] **Step 1:** Test: a member that never answers → `dispatchCalloutToMember` returns an error for which `errors.As(&appErr)` gives `appErr.Code == DISPATCH_TIMEOUT`, status 503, retryable. Run → FAIL.
- [ ] **Step 2:** In the timeout branch, replace `fmt.Errorf("%s dispatch timed out…")` with `common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout, fmt.Sprintf("%s dispatch timed out after %dms", label, timeoutMs)).AsRetryable()`.
- [ ] **Step 3:** Run → PASS (`go test ./internal/grpc/ -v`). **Step 4:** Commit — `git commit -am "fix(grpc): dispatch timeout surfaces 503 DISPATCH_TIMEOUT"`.

### Task 2.3: Cluster no-peer + forward-failure 503

**Files:** Modify `internal/cluster/dispatch/cluster_dispatcher.go` (`findPeerWithPolling` no-peer ~:191; peer-forward failure ~:103,162); Test `internal/cluster/dispatch/*_test.go` (isolated multi-node).

- [ ] **Step 1:** Tests: (a) no peer has the tag → 503 `NO_COMPUTE_MEMBER_FOR_TAG`; (b) peer selected but forward transport fails → 503 `DISPATCH_FORWARD_FAILED`. Both retryable. Run → FAIL.
- [ ] **Step 2:** Replace the `fmt.Errorf` embeddings with `common.Operational(503, common.ErrCodeNoComputeMemberForTag, …).AsRetryable()` and `common.Operational(503, common.ErrCodeDispatchForwardFailed, …).AsRetryable()` respectively. **Do not** wrap `ErrNoMatchingMember` such that `isNoMatchingMember` in the *local* path breaks — the mint is at the terminal no-peer decision, after forwarding was attempted.
- [ ] **Step 3:** Run → PASS. **Step 4:** Commit — `git commit -am "fix(cluster): no-peer and forward-failure surface retryable 503"`.

### Task 2.4: Member-disconnect mid-request → 503 `COMPUTE_MEMBER_DISCONNECTED`

**Files:** Modify the disconnect/`FailAllPending` path (`internal/grpc/members.go` + wherever the closed/failed channel is observed in `dispatchCalloutToMember`); Test `internal/grpc/*_test.go`.

- [ ] **Step 1:** Test: track a request, disconnect the member (trigger `FailAllPending`), assert the dispatch returns a 503 `COMPUTE_MEMBER_DISCONNECTED` (retryable) rather than a nil/closed-channel error. Run → FAIL.
- [ ] **Step 2:** Have `FailAllPending` deliver a sentinel `ProcessingResponse{Success:false, Error:"member disconnected", disconnected:true}` (or close with a distinguishable signal); in `dispatchCalloutToMember`, map that to `common.Operational(503, common.ErrCodeComputeMemberDisconnected, …).AsRetryable()`.
- [ ] **Step 3:** Run → PASS. **Step 4:** Commit — `git commit -am "fix(grpc): member disconnect mid-request surfaces 503 COMPUTE_MEMBER_DISCONNECTED"`.

**Phase 2 gate:** `go test ./internal/grpc/... ./internal/cluster/... ./internal/e2e/ -v` green. The four infra conditions now return retryable 503 with their documented codes, uniformly for processor and criterion.

> **Note on error-code constants:** the exact Go identifiers for the four infra codes live in `internal/common/error_codes.go` (~56-59); the review confirmed `ErrCodeNoComputeMemberForTag`. Verify the other three identifiers there before use rather than guessing.

---

## Phase 3 — SPI additions + coordinated pin bump (`cyoda-go-spi` first)

Per `MAINTAINING.md`, SPI changes land in the sibling repo (`../cyoda-go-spi`, composed locally via `go.work`), then the pin is bumped. These are **additive** (patch bump).

### Task 3.1: SPI types — `ScheduleFunction`, `TransitionSchedule.Function`, `ReconcileRequest.Cancel`

**Files (in `../cyoda-go-spi`):**
- Modify: `types.go` (`TransitionSchedule` ~:250; add `ScheduleFunction`)
- Modify: `persistence.go` (`ReconcileRequest` ~:32; reconcile godoc ~:29-31,53-55)
- Modify: the `scheduled_task_store_conformance.go` suite
- Test: SPI unit tests

**Interfaces:**
- Produces:
```go
// ScheduleFunction configures a Function callout that computes a scheduled
// transition's firing time (and optional expiry) per entity. Mutually
// exclusive with TransitionSchedule.DelayMs.
type ScheduleFunction struct {
	Name                 string `json:"name"`
	ResultKind           string `json:"resultKind"`           // must be "Schedule"
	CalculationNodesTags string `json:"calculationNodesTags"`
	AttachEntity         bool   `json:"attachEntity"`         // default true (see DTO default)
	Context              string `json:"context,omitempty"`
	ResponseTimeoutMs    int64  `json:"responseTimeoutMs,omitempty"`
}
// TransitionSchedule gains:
Function *ScheduleFunction `json:"function,omitempty"`
// ReconcileRequest gains:
Cancel []string `json:"cancel,omitempty"` // task IDs to delete this tx (born-expired)
```

- [ ] **Step 1:** In `../cyoda-go-spi`, write a failing conformance test asserting `ReconcileForEntity` with a populated `Cancel` deletes those rows and returns them distinctly from the `SourceState`-mismatch cancels. Run the SPI suite → FAIL.
- [ ] **Step 2:** Add the two struct fields + `ScheduleFunction`; update the reconcile godoc to document the explicit `Cancel` set (no longer "only `SourceState != CurrentState`").
- [ ] **Step 3:** Extend `scheduled_task_store_conformance.go` with the born-expired-cancel case (all backends run it).
- [ ] **Step 4:** Run SPI tests → PASS. Commit in `../cyoda-go-spi`.
- [ ] **Step 5:** Coordinated release: per `MAINTAINING.md`, during dev keep the local `go.work` composition; the SPI tag + pseudo-version pin is cut at milestone integration (Task 3.2). Do **not** add a committed `replace`.

### Task 3.2: Bump the SPI pin (root + 3 plugins, identical)

**Files:** `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`.

- [ ] **Step 1:** Update the `cyoda-go-spi` pseudo-version identically in all four files to the new SPI HEAD.
- [ ] **Step 2:** `go mod tidy` in each module; `make check-spi-pin-sync`.
- [ ] **Step 3:** `make test-all` (root + plugins) green.
- [ ] **Step 4: Commit** — `git commit -am "chore(spi): bump pin for ScheduleFunction + ReconcileRequest.Cancel (#419)"`

---

## Phase 4 — Function callout (wire + dispatch)

### Task 4.1: `EntityFunctionCalculation{Request,Response}` schemas + event types

**Files:**
- Create: `docs/cyoda/schema/processing/EntityFunctionCalculationRequest.json`, `…Response.json` (mirror the Criteria schemas; response has `result` (object) + `resultKind` (string) instead of `matches`/`reason`)
- Modify: `api/grpc/events/types.go` (add `EntityFunctionCalculationRequestJson`, `EntityFunctionCalculationResponseJson` mirroring the Criteria structs; response fields `RequestID, EntityID, Success bool, Result json.RawMessage, ResultKind string, Warnings []string, Error *…EventJsonError`)
- Modify: the CloudEvent type-constant list (add `EntityFunctionCalculationRequest`/`Response` alongside the existing `EntityCriteriaCalculation*` constants)
- Test: `api/grpc/events/…_test.go` round-trip marshal/unmarshal

- [ ] **Step 1:** Failing test: marshal an `EntityFunctionCalculationResponseJson{Result: json.RawMessage(`{"fireAt":1}`), ResultKind:"Schedule", Success:true}` and unmarshal it back; assert fields. Run → FAIL.
- [ ] **Step 2:** Add the schemas + Go structs + CE type constants.
- [ ] **Step 3:** Run → PASS (`go test ./api/grpc/events/ -v`). **Step 4:** Commit.

### Task 4.2: `ProcessingResponse.{Result,ResultKind}` + inbound handler

**Files:** Modify `internal/grpc/members.go` (add `Result json.RawMessage`, `ResultKind string` to `ProcessingResponse`); `internal/grpc/streaming.go` (receive-loop switch + `handleFunctionResponse`); Test `internal/grpc/streaming_test.go`.

- [ ] **Step 1:** Failing test: feed an inbound `EntityFunctionCalculationResponse` CE to the stream handler; assert the tracked `ProcessingResponse` carries `Result`/`ResultKind`/`Success`. Run → FAIL.
- [ ] **Step 2:** Add the two fields; add `case EntityFunctionCalculationResponse:` → `handleFunctionResponse` (copy `handleCriteriaResponse` shape, pull `result`/`resultKind` instead of `matches`/`reason`, incl. `Error`/`Retryable`/`Warnings`).
- [ ] **Step 3:** Run → PASS. **Step 4:** Commit.

### Task 4.3: `DispatchFunction` on the interface + all implementors

**Files:**
- Modify: `internal/contract/processing.go` (add method + `FunctionResult`)
- Modify: `internal/grpc/dispatch.go` (real impl via `dispatchCalloutToMember`)
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (`Kind:"function"` generic forward), `handler.go` (`case "function"`)
- Modify: `internal/observability/dispatch_tracing.go` (`typeFunction`), `internal/skeleton/processing.go` (no-op stub), `internal/testing/localproc/localproc.go` (map + `RegisterFunction` + counter), and any test mocks of `ExternalProcessingService`
- Test: `internal/grpc/dispatch_test.go`, `internal/cluster/dispatch/*_test.go`

**Interfaces:**
- Produces:
```go
type FunctionResult struct {
	Kind  string
	Value json.RawMessage
}
// ExternalProcessingService gains:
DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) (FunctionResult, error)
```

- [ ] **Step 1:** Failing test (grpc): register a member that answers an `EntityFunctionCalculationResponse` with `resultKind:"Schedule"`, `result:{"fireAfterMs":1000}`; call `DispatchFunction`; assert returned `FunctionResult{Kind:"Schedule", Value:…}`. Also assert `DispatchFunction` with unmatched tags returns `ErrNoMatchingMember`. Run → FAIL.
- [ ] **Step 2:** Implement grpc `DispatchFunction`: `FindByTags` (→ `ErrNoMatchingMember`), build `EntityFunctionCalculationRequestJson` (attach entity iff `fn.AttachEntity`, `Parameters=fn.Context`), `requestID`/`txID`, call `dispatchCalloutToMember(…, EntityFunctionCalculationRequest, req, requestID, txID, fn.ResponseTimeoutMs, "function")`, then `return FunctionResult{Kind: resp.ResultKind, Value: resp.Result}, nil`.
- [ ] **Step 3:** Add to the interface; implement the cluster path (build `DispatchCalloutRequest{Kind:"function", Function:…}`, `ForwardCallout`, map `DispatchCalloutResponse.{Result,ResultKind}` back); add `case "function"` to `handler.go`; add stubs to tracing/skeleton/localproc/mocks.
- [ ] **Step 4:** Run → PASS (`go test ./internal/grpc/... ./internal/cluster/... ./internal/contract/... -v`). **Step 5:** Commit — `git commit -am "feat(callout): add generic Function dispatch across single-node and cluster (#419)"`.

---

## Phase 5 — `schedule.function` config + validation

### Task 5.1: OpenAPI `ScheduleFunctionDto` + `TransitionScheduleDto.function`

**Files:** Modify `api/openapi.yaml` (`TransitionScheduleDto` ~:9967; add `ScheduleFunctionDto`), `api/generated.go` (regenerate or hand-add the mirror struct — note: spec is `//go:embed`-ed, generated types are hand-maintained; add `ScheduleFunctionDto` + `Function *ScheduleFunctionDto` on `TransitionScheduleDto`). Test: `internal/e2e/openapivalidator/` + oasdiff gate.

- [ ] **Step 1:** Add `ScheduleFunctionDto` (props `name`, `resultKind` enum `["Schedule"]`, `calculationNodesTags`, `attachEntity` default true, `context`, `responseTimeoutMs`; **no** `additionalProperties:false`), and `function: $ref` on `TransitionScheduleDto` (keep both `delayMs` and `function` optional; document the XOR in prose).
- [ ] **Step 2:** Run `go test ./internal/oasdiffcheck/ -v` → PASS (additive property, non-breaking). Add the generated Go mirror struct.
- [ ] **Step 3:** `go build ./...` clean. **Step 4:** Commit.

### Task 5.2: Import validation

**Files:** Modify `internal/domain/workflow/validate.go` (`validateWorkflowStructure`, the schedule block ~:336-345); Test `internal/domain/workflow/validate_test.go`.

- [ ] **Step 1:** Failing tests, one per rule: (a) `function`+`delayMs` both set → `VALIDATION_FAILED`; (b) `function`+`manual:true` → `VALIDATION_FAILED` (inherited by `:336`); (c) function-only (no `delayMs`) → **accepted** (this is the R2-B2 regression — currently rejected by the `delayMs<=0` check at `:341`); (d) `resultKind != "Schedule"` → `VALIDATION_FAILED`; (e) missing `name`/`calculationNodesTags` → `VALIDATION_FAILED`. Run → FAIL.
- [ ] **Step 2:** Rework the schedule block:

```go
if tr.Schedule != nil {
	hasDelay := tr.Schedule.DelayMs > 0
	hasFn := tr.Schedule.Function != nil
	if tr.Manual { /* existing "manual and scheduled are mutually exclusive" */ }
	if hasDelay == hasFn { // both or neither
		return validationErr("exactly one of schedule.delayMs or schedule.function is required")
	}
	if hasFn {
		f := tr.Schedule.Function
		if f.ResultKind != "Schedule" {
			return validationErr(`schedule.function.resultKind must be "Schedule"`)
		}
		if f.Name == "" || f.CalculationNodesTags == "" {
			return validationErr("schedule.function requires name and calculationNodesTags")
		}
	}
	// delayMs<=0 check now applies ONLY when !hasFn
}
```

- [ ] **Step 3:** Run → PASS (`go test ./internal/domain/workflow/ -run Validate -v`). **Step 4:** Commit.

---

## Phase 6 — Arm-time engine wiring (the feature core)

### Task 6.1: `Cancel`-set delete branch in all three stores

**Files:** Modify `plugins/{memory,sqlite,postgres}/scheduled_task_store.go` (`ReconcileForEntity`); Test: covered by the SPI conformance suite (Task 3.1) run per backend + a targeted unit test each.

**Per-store delta (same shape, three implementations):** inside `ReconcileForEntity`, after the existing Upsert(Arm) + `SourceState != CurrentState` cancel logic, delete each id in `req.Cancel` **within the same tx**; return those deletes **separately** from (or flagged distinctly in) the `cancelled` result so the caller audits them as `EXPIRE`, not `CANCEL`. Deleting a nonexistent id is a no-op (`Delete` returns `(false, nil)`).

- [ ] **Step 1:** Per backend, a failing test: `ReconcileForEntity` with `Arm: nil, Cancel: []string{id}` where `id` was previously armed in the current state → row deleted; the returned "cancelled via SourceState" set does **not** include `id`. Run → FAIL (field/logic absent).
- [ ] **Step 2:** Implement the delete branch in memory, then sqlite, then postgres (identical logic; SQL `DELETE … WHERE id = $1` inside the tx-scoped store for the latter two).
- [ ] **Step 3:** Run `go test ./plugins/memory/... ./plugins/sqlite/... ./plugins/postgres/... -run Reconcile -v` → PASS (postgres needs Docker). **Step 4:** Commit.

### Task 6.2: Invoke the Function, resolve `Schedule`, born-expired, malformed

**Files:** Modify `internal/domain/workflow/arm.go` (`reconcileScheduledTasks`); Create `internal/domain/workflow/schedule_function.go` (resolution helpers); Test `internal/domain/workflow/arm_test.go`, `schedule_function_test.go`.

**Interfaces:**
- Produces: `func resolveSchedule(raw json.RawMessage, armMs int64) (scheduledTime int64, timeoutMs *int64, bornExpired bool, err error)` — parses the `Schedule` value, applies §5.2/§5.3. `err` is a `*common.AppError` (500 `SCHEDULE_FUNCTION_INVALID_RESULT`) for malformed input.

- [ ] **Step 1: Write the resolution unit tests first** (pure function, no I/O — fastest RED):

```go
func TestResolveSchedule(t *testing.T) {
	const arm = 1_000_000
	cases := []struct {
		name       string
		raw        string
		wantSched  int64
		wantTO     *int64 // nil = none
		wantBorn   bool
		wantErrACode string // "" = no error
	}{
		{"abs fire", `{"fireAt":2000000}`, 2000000, nil, false, ""},
		{"rel fire", `{"fireAfterMs":5000}`, 1005000, nil, false, ""},
		{"abs fire + abs expire", `{"fireAt":2000000,"expireAt":2000600}`, 2000000, ptr(int64(600)), false, ""},
		{"rel fire + rel expire", `{"fireAfterMs":5000,"expireAfterMs":600}`, 1005000, ptr(int64(600)), false, ""},
		{"past fire ok", `{"fireAt":500000}`, 500000, nil, false, ""},
		{"born expired abs", `{"fireAt":2000000,"expireAt":1999999}`, 0, nil, true, ""},
		{"both fire fields", `{"fireAt":1,"fireAfterMs":2}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"no fire field", `{"expireAfterMs":600}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"both expiry fields", `{"fireAt":2000000,"expireAt":3,"expireAfterMs":4}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"non-numeric", `{"fireAt":"soon"}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, to, born, err := resolveSchedule(json.RawMessage(c.raw), arm)
			// assert against c.* ; when wantErrACode != "" assert errors.As gives that code + status 500
		})
	}
}
```

- [ ] **Step 2: Run → FAIL** (`undefined: resolveSchedule`).

- [ ] **Step 3: Implement `resolveSchedule`** in `schedule_function.go`:

```go
func resolveSchedule(raw json.RawMessage, armMs int64) (int64, *int64, bool, error) {
	var s struct {
		FireAt       *int64 `json:"fireAt"`
		FireAfterMs  *int64 `json:"fireAfterMs"`
		ExpireAt     *int64 `json:"expireAt"`
		ExpireAfterMs *int64 `json:"expireAfterMs"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return 0, nil, false, invalidResult("malformed Schedule result: %v", err)
	}
	if (s.FireAt == nil) == (s.FireAfterMs == nil) {
		return 0, nil, false, invalidResult("exactly one of fireAt/fireAfterMs required")
	}
	if s.ExpireAt != nil && s.ExpireAfterMs != nil {
		return 0, nil, false, invalidResult("at most one of expireAt/expireAfterMs")
	}
	sched := deref(s.FireAt)
	if s.FireAfterMs != nil {
		sched = armMs + *s.FireAfterMs
	}
	var expiry *int64
	switch {
	case s.ExpireAt != nil:
		expiry = s.ExpireAt
	case s.ExpireAfterMs != nil:
		v := sched + *s.ExpireAfterMs
		expiry = &v
	}
	if expiry != nil && *expiry <= sched {
		return 0, nil, true, nil // born expired
	}
	var timeoutMs *int64
	if expiry != nil {
		v := *expiry - sched
		timeoutMs = &v
	}
	return sched, timeoutMs, false, nil
}

func invalidResult(format string, a ...any) error {
	return common.Internal(common.ErrCodeScheduleFunctionInvalidResult, fmt.Sprintf(format, a...))
}
```

(`common.Internal` → 500 + ticket. **Add the constant `ErrCodeScheduleFunctionInvalidResult` in `internal/common/error_codes.go` AND create its topic `cmd/cyoda/help/content/errors/SCHEDULE_FUNCTION_INVALID_RESULT.md` in this same task/commit**, so `TestErrCode_Parity` stays green — run `go test ./... -run TestErrCode_Parity` before committing 6.2. Task 8.1 then only reviews/refines the topic prose.)

- [ ] **Step 4: Run → PASS** (`go test ./internal/domain/workflow/ -run ResolveSchedule -v`).

- [ ] **Step 5: Wire it into `reconcileScheduledTasks`.** For each scheduled transition on the current state: if `tr.Schedule.Function != nil`, dispatch and resolve; else the existing `DelayMs` arithmetic. Tx-gate scoped (resume immediately). Born-expired → add `taskID` to the reconcile `Cancel` set and emit `EXPIRE`; malformed/dispatch-error → return the error (fails the write).

```go
var armMs = e.now().UnixMilli()
// inside the per-transition loop, replacing the plain nowMs+DelayMs for Function transitions:
if tr.Schedule.Function != nil {
	resume := txgate.Suspend(ctx)
	res, derr := e.extProc.DispatchFunction(ctx, entity, *tr.Schedule.Function, wf.Name, tr.Name, txID)
	resume() // BEFORE any tx-buffer write (§6)
	if derr != nil {
		return derr // 503 (infra) or propagated; fails the write, fail-closed
	}
	if res.Kind != "Schedule" {
		return invalidResult("function %q returned resultKind %q, want Schedule", tr.Schedule.Function.Name, res.Kind)
	}
	sched, timeoutMs, born, rerr := resolveSchedule(res.Value, armMs)
	if rerr != nil {
		return rerr
	}
	id := taskID(entity.Meta.TenantID, entity.Meta.ID, state, tr.Name)
	if born {
		cancelIDs = append(cancelIDs, id)             // deleted in the same tx (Task 6.1)
		bornExpired = append(bornExpired, cancelAudit{id: id, transition: tr.Name})
		continue
	}
	arm = append(arm, spi.ScheduledTask{
		ID: id, TenantID: entity.Meta.TenantID, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: sched, TimeoutMs: timeoutMs, EntityID: entity.Meta.ID,
		ModelName: entity.Meta.ModelRef.EntityName, ModelVersion: modelVersion,
		Transition: tr.Name, SourceState: state, ArmedAt: armMs,
	})
	continue
}
// …existing DelayMs path unchanged…
```

Pass `Cancel: cancelIDs` into `sts.ReconcileForEntity`, and after it, emit `SCHEDULED_TRANSITION_EXPIRE` for each `bornExpired` entry (not `CANCEL`).

- [ ] **Step 6: Write the arm integration tests** (memory backend, fake `extProc`): absolute fire arms at `fireAt`; relative arms at `armMs+fireAfterMs`; expiry stored as `TimeoutMs`; born-expired does **not** arm and cancels a pre-existing row + emits `EXPIRE`; dispatch failure fails the write; malformed → 500. Run → GREEN.
- [ ] **Step 7: Commit** — `git commit -am "feat(workflow): compute scheduled firing time via arm-time Function callout (#419)"`

---

## Phase 7 — Background-failure observability

### Task 7.1: `slog.Warn` → `slog.Error` on the background fire path

**Files:** Modify `internal/scheduler/executor.go` (~:75, the fire-failure log); Test `internal/scheduler/executor_test.go`.

- [ ] **Step 1:** Failing test: a fire whose arm-Function dispatch fails logs at ERROR (assert via a captured `slog` handler) with structured `entityId`/`transition`/`sourceState`/error; the task is left in place (not deleted). Run → FAIL.
- [ ] **Step 2:** Change the level to `slog.Error` and add the structured fields; confirm the redispatch backoff still marks the task. Verify `DispatchFunction`'s `common.AddWarning` is a safe no-op under the system ctx (no diagnostics sink) — add a guard test if not already covered.
- [ ] **Step 3:** Run → PASS. **Step 4:** Commit — `git commit -am "feat(scheduler): ERROR-log background arm-Function failures"`.

---

## Phase 8 — Documentation (Gate 4 / Gate 7)

### Task 8.1: Review the new error-code topic prose

**Files:** Modify `cmd/cyoda/help/content/errors/SCHEDULE_FUNCTION_INVALID_RESULT.md` (created in Task 6.2). Test: `TestErrCode_Parity` (already green since 6.2 added constant+topic together).

- [ ] **Step 1:** Review/refine the topic prose: a scheduled-transition Function returned a result the engine could not interpret as a `Schedule`; HTTP 500; not retryable; no issue IDs; compact.
- [ ] **Step 2:** Run `go test ./... -run TestErrCode_Parity -v` → PASS. **Step 3:** Commit. *(The four infra-code topics already exist and already say 503 — no change needed; this task only covers the one new topic's wording.)*

### Task 8.2: `cyoda help` workflow authoring + compute-node contract

**Files:** Modify `cmd/cyoda/help/content/workflows.md` (SCHEDULED TRANSITIONS section); add a Functions subsection to the compute-node docs referencing the new schemas; Test: `cmd/cyoda/help/...` topic tests + `internal/api/help_action_test.go`.

- [ ] **Step 1:** Add a `schedule.function` authoring subsection: the `Schedule` return shape, absolute-vs-relative fire/expiry, invoked-on-every-re-arm, fail-closed on callout failure, born-expired, XOR with `delayMs`. Keep prose compact (state the actionable core).
- [ ] **Step 2:** Add the Function compute-node contract note (request/response shape, `resultKind:"Schedule"`) alongside the criteria/processor examples.
- [ ] **Step 3:** Run help topic tests → PASS. **Step 4:** Commit.

### Task 8.3: Cloud-parity, schema-versioning, README, CHANGELOG, COMPATIBILITY

**Files:** `docs/cloud-parity/scheduled-transitions.md`; `docs/workflow-schema-versioning.md`; `README.md`; `CHANGELOG.md`; `COMPATIBILITY.md`.

- [ ] **Step 1:** Extend `docs/cloud-parity/scheduled-transitions.md` (Gate 7): arm-time Function computation, composite `Schedule`, absolute/relative resolution → stored `scheduledTime`/`TimeoutMs`, born-expired (cancel prior row), uniform infra-503 failure surface.
- [ ] **Step 2:** `docs/workflow-schema-versioning.md`: record the MINOR bump for `schedule.function` (additive/loosening; no prior-MINOR retirement); bump `MaxMinor` (current 1.2 → 1.3) and the version constant.
- [ ] **Step 3:** `README.md` scheduled-transitions section (per-entity Function timer, one line); `CHANGELOG.md` Added entry; `COMPATIBILITY.md` new SPI-tag row.
- [ ] **Step 4:** Run any doc/schema-version tests (`go test ./... -run SchemaVersion -v`) → PASS. **Step 5:** Commit.

---

## Phase 9 — Coverage (carry the spec §13 matrix forward, gap-free)

Each row of §13 maps to a test here. Fake Function-serving compute node = extend the existing gRPC test member that answers processor/criteria to also answer `EntityFunctionCalculationResponse`.

### Task 9.1: E2E — import validation + arm happy paths (running postgres)

**Files:** Create `internal/e2e/scheduled_function_test.go`.

- [ ] **Step 1:** Tests (assert HTTP status + body code through the full stack): import valid `schedule.function` (200); `function`+`delayMs` → 400 `VALIDATION_FAILED`; `function`+`manual` → 400; `resultKind≠Schedule` / missing name/tags → 400; arm with absolute `fireAt` and relative `fireAfterMs` (fake node) → task fires at the expected time; with/without expiry.
- [ ] **Step 2:** Run `go test ./internal/e2e/ -run ScheduledFunction -v` (Docker) → GREEN. **Step 3:** Commit.

### Task 9.2: E2E — error surfaces + born-expired + settled-interval

**Files:** Extend `internal/e2e/scheduled_function_test.go`, `internal/e2e/dispatch_infra_error_test.go`.

- [ ] **Step 1:** Function no-member → 503 `NO_COMPUTE_MEMBER_FOR_TAG`; Function timeout → 503 `DISPATCH_TIMEOUT`; malformed result → 500 `SCHEDULE_FUNCTION_INVALID_RESULT`; born-expired → 2xx not armed + `SCHEDULED_TRANSITION_EXPIRE`; past `fireAt` → due now → fires; expiry elapsed → Expired, no fire; re-arm on plain data update recomputes (fake node called again); born-expired on re-arm cancels the prior armed row (no stale fire); explicit fire → 400 `TRANSITION_NOT_FOUND`.
- [ ] **Step 2:** Run → GREEN. **Step 3:** Commit.

### Task 9.3: Cross-backend parity + gRPC envelope + concurrency

**Files:** Create `e2e/parity/scheduledfunction/scheduledfunction.go` + register in `e2e/parity/registry.go`; extend `internal/grpc/*_test.go` (or `internal/e2e/grpc` if that is where envelope tests live); Create an isolated concurrency e2e.

- [ ] **Step 1 (parity):** Backend-agnostic scenarios (memory/sqlite/postgres + commercial): arm absolute/relative, born-expired cancel, past-fire-fires, expiry-elapsed. Register in `registry.go`. Run `go test ./e2e/parity/... -v`.
- [ ] **Step 2 (gRPC):** Envelope tests asserting `Success`/`Error.Code` for: import validation failures, Function no-member (503 mapping surfaced in the envelope), timeout, malformed, explicit-fire rejection. Cover the reconciled processor/criterion no-member → 503 at the gRPC entrypoint too.
- [ ] **Step 3 (concurrency, isolated single-backend — NOT parity):** concurrent writes racing re-arm of a Function schedule → exactly one consistent armed task, no torn write. Assert consistency (one winner), not a precise interleave.
- [ ] **Step 4:** Run all → GREEN. **Step 5:** Commit — `git commit -am "test: scheduled-transition Function coverage matrix (#419)"`.

### Task 9.4: Full-suite + race gate (pre-PR)

- [ ] **Step 1:** `go test ./... -v` (root incl. e2e, Docker) → GREEN.
- [ ] **Step 2:** `make test-all` (root + plugins) → GREEN.
- [ ] **Step 3:** `go vet ./...` clean; `make race` once → GREEN (E2E excluded per `.claude/rules/race-testing.md`; if E2E race coverage wanted, `go test -race -timeout=20m ./internal/e2e/...`).
- [ ] **Step 4:** Verify cross-plugin: the Cassandra plugin (`../cyoda-go-cassandra`) picks up the new parity scenario + SPI `ReconcileRequest.Cancel` on its next dep bump — note in the PR body; do not modify that repo here.

---

## Self-Review (spec coverage)

Checked against `docs/superpowers/specs/2026-07-17-scheduled-transition-function-design.md`:

| Spec § | Requirement | Task(s) |
|---|---|---|
| §4.1 wire | `EntityFunctionCalculation{Request,Response}` | 4.1 |
| §4.2 unification | shared primitive + cluster collapse | 1.1–1.4 |
| §4.3 method + fan-out | `DispatchFunction` + all implementors | 4.3 |
| §5.1 config | `ScheduleFunction` DTO/SPI, defaults, XOR | 3.1, 5.1, 5.2 |
| §5.2 resolution | fire/expiry → `scheduledTime`/`TimeoutMs` | 6.2 |
| §5.3 born-expired | cancel prior row, EXPIRE, not-armed | 3.1, 6.1, 6.2 |
| §6 arm wiring | dispatch in reconcile, tx-gate scoped | 6.2 |
| §7 infra-503 (4 codes) | mint at emit sites + `errors.As` passthrough | 2.1–2.4 |
| §7.2 new code | `SCHEDULE_FUNCTION_INVALID_RESULT` | 6.2 (constant), 8.1 (topic) |
| §8 observability | ERROR-log + backoff | 7.1 |
| §9 SPI | additive types + `ReconcileRequest.Cancel`, tag-first | 3.1, 3.2 |
| §10 OpenAPI | `ScheduleFunctionDto`, oasdiff | 5.1 |
| §11 docs | help/cloud-parity/schema-version/README/CHANGELOG/COMPATIBILITY/ARCHITECTURE | 1.4, 8.1–8.3 |
| §12/§13 matrix | every endpoint × code; HTTP+gRPC; parity; concurrency isolated | 9.1–9.4 |

**Placeholder scan:** none — every code step shows code; the three-backend store change (6.1) gives the shared pattern + per-store delta (DRY, not a placeholder).

**Type consistency:** `dispatchCalloutToMember`, `FunctionResult{Kind,Value}`, `ScheduleFunction`, `ReconcileRequest.Cancel`, `resolveSchedule`, `DispatchCalloutRequest/Response`, `ErrCodeScheduleFunctionInvalidResult` used identically across tasks.

**Ordering:** SPI (3) precedes its consumers (5,6); dispatch unification (1) precedes Function dispatch (4.3) and infra-503 (2); error constant added in 6.2, topic in 8.1 (same PR, so `TestErrCode_Parity` only needs to pass by the end — run it in 8.1).

