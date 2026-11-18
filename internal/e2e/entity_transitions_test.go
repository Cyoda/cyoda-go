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
// It is served by the generated router and is reachable as
// /api/platform-api/entity/fetch/transitions when the app is mounted under
// the /api context path.
//
// The test registers a model+workflow, creates an entity that auto-transitions
// from NONE to CREATED, then calls fetchEntityTransitions and
// GET /entity/{entityId}/transitions. Both list the transitions a client can
// name on a save from CREATED, in declaration order: the manual ones and the
// automated one whose criterion did not match, but not the disabled one and
// not the scheduled one (naming either is refused).
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
					{"name": "approve",  "next": "APPROVED", "manual": true},
					{"name": "parked",   "next": "REJECTED", "manual": true, "disabled": true},
					{"name": "expire",   "next": "REJECTED", "manual": false, "schedule": {"delayMs": 3600000}},
					{"name": "escalate", "next": "APPROVED", "manual": false,
					 "criterion": {"type": "simple", "jsonPath": "$.amount", "operatorType": "GREATER_THAN", "value": 1000}},
					{"name": "reject",   "next": "REJECTED", "manual": true}
				]},
				"APPROVED": {},
				"REJECTED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"test","amount":1,"status":"draft"}`)
	// The entity auto-transitions NONE→CREATED via the automated "init"
	// transition and stays there: escalate's criterion does not match and
	// expire is scheduled.

	entityClass := url.QueryEscape(fmt.Sprintf("%s.1", model))
	path := fmt.Sprintf("/api/platform-api/entity/fetch/transitions?entityClass=%s&entityId=%s",
		entityClass, entityID)

	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetchEntityTransitions: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Response is a TransitionNameList (JSON array of strings).
	want := []string{"approve", "escalate", "reject"}
	assertTransitionList(t, "fetchEntityTransitions", body, want)

	resp = doAuth(t, http.MethodGet, "/api/entity/"+entityID+"/transitions", "")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /entity/{id}/transitions: expected 200, got %d: %s", resp.StatusCode, body)
	}
	assertTransitionList(t, "GET /entity/{id}/transitions", body, want)
}

// assertTransitionList: body is a JSON array of exactly want, in order.
func assertTransitionList(t *testing.T, what, body string, want []string) {
	t.Helper()
	var got []string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%s: expected JSON array of strings; parse error: %v; body: %s", what, err, body)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: got %v, want %v (not disabled, not scheduled, whatever manual)", what, got, want)
	}
}
