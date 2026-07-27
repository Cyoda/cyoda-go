# Criterion Stoppage Reason in State-Machine Audit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the criterion "stoppage reason" (from `EntityCriteriaCalculationResponse.reason`) on the entity state-machine audit trail and the manual-transition 400 body, so clients can analyse *why* a passage was blocked.

**Architecture:** Thread the reason from the gRPC criteria response through `ProcessingResponse` → the widened `DispatchCriteria` interface → the widened engine `evaluateCriterion`, then populate the `data` payload of the existing `TRANSITION_NOT_MATCH_CRITERION` and `WORKFLOW_SKIP` state-machine audit events (which already fire with `nil` data) and enrich the manual-transition 400 error. All-internal; no `cyoda-go-spi` change.

**Tech Stack:** Go 1.26+, `log/slog`, testcontainers-go (e2e Postgres), the in-tree `internal/testing/localproc` in-process criteria harness.

**Spec:** `docs/superpowers/specs/2026-07-14-criterion-response-reason-audit-design.md`. Issue #413 (v0.8.3). Follow-on #414 (processor criteria — out of scope).

## Global Constraints

- Go 1.26+. Use `log/slog` exclusively — never `log.Printf`/`fmt.Printf` for operational logging.
- Wrap errors: `fmt.Errorf("...: %w", err)`. Use `uuid.UUID`, not `string`, for UUIDs.
- **No `cyoda-go-spi` change, no coordinated SPI release, no schema-version bump** — every type touched is in `internal/…`.
- **No issue IDs (`#NNN`) in shipped artefacts** — code, error messages, response bodies, comments, OpenAPI/help content. Issue IDs only in commits, PR bodies, and `docs/`.
- 4xx errors carry full domain detail; the reason appended to the 400 is same-tenant compute-node-supplied and reflected into a JSON body (no HTML surface).
- **Reason length cap:** `maxCriterionReasonLen = 2048` bytes; truncate once in `evaluateCriterion`'s return.
- **Inline D3 default reason:** `"criterion did not match"`.
- Never log the reason beyond existing `common.AddWarning`/diagnostics.
- Run `go build ./... && go vet ./...` after any signature change; it surfaces every broken call site.
- `data` field names align to the typed DTOs: transition events use `transition` (not `transitionName`).

---

### Task 1: Ingest `reason` into `ProcessingResponse`

Additive only — no signature change. The gRPC criteria response's `reason` field is currently dropped by `handleCriteriaResponse`.

**Files:**
- Modify: `internal/grpc/members.go` (ProcessingResponse struct, ~line 24)
- Modify: `internal/grpc/streaming.go` (`handleCriteriaResponse`, ~line 281)
- Test: `internal/grpc/streaming_test.go` (new test after `TestStreaming_CriteriaResponse`, ~line 488)

**Interfaces:**
- Produces: `ProcessingResponse.Reason string` — populated from the wire `reason` field on criteria responses; empty for processor responses.

- [ ] **Step 1: Write the failing test**

Add to `internal/grpc/streaming_test.go`:

```go
func TestStreaming_CriteriaResponse_PropagatesReason(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() { done <- svc.StartStreaming(stream) }()

	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest("req-reason-1")

	respPayload := map[string]any{
		"requestId": "req-reason-1",
		"success":   true,
		"matches":   false,
		"reason":    "credit score 540 below threshold 600",
	}
	respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create criteria response event: %v", err)
	}
	stream.enqueue(respCE)

	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.Reason != "credit score 540 below threshold 600" {
			t.Errorf("expected reason propagated, got %q", resp.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for criteria response")
	}

	stream.closeRecv()
	<-done
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpc/ -run TestStreaming_CriteriaResponse_PropagatesReason -v`
Expected: FAIL — `resp.Reason` undefined (compile error) or empty.

- [ ] **Step 3: Add the field to `ProcessingResponse`**

In `internal/grpc/members.go`, inside `type ProcessingResponse struct`, after the `Matches` field:

```go
	// Reason is the criteria-response explanation for a matches=false result
	// (EntityCriteriaCalculationResponse.reason). Empty for processor
	// responses and for criteria that supply no reason.
	Reason string
```

- [ ] **Step 4: Deserialize and propagate it**

In `internal/grpc/streaming.go` `handleCriteriaResponse`, add `Reason` to the anonymous struct:

```go
	var resp struct {
		RequestID string `json:"requestId"`
		Success   bool   `json:"success"`
		Matches   bool   `json:"matches"`
		Reason    string `json:"reason"`
		Error     *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string `json:"warnings"`
	}
```

and set it in the `CompleteRequest` call:

```go
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Success:   resp.Success,
		Error:     errMsg,
		Matches:   &matches,
		Reason:    resp.Reason,
		Warnings:  resp.Warnings,
		Retryable: retryable,
	})
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/grpc/ -run TestStreaming_CriteriaResponse -v`
Expected: PASS (both the original and the new test).

- [ ] **Step 6: Commit**

```bash
git add internal/grpc/members.go internal/grpc/streaming.go internal/grpc/streaming_test.go
git commit -m "feat(grpc): ingest criteria-response reason into ProcessingResponse"
```

