package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- Test 8.1: Entity update (PUT) ---

func TestEntityLifecycle_Update(t *testing.T) {
	const model = "e2e-lifecycle-1"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":50,"status":"draft"}`)

	// Update via loopback (PUT without transition).
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Alice","amount":75,"status":"updated"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify data changed.
	data := getEntityData(t, entityID, "")
	if data["status"] != "updated" {
		t.Errorf("expected status=updated, got %v", data["status"])
	}
	amount, _ := data["amount"].(float64)
	if amount != 75 {
		t.Errorf("expected amount=75, got %v", amount)
	}
}

// --- Test 8.2: Entity update with manual transition ---

func TestEntityLifecycle_UpdateWithTransition(t *testing.T) {
	const model = "e2e-lifecycle-2"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc2-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Bob","amount":100,"status":"draft"}`)

	if s := getEntityState(t, entityID); s != "CREATED" {
		t.Fatalf("expected CREATED, got %s", s)
	}

	// Manual transition via PUT /entity/{format}/{entityId}/{transition}.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Bob","amount":100,"status":"approved"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transition: expected 200, got %d: %s", resp.StatusCode, body)
	}

	if s := getEntityState(t, entityID); s != "APPROVED" {
		t.Errorf("expected APPROVED after transition, got %s", s)
	}
}

// --- Test 8.3: Get available transitions ---

func TestEntityLifecycle_AvailableTransitions(t *testing.T) {
	const model = "e2e-lifecycle-3"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc3-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [
					{"name": "approve", "next": "APPROVED", "manual": true},
					{"name": "reject", "next": "REJECTED", "manual": true}
				]},
				"APPROVED": {},
				"REJECTED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Carol","amount":50,"status":"draft"}`)

	// GET /entity/{entityId}/transitions
	path := fmt.Sprintf("/api/entity/%s/transitions", entityID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transitions: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var transitions []string
	if err := json.Unmarshal([]byte(body), &transitions); err != nil {
		// Might be array of objects.
		var transObjs []map[string]any
		if err2 := json.Unmarshal([]byte(body), &transObjs); err2 != nil {
			t.Fatalf("failed to parse transitions response: %v\nbody: %s", err, body)
		}
		for _, obj := range transObjs {
			if name, ok := obj["name"].(string); ok {
				transitions = append(transitions, name)
			}
		}
	}

	if len(transitions) < 2 {
		t.Errorf("expected at least 2 transitions (approve, reject), got %d: %s", len(transitions), body)
	}
}

// --- Test 8.4: Transition on non-existent entity ---

func TestEntityLifecycle_TransitionNotFound(t *testing.T) {
	fakeID := "00000000-0000-0000-0000-000000000000"
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", fakeID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Nobody"}`)
	readBody(t, resp)

	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 404 or 400 for non-existent entity transition, got %d", resp.StatusCode)
	}
}

// --- Test 8.5: Invalid transition name ---

func TestEntityLifecycle_InvalidTransition(t *testing.T) {
	const model = "e2e-lifecycle-5"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc5-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Dave","amount":50,"status":"draft"}`)

	// Try a transition that doesn't exist.
	path := fmt.Sprintf("/api/entity/JSON/%s/nonexistent", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Dave","amount":50,"status":"draft"}`)
	body := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error for invalid transition, got 200: %s", body)
	}
}

// --- Test 8.6: Disabled transition ---

func TestEntityLifecycle_DisabledTransition(t *testing.T) {
	const model = "e2e-lifecycle-6"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc6-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [
					{"name": "approve", "next": "APPROVED", "manual": true, "disabled": true}
				]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Eve","amount":50,"status":"draft"}`)

	// Try the disabled transition.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Eve","amount":50,"status":"draft"}`)
	body := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error for disabled transition, got 200: %s", body)
	}

	// Entity should still be in CREATED.
	if s := getEntityState(t, entityID); s != "CREATED" {
		t.Errorf("expected CREATED after disabled transition attempt, got %s", s)
	}
}

// --- Test 8.7: Entity version history (changes metadata) ---

