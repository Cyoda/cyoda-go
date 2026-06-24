# #256 Silent-Default Semantics — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace three silent-default behaviours in the workflow import / engine paths (H2 force-`Active=true`, H5 unobservable default-workflow substitution, M3 silently destructive empty-array import) with explicit and observable behaviour, per the decisions locked in issue #256's 2026-05-19 comment.

**Architecture:** Three independent commits on `feat/silent-defaults-256` (already cut from `release/v0.8.0`). H2 is a handler-side DTO change (`*bool` for `active`). H5 adds a small `e.logDefaultFallback` helper and updates four engine fallback sites. M3 inserts a pre-validation guard in the handler. No SPI changes.

**Tech Stack:** Go 1.26, `log/slog`, `net/http`, `httptest`, `testing`, in-memory `plugins/memory` store factory.

**Spec:** `docs/superpowers/specs/2026-06-14-256-silent-defaults-design.md`

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/domain/workflow/handler.go` | Modify | H2 DTO + conversion loop; M3 empty-array guard. |
| `internal/domain/workflow/engine.go` | Modify | H5 helper `logDefaultFallback` + 4 fallback call-site updates + tenant accessor. |
| `internal/domain/workflow/handler_test.go` | Modify | H2 + M3 HTTP-level tests. |
| `internal/domain/workflow/defaultwf_logging_test.go` | Create | H5 slog-capture tests. |
| `internal/domain/workflow/active_default_test.go` | Delete | Subsumed by H2 tests; obsoleted by the real handler defaulting. |
| `CHANGELOG.md` *(or equivalent release-notes file under v0.8.0)* | Modify | Three release-note entries. |

**Commit cadence:** one commit per task block (H2, H5, M3, release notes). Each is RED → GREEN per `.claude/rules/tdd.md`.

---

## Task 1: H2 — Honour explicit `active: false`

**Files:**
- Modify: `internal/domain/workflow/handler.go` (lines 26–30 + 100–103)
- Modify: `internal/domain/workflow/handler_test.go` (append tests)
- Delete: `internal/domain/workflow/active_default_test.go`

### Step 1.1: Write failing test — explicit `false` preserved

- [ ] Append this test to `internal/domain/workflow/handler_test.go` (still in `package workflow_test`):

```go
func TestImport_ExplicitActiveFalse_Preserved(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0",
				"name": "staged-flow",
				"initialState": "NEW",
				"active": false,
				"states": {"NEW": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("import expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].Active {
		t.Errorf("expected Active=false to be preserved, got Active=true")
	}
}
```

### Step 1.2: Run test — confirm RED

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run TestImport_ExplicitActiveFalse_Preserved -v
```

Expected: FAIL — assertion `expected Active=false to be preserved, got Active=true` fires (handler currently force-overrides `Active=true`).

### Step 1.3: Add the remaining H2 failing tests

- [ ] Append three more tests to `handler_test.go`:

```go
func TestImport_ActiveAbsent_DefaultsToTrue(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0",
				"name": "default-active-flow",
				"initialState": "NEW",
				"states": {"NEW": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("import expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if !wfs[0].Active {
		t.Errorf("expected absent active to default to true, got Active=false")
	}
}

func TestImport_ExplicitActiveTrue_Preserved(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0",
				"name": "explicit-true-flow",
				"initialState": "NEW",
				"active": true,
				"states": {"NEW": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("import expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if !wfs[0].Active {
		t.Errorf("expected Active=true to be preserved")
	}
}

func TestExportReimportRoundtrip_PreservesActiveFalse(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// First import: REPLACE with two workflows, one with active=false.
	seed := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0", "name": "alpha", "initialState": "S",
				"active": true,
				"states": {"S": {"transitions": []}}
			},
			{
				"version": "1.0", "name": "beta", "initialState": "S",
				"active": false,
				"states": {"S": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, seed)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("seed import expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Export.
	exportResp := doWorkflowExport(t, srv.URL, "Order", 1)
	if exportResp.StatusCode != http.StatusOK {
		t.Fatalf("export expected 200, got %d", exportResp.StatusCode)
	}
	exportedBody := readJSON(t, exportResp)
	// Build a re-import body from the export shape.
	reimportRaw, err := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows":  exportedBody["workflows"],
	})
	if err != nil {
		t.Fatalf("failed to marshal re-import body: %v", err)
	}

	// Re-import.
	resp2 := doWorkflowImport(t, srv.URL, "Order", 1, string(reimportRaw))
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("re-import expected 200, got %d: %s", resp2.StatusCode, b)
	}
	resp2.Body.Close()

	// Re-export and verify beta is still Active=false.
	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(wfs))
	}
	var beta *spi.WorkflowDefinition
	for i := range wfs {
		if wfs[i].Name == "beta" {
			beta = &wfs[i]
			break
		}
	}
	if beta == nil {
		t.Fatalf("workflow 'beta' missing from re-export")
	}
	if beta.Active {
		t.Errorf("expected beta to remain Active=false after export+REPLACE re-import round-trip")
	}
}
```

### Step 1.4: Run all four H2 tests — confirm RED

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run 'TestImport_ExplicitActiveFalse_Preserved|TestImport_ActiveAbsent_DefaultsToTrue|TestImport_ExplicitActiveTrue_Preserved|TestExportReimportRoundtrip_PreservesActiveFalse' -v
```

Expected: `TestImport_ExplicitActiveFalse_Preserved`, `TestExportReimportRoundtrip_PreservesActiveFalse` FAIL. The other two pass against current behaviour (handler already coerces absent and `true` to `true`) — that's fine; they're regression pins.

### Step 1.5: Implement the H2 handler change

- [ ] Edit `internal/domain/workflow/handler.go`.

Replace the existing `importRequest` definition (lines 26–30):

```go
// importRequest is the JSON body shape for workflow import.
type importRequest struct {
	ImportMode string                   `json:"importMode"`
	Workflows  []spi.WorkflowDefinition `json:"workflows"`
}
```

with:

```go
// workflowImportDef mirrors spi.WorkflowDefinition but uses *bool for Active
// so the handler can distinguish "absent" (default to true, preserves OOTB
// contract) from explicit "false" (operator wants the workflow staged
// inactive). The SPI type stays plain bool — this distinction is purely a
// request-shape concern. See #256 H2.
type workflowImportDef struct {
	Version      string                         `json:"version"`
	Name         string                         `json:"name"`
	Description  string                         `json:"desc,omitempty"`
	InitialState string                         `json:"initialState"`
	Active       *bool                          `json:"active"`
	Criterion    json.RawMessage                `json:"criterion,omitempty"`
	States       map[string]spi.StateDefinition `json:"states"`
}

// importRequest is the JSON body shape for workflow import.
type importRequest struct {
	ImportMode string              `json:"importMode"`
	Workflows  []workflowImportDef `json:"workflows"`
}
```

Replace the `// Default imported workflows to active (Cyoda Cloud behavior).` block (lines 100–103):

```go
	// Default imported workflows to active (Cyoda Cloud behavior).
	for i := range req.Workflows {
		req.Workflows[i].Active = true
	}
```

with:

```go
	// H2 (#256): default Active to true only when the field is absent;
	// explicit true/false pass through. This restores export → REPLACE
	// re-import idempotency and lets operators stage inactive workflows.
	incoming := make([]spi.WorkflowDefinition, len(req.Workflows))
	for i, w := range req.Workflows {
		active := true
		if w.Active != nil {
			active = *w.Active
		}
		incoming[i] = spi.WorkflowDefinition{
			Version:      w.Version,
			Name:         w.Name,
			Description:  w.Description,
			InitialState: w.InitialState,
			Active:       active,
			Criterion:    w.Criterion,
			States:       w.States,
		}
	}
```

Then replace the subsequent reference `req.Workflows` (used by `validateImportRequest` and `applyImportMode`) — change `validateImportRequest(req.Workflows)` to `validateImportRequest(incoming)` and `applyImportMode(existing, req.Workflows, mode)` to `applyImportMode(existing, incoming, mode)`.

### Step 1.6: Run the four H2 tests — confirm GREEN

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run 'TestImport_ExplicitActiveFalse_Preserved|TestImport_ActiveAbsent_DefaultsToTrue|TestImport_ExplicitActiveTrue_Preserved|TestExportReimportRoundtrip_PreservesActiveFalse' -v
```

Expected: all four PASS.

### Step 1.7: Delete the obsolete inline-faking test

- [ ] Delete `internal/domain/workflow/active_default_test.go`. Its contract ("workflow without `active` field defaults to active") is now exercised through the real HTTP stack by `TestImport_ActiveAbsent_DefaultsToTrue`. The file is in the internal package and only faked the handler's defaulting inline (`workflows[i].Active = true` inside the test) — leaving it would be misleading.

```bash
rm internal/domain/workflow/active_default_test.go
```

### Step 1.8: Run the full workflow package — confirm no regressions

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -v
```

Expected: all tests pass.

### Step 1.9: Commit

- [ ] Commit:

```bash
git add internal/domain/workflow/handler.go \
        internal/domain/workflow/handler_test.go \
        internal/domain/workflow/active_default_test.go
git commit -m "feat(workflow): honour explicit active=false at import (H2 of #256)

Replace the unconditional req.Workflows[i].Active = true with a
handler-local workflowImportDef carrying Active *bool. Default nil → true
when the field is absent; pass explicit true/false through unchanged.

The SPI WorkflowDefinition.Active stays plain bool — the presence/zero
distinction is purely a request-shape concern, kept inside handler.go to
avoid cascading the change into four storage-plugin schemas.

Restores export → REPLACE re-import idempotency, and lets operators stage
inactive workflows for blue/green rollout.

Refs #256

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: H5 — Elevate default-workflow fallback to `slog.Warn`

**Files:**
- Modify: `internal/domain/workflow/engine.go` (4 fallback sites + new helper + tenant accessor)
- Create: `internal/domain/workflow/defaultwf_logging_test.go`

### Step 2.1: Write failing test — slog.Warn on no-workflows-imported

- [ ] Create `internal/domain/workflow/defaultwf_logging_test.go`:

```go
package workflow

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// captureSlogWarn swaps slog.Default with a JSON-handler writing to a buffer
// at WARN level for the duration of the test. The returned function returns
// the parsed records (one per logged line, decoded into a map).
func captureSlogWarn(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("failed to parse log line: %v\nline: %s", err, line)
			}
			out = append(out, rec)
		}
		return out
	}
}

