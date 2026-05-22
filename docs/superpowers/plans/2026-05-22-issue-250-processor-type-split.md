# Issue #250 — Processor Type Split (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split `ProcessorDefinition.Type` from the conflated `[externalized, scheduled]` discriminator into an execution-location axis (`externalized` today, `internalized` reserved for #260). Remove `ScheduledTransitionProcessorDefinitionDto` and `ScheduledTransitionConfigDto` from OpenAPI. Reject `Type: "internalized"` at engine fire time via an early-return so the rejection cannot be silently swallowed by the existing `ExecutionMode`-keyed abort gate. Sweep stale `Type: "EXTERNAL"`/`"INTERNAL"` test fixtures (Gate 6). Introduce `docs/PROCESSOR_EXECUTION_MODES.md`.

**Spec:** `docs/superpowers/specs/2026-05-20-issue-250-processor-type-split-design.md` (revision 4).

**Architecture:** Engine code in `internal/domain/workflow` owns the recognised Type values via untyped string constants (precedent: `ExecutionMode*` constants in `validate.go:13-21`). The SPI's `ProcessorDefinition.Type` stays a plain `string` — comment-only docstring update, no symbols. The OpenAPI schema mutates: `type` becomes `enum: [externalized]` with explicit `discriminator.mapping`; `ScheduledTransition*Dto` deleted; `processors[]` `oneOf` collapses to one member. Engine adds an early-return at the top of the per-processor dispatch loop. No new audit-event emit paths — the rejection inherits the existing two-event shape (per-processor emit + caller-level failure emit) used by every other fatal processor failure.

**Tech Stack:** Go 1.26+, `oapi-codegen` (root `go generate ./api/...`), `log/slog`, OpenAPI 3.x, in-memory + Postgres + SQLite SPI plugins, `internal/e2e/` testcontainers harness.

**Branch + Worktree:** `refactor-250-processor-type-split` branched from `release/v0.8.0`. Worktree at `.worktrees/refactor-250-processor-type-split/`.

**Constraints (non-negotiable):**
- TDD mandatory (`.claude/rules/tdd.md`).
- No GitHub issue IDs in shipped artefacts (error messages, log lines, code comments, OpenAPI descriptions, help-topic content).
- Never use `log.Printf` / `fmt.Printf` for operational logging — use `log/slog`.
- Race detector is end-of-deliverable, not per-step (`.claude/rules/race-testing.md`).
- Plugin submodules need explicit `make test-all` at end of deliverable — `go test ./...` from root skips them.

---

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `docs/PROCESSOR_EXECUTION_MODES.md` | User-facing two-axis reference doc | **Create** (from parent worktree draft) |
| `internal/domain/workflow/validate.go` | Type-axis constants alongside ExecutionMode constants | Modify (add ~10 lines) |
| `internal/domain/workflow/engine_processors.go` | Engine dispatch — add early-return Type-axis check | Modify (add ~15 lines at top of loop) |
| `internal/domain/workflow/engine_processors_type_test.go` | New test file for Type-axis tests (Tasks 4, 6, 7, 8, 9, 11) | **Create** |
| `internal/domain/workflow/engine_processors_audit_test.go` | New test file for audit-event sequence tests (Tasks 5a, 5b) | **Create** |
| `internal/domain/workflow/handler_type_validation_test.go` | New test file for validator-ordering tests (Task 10) | **Create** |
| `internal/domain/workflow/engine_test.go` | Stale `Type: "EXTERNAL"`/`"INTERNAL"`/`"external"` fixture sweep | Modify (~30 lines) |
| `internal/domain/workflow/engine_ifmatch_test.go` | Stale fixture sweep | Modify (~3 lines) |
| `internal/domain/workflow/engine_result_test.go` | Stale fixture sweep | Modify (~2 lines) |
| `internal/domain/workflow/engine_transition_aborted_test.go` | Stale fixture sweep | Modify (~2 lines) |
| `internal/domain/workflow/scenarios_test.go` | Stale fixture sweep | Modify (~5 lines) |
| `internal/e2e/workflow_internalized_test.go` | E2E for Task 12 (manual transition fires + rejected) | **Create** |
| `api/openapi.yaml` | Schema: `type` enum + discriminator.mapping + drop scheduled DTOs + collapse oneOf | Modify (~30 lines net) |
| `api/generated.go` | Regenerated from openapi.yaml | Modify (regen — DO NOT hand-edit) |
| `cyoda-go-spi/types.go` (sibling repo at `../cyoda-go-spi`) | Docstring on `ProcessorDefinition.Type` | Modify (docstring only) |
| `cmd/cyoda/help/content/workflows.md` | Lines 63, 130, 135-139 rewrites | Modify |
| `cmd/cyoda/help/content/grpc.md` | Line 219 `EXTERNAL` → `externalized` | Modify |
| `e2e/externalapi/scenarios/08-workflow-import-export.yaml` | Strip `{"type":"scheduled",...}` from `wf-import/03` | Modify |

---

## Task 1: Introduce `docs/PROCESSOR_EXECUTION_MODES.md` from parent worktree draft

The file exists as untracked in the parent worktree's working copy (`/Users/paul/go-projects/cyoda-light/cyoda-go/docs/PROCESSOR_EXECUTION_MODES.md`, ~477 lines). Land it as the PR's first commit.

**Files:**
- Create: `docs/PROCESSOR_EXECUTION_MODES.md`

- [ ] **Step 1: Copy the file from parent worktree**

Run:
```bash
cp /Users/paul/go-projects/cyoda-light/cyoda-go/docs/PROCESSOR_EXECUTION_MODES.md \
   docs/PROCESSOR_EXECUTION_MODES.md
```

Expected: file appears at `docs/PROCESSOR_EXECUTION_MODES.md`.

- [ ] **Step 2: Verify it's the expected content**

Run:
```bash
head -5 docs/PROCESSOR_EXECUTION_MODES.md
wc -l docs/PROCESSOR_EXECUTION_MODES.md
```

Expected first line: `# Processor Execution Modes`. Expected line count: ~477.

- [ ] **Step 3: Commit**

```bash
git add docs/PROCESSOR_EXECUTION_MODES.md
git commit -m "$(cat <<'EOF'
docs: introduce PROCESSOR_EXECUTION_MODES.md

Drafted during the v0.8.0 workflow import/export audit. Provides a
single reference for workflow authors choosing an ExecutionMode and for
engine contributors maintaining the dispatch path. The §0 axis-summary
preface (added in a follow-up commit) anchors the execution-location
axis (type) against the execution-mode axis (executionMode).

Companion to cmd/cyoda/help/content/workflows.md and the engine's
internal/domain/workflow/engine_processors.go.
EOF
)"
```

---

## Task 2: Add the §0 axis-summary preface to `docs/PROCESSOR_EXECUTION_MODES.md`

**Files:**
- Modify: `docs/PROCESSOR_EXECUTION_MODES.md` (insert new section between line 16 (end of "Companion documents") and line 17 (`---`))

- [ ] **Step 1: Read the file's opening structure**

Run:
```bash
sed -n '1,20p' docs/PROCESSOR_EXECUTION_MODES.md
```

Expected: the file's title, intro paragraph, companion-documents bullet list, and a horizontal rule (`---`).

- [ ] **Step 2: Insert §0 between the companion list and the `---` rule**

Use the Edit tool. `old_string`:
```
- [`cmd/cyoda/help/content/cluster.md`](../cmd/cyoda/help/content/cluster.md) — multi-node routing and segment pinning

---

## 1. Quick Reference
```

`new_string`:
```
- [`cmd/cyoda/help/content/cluster.md`](../cmd/cyoda/help/content/cluster.md) — multi-node routing and segment pinning

---

## 0. Axis Summary

Workflow processors have two orthogonal configuration axes.

**`type` — execution-location.** Determines where the processor runs.
Currently only `externalized` (gRPC dispatch to a calculation node) is
implemented. The value `internalized` is reserved for in-process
execution; any transition firing a processor with `type: "internalized"`
is rejected at dispatch with `WORKFLOW_FAILED` (400). Empty or omitted
on the wire is treated as `externalized`.

**`executionMode` — transactional semantics of dispatch.** Determines
whether the dispatch is synchronous or asynchronous, and whether the
caller's transaction stays open across the dispatch. The four values
(`SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX`, `COMMIT_BEFORE_DISPATCH`) are
the focus of this document. All `executionMode` semantics described
below apply to `externalized` processors; the `internalized` location
has no documented dispatch semantics yet.

---

## 1. Quick Reference
```

- [ ] **Step 3: Verify the section landed**

Run:
```bash
grep -n "^## 0\. Axis Summary" docs/PROCESSOR_EXECUTION_MODES.md
```

Expected: one match.

- [ ] **Step 4: Commit**

```bash
git add docs/PROCESSOR_EXECUTION_MODES.md
git commit -m "$(cat <<'EOF'
docs: add §0 axis summary to PROCESSOR_EXECUTION_MODES.md

Anchors the execution-location axis (type) against the execution-mode
axis (executionMode) so readers know they are orthogonal. Prepares the
ground for #260's internalized execution-location landing.
EOF
)"
```

---

## Task 3: Add Type-axis constants to `internal/domain/workflow/validate.go`

**Files:**
- Modify: `internal/domain/workflow/validate.go` (insert constants after line 21, the existing `ExecutionMode*` block)

- [ ] **Step 1: Read the existing constants block**

Run:
```bash
sed -n '10,22p' internal/domain/workflow/validate.go
```

Expected: a comment block + `const ( ExecutionMode* = ... )` block ending at line 21.

- [ ] **Step 2: Insert the new constants block after the ExecutionMode block**

Use the Edit tool. `old_string`:
```
const (
	ExecutionModeSync                 = "SYNC"
	ExecutionModeAsyncSameTx          = "ASYNC_SAME_TX"
	ExecutionModeAsyncNewTx           = "ASYNC_NEW_TX"
	ExecutionModeCommitBeforeDispatch = "COMMIT_BEFORE_DISPATCH"
)
```

`new_string`:
```
const (
	ExecutionModeSync                 = "SYNC"
	ExecutionModeAsyncSameTx          = "ASYNC_SAME_TX"
	ExecutionModeAsyncNewTx           = "ASYNC_NEW_TX"
	ExecutionModeCommitBeforeDispatch = "COMMIT_BEFORE_DISPATCH"
)

// Processor execution-location tokens. Sourced from the OpenAPI enum in
// api/openapi.yaml (mirrored in api/generated.go's ProcessorDefinitionDto
// type constants). Centralised here as untyped strings so engine logic,
// validator rules, and tests can compare against a single source — the
// SPI's ProcessorDefinition.Type field is itself a plain string, so an
// enum type would not buy compile-time safety.
//
// Empty value is treated as ProcessorTypeExternalized at dispatch. Any
// value other than ProcessorTypeInternalized falls through to the
// ExecutionMode dispatch path at engine fire time; the only Type
// rejection performed by the engine is on the exact value
// ProcessorTypeInternalized.
const (
	ProcessorTypeExternalized = "externalized"
	ProcessorTypeInternalized = "internalized"
)
```

