package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestWorkflowImport_UnstorableText_400: a workflow whose text holds U+0000 —
// here a transition name — is refused at import with 400 BAD_REQUEST, as an
// entity payload holding it is. The workflow's names reach the state-machine
// audit trail (WORKFLOW_FOUND, TRANSITION_MAKE, PAUSE_FOR_PROCESSING), which
// cannot hold U+0000 on PostgreSQL.
func TestWorkflowImport_UnstorableText_400(t *testing.T) {
	const model = "e2e-wf-import-unstorable"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "unstorable-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "in\u0000it", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	if status != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") {
		t.Fatalf("expected 400 BAD_REQUEST for a workflow holding U+0000, got %d: %s", status, body)
	}
	if strings.ContainsRune(body, 0) {
		t.Errorf("the response echoes the raw U+0000: %q", body)
	}
	if _, exported := exportWorkflowE2E(t, model, 1); strings.Contains(fmt.Sprint(exported), "unstorable-wf") {
		t.Errorf("the refused workflow was saved: %v", exported)
	}
}
