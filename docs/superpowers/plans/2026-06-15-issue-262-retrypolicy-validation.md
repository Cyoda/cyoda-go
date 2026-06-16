# RetryPolicy Import Validation + Retryable Plumbing — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the import-side guard for `RetryPolicy` (reject anything outside `NONE` / `FIXED` / empty) and capture the inbound `retryable` flag on `ProcessingResponse`, without changing dispatch behaviour. The full retry loop stays in #254.

**Architecture:** Two thin slices that mirror in-tree precedents from v0.8.0. Slice 1 clones the ExecutionMode validation pattern (#255) — constants, a `valid…Policies` map, a one-block check in `validateWorkflowStructure`, three cloned tests. Slice 2 widens both streaming response handlers' anonymous error struct to capture `Retryable *bool` and surfaces it as a new nilable field on `ProcessingResponse`. Plus three docs touches: OpenAPI enum, workflows help topic, audit doc honesty pass.

**Tech Stack:** Go 1.26+, `log/slog`, `cyoda-go-spi` (already carries `ProcessorConfig.RetryPolicy string` as a bare field), `encoding/json` standard tri-state pointer-of-bool unmarshal.

**Spec:** `docs/superpowers/specs/2026-06-15-issue-262-retrypolicy-validation-design.md`
**Issue:** [#262](https://github.com/Cyoda-platform/cyoda-go/issues/262)
**Parent (deferred):** #254
**Worktree branch:** `feat/262-retrypolicy-validation` (based on `release/v0.8.0`)

---

## File Structure

```
internal/domain/workflow/
  validate.go                  modify  add constants + map + one check (~12 lines)
  validate_import_test.go      modify  add 3 tests (~80 lines)

internal/grpc/
  members.go                   modify  add Retryable *bool to ProcessingResponse (+1 line)
  streaming.go                 modify  widen 2 anonymous error structs, copy ptr (~12 lines)
  streaming_test.go            modify  add 5 tests (~80 lines)

api/
  openapi.yaml                 modify  add `enum: [NONE, FIXED]` to retryPolicy schema (both occurrences)

cmd/cyoda/help/content/
  workflows.md                 modify  rewrite retryPolicy bullet + add rejection-list bullet

docs/
  WORKFLOW_IMPORT_EXPORT_AUDIT.md   modify  flip §M1 status lines (3 spots)
```

No new files. No SPI changes. No `go.mod` changes. No env-var changes.

---

## Task 1: Add RetryPolicy validator constants, map, and check (RED)

**Files:**
- Modify: `internal/domain/workflow/validate.go` — new constants block + `validRetryPolicies` map (alongside `validExecutionModes` at `:43-49`); new check inside `validateWorkflowStructure`'s processor loop (alongside the `validExecutionModes` check at `:245-248`)
- Test: `internal/domain/workflow/validate_import_test.go` — three new tests appended after the ExecutionMode acceptance test (`TestValidateImportRequest_AcceptsAllKnownExecutionModes` at `:636-660`)

- [ ] **Step 1.1: Write the three failing tests**

Append to `internal/domain/workflow/validate_import_test.go` after `TestValidateImportRequest_AcceptsAllKnownExecutionModes` (around `:660`, just before the `--- AsyncResult + CrossoverToAsyncMs reject-at-import (audit §M6) ---` section header at `:662`):

```go
// --- H4/M1 — RetryPolicy enum check (audit §M1, issue #262) -----------------

// retryPolicyFixture builds a minimal valid two-state workflow with one
// externalized SYNC processor on the only transition, then sets the
// processor's RetryPolicy. Mirrors asyncResultRejectFixture (:667) so the
// retry-policy tests stay structurally identical to their sibling tests.
func retryPolicyFixture(retryPolicy string) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1", Name: "wf-rp", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: false, Processors: []spi.ProcessorDefinition{
					{
						Type:          ProcessorTypeExternalized,
						Name:          "p",
						ExecutionMode: ExecutionModeSync,
						Config:        spi.ProcessorConfig{RetryPolicy: retryPolicy},
					},
				}},
			}},
			"S2": {},
		},
	}
}

func TestValidateImportRequest_RejectsUnknownRetryPolicy(t *testing.T) {
	wf := retryPolicyFixture("LINEAR_BACKOFF")
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for unknown retryPolicy, got nil")
	}
	for _, want := range []string{
		`workflow "wf-rp"`, `state "S1"`, `transition "t"`, `processor "p"`,
		"unknown retryPolicy",
		`"LINEAR_BACKOFF"`,
		"NONE, FIXED, or empty",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message must contain %q; got: %v", want, err)
		}
	}
}

// Empty RetryPolicy must remain valid — the server defaults to FIXED.
func TestValidateImportRequest_AcceptsEmptyRetryPolicy(t *testing.T) {
	wf := retryPolicyFixture("")
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error for empty retryPolicy, got: %v", err)
	}
}

// Both named policies must be accepted.
func TestValidateImportRequest_AcceptsAllKnownRetryPolicies(t *testing.T) {
	for _, policy := range []string{
		RetryPolicyNone,
		RetryPolicyFixed,
	} {
		t.Run(policy, func(t *testing.T) {
			wf := retryPolicyFixture(policy)
			if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
				t.Fatalf("expected no error for retryPolicy=%q, got: %v", policy, err)
			}
		})
	}
}
```

- [ ] **Step 1.2: Run tests to verify they fail**

```bash
go test ./internal/domain/workflow/ -run 'TestValidateImportRequest_(Rejects|Accepts)(.*)RetryPolicy(.*)' -v
```

Expected:
- `TestValidateImportRequest_RejectsUnknownRetryPolicy` — **FAIL** (no error returned, current code silently accepts any string)
- `TestValidateImportRequest_AcceptsEmptyRetryPolicy` — **PASS** (current code accepts everything)
- `TestValidateImportRequest_AcceptsAllKnownRetryPolicies/NONE` — **PASS**
- `TestValidateImportRequest_AcceptsAllKnownRetryPolicies/FIXED` — **PASS**

A compilation error on `RetryPolicyNone` / `RetryPolicyFixed` is the expected failure mode if those constants don't exist yet. That's fine — Step 1.3 introduces them.

- [ ] **Step 1.3: Add constants and validRetryPolicies map**

In `internal/domain/workflow/validate.go`, insert the following block immediately after the `validExecutionModes` map (after `:49`):

```go
// Processor retry-policy tokens. Sourced from the workflow author's
// selector across the two server-resolved retry strategies (audit §M1).
// Centralised here as untyped strings so engine logic, validator rules,
// and tests can compare against a single source — the SPI's
// ProcessorConfig.RetryPolicy field is itself a plain string, so an enum
// type would not buy compile-time safety.
//
//   - NONE  — single attempt, no retry on member-level failure.
//   - FIXED — default when unset. Up to N additional attempts with a
//     fixed delay between tries; N and delay come from server-side
//     config and are not carried in the workflow.
//
// cyoda-go currently captures the policy at import time but does not yet
// honour it at dispatch — the dispatcher remains single-shot. The full
// retry loop ships in #254.
const (
	RetryPolicyNone  = "NONE"
	RetryPolicyFixed = "FIXED"
)

// validRetryPolicies is the set of accepted RetryPolicy values for
// import-time validation (audit §M1, issue #262). Empty string is also
// accepted — the server defaults to FIXED when RetryPolicy is unset.
var validRetryPolicies = map[string]struct{}{
	"":               {},
	RetryPolicyNone:  {},
	RetryPolicyFixed: {},
}
```

- [ ] **Step 1.4: Add the per-processor check inside validateWorkflowStructure**

In `internal/domain/workflow/validate.go`, inside the processor loop in `validateWorkflowStructure`, **after** the existing `validExecutionModes` block at `:245-248` and **before** the closing `}` of the `for _, p := range tr.Processors {` loop (so inside the same iteration), add:

```go
				if _, ok := validRetryPolicies[p.Config.RetryPolicy]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown retryPolicy %q (allowed: NONE, FIXED, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.Config.RetryPolicy)
				}
```

Also update the function's docstring to add the new rule. In the rules list around `:68-75`, add a new bullet after the H4 ExecutionMode bullet:

```go
//   - M1  — RetryPolicy must be one of NONE, FIXED, or empty (defaults to FIXED).
```

- [ ] **Step 1.5: Run tests to verify they pass**

```bash
go test ./internal/domain/workflow/ -run 'TestValidateImportRequest_(Rejects|Accepts)(.*)RetryPolicy(.*)' -v
```

Expected: all four sub-tests **PASS**. Then run the full workflow package to ensure no regression:

```bash
go test ./internal/domain/workflow/ -v
```

Expected: all tests pass (including all existing ExecutionMode and AsyncResult tests).

- [ ] **Step 1.6: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_import_test.go
git -c commit.gpgsign=false commit -m "feat(workflow): reject unknown RetryPolicy at import — refs #262

Adds RetryPolicyNone/RetryPolicyFixed constants and a validRetryPolicies
map alongside the existing validExecutionModes pattern (audit §M1).
Per-processor enum check in validateWorkflowStructure rejects any value
outside NONE, FIXED, or empty with the same error-message shape used by
the ExecutionMode and asyncResult validators.

Mirrors #255 (ExecutionMode) and #261 (asyncResult/crossoverToAsyncMs)
in-tree precedent: SPI keeps the bare string, strictness lives in the
cyoda-go import layer. No dispatch behaviour change — RetryPolicy
remains unconsumed at runtime; the full retry loop stays in #254.

Refs #262, #254

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Add Retryable *bool to ProcessingResponse + plumb through both handlers (RED)

**Files:**
- Modify: `internal/grpc/members.go:24-30` — add `Retryable *bool` field
- Modify: `internal/grpc/streaming.go:244-273` (processor handler), `:278-307` (criteria handler) — widen anonymous `Error` struct, copy pointer through
- Test: `internal/grpc/streaming_test.go` — add 5 tests after `TestStreaming_CriteriaResponse` (`:488`)

- [ ] **Step 2.1: Write the five failing tests**

Append to `internal/grpc/streaming_test.go` immediately after `TestStreaming_CriteriaResponse` (around `:488`, before `TestStreaming_KeepAliveTimeout` at `:490`):

```go
// --- Retryable flag plumbing (issue #262) ----------------------------------
//
// These tests cover the wire tri-state: retryable=true / retryable=false /
// retryable absent on a present error / no error at all. All four must
// round-trip into ProcessingResponse.Retryable as a *bool whose presence
// distinguishes "wire said so" from "wire didn't say". Plus one symmetric
// test on the criteria response handler — both halves of #254's eventual
// retry loop need this data, so both are surfaced now.

// retryableProcessorRespFixture spins up a streaming session and returns
// the registered memberID + the response channel for a tracked request.
// Common setup for all retryable tests — extracted to keep each test
// focused on the assertion. Caller MUST stream.closeRecv() and <-done.
type retryableHarness struct {
	svc      *CloudEventsServiceImpl
	stream   *mockBidiStream
	done     chan error
	memberID string
	respCh   <-chan *ProcessingResponse
}

func newRetryableHarness(t *testing.T, requestID string) *retryableHarness {
	t.Helper()
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	t.Cleanup(cancel)

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")
	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest(requestID)
	return &retryableHarness{svc, stream, done, memberID, respCh}
}

func (h *retryableHarness) closeAndWait() {
	h.stream.closeRecv()
	<-h.done
}

// awaitResponse blocks until a response is routed (or fails the test on
// timeout). Returns the captured *ProcessingResponse for assertion.
func (h *retryableHarness) awaitResponse(t *testing.T) *ProcessingResponse {
	t.Helper()
	select {
	case resp := <-h.respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		return resp
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
		return nil
	}
}

func TestStreaming_ProcessorResponse_PropagatesRetryableFalse(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-false")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-false",
		"success":   false,
		"error": map[string]any{
			"message":   "boom",
			"retryable": false,
		},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable == nil {
		t.Fatal("expected Retryable to be non-nil (wire said retryable=false)")
	}
	if *resp.Retryable {
		t.Errorf("expected Retryable=false, got true")
	}
}

func TestStreaming_ProcessorResponse_PropagatesRetryableTrue(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-true")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-true",
		"success":   false,
		"error": map[string]any{
			"message":   "transient",
			"retryable": true,
		},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable == nil {
		t.Fatal("expected Retryable to be non-nil (wire said retryable=true)")
	}
	if !*resp.Retryable {
		t.Errorf("expected Retryable=true, got false")
	}
}

func TestStreaming_ProcessorResponse_RetryableNilWhenAbsent(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-absent")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-absent",
		"success":   false,
		"error": map[string]any{
			"message": "no flag set",
		},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable != nil {
		t.Errorf("expected Retryable to be nil when wire omitted the key; got *Retryable=%v", *resp.Retryable)
	}
}

func TestStreaming_ProcessorResponse_RetryableNilOnSuccess(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-success")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-success",
		"success":   true,
		"payload":   map[string]any{"data": map[string]any{"ok": true}},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable != nil {
		t.Errorf("expected Retryable to be nil on success (no error block); got *Retryable=%v", *resp.Retryable)
	}
}

func TestStreaming_CriteriaResponse_PropagatesRetryableFlag(t *testing.T) {
	// Symmetric to the processor case: criteria responses can also carry a
	// member-vetoed retryable=false on the inbound error shape (see
	// api/grpc/events/types.go — Retryable declared on every CloudEvent
	// error variant). The future retry loop in #254 covers the criteria
	// dispatch path at internal/grpc/dispatch.go:181-260, so symmetry now
	// avoids a second streaming-handler edit later.
	t.Run("retryable_false", func(t *testing.T) {
		h := newRetryableHarness(t, "req-cri-false")
		defer h.closeAndWait()

		respPayload := map[string]any{
			"requestId": "req-cri-false",
			"success":   false,
			"matches":   false,
			"error": map[string]any{
				"message":   "criteria boom",
				"retryable": false,
			},
		}
		respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
		if err != nil {
			t.Fatalf("failed to create criteria response event: %v", err)
		}
		h.stream.enqueue(respCE)

		resp := h.awaitResponse(t)
		if resp.Retryable == nil || *resp.Retryable {
			t.Errorf("expected Retryable=false on criteria response; got %v", resp.Retryable)
		}
	})

	t.Run("absent_on_success", func(t *testing.T) {
		h := newRetryableHarness(t, "req-cri-success")
		defer h.closeAndWait()

		respPayload := map[string]any{
			"requestId": "req-cri-success",
			"success":   true,
			"matches":   true,
		}
		respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
		if err != nil {
			t.Fatalf("failed to create criteria response event: %v", err)
		}
		h.stream.enqueue(respCE)

		resp := h.awaitResponse(t)
		if resp.Retryable != nil {
			t.Errorf("expected Retryable=nil on criteria success; got *Retryable=%v", *resp.Retryable)
		}
	})
}
```

- [ ] **Step 2.2: Run tests to verify they fail with a compile error**

```bash
go test ./internal/grpc/ -run 'TestStreaming_(ProcessorResponse_(PropagatesRetryable|RetryableNil)|CriteriaResponse_PropagatesRetryableFlag)' -v
```

Expected: **compile error** — `resp.Retryable undefined (type *ProcessingResponse has no field or method Retryable)`. This is the expected RED state; Steps 2.3-2.4 add the field and plumbing.

- [ ] **Step 2.3: Add Retryable *bool to ProcessingResponse**

In `internal/grpc/members.go`, replace the struct definition at `:24-30`:

```go
// ProcessingResponse holds the response from a processor or criteria calculation.
type ProcessingResponse struct {
	Payload  json.RawMessage
	Success  bool
	Error    string
	Matches  *bool    // for criteria responses (nil for processor responses)
	Warnings []string // warnings from processor/criteria, propagated to client
	// Retryable carries the member-supplied retryable flag from the inbound
	// CloudEvent error shape (api/grpc/events/types.go: every *EventJsonError
	// variant declares Retryable *bool). The pointer is nil when the wire
	// omitted the key or when no error was present, distinguishing "wire
	// said so" from "wire didn't say". Captured here for the future retry
	// loop in #254; the current dispatcher is single-shot and does not
	// consult this field.
	Retryable *bool
}
```

- [ ] **Step 2.4: Widen both streaming handlers to capture and propagate the flag**

In `internal/grpc/streaming.go`, replace `handleProcessorResponse` (`:241-273`):

```go
// handleProcessorResponse routes a processor calculation response to the
// pending request on the given member.
func (s *CloudEventsServiceImpl) handleProcessorResponse(memberID string, payload json.RawMessage) {
	var resp struct {
		RequestID string `json:"requestId"`
		Success   bool   `json:"success"`
		Error     *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable,omitempty"`
		} `json:"error"`
		Warnings []string        `json:"warnings"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal processor response", "pkg", "grpc", "memberId", memberID, "error", err)
		return
	}

	member := s.registry.Get(memberID)
	if member == nil {
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Payload:   resp.Payload,
		Success:   resp.Success,
		Error:     errMsg,
		Warnings:  resp.Warnings,
		Retryable: retryable,
	})
}
```

And replace `handleCriteriaResponse` (`:277-308`):

```go
// handleCriteriaResponse routes a criteria calculation response to the
// pending request on the given member.
func (s *CloudEventsServiceImpl) handleCriteriaResponse(memberID string, payload json.RawMessage) {
	var resp struct {
		RequestID string `json:"requestId"`
		Success   bool   `json:"success"`
		Matches   bool   `json:"matches"`
		Error     *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable,omitempty"`
		} `json:"error"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal criteria response", "pkg", "grpc", "memberId", memberID, "error", err)
		return
	}

	member := s.registry.Get(memberID)
	if member == nil {
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	matches := resp.Matches
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Success:   resp.Success,
		Error:     errMsg,
		Matches:   &matches,
		Warnings:  resp.Warnings,
		Retryable: retryable,
	})
}
```

