package workflow_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// newTestServer creates an App with default config and returns an httptest.Server.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// importModel creates a model so that the workflow endpoints have something to reference.
func importModel(t *testing.T, base, entityName string, version int) {
	t.Helper()
	url := base + "/model/import/JSON/SAMPLE_DATA/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"name":"test"}`))
	if err != nil {
		t.Fatalf("model import failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model import expected 200, got %d", resp.StatusCode)
	}
}

func doWorkflowImport(t *testing.T, base, entityName string, version int, body string) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/workflow/import"
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("workflow import request failed: %v", err)
	}
	return resp
}

func doWorkflowExport(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/workflow/export"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("workflow export request failed: %v", err)
	}
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, data)
	}
	return result
}

func readWorkflows(t *testing.T, resp *http.Response) []spi.WorkflowDefinition {
	t.Helper()
	body := readJSON(t, resp)
	wfRaw, err := json.Marshal(body["workflows"])
	if err != nil {
		t.Fatalf("failed to marshal workflows: %v", err)
	}
	var wfs []spi.WorkflowDefinition
	if err := json.Unmarshal(wfRaw, &wfs); err != nil {
		t.Fatalf("failed to parse workflows: %v\nraw: %s", err, wfRaw)
	}
	return wfs
}

func TestImportAndExport(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "order-flow",
				"initialState": "NEW",
				"active": true,
				"states": {
					"NEW": {
						"transitions": [
							{"name": "approve", "next": "APPROVED", "manual": true}
						]
					},
					"APPROVED": {
						"transitions": []
					}
				}
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

	// Export and verify round-trip.
	exportResp := doWorkflowExport(t, srv.URL, "Order", 1)
	if exportResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(exportResp.Body)
		exportResp.Body.Close()
		t.Fatalf("export expected 200, got %d: %s", exportResp.StatusCode, b)
	}

	wfs := readWorkflows(t, exportResp)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].Name != "order-flow" {
		t.Errorf("expected name order-flow, got %s", wfs[0].Name)
	}
	if wfs[0].InitialState != "NEW" {
		t.Errorf("expected initialState NEW, got %s", wfs[0].InitialState)
	}
	if !wfs[0].Active {
		t.Error("expected workflow to be active")
	}
	if len(wfs[0].States) != 2 {
		t.Errorf("expected 2 states, got %d", len(wfs[0].States))
	}
}

func TestImportMerge(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Import WF-A.
	bodyA := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-a",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, bodyA)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import A: expected 200, got %d", resp.StatusCode)
	}

	// Import WF-B with MERGE → both should be present.
	bodyB := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-b",
				"initialState": "S2",
				"active": true,
				"states": {"S2": {"transitions": []}}
			}
		]
	}`
	resp = doWorkflowImport(t, srv.URL, "Order", 1, bodyB)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import B: expected 200, got %d", resp.StatusCode)
	}

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(wfs))
	}
	names := map[string]bool{}
	for _, wf := range wfs {
		names[wf.Name] = true
	}
	if !names["wf-a"] || !names["wf-b"] {
		t.Errorf("expected wf-a and wf-b, got %v", names)
	}
}

func TestImportReplace(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Import WF-A.
	bodyA := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-a",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, bodyA)
	resp.Body.Close()

	// Import WF-B with REPLACE → only WF-B.
	bodyB := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-b",
				"initialState": "S2",
				"active": true,
				"states": {"S2": {"transitions": []}}
			}
		]
	}`
	resp = doWorkflowImport(t, srv.URL, "Order", 1, bodyB)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import B: expected 200, got %d", resp.StatusCode)
	}

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].Name != "wf-b" {
		t.Errorf("expected wf-b, got %s", wfs[0].Name)
	}
}

