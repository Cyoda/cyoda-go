package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const workflowSampleModel = `{"name": "Test Order", "amount": 100, "status": "draft"}`

const workflowV1 = `{
	"importMode": "REPLACE",
	"workflows": [
		{
			"version": "1.1",
			"name": "order-workflow-v1",
			"initialState": "NONE",
			"active": true,
			"states": {
				"NONE": {
					"transitions": [{"name": "create", "next": "CREATED", "manual": false}]
				},
				"CREATED": {
					"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]
				},
				"APPROVED": {}
			}
		}
	]
}`

const workflowV2 = `{
	"importMode": "REPLACE",
	"workflows": [
		{
			"version": "1.1",
			"name": "order-workflow-v2",
			"initialState": "NONE",
			"active": true,
			"states": {
				"NONE": {
					"transitions": [{"name": "create", "next": "CREATED", "manual": false}]
				},
				"CREATED": {
					"transitions": [
						{"name": "approve", "next": "APPROVED", "manual": true},
						{"name": "reject", "next": "REJECTED", "manual": true}
					]
				},
				"APPROVED": {},
				"REJECTED": {}
			}
		}
	]
}`

// importModelE2E imports a model via the REST API and asserts a 200 response.
func importModelE2E(t *testing.T, entityName string, modelVersion int) {
	t.Helper()
	path := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, workflowSampleModel)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("importModel %s/%d: expected 200, got %d: %s", entityName, modelVersion, resp.StatusCode, body)
	}
}

// lockModelE2E locks a model via the REST API and asserts a 200 response.
func lockModelE2E(t *testing.T, entityName string, modelVersion int) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/lock", entityName, modelVersion)
	resp := doAuth(t, http.MethodPut, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lockModel %s/%d: expected 200, got %d: %s", entityName, modelVersion, resp.StatusCode, body)
	}
}

// importWorkflowE2E imports a workflow and returns the response status and body.
func importWorkflowE2E(t *testing.T, entityName string, modelVersion int, payload string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/workflow/import", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, payload)
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// exportWorkflowE2E exports a workflow and returns the decoded JSON body.
// Parses the body for any status code so callers can inspect both success
// and error responses (e.g., 404 with WORKFLOW_NOT_FOUND).
func exportWorkflowE2E(t *testing.T, entityName string, modelVersion int) (int, map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/workflow/export", entityName, modelVersion)
	resp := doAuth(t, http.MethodGet, path, "")
	raw := readBody(t, resp)
	var result map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("exportWorkflow %s/%d: failed to parse JSON: %v\nbody: %s", entityName, modelVersion, err, raw)
		}
	}
	return resp.StatusCode, result
}

// TestWorkflow_OverwriteWorkflow verifies that importing a workflow with
// REPLACE mode overwrites a previously imported workflow.
func TestWorkflow_OverwriteWorkflow(t *testing.T) {
	const entityName = "e2e-order-2"
	const modelVersion = 1

	// 1. Import model and lock.
	importModelE2E(t, entityName, modelVersion)
	lockModelE2E(t, entityName, modelVersion)

	// 2. Import workflow v1.
	status, body := importWorkflowE2E(t, entityName, modelVersion, workflowV1)
	if status != http.StatusOK {
		t.Fatalf("workflow import v1: expected 200, got %d: %s", status, body)
	}

	// Verify v1 is present.
	exportStatus, exportBody := exportWorkflowE2E(t, entityName, modelVersion)
	if exportStatus != http.StatusOK {
		t.Fatalf("workflow export after v1: expected 200, got %d", exportStatus)
	}
	wfs1, _ := exportBody["workflows"].([]any)
	if len(wfs1) != 1 {
		t.Fatalf("after v1 import: expected 1 workflow, got %d", len(wfs1))
	}
	wf1, _ := wfs1[0].(map[string]any)
	if name, _ := wf1["name"].(string); name != "order-workflow-v1" {
		t.Errorf("after v1 import: expected name=order-workflow-v1, got %q", name)
	}

	// 3. Import workflow v2 with REPLACE mode.
	status, body = importWorkflowE2E(t, entityName, modelVersion, workflowV2)
	if status != http.StatusOK {
		t.Fatalf("workflow import v2: expected 200, got %d: %s", status, body)
	}

	// 4. Export and verify v2 is returned (v1 replaced).
	exportStatus, exportBody = exportWorkflowE2E(t, entityName, modelVersion)
	if exportStatus != http.StatusOK {
		t.Fatalf("workflow export after v2: expected 200, got %d", exportStatus)
	}

	wfs2, ok := exportBody["workflows"].([]any)
	if !ok {
		t.Fatalf("workflow export: expected workflows array, got %T", exportBody["workflows"])
	}
	if len(wfs2) != 1 {
		t.Fatalf("after v2 REPLACE: expected 1 workflow, got %d", len(wfs2))
	}

	wf2, ok := wfs2[0].(map[string]any)
	if !ok {
		t.Fatalf("workflow export: expected workflow object, got %T", wfs2[0])
	}
	if name, _ := wf2["name"].(string); name != "order-workflow-v2" {
		t.Errorf("after v2 REPLACE: expected name=order-workflow-v2, got %q", name)
	}

	// v2 has 4 states (NONE, CREATED, APPROVED, REJECTED).
	states, _ := wf2["states"].(map[string]any)
	if len(states) != 4 {
		t.Errorf("after v2 REPLACE: expected 4 states, got %d", len(states))
	}
}

