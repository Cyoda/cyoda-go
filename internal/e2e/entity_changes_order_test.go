package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestGetEntityChanges_NewestFirst pins the server's reverse-chronological
// ordering for GET /api/entity/{id}/changes (getEntityChangesMetadata).
// The server sorts changes newest-first (service.go); this test is the
// executable contract that the prose in openapi.yaml describes.
func TestGetEntityChanges_NewestFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-changes-order"
	wf := `{ "importMode": "REPLACE", "workflows": [{
		"version": "1.1", "name": "co-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}}]}`
	setupModelWithWorkflow(t, model, wf)
	id := createEntityE2E(t, model, 1, `{"name":"order-1","amount":10,"status":"draft"}`)
	up := doAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/approve", `{"name":"order-1","amount":10,"status":"approved"}`)
	if up.StatusCode != http.StatusOK {
		t.Fatalf("transition: %d: %s", up.StatusCode, readBody(t, up))
	}
	resp := doAuth(t, http.MethodGet, "/api/entity/"+id+"/changes", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntityChangesMetadata: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var changes []map[string]any
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(changes) < 2 {
		t.Fatalf("expected >=2 changes, got %d: %s", len(changes), body)
	}
	first, err := time.Parse(time.RFC3339Nano, changes[0]["timeOfChange"].(string))
	if err != nil {
		t.Fatalf("parse changes[0].timeOfChange: %v", err)
	}
	last, err := time.Parse(time.RFC3339Nano, changes[len(changes)-1]["timeOfChange"].(string))
	if err != nil {
		t.Fatalf("parse changes[last].timeOfChange: %v", err)
	}
	if !first.After(last) {
		t.Errorf("expected newest-first: changes[0].timeOfChange %v should be after changes[last].timeOfChange %v", first, last)
	}
}