func TestImportActivate(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Import WF-A.
	bodyA := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-a",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, bodyA)
	resp.Body.Close()

	// Import WF-B with ACTIVATE → WF-A inactive, WF-B active.
	bodyB := `{
		"importMode": "ACTIVATE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-b",
				"initialState": "S2",
				"active": true,
				"states": {"S2": {"transitions": []}}
			}
		]
	}`
	resp = doWorkflowImport(t, srv.URL, "Order", 1, bodyB)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import B: expected 200, got %d", resp.StatusCode)
	}

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(wfs))
	}

	wfMap := map[string]spi.WorkflowDefinition{}
	for _, wf := range wfs {
		wfMap[wf.Name] = wf
	}

	if wfMap["wf-a"].Active {
		t.Error("expected wf-a to be inactive after ACTIVATE import")
	}
	if !wfMap["wf-b"].Active {
		t.Error("expected wf-b to be active after ACTIVATE import")
	}
}

func TestExportEmpty_Returns404(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	resp := doWorkflowExport(t, srv.URL, "Order", 1)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, b)
	}

	body := readJSON(t, resp)
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, common.ErrCodeWorkflowNotFound) {
		t.Errorf("expected error code %s in detail, got: %s", common.ErrCodeWorkflowNotFound, detail)
	}
}

// TestExport_UnknownModel_Returns404_ModelNotFound asserts the export
// handler distinguishes "model does not exist" (MODEL_NOT_FOUND) from
// "model exists but has no workflows" (WORKFLOW_NOT_FOUND). The import
// handler already enforces the same distinction (see
// TestImport_UnknownModel_Returns404); export now mirrors it.
func TestExport_UnknownModel_Returns404_ModelNotFound(t *testing.T) {
	srv := newTestServer(t)
	// NOTE: deliberately do NOT call importModel — the model "Ghost" does not exist.

	resp := doWorkflowExport(t, srv.URL, "Ghost", 1)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404 for export against unknown model, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeModelNotFound)
}

func TestImportFullWorkflow(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "PaymentRequest", 1)

	body := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "payment-request-flow",
				"initialState": "PENDING_VALIDATION",
				"active": true,
				"states": {
					"PENDING_VALIDATION": {
						"transitions": [
							{
								"name": "validate",
								"next": "VALIDATED",
								"manual": false,
								"processors": [
									{"name": "validate-payment", "type": "FUNCTION"}
								]
							}
						]
					},
					"VALIDATED": {
						"transitions": [
							{
								"name": "approve",
								"next": "APPROVED",
								"manual": true
							},
							{
								"name": "reject",
								"next": "REJECTED",
								"manual": true
							}
						]
					},
					"APPROVED": {
						"transitions": [
							{
								"name": "process",
								"next": "PROCESSED",
								"manual": false,
								"processors": [
									{"name": "execute-payment", "type": "EXTERNAL_API"}
								]
							}
						]
					},
					"PROCESSED": {
						"transitions": []
					},
					"REJECTED": {
						"transitions": []
					}
				}
			}
		]
	}`

	resp := doWorkflowImport(t, srv.URL, "PaymentRequest", 1, body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("import expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "PaymentRequest", 1))
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}

	wf := wfs[0]
	if wf.Name != "payment-request-flow" {
		t.Errorf("expected name payment-request-flow, got %s", wf.Name)
	}
	if wf.InitialState != "PENDING_VALIDATION" {
		t.Errorf("expected initialState PENDING_VALIDATION, got %s", wf.InitialState)
	}
	if len(wf.States) != 5 {
		t.Errorf("expected 5 states, got %d", len(wf.States))
	}

	// Verify PENDING_VALIDATION transitions.
	pv := wf.States["PENDING_VALIDATION"]
	if len(pv.Transitions) != 1 {
		t.Fatalf("PENDING_VALIDATION: expected 1 transition, got %d", len(pv.Transitions))
	}
	if pv.Transitions[0].Name != "validate" {
		t.Errorf("expected transition name validate, got %s", pv.Transitions[0].Name)
	}
	if pv.Transitions[0].Next != "VALIDATED" {
		t.Errorf("expected next VALIDATED, got %s", pv.Transitions[0].Next)
	}
	if len(pv.Transitions[0].Processors) != 1 {
		t.Fatalf("expected 1 processor, got %d", len(pv.Transitions[0].Processors))
	}
	if pv.Transitions[0].Processors[0].Name != "validate-payment" {
		t.Errorf("expected processor name validate-payment, got %s", pv.Transitions[0].Processors[0].Name)
	}

	// Verify VALIDATED has 2 manual transitions.
	v := wf.States["VALIDATED"]
	if len(v.Transitions) != 2 {
		t.Fatalf("VALIDATED: expected 2 transitions, got %d", len(v.Transitions))
	}
	for _, tr := range v.Transitions {
		if !tr.Manual {
			t.Errorf("expected transition %s to be manual", tr.Name)
		}
	}

	// Verify PROCESSED and REJECTED are terminal.
	for _, state := range []string{"PROCESSED", "REJECTED"} {
		s := wf.States[state]
		if len(s.Transitions) != 0 {
			t.Errorf("%s: expected 0 transitions, got %d", state, len(s.Transitions))
		}
	}
}

// TestImport_UnknownModel_Returns404 covers issue #131: importing a workflow
// targeting a model that does not exist must return HTTP 404 with the
// MODEL_NOT_FOUND error code, rather than the legacy 200 {"success":true}.
func TestImport_UnknownModel_Returns404(t *testing.T) {
	srv := newTestServer(t)
	// NOTE: deliberately do NOT call importModel — the model "Ghost" does not exist.

	body := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"version": "1.0",
				"name": "ghost-flow",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`

	resp := doWorkflowImport(t, srv.URL, "Ghost", 1, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404 for workflow import on unknown model, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeModelNotFound)
}