---

### Task 2: Add `Reason` to cross-node `DispatchCriteriaResponse`

Additive only. Prepares the cross-node carrier before the interface widens (Task 3) so the peer branch can compile.

**Files:**
- Modify: `internal/cluster/dispatch/types.go` (`DispatchCriteriaResponse`, ~line 49)
- Test: `internal/cluster/dispatch/types_test.go` (`TestDispatchCriteriaResponse_JSONRoundTrip`, ~line 210)

**Interfaces:**
- Produces: `DispatchCriteriaResponse.Reason string` (json `reason,omitempty`) — the criteria reason as evaluated on the peer node.

- [ ] **Step 1: Write the failing test**

Add to `internal/cluster/dispatch/types_test.go`:

```go
func TestDispatchCriteriaResponse_ReasonRoundTrip(t *testing.T) {
	in := DispatchCriteriaResponse{Matches: false, Success: true, Reason: "amount 5 below minimum 10"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out DispatchCriteriaResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Reason != in.Reason {
		t.Errorf("reason not round-tripped: got %q want %q", out.Reason, in.Reason)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cluster/dispatch/ -run TestDispatchCriteriaResponse_ReasonRoundTrip -v`
Expected: FAIL — `Reason` undefined (compile error).

- [ ] **Step 3: Add the field**

In `internal/cluster/dispatch/types.go`, inside `type DispatchCriteriaResponse struct`, after `Error`:

```go
	Reason   string   `json:"reason,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cluster/dispatch/ -run TestDispatchCriteriaResponse_ReasonRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cluster/dispatch/types.go internal/cluster/dispatch/types_test.go
git commit -m "feat(cluster): add Reason to cross-node DispatchCriteriaResponse"
```

---

### Task 3: Widen `DispatchCriteria` to `(bool, string, error)` across all implementors

Compilation-atomic: the interface change breaks every implementor, test double, and call site at once. `ExternalProcessingService` is `internal/contract/processing.go:12` (internal — no SPI change). Verified implementor set: `ProcessorDispatcher` (grpc), `ClusterDispatcher`, `skeleton`, `localproc`, `TracingExternalProcessingService`. Verified 6 doubles: `stubDispatcher`, `fakeLocalDispatcher`, `capturingLocalDispatcher`, `fakeDispatcher`, `mockExtProc`, `mockExternalProcessing`.

**Files:**
- Modify: `internal/contract/processing.go` (interface, ~line 12)
- Modify: `internal/grpc/dispatch.go` (`DispatchCriteria`, ~line 195)
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (`DispatchCriteria`, lines 121, 135-137, 171)
- Modify: `internal/cluster/dispatch/handler.go` (`handleCriteria`, ~line 96-109)
- Modify: `internal/skeleton/processing.go` (~line 20)
- Modify: `internal/observability/dispatch_tracing.go` (~line 77)
- Modify: `internal/testing/localproc/localproc.go` (interface method + `CriteriaFunc` plumbing)
- Modify (test doubles): `internal/cluster/dispatch/cluster_dispatcher_test.go:39`, `internal/cluster/dispatch/handler_test.go:40`, `internal/cluster/dispatch/integration_txtoken_test.go:41`, `internal/observability/dispatch_tracing_test.go:32`, `internal/domain/workflow/engine_test.go:871` and `:1600`
- Test: `internal/grpc/dispatch_test.go` (`TestDispatchCriteria_MatchesFalse`, ~line 313), `internal/cluster/dispatch/handler_test.go`

**Interfaces:**
- Consumes: `ProcessingResponse.Reason` (Task 1), `DispatchCriteriaResponse.Reason` (Task 2).
- Produces:
  - `contract.ExternalProcessingService.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID) (matches bool, reason string, err error)`.
  - `localproc.RegisterCriteriaReason(name string, fn func(ctx, *spi.Entity, json.RawMessage) (bool, string, error))` — registers a reason-returning criterion. Existing `RegisterCriteria` (the `(bool, error)` form) is preserved as a shim returning `""`, so its existing call sites are untouched.

- [ ] **Step 1: Write the failing tests**

Modify `internal/grpc/dispatch_test.go` `TestDispatchCriteria_MatchesFalse` — set a reason on the response and assert it is returned:

```go
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success: true,
			Matches: &matchesFalse,
			Reason:  "amount 5 below minimum 10",
		})
	}()

	result, reason, err := dispatcher.DispatchCriteria(ctx, entity, criterion, "transition", "wf1", "t1", "", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected matches=false")
	}
	if reason != "amount 5 below minimum 10" {
		t.Errorf("expected reason returned, got %q", reason)
	}
