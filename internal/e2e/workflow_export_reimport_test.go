package e2e_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestWorkflow_ExportReimport: an export is not an import body. Posted back
// unchanged it is refused with 400 BAD_REQUEST, naming `entityName`, a member
// the import does not declare. Its `workflows`, under an `importMode`, import
// cleanly — every field the export writes is one the import accepts — and the
// model exports the same workflows again.
func TestWorkflow_ExportReimport(t *testing.T) {
	entityName := "e2e-wf-reimport-" + randSuffix(t)
	const modelVersion = 1
	importModelE2E(t, entityName, modelVersion)
	lockModelE2E(t, entityName, modelVersion)

	const original = `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "reimport-wf", "desc": "round trip", "initialState": "NONE", "active": true,
			"annotations": {"owner": "team-a"},
			"states": {
				"NONE": {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [
					{"name": "approve", "next": "APPROVED", "manual": true,
					 "criterion": {"type": "simple", "jsonPath": "$.amount", "operatorType": "GREATER_THAN", "value": 10},
					 "criterionAnnotations": {"description": "large orders only"},
					 "processors": [{"type": "externalized", "name": "notify", "executionMode": "ASYNC_NEW_TX",
					                 "config": {"calculationNodesTags": "notify", "responseTimeoutMs": 5000}}]},
					{"name": "expire", "next": "EXPIRED", "manual": false, "schedule": {"delayMs": 60000}}
				]},
				"APPROVED": {},
				"EXPIRED": {}
			}
		}]
	}`
	if status, body := importWorkflowE2E(t, entityName, modelVersion, original); status != http.StatusOK {
		t.Fatalf("initial import: expected 200, got %d: %s", status, body)
	}
	status, exported := exportWorkflowE2E(t, entityName, modelVersion)
	if status != http.StatusOK {
		t.Fatalf("export: expected 200, got %d", status)
	}

	verbatim, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	status, body := importWorkflowE2E(t, entityName, modelVersion, string(verbatim))
	if status != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") || !strings.Contains(body, "entityName") {
		t.Fatalf("export posted back unchanged: expected 400 BAD_REQUEST naming entityName, got %d: %s", status, body)
	}

	reimport, err := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": exported["workflows"]})
	if err != nil {
		t.Fatalf("marshal re-import body: %v", err)
	}
	if status, body := importWorkflowE2E(t, entityName, modelVersion, string(reimport)); status != http.StatusOK {
		t.Fatalf("re-import of the exported workflows: expected 200, got %d: %s", status, body)
	}
	status, again := exportWorkflowE2E(t, entityName, modelVersion)
	if status != http.StatusOK {
		t.Fatalf("second export: expected 200, got %d", status)
	}
	if !reflect.DeepEqual(exported["workflows"], again["workflows"]) {
		t.Errorf("workflows changed across export → import → export:\nfirst:  %v\nsecond: %v", exported["workflows"], again["workflows"])
	}
}
