# Issue #261 — asyncResult + crossoverToAsyncMs SPI Shape Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `AsyncResult *bool` and `CrossoverToAsyncMs *int64` to the SPI `ProcessorConfig`, regenerate the DTO, and reject `asyncResult=true` or any non-nil `crossoverToAsyncMs` at workflow import with `400 VALIDATION_FAILED`.

**Architecture:** SPI carve-out + cyoda-go validator rule + documentation sync. The SPI repo (`../../../../cyoda-go-spi`, working tree at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`) gains the two pointer fields with `omitempty` (precedent: `StartNewTxOnDispatch *bool`). The cyoda-go side pseudo-version-pins to SPI `main` HEAD across all four `go.mod` files, regenerates the DTO (descriptions only — the wire fields were already there), and adds two location-naming validator rules inside `validateWorkflowStructure`'s per-processor loop. No engine runtime work — the audit-M6 finding closes via loud import-time rejection rather than the originally proposed WARN (project-lead override; spec §1 records the rationale).

**Tech Stack:** Go 1.26.4, `encoding/json` with omitempty for pointer round-trip, oapi-codegen-style generated DTO embedded via `//go:embed`, testcontainers for E2E PostgreSQL, in-process `httptest.Server` for E2E HTTP path.

**Spec:** `docs/superpowers/specs/2026-06-15-issue-261-asyncresult-crossover-spi-design.md` rev 1.

---

## File Structure

**cyoda-go-spi repo** (`/Users/paul/go-projects/cyoda-light/cyoda-go-spi`)
- Modify: `types.go` — add two fields to `ProcessorConfig`
- Modify: `types_test.go` — add round-trip test
- Modify: `CHANGELOG.md` — `[Unreleased] ### Added` entry

**cyoda-go repo** (this worktree)
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod` — pseudo-version bump
- Modify: `go.sum`, `plugins/memory/go.sum`, `plugins/sqlite/go.sum`, `plugins/postgres/go.sum` — refreshed by `go mod tidy`
- Modify: `api/openapi.yaml` — amend descriptions on `asyncResult` (line 8798) and `crossoverToAsyncMs` (line 8805)
- Modify: `api/generated.go` — regenerated; description text only
- Modify: `internal/domain/workflow/validate.go` — two new rules in `validateWorkflowStructure`'s per-processor loop
- Modify: `internal/domain/workflow/validate_import_test.go` — 8 new test functions
- Create: `internal/domain/workflow/async_result_roundtrip_test.go` — SPI-level round-trip + legacy-data export
- Create: `internal/e2e/async_result_import_reject_test.go` — E2E HTTP-stack reject tests
- Modify: `e2e/externalapi/scenarios/08-workflow-import-export.yaml` — append `wf-import/10` + `wf-import/11`
- Modify: `e2e/parity/externalapi/workflow_import_export.go` — register + implement two new runners
- Modify: `docs/cyoda/cloud-divergences.md` — rows 16, 17
- Modify: `cmd/cyoda/help/content/workflows.md` — extend `ProcessorConfig fields` bullet list

**Order of operations:** SPI first (Phase A) → cyoda-go pin bump (Phase B) → validator TDD (Phase C) → OpenAPI/DTO sync (Phase D) → round-trip tests (Phase E) → E2E (Phase F) → parity (Phase G) → docs (Phase H) → final verification (Phase I).

---

## Phase A — SPI repo changes

### Task A1: Add failing SPI round-trip test for AsyncResult + CrossoverToAsyncMs

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_test.go`

- [ ] **Step 1: Append the failing test to `types_test.go`**

Append at the end of the file (after `TestTransitionDefinition_Schedule_RoundTrips`):

```go
func TestProcessorConfig_AsyncResultAndCrossover_RoundTrips(t *testing.T) {
	// Helper to make pointer literals readable.
	boolPtr := func(b bool) *bool { return &b }
	i64Ptr := func(i int64) *int64 { return &i }

	cases := []struct {
		name          string
		cfg           ProcessorConfig
		wantInJSON    []string // substrings that MUST appear
		wantNotInJSON []string // substrings that MUST NOT appear
	}{
		{
			name:          "both_nil_omitted",
			cfg:           ProcessorConfig{},
			wantNotInJSON: []string{"asyncResult", "crossoverToAsyncMs"},
		},
		{
			name:          "async_true_only",
			cfg:           ProcessorConfig{AsyncResult: boolPtr(true)},
			wantInJSON:    []string{`"asyncResult":true`},
			wantNotInJSON: []string{"crossoverToAsyncMs"},
		},
		{
			name:          "async_false_only",
			cfg:           ProcessorConfig{AsyncResult: boolPtr(false)},
			wantInJSON:    []string{`"asyncResult":false`},
			wantNotInJSON: []string{"crossoverToAsyncMs"},
		},
		{
			name:          "crossover_zero_only",
			cfg:           ProcessorConfig{CrossoverToAsyncMs: i64Ptr(0)},
			wantInJSON:    []string{`"crossoverToAsyncMs":0`},
			wantNotInJSON: []string{"asyncResult"},
		},
		{
			name:          "crossover_positive_only",
			cfg:           ProcessorConfig{CrossoverToAsyncMs: i64Ptr(5000)},
			wantInJSON:    []string{`"crossoverToAsyncMs":5000`},
			wantNotInJSON: []string{"asyncResult"},
		},
		{
			name:       "both_set",
			cfg:        ProcessorConfig{AsyncResult: boolPtr(true), CrossoverToAsyncMs: i64Ptr(5000)},
			wantInJSON: []string{`"asyncResult":true`, `"crossoverToAsyncMs":5000`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantInJSON {
				if !strings.Contains(string(bs), want) {
					t.Errorf("expected JSON to contain %q; got %s", want, bs)
				}
			}
			for _, notWant := range tc.wantNotInJSON {
				if strings.Contains(string(bs), notWant) {
					t.Errorf("expected JSON NOT to contain %q; got %s", notWant, bs)
				}
			}

			var back ProcessorConfig
			if err := json.Unmarshal(bs, &back); err != nil {
				t.Fatal(err)
			}
			// Pointer-state-preserving equality for AsyncResult.
			if (back.AsyncResult == nil) != (tc.cfg.AsyncResult == nil) {
				t.Errorf("AsyncResult pointer-presence mismatched: got %v, want %v",
					back.AsyncResult, tc.cfg.AsyncResult)
			}
			if back.AsyncResult != nil && tc.cfg.AsyncResult != nil &&
				*back.AsyncResult != *tc.cfg.AsyncResult {
				t.Errorf("AsyncResult value mismatched: got %v, want %v",
					*back.AsyncResult, *tc.cfg.AsyncResult)
			}
			// Pointer-state-preserving equality for CrossoverToAsyncMs.
			if (back.CrossoverToAsyncMs == nil) != (tc.cfg.CrossoverToAsyncMs == nil) {
				t.Errorf("CrossoverToAsyncMs pointer-presence mismatched: got %v, want %v",
					back.CrossoverToAsyncMs, tc.cfg.CrossoverToAsyncMs)
			}
			if back.CrossoverToAsyncMs != nil && tc.cfg.CrossoverToAsyncMs != nil &&
				*back.CrossoverToAsyncMs != *tc.cfg.CrossoverToAsyncMs {
				t.Errorf("CrossoverToAsyncMs value mismatched: got %d, want %d",
					*back.CrossoverToAsyncMs, *tc.cfg.CrossoverToAsyncMs)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run TestProcessorConfig_AsyncResultAndCrossover_RoundTrips -v
```

Expected: compile-time failure — `back.AsyncResult undefined`, `tc.cfg.CrossoverToAsyncMs undefined`. This proves the test is wired correctly to the missing fields.

### Task A2: Add the SPI fields to make the test pass

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go`

- [ ] **Step 1: Edit `ProcessorConfig` to add the two new fields with docstrings**

Replace the existing struct (currently lines 158–173) with:

```go
// ProcessorConfig holds configuration for a processor.
type ProcessorConfig struct {
	AttachEntity         bool   `json:"attachEntity,omitempty"`
	CalculationNodesTags string `json:"calculationNodesTags,omitempty"`
	ResponseTimeoutMs    int64  `json:"responseTimeoutMs,omitempty"`
	RetryPolicy          string `json:"retryPolicy,omitempty"`
	Context              string `json:"context,omitempty"`
	// StartNewTxOnDispatch, when true and ExecutionMode is COMMIT_BEFORE_DISPATCH,
	// causes the cascade engine to open a fresh transaction before dispatching
	// the processor (so the processor may perform transactional work via that
	// tx's token). When false (default) the processor runs with no transaction
	// context and the connection is released entirely during dispatch.
	// Ignored for any other ExecutionMode.
	StartNewTxOnDispatch *bool `json:"startNewTxOnDispatch,omitempty"`

	// AsyncResult, when true, requests that the cascade engine suspend
	// the transaction at processor dispatch and resume only when the
	// processor's result eventually arrives via the async-result
	// delivery slot. The runtime that implements this — durable
	// suspend state, work-stealing recovery, distributed timer
	// coordination — is gated on storage-engine primitives not
	// available in every backend. Consuming engines that do not
	// implement async-result semantics MUST reject this field at
	// import (or the equivalent configuration-boundary) rather than
	// silently degrade to synchronous dispatch.
	//
	// Pointer so that nil (absent) and &false (explicit no-async) are
	// distinguishable on the wire and round-trip byte-equivalent.
	AsyncResult *bool `json:"asyncResult,omitempty"`

	// CrossoverToAsyncMs is the timer, in milliseconds, after which
	// the engine crosses over from sync-wait to async-result delivery
	// for an AsyncResult=true processor. Effective only when
	// AsyncResult is true. Consuming engines that do not implement
	// async-result semantics MUST reject any non-nil value at import.
	CrossoverToAsyncMs *int64 `json:"crossoverToAsyncMs,omitempty"`
}
```

- [ ] **Step 2: Run the test and verify it passes**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run TestProcessorConfig_AsyncResultAndCrossover_RoundTrips -v
```

Expected: PASS — all six subtests green.

- [ ] **Step 3: Run the full SPI test suite to verify nothing regressed**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -v
```

Expected: all tests PASS, no regressions.

- [ ] **Step 4: Run `go vet`**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./...
```

Expected: no output, exit 0.

### Task A3: Update SPI CHANGELOG

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/CHANGELOG.md`

- [ ] **Step 1: Append the new bullet to the existing `[Unreleased] ### Added` section**

Find the existing `[Unreleased] ### Added` section. After the bullet that begins with `` `TransitionSchedule` type + `TransitionDefinition.Schedule` field `` (around line ~55 — exact line depends on prior edits), append a new bullet:

```markdown
- `ProcessorConfig.AsyncResult *bool` and
  `ProcessorConfig.CrossoverToAsyncMs *int64` for the async-result /
  crossover-timer configuration shape carve-out (cyoda-go #261). The
  fields are pointer-typed (omitempty) so the absent case round-trips
  byte-equivalent. Runtime not yet wired — see cyoda-go #223 for full
  feature tracking. Consuming engines that do not implement
  async-result semantics MUST reject non-default values at the
  configuration-import boundary rather than silently degrade.
```

### Task A4: Commit and push SPI changes

- [ ] **Step 1: Stage and commit**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add types.go types_test.go CHANGELOG.md
git status
```

Expected: three files staged.

- [ ] **Step 2: Commit**

```bash
git commit -m "feat(types): add ProcessorConfig.AsyncResult + CrossoverToAsyncMs (cyoda-go #261)

Pointer-typed fields with omitempty so the absent case round-trips
byte-equivalent. Precedent: StartNewTxOnDispatch *bool on the same
struct. Docstrings record the contract that consuming engines without
async-result runtime support MUST reject non-default values at the
configuration-import boundary rather than silently degrade.

Runtime not wired — see cyoda-go #223 for full feature tracking.

Bundled into the v0.8.0 milestone tag per MAINTAINING.md cadence; no
per-PR tag.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 3: Push to SPI main**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push origin main
```

Expected: push accepted. Capture the new HEAD SHA — needed for the pseudo-version pin in Phase B.

```bash
git rev-parse HEAD
```

Record the SHA (12-char prefix is what `go get` uses in the pseudo-version).

---

## Phase B — cyoda-go SPI pin bump

### Task B1: Bump pseudo-version pin across all four go.mod files

**Files:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`

- [ ] **Step 1: Run `go get` on the SPI module from each go.mod's containing directory**

From the worktree root:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/issue-261-asyncresult-crossover-spi

# Set GOPRIVATE so the bypass for sum.golang.org applies (memory project_v0_8_0_milestone_state)
export GOPRIVATE=github.com/Cyoda-platform/*

go get github.com/cyoda-platform/cyoda-go-spi@main
go mod tidy

cd plugins/memory && go get github.com/cyoda-platform/cyoda-go-spi@main && go mod tidy && cd ../..
cd plugins/sqlite && go get github.com/cyoda-platform/cyoda-go-spi@main && go mod tidy && cd ../..
cd plugins/postgres && go get github.com/cyoda-platform/cyoda-go-spi@main && go mod tidy && cd ../..
```

Expected: each `go get` reports the new pseudo-version (e.g. `v0.7.2-0.20260615HHMMSS-<sha>`), and each `go mod tidy` runs clean.

- [ ] **Step 2: Verify all four files now pin the same pseudo-version**

Run:
```bash
grep "cyoda-go-spi" go.mod plugins/memory/go.mod plugins/sqlite/go.mod plugins/postgres/go.mod
```

Expected: all four lines show the **same** pseudo-version string.

- [ ] **Step 3: Quick smoke test — compile the new SPI surface**

Run:
```bash
go build ./...
```

Expected: build succeeds. If you see `undefined: AsyncResult` errors, the pin did not pick up the new SPI commit; re-run `go get` with the explicit SHA.

- [ ] **Step 4: Commit the pin bump**

```bash
git add go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum plugins/sqlite/go.mod plugins/sqlite/go.sum plugins/postgres/go.mod plugins/postgres/go.sum
git commit -m "chore(deps): bump cyoda-go-spi pseudo-version pin for #261

Picks up the new ProcessorConfig.AsyncResult + CrossoverToAsyncMs
fields. Pseudo-version pin per memory project_v0_8_0_milestone_state
— v0.8.0 SPI work bundles into the milestone-end tag.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase C — Validator rules (TDD: red → green → commit)

### Task C1: Write the failing validator tests

**Files:**
- Modify: `internal/domain/workflow/validate_import_test.go`

- [ ] **Step 1: Append the new test functions at the end of the file**

Add the following at the very end of `validate_import_test.go`:

```go
// --- AsyncResult + CrossoverToAsyncMs reject-at-import (#261) ------------

// asyncResultRejectFixture builds a minimal valid two-state workflow with
// one externalized SYNC processor on the only transition, then lets the
// caller mutate the processor's ProcessorConfig before validation.
func asyncResultRejectFixture(mutate func(*spi.ProcessorConfig)) spi.WorkflowDefinition {
	wf := spi.WorkflowDefinition{
		Version: "1", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: false, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: ExecutionModeSync},
				}},
			}},
			"S2": {},
		},
	}
	if mutate != nil {
		mutate(&wf.States["S1"].Transitions[0].Processors[0].Config)
	}
	return wf
}

func TestValidator_AsyncResultTrue_Rejected(t *testing.T) {
	tt := true
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &tt
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for asyncResult=true, got nil")
	}
	for _, want := range []string{`workflow "wf"`, `state "S1"`, `transition "t"`, `processor "p"`, "asyncResult=true is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message must contain %q; got: %v", want, err)
		}
	}
}

func TestValidator_AsyncResultFalse_Accepted(t *testing.T) {
	ff := false
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &ff
	})
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("explicit asyncResult=false must be accepted; got: %v", err)
	}
}

func TestValidator_AsyncResultAbsent_Accepted(t *testing.T) {
	wf := asyncResultRejectFixture(nil)
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("absent asyncResult must be accepted; got: %v", err)
	}
}

func TestValidator_CrossoverOrphan_Rejected(t *testing.T) {
	cv := int64(5000)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for orphan crossoverToAsyncMs, got nil")
	}
	for _, want := range []string{`processor "p"`, "crossoverToAsyncMs is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message must contain %q; got: %v", want, err)
		}
	}
}

func TestValidator_CrossoverZero_Rejected(t *testing.T) {
	cv := int64(0)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for crossoverToAsyncMs=&0 (non-nil zero), got nil")
	}
	if !strings.Contains(err.Error(), "crossoverToAsyncMs is not supported") {
		t.Errorf("error must name the crossover rule; got: %v", err)
	}
}

func TestValidator_AsyncTrueAndCrossover_AsyncRuleFiresFirst(t *testing.T) {
	tt := true
	cv := int64(5000)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &tt
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for asyncResult=true + crossoverToAsyncMs, got nil")
	}
	// Async rule fires first per documented ordering (spec §4).
	if !strings.Contains(err.Error(), "asyncResult=true is not supported") {
		t.Errorf("expected asyncResult message (ordering: async-rule-first); got: %v", err)
	}
	if strings.Contains(err.Error(), "crossoverToAsyncMs is not supported") {
		t.Errorf("crossover message should not surface when async rule fires first; got: %v", err)
	}
}

func TestValidator_AsyncFalseAndCrossover_CrossoverRuleFires(t *testing.T) {
	ff := false
	cv := int64(5000)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &ff
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for asyncResult=&false + crossoverToAsyncMs, got nil")
	}
	if !strings.Contains(err.Error(), "crossoverToAsyncMs is not supported") {
		t.Errorf("expected crossover message when async is false; got: %v", err)
	}
}

func TestValidator_MultiProcessor_OnlyOneBad_Rejected(t *testing.T) {
	tt := true
	// Transition with three processors; the SECOND has the bad field.
	wf := spi.WorkflowDefinition{
		Version: "1", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: false, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
					{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync,
						Config: spi.ProcessorConfig{AsyncResult: &tt}},
					{Type: ProcessorTypeExternalized, Name: "p3", ExecutionMode: ExecutionModeSync},
				}},
			}},
			"S2": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error naming p2, got nil")
	}
	if !strings.Contains(err.Error(), `processor "p2"`) {
		t.Errorf("error must name the offending processor p2 specifically; got: %v", err)
	}
	if strings.Contains(err.Error(), `processor "p1"`) || strings.Contains(err.Error(), `processor "p3"`) {
		t.Errorf("error must not name unrelated processors; got: %v", err)
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run:
```bash
go test ./internal/domain/workflow/ -run "TestValidator_AsyncResult|TestValidator_Crossover|TestValidator_AsyncTrueAndCrossover|TestValidator_AsyncFalseAndCrossover|TestValidator_MultiProcessor_OnlyOneBad" -v
```

Expected: the four "Rejected" tests FAIL with `expected error … got nil` (validator silently passes today); the three "Accepted" tests PASS by accident (no current rule rejects them).

### Task C2: Implement the validator rules

**Files:**
- Modify: `internal/domain/workflow/validate.go`

- [ ] **Step 1: Insert the two new checks inside the per-processor loop**

Open `internal/domain/workflow/validate.go`. Find the per-processor loop inside `validateWorkflowStructure` (around line 198–223). The processor-name and length checks are present; the `validExecutionModes` check sits near the end (around line 220). **Insert** the two new rules **between** the processor-name length check and the `validExecutionModes` check.

Replace the existing block:

```go
			for _, p := range tr.Processors {
				if p.Name == "" {
					return fmt.Errorf("workflow %q state %q transition %q: empty processor name is not allowed",
						wf.Name, stateName, tr.Name)
				}
				if len(p.Name) > maxIdentifierLen {
					return fmt.Errorf("workflow %q state %q transition %q: processor name length %d exceeds the %d-char limit",
						wf.Name, stateName, tr.Name, len(p.Name), maxIdentifierLen)
				}
				if _, ok := validExecutionModes[p.ExecutionMode]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown executionMode %q (allowed: SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.ExecutionMode)
				}
			}
```

with:

```go
			for _, p := range tr.Processors {
				if p.Name == "" {
					return fmt.Errorf("workflow %q state %q transition %q: empty processor name is not allowed",
						wf.Name, stateName, tr.Name)
				}
				if len(p.Name) > maxIdentifierLen {
					return fmt.Errorf("workflow %q state %q transition %q: processor name length %d exceeds the %d-char limit",
						wf.Name, stateName, tr.Name, len(p.Name), maxIdentifierLen)
				}
				// M6 (audit) — asyncResult=true requests a runtime semantic
				// this backend does not implement; reject rather than
				// silently degrade to sync dispatch. Spec §5.4 of
				// docs/superpowers/specs/2026-06-15-issue-261-...
				if p.Config.AsyncResult != nil && *p.Config.AsyncResult {
					return fmt.Errorf(
						"workflow %q state %q transition %q processor %q: asyncResult=true is not supported on this backend (async/crossover semantics are not implemented)",
						wf.Name, stateName, tr.Name, p.Name)
				}
				// M6 (audit) — crossoverToAsyncMs is a tuner for the
				// asyncResult semantic. With that semantic unsupported,
				// any non-nil value has no honourable home. Includes the
				// orphan (no asyncResult=true) and the paired cases.
				if p.Config.CrossoverToAsyncMs != nil {
					return fmt.Errorf(
						"workflow %q state %q transition %q processor %q: crossoverToAsyncMs is not supported on this backend (async/crossover semantics are not implemented)",
						wf.Name, stateName, tr.Name, p.Name)
				}
				if _, ok := validExecutionModes[p.ExecutionMode]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown executionMode %q (allowed: SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.ExecutionMode)
				}
			}
```

- [ ] **Step 2: Re-run the validator tests, verify all 8 pass**

Run:
```bash
go test ./internal/domain/workflow/ -run "TestValidator_AsyncResult|TestValidator_Crossover|TestValidator_AsyncTrueAndCrossover|TestValidator_AsyncFalseAndCrossover|TestValidator_MultiProcessor_OnlyOneBad" -v
```

Expected: all 8 PASS.

- [ ] **Step 3: Run the full workflow test suite, verify nothing regresses**

Run:
```bash
go test ./internal/domain/workflow/ -v
```

Expected: all tests PASS.

### Task C3: Commit Phase C

- [ ] **Step 1: Stage and commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_import_test.go
git commit -m "feat(workflow): reject asyncResult=true + non-nil crossoverToAsyncMs at import (closes #261)

Both fields advertise an async-result / crossover-timer semantic that
cyoda-go's storage engines cannot implement (runtime gated on Cassandra-
plugin work-recovery primitives — out of scope per #223). Audit §M6
recorded the silent-drop behaviour; this carve-out converts silent drop
to loud 400 VALIDATION_FAILED at the import boundary, naming the
offending workflow/state/transition/processor in the error message.

Project-lead override of the original WARN stance: Cloud→cyoda-go
downgrade is not a flow we care about; loud rejection beats silent
semantic drift at runtime. Spec §1 records the rationale.

Behaviour:
- asyncResult=true → reject
- asyncResult=&false (explicit no-async) → accept, round-trips
- asyncResult absent → accept
- crossoverToAsyncMs != nil (any value, paired or orphan) → reject
- Async rule fires first when both are set (more semantically anchored
  message)

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase D — OpenAPI description amendment + DTO regeneration

### Task D1: Amend OpenAPI descriptions

**Files:**
- Modify: `api/openapi.yaml` (lines around 8798–8811)

- [ ] **Step 1: Edit the `asyncResult` description**

Find and replace:

```yaml
        asyncResult:
          type: boolean
          description: |
            Whether to await the result asynchronously, outside of the
            transaction. Behavior is storage-engine-plugin dependent — not
            every plugin implements crossover semantics; consult the runtime
            plugin's documentation for the supported behavior.
```

with:

```yaml
        asyncResult:
          type: boolean
          description: |
            Whether to await the result asynchronously, outside of the
            transaction. Behavior is storage-engine-plugin dependent — not
            every plugin implements crossover semantics; consult the runtime
            plugin's documentation for the supported behavior. This backend
            does not implement async/crossover semantics; imports that set
            asyncResult=true are rejected with HTTP 400 VALIDATION_FAILED.
            The field is round-tripped for nil (absent) and the explicit-
            false case.
```

- [ ] **Step 2: Edit the `crossoverToAsyncMs` description**

Find and replace:

```yaml
        crossoverToAsyncMs:
          type: integer
          format: int64
          description: |
            Crossover delay to switch to asynchronous processing (ms),
            effective only when asyncResult is true. Behavior is
            storage-engine-plugin dependent — see asyncResult.
```

with:

```yaml
        crossoverToAsyncMs:
          type: integer
          format: int64
          description: |
            Crossover delay to switch to asynchronous processing (ms),
            effective only when asyncResult is true. Behavior is
            storage-engine-plugin dependent — see asyncResult. This
            backend does not implement async/crossover semantics; imports
            that set any value for crossoverToAsyncMs are rejected with
            HTTP 400 VALIDATION_FAILED.
```

### Task D2: Regenerate the DTO

**Files:**
- Modify: `api/generated.go`

- [ ] **Step 1: Locate the codegen invocation**

Run:
```bash
grep -rn "oapi-codegen\|openapi.yaml" Makefile scripts/ 2>/dev/null | head -10
```

Capture the codegen command (likely `make generate` or `go generate ./...`).

- [ ] **Step 2: Regenerate**

Run the codegen command. If `make generate` exists, use it; otherwise look for the `//go:generate` directive in the project root.

```bash
make generate
# or
go generate ./...
```

Expected: `api/generated.go` regenerates; diff should show only description text changes on the `AsyncResult` and `CrossoverToAsyncMs` fields. **No structural changes** (the fields were already there with the same `*bool` / `*int64` types).

- [ ] **Step 3: Verify the diff is description-only**

Run:
```bash
git diff api/generated.go | grep -E "^[-+]" | grep -v "^[-+]{3}" | head -40
```

Expected: lines starting with `-` and `+` are only comment/description text — no struct field changes, no function signature changes. If you see field additions or signature changes, something is wrong; stop and surface the issue.

- [ ] **Step 4: Build and run all tests**

Run:
```bash
go vet ./...
go test ./... -short
```

Expected: vet clean; all short tests PASS.

### Task D3: Commit Phase D

- [ ] **Step 1: Stage and commit**

```bash
git add api/openapi.yaml api/generated.go
git commit -m "docs(openapi): record reject-at-import behaviour for asyncResult + crossoverToAsyncMs (#261)

OpenAPI descriptions amended on both fields under
ExternalizedProcessorConfigDto to record that this backend does not
implement async/crossover semantics; imports that set asyncResult=true
or any non-nil crossoverToAsyncMs are rejected with HTTP 400
VALIDATION_FAILED.

The generated DTO already carried these fields as *bool / *int64 since
the OpenAPI declared them; this commit refreshes only the description
text. No structural change to the generated DTO.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase E — Round-trip tests (SPI-level + legacy-data)

### Task E1: Write the round-trip test file

**Files:**
- Create: `internal/domain/workflow/async_result_roundtrip_test.go`

- [ ] **Step 1: Create the new test file**

Create `internal/domain/workflow/async_result_roundtrip_test.go` with:

```go
package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestAsyncResult_RoundTrip_PointerStates covers the JSON round-trip
// behaviour of the validator-accepted AsyncResult states through the
// SPI ProcessorConfig type. Validator rejects AsyncResult=&true and
// non-nil CrossoverToAsyncMs, so only nil and &false are observable in
// the import→store→export path; both must round-trip byte-equivalent.
func TestAsyncResult_RoundTrip_PointerStates(t *testing.T) {
	ff := false

	cases := []struct {
		name          string
		cfg           spi.ProcessorConfig
		wantInJSON    string
		wantNotInJSON string
	}{
		{
			name:          "async_nil_omitted",
			cfg:           spi.ProcessorConfig{},
			wantNotInJSON: "asyncResult",
		},
		{
			name:       "async_explicit_false_preserved",
			cfg:        spi.ProcessorConfig{AsyncResult: &ff},
			wantInJSON: `"asyncResult":false`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantInJSON != "" && !strings.Contains(string(bs), tc.wantInJSON) {
				t.Errorf("expected JSON to contain %q; got %s", tc.wantInJSON, bs)
			}
			if tc.wantNotInJSON != "" && strings.Contains(string(bs), tc.wantNotInJSON) {
				t.Errorf("expected JSON NOT to contain %q; got %s", tc.wantNotInJSON, bs)
			}

			var back spi.ProcessorConfig
			if err := json.Unmarshal(bs, &back); err != nil {
				t.Fatal(err)
			}
			// Pointer-state-preserving equality.
			if (back.AsyncResult == nil) != (tc.cfg.AsyncResult == nil) {
				t.Errorf("AsyncResult pointer-presence mismatched: got %v, want %v",
					back.AsyncResult, tc.cfg.AsyncResult)
			}
			if back.AsyncResult != nil && tc.cfg.AsyncResult != nil &&
				*back.AsyncResult != *tc.cfg.AsyncResult {
				t.Errorf("AsyncResult value mismatched: got %v, want %v",
					*back.AsyncResult, *tc.cfg.AsyncResult)
			}
		})
	}
}

// TestAsyncResult_LegacyData_RoundTrips verifies that a ProcessorConfig
// containing non-default AsyncResult / CrossoverToAsyncMs (which the
// validator rejects at import) round-trips byte-equivalent through the
// raw JSON marshalling path. Backs the spec §5.4 assertion that
// pre-existing stored data (which could only land via a store-direct
// write that bypasses the import handler) survives export unchanged.
// No engine consumer means runtime ignores it; export must preserve.
func TestAsyncResult_LegacyData_RoundTrips(t *testing.T) {
	tt := true
	cv := int64(5000)
	original := spi.ProcessorConfig{
		AsyncResult:        &tt,
		CrossoverToAsyncMs: &cv,
	}

	bs, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"asyncResult":true`) {
		t.Errorf("marshalled JSON missing asyncResult=true: %s", bs)
	}
	if !strings.Contains(string(bs), `"crossoverToAsyncMs":5000`) {
		t.Errorf("marshalled JSON missing crossoverToAsyncMs=5000: %s", bs)
	}

	var back spi.ProcessorConfig
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if back.AsyncResult == nil || *back.AsyncResult != true {
		t.Errorf("AsyncResult lost on round-trip: got %v", back.AsyncResult)
	}
	if back.CrossoverToAsyncMs == nil || *back.CrossoverToAsyncMs != 5000 {
		t.Errorf("CrossoverToAsyncMs lost on round-trip: got %v", back.CrossoverToAsyncMs)
	}
}
```

- [ ] **Step 2: Run the new tests**

Run:
```bash
go test ./internal/domain/workflow/ -run "TestAsyncResult_" -v
```

Expected: both tests (and their subtests) PASS.

### Task E2: Commit Phase E

- [ ] **Step 1: Stage and commit**

```bash
git add internal/domain/workflow/async_result_roundtrip_test.go
git commit -m "test(workflow): SPI-level + legacy-data round-trip for AsyncResult/CrossoverToAsyncMs (#261)

Two pinned behaviours:
- The validator-accepted shapes (nil, &false for AsyncResult) round-trip
  byte-equivalent through SPI ProcessorConfig JSON marshalling.
- A legacy ProcessorConfig with non-default AsyncResult + CrossoverToAsyncMs
  (which could only land via store-direct write bypassing the import
  handler) survives marshal→unmarshal unchanged. Backs the spec §5.4
  assertion that pre-existing stored data is silently inert at runtime
  but exported faithfully.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase F — E2E HTTP-stack reject tests

### Task F1: Write the E2E reject tests

**Files:**
- Create: `internal/e2e/async_result_import_reject_test.go`

- [ ] **Step 1: Create the new E2E test file**

Helper ground truth (verified):
- `importModelE2E(t, model, 1)` — creates the model.
- `lockModelE2E(t, model, 1)` — locks it (required before workflow import).
- `importWorkflowE2E(t, model, 1, workflowJSON)` — returns `(status int, body string)`. Use this directly for the rejection check (no need for `doAuth` + `readBody`).

The convention `setupModelWithWorkflow` in `internal/e2e/workflow_proc_test.go:55` is `importModelE2E + lockModelE2E + importWorkflowE2E` followed by a 200-asserter. We need the import+lock prelude but **not** the 200 assertion (we want a 400). Inline the three calls and assert directly.

Create `internal/e2e/async_result_import_reject_test.go` with:

```go
package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_AsyncResultTrueRejectedAtImport exercises the full HTTP stack:
// POST a workflow import body whose processor sets asyncResult=true,
// expect 400 VALIDATION_FAILED with a problem+json error response
// identifying the processor by location.
//
// Mirrors the rejection pattern from
// TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound
// (#259), differing only in the validator rule under test.
func TestE2E_AsyncResultTrueRejectedAtImport(t *testing.T) {
	const model = "e2e-async-result-true-reject"
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version":"1","name":"async-reject-wf","initialState":"S1","active":true,
			"states":{
				"S1":{"transitions":[{
					"name":"t","next":"S2","manual":false,
					"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
						"config":{"asyncResult":true}}]
				}]},
				"S2":{}
			}
		}]
	}`

	status, respBody := importWorkflowE2E(t, model, 1, body)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 VALIDATION_FAILED; got %d: %s", status, respBody)
	}

	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, respBody)
	}
	if pd.Status != http.StatusBadRequest {
		t.Errorf("ProblemDetail.status: got %d, want 400", pd.Status)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "VALIDATION_FAILED" {
		t.Errorf("errorCode: got %q, want VALIDATION_FAILED; body=%s", code, respBody)
	}
	// Body must carry the rule-naming substring and the processor location.
	// Assert against raw body so we're robust to where common.WriteError
	// surfaces the wrapped error string (detail vs title vs another field).
	for _, want := range []string{
		`processor "p"`,
		"asyncResult=true is not supported",
	} {
		if !strings.Contains(respBody, want) {
			t.Errorf("response body must contain %q; got: %s", want, respBody)
		}
	}
}

// TestE2E_CrossoverToAsyncMsRejectedAtImport exercises the orphan
// crossoverToAsyncMs rejection path: asyncResult is absent, but
// crossoverToAsyncMs is set. Validator rejects with 400 VALIDATION_FAILED.
func TestE2E_CrossoverToAsyncMsRejectedAtImport(t *testing.T) {
	const model = "e2e-crossover-orphan-reject"
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version":"1","name":"crossover-orphan-wf","initialState":"S1","active":true,
			"states":{
				"S1":{"transitions":[{
					"name":"t","next":"S2","manual":false,
					"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
						"config":{"crossoverToAsyncMs":5000}}]
				}]},
				"S2":{}
			}
		}]
	}`

	status, respBody := importWorkflowE2E(t, model, 1, body)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 VALIDATION_FAILED; got %d: %s", status, respBody)
	}

	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, respBody)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "VALIDATION_FAILED" {
		t.Errorf("errorCode: got %q, want VALIDATION_FAILED; body=%s", code, respBody)
	}
	for _, want := range []string{
		`processor "p"`,
		"crossoverToAsyncMs is not supported",
	} {
		if !strings.Contains(respBody, want) {
			t.Errorf("response body must contain %q; got: %s", want, respBody)
		}
	}
}
```

- [ ] **Step 2: Run the two new E2E tests**

Run:
```bash
go test ./internal/e2e/ -run "TestE2E_AsyncResultTrueRejectedAtImport|TestE2E_CrossoverToAsyncMsRejectedAtImport" -v
```

Expected: both tests PASS. (Requires Docker running for the testcontainers-go PostgreSQL spin-up.)

- [ ] **Step 3 (sanity): if helpers are missing, fix here, not by changing test shape**

Verify the three helpers exist:
```bash
grep -n "^func importModelE2E\|^func lockModelE2E\|^func importWorkflowE2E" internal/e2e/*.go
```

Expected: three matches in `internal/e2e/helpers_test.go` (or near it). If any are missing, do not rewrite the tests to bypass them — stop and add the missing helper, mirroring the existing pattern.

### Task F2: Commit Phase F

- [ ] **Step 1: Stage and commit**

```bash
git add internal/e2e/async_result_import_reject_test.go
git commit -m "test(e2e): asyncResult=true + orphan crossoverToAsyncMs rejected at import (#261)

End-to-end through the full HTTP stack (testcontainers PostgreSQL +
httptest.Server): POST workflow import with asyncResult=true or
crossoverToAsyncMs set returns 400 with errorCode=VALIDATION_FAILED.
Response body identifies the offending processor by location.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase G — Parity scenarios

### Task G1: Append two new scenarios to the YAML dictionary

**Files:**
- Modify: `e2e/externalapi/scenarios/08-workflow-import-export.yaml`

- [ ] **Step 1: Append `wf-import/10` and `wf-import/11` at the end of the `scenarios:` list**

Append the following at the end of the file (after `wf-import/09-allowcycles-bypass`):

```yaml
  - id: wf-import/10-asyncresult-true-rejects
    name: asyncResult=true — rejected at import
    description: |
      The async-result semantic (suspend cascade transaction, resume on
      deferred async-result delivery, optional crossover timer) is gated
      on storage-engine work-recovery primitives this backend does not
      provide. Workflow import rejects any processor that sets
      `asyncResult=true` with HTTP 400 and a `VALIDATION_FAILED` error
      class. The error message names the offending workflow / state /
      transition / processor and the rule violated.
    data:
      model: { name: wfAsyncRej, version: 1 }
      async_true_body: |
        {"workflows":[{
          "version":"1.0","name":"Async True Reject",
          "initialState":"start","active":true,
          "states":{
            "start":{"transitions":[{
              "name":"bad","next":"end","manual":false,
              "processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
                "config":{"asyncResult":true}}]
            }]},
            "end":{"transitions":[]}
          }
        }]}
    steps:
      - { action: import_workflow, endpoint: { rest: POST /entity/wfAsyncRej/1/workflow/import }, body_ref: data.async_true_body, capture: respAsync }
    assertions:
      - type: http_status
        target: respAsync
        expected: 400
      - type: error_class
        target: respAsync
        expected: VALIDATION_FAILED
      - type: error_message_contains
        target: respAsync
        expected: "asyncResult=true is not supported"

  - id: wf-import/11-crossover-orphan-rejects
    name: crossoverToAsyncMs without asyncResult=true — rejected at import
    description: |
      `crossoverToAsyncMs` is a tuner for the async-result semantic. With
      that semantic unsupported on this backend, any non-nil
      `crossoverToAsyncMs` (including zero, including the orphan case
      where `asyncResult` is absent or false) is rejected with HTTP 400
      `VALIDATION_FAILED`.
    data:
      model: { name: wfCrossOrphanRej, version: 1 }
      crossover_orphan_body: |
        {"workflows":[{
          "version":"1.0","name":"Crossover Orphan Reject",
          "initialState":"start","active":true,
          "states":{
            "start":{"transitions":[{
              "name":"bad","next":"end","manual":false,
              "processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
                "config":{"crossoverToAsyncMs":5000}}]
            }]},
            "end":{"transitions":[]}
          }
        }]}
    steps:
      - { action: import_workflow, endpoint: { rest: POST /entity/wfCrossOrphanRej/1/workflow/import }, body_ref: data.crossover_orphan_body, capture: respCrossover }
    assertions:
      - type: http_status
        target: respCrossover
        expected: 400
      - type: error_class
        target: respCrossover
        expected: VALIDATION_FAILED
      - type: error_message_contains
        target: respCrossover
        expected: "crossoverToAsyncMs is not supported"
```

### Task G2: Add and register the two new runners

**Files:**
- Modify: `e2e/parity/externalapi/workflow_import_export.go`

- [ ] **Step 1: Register the two new runners in `init`**

Find the `init()` block (around line 28) and add the two new `NamedTest` entries:

```go
		parity.NamedTest{Name: "ExternalAPI_08_07_ScheduledTransitionRoundtrip", Fn: RunExternalAPI_08_07_ScheduledTransitionRoundtrip},
		parity.NamedTest{Name: "ExternalAPI_08_08_ScheduledTransitionRejects", Fn: RunExternalAPI_08_08_ScheduledTransitionRejects},
		parity.NamedTest{Name: "ExternalAPI_08_09_AllowCyclesBypass", Fn: RunExternalAPI_08_09_AllowCyclesBypass},
		parity.NamedTest{Name: "ExternalAPI_08_10_AsyncResultTrueRejects", Fn: RunExternalAPI_08_10_AsyncResultTrueRejects},
		parity.NamedTest{Name: "ExternalAPI_08_11_CrossoverOrphanRejects", Fn: RunExternalAPI_08_11_CrossoverOrphanRejects},
```

- [ ] **Step 2: Implement the two new runners at the end of the file**

Helper ground truth (verified against runner 08_08 at line 465):
- `d := driver.NewInProcess(t, fixture)` constructs the driver.
- `d.CreateModelFromSample("name", 1, `{"k":1}`)` creates the model.
- `d.ImportWorkflowRaw(name, 1, body)` returns `(status int, body []byte, err error)`. Use this — the plain `ImportWorkflow` returns an error and is meant for the happy path.
- `errorcontract.Match(t, status, body, errorcontract.ExpectedError{HTTPStatus, ErrorCode})` asserts the status code and error code.
- `rfc9457Detail(body)` extracts the `detail` field; assert substring against the return value.

Append at the end of `workflow_import_export.go`:

```go
// RunExternalAPI_08_10_AsyncResultTrueRejects — dictionary 08/10.
// Workflow import with asyncResult=true on a processor config is
// rejected with HTTP 400 + VALIDATION_FAILED and a detail containing
// "asyncResult=true is not supported".
func RunExternalAPI_08_10_AsyncResultTrueRejects(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfAsyncRej", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	body := `{"workflows":[{
		"version":"1.0","name":"Async True Reject",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[{
				"name":"bad","next":"end","manual":false,
				"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
					"config":{"asyncResult":true}}]
			}]},
			"end":{"transitions":[]}
		}
	}]}`

	status, respBody, err := d.ImportWorkflowRaw("wfAsyncRej", 1, body)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw: %v", err)
	}
	errorcontract.Match(t, status, respBody, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})
	if detail := rfc9457Detail(respBody); !strings.Contains(detail, "asyncResult=true is not supported") {
		t.Errorf("asyncResult=true: expected detail substring 'asyncResult=true is not supported'; got %q (body=%s)", detail, string(respBody))
	}
}

// RunExternalAPI_08_11_CrossoverOrphanRejects — dictionary 08/11.
// Workflow import with crossoverToAsyncMs set without asyncResult=true
// is rejected with HTTP 400 + VALIDATION_FAILED and a detail containing
// "crossoverToAsyncMs is not supported".
func RunExternalAPI_08_11_CrossoverOrphanRejects(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfCrossOrphanRej", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	body := `{"workflows":[{
		"version":"1.0","name":"Crossover Orphan Reject",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[{
				"name":"bad","next":"end","manual":false,
				"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
					"config":{"crossoverToAsyncMs":5000}}]
			}]},
			"end":{"transitions":[]}
		}
	}]}`

	status, respBody, err := d.ImportWorkflowRaw("wfCrossOrphanRej", 1, body)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw: %v", err)
	}
	errorcontract.Match(t, status, respBody, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})
	if detail := rfc9457Detail(respBody); !strings.Contains(detail, "crossoverToAsyncMs is not supported") {
		t.Errorf("crossover-orphan: expected detail substring 'crossoverToAsyncMs is not supported'; got %q (body=%s)", detail, string(respBody))
	}
}
```

The `errorcontract` import path and `rfc9457Detail` symbol are already in scope in this file — copy from runner 08_08's imports block at the top of `workflow_import_export.go`; do not introduce new imports without checking.

- [ ] **Step 3: Run the new parity tests against each backend**

Run on memory (fastest):
```bash
go test -short ./e2e/parity/externalapi/ -run "ExternalAPI_08_10|ExternalAPI_08_11" -v
```

Run all backends:
```bash
make test-all
```

Expected: PASS on all three backends.

### Task G3: Commit Phase G

- [ ] **Step 1: Stage and commit**

```bash
git add e2e/externalapi/scenarios/08-workflow-import-export.yaml e2e/parity/externalapi/workflow_import_export.go
git commit -m "test(parity): wf-import/10 + wf-import/11 — asyncResult/crossover reject scenarios (#261)

Two parity scenarios covering the new import-time rejections:
- 10: asyncResult=true → 400 VALIDATION_FAILED
- 11: crossoverToAsyncMs without asyncResult=true (orphan) → 400 VALIDATION_FAILED

Runners registered in init() and implemented at the end of
workflow_import_export.go, mirroring the 08_08 (scheduled-transition
rejects) pattern. Both runners pass on memory, sqlite, postgres.
The Cassandra plugin sibling parity job will pick these up on its
next dependency update.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase H — Documentation sync

### Task H1: Update cloud-divergences.md

**Files:**
- Modify: `docs/cyoda/cloud-divergences.md`

- [ ] **Step 1: Replace rows 16 and 17**

Find the two existing rows (around lines 16, 17):

```markdown
| `ProcessorDefinitionDto.asyncResult` | Field declared in OpenAPI; OSS storage engine plugins (memory/sqlite/postgres) silently ignore it. Crossover semantics need durable suspend state + cluster-wide work-stealing recovery + a distributed timer — implementable only in the closed-source Cassandra plugin. | Documented gap; OSS no-op; enterprise-tier in Cassandra plugin (not yet implemented there either). | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
| `ProcessorDefinitionDto.crossoverToAsyncMs` | See `asyncResult` — same parity gap. | Same. | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
```

Replace with:

```markdown
| `ProcessorDefinitionDto.asyncResult` | Field declared in OpenAPI; OSS backend rejects `asyncResult=true` at workflow import (400 VALIDATION_FAILED). The explicit `asyncResult=false` and absent cases are accepted and round-tripped. Crossover semantics need durable suspend state + cluster-wide work-stealing recovery + a distributed timer — implementable only in the commercial backend. | Reject-at-import on OSS; enterprise-tier in the commercial backend (not yet implemented there either). | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
| `ProcessorDefinitionDto.crossoverToAsyncMs` | Field declared in OpenAPI; OSS backend rejects any non-nil `crossoverToAsyncMs` at workflow import (400 VALIDATION_FAILED), including the orphan case where `asyncResult` is absent or false. See `asyncResult` — same parity gap. | Reject-at-import on OSS; enterprise-tier in the commercial backend. | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
```

(Note: the wording change from "closed-source Cassandra plugin" to "commercial backend" intentionally also fixes a pre-existing memory-violation per `feedback_cassandra_repo_private`.)

### Task H2: Update workflows.md help-topic

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md`

- [ ] **Step 1: Extend the `ProcessorConfig fields` bullet list**

Find the `**ProcessorConfig fields:**` bullet list under `## PROCESSORS` (around line 169). The list currently ends with the `context` bullet. **Append** two new bullets after the `context` bullet (before the next section heading):

```markdown
- `asyncResult` — boolean (pointer; nil-default) — declared in the
  OpenAPI for Cloud parity; the runtime does **not** implement
  async-result semantics on this backend. Imports that set
  `asyncResult: true` are rejected with `400 VALIDATION_FAILED`. The
  explicit `asyncResult: false` and absent cases are accepted and
  round-tripped.
- `crossoverToAsyncMs` — int64 (pointer; nil-default) — crossover
  delay (ms) for the async-result semantic; declared in the OpenAPI
  for Cloud parity; the runtime does **not** implement it. Imports
  that set any non-nil value are rejected with `400 VALIDATION_FAILED`,
  including the orphan case where `asyncResult` is absent or false.
```

### Task H3: Commit Phase H

- [ ] **Step 1: Stage and commit**

```bash
git add docs/cyoda/cloud-divergences.md cmd/cyoda/help/content/workflows.md
git commit -m "docs: reflect asyncResult/crossoverToAsyncMs reject-at-import (#261)

- cloud-divergences.md rows 16, 17: status flips from \"silently ignored\"
  to \"rejected at import (400 VALIDATION_FAILED)\". Also corrects
  pre-existing \"closed-source Cassandra plugin\" wording to \"commercial
  backend\" per project policy (Gate-6 incidental fix).
- workflows.md PROCESSORS / ProcessorConfig fields list: two new bullets
  documenting the SPI shape and the reject-at-import behaviour for both
  fields.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase I — Final verification

### Task I1: Run the full local verification battery

- [ ] **Step 1: `go vet` from the root module**

Run:
```bash
go vet ./...
```

Expected: no output, exit 0.

- [ ] **Step 2: Per-plugin `go vet` (per memory `feedback_plugin_submodule_tests`)**

Run:
```bash
( cd plugins/memory && go vet ./... )
( cd plugins/sqlite && go vet ./... )
( cd plugins/postgres && go vet ./... )
```

Expected: each command exits 0.

- [ ] **Step 3: Short tests across all modules**

Run:
```bash
make test-short-all
```

Expected: green across root + plugins.

- [ ] **Step 4: E2E tests (requires Docker)**

Run:
```bash
go test ./internal/e2e/... -v
```

Expected: green; includes the two new `TestE2E_AsyncResult*` tests.

- [ ] **Step 5: Full plugin battery (requires Docker for postgres testcontainers)**

Run:
```bash
make test-all
```

Expected: green across root + all plugins + parity tests on all backends.

- [ ] **Step 6: One-shot race detector — once, end-of-deliverable, NOT per iteration (per `.claude/rules/race-testing.md`)**

Run:
```bash
go test -race ./...
```

Expected: green, no race detected.

### Task I2: Verification-before-completion checklist

- [ ] **Step 1: Confirm spec acceptance criteria are met**

Walk through spec §9 (Acceptance mapping) and confirm each row is satisfied by a committed change. Specifically:

1. SPI types added to `main` (Phase A) — pseudo-version pin bump (Phase B).
2. Validator accepts both fields with type checks; rejects the unsupported shapes (Phase C).
3. WARN bullet — superseded by reject (recorded in §1 revision history of the spec).
4. Export round-trip preserves both fields for accepted shapes (Phase E).
5. OpenAPI descriptions amended (Phase D).
6. Release-notes bullet wording captured in spec §5.8 (no separate file change in this PR; release-notes assembly is a separate process).

- [ ] **Step 2: Push to the remote**

Run:
```bash
git push -u origin worktree-issue-261-asyncresult-crossover-spi
```

Expected: push accepted.

### Task I3: Hand off to PR / review

The plan ends here. The PR-creation and review steps run under separate skills:

- `superpowers:requesting-code-review` for the work-completion review.
- `antigravity-bundle-security-developer:cc-skill-security-review` for the security review.
- Standard PR-opening flow targeting `release/v0.8.0` (per memory `feedback_release_branch_for_milestones`).
- The PR body must include `Closes #261` (per memory `feedback_release_branch_issue_closure`).
- Apply the v0.8.0 milestone to the PR before merge (per memory `feedback_release_milestone_invariant`).