func TestDefaultFallback_NoWorkflowsImported_EmitsSlogWarn(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ctx = common.WithDiagnostics(ctx)
	getRecords := captureSlogWarn(t)

	modelRef := spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}
	entity := makeEntity("widget-1", modelRef, map[string]any{"k": "v"})

	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	records := getRecords()
	// Filter to "default workflow substituted" records — other slog WARN
	// lines in the engine (e.g. transition-aborted) should not affect this.
	var fallbackRecords []map[string]any
	for _, r := range records {
		if r["msg"] == "default workflow substituted" {
			fallbackRecords = append(fallbackRecords, r)
		}
	}
	if len(fallbackRecords) != 1 {
		t.Fatalf("expected exactly 1 default-fallback WARN record, got %d (all records: %v)", len(fallbackRecords), records)
	}
	r := fallbackRecords[0]
	if r["pkg"] != "workflow" {
		t.Errorf("pkg: expected 'workflow', got %v", r["pkg"])
	}
	if r["tenant"] != string(testTenant) {
		t.Errorf("tenant: expected %q, got %v", string(testTenant), r["tenant"])
	}
	if r["entityName"] != "Widget" {
		t.Errorf("entityName: expected 'Widget', got %v", r["entityName"])
	}
	if r["modelVersion"] != "1" {
		t.Errorf("modelVersion: expected '1', got %v", r["modelVersion"])
	}
	if r["entityId"] != "widget-1" {
		t.Errorf("entityId: expected 'widget-1', got %v", r["entityId"])
	}
	if r["reason"] != "no_workflows_imported" {
		t.Errorf("reason: expected 'no_workflows_imported', got %v", r["reason"])
	}
}
```

### Step 2.2: Run new test — confirm RED

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run TestDefaultFallback_NoWorkflowsImported_EmitsSlogWarn -v
```