func TestEntityLifecycle_VersionHistory(t *testing.T) {
	const model = "e2e-lifecycle-7"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc7-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Frank","amount":50,"status":"v1"}`)

	// Update entity.
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Frank","amount":75,"status":"v2"}`)
	readBody(t, resp)

	// Get changes metadata.
	changesPath := fmt.Sprintf("/api/entity/%s/changes", entityID)
	changesResp := doAuth(t, http.MethodGet, changesPath, "")
	changesBody := readBody(t, changesResp)
	if changesResp.StatusCode != http.StatusOK {
		t.Fatalf("changes: expected 200, got %d: %s", changesResp.StatusCode, changesBody)
	}

	var changes []map[string]any
	if err := json.Unmarshal([]byte(changesBody), &changes); err != nil {
		t.Fatalf("failed to parse changes: %v\nbody: %s", err, changesBody)
	}

	// Should have at least 2 versions (CREATED + UPDATED).
	if len(changes) < 2 {
		t.Errorf("expected >= 2 change entries, got %d: %s", len(changes), changesBody)
	}
}

// --- Test 8.8: Temporal as-at after multiple updates ---

func TestEntityLifecycle_TemporalAsAt(t *testing.T) {
	const model = "e2e-lifecycle-8"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc8-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Grace","amount":50,"status":"v1"}`)

	// Record time between creates.
	time.Sleep(50 * time.Millisecond)
	midpoint := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(50 * time.Millisecond)

	// Update entity.
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Grace","amount":100,"status":"v2"}`)
	readBody(t, resp)

	// Get current version — should be v2.
	currentData := getEntityData(t, entityID, "")
	if currentData["status"] != "v2" {
		t.Errorf("current: expected status=v2, got %v", currentData["status"])
	}

	// Get as-at midpoint — should be v1.
	histData := getEntityData(t, entityID, midpoint)
	if histData["status"] != "v1" {
		t.Errorf("as-at %s: expected status=v1, got %v", midpoint, histData["status"])
	}
}

// --- Test 8.8b: Temporal GET by transactionId (issue #150) ---

// TestEntityLifecycle_TemporalByTransactionID exercises GET /entity/{id}
// ?transactionId=<tx> through the full HTTP stack. Issue #150 fixed the
// silent-drop of the query param; this test pins the behavior end-to-end:
//   - the create-time txID returns the create snapshot (status=v1) even after
//     two updates,
//   - a bogus txID returns 404 ENTITY_NOT_FOUND (dictionary 12/neg/05).
func TestEntityLifecycle_TemporalByTransactionID(t *testing.T) {
	const model = "e2e-lifecycle-8b"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc8b-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID, createTxID := createEntityE2EWithTxID(t, model, 1, `{"name":"Henry","amount":50,"status":"v1"}`)

	// Update twice — v1 → v2 → v3.
	updEntityE2E := func(payload string) {
		t.Helper()
		path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
		resp := doAuth(t, http.MethodPut, path, payload)
		readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: got %d", resp.StatusCode)
		}
	}
	updEntityE2E(`{"name":"Henry","amount":75,"status":"v2"}`)
	updEntityE2E(`{"name":"Henry","amount":100,"status":"v3"}`)

	// Sanity: latest is v3.
	if data := getEntityData(t, entityID, ""); data["status"] != "v3" {
		t.Fatalf("sanity: latest status=%v, want v3", data["status"])
	}

	// At create-tx: v1.
	status, body := getEntityAtTransactionID(t, entityID, createTxID)
	if status != http.StatusOK {
		t.Fatalf("GET ?transactionId=%s: got %d, want 200; body: %s", createTxID, status, body)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode at-tx envelope: %v", err)
	}
	atTxData, _ := envelope["data"].(map[string]any)
	if atTxData["status"] != "v1" {
		t.Errorf("at createTxID: status=%v, want v1", atTxData["status"])
	}
	atTxMeta, _ := envelope["meta"].(map[string]any)
	if got, _ := atTxMeta["transactionId"].(string); got != createTxID {
		t.Errorf("at-tx envelope meta.transactionId: got %q, want %q", got, createTxID)
	}

	// Bogus txID: 404 ENTITY_NOT_FOUND.
	status, body = getEntityAtTransactionID(t, entityID, "00000000-0000-0000-0000-000000000000")
	if status != http.StatusNotFound {
		t.Fatalf("bogus tx: got %d, want 404; body: %s", status, body)
	}
	if !strings.Contains(body, "ENTITY_NOT_FOUND") {
		t.Errorf("bogus tx: body missing ENTITY_NOT_FOUND code; got: %s", body)
	}
}