- [ ] **Step 2.5: Run tests to verify they pass**

```bash
go test ./internal/grpc/ -run 'TestStreaming_(ProcessorResponse_(PropagatesRetryable|RetryableNil)|CriteriaResponse_PropagatesRetryableFlag)' -v
```

Expected: all six sub-tests **PASS** (four processor-side + two criteria-side subtests).

Then run the whole grpc package to ensure no regression:

```bash
go test ./internal/grpc/ -v
```

Expected: all tests pass (the existing `TestStreaming_ProcessorResponse` and `TestStreaming_CriteriaResponse` still work — their payloads don't set `error`, so `Retryable` stays nil and they don't assert on it).

- [ ] **Step 2.6: Commit**

```bash
git add internal/grpc/members.go internal/grpc/streaming.go internal/grpc/streaming_test.go
git -c commit.gpgsign=false commit -m "feat(grpc): surface inbound retryable flag on ProcessingResponse — refs #262

Adds Retryable *bool to ProcessingResponse (internal/grpc/members.go) and
plumbs it from the inbound CloudEvent error shape in both processor and
criteria response handlers. The pointer distinguishes wire-tri-state:
true / false / absent on a present error / no error at all.

Wire field has been declared on every *EventJsonError variant since the
CloudEvent schema was generated (api/grpc/events/types.go:36, 111, 189,
261, 2536, …); this PR starts reading it.

No consumer yet — the dispatcher is single-shot regardless of policy.
The retry loop that consults Retryable ships in #254. Both processor
and criteria handlers updated for symmetry (the future loop covers
both dispatch paths at internal/grpc/dispatch.go:46-135 and :181-260).

Refs #262, #254

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Tighten OpenAPI retryPolicy schema with an enum

**Files:**
- Modify: `api/openapi.yaml:8120-8122` (on `ExternalizedFunctionConfigDto`) and `:8785-8787` (on `ExternalizedProcessorConfigDto`)

- [ ] **Step 3.1: Update the first retryPolicy schema occurrence (line ~8120)**

In `api/openapi.yaml`, replace the three lines at `:8120-8122` (on `ExternalizedFunctionConfigDto`):

```yaml
        retryPolicy:
          type: string
          description: Retry policy for the function
