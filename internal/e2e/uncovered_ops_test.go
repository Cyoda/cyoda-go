package e2e_test

// uncovered_ops_test.go — minimal happy-path E2E tests for operations that were
// not covered by earlier tasks. Added by Task 10.1 of the OpenAPI conformance
// plan (#21). Each test issues an authenticated request, asserts 2xx, and relies
// on the validator middleware to check the response shape against the spec.
//
// Stub ops covered by issue #194 (IAM, stream-data, schema ops) are deliberately
// omitted — they return 501 Not Implemented, which the conformance report marks
// as "uncovered" (no 2xx path exercised). That is acceptable per the A+C policy.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestUncoveredOps_GetEntityStatistics covers operationId: getEntityStatistics
// GET /api/entity/stats — returns stats for all entity models.
func TestUncoveredOps_GetEntityStatistics(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-stats-global-1"
	importModel(t, model, 1)
	createEntityE2E(t, model, 1, `{"x":1}`)

	resp := doAuth(t, http.MethodGet, "/api/entity/stats", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntityStatistics: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Must be a JSON array.
	var arr []any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("getEntityStatistics: response is not a JSON array: %v\nbody: %s", err, body)
	}
}

// TestUncoveredOps_GetEntityStatisticsByState covers operationId: getEntityStatisticsByState
// GET /api/entity/stats/states — returns stats grouped by model and state.
func TestUncoveredOps_GetEntityStatisticsByState(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-stats-state-1"
	importModel(t, model, 1)
	createEntityE2E(t, model, 1, `{"x":1}`)

	resp := doAuth(t, http.MethodGet, "/api/entity/stats/states", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntityStatisticsByState: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Must be a JSON array.
	var arr []any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("getEntityStatisticsByState: response is not a JSON array: %v\nbody: %s", err, body)
	}
}

// TestUncoveredOps_DeleteSingleEntity covers operationId: deleteSingleEntity
// DELETE /api/entity/{entityId} — deletes a specific entity by UUID.
func TestUncoveredOps_DeleteSingleEntity(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-delsingle-1"
	importModel(t, model, 1)
	entityID := createEntityE2E(t, model, 1, `{"x":42}`)

	path := fmt.Sprintf("/api/entity/%s", entityID)
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deleteSingleEntity: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Response must have id and transactionId.
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("deleteSingleEntity: response is not a JSON object: %v\nbody: %s", err, body)
	}
	if _, ok := result["id"]; !ok {
		t.Errorf("deleteSingleEntity: missing 'id' field; got keys: %v", keys(result))
	}
	if _, ok := result["transactionId"]; !ok {
		t.Errorf("deleteSingleEntity: missing 'transactionId' field; got keys: %v", keys(result))
	}
}

// TestUncoveredOps_UpdateCollection covers operationId: updateCollection
// PUT /api/entity/{format}/collection — updates multiple entities in one call.
func TestUncoveredOps_UpdateCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-updatecoll-1"
	importModel(t, model, 1)

	// Create two entities whose IDs we'll collect.
	id1 := createEntityE2E(t, model, 1, `{"x":1}`)
	id2 := createEntityE2E(t, model, 1, `{"x":2}`)

	// Build the updateCollection request body.
	// Each element has id (entity UUID), payload (JSON string), and transition.
	body := fmt.Sprintf(`[
		{"id":%q,"payload":"{\"x\":10}","transition":""},
		{"id":%q,"payload":"{\"x\":20}","transition":""}
	]`, id1, id2)

	// PUT /api/entity/{format} — format=JSON for collection updates.
	resp := doAuth(t, http.MethodPut, "/api/entity/JSON", body)
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("updateCollection: expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	// Response is an array of EntityTransactionResponse-like objects.
	var arr []any
	if err := json.Unmarshal([]byte(respBody), &arr); err != nil {
		t.Fatalf("updateCollection: response is not a JSON array: %v\nbody: %s", err, respBody)
	}
}

// TestUncoveredOps_GetAvailableEntityModels covers operationId: getAvailableEntityModels
// GET /api/model/ — lists all entity models.
func TestUncoveredOps_GetAvailableEntityModels(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	// Ensure at least one model exists so the list is non-empty.
	const model = "e2e-getmodels-1"
	importModel(t, model, 1)

	resp := doAuth(t, http.MethodGet, "/api/model/", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAvailableEntityModels: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Must be a JSON array.
	var arr []any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("getAvailableEntityModels: response is not a JSON array: %v\nbody: %s", err, body)
	}
}

// TestUncoveredOps_ValidateEntityModel covers operationId: validateEntityModel
// POST /api/model/validate/{entityName}/{modelVersion} — validates a model schema.
func TestUncoveredOps_ValidateEntityModel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-validatemodel-1"
	importModelE2E(t, model, 1)

	// Submit an empty JSON body (valid for the validate endpoint; it checks
	// the model definition, not the payload).
	path := fmt.Sprintf("/api/model/validate/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, `{}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("validateEntityModel: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Response must be EntityModelActionResultDto: {success, message, ...}
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("validateEntityModel: response is not a JSON object: %v\nbody: %s", err, body)
	}
	if _, ok := result["success"]; !ok {
		t.Errorf("validateEntityModel: missing 'success' field; got keys: %v", keys(result))
	}
}

// TestUncoveredOps_DeleteEntityModel covers operationId: deleteEntityModel
// DELETE /api/model/{entityName}/{modelVersion} — deletes an unlocked model with no entities.
func TestUncoveredOps_DeleteEntityModel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	// Use a unique model name to avoid collision with other tests.
	const model = "e2e-delmodel-1"

	// Import model (creates it in UNLOCKED state), don't lock it.
	importModelE2E(t, model, 1)

	// Delete the model — must be unlocked and have no entities.
	path := fmt.Sprintf("/api/model/%s/1", model)
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deleteEntityModel: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("deleteEntityModel: response is not a JSON object: %v\nbody: %s", err, body)
	}
	if _, ok := result["success"]; !ok {
		t.Errorf("deleteEntityModel: missing 'success' field; got keys: %v", keys(result))
	}
}

// TestUncoveredOps_UnlockEntityModel covers operationId: unlockEntityModel
// PUT /api/model/{entityName}/{modelVersion}/unlock — unlocks a locked model.
func TestUncoveredOps_UnlockEntityModel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-unlockmodel-1"
	// Import and lock — then unlock.
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	path := fmt.Sprintf("/api/model/%s/1/unlock", model)
	resp := doAuth(t, http.MethodPut, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlockEntityModel: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("unlockEntityModel: response is not a JSON object: %v\nbody: %s", err, body)
	}
	if _, ok := result["success"]; !ok {
		t.Errorf("unlockEntityModel: missing 'success' field; got keys: %v", keys(result))
	}
}