// TestWorkflow_ImportUnknownModel verifies that importing a workflow targeting
// a model that does not exist returns 404 MODEL_NOT_FOUND. This covers issue
// #131: previously the import silently succeeded with 200 {"success":true};
// cyoda-cloud parity requires HTTP 404 + MODEL_NOT_FOUND. See the workflow
// handler unit test TestImport_UnknownModel_Returns404 for the canonical
// assertion.
func TestWorkflow_ImportUnknownModel(t *testing.T) {
	const entityName = "e2e-ghost-model"
	const modelVersion = 1

	// Deliberately do NOT import the model — it must not exist.
	status, body := importWorkflowE2E(t, entityName, modelVersion, workflowV1)
	if status != http.StatusNotFound {
		t.Fatalf("workflow import on unknown model: expected 404, got %d: %s", status, body)
	}

	// Parse RFC 9457 problem-detail body and assert the error code is in the detail.
	var problem map[string]any
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("workflow import 404: failed to parse problem-detail JSON: %v\nbody: %s", err, body)
	}
	detail, _ := problem["detail"].(string)
	if detail == "" {
		t.Fatal("workflow import 404: expected non-empty detail in response body")
	}
	const wantCode = "MODEL_NOT_FOUND"
	if !strings.Contains(detail, wantCode) {
		t.Errorf("workflow import 404: expected error code %s in detail, got: %s", wantCode, detail)
	}
}

// TestWorkflow_Import_UnknownField_Returns400 is the e2e fence for the
// strict-decoder boundary. Through the full JWT-authenticated HTTP
// stack, a workflow-import body carrying a typo'd nested field
// (`transitionn` for `transitions`) is rejected with 400 BAD_REQUEST
// and the offending field name surfaced in the RFC 9457 detail. Pairs
// with handler-level TestImport_UnknownField_Typo_Returns400.
func TestWorkflow_Import_UnknownField_Returns400(t *testing.T) {
	const entityName = "e2e-strict-decoder"
	const modelVersion = 1

	importModelE2E(t, entityName, modelVersion)
	lockModelE2E(t, entityName, modelVersion)

	// `transitionn` (extra n) inside a StateDefinition — previously
	// silently dropped, now rejected by the strict decoder.
	body := `{
		"importMode":"REPLACE",
		"workflows":[{
			"version":"1.1","name":"typo-wf","initialState":"S1","active":true,
			"states":{"S1":{"transitionn":[{"name":"go","next":"S2","manual":true}]},"S2":{}}
		}]
	}`
	status, respBody := importWorkflowE2E(t, entityName, modelVersion, body)
	if status != http.StatusBadRequest {
		t.Fatalf("workflow import with typo'd field: expected 400, got %d: %s", status, respBody)
	}

	var problem map[string]any
	if err := json.Unmarshal([]byte(respBody), &problem); err != nil {
		t.Fatalf("workflow import 400: failed to parse problem-detail JSON: %v\nbody: %s", err, respBody)
	}
	detail, _ := problem["detail"].(string)
	if !strings.Contains(detail, "BAD_REQUEST") {
		t.Errorf("workflow import 400: expected error code BAD_REQUEST in detail, got: %s", detail)
	}
	if !strings.Contains(detail, "transitionn") {
		t.Errorf("workflow import 400: expected field name 'transitionn' in detail, got: %s", detail)
	}
}

// TestWorkflow_ExportEmpty verifies that exporting a workflow for a model
// that has no imported workflows returns 404 WORKFLOW_NOT_FOUND. A properly
// structured GET on a non-existent resource should return NOT FOUND, not
// 200 with an empty list. See the workflow handler unit test
// TestExportEmpty_Returns404 for the canonical assertion.
func TestWorkflow_ExportEmpty(t *testing.T) {
	const entityName = "e2e-order-3"
	const modelVersion = 1

	// 1. Import model and lock (no workflow imported).
	importModelE2E(t, entityName, modelVersion)
	lockModelE2E(t, entityName, modelVersion)

	// 2. Export workflow — should return 404 WORKFLOW_NOT_FOUND.
	exportStatus, exportBody := exportWorkflowE2E(t, entityName, modelVersion)
	if exportStatus != http.StatusNotFound {
		t.Fatalf("workflow export (no workflows): expected 404, got %d", exportStatus)
	}

	// Verify the error code is in the response body's detail field (RFC 9457).
	detail, _ := exportBody["detail"].(string)
	if detail == "" {
		t.Fatal("workflow export 404: expected non-empty detail in response body")
	}
	const wantCode = "WORKFLOW_NOT_FOUND"
	if !strings.Contains(detail, wantCode) {
		t.Errorf("workflow export 404: expected error code %s in detail, got: %s", wantCode, detail)
	}
}