```

with:

```yaml
        retryPolicy:
          type: string
          enum: [NONE, FIXED]
          description: |
            Retry policy selector. NONE → single attempt, no retry. FIXED →
            up to N additional attempts with fixed delay between tries
            (N and delay are server-configured). When omitted, defaults
            to FIXED at engine fire. Import-time validation rejects any
            other value.
```

- [ ] **Step 3.2: Update the second retryPolicy schema occurrence (line ~8785)**

After Step 3.1, the second occurrence's line number will have shifted slightly. Search for the second `retryPolicy:` (on `ExternalizedProcessorConfigDto`) and apply the identical replacement — same `type`, same `enum`, same `description` block.

Verify with:

```bash
grep -c "enum: \[NONE, FIXED\]" api/openapi.yaml
```

Expected output: `2`.

- [ ] **Step 3.3: Re-run any OpenAPI conformance tests**

```bash
go test ./internal/e2e/ -run 'OpenAPI|Conformance' -v 2>&1 | tail -30
```

Expected: no new failures from the YAML edit. The conformance test lives at `internal/e2e/zzz_openapi_conformance_test.go` and validates the embedded spec against the running server's responses; an enum tightening that the validator already enforces should be a no-op.

If `oapi-codegen` regeneration is part of the build, run `go generate ./api/...` and verify the generated DTO still compiles (the field stays `*string` — adding an enum to the schema doesn't change the generated Go type for OpenAPI 3.0). Per memory `project_openapi_spec_embed_via_goembed.md`, the spec is embedded via `//go:embed` (not native oapi-codegen embed); the regeneration step matters only if `api/generated.go` regen is in the workflow.

