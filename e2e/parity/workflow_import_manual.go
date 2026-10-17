package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// manualWorkflow builds a workflow with two transitions out of OPEN, "go"
// and "auto"; goManual is a raw `"manual": …,` JSON member for "go", or ""
// to omit it. "auto" always sends "manual": false.
func manualWorkflow(wfName, goManual string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.5", "name": %q, "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [
					{%s"name": "go", "next": "DONE"},
					{"name": "auto", "next": "DONE", "manual": false}
				]},
				"DONE": {"transitions": []}
			}
		}]
	}`, wfName, goManual)
}

// RunWorkflowImportManualRequired: a transition's manual is required. An
// omitted or null manual is 400 VALIDATION_FAILED naming the transition, and
// the refused import leaves the stored workflows as they were; an explicit
// true or false is stored and exported as sent.
func RunWorkflowImportManualRequired(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-manual-required"
	calloutConfigModel(t, c, modelName)

	if err := c.ImportWorkflow(t, modelName, 1, manualWorkflow("manual-ok-wf", `"manual": true,`)); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	for _, tc := range []struct{ label, member string }{
		{"omitted", ""},
		{"null", `"manual": null,`},
	} {
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, manualWorkflow("manual-bad-wf", tc.member))
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] manual: expected 400, got %d; body=%s", tc.label, status, resp)
		}
		if !containsErrorCode(resp, "VALIDATION_FAILED") {
			t.Errorf("[%s] expected errorCode VALIDATION_FAILED, body=%s", tc.label, resp)
		}
		for _, want := range []string{"manual-bad-wf", `transition \"go\"`, "manual is required"} {
			if !bytes.Contains(resp, []byte(want)) {
				t.Errorf("[%s] detail must contain %s, body=%s", tc.label, want, resp)
			}
		}
	}

	raw, err := c.ExportWorkflow(t, modelName, 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	var exported struct {
		Workflows []struct {
			Name   string `json:"name"`
			States map[string]struct {
				Transitions []struct {
					Name   string `json:"name"`
					Manual *bool  `json:"manual"`
				} `json:"transitions"`
			} `json:"states"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatalf("decode export: %v; raw: %s", err, raw)
	}
	if len(exported.Workflows) != 1 || exported.Workflows[0].Name != "manual-ok-wf" {
		t.Fatalf("export must hold only manual-ok-wf, imported before the refused REPLACEs; raw: %s", raw)
	}
	want := map[string]bool{"go": true, "auto": false}
	got := exported.Workflows[0].States["OPEN"].Transitions
	if len(got) != len(want) {
		t.Fatalf("export must hold %d transitions out of OPEN; raw: %s", len(want), raw)
	}
	for _, tr := range got {
		if tr.Manual == nil || *tr.Manual != want[tr.Name] {
			t.Errorf("transition %q: exported manual = %v, want %t; raw: %s", tr.Name, tr.Manual, want[tr.Name], raw)
		}
	}
}
