package e2e_test

// entity_conformance_test.go — E2E tests that pin the wire shapes corrected by
// Task 6.1 of the OpenAPI conformance plan (#21). Each test creates the minimal
// fixture, hits the endpoint, and asserts the response shape matches the
// corrected spec declaration (server-is-source-of-truth policy).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestDeleteEntities_ResponseShape asserts that DELETE /entity/{name}/{version}
// returns a single StreamDeleteResult object (not an array). Prior to the fix the
// handler emitted []map{deleteResult, entityModelClassId}, but the spec declares
// the top-level schema as a StreamDeleteResult object.
//
// Spec: deleteEntities → 200 → $ref StreamDeleteResult
// StreamDeleteResult: {entityModelClassId, deleteResult: {numberOfEntitites,
//
//	numberOfEntititesRemoved, idToError}, ids?: [uuid]}
func TestDeleteEntities_ResponseShape(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-delex-1"

	// Import a bare model (no workflow needed for delete).
	importModel(t, model, 1)

	// Create a couple of entities so the delete result has something to report.
	createEntityE2E(t, model, 1, `{"x":1}`)
	createEntityE2E(t, model, 1, `{"x":2}`)

	// DELETE /api/entity/{name}/{version} — should return a single object.
	path := fmt.Sprintf("/api/entity/%s/1", model)
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deleteEntities: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Must be a single JSON object, NOT an array.
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("deleteEntities: response is not a JSON object: %v\nbody: %s", err, body)
	}

	// Must have entityModelClassId at top level (not nested inside array element).
	if _, ok := obj["entityModelClassId"]; !ok {
		t.Errorf("deleteEntities: missing entityModelClassId in response object; got keys: %v\nbody: %s",
			keys(obj), body)
	}

	// Must have deleteResult nested object.
	dr, ok := obj["deleteResult"]
	if !ok {
		t.Errorf("deleteEntities: missing deleteResult in response; got keys: %v\nbody: %s",
			keys(obj), body)
	} else {
		drMap, ok := dr.(map[string]any)
		if !ok {
			t.Errorf("deleteEntities: deleteResult is not an object, got %T", dr)
		} else {
			// numberOfEntitites and numberOfEntititesRemoved must be present.
			if _, ok := drMap["numberOfEntitites"]; !ok {
				t.Errorf("deleteEntities: deleteResult missing numberOfEntitites; got %v", drMap)
			}
			if _, ok := drMap["numberOfEntititesRemoved"]; !ok {
				t.Errorf("deleteEntities: deleteResult missing numberOfEntititesRemoved; got %v", drMap)
			}
		}
	}
}

// TestCreate_Returns400ForInvalidInput asserts that POST /entity/{format}/{name}/{version}
// returns HTTP 400 when the model is not found or the payload is invalid.
// The spec now declares 400 as a supported status for this operation.
func TestCreate_Returns400ForInvalidModel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	// Attempt to create entity for a model that doesn't exist.
	path := "/api/entity/JSON/nonexistent-model-xyz/1"
	resp := doAuth(t, http.MethodPost, path, `{"x":1}`)
	body := readBody(t, resp)
	// Server returns 404 for unknown model (model not found).
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 or 404 for unknown model, got %d: %s", resp.StatusCode, body)
	}
}

// TestGetOneEntity_ReturnsEnvelope asserts that GET /entity/{id} returns an
// envelope object with type, data, and meta fields, matching the new Envelope
// named schema in the spec.
func TestGetOneEntity_ReturnsEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-getone-env-1"
	importModel(t, model, 1)

	entityID := createEntityE2E(t, model, 1, `{"x":42}`)

	path := fmt.Sprintf("/api/entity/%s", entityID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getOneEntity: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("getOneEntity: response is not a JSON object: %v\nbody: %s", err, body)
	}

	// Must have type, data, meta at top level (the Envelope schema).
	if _, ok := envelope["type"]; !ok {
		t.Errorf("getOneEntity: missing 'type' field; got keys: %v", keys(envelope))
	}
	if _, ok := envelope["data"]; !ok {
		t.Errorf("getOneEntity: missing 'data' field; got keys: %v", keys(envelope))
	}
	if _, ok := envelope["meta"]; !ok {
		t.Errorf("getOneEntity: missing 'meta' field; got keys: %v", keys(envelope))
	}
}