// TestImport_ValidationFailures_Return400 covers issue #255: the
// structural validator must surface through the public import HTTP path
// with a 400 Bad Request and the offending name/state/transition in
// the error detail. One sub-test per H6 rule serves as a regression
// fence at the wire boundary; the unit-level coverage lives in
// validate_import_test.go.
func TestImport_ValidationFailures_Return400(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	cases := []struct {
		name        string
		body        string
		mustContain string // substring required in 400 detail
	}{
		{
			name: "H6.c empty workflow name",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"","initialState":"S1","active":true,"states":{"S1":{}}}]}`,
			mustContain: "name",
		},
		{
			name: "H6.d duplicate workflow names in request",
			body: `{"importMode":"REPLACE","workflows":[
				{"version":"1","name":"dup","initialState":"S1","active":true,"states":{"S1":{}}},
				{"version":"1","name":"dup","initialState":"S2","active":true,"states":{"S2":{}}}
			]}`,
			mustContain: "duplicate",
		},
		{
			name: "H6.a empty initialState",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"","active":true,"states":{"S1":{}}}]}`,
			mustContain: "initialState",
		},
		{
			name: "H6.a initialState not in States",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"MISSING","active":true,"states":{"S1":{}}}]}`,
			mustContain: "MISSING",
		},
		{
			name: "H6.b transition.next not in States",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"S1","active":true,
				"states":{"S1":{"transitions":[{"name":"go","next":"NOWHERE","manual":true}]}}}]}`,
			mustContain: "NOWHERE",
		},
		{
			name: "H6.e duplicate transition name in state",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"S1","active":true,
				"states":{
					"S1":{"transitions":[{"name":"go","next":"S2","manual":true},{"name":"go","next":"S3","manual":true}]},
					"S2":{},"S3":{}
				}}]}`,
			mustContain: "duplicate",
		},
		{
			name: "H4 unknown ExecutionMode",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"S1","active":true,
				"states":{"S1":{"transitions":[{"name":"t","next":"S2","manual":true,"processors":[
					{"type":"externalized","name":"p","executionMode":"ASYN_SAME_TX"}
				]}]}, "S2":{}}}]}`,
			mustContain: "ASYN_SAME_TX",
		},
		// Security-audit follow-ups M-1 + L-1 + L-2.
		{
			name: "M-1 empty state-map key",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"S1","active":true,
				"states":{"S1":{},"":{}}}]}`,
			mustContain: "empty state name",
		},
		{
			name: "L-1 empty transition name",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"S1","active":true,
				"states":{"S1":{"transitions":[{"name":"","next":"S2","manual":true}]}, "S2":{}}}]}`,
			mustContain: "empty transition name",
		},
		{
			name: "L-1 empty processor name",
			body: `{"importMode":"REPLACE","workflows":[{"version":"1","name":"wf","initialState":"S1","active":true,
				"states":{"S1":{"transitions":[{"name":"t","next":"S2","manual":true,"processors":[
					{"type":"externalized","name":"","executionMode":"SYNC"}
				]}]}, "S2":{}}}]}`,
			mustContain: "empty processor name",
		},
		{
			name:        "L-2 workflow name exceeds 256 chars",
			body:        `{"importMode":"REPLACE","workflows":[{"version":"1","name":"` + strings.Repeat("x", 257) + `","initialState":"S1","active":true,"states":{"S1":{}}}]}`,
			mustContain: "256-char limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doWorkflowImport(t, srv.URL, "Order", 1, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
			}
			commontest.ExpectErrorCode(t, resp, common.ErrCodeValidationFailed)
			body := readJSON(t, resp)
			detail, _ := body["detail"].(string)
			if !strings.Contains(detail, tc.mustContain) {
				t.Errorf("detail %q missing required substring %q", detail, tc.mustContain)
			}
		})
	}
}