Expected: FAIL — `expected exactly 1 default-fallback WARN record, got 0` (engine doesn't emit slog yet).

### Step 2.3: Add the no-criterion-matched test

- [ ] Append to `defaultwf_logging_test.go`:

```go
func TestDefaultFallback_NoCriterionMatched_EmitsSlogWarn(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ctx = common.WithDiagnostics(ctx)
	getRecords := captureSlogWarn(t)

	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{
		{
			Version:      "1",
			Name:         "guarded-only",
			InitialState: "S",
			Active:       true,
			Criterion:    simpleCriterion("$.never", "EQUALS", "match-me"),
			States:       map[string]spi.StateDefinition{"S": {Transitions: []spi.TransitionDefinition{}}},
		},
	})

	entity := makeEntity("ord-1", modelRef, map[string]any{"k": "v"})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var fallbackRecords []map[string]any
	for _, r := range getRecords() {
		if r["msg"] == "default workflow substituted" {
			fallbackRecords = append(fallbackRecords, r)
		}
	}
	if len(fallbackRecords) != 1 {
		t.Fatalf("expected exactly 1 default-fallback WARN record, got %d", len(fallbackRecords))
	}
	if fallbackRecords[0]["reason"] != "no_criterion_matched" {
		t.Errorf("reason: expected 'no_criterion_matched', got %v", fallbackRecords[0]["reason"])
	}
}

func TestDefaultFallback_PreservesBodyWarning(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ctx = common.WithDiagnostics(ctx)

	modelRef := spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}
	entity := makeEntity("widget-warn-1", modelRef, map[string]any{"k": "v"})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	warns := common.GetDiagnostics(ctx).GetWarnings()
	found := false
	for _, w := range warns {
		if strings.Contains(w, "default workflow") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a body warning mentioning 'default workflow', got %v", warns)
	}
}
```

NOTE on `common.GetDiagnostics`: verify the exact accessor name by reading `internal/common/diagnostics.go` (we saw `WithDiagnostics` and `GetWarnings()` on `*RequestDiagnostics`). If the package accessor is `common.DiagnosticsFromContext` rather than `common.GetDiagnostics`, adjust this call. The test's intent is unambiguous: pull the per-request warning slice out of the context and assert one of them mentions "default workflow".

### Step 2.4: Run all three H5 tests — confirm RED

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run TestDefaultFallback -v
```

Expected: `_EmitsSlogWarn` cases FAIL; `_PreservesBodyWarning` may PASS already (current code already adds a body warning). RED count: 2 fails out of 3.

### Step 2.5: Add the tenant accessor and the logging helper

- [ ] Edit `internal/domain/workflow/engine.go`. Add imports `log/slog` to the existing import block (keep alphabetical order — `log/slog` goes between `fmt` and `time`).

- [ ] At the bottom of `engine.go`, add:

```go
// tenantFromContext mirrors the helper in internal/domain/search/service.go.
// Returns the bound tenant ID, or empty when no UserContext is on the
// context (unauthenticated path). Kept local to this package; if a third
// caller appears, extract to internal/common per rule-of-three.
func tenantFromContext(ctx context.Context) string {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return ""
	}
	return string(uc.Tenant.ID)
}

