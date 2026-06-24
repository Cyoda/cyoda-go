package e2e_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const annotatedWorkflowPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.1",
    "name": "annot-wf",
    "initialState": "S",
    "active": true,
    "annotations": { "roles": ["admin"], "label": "WF" },
    "states": {
      "S": {
        "annotations": { "ui": "start" },
        "transitions": [
          { "name": "t", "next": "S", "manual": true, "annotations": { "icon": "x" } }
        ]
      }
    }
  }]
}`

const plainWorkflowPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.1", "name": "plain-wf", "initialState": "S", "active": true,
    "states": { "S": { "transitions": [ { "name": "t", "next": "S", "manual": true } ] } }
  }]
}`

func firstWorkflow(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	wfs, ok := body["workflows"].([]any)
	if !ok || len(wfs) == 0 {
		t.Fatalf("export: expected workflows array, got %T", body["workflows"])
	}
	wf, ok := wfs[0].(map[string]any)
	if !ok {
		t.Fatalf("export: expected workflow object, got %T", wfs[0])
	}
	return wf
}

func TestWorkflowAnnotations_RoundTrip(t *testing.T) {
	const entity, version = "annot-rt", 1
	importModelE2E(t, entity, version)
	if status, body := importWorkflowE2E(t, entity, version, annotatedWorkflowPayload); status != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", status, body)
	}
	status, body := exportWorkflowE2E(t, entity, version)
	if status != http.StatusOK {
		t.Fatalf("export: expected 200, got %d", status)
	}
	wf := firstWorkflow(t, body)
	if got, want := wf["annotations"], map[string]any{"roles": []any{"admin"}, "label": "WF"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workflow annotations: got %#v, want %#v", got, want)
	}
	state := wf["states"].(map[string]any)["S"].(map[string]any)
	if got, want := state["annotations"], map[string]any{"ui": "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("state annotations: got %#v, want %#v", got, want)
	}
	tr := state["transitions"].([]any)[0].(map[string]any)
	if got, want := tr["annotations"], map[string]any{"icon": "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("transition annotations: got %#v, want %#v", got, want)
	}
}

func TestWorkflowAnnotations_AbsentOmittedOnExport(t *testing.T) {
	const entity, version = "annot-absent", 1
	importModelE2E(t, entity, version)
	if status, body := importWorkflowE2E(t, entity, version, plainWorkflowPayload); status != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", status, body)
	}
	_, body := exportWorkflowE2E(t, entity, version)
	wf := firstWorkflow(t, body)
	if _, present := wf["annotations"]; present {
		t.Errorf("absent annotations should be omitted on export, got %#v", wf["annotations"])
	}
}

func TestWorkflowAnnotations_NonObjectRejected(t *testing.T) {
	const entity, version = "annot-bad", 1
	importModelE2E(t, entity, version)
	payload := strings.Replace(annotatedWorkflowPayload, `"annotations": { "roles": ["admin"], "label": "WF" }`, `"annotations": [1,2,3]`, 1)
	status, body := importWorkflowE2E(t, entity, version, payload)
	if status != http.StatusBadRequest || !strings.Contains(body, "VALIDATION_FAILED") {
		t.Fatalf("expected 400 VALIDATION_FAILED, got %d: %s", status, body)
	}
}