func TestImportDefaultMode(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// No importMode specified → defaults to MERGE.
	body := `{
		"workflows": [
			{
				"version": "1.0",
				"name": "wf-default",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].Name != "wf-default" {
		t.Errorf("expected wf-default, got %s", wfs[0].Name)
	}
}

// TestImport_LowercaseImportMode asserts importMode parsing is
// case-insensitive — the handler upper-cases the incoming value before
// dispatch. This test pins the behaviour at the wire boundary so the OpenAPI
// description (which declares case-insensitivity) and the implementation
// stay aligned.
func TestImport_LowercaseImportMode(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	cases := []string{"merge", "Merge", "replace", "Replace", "activate", "Activate"}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			body := `{
				"importMode": "` + mode + `",
				"workflows": [
					{
						"version": "1.0",
						"name": "case-flow",
						"initialState": "S1",
						"active": true,
						"states": {"S1": {"transitions": []}}
					}
				]
			}`
			resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200 for importMode=%q, got %d: %s", mode, resp.StatusCode, b)
			}
		})
	}
}

// TestImport_EmptyImportMode_DefaultsToMerge asserts an explicit empty
// string defaults to MERGE — the omitted-field path is covered by
// TestImportDefaultMode; this pins the explicit-empty half of the
// OpenAPI claim ("Defaults to MERGE when the field is omitted or empty").
func TestImport_EmptyImportMode_DefaultsToMerge(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{
		"importMode": "",
		"workflows": [
			{
				"version": "1.0",
				"name": "empty-mode-flow",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for empty importMode, got %d: %s", resp.StatusCode, b)
	}

	wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
	if len(wfs) != 1 || wfs[0].Name != "empty-mode-flow" {
		t.Errorf("expected empty-mode-flow stored after MERGE default, got %v", wfs)
	}
}

// TestImport_UnknownImportMode asserts an importMode value outside the
// documented enum returns 400 BAD_REQUEST. Pre-existing happy-path tests
// exercised only the success branch; the rejection branch needs its own
// regression fence.
func TestImport_UnknownImportMode(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{
		"importMode": "MIGRATE",
		"workflows": [
			{
				"version": "1.0",
				"name": "wf",
				"initialState": "S1",
				"active": true,
				"states": {"S1": {"transitions": []}}
			}
		]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for unknown importMode, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeBadRequest)
	respBody := readJSON(t, resp)
	detail, _ := respBody["detail"].(string)
	if !strings.Contains(detail, "MIGRATE") {
		t.Errorf("expected detail to name the offending mode 'MIGRATE', got: %s", detail)
	}
}

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

func TestImport_ExplicitActiveNull_DefaultsToTrue(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Explicit JSON null on `active` decodes to (*bool)(nil), which the
	// handler defaults to true — same as the field being absent.
	body := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0",
				"name": "null-active-flow",
				"initialState": "NEW",
				"active": null,
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
		t.Errorf("expected explicit null active to default to true, got Active=false")
	}
}

func TestImport_EmptyArrayReplace_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{"importMode":"REPLACE","workflows":[]}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), common.ErrCodeValidationFailed) {
		t.Errorf("expected error code %s in body, got: %s", common.ErrCodeValidationFailed, b)
	}
	if !strings.Contains(string(b), "empty workflows array not allowed") {
		t.Errorf("expected detail mentioning 'empty workflows array not allowed', got: %s", b)
	}
}

func TestImport_EmptyArrayActivate_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{"importMode":"ACTIVATE","workflows":[]}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), common.ErrCodeValidationFailed) {
		t.Errorf("expected error code %s in body, got: %s", common.ErrCodeValidationFailed, b)
	}
	if !strings.Contains(string(b), "empty workflows array not allowed") {
		t.Errorf("expected detail mentioning 'empty workflows array not allowed', got: %s", b)
	}
}

func TestImport_MissingWorkflowsKeyReplace_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// `workflows` key entirely absent → JSON unmarshal yields nil slice
	// → len() == 0 → must reject equivalently to explicit []. Confirm we
	// hit the M3 guard specifically (not, e.g., a downstream validator).
	body := `{"importMode":"REPLACE"}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), common.ErrCodeValidationFailed) {
		t.Errorf("expected error code %s in body, got: %s", common.ErrCodeValidationFailed, b)
	}
	if !strings.Contains(string(b), "empty workflows array not allowed") {
		t.Errorf("expected detail mentioning 'empty workflows array not allowed', got: %s", b)
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

// TestImport_MalformedJSONBody_Returns400 pins the body-parser's
// rejection path: a malformed JSON body must be rejected at the wire
// boundary before any validation runs. The rejection path exists at
// handler.go's json.Unmarshal call but no test exercised it before
// this coverage sweep.
func TestImport_MalformedJSONBody_Returns400(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Unterminated string + missing brace — definitely not valid JSON.
	body := `{"importMode": "MERGE", "workflows": [{"name": "broken`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for malformed JSON, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeBadRequest)
	respBody := readJSON(t, resp)
	detail, _ := respBody["detail"].(string)
	if !strings.Contains(detail, "invalid JSON") {
		t.Errorf("expected detail to mention 'invalid JSON', got: %s", detail)
	}
}