// logDefaultFallback emits a single slog.Warn line whenever the engine
// substitutes the embedded default workflow. Reason discriminates the four
// call sites:
//   - "no_workflows_imported": cold-path (Execute/ManualTransition/Loopback)
//     with no stored workflows for the model.
//   - "no_criterion_matched":  workflows exist but no criterion matched the
//     entity (selectWorkflow tail).
//
// The body-level warning via common.AddWarning is retained at each call
// site for client-facing surfacing; this log line is purely additive for
// operational observability (#256 H5).
func (e *Engine) logDefaultFallback(ctx context.Context, entity *spi.Entity, reason string) {
	slog.WarnContext(ctx, "default workflow substituted",
		slog.String("pkg", "workflow"),
		slog.String("tenant", tenantFromContext(ctx)),
		slog.String("entityName", entity.Meta.ModelRef.EntityName),
		slog.String("modelVersion", entity.Meta.ModelRef.ModelVersion),
		slog.String("entityId", entity.Meta.ID),
		slog.String("reason", reason))
}
```

### Step 2.6: Update the four fallback call sites

- [ ] In `engine.go`, replace each of these three blocks (all identical text today):

```go
	// No workflows defined → use default workflow.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		workflows = e.defaultWorkflows
	}
```

with (cold-path message corrected):

```go
	// No workflows defined → use embedded default. Body warning surfaces to
	// the client; slog.Warn surfaces to operators (#256 H5).
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no workflows imported for model — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_workflows_imported")
		workflows = e.defaultWorkflows
	}
