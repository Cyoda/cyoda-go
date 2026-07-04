package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestGetAllEntities_AsAt asserts the model-scoped list read honours
// pointInTime (E3): a list as-at a time before an update reflects the
// pre-update state, and meta.pointInTime echoes the requested time.
func TestGetAllEntities_AsAt(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-getall-asat"
	wf := `{ "importMode": "REPLACE", "workflows": [{
		"version": "1.1", "name": "asat-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}}]}`
	setupModelWithWorkflow(t, model, wf)

	id := createEntityE2E(t, model, 1, `{"name":"Bob","status":"draft"}`)
	time.Sleep(10 * time.Millisecond)
	midpoint := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(10 * time.Millisecond)

	// Update via manual transition so state + data change after midpoint.
	up := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/approve", id), `{"name":"Bob","status":"approved"}`)
	if up.StatusCode != http.StatusOK {
		t.Fatalf("transition: %d: %s", up.StatusCode, readBody(t, up))
	}

	// List as-at midpoint — must show CREATED (pre-update) + meta.pointInTime.
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1?pointInTime=%s", model, midpoint), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAllEntities as-at: %d: %s", resp.StatusCode, body)
	}
	var envs []map[string]any
	if err := json.Unmarshal([]byte(body), &envs); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(envs) != 1 {
		t.Fatalf("expected 1 entity as-at midpoint, got %d", len(envs))
	}
	meta := envs[0]["meta"].(map[string]any)
	if meta["state"] != "CREATED" {
		t.Errorf("as-at state = %v, want CREATED (pre-update)", meta["state"])
	}
	if meta["pointInTime"] == nil {
		t.Error("meta.pointInTime not populated on as-at list read")
	}
}
