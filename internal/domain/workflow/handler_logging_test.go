package workflow_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// captureSlog swaps slog.Default with a JSON handler writing to a buffer at
// the given level for the duration of the test. The returned function parses
// every line into a map. Mirrors the helper in defaultwf_logging_test.go but
// lives in the external test package so handler-level wire tests can use it.
func captureSlog(t *testing.T, level slog.Level) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
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

// recordsByMsg filters slog records by the "msg" field.
func recordsByMsg(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// TestImport_Success_EmitsSlogInfo covers issue #257 M7: every successful
// workflow import must emit one slog.Info record capturing pkg, mode, entity
// model, modelVersion, workflowCount, and the per-workflow {name, desc}
// pairs. Workflow configuration is a high-impact mutable surface and the
// application layer was silent on the content of changes until now.
//
// This test also pins the "wire Description" disposition (issue #257 option
// (a) — wire it): WorkflowDefinition.Description ships through the audit
// log so operators can correlate human-readable change intent.
func TestImport_Success_EmitsSlogInfo(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	getRecords := captureSlog(t, slog.LevelInfo)

	body := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "order-flow",
				"desc": "primary order lifecycle",
				"initialState": "NEW",
				"active": true,
				"states": {"NEW": {"transitions": []}}
			},
			{
				"version": "1.0",
				"name": "audit-flow",
				"desc": "audit-only mirror of order-flow",
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

	imports := recordsByMsg(getRecords(), "workflow import applied")
	if len(imports) != 1 {
		t.Fatalf("expected exactly 1 'workflow import applied' INFO record, got %d", len(imports))
	}
	r := imports[0]
	if r["level"] != "INFO" {
		t.Errorf("level: expected INFO, got %v", r["level"])
	}
	if r["pkg"] != "workflow" {
		t.Errorf("pkg: expected 'workflow', got %v", r["pkg"])
	}
	if r["importMode"] != "MERGE" {
		t.Errorf("importMode: expected 'MERGE', got %v", r["importMode"])
	}
	if r["entityName"] != "Order" {
		t.Errorf("entityName: expected 'Order', got %v", r["entityName"])
	}
	if r["modelVersion"] != "1" {
		t.Errorf("modelVersion: expected '1' (string), got %v (%T)", r["modelVersion"], r["modelVersion"])
	}
	// JSON unmarshal turns numbers into float64.
	if r["workflowCount"] != float64(2) {
		t.Errorf("workflowCount: expected 2, got %v", r["workflowCount"])
	}

	// workflows must list the {name, desc} pairs — this is the Description-wiring
	// half of the M7 + Description-disposition decision.
	wfs, ok := r["workflows"].([]any)
	if !ok {
		t.Fatalf("workflows: expected []any, got %T", r["workflows"])
	}
	if len(wfs) != 2 {
		t.Fatalf("workflows: expected 2 entries, got %d", len(wfs))
	}
	expected := map[string]string{
		"order-flow": "primary order lifecycle",
		"audit-flow": "audit-only mirror of order-flow",
	}
	for _, raw := range wfs {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("workflows entry: expected map[string]any, got %T", raw)
		}
		name, _ := entry["name"].(string)
		desc, _ := entry["desc"].(string)
		want, found := expected[name]
		if !found {
			t.Errorf("workflows entry: unexpected name %q", name)
			continue
		}
		if desc != want {
			t.Errorf("workflows entry %q: expected desc %q, got %q", name, want, desc)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		t.Errorf("workflows: missing expected entries: %v", expected)
	}
}

// TestImport_ZeroResultAfterMerge_EmitsSlogWarn covers the WARN half of #257
// M7: when an import call leaves the model with zero workflows, the engine
// will silently fall back to the embedded default on every subsequent
// execution. The handler must log the result at WARN so the
// "running-on-default" outcome is visible in operator logs.
//
// REPLACE/ACTIVATE with empty workflows is already rejected (issue #256
// M3), so the only reachable path to zero-after-import is MERGE with an
// empty incoming on a model that has no prior workflows. The destruction
// scenario the original audit framed against REPLACE-with-empty is now
// covered by this single canary.
func TestImport_ZeroResultAfterMerge_EmitsSlogWarn(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	getRecords := captureSlog(t, slog.LevelWarn)

	// MERGE-empty on a fresh model with no prior workflows → result is empty.
	resp := doWorkflowImport(t, srv.URL, "Order", 1, `{"importMode":"MERGE","workflows":[]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("MERGE-empty expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	warns := recordsByMsg(getRecords(), "workflow import resulted in zero workflows")
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 'zero workflows' WARN record, got %d", len(warns))
	}
	r := warns[0]
	if r["level"] != "WARN" {
		t.Errorf("level: expected WARN, got %v", r["level"])
	}
	if r["pkg"] != "workflow" {
		t.Errorf("pkg: expected 'workflow', got %v", r["pkg"])
	}
	if r["entityName"] != "Order" {
		t.Errorf("entityName: expected 'Order', got %v", r["entityName"])
	}
	if r["modelVersion"] != "1" {
		t.Errorf("modelVersion: expected '1', got %v", r["modelVersion"])
	}
	if r["importMode"] != "MERGE" {
		t.Errorf("importMode: expected 'MERGE', got %v", r["importMode"])
	}
}