```

This applies to `Execute` (around line 127), `ManualTransition` (around 229), `Loopback` (around 311). Each function already has `entity` in scope at that point.

- [ ] In `selectWorkflow`, replace the tail block:

```go
	// No imported workflow matched — fall back to the default workflow.
	if len(e.defaultWorkflows) > 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		defaultWF := &e.defaultWorkflows[0]
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowFound, fmt.Sprintf("No imported workflow matched; using default workflow %q", defaultWF.Name), nil)
		return defaultWF, nil
	}
```

with:

```go
	// No imported workflow matched the entity — fall back to the embedded
	// default. Both channels (body warning + slog.Warn) fire (#256 H5).
	if len(e.defaultWorkflows) > 0 {
		common.AddWarning(ctx, "no imported workflow matched entity — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_criterion_matched")
		defaultWF := &e.defaultWorkflows[0]
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowFound, fmt.Sprintf("No imported workflow matched; using default workflow %q", defaultWF.Name), nil)
		return defaultWF, nil
	}
```

### Step 2.7: Run H5 tests — confirm GREEN

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run TestDefaultFallback -v
```

Expected: all three PASS.

### Step 2.8: Run the full workflow package — confirm no regressions

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -v
```

Expected: all tests pass. The pre-existing `defaultwf_fallback_test.go` still passes (it asserts state outcome, not body-warning text).

### Step 2.9: Commit

- [ ] Commit:

```bash
git add internal/domain/workflow/engine.go \
        internal/domain/workflow/defaultwf_logging_test.go
git commit -m "feat(workflow): slog.Warn on default-workflow substitution (H5 of #256)

When the engine falls back to the embedded default workflow — either at
the cold path (no workflows imported for the model) or at the
no-criterion-matched tail of selectWorkflow — emit a structured slog.Warn
line in addition to the existing common.AddWarning body-level warning.

Log fields: pkg, tenant, entityName, modelVersion, entityId, reason.
Reason discriminates 'no_workflows_imported' from 'no_criterion_matched'.

Body warning text is corrected per site: 'no workflows imported for
model' for the three cold paths; 'no imported workflow matched entity'
for the selectWorkflow tail. The previous shared text was inaccurate for
the cold paths.

Refs #256

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: M3 — Reject `workflows: []` in REPLACE / ACTIVATE

**Files:**
- Modify: `internal/domain/workflow/handler.go` (insert M3 guard)
- Modify: `internal/domain/workflow/handler_test.go` (append five tests)

### Step 3.1: Write failing tests for the rejection paths

- [ ] Append to `internal/domain/workflow/handler_test.go`:

```go
func TestImport_EmptyArrayReplace_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{"importMode":"REPLACE","workflows":[]}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
	body2, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body2), common.ErrCodeValidationFailed) {
		t.Errorf("expected error code %s in body, got: %s", common.ErrCodeValidationFailed, body2)
	}
	if !strings.Contains(string(body2), "empty workflows array not allowed") {
		t.Errorf("expected detail mentioning 'empty workflows array not allowed', got: %s", body2)
	}
}

func TestImport_EmptyArrayActivate_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{"importMode":"ACTIVATE","workflows":[]}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
}

func TestImport_MissingWorkflowsKeyReplace_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// `workflows` key entirely absent → JSON unmarshal yields nil slice
	// → len() == 0 → must reject equivalently to explicit [].
	body := `{"importMode":"REPLACE"}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
}

