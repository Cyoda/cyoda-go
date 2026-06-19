package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestWorkflowSchemaVersion_ImportAccepts10 — happy path: a "1.0"
// workflow imports successfully.
func TestWorkflowSchemaVersion_ImportAccepts10(t *testing.T) {
	const entity = "wf-schema-accept"
	importModelE2E(t, entity, 1)
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0",
			"name": "wf-1",
			"initialState": "S1",
			"active": true,
			"states": {"S1": {}}
		}]
	}`
	status, respBody := importWorkflowE2E(t, entity, 1, body)
	if status != http.StatusOK {
		t.Fatalf("import status = %d; want 200; body: %s", status, respBody)
	}
}

// TestWorkflowSchemaVersion_ImportRejectsMajorUnsupported — a "2.0"
// workflow is rejected with WORKFLOW_SCHEMA_VERSION_UNSUPPORTED.
func TestWorkflowSchemaVersion_ImportRejectsMajorUnsupported(t *testing.T) {
	const entity = "wf-schema-reject"
	importModelE2E(t, entity, 1)
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "2.0",
			"name": "wf-bad",
			"initialState": "S1",
			"active": true,
			"states": {"S1": {}}
		}]
	}`
	status, respBody := importWorkflowE2E(t, entity, 1, body)
	if status != http.StatusBadRequest {
		t.Fatalf("import status = %d; want 400; body: %s", status, respBody)
	}
	// The server uses RFC 9457 Problem Details: errorCode is in
	// properties.errorCode and the human-readable detail is in "detail".
	var errBody struct {
		Detail     string `json:"detail"`
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &errBody); err != nil {
		t.Fatalf("decode error body: %v; raw: %s", err, respBody)
	}
	if errBody.Properties.ErrorCode != "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED" {
		t.Fatalf("errorCode = %q; want WORKFLOW_SCHEMA_VERSION_UNSUPPORTED; body: %s", errBody.Properties.ErrorCode, respBody)
	}
	if !strings.Contains(errBody.Detail, "wf-bad") {
		t.Fatalf("detail %q does not name offending workflow", errBody.Detail)
	}
}

// TestWorkflowSchemaVersion_ImportRejectsMalformed — a "1.0.0"
// workflow is rejected with a message pointing at MAJOR.MINOR.
func TestWorkflowSchemaVersion_ImportRejectsMalformed(t *testing.T) {
	const entity = "wf-schema-malformed"
	importModelE2E(t, entity, 1)
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0.0",
			"name": "wf-malformed",
			"initialState": "S1",
			"active": true,
			"states": {"S1": {}}
		}]
	}`
	status, respBody := importWorkflowE2E(t, entity, 1, body)
	if status != http.StatusBadRequest {
		t.Fatalf("import status = %d; want 400; body: %s", status, respBody)
	}
	if !strings.Contains(respBody, "MAJOR.MINOR") {
		t.Fatalf("body does not mention MAJOR.MINOR form: %s", respBody)
	}
	if !strings.Contains(respBody, "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED") {
		t.Fatalf("body does not contain WORKFLOW_SCHEMA_VERSION_UNSUPPORTED: %s", respBody)
	}
}

// TestWorkflowSchemaVersion_ExportStampsCurrent — after import, the
// export response carries the current schema version on every workflow.
func TestWorkflowSchemaVersion_ExportStampsCurrent(t *testing.T) {
	const entity = "wf-schema-export"
	importModelE2E(t, entity, 1)
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0",
			"name": "wf-export",
			"initialState": "S1",
			"active": true,
			"states": {"S1": {}}
		}]
	}`
	if status, b := importWorkflowE2E(t, entity, 1, body); status != http.StatusOK {
		t.Fatalf("import status = %d; body: %s", status, b)
	}
	status, exportBody := exportWorkflowE2E(t, entity, 1)
	if status != http.StatusOK {
		t.Fatalf("export status = %d", status)
	}
	wfs, ok := exportBody["workflows"].([]any)
	if !ok {
		t.Fatalf("export body missing workflows: %+v", exportBody)
	}
	for i, raw := range wfs {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("workflow[%d] not a map: %T", i, raw)
		}
		if m["version"] != "1.0" {
			t.Fatalf("workflow[%d] version = %v; want \"1.0\"", i, m["version"])
		}
	}
}

// TestWorkflowSchemaVersion_HelpVersionsAction — discovery endpoint.
func TestWorkflowSchemaVersion_HelpVersionsAction(t *testing.T) {
	resp := doAuth(t, http.MethodGet, "/api/help/workflows/schema-version/versions", "")
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, respBody)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q; want application/json", ct)
	}
	var got struct {
		Current   string           `json:"current"`
		Supported []map[string]int `json:"supported"`
	}
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode: %v; raw: %s", err, respBody)
	}
	if got.Current != "1.0" {
		t.Fatalf("current = %q; want 1.0", got.Current)
	}
	if len(got.Supported) != 1 {
		t.Fatalf("supported length = %d; want 1; got %+v", len(got.Supported), got.Supported)
	}
	s := got.Supported[0]
	if s["major"] != 1 || s["minMinor"] != 0 || s["maxMinor"] != 0 {
		t.Fatalf("supported[0] = %+v; want {major:1, minMinor:0, maxMinor:0}", s)
	}
}

// TestWorkflowSchemaVersion_HelpGRPCProtoStillWorks — regression on a
// pre-existing action, proves the HTTP action mirror is generic.
func TestWorkflowSchemaVersion_HelpGRPCProtoStillWorks(t *testing.T) {
	resp := doAuth(t, http.MethodGet, "/api/help/grpc/proto", "")
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, "syntax") && !strings.Contains(respBody, "proto") {
		t.Fatalf("body does not contain proto source: %.200s", respBody)
	}
}
