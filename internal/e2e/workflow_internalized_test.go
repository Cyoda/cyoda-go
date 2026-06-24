package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_InternalizedRejection_ManualTransitionReturns400 verifies the
// full HTTP path: import a workflow whose transition has a Type:
// "internalized" processor → import succeeds (no Type-axis validation at
// handler) → create an entity (no cascade) → fire the manual transition
// → 400 WORKFLOW_FAILED with the rejection sub-string → entity stays in
// the source state.
func TestE2E_InternalizedRejection_ManualTransitionReturns400(t *testing.T) {
	const model = "e2e-internalized-reject"

	// (1) Import workflow with Type: "internalized" processor on a
	// Manual=true transition. setupModelWithWorkflow combines model
	// import + lock + workflow import. The workflow has no cascade out
	// of the initial state, so entity creation lands in NONE without
	// firing the internalized processor.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "internalized-wf",
			"initialState": "NONE",
			"active": true,
			"states": {
				"NONE": {
					"transitions": [{
						"name": "fire",
						"next": "DONE",
						"manual": true,
						"processors": [{
							"type": "internalized",
							"name": "internal-proc",
							"executionMode": "SYNC"
						}]
					}]
				},
				"DONE": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// (2) Create an entity instance — lands in NONE (no automatic
	// transitions out of the initial state, so no cascade fires).
	entityID := createEntityE2E(t, model, 1, `{"name":"test"}`)
	if s := getEntityState(t, entityID); s != "NONE" {
		t.Fatalf("entity initial state = %q, want NONE", s)
	}

	// (3) Fire the manual transition via PUT /api/entity/{format}/{id}/{transition}.
	// Expect 400 WORKFLOW_FAILED with the rejection sub-string.
	path := fmt.Sprintf("/api/entity/JSON/%s/fire", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"test"}`)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("transition fire: status=%d (want 400), body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "WORKFLOW_FAILED") {
		t.Errorf("response body missing WORKFLOW_FAILED code: %s", body)
	}
	if !strings.Contains(body, `execution type \"internalized\" is not yet implemented`) &&
		!strings.Contains(body, `execution type "internalized" is not yet implemented`) {
		t.Errorf("response body missing rejection sub-string: %s", body)
	}

	// (4) Confirm the entity stayed in the source state — the failed
	// transition must not have advanced it to DONE.
	if s := getEntityState(t, entityID); s != "NONE" {
		t.Errorf("entity state after failed transition = %q, want NONE (source state preserved)", s)
	}
}