- [ ] **Step 3: Verify the file still compiles**

Run:
```bash
go build ./internal/domain/workflow/...
```

Expected: no output (clean build).

- [ ] **Step 4: Commit**

```bash
git add internal/domain/workflow/validate.go
git commit -m "$(cat <<'EOF'
feat(workflow): add ProcessorType* constants for execution-location axis

Mirrors the ExecutionMode* constants block (validate.go:13-21). Engine
code, validator rules, and tests use these constants instead of literal
strings or the api/generated.go regenerator-emitted constants. The
authoritative source of truth for Type values lives in
internal/domain/workflow; api/generated.go's constants stay scoped to
that package and the OpenAPI tooling boundary.
EOF
)"
```

---

## Task 4: Write failing tests for `Type: internalized` rejection (four ExecutionMode matrix)

This is the central behaviour change. Per `.claude/rules/tdd.md`, the failing tests come first.

**Files:**
- Create: `internal/domain/workflow/engine_processors_type_test.go`

- [ ] **Step 1: Create the test file with the four-case matrix**

Use the Write tool to create `internal/domain/workflow/engine_processors_type_test.go`:

```go
package workflow

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestInternalizedRejection_ExecutionModeMatrix asserts that Type:
// "internalized" is rejected at fire time regardless of the declared
// ExecutionMode. The critical case is ASYNC_NEW_TX — the existing abort
// gate at engine_processors.go:109 keys on proc.ExecutionMode, so a
// rejection that fell through to the gate would be silently swallowed
// for ASYNC_NEW_TX and the transition would succeed.
func TestInternalizedRejection_ExecutionModeMatrix(t *testing.T) {
	cases := []struct {
		name          string
		executionMode string
	}{
		{name: "ExecutionMode unset", executionMode: ""},
		{name: "ExecutionMode SYNC", executionMode: ExecutionModeSync},
		{name: "ExecutionMode ASYNC_NEW_TX", executionMode: ExecutionModeAsyncNewTx},
		{name: "ExecutionMode COMMIT_BEFORE_DISPATCH", executionMode: ExecutionModeCommitBeforeDispatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "internalized-reject-" + tc.executionMode, ModelVersion: "1.0"}

			engine.extProc = &mockExternalProcessing{
				// Should NEVER be called — the Type-axis early-return must
				// short-circuit before any ExecutionMode dispatch.
				dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
					t.Fatalf("mockExternalProcessing.DispatchProcessor was called for %q (proc=%s) — internalized rejection should have short-circuited before dispatch", tc.executionMode, proc.Name)
					return entity, nil
				},
			}

			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "InternalizedRejectWF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: false,
							Processors: []spi.ProcessorDefinition{
								{
									Type:          ProcessorTypeInternalized,
									Name:          "internal-proc",
									ExecutionMode: tc.executionMode,
								},
							}},
					}},
					"DONE": {},
				},
			}
			saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

			entity := makeEntity("e1", modelRef, map[string]any{})
			result, err := engine.Execute(ctx, entity, "")
			if err == nil {
				t.Fatalf("expected error from internalized processor rejection, got nil (result=%+v)", result)
			}

			msg := err.Error()
			if !strings.Contains(msg, `execution type "internalized" is not yet implemented`) {
				t.Errorf("error message missing rejection text: %q", msg)
			}
			if !strings.Contains(msg, "processor internal-proc failed:") {
				t.Errorf("error message missing outer-wrap prefix: %q", msg)
			}

			// Entity must remain in the source state.
			if entity.Meta.State != "" {
				t.Errorf("entity state expected source state (\"\"), got %q", entity.Meta.State)
			}
		})
	}
}
```

Add the required import for `context`:

Use the Edit tool. `old_string`:
```
import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)
```

`new_string`:
```
import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)
```

- [ ] **Step 2: Run the test and confirm it FAILS**

Run:
```bash
go test ./internal/domain/workflow/ -run TestInternalizedRejection_ExecutionModeMatrix -v
```

Expected: **FAIL.** Most likely failure mode: either the test asserts an error but `engine.Execute` succeeds (the engine ignores `Type`), OR `mockExternalProcessing.DispatchProcessor` is called (causing the `t.Fatalf`). For `ASYNC_NEW_TX` the most damning sub-case is silent success — the test catches it because there's no error returned but the assertion `if err == nil` fires.

If the test passes at this point, **STOP** — the early-return logic must not be there yet; debug before proceeding.

- [ ] **Step 3: Commit the failing test (RED state)**

```bash
git add internal/domain/workflow/engine_processors_type_test.go
git commit -m "$(cat <<'EOF'
test(workflow): RED — Type: internalized rejection ExecutionMode matrix

Four-case matrix: Type=internalized × ExecutionMode={unset, SYNC,
ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH}. The ASYNC_NEW_TX case is the
critical regression catcher — a naive "set procErr then fall through"
shape would let the line 109 abort gate (which keys on proc.ExecutionMode)
silently swallow the rejection.

Tests assert: error returned, message contains rejection sub-string,
outer wrap names the processor, entity stays in source state, the
mockExternalProcessing.DispatchProcessor mock is never called.

RED state — engine does not yet implement Type-axis rejection.
EOF
)"
```

---

## Task 5: Implement the early-return Type-axis check in `engine_processors.go`

**Files:**
- Modify: `internal/domain/workflow/engine_processors.go` (insert early-return at top of per-processor loop, between lines 59 and 60)

- [ ] **Step 1: Inspect the current loop entry**

Run:
```bash
sed -n '55,65p' internal/domain/workflow/engine_processors.go
```

Expected:
```
	currentCtx := ctx
	currentTxID := txID

	for _, proc := range processors {
		var success bool
		var procErr error

		switch proc.ExecutionMode {
```

- [ ] **Step 2: Insert the Type-axis early-return**

Use the Edit tool. `old_string`:
```
	for _, proc := range processors {
		var success bool
		var procErr error

		switch proc.ExecutionMode {
```

`new_string`:
```
	for _, proc := range processors {
		// Execution-location axis. Rejection is fatal and self-contained:
		// emit the per-processor SMEventStateProcessResult audit row
		// explicitly (mirroring the post-dispatch emit lower in this loop),
		// roll back any open segment, then return. The post-dispatch abort
		// gate keys on proc.ExecutionMode and would silently swallow the
		// rejection if proc.ExecutionMode == ExecutionModeAsyncNewTx, so the
		// rejection must short-circuit the loop entirely.
		if proc.Type == ProcessorTypeInternalized {
			auditData := map[string]any{
				"success": false,
				"mode":    proc.ExecutionMode,
			}
			e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventStateProcessResult,
				fmt.Sprintf("Processor %q completed", proc.Name), auditData)
			e.rollbackOpenSegmentOnFailure(currentCtx, currentTxID, txID, proc.Name)
			return currentCtx, currentTxID, fmt.Errorf(
				"processor %s failed: execution type %q is not yet implemented",
				proc.Name, proc.Type)
		}

		var success bool
		var procErr error

		switch proc.ExecutionMode {
```

- [ ] **Step 3: Re-run the failing test — should now pass (GREEN)**

Run:
```bash
go test ./internal/domain/workflow/ -run TestInternalizedRejection_ExecutionModeMatrix -v
```

Expected: **PASS** on all four sub-cases (`ExecutionMode unset`, `ExecutionMode SYNC`, `ExecutionMode ASYNC_NEW_TX`, `ExecutionMode COMMIT_BEFORE_DISPATCH`).

- [ ] **Step 4: Run the full workflow package tests to confirm no regression**

Run:
```bash
go test ./internal/domain/workflow/... -v 2>&1 | tail -40
```

Expected: all PASS, including the new matrix and all pre-existing tests. The pre-existing tests should not have broken because:
- Stale `Type: "EXTERNAL"`, `"INTERNAL"`, `"external"` fixtures fall through (not exact-match `"internalized"`).
- No fixture used `"internalized"` previously.

If anything fails, investigate before proceeding.

- [ ] **Step 5: Commit (GREEN)**

```bash
git add internal/domain/workflow/engine_processors.go
git commit -m "$(cat <<'EOF'
feat(workflow): GREEN — reject Type: internalized at engine dispatch

Early-return at the top of the per-processor loop in executeProcessors.
Emits the per-processor SMEventStateProcessResult audit row explicitly
(mirroring the existing post-dispatch emit shape), rolls back any open
segment via rollbackOpenSegmentOnFailure, and returns the wrapped error.
Bypasses the post-dispatch abort gate entirely so the rejection cannot
be swallowed when ExecutionMode is declared as ASYNC_NEW_TX.

The error message is self-contained — no issue-ID references. The
operator-visible message becomes:
  processor X failed: execution type "internalized" is not yet implemented
EOF
)"
```

---

## Task 6: Add audit-event sequence tests (manual + cascade transition paths)

**Files:**
- Create: `internal/domain/workflow/engine_processors_audit_test.go`

The audit trail for a fatal processor failure emits `SMEventStateProcessResult` **twice**: once inside `executeProcessors` at line 104 (with `Data: {success, mode}`), and once in the caller (`engine.go:466` for manual transitions, `:554` for cascaded automatic transitions) with `Data: {success}` and Details embedding the full wrapped error string.

- [ ] **Step 1: Create the audit-event test file**

Use the Write tool to create `internal/domain/workflow/engine_processors_audit_test.go`:

