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

func TestDefaultFallback_ManualTransition_NoWorkflowsImported_EmitsSlogWarn(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ctx = common.WithDiagnostics(ctx)
	getRecords := captureSlogWarn(t)

	// ManualTransition reads the entity's current state, so we have to give
	// the entity a state that lives in the embedded default workflow. The
	// default workflow's initial state is "NONE" — pin to "CREATED" so the
	// fallback path finds a matching workflow and the test exercises only
	// the cold-path log line, not engine error paths beyond it.
	modelRef := spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}
	entity := makeEntity("widget-manual-1", modelRef, map[string]any{"k": "v"})
	entity.Meta.State = "CREATED"

	// "UPDATE" is a manual transition in the default workflow's CREATED state.
	// We don't care whether it succeeds; the fallback log fires before
	// transition resolution.
	_, _ = engine.ManualTransition(ctx, entity, "UPDATE")

	var fallbackRecords []map[string]any
	for _, r := range getRecords() {
		if r["msg"] == "default workflow substituted" {
			fallbackRecords = append(fallbackRecords, r)
		}
	}
	if len(fallbackRecords) != 1 {
		t.Fatalf("expected exactly 1 default-fallback WARN record, got %d", len(fallbackRecords))
	}
	r := fallbackRecords[0]
	if r["reason"] != "no_workflows_imported" {
		t.Errorf("reason: expected 'no_workflows_imported', got %v", r["reason"])
	}
	if r["entityId"] != "widget-manual-1" {
		t.Errorf("entityId: expected 'widget-manual-1', got %v", r["entityId"])
	}
}

func TestDefaultFallback_Loopback_NoWorkflowsImported_EmitsSlogWarn(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ctx = common.WithDiagnostics(ctx)
	getRecords := captureSlogWarn(t)

	modelRef := spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}
	entity := makeEntity("widget-loop-1", modelRef, map[string]any{"k": "v"})
	entity.Meta.State = "CREATED"

	_, _ = engine.Loopback(ctx, entity)

	var fallbackRecords []map[string]any
	for _, r := range getRecords() {
		if r["msg"] == "default workflow substituted" {
			fallbackRecords = append(fallbackRecords, r)
		}
	}
	if len(fallbackRecords) != 1 {
		t.Fatalf("expected exactly 1 default-fallback WARN record, got %d", len(fallbackRecords))
	}
	r := fallbackRecords[0]
	if r["reason"] != "no_workflows_imported" {
		t.Errorf("reason: expected 'no_workflows_imported', got %v", r["reason"])
	}
	if r["entityId"] != "widget-loop-1" {
		t.Errorf("entityId: expected 'widget-loop-1', got %v", r["entityId"])
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