```

Add to `internal/cluster/dispatch/handler_test.go` a peer-producer assertion (adapt to the file's existing `fakeLocalDispatcher` — give it a `reason` return, then assert the built `DispatchCriteriaResponse.Reason`):

```go
func TestHandleCriteria_PropagatesReason(t *testing.T) {
	// fakeLocalDispatcher.DispatchCriteria returns (false, "peer reason here", nil).
	// Drive handleCriteria and assert the response body carries Reason.
	h := newTestHandlerWithLocal(t, &fakeLocalDispatcher{matches: false, reason: "peer reason here"})
	rec := httptest.NewRecorder()
	h.handleCriteria(rec, newCriteriaRequest(t))
	var resp DispatchCriteriaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Reason != "peer reason here" {
		t.Errorf("expected peer reason propagated, got %q", resp.Reason)
	}
}
```

(If `newTestHandlerWithLocal`/`newCriteriaRequest` helpers do not exist, reuse the construction already present in `handler_test.go`'s other criteria tests; the assertion on `resp.Reason` is the point.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go build ./... 2>&1 | head`
Expected: compile errors — call sites now expect 3 return values / doubles have wrong signature. This is the RED for a signature change.

- [ ] **Step 3: Widen the interface**

In `internal/contract/processing.go`, change the `DispatchCriteria` method signature to:

```go
	DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (matches bool, reason string, err error)
```

- [ ] **Step 4: Update the grpc dispatcher**

In `internal/grpc/dispatch.go` `DispatchCriteria`, update the signature to `(bool, string, error)` and every `return`. On the response path return `resp.Reason`; on all error/empty paths return `""`:

```go
		if resp == nil || !resp.Success {
			errMsg := "criteria returned failure"
			if resp != nil && resp.Error != "" {
				errMsg = resp.Error
				common.AddError(ctx, fmt.Sprintf("criteria %s: %s", parsed.Function.Name, errMsg))
			}
			return false, "", fmt.Errorf("criteria dispatch failed: %s", errMsg)
		}
		slog.Info("criteria completed", "pkg", "grpc", "memberId", member.ID, "criteria", parsed.Function.Name, "requestId", requestID, "success", true)
		if resp.Matches != nil {
			return *resp.Matches, resp.Reason, nil
		}
		return false, resp.Reason, nil
```

Update the earlier error returns (`invalid criterion JSON`, `ErrNoMatchingMember`, send failure, cloud-event build failure, timeout, `ctx.Done`) to `return false, "", <err>`.

- [ ] **Step 5: Update the cluster dispatcher (local + peer branches)**

In `internal/cluster/dispatch/cluster_dispatcher.go` `DispatchCriteria`, signature → `(bool, string, error)`. Local branch (lines 135-137):

```go
	// Try local first.
	matches, reason, err := d.local.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err == nil {
		return matches, reason, nil
	}
	if !isNoMatchingMember(err) {
		return false, "", err
	}
```

All intermediate error returns → `return false, "", <err>`. Peer branch final return (line 171):

```go
	return resp.Matches, resp.Reason, nil
```

- [ ] **Step 6: Update the cluster peer producer**

In `internal/cluster/dispatch/handler.go` `handleCriteria` (~line 96-109):

```go
	matches, reason, err := h.local.DispatchCriteria(ctx, entity, req.Criterion, req.Target, req.WorkflowName, req.TransitionName, req.ProcessorName, req.TxID)
	if err != nil {
		slog.Error("dispatch criteria failed", "pkg", "dispatch", "err", err)
		writeJSON(w, http.StatusOK, DispatchCriteriaResponse{
			Success: false,
			Error:   "dispatch criteria failed",
		})
		return
	}

	writeJSON(w, http.StatusOK, DispatchCriteriaResponse{
		Success: true,
		Matches: matches,
		Reason:  reason,
	})
```

- [ ] **Step 7: Update the leaf implementors**

`internal/skeleton/processing.go` — return `(false, "", <err>)` (it is a not-configured stub).

`internal/observability/dispatch_tracing.go` `DispatchCriteria` — pass through the extra value:

```go
func (t *TracingExternalProcessingService) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (bool, string, error) {
	// ... existing span setup ...
	matches, reason, err := t.inner.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	// ... existing span end / error recording ...
	return matches, reason, err
}
```

`internal/testing/localproc/localproc.go` — store a reason-returning form internally, keep `RegisterCriteria` as a shim, add `RegisterCriteriaReason`, and return the reason from `DispatchCriteria`:

```go
// criteriaFuncR is the internal reason-returning criterion form.
type criteriaFuncR func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, string, error)

// (change the field type)
//   criteria map[string]criteriaFuncR

// RegisterCriteria registers a criteria callback by function name (no reason).
func (s *LocalProcessingService) RegisterCriteria(name string, fn CriteriaFunc) {
	s.RegisterCriteriaReason(name, func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
		m, err := fn(ctx, e, c)
		return m, "", err
	})
}

// RegisterCriteriaReason registers a criteria callback that also returns a reason.
func (s *LocalProcessingService) RegisterCriteriaReason(name string, fn criteriaFuncR) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.criteria[name] = fn
	if _, ok := s.criteriaCalls[name]; !ok {
		s.criteriaCalls[name] = &atomic.Int64{}
	}
}

func (s *LocalProcessingService) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	// ... existing name-parse ...
	if err := json.Unmarshal(criterion, &parsed); err != nil {
		return false, "", fmt.Errorf("invalid criterion JSON: %w", err)
	}
	// ... lookup ...
	if !ok {
		return false, "", fmt.Errorf("no local criteria registered for %q", name)
	}
	counter.Add(1)
	return fn(ctx, entity, criterion)
}
```

