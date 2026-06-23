package parity

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

const workflowRoundTripPayload = `{
	"importMode": "REPLACE",
	"workflows": [
		{
			"version": "1.1",
			"name": "round-trip-wf",
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

// RunWorkflowImportExport verifies that a workflow can be imported and
// then round-tripped back via the export endpoint.
func RunWorkflowImportExport(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wf-import-export-test"
	const modelVersion = 1

	// 1. Import model.
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test Order","amount":100,"status":"draft"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}

	// 2. Lock model.
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	// 3. Import workflow.
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowRoundTripPayload); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// 4. Export and verify the workflow matches.
	raw, err := c.ExportWorkflow(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}

	var exportBody map[string]any
	if err := json.Unmarshal(raw, &exportBody); err != nil {
		t.Fatalf("ExportWorkflow: failed to parse JSON: %v", err)
	}

	if en, _ := exportBody["entityName"].(string); en != modelName {
		t.Errorf("ExportWorkflow: expected entityName=%q, got %q", modelName, en)
	}

	workflows, ok := exportBody["workflows"].([]any)
	if !ok {
		t.Fatalf("ExportWorkflow: expected workflows array, got %T", exportBody["workflows"])
	}
	if len(workflows) != 1 {
		t.Fatalf("ExportWorkflow: expected 1 workflow, got %d", len(workflows))
	}

	wf, ok := workflows[0].(map[string]any)
	if !ok {
		t.Fatalf("ExportWorkflow: expected workflow to be an object, got %T", workflows[0])
	}
	if name, _ := wf["name"].(string); name != "round-trip-wf" {
		t.Errorf("ExportWorkflow: expected name=round-trip-wf, got %q", name)
	}
	if initialState, _ := wf["initialState"].(string); initialState != "NONE" {
		t.Errorf("ExportWorkflow: expected initialState=NONE, got %q", initialState)
	}
	if active, _ := wf["active"].(bool); !active {
		t.Errorf("ExportWorkflow: expected active=true")
	}
}

const workflowAnnotationsPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.1", "name": "annot-wf", "initialState": "NONE", "active": true,
    "annotations": { "roles": ["admin"] },
    "states": { "NONE": { "annotations": { "ui": "start" }, "transitions": [] } }
  }]
}`

// RunWorkflowAnnotationsRoundTrip verifies client-owned annotations survive
// an import → export cycle on every backend.
func RunWorkflowAnnotationsRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wf-annotations-test"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test Order","amount":100,"status":"draft"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowAnnotationsPayload); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	raw, err := c.ExportWorkflow(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("ExportWorkflow: parse: %v", err)
	}
	workflows, ok := body["workflows"].([]any)
	if !ok {
		t.Fatalf("ExportWorkflow: expected workflows array, got %T", body["workflows"])
	}
	if len(workflows) == 0 {
		t.Fatalf("ExportWorkflow: expected at least 1 workflow, got 0")
	}
	wf, ok := workflows[0].(map[string]any)
	if !ok {
		t.Fatalf("ExportWorkflow: expected workflow to be an object, got %T", workflows[0])
	}
	if got, want := wf["annotations"], map[string]any{"roles": []any{"admin"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("workflow annotations: got %#v, want %#v", got, want)
	}
	statesRaw, ok := wf["states"].(map[string]any)
	if !ok {
		t.Fatalf("ExportWorkflow: expected states to be an object, got %T", wf["states"])
	}
	state, ok := statesRaw["NONE"].(map[string]any)
	if !ok {
		t.Fatalf("ExportWorkflow: expected NONE state to be an object, got %T", statesRaw["NONE"])
	}
	if got, want := state["annotations"], map[string]any{"ui": "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("state annotations: got %#v, want %#v", got, want)
	}
}
