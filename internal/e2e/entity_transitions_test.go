package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// TestFetchEntityTransitions exercises operationId: fetchEntityTransitions
// GET /platform-api/entity/fetch/transitions?entityClass=<Name>.<Version>&entityId=<uuid>
//
// This endpoint is routed outside the generated ServerInterface dispatch —
// it is registered on the inner mux at /platform-api/entity/fetch/transitions
// and is reachable as /api/platform-api/entity/fetch/transitions when the
// app is mounted under the /api context path.
//
// The test registers a model+workflow, creates an entity that auto-transitions
// from NONE to CREATED, then calls fetchEntityTransitions and asserts a 200
// response whose body is a JSON array.
func TestFetchEntityTransitions(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-fetch-tr-1"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "fetch-tr-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init",    "next": "CREATED",  "manual": false}]},
				"CREATED": {"transitions": [
					{"name": "approve", "next": "APPROVED", "manual": true},
					{"name": "reject",  "next": "REJECTED", "manual": true}
				]},
				"APPROVED": {},
				"REJECTED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"test","amount":1,"status":"draft"}`)
	// The entity auto-transitions NONE→CREATED via the automated "init"
	// transition; fetchEntityTransitions now returns the manual transitions
	// available from the CREATED state (approve, reject).

	entityClass := url.QueryEscape(fmt.Sprintf("%s.1", model))
	path := fmt.Sprintf("/api/platform-api/entity/fetch/transitions?entityClass=%s&entityId=%s",
		entityClass, entityID)

	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetchEntityTransitions: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Response must be a JSON array (TransitionNameList).
	var result []any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("fetchEntityTransitions: expected JSON array; parse error: %v; body: %s", err, body)
	}
}