Leave `type CriteriaFunc` as `func(...) (bool, error)` (the shim source) so existing `RegisterCriteria` call sites across e2e/engine tests compile unchanged.

- [ ] **Step 8: Update the 6 test doubles**

For each, change the `DispatchCriteria` method to `(bool, string, error)` and return `""` for reason (or a fixed reason where the test asserts one):
- `internal/cluster/dispatch/cluster_dispatcher_test.go:39` `stubDispatcher`
- `internal/cluster/dispatch/handler_test.go:40` `fakeLocalDispatcher` — add a `reason string` field and return it (used by the Step-1 test)
- `internal/cluster/dispatch/integration_txtoken_test.go:41` `capturingLocalDispatcher`
- `internal/observability/dispatch_tracing_test.go:32` `fakeDispatcher`
- `internal/domain/workflow/engine_test.go:871` `mockExtProc`
- `internal/domain/workflow/engine_test.go:1600` `mockExternalProcessing`

- [ ] **Step 9: Sweep remaining call sites**

Run: `go build ./... && go vet ./... 2>&1 | head -40`
Fix each reported call site (`matches, err :=` → `matches, _, err :=` where the reason is unused). These are compilation-enforced; the build output is the checklist.

- [ ] **Step 10: Run tests to verify they pass**

Run: `go test ./internal/grpc/ ./internal/cluster/... ./internal/observability/ ./internal/skeleton/ ./internal/testing/localproc/ -v 2>&1 | tail -30`
Expected: PASS, including the new reason tests.

- [ ] **Step 11: Commit**

```bash
git add internal/contract internal/grpc internal/cluster internal/skeleton internal/observability internal/testing/localproc
git commit -m "feat: widen DispatchCriteria to return the criterion reason"
```

---

### Task 4: Engine — thread reason into audit `data` and the manual-transition 400

Compilation-atomic within `internal/domain/workflow`: widening `evaluateCriterion` breaks its three callers (`selectWorkflow:386`, manual `attemptTransition:476`, `cascadeAutomated:553`), which are exactly the three no-match sites.

**Files:**
- Modify: `internal/domain/workflow/engine.go` (`evaluateCriterion` ~623; sites ~399, ~483/486, ~560; add helpers + consts)
- Test: `internal/domain/workflow/engine_test.go`

**Interfaces:**
- Consumes: `contract.ExternalProcessingService.DispatchCriteria(...) (bool, string, error)` (Task 3).
- Produces:
  - `evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (matched bool, reason string, err error)` — `reason` is the capped external reason for a clean `matches=false` (empty for inline predicates and for `matched=true`).
  - `data` payloads: `TRANSITION_NOT_MATCH_CRITERION` → `{"workflowName","transition","criterion","reason"}`; `WORKFLOW_SKIP` → `{"workflowName","reason"}`. `reason` is always present (external reason, else the inline default `"criterion did not match"`).
  - Manual-transition rejection error: `transition %q criterion not matched: %s` (reason appended) — but **only when the reason is a real external reason**; the bare `transition %q criterion not matched` is kept when the reason is the inline default.

- [ ] **Step 1: Write the failing tests**

Add to `internal/domain/workflow/engine_test.go`. Use the existing pattern: build an engine with a `mockExtProc` whose `DispatchCriteria` returns `(false, "amount 5 below minimum 10", nil)` for a FUNCTION criterion, run an automated cascade whose transition has a FUNCTION criterion, then read the audit store and assert the `TRANSITION_NOT_MATCH_CRITERION` event's `Data`:

```go
func TestEngine_AutomatedCriterionNoMatch_RecordsReason(t *testing.T) {
	// ... build engine with extProc returning (false, "amount 5 below minimum 10", nil) ...
	// ... workflow: CREATED --[auto, criterion function "min-amount"]--> PREMIUM ...
	// ... run Execute so the cascade evaluates the criterion and stops ...

	events, err := auditStore.GetEventsByTransaction(ctx, "txid", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	var found bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventTransitionCriterionNoMatch {
			found = true
			if ev.Data["reason"] != "amount 5 below minimum 10" {
				t.Errorf("reason: got %v", ev.Data["reason"])
			}
			if ev.Data["transition"] == "" || ev.Data["workflowName"] == "" || ev.Data["criterion"] == "" {
				t.Errorf("missing data keys: %v", ev.Data)
			}
		}
	}
	if !found {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION event recorded")
	}
}

func TestEngine_InlineCriterionNoMatch_DefaultsReason(t *testing.T) {
	// ... workflow transition with an INLINE criterion that evaluates false
	//     (e.g. {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100} with amount=5) ...
	// assert ev.Data["reason"] == "criterion did not match" and ev.Data["criterion"] == "simple"
}

func TestEngine_ManualCriterionNoMatch_EnrichesError(t *testing.T) {
	// ... manual transition whose FUNCTION criterion returns (false, "reason X", nil) ...
	_, err := eng.ManualTransition(ctx, entity, "approve", auditStore, txID)
	if err == nil || !strings.Contains(err.Error(), "criterion not matched: reason X") {
		t.Fatalf("expected enriched error, got %v", err)
	}
}

func TestEngine_ManualInlineCriterionNoMatch_NoRedundantSuffix(t *testing.T) {
	// inline criterion → error stays "transition \"approve\" criterion not matched" (no ": criterion did not match")
	_, err := eng.ManualTransition(ctx, entity, "approve", auditStore, txID)
	if err == nil || strings.Contains(err.Error(), "criterion not matched: criterion did not match") {
		t.Fatalf("unexpected redundant suffix: %v", err)
	}
}

func TestEngine_WorkflowSkipped_RecordsReason(t *testing.T) {
	// workflow-selection FUNCTION criterion returns (false, "tenant not eligible", nil) → WORKFLOW_SKIP data.reason
	// assert an SMEventWorkflowSkipped event with Data["reason"]=="tenant not eligible" and Data["workflowName"] set
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/workflow/ -run 'TestEngine_(AutomatedCriterionNoMatch|InlineCriterionNoMatch|ManualCriterionNoMatch|ManualInlineCriterionNoMatch|WorkflowSkipped)' -v`
Expected: FAIL (compile error on `evaluateCriterion` return / missing data keys).