// TestImport_EmptyBody_Returns400 covers the zero-length body case.
// JSON unmarshal of "" yields an "unexpected end of JSON input" error
// which the handler surfaces as 400 BAD_REQUEST. Without this fence,
// a future migration to a streaming decoder could silently accept the
// empty case.
func TestImport_EmptyBody_Returns400(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	resp := doWorkflowImport(t, srv.URL, "Order", 1, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for empty body, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeBadRequest)
}

// TestImport_OversizedBody_Rejected pins the http.MaxBytesReader cap
// wired at handler.go (10 MiB). The handler reads the body via
// io.ReadAll, which surfaces the cap as a read error → 400 BAD_REQUEST
// with the "failed to read request body" detail. Without this test the
// MaxBytesReader could be silently removed in a refactor.
func TestImport_OversizedBody_Rejected(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Synthesise a body just past the 10 MiB cap by padding a workflow
	// `desc` field with whitespace. The JSON itself stays well-formed so
	// the rejection is unambiguously about size, not shape.
	pad := strings.Repeat(" ", 10*1024*1024+1024) // 10 MiB + 1 KiB
	body := `{"importMode":"MERGE","workflows":[{"version":"1.0","name":"big","initialState":"S","active":true,"desc":"` + pad + `","states":{"S":{}}}]}`

	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for oversized body, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeBadRequest)
}