Verify the build still compiles:

```bash
go build ./...
```

Expected: clean build.

- [ ] **Step 3.4: Verify the help-shipped spec reflects the enum**

```bash
go build -o /tmp/cyoda-help-test ./cmd/cyoda
/tmp/cyoda-help-test help openapi yaml | grep -A 3 "^        retryPolicy:" | head -20
rm /tmp/cyoda-help-test
```

Expected: two stanzas, each showing `enum:` with `- NONE` and `- FIXED` (or `[NONE, FIXED]` flow-style — both are valid YAML).

- [ ] **Step 3.5: Commit**

```bash
git add api/openapi.yaml
git -c commit.gpgsign=false commit -m "feat(api): retryPolicy schema enum (NONE, FIXED) — refs #262

Tightens api/openapi.yaml retryPolicy schema on both
ExternalizedFunctionConfigDto and ExternalizedProcessorConfigDto with
\`enum: [NONE, FIXED]\` and an accurate description (the previous
description was a one-liner; new text clarifies the NONE/FIXED semantics
and the FIXED default-when-omitted behaviour).

Non-breaking: the import validator from the previous commit rejects
unknown values at the cyoda-go boundary, so any input the new enum
would reject was already going to be rejected. The schema tightening
flows automatically to clients via cyoda help openapi {json,yaml}.

Refs #262

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Update workflows help topic

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md` — the `retryPolicy` bullet at `:174` (current text is wrong about the empty-default), and the rejected-imports bullet list around `:318`

