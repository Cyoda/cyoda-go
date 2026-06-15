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

// TestImport_Success_EmitsSlogInfo asserts every successful workflow
// import emits one slog.Info record capturing pkg, mode, entity model,
// modelVersion, workflowCount, and the per-workflow {name, desc} pairs.
// Workflow configuration is a high-impact mutable surface; the
// application-layer audit log lets operators correlate change intent
// without consulting the workflow JSON.
//
// This test also pins the WorkflowDefinition.Description disposition:
// the field is wired through the audit log as its operator-visible
// consumer, so a non-empty `desc` shows up in the per-workflow digest.
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
	// workflowCount names the size of THIS import call's incoming payload;
	// storedWorkflowCount names the model's post-merge total. On a fresh
	// model with two incoming workflows the two coincide; the
	// MergeWithExisting test exercises the divergence.
	if r["workflowCount"] != float64(2) {
		t.Errorf("workflowCount: expected 2 (incoming), got %v", r["workflowCount"])
	}
	if r["storedWorkflowCount"] != float64(2) {
		t.Errorf("storedWorkflowCount: expected 2 (post-merge), got %v", r["storedWorkflowCount"])
	}
	// workflowNames was a duplicate of workflows[].name; it must NOT appear.
	if _, present := r["workflowNames"]; present {
		t.Errorf("workflowNames: must not be emitted (duplicates workflows[].name)")
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

// TestImport_MergeWithExisting_LogsIncomingNotMerged is the divergence
// test: the audit log emits the THIS-CALL incoming payload as the
// `workflows` digest, not the post-merge final state. workflowCount
// reports the incoming size; storedWorkflowCount reports the post-merge
// total. Logging the merged state under "change intent" would distort
// what a single MERGE call actually did.
func TestImport_MergeWithExisting_LogsIncomingNotMerged(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Seed one workflow first; this one MUST NOT appear in the audit log
	// for the second import below.
	seed := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0", "name": "seed-existing", "desc": "preexisting",
				"initialState": "S1", "active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, seed)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed import expected 200, got %d", resp.StatusCode)
	}

	getRecords := captureSlog(t, slog.LevelInfo)

	// MERGE one new workflow; existing seed-existing stays in the store
	// but must not be conflated with the THIS-CALL audit subject.
	body := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0", "name": "incoming-new", "desc": "added this call",
				"initialState": "S1", "active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp = doWorkflowImport(t, srv.URL, "Order", 1, body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("merge import expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	imports := recordsByMsg(getRecords(), "workflow import applied")
	if len(imports) != 1 {
		t.Fatalf("expected exactly 1 'workflow import applied' INFO record, got %d", len(imports))
	}
	r := imports[0]
	if r["workflowCount"] != float64(1) {
		t.Errorf("workflowCount: expected 1 (incoming this call), got %v", r["workflowCount"])
	}
	if r["storedWorkflowCount"] != float64(2) {
		t.Errorf("storedWorkflowCount: expected 2 (seed + incoming), got %v", r["storedWorkflowCount"])
	}
	wfs, _ := r["workflows"].([]any)
	if len(wfs) != 1 {
		t.Fatalf("workflows: expected 1 entry (the incoming), got %d: %v", len(wfs), wfs)
	}
	entry, _ := wfs[0].(map[string]any)
	if entry["name"] != "incoming-new" {
		t.Errorf("workflows[0].name: expected 'incoming-new' (THIS CALL), got %v", entry["name"])
	}
	if entry["desc"] != "added this call" {
		t.Errorf("workflows[0].desc: expected 'added this call', got %v", entry["desc"])
	}
}

// TestImport_LongDescription_TruncatedInLog asserts the per-workflow desc
// is truncated to bound log volume. WorkflowDefinition.Description is
// operator-supplied free text with no length cap upstream; emitting it
// verbatim risks unbounded log lines from a multi-KB paste or
// deliberately-large value.
func TestImport_LongDescription_TruncatedInLog(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	getRecords := captureSlog(t, slog.LevelInfo)

	// 1 KB description — well past any sensible audit-line budget.
	longDesc := strings.Repeat("x", 1024)
	body := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0", "name": "long-desc-flow",
				"desc": "` + longDesc + `",
				"initialState": "S1", "active": true,
				"states": {"S1": {"transitions": []}}
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
		t.Fatalf("expected 1 INFO record, got %d", len(imports))
	}
	wfs, _ := imports[0]["workflows"].([]any)
	entry, _ := wfs[0].(map[string]any)
	desc, _ := entry["desc"].(string)
	if len(desc) >= 1024 {
		t.Errorf("desc: expected truncation below 1024 chars, got %d", len(desc))
	}
	if !strings.HasSuffix(desc, "...") {
		t.Errorf("desc: expected '...' truncation marker, got tail %q", desc[max(0, len(desc)-10):])
	}
}