// TestImport_MultipleCycles_RejectsWithFirstCycle covers the
// definite-loop detector against a workflow with two distinct
// unguarded automated cycles. validateWorkflowLoops iterates wf.States
// in Go map order, so the *specific* cycle reported is
// non-deterministic; this test asserts only that AT LEAST one of the
// two cycle paths is named in the error detail, which is enough as a
// regression fence and stays deterministic across runs.
func TestImport_MultipleCycles_RejectsWithFirstCycle(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	// Two disjoint unguarded automated cycles:
	//   Cycle 1: A → B → A
	//   Cycle 2: C → D → C
	// The validator returns whichever it finds first.
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "multi-cycle", "initialState": "A",
			"active": true,
			"states": {
				"A": {"transitions": [{"name": "to-b", "next": "B", "manual": false}]},
				"B": {"transitions": [{"name": "to-a", "next": "A", "manual": false}]},
				"C": {"transitions": [{"name": "to-d", "next": "D", "manual": false}]},
				"D": {"transitions": [{"name": "to-c", "next": "C", "manual": false}]}
			}
		}]
	}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for multi-cycle workflow, got %d: %s", resp.StatusCode, b)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeValidationFailed)
	respBody := readJSON(t, resp)
	detail, _ := respBody["detail"].(string)
	if !strings.Contains(detail, "infinite loop") {
		t.Errorf("detail must mention 'infinite loop', got: %s", detail)
	}
	// Assert at least one of the two cycles is reported. The validator's
	// map-iteration order can pick either; both are valid findings.
	cycle1 := strings.Contains(detail, "A -> B") || strings.Contains(detail, "B -> A")
	cycle2 := strings.Contains(detail, "C -> D") || strings.Contains(detail, "D -> C")
	if !cycle1 && !cycle2 {
		t.Errorf("detail must name at least one of the two cycles, got: %s", detail)
	}
}