- [ ] **Step 4.1: Replace the retryPolicy ProcessorConfig bullet**

In `cmd/cyoda/help/content/workflows.md`, replace the single line at `:174`:

```markdown
- `retryPolicy` — string — retry policy name (plugin/platform-defined); empty means no retry
```

with a multi-line bullet matching the style of the surrounding `asyncResult` / `crossoverToAsyncMs` bullets (`:176-186`):

```markdown
- `retryPolicy` — string — selects the server-resolved retry strategy.
  Valid values: `NONE` (single attempt, no retry), `FIXED` (up to N
  additional attempts with fixed delay between tries, where N and delay
  are server-configured). When omitted, defaults to `FIXED` at engine
  fire. Import-time validation rejects any other value with `400
  VALIDATION_FAILED`. **cyoda-go status:** captured but not consumed —
  the dispatcher is single-shot regardless of policy; the full retry
  loop ships in a later release. Cloud honours both policies.
```

- [ ] **Step 4.2: Add a bullet to the rejected-imports list**

In `cmd/cyoda/help/content/workflows.md`, insert the following line **immediately after** the existing `Unknown executionMode value…` line at `:318`:

```markdown
- Unknown `retryPolicy` value on any processor (allowed: `NONE`, `FIXED`, or empty).
```

(The list is alphabetic-ish by topic but the audit groups validation rules together; inserting right after the executionMode bullet keeps the enum-rules clustered.)