// --- Test 8.9: Batch entity creation ---

func TestEntityLifecycle_BatchCreate(t *testing.T) {
	const model = "e2e-lifecycle-9"

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc9-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	// Batch create via POST /entity/{format} (createCollection).
	batchPayload := fmt.Sprintf(`[
		{"model":{"name":"%s","version":1},"payload":"{\"name\":\"Batch1\",\"amount\":10,\"status\":\"new\"}"},
		{"model":{"name":"%s","version":1},"payload":"{\"name\":\"Batch2\",\"amount\":20,\"status\":\"new\"}"},
		{"model":{"name":"%s","version":1},"payload":"{\"name\":\"Batch3\",\"amount\":30,\"status\":\"new\"}"}
	]`, model, model, model)
	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", batchPayload)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch create: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify at least 3 entities created.
	var results []map[string]any
	json.Unmarshal([]byte(body), &results)

	totalIDs := 0
	for _, r := range results {
		ids, _ := r["entityIds"].([]any)
		totalIDs += len(ids)
	}
	if totalIDs < 3 {
		t.Errorf("expected >= 3 entity IDs from batch, got %d: %s", totalIDs, body)
	}
}

// --- Test 8.10: Entity statistics ---

func TestEntityLifecycle_Statistics(t *testing.T) {
	const model = "e2e-lifecycle-10"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "lc10-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Create 3 entities.
	entityID1 := createEntityE2E(t, model, 1, `{"name":"S1","amount":10,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"S2","amount":20,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"S3","amount":30,"status":"new"}`)

	// Approve one.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID1)
	resp := doAuth(t, http.MethodPut, path, `{"name":"S1","amount":10,"status":"approved"}`)
	readBody(t, resp)

	// Get statistics for model.
	statsPath := fmt.Sprintf("/api/entity/stats/%s/%d", model, 1)
	statsResp := doAuth(t, http.MethodGet, statsPath, "")
	statsBody := readBody(t, statsResp)
	if statsResp.StatusCode != http.StatusOK {
		t.Fatalf("stats: expected 200, got %d: %s", statsResp.StatusCode, statsBody)
	}

	var stats map[string]any
	json.Unmarshal([]byte(statsBody), &stats)
	count, _ := stats["count"].(float64)
	if count < 3 {
		t.Errorf("expected count >= 3, got %v", count)
	}

	// Get state statistics.
	stateStatsPath := fmt.Sprintf("/api/entity/stats/states/%s/%d", model, 1)
	stateResp := doAuth(t, http.MethodGet, stateStatsPath, "")
	stateBody := readBody(t, stateResp)
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("state stats: expected 200, got %d: %s", stateResp.StatusCode, stateBody)
	}

	// Should have entries for CREATED and APPROVED states.
	if !strings.Contains(stateBody, "CREATED") {
		t.Error("state stats: expected CREATED state in response")
	}
	if !strings.Contains(stateBody, "APPROVED") {
		t.Error("state stats: expected APPROVED state in response (one entity was approved)")
	}

	// State filter: request only CREATED — APPROVED must not appear.
	filteredPath := fmt.Sprintf("/api/entity/stats/states/%s/%d?states=CREATED", model, 1)
	filteredResp := doAuth(t, http.MethodGet, filteredPath, "")
	filteredBody := readBody(t, filteredResp)
	if filteredResp.StatusCode != http.StatusOK {
		t.Fatalf("filtered state stats: expected 200, got %d: %s", filteredResp.StatusCode, filteredBody)
	}
	if !strings.Contains(filteredBody, "CREATED") {
		t.Errorf("filtered state stats: expected CREATED in response, got %s", filteredBody)
	}
	if strings.Contains(filteredBody, "APPROVED") {
		t.Errorf("filtered state stats: APPROVED must NOT appear when filter is CREATED only, got %s", filteredBody)
	}
}

// --- Precision: large-int round-trip through the postgres-backed stack ---