- [ ] **Step 3: Add helpers and constants**

In `internal/domain/workflow/engine.go` (near the other package consts):

```go
// maxCriterionReasonLen bounds the criterion reason stored in the audit and
// reflected into the 400 body. The reason is compute-node-supplied and the
// audit is durable storage, so it is capped defensively.
const maxCriterionReasonLen = 2048

// defaultCriterionReason is recorded when a criterion returns false without an
// explanatory reason (inline predicates, or a FUNCTION criterion that supplies
// none). Keeps the audit data.reason shape stable.
const defaultCriterionReason = "criterion did not match"

func capReason(s string) string {
	if len(s) > maxCriterionReasonLen {
		return s[:maxCriterionReasonLen]
	}
	return s
}

// criterionName derives the audit "criterion" field: the FUNCTION name for a
// function criterion, else the parsed condition type (e.g. "simple", "group").
func criterionName(criterion []byte) string {
	var fn struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(criterion, &fn) == nil && fn.Function.Name != "" {
		return fn.Function.Name
	}
	if cond, err := predicate.ParseCondition(criterion); err == nil {
		return cond.Type()
	}
	return ""
}
```

- [ ] **Step 4: Widen `evaluateCriterion`**

Change the signature to `(bool, string, error)`. Return the capped reason from the FUNCTION dispatch; `""` for the inline `match.Match` path and for `matched=true`:

```go
func (e *Engine) evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (bool, string, error) {
	cond, err := predicate.ParseCondition(criterion)
	if err != nil {
		return false, "", fmt.Errorf("failed to parse criterion: %w", err)
	}
	if _, ok := cond.(*predicate.FunctionCondition); ok {
		if e.extProc == nil {
			return false, "", fmt.Errorf("no external processing service configured for FUNCTION criteria")
		}
		matched, reason, dErr := e.extProc.DispatchCriteria(cc.ctx, entity, criterion, cc.target, cc.workflowName, cc.transitionName, "", cc.txID)
		return matched, capReason(reason), dErr
	}
	matched, mErr := match.Match(cond, entity) // preserve the existing inline evaluation call
	return matched, "", mErr
}
```