// TestExport_ReimportReplace_RoundtripEquivalence covers the generic
// "all fields survive an export → REPLACE re-import → re-export" round
// trip. The pre-existing TestExportReimportRoundtrip_PreservesActiveFalse
// pins the Active=false-specific case; this test exercises the full
// shape including criterion, multi-state transitions with
// manual/automated/disabled/criterion mixed, and processor configs.
func TestExport_ReimportReplace_RoundtripEquivalence(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	seed := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.0", "name": "rich-wf",
				"desc": "covers all the round-trip surface area",
				"initialState": "NEW",
				"active": true,
				"criterion": {
					"type": "simple",
					"jsonPath": "$.kind",
					"operatorType": "EQUALS",
					"value": "premium"
				},
				"states": {
					"NEW": {
						"transitions": [
							{
								"name": "approve", "next": "APPROVED", "manual": true,
								"criterion": {
									"type": "simple", "jsonPath": "$.amount",
									"operatorType": "GREATER_THAN", "value": "100"
								}
							},
							{
								"name": "auto-validate", "next": "VALIDATED", "manual": false,
								"disabled": true,
								"processors": [
									{
										"type": "externalized", "name": "validator",
										"executionMode": "SYNC",
										"config": {
											"attachEntity": true,
											"calculationNodesTags": "validation-svc",
											"responseTimeoutMs": 5000,
											"context": "role-A"
										}
									}
								]
							}
						]
					},
					"APPROVED": {"transitions": []},
					"VALIDATED": {"transitions": []}
				}
			},
			{
				"version": "1.0", "name": "fallback-wf",
				"initialState": "S",
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

	// First export.
	export1 := readJSON(t, doWorkflowExport(t, srv.URL, "Order", 1))

	// Re-import via REPLACE using the exported shape.
	reimport, err := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows":  export1["workflows"],
	})
	if err != nil {
		t.Fatalf("marshal re-import: %v", err)
	}
	resp2 := doWorkflowImport(t, srv.URL, "Order", 1, string(reimport))
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("re-import expected 200, got %d: %s", resp2.StatusCode, b)
	}
	resp2.Body.Close()

	// Second export — assert semantic equality with the first.
	export2 := readJSON(t, doWorkflowExport(t, srv.URL, "Order", 1))

	// Workflow envelope first — entityName + modelVersion must match.
	if export1["entityName"] != export2["entityName"] {
		t.Errorf("entityName drift: %v → %v", export1["entityName"], export2["entityName"])
	}
	if export1["modelVersion"] != export2["modelVersion"] {
		t.Errorf("modelVersion drift: %v → %v", export1["modelVersion"], export2["modelVersion"])
	}

	// Decode both workflow arrays as []spi.WorkflowDefinition for deep
	// comparison — bypasses any map-iteration ordering noise in the raw
	// JSON envelope.
	wfs1Raw, _ := json.Marshal(export1["workflows"])
	wfs2Raw, _ := json.Marshal(export2["workflows"])
	var wfs1, wfs2 []spi.WorkflowDefinition
	if err := json.Unmarshal(wfs1Raw, &wfs1); err != nil {
		t.Fatalf("decode export1 workflows: %v", err)
	}
	if err := json.Unmarshal(wfs2Raw, &wfs2); err != nil {
		t.Fatalf("decode export2 workflows: %v", err)
	}

	if len(wfs1) != len(wfs2) {
		t.Fatalf("workflow count drift: %d → %d", len(wfs1), len(wfs2))
	}

	// Index by name so declaration-order differences don't masquerade
	// as semantic drift.
	idx2 := map[string]spi.WorkflowDefinition{}
	for _, w := range wfs2 {
		idx2[w.Name] = w
	}
	for _, w1 := range wfs1 {
		w2, ok := idx2[w1.Name]
		if !ok {
			t.Errorf("workflow %q missing from second export", w1.Name)
			continue
		}
		if w1.Version != w2.Version {
			t.Errorf("%s: version drift %q → %q", w1.Name, w1.Version, w2.Version)
		}
		if w1.Description != w2.Description {
			t.Errorf("%s: desc drift %q → %q", w1.Name, w1.Description, w2.Description)
		}
		if w1.InitialState != w2.InitialState {
			t.Errorf("%s: initialState drift %q → %q", w1.Name, w1.InitialState, w2.InitialState)
		}
		if w1.Active != w2.Active {
			t.Errorf("%s: active drift %v → %v", w1.Name, w1.Active, w2.Active)
		}
		// Criterion is json.RawMessage — compare semantically after
		// canonicalising via json.Unmarshal/Marshal so key-order doesn't
		// cause false positives.
		if !semanticJSONEqual(t, w1.Criterion, w2.Criterion) {
			t.Errorf("%s: criterion drift %s → %s", w1.Name, w1.Criterion, w2.Criterion)
		}
		if len(w1.States) != len(w2.States) {
			t.Errorf("%s: state-count drift %d → %d", w1.Name, len(w1.States), len(w2.States))
			continue
		}
		for stateName, s1 := range w1.States {
			s2, ok := w2.States[stateName]
			if !ok {
				t.Errorf("%s: state %q missing", w1.Name, stateName)
				continue
			}
			if len(s1.Transitions) != len(s2.Transitions) {
				t.Errorf("%s.%s: transition-count drift %d → %d",
					w1.Name, stateName, len(s1.Transitions), len(s2.Transitions))
			}
			// Transitions are declared as an ordered slice — preserve
			// order, but a state with zero transitions in both exports
			// is acceptable as either nil or []TransitionDefinition{}.
			for i := range s1.Transitions {
				t1, t2 := s1.Transitions[i], s2.Transitions[i]
				if t1.Name != t2.Name || t1.Next != t2.Next ||
					t1.Manual != t2.Manual || t1.Disabled != t2.Disabled {
					t.Errorf("%s.%s.transitions[%d] drift: %+v → %+v",
						w1.Name, stateName, i, t1, t2)
				}
				if !semanticJSONEqual(t, t1.Criterion, t2.Criterion) {
					t.Errorf("%s.%s.transitions[%d].criterion drift: %s → %s",
						w1.Name, stateName, i, t1.Criterion, t2.Criterion)
				}
				if len(t1.Processors) != len(t2.Processors) {
					t.Errorf("%s.%s.transitions[%d]: processor-count drift %d → %d",
						w1.Name, stateName, i, len(t1.Processors), len(t2.Processors))
				}
			}
		}
	}
}

// semanticJSONEqual compares two json.RawMessage values for semantic
// equality by canonicalising both through encoding/json. An absent
// (`nil` / zero-length) value is equal to JSON null and to another
// absent value.
func semanticJSONEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	norm := func(raw json.RawMessage) string {
		s := strings.TrimSpace(string(raw))
		if s == "" || s == "null" {
			return "null"
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return s // fall back to raw on decode failure; mismatch will show
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	return norm(a) == norm(b)
}
