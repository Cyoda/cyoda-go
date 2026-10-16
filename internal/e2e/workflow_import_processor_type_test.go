package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// processorTypeWorkflow builds a one-transition workflow whose processor
// carries typeMember (a raw `"type": …,` JSON member, or "" to omit it).
func processorTypeWorkflow(name, typeMember string) string {
	return fmt.Sprintf(`{
	  "importMode": "REPLACE",
	  "workflows": [{
	    "version": "1.1", "name": %q, "initialState": "S", "active": true,
	    "states": {
	      "S": { "transitions": [ { "name": "t", "next": "Done", "manual": true,
	        "processors": [ { %s"name": "p", "executionMode": "SYNC",
	          "config": { "calculationNodesTags": "workers" } } ] } ] },
	      "Done": {}
	    }
	  }]
	}`, name, typeMember)
}

// TestWorkflowImport_ProcessorType covers the import rule for a processor's
// type through the HTTP stack: a value other than externalized is refused
// with 400 VALIDATION_FAILED, and an omitted type is stored and exported as
// externalized.
func TestWorkflowImport_ProcessorType(t *testing.T) {
	t.Run("internalized_400", func(t *testing.T) {
		const entity = "wf-proc-type-internalized"
		importModelE2E(t, entity, 1)
		importRejection(t, entity, processorTypeWorkflow("internalized-wf", `"type": "internalized",`),
			"internalized-wf", `state "S"`, `transition "t"`, `processor "p"`,
			`unknown type "internalized"`, "allowed: externalized")
	})

	t.Run("banana_400", func(t *testing.T) {
		const entity = "wf-proc-type-banana"
		importModelE2E(t, entity, 1)
		importRejection(t, entity, processorTypeWorkflow("banana-wf", `"type": "banana",`),
			"banana-wf", `processor "p"`, `unknown type "banana"`, "allowed: externalized")
	})

	t.Run("omitted_stored_as_externalized", func(t *testing.T) {
		const entity = "wf-proc-type-omitted"
		importModelE2E(t, entity, 1)
		status, body := importWorkflowE2E(t, entity, 1, processorTypeWorkflow("omitted-wf", ""))
		if status != http.StatusOK {
			t.Fatalf("import status = %d; want 200; body: %s", status, body)
		}

		status, exported := exportWorkflowE2E(t, entity, 1)
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
		procs, _ := tr["processors"].([]any)
		if len(procs) != 1 {
			t.Fatalf("exported processors = %v; want one", tr["processors"])
		}
		proc, _ := procs[0].(map[string]any)
		if got, present := proc["type"]; !present || got != "externalized" {
			t.Errorf("exported processor type = %v (present=%v); want \"externalized\"", got, present)
		}
	})
}