- [ ] **Step 4.3: Verify the help topic renders sensibly**

```bash
go build -o /tmp/cyoda-help-test ./cmd/cyoda
/tmp/cyoda-help-test help workflows | grep -A 7 "^- .retryPolicy.\|^- Unknown .retryPolicy." 
rm /tmp/cyoda-help-test
```

Expected: the bullet text appears as written, with the new rejected-imports line surfacing.

Also run any help-content tests:

```bash
go test ./cmd/cyoda/help/... -v
```

Expected: pass (these tests check topic-existence and structural shape, not text content).

- [ ] **Step 4.4: Commit**

```bash
git add cmd/cyoda/help/content/workflows.md
git -c commit.gpgsign=false commit -m "docs(help): correct retryPolicy semantics in workflows topic — refs #262

The existing retryPolicy bullet said 'empty means no retry', which is
wrong: per audit doc §M1 (and Cloud semantics), an empty retryPolicy
defaults to FIXED at engine fire. Rewritten to match the asyncResult /
crossoverToAsyncMs bullet style: list the two valid values, note the
default, and call out the cyoda-go status (captured-but-not-consumed
until the retry loop in #254 lands).

Also adds the new validation rule to the rejected-imports list so
operators can locate the rule alongside the existing executionMode and
startNewTxOnDispatch coherence checks.

Refs #262

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Flip audit doc §M1 status lines

**Files:**
- Modify: `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` — three spots (lines around `:331`, `:410`, `:729`)

- [ ] **Step 5.1: Update the §M1 special-case-RetryPolicy reference table line**

In `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md`, replace the line at `:331` (note: the line number may have shifted slightly after prior commits — search for the exact text):

```
| `ProcessorConfig.RetryPolicy` | `api/openapi.yaml:8619–8621`, `cyoda-go-spi@v0.7.1/types.go:152` | **No.** Only references are the generated DTO and the SPI struct. |
```

with:

```
| `ProcessorConfig.RetryPolicy` | `api/openapi.yaml:8120–8126`, `cyoda-go-spi@v0.7.1/types.go:152` | **Validated at import (#262).** Rejected at import unless ∈ {NONE, FIXED, ""}. Dispatcher still single-shot; full retry loop deferred to #254. |
```

(Line range updated to reflect the post-Task-3 schema size, and the consumer column flipped from "No" to the import-validation status.)

- [ ] **Step 5.2: Update the "flag is not surfaced into ProcessingResponse" sentence**

Search for the sentence at `:410` containing `flag is not surfaced into ProcessingResponse`:

```
But the
flag is not surfaced into `ProcessingResponse`
(`internal/grpc/members.go:24` — the struct exposes `Success`, `Error`,
`Matches`, `Warnings` only), so the dispatcher could not consult it today
even if it tried.
```

Replace with:

```
The flag is surfaced into `ProcessingResponse.Retryable` as a `*bool`
(`internal/grpc/members.go:24` — captured by both processor and criteria
response handlers since #262), but the dispatcher does not yet consult it
— that wiring is deferred to #254.
```

- [ ] **Step 5.3: Update the consumption-table line at :729**

In `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md`, replace the line at `:729`:

```
| `ProcessorConfig.RetryPolicy` | DEAD | no consumer |
```

with:

```
| `ProcessorConfig.RetryPolicy` | IMPORT-VALIDATED | rejected at import unless ∈ {NONE, FIXED, ""} (validate.go); no dispatch consumer yet — see #254 |
```

- [ ] **Step 5.4: Update the §10.x summary mentioning RetryPolicy as an implementation gap**

At line `:737-738`:

```
- `ProcessorConfig.Context` and `ProcessorConfig.RetryPolicy` are
  **implementation gaps**, not contract artefacts — see the special-case
  subsections in §M1 above.
```

Update to:

```
- `ProcessorConfig.RetryPolicy` is import-validated (#262) but the
  dispatcher does not yet consume it — the full retry loop is deferred
  to #254. (`ProcessorConfig.Context` was resolved in v0.8.0 by #253.)
```

- [ ] **Step 5.5: Verify the audit doc still renders coherently**

```bash
grep -n "RetryPolicy" docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md | head -20
```

Expected: no orphan "DEAD" or "no consumer" markers next to RetryPolicy. The §3.5 carve-out roadmap line at `:67` and the §9 multi-part scope at `:904+` stay untouched — they remain accurate as forward-looking references to #254.

- [ ] **Step 5.6: Commit**

```bash
git add docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md
git -c commit.gpgsign=false commit -m "docs(audit): flip §M1 status lines for RetryPolicy + Retryable — refs #262

The audit doc previously listed RetryPolicy as DEAD/no-consumer and the
ProcessingResponse.Retryable plumbing as not-surfaced. Both halves change
status after #262: validator rejects unknown policies at import, and the
inbound retryable flag now round-trips into ProcessingResponse on both
processor and criteria response paths.

The gap-list pointing at #254 (retry loop, exclusion-set FindByTags,
server-side config keys, async-suppression, aggregated-failure shape)
stays accurate and is unmodified.

Refs #262

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: End-of-deliverable verification

- [ ] **Step 6.1: Run the full workflow + grpc test packages**

```bash
go test ./internal/domain/workflow/... ./internal/grpc/... -v
```

Expected: all tests pass.

- [ ] **Step 6.2: Run the full root module test suite**

```bash
go test ./... -v 2>&1 | tail -50
```

Expected: all packages pass. E2E tests require Docker — they will spin up postgres testcontainers automatically.

- [ ] **Step 6.3: Run the plugin submodule aggregator (per project convention)**

```bash
make test-all 2>&1 | tail -30
```

Expected: root + `plugins/memory|sqlite|postgres` all pass. Postgres needs Docker.

If `make test-all` reveals plugin failures unrelated to this PR, surface them rather than push through.

- [ ] **Step 6.4: Run go vet**

```bash
go vet ./...
```

Expected: no warnings.

- [ ] **Step 6.5: Race detector — one-shot end-of-deliverable sanity check**

Per `.claude/rules/race-testing.md`, this is run once per deliverable, not per task.

```bash
go test -race ./...
```

Expected: no race detector reports. (Streaming tests use goroutines + channels — this catches any unguarded shared state introduced by the new tests.)

- [ ] **Step 6.6: Verify the spec acceptance criteria are met**

Open `docs/superpowers/specs/2026-06-15-issue-262-retrypolicy-validation-design.md` and walk the "Acceptance criteria mapping" section. Each bullet should now correspond to passing tests in Tasks 1-2. Confirm by re-running:

```bash
go test ./internal/domain/workflow/ -run 'RetryPolicy' -v
go test ./internal/grpc/ -run 'Retryable' -v
```

Expected: every named test from the spec is present and green.

- [ ] **Step 6.7: Final git log review**

```bash
git log --oneline origin/release/v0.8.0..HEAD
```

Expected: six commits in this order (or close — Tasks 3-5 can be reordered):
1. `docs(superpowers): spec for #262 — RetryPolicy import validation + Retryable plumbing`
2. `feat(workflow): reject unknown RetryPolicy at import — refs #262`
3. `feat(grpc): surface inbound retryable flag on ProcessingResponse — refs #262`
4. `feat(api): retryPolicy schema enum (NONE, FIXED) — refs #262`
5. `docs(help): correct retryPolicy semantics in workflows topic — refs #262`
6. `docs(audit): flip §M1 status lines for RetryPolicy + Retryable — refs #262`

---

## Task 7: PR submission

Per `memory/feedback_release_branch_for_milestones.md` and `memory/feedback_release_branch_issue_closure.md`:
- Target branch is `release/v0.8.0`, not `main`.
- `Closes #262` MUST appear in the PR body so the milestone bookkeeping is correct at release-merge time.

- [ ] **Step 7.1: Push the branch**

```bash
git push -u origin feat/262-retrypolicy-validation
```

- [ ] **Step 7.2: Open the PR against release/v0.8.0**

```bash
gh pr create \
  --base release/v0.8.0 \
  --head feat/262-retrypolicy-validation \
  --title "feat(workflow): RetryPolicy import-time validation + surface inbound retryable flag — closes #262" \
  --body "$(cat <<'EOF'
## What

v0.8.0 carve-out from #254. Lands the **import-side guard and the wire-level flag plumbing** for `RetryPolicy`. The full member-failover retry loop stays in #254 for a later release.

### Validation
- Rejects unknown `RetryPolicy` values at import (allowed: `NONE`, `FIXED`, or empty → defaults to FIXED at engine fire). Mirrors the ExecutionMode validator (#255) and asyncResult validator (#261).

### Plumbing
- Adds `Retryable *bool` to `internal/grpc/members.go:ProcessingResponse`.
- Captures the inbound flag from the CloudEvent error shape in both `handleProcessorResponse` and `handleCriteriaResponse`.
- Pointer is nil when wire omitted the key or no error was present, distinguishing "wire said so" from "wire didn't say".

### Docs / schema
- OpenAPI `retryPolicy` schema gets `enum: [NONE, FIXED]` (both DTO occurrences). Non-breaking — the validator runs first; any input the enum would reject is already rejected by the validator.
- `cmd/cyoda/help/content/workflows.md` retryPolicy bullet rewritten — the current "empty means no retry" was wrong (empty defaults to FIXED). Also adds the new validation rule to the rejected-imports list.
- `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M1 status lines flipped — RetryPolicy is now import-validated, Retryable is surfaced.

### No behaviour change to dispatch
- The dispatcher remains single-shot. `ProcessingResponse.Retryable` has zero consumers in this PR. The retry loop, exclusion-set `FindByTags`, server-side config keys, async-suppression, and aggregated-failure error shape all stay in #254.

## Spec + plan

- Spec: `docs/superpowers/specs/2026-06-15-issue-262-retrypolicy-validation-design.md`
- Plan: `docs/superpowers/plans/2026-06-15-issue-262-retrypolicy-validation.md`

## Verification

- `go test ./... -v` — green
- `go test -race ./...` — clean
- `make test-all` — root + plugin submodules green
- `go vet ./...` — clean

Closes #262

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)" \
  --milestone v0.8.0
```

Expected: PR created, milestone set, base branch is `release/v0.8.0`.

- [ ] **Step 7.3: Verify the PR carries the milestone**

```bash
gh pr view --json milestone,baseRefName | jq
```

Expected:
```json
{
  "milestone": { "title": "v0.8.0", ... },
  "baseRefName": "release/v0.8.0"
}
```

Per `memory/feedback_release_milestone_invariant.md` — un-milestoned closed-by-release-PR issues are invisible to release notes. The `--milestone v0.8.0` flag above is the safeguard.

---

## Self-review checklist

After the plan is complete, before kicking off implementation, the executing agent must confirm:

- [ ] Each spec acceptance criterion maps to a green test in Tasks 1-2 (Step 6.6 verifies this).
- [ ] No placeholders, no "TBD", no "add error handling" — every step contains the actual content.
- [ ] Type and name consistency — `RetryPolicyNone` and `RetryPolicyFixed` defined in Task 1 are referenced in Task 1 only; `Retryable *bool` defined in Task 2 Step 2.3 is referenced in Task 2 Steps 2.4 and the test fixtures (Step 2.1).
- [ ] Each task commits independently with `Refs #262` in the trailer; the final PR body uses `Closes #262`.
- [ ] Tests are RED-first: Step X.1 writes the failing test, Step X.2 confirms it fails, Step X.3-X.4 implement, Step X.5 confirms it passes.
- [ ] No SPI changes are introduced.
- [ ] No env-var changes are introduced.
- [ ] No edits to `internal/grpc/dispatch.go` (the acceptance criterion "No behaviour change to the dispatch path itself").