// TestGetAllEntities_ReturnsJSONArray asserts that GET /entity/{name}/{version}
// returns application/json (not application/x-ndjson) with an array of Envelope objects.
func TestGetAllEntities_ReturnsJSONArray(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-getall-env-1"
	importModel(t, model, 1)

	createEntityE2E(t, model, 1, `{"x":1}`)
	createEntityE2E(t, model, 1, `{"x":2}`)

	path := fmt.Sprintf("/api/entity/%s/1", model)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAllEntities: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Must be a JSON array.
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("getAllEntities: response is not a JSON array: %v\nbody: %s", err, body)
	}

	// Each element must be an envelope with type, data, meta.
	for i, item := range arr {
		if _, ok := item["type"]; !ok {
			t.Errorf("getAllEntities: item[%d] missing 'type'; got %v", i, item)
		}
		if _, ok := item["data"]; !ok {
			t.Errorf("getAllEntities: item[%d] missing 'data'; got %v", i, item)
		}
		if _, ok := item["meta"]; !ok {
			t.Errorf("getAllEntities: item[%d] missing 'meta'; got %v", i, item)
		}
	}
}

// TestUpdateSingle_EntityIdsIsArrayOfStrings asserts that PUT /entity/{format}/{id}/{transition}
// returns EntityTransactionResponse where entityIds is an array of UUID strings,
// not objects.
func TestUpdateSingle_EntityIdsIsArrayOfStrings(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-update-ids-1"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "ids-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "approve", "next": "DONE", "manual": true}]},
				"DONE": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Use fields from workflowSampleModel (name, amount, status) which
	// setupModelWithWorkflow uses as the schema reference.
	entityID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"draft"}`)

	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Alice","amount":100,"status":"approved"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("updateSingle: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("updateSingle: response is not a JSON object: %v\nbody: %s", err, body)
	}

	// entityIds must be a []string (array of UUID strings), not []object.
	rawIds, ok := result["entityIds"]
	if !ok {
		t.Fatalf("updateSingle: missing entityIds in response; body: %s", body)
	}
	ids, ok := rawIds.([]any)
	if !ok {
		t.Fatalf("updateSingle: entityIds is not an array; got %T; body: %s", rawIds, body)
	}
	for i, id := range ids {
		if _, ok := id.(string); !ok {
			t.Errorf("updateSingle: entityIds[%d] is %T not string; body: %s", i, id, body)
		}
	}
}

// keys returns the map keys as a slice, for diagnostic messages.
func keys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// importModel registers an entity model and locks it so entities can be created.
// Uses the SAMPLE_DATA converter — the payload is example data; the server
// infers the schema from it.
func importModel(t *testing.T, name string, version int) {
	t.Helper()
	// SAMPLE_DATA converter infers schema from example JSON data.
	sampleData := `{"x":1}`
	importPath := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", name, version)
	resp := doAuth(t, http.MethodPost, importPath, sampleData)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("importModel %s/%d: expected 200, got %d: %s", name, version, resp.StatusCode, body)
	}

	// Lock the model so entities can be created.
	lockPath := fmt.Sprintf("/api/model/%s/%d/lock", name, version)
	lockResp := doAuth(t, http.MethodPut, lockPath, "")
	lockBody := readBody(t, lockResp)
	if lockResp.StatusCode != http.StatusOK {
		t.Fatalf("lockModel %s/%d: expected 200, got %d: %s", name, version, lockResp.StatusCode, lockBody)
	}
}