(Adapt the inline branch to the file's actual local evaluation — the point is: FUNCTION returns the reason, inline returns `""`.)

- [ ] **Step 5: Populate `data` at the automated-cascade site (~line 560)**

```go
			matched, reason, err := e.evaluateCriterion(tr.Criterion, entity, &criterionContext{
				ctx: currentCtx, txID: txID, workflowName: wf.Name, transitionName: tr.Name, target: "TRANSITION",
			})
			if err != nil {
				return currentCtx, currentTxID, fmt.Errorf("failed to evaluate transition criterion: %w", err)
			}
			if !matched {
				if reason == "" {
					reason = defaultCriterionReason
				}
				e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
					spi.SMEventTransitionCriterionNoMatch,
					fmt.Sprintf("Automated transition %q criterion not matched", tr.Name),
					map[string]any{
						"workflowName": wf.Name,
						"transition":   tr.Name,
						"criterion":    criterionName(tr.Criterion),
						"reason":       reason,
					})
				continue
			}
```

- [ ] **Step 6: Populate `data` + enrich the error at the manual site (~line 476-486)**

```go
	if len(transition.Criterion) > 0 && string(transition.Criterion) != "null" {
		matched, reason, err := e.evaluateCriterion(transition.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, transitionName: transitionName, target: "TRANSITION",
		})
		if err != nil {
			return ctx, txID, fmt.Errorf("failed to evaluate transition criterion: %w", err)
		}
		if !matched {
			external := reason != ""
			if reason == "" {
				reason = defaultCriterionReason
			}
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionCriterionNoMatch,
				fmt.Sprintf("Transition %q criterion not matched", transitionName),
				map[string]any{
					"workflowName": wf.Name,
					"transition":   transitionName,
					"criterion":    criterionName(transition.Criterion),
					"reason":       reason,
				})
			if external {
				return ctx, txID, fmt.Errorf("transition %q criterion not matched: %s", transitionName, reason)
			}
			return ctx, txID, fmt.Errorf("transition %q criterion not matched", transitionName)
		}
	}
```

- [ ] **Step 7: Populate `data` at the workflow-skip site (~line 399)**

```go
		matched, reason, err := e.evaluateCriterion(wf.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, target: "WORKFLOW",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate workflow criterion for %q: %w", wf.Name, err)
		}
		if matched {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventWorkflowFound, fmt.Sprintf("Workflow %q matched criterion", wf.Name), nil)
			return wf, nil
		}
		if reason == "" {
			reason = defaultCriterionReason
		}
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowSkipped, fmt.Sprintf("Workflow %q criterion not matched", wf.Name),
			map[string]any{"workflowName": wf.Name, "reason": reason})
```

- [ ] **Step 8: Sweep and run**

Run: `go build ./... && go test ./internal/domain/workflow/ -v 2>&1 | tail -30`
Expected: PASS (new tests green, no regressions). Fix any remaining `evaluateCriterion` caller the build flags.

- [ ] **Step 9: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): record criterion reason in state-machine audit + 400 body"
```

---

### Task 5: HTTP e2e coverage (running Postgres backend)

Covers the manual-transition 400 body, the audit `data.reason` for all three events, and the inline default. Uses the `procSvc.RegisterCriteriaReason` harness (Task 3) and the existing `getSMAuditEvents` helper (`internal/e2e/workflow_proc_test.go:34`).

**Files:**
- Create: `internal/e2e/criterion_reason_test.go`

**Interfaces:**
- Consumes: `procSvc *localproc.LocalProcessingService` (the e2e-global processing service), `getSMAuditEvents`, `setupModelWithWorkflow`, `createEntityE2E`, `doAuth`, `readBody` (all in `internal/e2e`).

- [ ] **Step 1: Write the failing tests**

Create `internal/e2e/criterion_reason_test.go`:

```go
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// smEventReason returns the reason recorded on the first StateMachine audit
// event of the given eventType for the entity, or "" if none.
func smEventReason(t *testing.T, entityID, eventType string) (string, bool) {
	t.Helper()
	for _, ev := range getSMAuditEvents(t, entityID) {
		if ev["eventType"] == eventType {
			data, _ := ev["data"].(map[string]any)
			if data == nil {
				return "", true
			}
			r, _ := data["reason"].(string)
			return r, true
		}
	}
	return "", false
}

func TestCriterionReason_ManualReject_Enriches400AndAudit(t *testing.T) {
	const model = "e2e-critreason-manual"
	procSvc.RegisterCriteriaReason("credit-check",
		func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
			return false, "credit score 540 below threshold 600", nil
		})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cr-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"criterion": {"type": "function", "function": {"name": "credit-check"}}}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	// Manual transition rejected → 400 WORKFLOW_FAILED carrying the reason.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"X","amount":10}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "credit score 540 below threshold 600") {
		t.Errorf("400 body missing reason: %s", body)
	}

	// Audit carries it too.
	reason, ok := smEventReason(t, entityID, "TRANSITION_NOT_MATCH_CRITERION")
	if !ok {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION audit event")
	}
	if reason != "credit score 540 below threshold 600" {
		t.Errorf("audit reason: got %q", reason)
	}
}

func TestCriterionReason_AutomatedReject_Audit(t *testing.T) {
	const model = "e2e-critreason-auto"
	procSvc.RegisterCriteriaReason("min-amount",
		func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
			return false, "amount 10 below minimum 100", nil
		})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "auto-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type": "function", "function": {"name": "min-amount"}}}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	reason, ok := smEventReason(t, entityID, "TRANSITION_NOT_MATCH_CRITERION")
	if !ok {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION audit event")
	}
	if reason != "amount 10 below minimum 100" {
		t.Errorf("audit reason: got %q", reason)
	}
}

func TestCriterionReason_InlineReject_DefaultReason(t *testing.T) {
	const model = "e2e-critreason-inline"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "inline-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	reason, ok := smEventReason(t, entityID, "TRANSITION_NOT_MATCH_CRITERION")
	if !ok {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION audit event")
	}
	if reason != "criterion did not match" {
		t.Errorf("expected default reason, got %q", reason)
	}
}
```

(If the inline `simple`/`GREATER_THAN` criterion shape differs from the engine's parser, mirror the exact shape used by an existing passing e2e workflow criterion; the assertion is the default reason.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/e2e/ -run TestCriterionReason -v 2>&1 | tail -30` (Docker required)
Expected: initially FAIL if run before Tasks 3-4 land; after them, they exercise the full stack — run to confirm they PASS. (If executing strictly in order, they pass here.)

- [ ] **Step 3: Run to verify pass**