// TestEntityLifecycle_PreservesLargeIntPrecision verifies that an integer
// JSON field whose magnitude exceeds 2^53 round-trips through the full
// postgres-backed HTTP stack without precision loss. Bare json.Unmarshal
// would decode such an integer into a float64 and round it to the nearest
// representable double; the precision-preserving path must keep the
// literal exactly. Gate 2 coverage for the user-facing JSON-precision
// behaviour change in this PR.
func TestEntityLifecycle_PreservesLargeIntPrecision(t *testing.T) {
	const model = "e2e-precision-bigint"

	// Sample data must include the `id` field so the inferred schema
	// accepts it on entity create (unknown fields are rejected). The
	// seed value 9007199254740993 exceeds 2^31 so the inferred schema
	// is LEAF(LONG), not LEAF(INTEGER); validation under strict
	// ChangeLevel accepts same-type LONG values without requiring
	// schema extension.
	sample := `{"id":9007199254740993,"name":"sample","amount":50,"status":"new"}`
	importPath := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", model, 1)
	if r := doAuth(t, http.MethodPost, importPath, sample); r.StatusCode != http.StatusOK {
		t.Fatalf("import model: %d: %s", r.StatusCode, readBody(t, r))
	}
	lockPath := fmt.Sprintf("/api/model/%s/%d/lock", model, 1)
	if r := doAuth(t, http.MethodPut, lockPath, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("lock model: %d: %s", r.StatusCode, readBody(t, r))
	}
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "precision-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	status, body := importWorkflowE2E(t, model, 1, wf)
	if status != http.StatusOK {
		t.Fatalf("workflow import: %d: %s", status, body)
	}

	// 9007199254740993 == 2^53 + 1, the smallest positive integer that is
	// not exactly representable as a float64. A precision-losing path
	// rounds it to 9007199254740992 (an even neighbour).
	const bigIDLiteral = "9007199254740993"
	const losyNeighbour = "9007199254740992"

	createPayload := `{"id":` + bigIDLiteral + `,"name":"big","amount":1,"status":"new"}`
	entityID := createEntityE2E(t, model, 1, createPayload)

	// Read the entity back via the entity API and assert against the raw
	// body — the in-test getEntityData helper uses bare json.Unmarshal,
	// which would itself round the literal and mask the bug. Substring
	// assertions on the raw response body are the unambiguous check here.
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s", entityID), "")
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get entity: %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, bigIDLiteral) {
		t.Fatalf("precision lost: response body does not contain %q\nbody: %s", bigIDLiteral, respBody)
	}
	if strings.Contains(respBody, losyNeighbour) {
		t.Fatalf("precision lost: response body contains float-rounded neighbour %q\nbody: %s", losyNeighbour, respBody)
	}
}

// --- Test 8.11: Collection create with 50 entities ---

func TestEntityLifecycle_CollectionCreate50(t *testing.T) {
	const model = "e2e-collection-50"

	// Set up a model with a simple workflow.
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "col50-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	// Build a 50-item collection payload.
	var items []string
	for i := 0; i < 50; i++ {
		payload := fmt.Sprintf(`{"name":"Item%d","amount":%d,"status":"new"}`, i, i)
		// Escape the payload JSON for embedding in the outer JSON string.
		escapedPayload := strings.ReplaceAll(payload, `"`, `\"`)
		item := fmt.Sprintf(`{"model":{"name":"%s","version":1},"payload":"%s"}`, model, escapedPayload)
		items = append(items, item)
	}
	body := "[" + strings.Join(items, ",") + "]"

	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", body)
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collection create: expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	// Parse response — should be an array with one result containing 50 entity IDs and a transactionId.
	var results []map[string]any
	if err := json.Unmarshal([]byte(respBody), &results); err != nil {
		t.Fatalf("failed to parse response: %v\nbody: %s", err, respBody)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result entry, got %d", len(results))
	}

	result := results[0]
	ids, ok := result["entityIds"].([]any)
	if !ok {
		t.Fatalf("missing or invalid entityIds in response: %v", result)
	}
	if len(ids) != 50 {
		t.Fatalf("expected 50 entity IDs, got %d", len(ids))
	}

	txID, ok := result["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatalf("missing or empty transactionId in response: %v", result)
	}

	// Verify each entity is readable via GET.
	for i, rawID := range ids {
		id, ok := rawID.(string)
		if !ok {
			t.Fatalf("entity ID %d is not a string: %v", i, rawID)
		}
		getResp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s", id), "")
		if getResp.StatusCode != http.StatusOK {
			getBody := readBody(t, getResp)
			t.Errorf("GET entity %d (%s): expected 200, got %d: %s", i, id, getResp.StatusCode, getBody)
		}
	}
}