// TestImport_ZeroResultAfterMerge_EmitsSlogWarn asserts the WARN canary:
// when an import call leaves the model with zero workflows, the engine
// will silently fall back to the embedded default on every subsequent
// execution. The handler logs the result at WARN so the
// "running-on-default" outcome is visible in operator logs.
//
// REPLACE/ACTIVATE with empty workflows is already rejected by the
// structural validator, so the only reachable path to zero-after-import
// is MERGE with an empty incoming on a model that has no prior workflows.
// The canary still defends against any future code path that lands there.
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

// cyclicWorkflowImportBody returns an import request body whose workflow
// declares an unguarded automated cycle S1 -> S2 -> S1. validateWorkflowLoops
// rejects this shape by default. The allowCycles flag is the operator
// opt-in.
func cyclicWorkflowImportBody(allowCycles bool) string {
	var allow string
	if allowCycles {
		allow = `"allowCycles": true,`
	}
	return `{
		"importMode": "REPLACE",
		` + allow + `
		"workflows": [{
			"version": "1.0",
			"name": "cyclic",
			"initialState": "S1",
			"active": true,
			"states": {
				"S1": {"transitions": [{"name": "to-S2", "next": "S2", "manual": false}]},
				"S2": {"transitions": [{"name": "to-S1", "next": "S1", "manual": false}]}
			}
		}]
	}`
}

// TestImport_AllowCyclesTrue_EmitsBypassWarn pins the security-relevant
// audit log line emitted when the request-level allowCycles=true bypass
// is exercised. Operators reviewing logs must be able to see which
// tenant/model/import-call invoked the bypass and which workflow names
// were admitted under it.
//
// Asserts:
//   - Exactly one WARN record with msg "workflow import: cycle validation
//     bypassed".
//   - pkg=workflow, tenant present, entityName/modelVersion/importMode
//     match the request, workflows lists the bypassed workflow names.
//
// This is the negative-space pair for TestImport_Success_EmitsSlogInfo:
// the success log says "what changed"; this log says "with what safety
// check bypassed".
func TestImport_AllowCyclesTrue_EmitsBypassWarn(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Polling", 1)

	getRecords := captureSlog(t, slog.LevelWarn)

	resp := doWorkflowImport(t, srv.URL, "Polling", 1, cyclicWorkflowImportBody(true))
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("import with allowCycles=true expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	warns := recordsByMsg(getRecords(), "workflow import: cycle validation bypassed")
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 'cycle validation bypassed' WARN record, got %d", len(warns))
	}
	r := warns[0]
	if r["level"] != "WARN" {
		t.Errorf("level: expected WARN, got %v", r["level"])
	}
	if r["pkg"] != "workflow" {
		t.Errorf("pkg: expected 'workflow', got %v", r["pkg"])
	}
	if _, present := r["tenant"]; !present {
		t.Errorf("tenant: field missing — bypass log must carry tenant context for multi-tenant audit")
	}
	if r["entityName"] != "Polling" {
		t.Errorf("entityName: expected 'Polling', got %v", r["entityName"])
	}
	if r["modelVersion"] != "1" {
		t.Errorf("modelVersion: expected '1' (string), got %v (%T)", r["modelVersion"], r["modelVersion"])
	}
	if r["importMode"] != "REPLACE" {
		t.Errorf("importMode: expected 'REPLACE', got %v", r["importMode"])
	}
	workflows, ok := r["workflows"].([]any)
	if !ok {
		t.Fatalf("workflows: expected []any, got %T", r["workflows"])
	}
	if len(workflows) != 1 || workflows[0] != "cyclic" {
		t.Errorf("workflows: expected ['cyclic'], got %v", workflows)
	}
}

// TestImport_AllowCyclesAbsent_DoesNotEmitBypassWarn pins the negative
// invariant: an import request without allowCycles=true MUST NOT emit
// the bypass WARN line. A regression that started emitting the bypass
// log on every import would silently elevate WARN volume and confuse
// operators about which imports actually used the safety bypass.
//
// The cyclic workflow is rejected at validation time (400), so the
// import doesn't succeed; the assertion is that no bypass-warn fired
// regardless of the import outcome.
func TestImport_AllowCyclesAbsent_DoesNotEmitBypassWarn(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "PollingRejected", 1)

	getRecords := captureSlog(t, slog.LevelWarn)

	resp := doWorkflowImport(t, srv.URL, "PollingRejected", 1, cyclicWorkflowImportBody(false))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("import without allowCycles expected 400, got %d", resp.StatusCode)
	}

	warns := recordsByMsg(getRecords(), "workflow import: cycle validation bypassed")
	if len(warns) != 0 {
		t.Errorf("bypass WARN must not fire when allowCycles is absent; got %d records", len(warns))
	}
}