Run: `go test ./internal/e2e/ -run TestCriterionReason -v 2>&1 | tail -30`
Expected: PASS (3 tests).

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/criterion_reason_test.go
git commit -m "test(e2e): criterion reason in 400 body and state-machine audit"
```

---

### Task 6: Cross-backend parity scenario

Backend-agnostic behaviour: an inline criterion that fails records `data.reason == "criterion did not match"` on the audit, identically across memory/sqlite/postgres (+ commercial). Inline is chosen so no compute node is required in the parity harness.

**Files:**
- Create: `e2e/parity/criterion_reason.go`
- Modify: `e2e/parity/registry.go` (add entry to `allTests`)
- Modify: `e2e/parity/registry_count_test.go` (`wantParityScenarioCount` +1)

**Interfaces:**
- Consumes: `BackendFixture`, its client `c` (`ImportWorkflow`, `CreateEntity`, `GetAuditEvents`), and `AuditEvent.AsStateMachine()` → typed SM event with `.EventType` and `.Data`.

- [ ] **Step 1: Write the scenario (failing until registered/implemented)**

Create `e2e/parity/criterion_reason.go`:

```go
package parity

import "testing"

// RunCriterionReasonInlineDefault verifies that an inline transition criterion
// which evaluates false records the default stoppage reason on the
// TRANSITION_NOT_MATCH_CRITERION state-machine audit event, identically across
// all backends.
func RunCriterionReasonInlineDefault(t *testing.T, fixture BackendFixture) {
	c := fixture.Client()
	modelName, modelVersion := fixture.UniqueModel("critreason"), 1

	if err := c.ImportModelAndLock(t, modelName, modelVersion); err != nil {
		t.Fatalf("ImportModelAndLock: %v", err)
	}
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "critreason-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}}]},
				"PREMIUM": {}
			}
		}]
	}`
	if err := c.ImportWorkflow(t, modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"X","amount":10}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	auditResp, err := c.GetAuditEvents(t, entityID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	var found bool
	for i := range auditResp.Items {
		ev := &auditResp.Items[i]
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil || sm.EventType != "TRANSITION_NOT_MATCH_CRITERION" {
			continue
		}
		found = true
		reason, _ := sm.Data["reason"].(string)
		if reason != "criterion did not match" {
			t.Errorf("backend %s: reason = %q, want default", fixture.Name(), reason)
		}
	}
	if !found {
		t.Fatalf("backend %s: no TRANSITION_NOT_MATCH_CRITERION event", fixture.Name())
	}
}
```

(Adapt `fixture.Client()`, `UniqueModel`, `ImportModelAndLock`, `fixture.Name()`, and the `sm.Data` accessor to the exact `BackendFixture`/generated-client API in `e2e/parity/fixture.go` and the parity client. If `AsStateMachine().Data` is not exposed by the generated client, extend the parity client's SM-event accessor to surface the `data` map — mirror how `AsEntityChange().ChangeType` is exposed.)

- [ ] **Step 2: Register it and bump the count**

In `e2e/parity/registry.go` add to `allTests` (near the other workflow entries):

```go
	{"CriterionReasonInlineDefault", RunCriterionReasonInlineDefault},
```

In `e2e/parity/registry_count_test.go`, increment `wantParityScenarioCount` by 1.

- [ ] **Step 3: Run to verify it passes on every backend**

Run: `go test ./e2e/parity/... -run 'Parity.*CriterionReasonInlineDefault' -v 2>&1 | tail -40` (Docker required for postgres)
Expected: PASS for memory, sqlite, postgres; `TestParityScenarioCount` green.

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/criterion_reason.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): criterion default reason is backend-agnostic in audit"
```

---

### Task 7: Documentation (Gate 4 & Gate 7)

**Files:**
- Modify: `api/openapi.yaml` (`StateMachineAuditEventDto.data` description, ~line 10893)
- Modify: `docs/cyoda/openapi.yml` (mirror the same description)
- Modify: `cmd/cyoda/help/content/grpc.md` (criteria-response section)
- Create: `docs/cloud-parity/criterion-stoppage-reason.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Update the OpenAPI `data` description**

In `api/openapi.yaml`, extend the `StateMachineAuditEventDto.data` `description` to document the two new data-bearing events (keep it compact):

```yaml
              description: >
                Optional event-specific payload. Present for some event types
                (e.g. STATE_MACHINE_FINISH includes {"success": true},
                STATE_PROCESS_RESULT includes {"success": false}).
                TRANSITION_NOT_MATCH_CRITERION carries
                {workflowName, transition, criterion, reason} and WORKFLOW_SKIP
                carries {workflowName, reason}, where reason explains why the
                criterion blocked the passage. Null for most event types.
```

