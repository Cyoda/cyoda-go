package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound exercises
// the explicit-fire-of-a-scheduled-transition rejection path end-to-end through
// the full HTTP stack (#259). The validator accepts the shape-coherent
// scheduled transition (Schedule.DelayMs > 0, manual=false), cascade silently
// skips it on entity creation (no other automated exit), and a client-issued
// PUT against the transition by name returns 400 TRANSITION_NOT_FOUND with a
// message containing "scheduled transitions are not yet implemented". The
// entity stays in the source state.
//
// Mirrors the Disabled-transition precedent (TestEntityLifecycle_DisabledTransition):
// the transition exists in config but is not currently dispatchable from the
// caller's POV.
func TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound(t *testing.T) {
	const model = "e2e-scheduled-explicit-fire"

	// 1+2: Import model and workflow with a single scheduled, non-manual
	// transition out of the initial state. setupModelWithWorkflow asserts
	// 200 on workflow import — pins that the validator accepts shape-coherent
	// scheduled transitions.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "sched-explicit-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 1000}}]},
				"Closed": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// 3: Create entity instance. Cascade silently skips the scheduled
	// transition (no other automated exit), so the entity rests in Open.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if s := getEntityState(t, entityID); s != "Open" {
		t.Fatalf("expected entity to rest in initial state Open (cascade skips scheduled); got %q", s)
	}

	// 4: Fire AutoClose by name. Expect 400 TRANSITION_NOT_FOUND with the
	// message naming the rejection cause.
	path := fmt.Sprintf("/api/entity/JSON/%s/AutoClose", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test Order","amount":100,"status":"draft"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 TRANSITION_NOT_FOUND firing scheduled transition; got %d: %s", resp.StatusCode, body)
	}

	// Error response is RFC 9457 problem+json with properties.errorCode
	// (per common.WriteError) — same shape as TestGroupedStats_E2E_ValidationError.
	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Title      string         `json:"title"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, body)
	}
	if pd.Status != http.StatusBadRequest {
		t.Errorf("ProblemDetail.status: got %d, want 400", pd.Status)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "TRANSITION_NOT_FOUND" {
		t.Errorf("properties.errorCode: got %q, want TRANSITION_NOT_FOUND; body=%s", code, body)
	}
	// The "scheduled transitions are not yet implemented" substring may appear
	// in detail, title, or another problem-detail field — assert against the
	// raw body so we're robust to where common.WriteError surfaces the wrapped
	// error string.
	if !strings.Contains(body, "scheduled transitions are not yet implemented") {
		t.Errorf("expected response body to contain rejection cause %q; got: %s",
			"scheduled transitions are not yet implemented", body)
	}

	// 5: Entity must remain in the source state after rejection.
	if s := getEntityState(t, entityID); s != "Open" {
		t.Errorf("expected entity to remain in Open after rejected explicit-fire; got %q", s)
	}
}