```go
package workflow

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestInternalizedRejection_AuditEvents_ManualTransition asserts the
// full audit-event sequence for a manual-fired internalized processor:
// 1× SMEventProcessingPaused, 2× SMEventStateProcessResult (one inside
// executeProcessors, one in the caller-level failure emit), 0×
// SMEventStateMachineFinish.
func TestInternalizedRejection_AuditEvents_ManualTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit-manual", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AuditManualWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "FIRE", Next: "DONE", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeInternalized, Name: "internal-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "FIRE")
	if err == nil {
		t.Fatalf("expected rejection error from internalized processor, got nil")
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	var (
		processingPausedCount int
		stateProcessResults   []spi.StateMachineEvent
		stateMachineFinished  int
	)
	for _, ev := range events {
		switch ev.EventType {
		case spi.SMEventProcessingPaused:
			processingPausedCount++
		case spi.SMEventStateProcessResult:
			stateProcessResults = append(stateProcessResults, ev)
		case spi.SMEventFinished:
			stateMachineFinished++
		}
	}

	if processingPausedCount != 1 {
		t.Errorf("SMEventProcessingPaused count = %d, want 1", processingPausedCount)
	}
	if got, want := len(stateProcessResults), 2; got != want {
		t.Fatalf("SMEventStateProcessResult count = %d, want %d (one from executeProcessors, one from caller failure emit)", got, want)
	}
	if stateMachineFinished != 0 {
		t.Errorf("SMEventFinished count = %d, want 0 (cascade aborted)", stateMachineFinished)
	}

	// First emit comes from executeProcessors:104-106 with mode = declared ExecutionMode.
	first := stateProcessResults[0]
	if !strings.Contains(first.Details, `Processor "internal-proc" completed`) {
		t.Errorf("first SMEventStateProcessResult Details = %q, want 'Processor \"internal-proc\" completed'", first.Details)
	}
	if mode, _ := first.Data["mode"].(string); mode != ExecutionModeSync {
		t.Errorf("first SMEventStateProcessResult Data.mode = %v, want %q", first.Data["mode"], ExecutionModeSync)
	}
	if success, _ := first.Data["success"].(bool); success {
		t.Errorf("first SMEventStateProcessResult Data.success = true, want false")
	}

	// Second emit comes from engine.go:466 with the full wrapped error in Details.
	second := stateProcessResults[1]
	if !strings.Contains(second.Details, `Processor failed for transition "FIRE":`) {
		t.Errorf("second SMEventStateProcessResult Details = %q, want 'Processor failed for transition \"FIRE\":' prefix", second.Details)
	}
	if !strings.Contains(second.Details, `execution type "internalized" is not yet implemented`) {
		t.Errorf("second SMEventStateProcessResult Details = %q, want rejection sub-string", second.Details)
	}
	if _, hasMode := second.Data["mode"]; hasMode {
		t.Errorf("second SMEventStateProcessResult Data should not have a 'mode' key (caller emit does not set it); got %v", second.Data)
	}
}

// TestInternalizedRejection_AuditEvents_CascadedTransition asserts the
// same audit sequence for a non-manual transition reached via
// engine.cascadeAutomated (engine.go:554 emit path).
func TestInternalizedRejection_AuditEvents_CascadedTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit-cascade", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AuditCascadeWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "AUTO", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeInternalized, Name: "internal-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e2", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatalf("expected rejection error from internalized processor in cascade path, got nil")
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e2")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	var stateProcessResults []spi.StateMachineEvent
	for _, ev := range events {
		if ev.EventType == spi.SMEventStateProcessResult {
			stateProcessResults = append(stateProcessResults, ev)
		}
	}

	if got, want := len(stateProcessResults), 2; got != want {
		t.Fatalf("SMEventStateProcessResult count = %d, want %d (one inside executeProcessors, one from cascade failure emit at engine.go:554)", got, want)
	}

	// The second emit (from engine.go:554) carries the wrapped error in Details.
	second := stateProcessResults[1]
	if !strings.Contains(second.Details, `Processor failed for transition "AUTO":`) {
		t.Errorf("cascade-path second emit Details = %q, want 'Processor failed for transition \"AUTO\":' prefix", second.Details)
	}
	if !strings.Contains(second.Details, `execution type "internalized" is not yet implemented`) {
		t.Errorf("cascade-path second emit Details = %q, want rejection sub-string", second.Details)
	}
}
```

- [ ] **Step 2: Run the audit-event tests**

Run:
```bash
go test ./internal/domain/workflow/ -run 'TestInternalizedRejection_AuditEvents' -v
```

Expected: both PASS. If a test fails because the audit-event field structure (e.g. `ev.Data["mode"]`) doesn't match what we're asserting, inspect the in-memory audit store's actual emit shape and adjust the assertion. The recordEvent shape is set by the engine's `recordEvent` helper — look at `internal/domain/workflow/engine.go` (look for `recordEvent` definition) if the assertion needs tightening.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_processors_audit_test.go
git commit -m "$(cat <<'EOF'
test(workflow): audit-event sequence for Type: internalized rejection

Two test cases — manual transition (engine.go:466 caller emit) and
cascaded automatic transition (engine.go:554 caller emit). Both assert:
1× SMEventProcessingPaused, 2× SMEventStateProcessResult (one inside
executeProcessors, one from the caller's failure emit), 0×
SMEventFinished.

The second emit's Details field carries the full wrapped error
("Processor failed for transition X: processor X failed: execution
type \"internalized\" is not yet implemented") — this IS the
audit-trail's self-descriptive record of the rejection cause.
EOF
)"
```

---

## Task 7: Add fall-through behaviour tests (externalized, unknown, empty)

**Files:**
- Modify: `internal/domain/workflow/engine_processors_type_test.go` (append new test funcs)

- [ ] **Step 1: Append the fall-through tests**

Use the Edit tool to append to `internal/domain/workflow/engine_processors_type_test.go`. `old_string` is the closing `}` of the existing matrix test (the last line of the file). `new_string`:

```
}

// TestType_Externalized_FallsThroughToExecutionModeDispatch asserts that
// Type: "externalized" (and Type: "" / unset) is treated identically by
// the engine — both fall through to the ExecutionMode dispatch path.
func TestType_Externalized_FallsThroughToExecutionModeDispatch(t *testing.T) {
	cases := []struct {
		name     string
		typeVal  string
	}{
		{name: "Type unset", typeVal: ""},
		{name: "Type externalized", typeVal: ProcessorTypeExternalized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "fall-through-" + tc.typeVal, ModelVersion: "1.0"}

			var dispatched bool
			engine.extProc = &mockExternalProcessing{
				dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
					dispatched = true
					return entity, nil
				},
			}

			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "FallThroughWF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: false,
							Processors: []spi.ProcessorDefinition{
								{Type: tc.typeVal, Name: "p", ExecutionMode: ExecutionModeSync},
							}},
					}},
					"DONE": {},
				},
			}
			saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

			entity := makeEntity("e1", modelRef, map[string]any{})
			result, err := engine.Execute(ctx, entity, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !dispatched {
				t.Errorf("mockExternalProcessing.DispatchProcessor was NOT called — externalized fall-through is broken")
			}
			if result.Meta.State != "DONE" {
				t.Errorf("entity state = %q, want DONE", result.Meta.State)
			}
		})
	}
}

// TestType_UnknownValues_FallThrough asserts that any Type value other
// than the exact string "internalized" falls through to the ExecutionMode
// dispatch path. Pins the no-normalisation, exact-match contract.
func TestType_UnknownValues_FallThrough(t *testing.T) {
	cases := []string{
		"scheduled",            // legacy value removed from OpenAPI in this PR
		"EXTERNALIZED",         // case-mismatched uppercase
		" externalized ",       // whitespace-padded
		"internalized\nfoo",    // newline injection — exact-match boundary
		"future_unknown_value", // arbitrary unknown
	}

	for _, typeVal := range cases {
		t.Run("Type="+typeVal, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "unknown-" + typeVal, ModelVersion: "1.0"}

			var dispatched bool
			engine.extProc = &mockExternalProcessing{
				dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
					dispatched = true
					return entity, nil
				},
			}

			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "UnknownTypeWF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: false,
							Processors: []spi.ProcessorDefinition{
								{Type: typeVal, Name: "p", ExecutionMode: ExecutionModeSync},
							}},
					}},
					"DONE": {},
				},
			}
			saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

			entity := makeEntity("e1", modelRef, map[string]any{})
			result, err := engine.Execute(ctx, entity, "")
			if err != nil {
				t.Fatalf("Type=%q produced error %v — should have fallen through to externalized dispatch", typeVal, err)
			}
			if !dispatched {
				t.Errorf("Type=%q did not reach the ExecutionMode dispatch — fall-through broken", typeVal)
			}
			if result.Meta.State != "DONE" {
				t.Errorf("Type=%q result state = %q, want DONE", typeVal, result.Meta.State)
			}
		})
	}
}

// TestType_EmptyProcessors_NoOp asserts that a transition with no
// processors at all imports, fires, and reaches the target state without
// entering the per-processor loop. No SMEventProcessingPaused is emitted.
// Pins that the new Type-axis early-return does not regress the
// empty-pipeline path (executeProcessors early-return at lines 43-45).
func TestType_EmptyProcessors_NoOp(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-procs", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NoProcsWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Meta.State != "DONE" {
		t.Errorf("entity state = %q, want DONE", result.Meta.State)
	}

	auditStore, _ := factory.StateMachineAuditStore(ctx)
	events, _ := auditStore.GetEvents(ctx, "e1")
	for _, ev := range events {
		if ev.EventType == spi.SMEventProcessingPaused {
			t.Errorf("SMEventProcessingPaused was emitted for an empty processors[] — expected zero")
		}
	}
```

(Note: the final closing `}` of the file is added by the Edit. Make sure the new content ends with `}` to close `TestType_EmptyProcessors_NoOp`.)

Use this exact `old_string` for the Edit (the closing brace of `TestInternalizedRejection_ExecutionModeMatrix` followed by EOF):
```
			if entity.Meta.State != "" {
				t.Errorf("entity state expected source state (\"\"), got %q", entity.Meta.State)
			}
		})
	}
}
```

And the `new_string` is the same content followed by the new test functions above.

- [ ] **Step 2: Run the new tests**

Run:
```bash
go test ./internal/domain/workflow/ -run 'TestType_Externalized_FallsThroughToExecutionModeDispatch|TestType_UnknownValues_FallThrough|TestType_EmptyProcessors_NoOp' -v
```

Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_processors_type_test.go
git commit -m "$(cat <<'EOF'
test(workflow): Type-axis fall-through and empty-pipeline coverage

- Type="" and Type="externalized" both reach mock dispatch — confirms
  the two are interchangeable.
- Unknown Type values (case-mismatched, whitespace, control-char-padded,
  legacy "scheduled", arbitrary unknown) all fall through. Pins the
  no-normalisation, exact-match contract for the Type-axis early-return.
- Empty processors[] is a no-op — no SMEventProcessingPaused emitted.
  Confirms the early-return at engine_processors.go:43-45 is not
  regressed by the new Type-axis check.
EOF
)"
```

---

## Task 8: Add cascade-position test (A:SYNC succeeds, B:internalized aborts, C never runs)

**Files:**
- Modify: `internal/domain/workflow/engine_processors_type_test.go` (append new test func)

- [ ] **Step 1: Append the cascade-position test**