Apply the identical change to `docs/cyoda/openapi.yml`. Run the OpenAPI conformance/lint check the repo uses (e.g. `go test ./internal/e2e/openapivalidator/... ./e2e/parity/... -run OpenAPI` or the project's `oasdiff` task) and confirm no unintended diff.

- [ ] **Step 2: Update the gRPC help topic**

In `cmd/cyoda/help/content/grpc.md`, in the criteria-response section, add a compact note that a compute node may return `reason` on a `matches:false` response, and that it surfaces on the state-machine audit (`TRANSITION_NOT_MATCH_CRITERION` / `WORKFLOW_SKIP` `data.reason`) and in the manual-transition 400 body. No issue IDs.

- [ ] **Step 3: Write the cloud-parity note**

Create `docs/cloud-parity/criterion-stoppage-reason.md` describing: cyoda-go leads; the criterion reason flows `EntityCriteriaCalculationResponse.reason` → audit `data.reason` on `TRANSITION_NOT_MATCH_CRITERION`/`WORKFLOW_SKIP` and the manual-transition 400 detail; the `data` field names (`workflowName`, `transition`, `criterion`, `reason`); the inline default; the 2 KiB cap; and that Cloud mirrors this shape. Mirror the structure of an existing file such as `docs/cloud-parity/processor-criteria-annotations.md`.

- [ ] **Step 4: Update the CHANGELOG**

Add an entry under the unreleased/v0.8.3 section:

```markdown
### Added
- Workflow criteria can now explain *why* a passage was blocked: a criteria
  compute node's `reason` is recorded on the `TRANSITION_NOT_MATCH_CRITERION`
  and `WORKFLOW_SKIP` state-machine audit events (`data.reason`) and appended to
  the manual-transition `400 WORKFLOW_FAILED` detail.
```

- [ ] **Step 5: Verify help/openapi self-report and commit**

Run: `go test ./cmd/... ./internal/e2e/openapivalidator/... 2>&1 | tail -20`
Expected: PASS (help-topic and OpenAPI parity guards green).

```bash
git add api/openapi.yaml docs/cyoda/openapi.yml cmd/cyoda/help/content/grpc.md docs/cloud-parity/criterion-stoppage-reason.md CHANGELOG.md
git commit -m "docs: criterion stoppage reason (openapi data, help, cloud-parity, changelog)"
```

---

### Task 8: Final verification (Gate 5)

- [ ] **Step 1: Full root-module test (incl. e2e)**

Run: `go test ./... 2>&1 | tail -30` (Docker required)
Expected: all PASS.

- [ ] **Step 2: Plugin submodules**

Run: `make test-short-all 2>&1 | tail -20` (and `make test-all` if Docker is available for the postgres plugin)
Expected: PASS across `plugins/memory|sqlite|postgres`. (No plugin implements `DispatchCriteria`, so these should be unaffected — this confirms it.)

- [ ] **Step 3: Vet**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 4: Race sanity (once, end-of-deliverable)**

Run: `make race 2>&1 | tail -20`
Expected: PASS (per `.claude/rules/race-testing.md`; e2e excluded by the target).

- [ ] **Step 5: Confirm no issue IDs leaked into shipped artefacts**

Run: `git diff 7bc8bf9..HEAD -- ':!docs' ':!CHANGELOG.md' | grep -nE '#[0-9]{2,}' || echo "clean"`
Expected: `clean` (issue IDs only in commits/docs).

---

## Self-Review

**Spec coverage** — every spec section maps to a task:
- Audit surface (transition manual/auto, workflow-skip) → Task 4 (Steps 5-7), Task 5, Task 6.
- Manual-transition 400 enrichment (suppress inline default) → Task 4 (Step 6), Task 5 (Test 1).
- Plumbing (ingest, `ProcessingResponse.Reason`, `DispatchCriteria` widen, cross-node producer + local/peer branches, `evaluateCriterion`) → Tasks 1, 2, 3, 4.
- D1 `(bool, string, error)` → Task 3. D3 default + always-present reason → Task 4 helpers/sites. Criterion-name derivation → Task 4 Step 3.
- Reason length cap (once, in `evaluateCriterion`) → Task 4 Step 4.
- Data-shape field names aligned to typed DTOs → Task 4 Steps 5-7; documented → Task 7 Step 1.
- Coverage matrix: gRPC ingestion → Task 1/3; e2e (external manual+400, automated, inline default) → Task 5; parity (backend-agnostic inline default) → Task 6; multi-node peer + local-branch reason → Task 3 (handler test + dispatcher branches). Concurrency: none required (spec).
- Gate 4 docs (OpenAPI both files, help, CHANGELOG) + Gate 7 cloud-parity → Task 7. No new error code → no `errors/<CODE>.md` task (correct).
- Verification (Gate 5) incl. plugins + race → Task 8.

**Placeholder scan** — no TBD/TODO; every code step shows real code. Two adaptation notes (parity client `sm.Data` accessor; inline-criterion JSON shape) are explicit "mirror the existing X" instructions with a named reference, not vague placeholders.

**Type consistency** — `DispatchCriteria(...) (bool, string, error)` and `evaluateCriterion(...) (bool, string, error)` used consistently; `ProcessingResponse.Reason` (Task 1) and `DispatchCriteriaResponse.Reason` (Task 2) consumed in Task 3; `data` keys `workflowName`/`transition`/`criterion`/`reason` consistent across Task 4 sites, Task 5/6 assertions, and Task 7 docs; `defaultCriterionReason` / `maxCriterionReasonLen` defined once (Task 4) and referenced by name.
