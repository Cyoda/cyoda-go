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

// TestExport_UnknownModel_Returns404_ModelNotFound covers issue #257 M2: the
// export handler must distinguish "model does not exist" (MODEL_NOT_FOUND)
// from "model exists but has no workflows" (WORKFLOW_NOT_FOUND). The import
// handler already enforces the same distinction (see
// TestImport_UnknownModel_Returns404); export was left behind.
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

// TestImport_LowercaseImportMode covers issue #257 M5: importMode parsing is
// case-insensitive — the handler upper-cases the incoming value before
// dispatch. This test pins the behaviour at the wire boundary so the OpenAPI
// description (which now declares case-insensitivity) and the implementation
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

// TestImport_UnknownImportMode covers issue #257 M5: an importMode value
// outside the documented enum must return 400 BAD_REQUEST. The existing
// happy-path tests exercised only the success branch; the rejection branch
// was unguarded by any test until now.
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