Use the Edit tool to append after `TestType_EmptyProcessors_NoOp`'s closing brace. New content:

```go

// TestInternalizedRejection_CascadePosition_SYNC asserts that a SYNC
// predecessor's mutations land in entity.Data, the internalized rejection
// aborts the pipeline, and the successor processor is never invoked.
// Entity.Meta.State stays in the source state.
//
// Per spec §6.1 test 5 the variant tests for A:ASYNC_NEW_TX and
// A:COMMIT_BEFORE_DISPATCH are explicitly out of scope — those
// transactional shapes have their own well-tested abort semantics
// independent of the Type-axis check.
func TestInternalizedRejection_CascadePosition_SYNC(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cascade-position", ModelVersion: "1.0"}

	dispatchOrder := []string{}
	engine.extProc = &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
			dispatchOrder = append(dispatchOrder, proc.Name)
			if proc.Name == "A" {
				modified := *entity
				modified.Data = []byte(`{"A_ran":true}`)
				return &modified, nil
			}
			return entity, nil
		},
	}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CascadePosWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "A", ExecutionMode: ExecutionModeSync},
						{Type: ProcessorTypeInternalized, Name: "B", ExecutionMode: ExecutionModeSync},
						{Type: ProcessorTypeExternalized, Name: "C", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{"original": true})
	result, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatalf("expected rejection error from B (internalized), got nil")
	}
	if !strings.Contains(err.Error(), `execution type "internalized" is not yet implemented`) {
		t.Errorf("error = %q, want rejection sub-string", err.Error())
	}

	// A ran, B never reached the mock (early-return short-circuit), C never ran.
	if len(dispatchOrder) != 1 || dispatchOrder[0] != "A" {
		t.Errorf("dispatch order = %v, want [A] (B should not reach mock; C should not be dispatched)", dispatchOrder)
	}

	// A's mutation applied to in-memory entity.Data.
	if string(result.Data) != `{"A_ran":true}` {
		t.Errorf("result.Data = %s, want A's mutation (in-memory layer)", result.Data)
	}

	// Entity.Meta.State stays in source state — engine never reached the
	// post-executeProcessors line that sets entity.Meta.State.
	if result.Meta.State != "" {
		t.Errorf("result.Meta.State = %q, want source state (\"\")", result.Meta.State)
	}
}
```

- [ ] **Step 2: Run the cascade-position test**

Run:
```bash
go test ./internal/domain/workflow/ -run TestInternalizedRejection_CascadePosition_SYNC -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_processors_type_test.go
git commit -m "$(cat <<'EOF'
test(workflow): cascade-position behaviour with internalized rejection

A:SYNC succeeds → B:internalized aborts → C never dispatched.
- Dispatch order asserted: only A reaches the mock.
- A's mutation applied to in-memory entity.Data (executeSyncProcessor
  writes entity.Data on success at engine_processors.go:148).
- entity.Meta.State stays in source state — engine never reaches the
  post-executeProcessors state-mutation line.
EOF
)"
```

---

## Task 9: Add wire round-trip test for unknown Type values

**Files:**
- Modify: `internal/domain/workflow/engine_processors_type_test.go` (append new test func)

- [ ] **Step 1: Append the round-trip test**

Use the Edit tool to append after `TestInternalizedRejection_CascadePosition_SYNC`'s closing brace. New content:

```go

// TestType_RoundTrip_PreservesUnknownValues confirms that removing the
// ScheduledTransitionProcessorDefinitionDto from OpenAPI does not change
// wire behaviour for the SPI's free-string Type field. An unknown value
// like "scheduled" or "future_unknown_value" survives the JSON round-trip
// through spi.WorkflowDefinition verbatim.
func TestType_RoundTrip_PreservesUnknownValues(t *testing.T) {
	cases := []string{"scheduled", "future_unknown_value"}

	for _, typeVal := range cases {
		t.Run("Type="+typeVal, func(t *testing.T) {
			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "RoundTrip", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: true,
							Processors: []spi.ProcessorDefinition{
								{Type: typeVal, Name: "p", ExecutionMode: ExecutionModeSync},
							}},
					}},
					"DONE": {},
				},
			}

			raw, err := json.Marshal(wf)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var roundTrip spi.WorkflowDefinition
			if err := json.Unmarshal(raw, &roundTrip); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			gotType := roundTrip.States["INITIAL"].Transitions[0].Processors[0].Type
			if gotType != typeVal {
				t.Errorf("round-trip Type = %q, want %q", gotType, typeVal)
			}
		})
	}
}
```

Note: this test uses `json.Marshal`/`json.Unmarshal` directly — verify the `encoding/json` import is already present at the top of the file. If not, add it:

Use the Edit tool. `old_string`:
```
import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)
```

`new_string`:
```
import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)
```

- [ ] **Step 2: Run the round-trip test**

Run:
```bash
go test ./internal/domain/workflow/ -run TestType_RoundTrip_PreservesUnknownValues -v
```

Expected: PASS for both `"scheduled"` and `"future_unknown_value"`.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_processors_type_test.go
git commit -m "$(cat <<'EOF'
test(workflow): round-trip preserves unknown Type values verbatim

Confirms that removing ScheduledTransitionProcessorDefinitionDto from
OpenAPI does not change wire behaviour for the SPI's free-string Type
field. JSON marshal+unmarshal through spi.WorkflowDefinition preserves
the value byte-for-byte.