func TestImport_EmptyArrayMerge_NoOp(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Seed: import a workflow first.
	seed := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0", "name": "seed-wf", "initialState": "S",
				"active": true,
				"states": {"S": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, seed)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed import expected 200, got %d", resp.StatusCode)
	}

	// MERGE empty → 200, no change.
	resp2 := doWorkflowImport(t, srv.URL, "Order", 1, `{"importMode":"MERGE","workflows":[]}`)
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("MERGE empty expected 200, got %d: %s", resp2.StatusCode, b)
	}
	resp2.Body.Close()

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 1 || wfs[0].Name != "seed-wf" {
		t.Errorf("expected seed-wf preserved, got %v", wfs)
	}
}

func TestImport_EmptyArrayDefaultMode_NoOp(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// No importMode → defaults to MERGE; empty array → no-op (no existing).
	resp := doWorkflowImport(t, srv.URL, "Order", 1, `{"workflows":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("default-mode empty expected 200, got %d: %s", resp.StatusCode, b)
	}
}
```

### Step 3.2: Run tests — confirm RED

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run 'TestImport_EmptyArray|TestImport_MissingWorkflowsKey' -v
```

Expected: the three "Rejected" tests FAIL (current handler accepts empty arrays); the two "NoOp" tests PASS (current behaviour already no-op-equivalent for MERGE).

### Step 3.3: Implement the M3 guard

- [ ] Edit `internal/domain/workflow/handler.go`. After the mode-parsing block (which ends around line 55, after the `unknown importMode` check) AND after the model-existence check + workflow-store acquisition, but BEFORE the H2 conversion loop, insert:

```go
	// M3 (#256): REPLACE / ACTIVATE with an empty workflows array would
	// silently wipe or deactivate all stored workflows for the model;
	// engine fallback to the embedded default then masks the destruction
	// behind HTTP 200. Reject explicitly. MERGE-empty stays a no-op
	// (covered by the carve-out below).
	if len(req.Workflows) == 0 && (mode == "REPLACE" || mode == "ACTIVATE") {
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			common.ErrCodeValidationFailed,
			"empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op"))
		return
	}
```

Placement: the guard goes between "existing load" (line ~98) and the H2 conversion loop introduced in Task 1. The existing load is cheap (memory store) but produces no side effects, so ordering does not matter functionally; we keep the model-existence + store-acquisition checks first because they produce more specific 4xx codes (404 + MODEL_NOT_FOUND vs 400 VALIDATION_FAILED).

### Step 3.4: Run M3 tests — confirm GREEN

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -run 'TestImport_EmptyArray|TestImport_MissingWorkflowsKey' -v
```

Expected: all five PASS.

### Step 3.5: Run the full workflow package — confirm no regressions

- [ ] Run:

```bash
go test ./internal/domain/workflow/... -v
```

Expected: all tests pass.

### Step 3.6: Commit

- [ ] Commit:

