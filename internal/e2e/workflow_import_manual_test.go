package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// manualWorkflow builds a one-transition workflow whose transition carries
// manualMember (a raw `"manual": …,` JSON member, or "" to omit it).
func manualWorkflow(name, manualMember string) string {
	return fmt.Sprintf(`{
	  "importMode": "REPLACE",
	  "workflows": [{
	    "version": "1.1", "name": %q, "initialState": "S", "active": true,
	    "states": {
	      "S": { "transitions": [ { %s"name": "t", "next": "Done" } ] },
	      "Done": {}
	    }
	  }]
	}`, name, manualMember)
}

// TestWorkflowImport_ManualRequired covers the import rule for a transition's
// manual through the HTTP stack: an omitted or null manual is refused with
// 400 VALIDATION_FAILED, and an explicit true or false is stored and
// exported as sent.
func TestWorkflowImport_ManualRequired(t *testing.T) {
	for _, tc := range []struct {
		name, entity, manualMember string
	}{
		{"omitted_400", "wf-manual-omitted", ""},
		{"null_400", "wf-manual-null", `"manual": null,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importModelE2E(t, tc.entity, 1)
			importRejection(t, tc.entity, manualWorkflow(tc.entity+"-wf", tc.manualMember),
				tc.entity+"-wf", `state "S"`, `transition "t"`, "manual is required (true or false)")
		})
	}

	for _, tc := range []struct {
		name, entity string
		manual       bool
	}{
		{"true_stored", "wf-manual-true", true},
		{"false_stored", "wf-manual-false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importModelE2E(t, tc.entity, 1)
			member := fmt.Sprintf(`"manual": %t,`, tc.manual)
			status, body := importWorkflowE2E(t, tc.entity, 1, manualWorkflow(tc.entity+"-wf", member))
			if status != http.StatusOK {
				t.Fatalf("import status = %d; want 200; body: %s", status, body)
			}

			status, exported := exportWorkflowE2E(t, tc.entity, 1)
			if status != http.StatusOK {
				t.Fatalf("export status = %d; want 200; body: %v", status, exported)
			}
			wfs, _ := exported["workflows"].([]any)
			if len(wfs) != 1 {
				t.Fatalf("exported workflows = %v; want one", exported["workflows"])
			}
			wf, _ := wfs[0].(map[string]any)
			states, _ := wf["states"].(map[string]any)
			s, _ := states["S"].(map[string]any)
			trs, _ := s["transitions"].([]any)
			if len(trs) != 1 {
				t.Fatalf("exported transitions of S = %v; want one", s["transitions"])
			}
			tr, _ := trs[0].(map[string]any)
			if got, present := tr["manual"]; !present || got != tc.manual {
				t.Errorf("exported manual = %v (present=%v); want %t", got, present, tc.manual)
			}
		})
	}
}