Anchors that Type is a free string in the SPI — not constrained by an
enum at the storage or transport layer.
EOF
)"
```

---

## Task 10: Add validator-ordering test (StartNewTxOnDispatch + Type interactions)

**Files:**
- Create: `internal/domain/workflow/handler_type_validation_test.go`

The validator (`validateProcessorFlags`) runs at workflow import time. The Type-axis rejection runs at engine fire time. The two checks are independent — when both could fire (e.g. `Type: internalized + StartNewTxOnDispatch: true + ExecutionMode: SYNC`), the validator wins because import precedes any fire.

- [ ] **Step 1: Create the test file**

Use the Write tool:

```go
package workflow

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestValidator_TypeInternalized_WithStartNewTxOnDispatch_FailsAtImport
// asserts that the existing StartNewTxOnDispatch flag-coherence check
// (validate.go:51-55) fires at import time and short-circuits before any
// engine-time Type rejection can run. Two sub-cases pin the ordering.
func TestValidator_TypeInternalized_WithStartNewTxOnDispatch(t *testing.T) {
	t.Run("internalized+SYNC+startNewTxOnDispatch=true is rejected at import", func(t *testing.T) {
		trueVal := true
		wf := spi.WorkflowDefinition{
			Version: "1.0", Name: "BadFlagsWF", InitialState: "INITIAL", Active: true,
			States: map[string]spi.StateDefinition{
				"INITIAL": {Transitions: []spi.TransitionDefinition{
					{Name: "RUN", Next: "DONE", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{
								Type:          ProcessorTypeInternalized,
								Name:          "p",
								ExecutionMode: ExecutionModeSync,
								Config: spi.ProcessorConfig{
									StartNewTxOnDispatch: &trueVal,
								},
							},
						}},
				}},
				"DONE": {},
			},
		}

		err := validateWorkflows([]spi.WorkflowDefinition{wf})
		if err == nil {
			t.Fatalf("expected validator to reject startNewTxOnDispatch=true with non-CBD ExecutionMode, got nil")
		}
		// Error should reference startNewTxOnDispatch — confirms it's the
		// flag-coherence check firing, NOT the Type-axis engine rejection.
		if !contains(err.Error(), "startNewTxOnDispatch") {
			t.Errorf("error = %q, want it to mention startNewTxOnDispatch (proves flag-coherence check fired, not Type-axis)", err.Error())
		}
	})

	t.Run("internalized+COMMIT_BEFORE_DISPATCH+startNewTxOnDispatch=true passes validator (Type rejection happens at engine fire only)", func(t *testing.T) {
		trueVal := true
		wf := spi.WorkflowDefinition{
			Version: "1.0", Name: "ValidFlagsWF", InitialState: "INITIAL", Active: true,
			States: map[string]spi.StateDefinition{
				"INITIAL": {Transitions: []spi.TransitionDefinition{
					{Name: "RUN", Next: "DONE", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{
								Type:          ProcessorTypeInternalized,
								Name:          "p",
								ExecutionMode: ExecutionModeCommitBeforeDispatch,
								Config: spi.ProcessorConfig{
									StartNewTxOnDispatch: &trueVal,
								},
							},
						}},
				}},
				"DONE": {},
			},
		}

		err := validateWorkflows([]spi.WorkflowDefinition{wf})
		if err != nil {
			t.Fatalf("validator should pass for coherent CBD flags regardless of Type; got %v", err)
		}
		// Engine-time rejection is covered by
		// TestInternalizedRejection_ExecutionModeMatrix/ExecutionMode_COMMIT_BEFORE_DISPATCH.
	})
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || stringContains(haystack, needle))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
```

Note: the local `contains` and `stringContains` helpers avoid importing `strings` here; the existing test file may have the helper too. If `internal/domain/workflow/engine_processors_type_test.go` already imports `strings`, prefer that — but this test file is independent. Inspect to keep DRY:

Run (Bash) before writing:
```bash
grep -l "func contains\|strings.Contains" internal/domain/workflow/*_test.go | head
```

If a `contains` helper already exists in the package's test files, drop the local helpers and just use `strings.Contains` instead — replace `contains(err.Error(), ...)` with `strings.Contains(err.Error(), ...)` and adjust the imports accordingly.

- [ ] **Step 2: Run the validator-ordering test**

Run:
```bash
go test ./internal/domain/workflow/ -run TestValidator_TypeInternalized_WithStartNewTxOnDispatch -v
```

Expected: both sub-cases PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/handler_type_validation_test.go
git commit -m "$(cat <<'EOF'
test(workflow): validator vs engine rejection ordering for Type: internalized

Two sub-cases pin the ordering contract:
- Type: internalized + ExecutionMode: SYNC + StartNewTxOnDispatch: true
  → rejected at import by the existing validateProcessorFlags check
  (flag-coherence error mentions startNewTxOnDispatch); engine-time
  Type rejection never runs.
- Type: internalized + ExecutionMode: COMMIT_BEFORE_DISPATCH +
  StartNewTxOnDispatch: true → validator passes (the flags are
  coherent); engine rejects at fire time (already covered by the
  ExecutionMode matrix test).

Documents the layered validation contract: existing validators own
import-time tightening; the new Type-axis check is purely an engine
fire-time concern.
EOF
)"
```

---

## Task 11: Sweep stale Type fixtures in workflow tests (Gate 6)

Replace `Type: "EXTERNAL"` / `"INTERNAL"` / `"external"` with `ProcessorTypeExternalized` or drop. Per memory `feedback_gate6_no_followups`, resolve in this PR — don't defer.

**Files:**
- Modify: `internal/domain/workflow/engine_test.go` (~30 lines)
- Modify: `internal/domain/workflow/engine_ifmatch_test.go` (~3 lines)
- Modify: `internal/domain/workflow/engine_result_test.go` (~2 lines)
- Modify: `internal/domain/workflow/engine_transition_aborted_test.go` (~2 lines)
- Modify: `internal/domain/workflow/scenarios_test.go` (~5 lines)

- [ ] **Step 1: List all stale fixture occurrences**

Run:
```bash
grep -rn 'Type:.*"\(EXTERNAL\|INTERNAL\|external\|scheduled\)"' \
     --include="*_test.go" internal/domain/workflow/
```

Expected: ~40 lines across 5 files. Capture the full list to a scratchpad for the next steps.

- [ ] **Step 2: Replace `Type: "EXTERNAL"` with `Type: ProcessorTypeExternalized` across all matches**

For each file in the grep output, use the Edit tool with `replace_all: true`:

```
old_string: Type: "EXTERNAL",
new_string: Type: ProcessorTypeExternalized,
```

Apply to:
- `internal/domain/workflow/engine_test.go`
- `internal/domain/workflow/engine_ifmatch_test.go`
- `internal/domain/workflow/engine_result_test.go`
- `internal/domain/workflow/engine_transition_aborted_test.go`
- `internal/domain/workflow/scenarios_test.go`

For each file run:
```bash
grep -c 'Type: ProcessorTypeExternalized' internal/domain/workflow/engine_test.go
```
The count should match the original `Type: "EXTERNAL"` count for that file.

- [ ] **Step 3: Replace `Type: "external"` (lowercase, non-canonical)**

Use the Edit tool on `internal/domain/workflow/engine_test.go`:
```
old_string: {Type: "external", Name: "myProcessor"},
new_string: {Type: ProcessorTypeExternalized, Name: "myProcessor"},
```

Verify the only such occurrence is at line 407:
```bash
grep -n 'Type:.*"external"' internal/domain/workflow/*_test.go
```

Expected: no matches after the edit.

- [ ] **Step 4: Replace `Type: "INTERNAL"` (uppercase, stale non-canonical)**

These tests pre-date #250 and use `Type: "INTERNAL"` as a free-string value — post-#250, the value `"INTERNAL"` falls through (it's exact-mismatch against `"internalized"`). To keep the test intent honest, replace with `ProcessorTypeExternalized` (matches the rest of the test's externalized-only behaviour).

Use the Edit tool on `internal/domain/workflow/engine_test.go`:

```
old_string: {Type: "INTERNAL", Name: "sync-proc", ExecutionMode: "SYNC"},
new_string: {Type: ProcessorTypeExternalized, Name: "sync-proc", ExecutionMode: "SYNC"},
```

```
old_string: {Type: "INTERNAL", Name: "same-proc", ExecutionMode: "ASYNC_SAME_TX"},
new_string: {Type: ProcessorTypeExternalized, Name: "same-proc", ExecutionMode: "ASYNC_SAME_TX"},
```

Verify:
```bash
grep -n 'Type:.*"INTERNAL"' internal/domain/workflow/*_test.go
```

Expected: no matches after the edit.

- [ ] **Step 5: Final grep — confirm sweep is complete**

Run:
```bash
grep -rn 'Type:.*"\(EXTERNAL\|INTERNAL\|external\|scheduled\)"' \
     --include="*_test.go" internal/domain/workflow/
```

Expected: empty output.

- [ ] **Step 6: Re-run the workflow package tests**

Run:
```bash
go test ./internal/domain/workflow/... -v 2>&1 | tail -30
```

Expected: all PASS. The sweep should be behaviour-preserving — `"EXTERNAL"`, `"INTERNAL"`, etc. were all falling through to ExecutionMode dispatch already; `ProcessorTypeExternalized` (= `"externalized"`) is also fall-through-via-recognised-value. No semantic change.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/workflow/engine_test.go \
        internal/domain/workflow/engine_ifmatch_test.go \
        internal/domain/workflow/engine_result_test.go \
        internal/domain/workflow/engine_transition_aborted_test.go \
        internal/domain/workflow/scenarios_test.go
git commit -m "$(cat <<'EOF'
test(workflow): normalise stale Type fixture strings to constants

Gate 6 sweep — replace Type: "EXTERNAL" / "INTERNAL" / "external"
literals in internal/domain/workflow/**_test.go with the new
workflow.ProcessorTypeExternalized constant. The "INTERNAL" fixtures
were particularly risky after the Type-axis rejection landed: a
future reader sees Type: "INTERNAL" and reasonably assumes it tests
the rejection path, when in fact it falls through (case-mismatch
against "internalized").

Behaviour-preserving: all the replaced literals were falling through
to ExecutionMode dispatch already; ProcessorTypeExternalized
("externalized") is also fall-through-via-recognised-value.
EOF
)"
```

---

## Task 12: E2E test — POST /workflow/import + manual transition fire returns 400 WORKFLOW_FAILED

**Files:**
- Create: `internal/e2e/workflow_internalized_test.go`

Model on existing `internal/e2e/` workflow tests (e.g. `internal/e2e/collection_workflow_test.go`). The E2E harness spins up its own PostgreSQL container via testcontainers-go and an in-process `httptest.Server` with JWT auth — Docker required.

- [ ] **Step 1: Inspect an existing E2E workflow test pattern**

Run:
```bash
ls internal/e2e/*workflow*
head -80 internal/e2e/collection_workflow_test.go 2>/dev/null || head -80 internal/e2e/entity_lifecycle_test.go
```

Note the helper functions used (likely `helpers_test.go`) and the JSON-body format the existing tests POST.

- [ ] **Step 2: Create the E2E test**

Use the Write tool to create `internal/e2e/workflow_internalized_test.go`:

```go
package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_InternalizedRejection_ManualTransitionReturns400 verifies the
// full HTTP path: import a workflow whose transition has a Type:
// "internalized" processor → import succeeds (no Type-axis validation at
// handler) → create an entity (no cascade) → fire the manual transition
// → 400 WORKFLOW_FAILED with the rejection sub-string → GET entity shows
// it's still in the source state.
func TestE2E_InternalizedRejection_ManualTransitionReturns400(t *testing.T) {
	client := newAuthedClient(t)
	model := "internalized-reject-e2e"

	// (1) Create the entity model. Use whatever helper the rest of
	// internal/e2e/ uses — for collection_workflow_test.go style this is
	// likely a helper like ensureModel(t, client, model, 1) or a direct
	// POST. Adjust the call shape to the existing pattern; the body shape
	// for the model is irrelevant to this test as long as version=1 is
	// created.
	ensureModel(t, client, model, 1)

	// (2) Import the workflow with a Type: "internalized" processor on a
	// Manual=true transition.
	importBody := map[string]any{
		"workflows": []map[string]any{
			{
				"version":      "1.0",
				"name":         "InternalizedRejectWF",
				"initialState": "INITIAL",
				"active":       true,
				"states": map[string]any{
					"INITIAL": map[string]any{
						"transitions": []map[string]any{
							{
								"name":     "FIRE",
								"next":     "DONE",
								"manual":   true,
								"disabled": false,
								"processors": []map[string]any{
									{
										"type":          "internalized",
										"name":          "internal-proc",
										"executionMode": "SYNC",
									},
								},
							},
						},
					},
					"DONE": map[string]any{},
				},
			},
		},
	}
	importBytes, _ := json.Marshal(importBody)
	resp := doRequest(t, client, http.MethodPost,
		"/model/"+model+"/1/workflow/import",
		"application/json", importBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workflow import failed: status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	// (3) Create an entity instance — initial state, no cascade fires
	// because the only transition is Manual=true.
	entityResp := doRequest(t, client, http.MethodPost,
		"/entity/"+model+"/1",
		"application/json", []byte(`{"some":"data"}`))
	if entityResp.StatusCode != http.StatusOK && entityResp.StatusCode != http.StatusCreated {
		t.Fatalf("entity create failed: status=%d body=%s", entityResp.StatusCode, readBody(t, entityResp))
	}
	var entityCreatedBody map[string]any
	if err := json.NewDecoder(entityResp.Body).Decode(&entityCreatedBody); err != nil {
		t.Fatalf("entity create response decode: %v", err)
	}
	entityID, _ := entityCreatedBody["id"].(string)
	if entityID == "" {
		t.Fatalf("entity create response missing id: %+v", entityCreatedBody)
	}

	// (4) Fire the manual transition. Expect 400 WORKFLOW_FAILED with the
	// rejection sub-string.
	fireResp := doRequest(t, client, http.MethodPost,
		"/entity/"+model+"/1/"+entityID+"/transition/FIRE",
		"application/json", []byte("{}"))
	if fireResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("transition fire: status=%d (want 400), body=%s", fireResp.StatusCode, readBody(t, fireResp))
	}
	fireBody := readBody(t, fireResp)
	if !strings.Contains(fireBody, "WORKFLOW_FAILED") {
		t.Errorf("response body missing WORKFLOW_FAILED code: %s", fireBody)
	}
	if !strings.Contains(fireBody, `execution type "internalized" is not yet implemented`) {
		t.Errorf("response body missing rejection sub-string: %s", fireBody)
	}

	// (5) GET the entity — confirm it's in the initial state.
	getResp := doRequest(t, client, http.MethodGet,
		"/entity/"+model+"/1/"+entityID,
		"application/json", nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("entity get: status=%d body=%s", getResp.StatusCode, readBody(t, getResp))
	}
	var getBody map[string]any
	if err := json.NewDecoder(getResp.Body).Decode(&getBody); err != nil {
		t.Fatalf("entity get decode: %v", err)
	}
	// State field name in the response body depends on the existing
	// project conventions — adapt as needed by inspecting helpers_test.go.
	// Common shapes are getBody["meta"].(map[string]any)["state"] or
	// getBody["state"]. Pick the right one based on actual response shape.
	state := extractEntityState(t, getBody)
	if state != "INITIAL" {
		t.Errorf("entity state = %q after failed transition, want INITIAL (source state preserved)", state)
	}
}
```

**Note for the implementer:** the exact helper names (`newAuthedClient`, `ensureModel`, `doRequest`, `readBody`, `extractEntityState`) likely differ from what `internal/e2e/helpers_test.go` actually exports. Before writing the file, run:
```bash
grep -n 'func ' internal/e2e/helpers_test.go | head -20
```
Adapt the helper calls to match the existing names. If a helper doesn't exist for "extract state from entity response", inline the equivalent (`getBody["meta"].(map[string]any)["state"].(string)` or similar based on the actual shape). The test's *behaviour* is the contract; helper choice is implementation detail.

- [ ] **Step 3: Run the E2E test (Docker required)**

Run:
```bash
go test ./internal/e2e/ -run TestE2E_InternalizedRejection_ManualTransitionReturns400 -v
```

Expected: PASS. Test takes 5-15s due to testcontainers Postgres start-up.

If Docker isn't available, the test will error with a `docker not running` message — that's an environment issue, not a test failure.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/workflow_internalized_test.go
git commit -m "$(cat <<'EOF'
test(e2e): internalized processor returns 400 via full HTTP stack

End-to-end test exercising the full path: workflow import → entity
create → manual transition fire → 400 WORKFLOW_FAILED with rejection
sub-string in body → entity remains in source state on subsequent GET.

The import step succeeds because handler.go bypasses OpenAPI DTOs —
Type-axis validation is purely an engine fire-time concern in #250.
EOF
)"
```

---

## Task 13: Modify `api/openapi.yaml` — enum, mapping, drop scheduled DTOs, collapse oneOf

**Files:**
- Modify: `api/openapi.yaml` lines 8672-8753

- [ ] **Step 1: Inspect the current state around line 8672**

Run:
```bash
sed -n '8665,8755p' api/openapi.yaml
```

Expected: the current `ExternalizedProcessorDefinitionDto`, `ProcessorDefinitionDto`, `ScheduledTransitionConfigDto`, `ScheduledTransitionProcessorDefinitionDto`, and `TransitionDefinitionDto.processors[]` definitions.

- [ ] **Step 2: Rewrite `ProcessorDefinitionDto`**

Use the Edit tool. `old_string`:
```
    ProcessorDefinitionDto:
      type: object
      discriminator:
        propertyName: type
      properties:
        type:
          type: string
          description: Type of the processor (externalized or scheduled)
        name:
          type: string
          description: Name of the processor
      required:
        - name
```

`new_string`:
```
    ProcessorDefinitionDto:
      type: object
      discriminator:
        propertyName: type
        mapping:
          externalized: "#/components/schemas/ExternalizedProcessorDefinitionDto"
      properties:
        type:
          type: string
          enum:
            - externalized
          description: |
            Processor execution-location axis. `externalized` dispatches the
            processor via gRPC to a calculation node selected by
            `calculationNodesTags`. Empty or omitted is treated as
            `externalized` by the runtime. The engine reserves the value
            `internalized` for an in-process execution location not yet
            implemented; any payload carrying `type: "internalized"` is
            rejected at engine dispatch with `WORKFLOW_FAILED` (400). The
            reserved value is intentionally absent from this enum until the
            in-process subtype lands together with its schema.
        name:
          type: string
          description: Name of the processor
      required:
        - name
```

- [ ] **Step 3: Remove `ScheduledTransitionConfigDto` and `ScheduledTransitionProcessorDefinitionDto`**

Use the Edit tool. `old_string`:
```
    ScheduledTransitionConfigDto:
      type: object
      properties:
        delayMs:
          type: integer
          format: int64
          description: Delay in milliseconds before executing the transition
        transition:
          type: string
          description: The transition to execute after waiting delayMs
        timeoutMs:
          type: integer
          format: int64
          description: Timeout in milliseconds for executing the transition task, after
            which it will be expired
      required:
        - delayMs
        - transition
    ScheduledTransitionProcessorDefinitionDto:
      allOf:
        - $ref: "#/components/schemas/ProcessorDefinitionDto"
        - type: object
          properties:
            config:
              $ref: "#/components/schemas/ScheduledTransitionConfigDto"
      required:
        - config
        - name
    StateDefinitionDto:
```

`new_string`:
```
    StateDefinitionDto:
```

- [ ] **Step 4: Collapse the `TransitionDefinitionDto.processors[].oneOf` to a single member**

Use the Edit tool. `old_string`:
```
        processors:
          type: array
          description: List of processors to execute for this transition
          items:
            oneOf:
              - $ref: "#/components/schemas/ExternalizedProcessorDefinitionDto"
              - $ref: "#/components/schemas/ScheduledTransitionProcessorDefinitionDto"
```

`new_string`:
```
        processors:
          type: array
          description: List of processors to execute for this transition
          items:
            oneOf:
              - $ref: "#/components/schemas/ExternalizedProcessorDefinitionDto"
```

- [ ] **Step 5: Verify no other references to the deleted DTOs**

Run:
```bash
grep -n "ScheduledTransition" api/openapi.yaml
```

Expected: empty output.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml
git commit -m "$(cat <<'EOF'
refactor(openapi): split processor type from scheduled-transition shape

- ProcessorDefinitionDto.type: enum: [externalized] + explicit
  discriminator.mapping. Description documents the reserved internalized
  value (rejected at engine dispatch as not yet implemented) without
  enum-ing it until the subtype lands.
- ScheduledTransitionConfigDto: removed. The shape returns at its
  proper home (sibling primitive on TransitionDefinition) in a separate
  carve-out.
- ScheduledTransitionProcessorDefinitionDto: removed.
- TransitionDefinitionDto.processors[].oneOf: collapsed to a single
  member (ExternalizedProcessorDefinitionDto). The wrapper stays
  forward-compatible for InternalizedProcessorDefinitionDto.
EOF
)"
```

---

## Task 14: Regenerate `api/generated.go`

**Files:**
- Modify: `api/generated.go` (regenerated)

- [ ] **Step 1: Run the generator**

Run:
```bash
go generate ./api/...
```

Expected: clean run, no errors. The output should regenerate `api/generated.go` (and possibly an embedded swagger spec via `tools/swagger-encode`).

- [ ] **Step 2: Verify acceptance criteria — build is clean**

Run:
```bash
go build ./...
```

Expected: clean (no errors).

- [ ] **Step 3: Verify acceptance criteria — scheduled DTOs disappeared**

Run:
```bash
grep -n "ScheduledTransition" api/generated.go
```

Expected: empty output. If any matches remain, the regeneration is incomplete — investigate.

- [ ] **Step 4: Verify acceptance criteria — externalized union helpers preserved**

Run:
```bash
grep -n "AsExternalizedProcessorDefinitionDto\|FromExternalizedProcessorDefinitionDto\|MergeExternalizedProcessorDefinitionDto" api/generated.go
```

Expected: three matches (one for each helper).

- [ ] **Step 5: Verify acceptance criteria — processors[] union type preserved**

Run:
```bash
grep -n "TransitionDefinitionDto_Processors_Item\|Processors\s*\[\]" api/generated.go | head -5
```

Expected: at least one match showing the union-helper type or the embedded array shape. Open the area in an editor if needed to confirm the shape is structurally correct.

- [ ] **Step 6: Fallback path if any of (2)–(5) fail**

If `oapi-codegen` produced broken output (e.g. malformed union helpers because of the single-member `oneOf`), drop the `oneOf` wrapper as the spec's fallback path:

Use the Edit tool on `api/openapi.yaml`. `old_string`:
```
        processors:
          type: array
          description: List of processors to execute for this transition
          items:
            oneOf:
              - $ref: "#/components/schemas/ExternalizedProcessorDefinitionDto"
```

`new_string`:
```
        processors:
          type: array
          description: List of processors to execute for this transition
          items:
            $ref: "#/components/schemas/ExternalizedProcessorDefinitionDto"
```

Then re-run `go generate ./api/...` and `go build ./...`. Document the fallback choice in the PR description.

- [ ] **Step 7: Run the full test suite to confirm no breakage from the regeneration**

Run:
```bash
go test -short ./...
```

Expected: all PASS. If anything breaks, the issue is most likely in test files that reference DTO types removed from `api/generated.go` (unlikely — the spec confirmed no Go test code outside `api/generated.go` references the scheduled DTOs).

- [ ] **Step 8: Commit**

```bash
git add api/generated.go
git commit -m "$(cat <<'EOF'
refactor(api): regenerate generated.go from updated openapi.yaml

Picks up the schema cleanup:
- ScheduledTransitionProcessorDefinitionDto + ScheduledTransitionConfigDto
  removed; the three union helpers
  (As/From/Merge)ScheduledTransitionProcessorDefinitionDto disappear.
- ExternalizedProcessorDefinitionDto and its union helpers preserved.
- ProcessorDefinitionDto.Type gains the constrained enum constant emit
  from oapi-codegen.
EOF
)"
```

If the swagger-encode artefact also changed (a separate file), stage and commit it in the same commit.

---

## Task 15: Update SPI docstring (sibling repo)

**Files:**
- Modify: `../cyoda-go-spi/types.go` (sibling repo at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`)

The SPI change is comment-only. Per the spec, no SPI tag is forced by #250 — the v0.8.0 bundled SPI tag (carrying #259's and #260's substantive shape work) picks up this docstring change. Land the change as a PR against `cyoda-go-spi`'s `main` branch (or its v0.8.0 prep branch — verify with the upstream).

- [ ] **Step 1: Open the SPI repo and inspect the current docstring**

Run:
```bash
sed -n '135,165p' /Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go
```

Expected: the existing `ProcessorDefinition` struct.

- [ ] **Step 2: Rewrite the `Type` field docstring**

Use the Edit tool. `old_string`:
```
// ProcessorDefinition represents a processor attached to a transition.
type ProcessorDefinition struct {
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	ExecutionMode string          `json:"executionMode,omitempty"`
	Config        ProcessorConfig `json:"config,omitempty"`
}
```

`new_string`:
```
// ProcessorDefinition represents a processor attached to a transition.
type ProcessorDefinition struct {
	// Type is the execution-location axis. Recognised values are defined
	// by the cyoda-go engine package; canonical values are "externalized"
	// (dispatched via gRPC to a calculation node selected by
	// Config.CalculationNodesTags) and "internalized" (reserved for an
	// in-process execution location, currently rejected at engine
	// dispatch as not yet implemented). Empty is treated as "externalized".
	// Any value other than "internalized" falls through to the
	// ExecutionMode dispatch path; import-time validation does not
	// constrain this field.
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	ExecutionMode string          `json:"executionMode,omitempty"`
	Config        ProcessorConfig `json:"config,omitempty"`
}
```

- [ ] **Step 3: From the SPI repo, build/vet to confirm no breakage**

Run (from the SPI repo directory):
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go build ./...
go vet ./...
cd -
```

Expected: clean.

- [ ] **Step 4: Open a PR against cyoda-go-spi**

This is a sibling-repo PR, not a commit in the cyoda-go worktree. Use `gh` from the SPI repo directory:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout -b spi-docstring-processor-type
git add types.go
git commit -m "$(cat <<'EOF'
docs: document ProcessorDefinition.Type as execution-location axis

Aligns with cyoda-go#250. Comment-only — no symbol or signature
change. The cyoda-go engine package owns the recognised value
semantics; this docstring documents the contract for SPI consumers
(in-tree plugins + the commercial backend).
EOF
)"
git push -u origin spi-docstring-processor-type
gh pr create --title "docs: document ProcessorDefinition.Type as execution-location axis" \
             --body "$(cat <<'EOF'
## Summary

- Document \`ProcessorDefinition.Type\` as the execution-location axis (\`externalized\` | reserved \`internalized\`).
- Comment-only — no symbol or signature change. No tag bump required for #250 alone; this docstring rides the bundled v0.8.0 SPI tag that #259 / #260 will need.

## Cross-reference

- cyoda-go #250 — schema cleanup that motivates this docstring.

## Test plan

- [x] \`go build ./...\` clean
- [x] \`go vet ./...\` clean
- [ ] Parity registry runs green against in-tree + commercial backends once #259 / #260 land the bundled tag

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
cd -
```

Note the PR URL for the cyoda-go PR description.

- [ ] **Step 5: Return to the worktree (no commit in this repo)**

The SPI change lives in the sibling repo; no commit happens in the cyoda-go worktree for Task 15. Continue with Task 16.

---

## Task 16: Update `cmd/cyoda/help/content/workflows.md` (lines 63, 130, 135-139)

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md`

- [ ] **Step 1: Fix the stale `EXTERNAL` example at line 63**

Run:
```bash
sed -n '60,70p' cmd/cyoda/help/content/workflows.md
```

Note the exact context.

Use the Edit tool. `old_string`:
```
              "type": "EXTERNAL",
```

`new_string`:
```
              "type": "externalized",
```

- [ ] **Step 2: Rewrite lines 130 + 135-139 (the EXTERNAL claim)**

Inspect lines 125-142:
```bash
sed -n '125,142p' cmd/cyoda/help/content/workflows.md
```

Use the Edit tool. `old_string`:
```
- `type` — string — processor type; see valid values below
- `name` — string — logical processor name
- `executionMode` — string — execution mode; see valid values below
- `config` — `ProcessorConfig`

**Valid `type` values (exhaustive for v0.6.1):**

- `"EXTERNAL"` — dispatches to a calculation node via gRPC using `calculationNodesTags` for routing

No other types are supported. Supplying any other value produces `errors.VALIDATION_FAILED` at workflow import time.
```

`new_string`:
```
- `type` — string — execution-location axis; see below for valid values
- `name` — string — logical processor name
- `executionMode` — string — execution mode; see valid values below
- `config` — `ProcessorConfig`

**Processor `type` (execution-location axis):**

- `"externalized"` (default when omitted) — dispatched via gRPC to a calculation node selected by `Config.calculationNodesTags`. This is the only execution location implemented today; all the `executionMode` semantics below apply to externalized processors.

The engine reserves the value `"internalized"` for an in-process execution location not yet implemented. Any transition that fires a processor with `type: "internalized"` is rejected at dispatch with `WORKFLOW_FAILED` (400) and the operator-visible error `processor X failed: execution type "internalized" is not yet implemented`. The reserved value is intentionally absent from the OpenAPI enum until the subtype lands; workflow authors who include it in import payloads will not be rejected at import, but their entities cannot transit past the affected step.

Any value other than `"internalized"` (including the empty string, the canonical `"externalized"`, and unknown values such as legacy `"scheduled"` or `"EXTERNAL"`) falls through to the `executionMode` dispatch path. This permissiveness will narrow in a future release; do not rely on it.
```

- [ ] **Step 3: Verify the rewrites landed**

Run:
```bash
grep -n "externalized\|EXTERNAL" cmd/cyoda/help/content/workflows.md
```

Expected: matches contain `"externalized"`, `"EXTERNAL"` (only in the parenthetical "such as legacy" string), no claim that `"EXTERNAL"` is exhaustive.

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/workflows.md
git commit -m "$(cat <<'EOF'
docs(help): rewrite workflows.md processor.type section

- Stale example payload at line 63: "EXTERNAL" → "externalized".
- Field-listing one-liner: "type" is now described as the
  execution-location axis.
- The EXTERNAL claim at 135-139 (claimed exhaustive + claimed
  VALIDATION_FAILED at import — both false) replaced with an
  accurate description of the two recognised values, the engine's
  fire-time rejection of "internalized", and the fall-through
  behaviour for unknown values.
EOF
)"
```

---

## Task 17: Update `cmd/cyoda/help/content/grpc.md:219`

**Files:**
- Modify: `cmd/cyoda/help/content/grpc.md`

- [ ] **Step 1: Inspect line 219**

Run:
```bash
sed -n '215,225p' cmd/cyoda/help/content/grpc.md
```

- [ ] **Step 2: Replace `EXTERNAL` with `externalized`**

Use the Edit tool. `old_string`:
```
Server sends `EntityProcessorCalculationRequest` when a workflow transition invokes an `EXTERNAL` processor whose `calculationNodesTags` matches one of the member's declared tags:
```

`new_string`:
```
Server sends `EntityProcessorCalculationRequest` when a workflow transition invokes an `externalized` processor whose `calculationNodesTags` matches one of the member's declared tags:
```

- [ ] **Step 3: Verify**

Run:
```bash
grep -n "EXTERNAL" cmd/cyoda/help/content/grpc.md
```

Expected: empty output.

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/grpc.md
git commit -m "docs(help): grpc.md — EXTERNAL → externalized for processor type"
```

---

## Task 18: Strip the scheduled processor entry from parity scenario 08

**Files:**
- Modify: `e2e/externalapi/scenarios/08-workflow-import-export.yaml`

- [ ] **Step 1: Inspect the affected lines**

Run:
```bash
sed -n '100,135p' e2e/externalapi/scenarios/08-workflow-import-export.yaml
```

Expected: the `wf-import/03` test definition with the import body containing two `processors[]` entries.

- [ ] **Step 2: Strip the scheduled entry and add the explanatory comment**

Use the Edit tool. `old_string`:
```
  - id: wf-import/03-advanced-criteria-and-processors
    name: Advanced FSM: group criterion (AND), function criterion, scheduled processor
    source_test: integration-tests/src/test/kotlin/net/cyoda/saas/model/workflow/EntityModelWorkflowInteractorIT.kt#test import workflow with criterion on transition and processors
    data:
      model: { name: wfAdvanced, version: 1 }
      import_body: |
        {"workflows":[{
          "version":"1.0","name":"Advanced Test Workflow",
          "desc":"A workflow with transition criteria and processors",
          "initialState":"pending","active":true,
          "criterion":{"type":"simple","jsonPath":"$.category",
                       "operation":"EQUALS","value":"premium"},
          "states":{
            "pending":{"transitions":[{
              "name":"Approve with Validation","next":"approved",
              "manual":true,"disabled":false,
              "criterion":{"type":"group","operator":"AND","conditions":[
                {"type":"simple","jsonPath":"$.score","operation":"GREATER_OR_EQUAL","value":80},
                {"type":"function","function":{"name":"validate_entity_eligibility",
                  "config":{"attachEntity":true,"calculationNodesTags":"validation,eligibility",
                            "responseTimeoutMs":3000,"retryPolicy":"FIXED"}}}
              ]},
              "processors":[
                {"name":"send_approval_notification","executionMode":"ASYNC_NEW_TX",
                 "config":{"attachEntity":true,"calculationNodesTags":"notification,approval",
                           "responseTimeoutMs":5000,"retryPolicy":"NONE"}},
                {"type":"scheduled","name":"schedule_close_process",
                 "config":{"delayMs":300,"timeoutMs":30000,"transition":"Close"}}
              ]}]},
            "approved":{"transitions":[{"name":"Close","manual":true,"next":"closed","disabled":false}]},
            "closed":{"transitions":[]}
          }
        }]}
```

`new_string`:
```
  # wf-import/03 originally exercised a scheduled-transition processor inside
  # processors[]. That conflation has been removed from the schema; the
  # scheduled-transition primitive is reintroduced at its proper home (a
  # sibling primitive on TransitionDefinition) in a separate scenario. This
  # test now covers group-criterion + function-criterion + externalized-
  # processor round-trip only.
  - id: wf-import/03-advanced-criteria-and-processors
    name: Advanced FSM: group criterion (AND), function criterion, externalized processor
    source_test: integration-tests/src/test/kotlin/net/cyoda/saas/model/workflow/EntityModelWorkflowInteractorIT.kt#test import workflow with criterion on transition and processors
    data:
      model: { name: wfAdvanced, version: 1 }
      import_body: |
        {"workflows":[{
          "version":"1.0","name":"Advanced Test Workflow",
          "desc":"A workflow with transition criteria and processors",
          "initialState":"pending","active":true,
          "criterion":{"type":"simple","jsonPath":"$.category",
                       "operation":"EQUALS","value":"premium"},
          "states":{
            "pending":{"transitions":[{
              "name":"Approve with Validation","next":"approved",
              "manual":true,"disabled":false,
              "criterion":{"type":"group","operator":"AND","conditions":[
                {"type":"simple","jsonPath":"$.score","operation":"GREATER_OR_EQUAL","value":80},
                {"type":"function","function":{"name":"validate_entity_eligibility",
                  "config":{"attachEntity":true,"calculationNodesTags":"validation,eligibility",
                            "responseTimeoutMs":3000,"retryPolicy":"FIXED"}}}
              ]},
              "processors":[
                {"name":"send_approval_notification","executionMode":"ASYNC_NEW_TX",
                 "config":{"attachEntity":true,"calculationNodesTags":"notification,approval",
                           "responseTimeoutMs":5000,"retryPolicy":"NONE"}}
              ]}]},
            "approved":{"transitions":[{"name":"Close","manual":true,"next":"closed","disabled":false}]},
            "closed":{"transitions":[]}
          }
        }]}
```

- [ ] **Step 3: Confirm no scheduled entries remain across parity scenarios**

Run:
```bash
grep -rn '"type":"scheduled"' e2e/externalapi/scenarios/
```

Expected: empty output.

- [ ] **Step 4: Run the parity test suite**

The parity scenarios are driven by the parity test runner. Inspect existing tests for how they fire:

Run:
```bash
grep -rn "08-workflow-import-export.yaml\|TestParityScenarios" e2e/ internal/ 2>/dev/null | head -5
```

Identify the test entry point (likely something like `TestParityScenarios` or a generic runner that walks the `e2e/externalapi/scenarios/` directory). Run it:

```bash
go test ./e2e/parity/... -v -run 'wf-import/03' 2>&1 | tail -30
```

Or, if the parity runner uses a different invocation, adapt the command. The acceptance criterion: `wf-import/03` passes with the new import_body.

Expected: PASS. The `json_equals_normalized` assertion holds because import and export both contain only the externalized processor entry.

- [ ] **Step 5: Commit**

```bash
git add e2e/externalapi/scenarios/08-workflow-import-export.yaml
git commit -m "$(cat <<'EOF'
test(parity): scenario 08/wf-import/03 — strip scheduled processor entry

The scheduled-transition conflation in processors[] has been removed
from the schema. The scenario's round-trip assertion still holds for
the remaining group-criterion + function-criterion + externalized
processor combination.

Sweep of e2e/externalapi/scenarios/ confirms 08 was the only scenario
carrying a "type":"scheduled" payload.
EOF
)"
```

---

## Task 19: Final verification before PR

**Files:** None (verification only).

- [ ] **Step 1: Run `go vet ./...`**

Run:
```bash
go vet ./...
```

Expected: no output.

- [ ] **Step 2: Run `go build ./...`**

Run:
```bash
go build ./...
```

Expected: clean.

- [ ] **Step 3: Run the full root-module test suite (short mode)**

Run:
```bash
go test -short ./... 2>&1 | tail -30
```

Expected: all PASS.

- [ ] **Step 4: Run the full root-module test suite without -short (includes E2E + Docker)**

Docker must be running.

Run:
```bash
go test ./... 2>&1 | tail -40
```

Expected: all PASS. The new `TestE2E_InternalizedRejection_ManualTransitionReturns400` runs here.

- [ ] **Step 5: Run plugin submodule tests (`make test-all`)**

Per memory `feedback_plugin_submodule_tests`, root `go test ./...` skips `plugins/memory|sqlite|postgres`. Run the aggregator:

```bash
make test-all 2>&1 | tail -30
```

Expected: all PASS across root + memory + sqlite + postgres. Docker required for postgres.

- [ ] **Step 6: One-shot race detector run**

Per `.claude/rules/race-testing.md`, race detector is end-of-deliverable.

Run:
```bash
go test -race ./... 2>&1 | tail -20
```

Expected: all PASS, no race detected.

- [ ] **Step 7: `make todos` — no new TODOs introduced**

Run:
```bash
make todos 2>&1 | head -30
```

Expected: same set as before this PR; no new entries from #250 work. (The engine's default-branch "tolerate unknown Type" is a deferral to #255, but per the no-issue-IDs rule the source comment must not reference it — the deferral is recorded in this spec and the PR body.)

- [ ] **Step 8: Confirm no GitHub issue IDs leaked into shipped artefacts**

Run:
```bash
grep -rn '#25[0-9]\|#26[0-9]\|#19[0-9]\|#13[0-9]\|#3[0-9]' \
     api/ internal/ cmd/ docs/PROCESSOR_EXECUTION_MODES.md \
     cyoda-go-spi/types.go 2>/dev/null \
     | grep -v 'docs/superpowers/' \
     | grep -v 'docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md' \
     | grep -v '_test.go' \
     | head -20
```

Expected: empty output. (Spec docs under `docs/superpowers/` and the audit doc are explicitly allowed to reference issue IDs; test files may reference issues if they were grandfathered in.)

If anything matches, examine the location and remove the issue ID — per memory `feedback_no_issue_ids_in_code`, shipped artefacts must be self-contained.

---

## Task 20: Open the PR

- [ ] **Step 1: Push the branch**

```bash
git push -u origin refactor-250-processor-type-split
```

- [ ] **Step 2: Open the PR against `release/v0.8.0`**

Per memory `feedback_release_branch_for_milestones`, milestone work targets the release branch, not main.

```bash
gh pr create --base release/v0.8.0 \
  --title "refactor(workflow): #250 split processor execution-location from scheduled-transition timing" \
  --body "$(cat <<'EOF'
## Summary

Closes #250.

Splits `ProcessorDefinition.Type` from the conflated `[externalized, scheduled]` discriminator into an execution-location axis. The legacy `scheduled` value is removed from OpenAPI; `internalized` is reserved for #260 (rejected at engine dispatch as not yet implemented). Scheduled transitions get their proper home in #259.

Spec: \`docs/superpowers/specs/2026-05-20-issue-250-processor-type-split-design.md\` (revision 4, four review passes).

## Changes

- **OpenAPI** (\`api/openapi.yaml\`): \`ProcessorDefinitionDto.type\` becomes \`enum: [externalized]\` with explicit \`discriminator.mapping\`. \`ScheduledTransitionProcessorDefinitionDto\` and \`ScheduledTransitionConfigDto\` deleted. \`processors[]\` \`oneOf\` collapsed to one member.
- **Generated DTOs** (\`api/generated.go\`): regenerated.
- **Engine** (\`internal/domain/workflow/engine_processors.go\`): early-return Type-axis check at the top of \`executeProcessors\`'s per-processor loop. Rejects \`Type: "internalized"\` regardless of declared \`ExecutionMode\` — the early-return short-circuits the post-dispatch abort gate (which keys on \`ExecutionMode\` and would otherwise let \`Type: internalized + ExecutionMode: ASYNC_NEW_TX\` silently succeed).
- **Constants** (\`internal/domain/workflow/validate.go\`): added \`ProcessorTypeExternalized\` and \`ProcessorTypeInternalized\`. Mirrors the \`ExecutionMode*\` pattern.
- **SPI** (sibling repo PR — see cross-link below): docstring-only update on \`ProcessorDefinition.Type\`. No tag forced; rides v0.8.0's bundled SPI tag with #259 / #260.
- **Help text** (\`cmd/cyoda/help/content/workflows.md\`, \`grpc.md\`): stale \`EXTERNAL\` references corrected; new axis-summary describes both \`externalized\` and the reserved \`internalized\` honestly.
- **New doc** (\`docs/PROCESSOR_EXECUTION_MODES.md\`): two-axis reference for workflow authors. §0 axis-summary anchors \`type\` × \`executionMode\` distinction.
- **Parity scenario** (\`e2e/externalapi/scenarios/08-workflow-import-export.yaml\`): \`wf-import/03\` strips the scheduled processor entry.
- **Test-fixture sweep** (Gate 6): stale \`Type: "EXTERNAL"\`/\`"INTERNAL"\`/\`"external"\` strings normalised to \`workflow.ProcessorTypeExternalized\` across \`internal/domain/workflow/**_test.go\`.

## Tests added

- \`TestInternalizedRejection_ExecutionModeMatrix\` — four-case ExecutionMode matrix. The \`ASYNC_NEW_TX\` sub-case is the regression catcher for the silent-success path that early review iterations missed.
- \`TestInternalizedRejection_AuditEvents_ManualTransition\` / \`_CascadedTransition\` — full audit-event sequence (two \`SMEventStateProcessResult\` emits per fatal failure, no \`SMEventFinished\`).
- \`TestType_Externalized_FallsThroughToExecutionModeDispatch\` — empty + canonical externalized.
- \`TestType_UnknownValues_FallThrough\` — case-mismatch, whitespace, control-char, legacy \`"scheduled"\`, arbitrary unknown.
- \`TestType_EmptyProcessors_NoOp\` — pins early-return at \`engine_processors.go:43-45\`.
- \`TestInternalizedRejection_CascadePosition_SYNC\` — A:SYNC mutates entity.Data → B:internalized aborts → C:SYNC never dispatched.
- \`TestType_RoundTrip_PreservesUnknownValues\` — wire round-trip preserves unknown Type values verbatim.
- \`TestValidator_TypeInternalized_WithStartNewTxOnDispatch\` — validator vs engine ordering contract.
- \`TestE2E_InternalizedRejection_ManualTransitionReturns400\` — full HTTP stack via in-process \`httptest.Server\` + testcontainers Postgres.

## Backwards compatibility

The wire surface is **strictly tighter** in exactly one place — \`Type: "internalized"\` now fails at fire time where previously it silently default-dispatched. Everywhere else the wire is identical. v0.7.x → v0.8.0 upgrade requires no migration: stored workflows with any Type value continue to operate identically.

## Cross-repo

- SPI docstring PR: [link to be added after Task 15]

## Test plan

- [x] \`go vet ./...\` clean
- [x] \`go build ./...\` clean
- [x] \`go test -short ./...\` green
- [x] \`go test ./... \` green (Docker required)
- [x] \`make test-all\` green (root + plugin submodules)
- [x] \`go test -race ./...\` green
- [x] \`make todos\` no new entries
- [x] No issue-ID references in shipped artefacts

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Capture the PR URL for the project tracker.

---

## Self-Review Notes

**Spec coverage check:**
- §5.1 OpenAPI changes → Task 13.
- §5.2 generated.go regen → Task 14.
- §5.3 SPI docstring → Task 15.
- §5.4 engine dispatch + audit-event behaviour → Tasks 4, 5, 6.
- §5.5 validator + constants → Task 3 (constants); validator left unchanged per §5.5 — covered by Task 10's ordering test.
- §5.6 help text → Task 16.
- §5.7 parity scenario → Task 18.
- §5.8 docs hygiene + test-fixture sweep → Tasks 1, 2, 11, 17; vendored `docs/cyoda/*` declared out of scope.
- §6.1 test 1 → Task 4. test 2 + 2b → Task 7. test 3 → Task 7. test 4a + 4b → Task 6. test 5 → Task 8. test 6 → Task 9. test 7 → Task 10. test 8 → Task 12. test 9 → covered indirectly by full test suite + parity scenario 08. test 10 → Task 18.
- Appendix B verification → Task 19.

**Placeholder scan:** No "TBD" / "TODO" / "fill in later" entries. The one fallback path (Task 14 Step 6) is concrete with exact YAML.

**Type consistency:**
- `ProcessorTypeExternalized` / `ProcessorTypeInternalized` constants defined in Task 3, used in Tasks 4, 7, 8, 10, 11.
- `ProcessorDefinition` struct fields (`Type`, `Name`, `ExecutionMode`, `Config.StartNewTxOnDispatch`) match the SPI shape consistently.
- Audit event types (`SMEventProcessingPaused`, `SMEventStateProcessResult`, `SMEventFinished`) use the `spi.SMEvent*` constants consistently across Tasks 6, 7.
- File paths consistent (`internal/domain/workflow/engine_processors_type_test.go` referenced in Tasks 4, 7, 8, 9; `_audit_test.go` in Task 6; `handler_type_validation_test.go` in Task 10).

---

**Plan complete.**