```bash
git add internal/domain/workflow/handler.go \
        internal/domain/workflow/handler_test.go
git commit -m "feat(workflow): reject empty workflows[] in REPLACE/ACTIVATE (M3 of #256)

REPLACE with workflows:[] silently wiped all stored workflows for the
model; ACTIVATE with [] silently flipped every existing workflow to
Active=false. In both cases the engine then fell back to the embedded
default at runtime, masking the destruction behind HTTP 200.

Reject both with 400 VALIDATION_FAILED. MERGE-empty stays a no-op (no
legitimate way to silently destroy via MERGE). The 'workflows' key being
absent entirely is equivalent to an empty array under JSON unmarshal
semantics and is rejected the same way.

Refs #256

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Release notes & documentation hygiene

**Files:**
- Modify: `CHANGELOG.md` (or whichever release-notes file the project uses; verify before editing)
- Modify (only if affected): any `cmd/cyoda/help/content/` topic mentioning workflow import / `active` field semantics

### Step 4.1: Locate the v0.8.0 release-notes file

- [ ] Identify the right file:

```bash
ls CHANGELOG.md docs/release-notes/ 2>/dev/null
grep -rn "v0.8.0" CHANGELOG.md docs/ 2>/dev/null | head
```

The project may keep release notes in `CHANGELOG.md`, in `docs/release-notes/v0.8.0.md`, or as a bullet list inside `README.md`. Pick whichever the most recent release (v0.7.x) updated.

### Step 4.2: Add the three v0.8.0 entries

- [ ] Append to the v0.8.0 section (create the section if absent):

```markdown
- **workflow/import: explicit `"active": false` is now honoured.** The import
  handler previously force-overrode every incoming workflow's `active` field
  to `true`. Operators can now stage inactive workflows via REPLACE without
  silent re-activation. Export → re-import via REPLACE is now an idempotent
  round-trip. (#256)
- **workflow/engine: default-workflow substitution is now logged at WARN.**
  When the engine falls back to the embedded default workflow — either
  because no workflow was imported for the model, or because no imported
  workflow's criterion matched the entity — a structured `slog.Warn` line
  is emitted in addition to the existing body warning. The body warning
  text now distinguishes the two causes. (#256)
- **workflow/import: empty `workflows: []` in REPLACE / ACTIVATE is now
  rejected.** Previously these modes silently wiped or deactivated all
  stored workflows for the model, falling back to the embedded default at
  runtime with no error. They now return HTTP 400 with `VALIDATION_FAILED`.
  MERGE with empty stays a no-op. (#256)
```

### Step 4.3: Check help topics

- [ ] Grep for any help topic mentioning workflow import or the `active` field:

```bash
grep -rln "workflow.*import\|\"active\":" cmd/cyoda/help/content/ 2>/dev/null
```

If any matches reference the old force-override behaviour or empty-array acceptance, update them to reflect the new semantics. If no matches, skip — the help subsystem doesn't surface these details today.

### Step 4.4: Commit

- [ ] Commit:

```bash
git add CHANGELOG.md cmd/cyoda/help/content/ 2>/dev/null
git commit -m "docs: release notes for #256 (silent-default semantics)

H2, H5, M3 — three behaviour changes against v0.8.0.

Refs #256

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

If no help-topic files needed updating, drop the `cmd/cyoda/help/content/` add — just commit `CHANGELOG.md` (or the equivalent release-notes file).

---

## Task 5: Pre-PR verification (Gate 5)

### Step 5.1: Static analysis

- [ ] Run:

```bash
go vet ./...
```

Expected: clean.

### Step 5.2: Short test sweep (root module)

- [ ] Run:

```bash
go test -short ./... -v 2>&1 | tail -40
```

Expected: all green.

### Step 5.3: Full E2E sweep

- [ ] Ensure Docker is running, then:

```bash
go test ./internal/e2e/... -v 2>&1 | tail -60
```

Expected: all green. E2E exercises workflow import via the HTTP stack, so H2 and M3 would surface here if anything regresses.

### Step 5.4: Plugin submodules

- [ ] Run:

```bash
make test-short-all
```

Expected: all green. (No SPI changes in this work, so plugin behaviour is unaffected; this is the rule-of-six-eyes sanity check.)

### Step 5.5: Race detector (one-shot, end-of-deliverable)

- [ ] Per `.claude/rules/race-testing.md`, run once before PR creation:

```bash
go test -race ./... 2>&1 | tail -20
```

Expected: clean. No new concurrent paths were introduced; this catches accidental aliasing from the new `incoming` slice in the handler if anywhere yielded control mid-mutation.

### Step 5.6: PR

- [ ] Use `superpowers:requesting-code-review` to prepare and open the PR against `release/v0.8.0`. PR body must reference `Closes #256` (the milestone-invariant memory applies: every closed-by-release-PR issue is the release-notes source of truth and must be milestoned).

---

## Self-review checklist (writer-side)

- [x] H2 (spec §H2) → Task 1 covers DTO swap, four failing tests, deletion of obsolete fake test, commit.
- [x] H5 (spec §H5) → Task 2 covers helper, four call sites (3 cold-path + 1 no-criterion-match), three failing tests, body-warning preservation, commit.
- [x] M3 (spec §M3) → Task 3 covers guard insertion point, five failing tests (3 rejection + 2 no-op), commit.
- [x] Release notes → Task 4.
- [x] Verification gates → Task 5.
- [x] No "TBD"/"implement later"/"appropriate handling" placeholders.
- [x] All code snippets are complete and copy-pasteable.
- [x] All exact file paths given.
- [x] Type consistency: `workflowImportDef` (Task 1) is referenced consistently throughout; `e.logDefaultFallback` signature consistent across Task 2.
- [x] Spec consistency: no diverged decisions from the design doc.
