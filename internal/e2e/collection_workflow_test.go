package e2e_test

// Tests for issue #227 — CreateEntityCollection must route every item through
// the workflow engine, matching the per-item engine call discipline of single
// CreateEntity and per-item Loopback/ManualTransition discipline of
// UpdateEntityCollection. Pre-fix, the handler hard-coded State="CREATED" and
// called entityStore.SaveAll directly, so:
//
//   - the workflow's initialState was ignored,
//   - automated cascade transitions from initialState never fired,
//   - no SMEvent{Started,Finished,TransitionMade} audit events were emitted,
//   - state-machine validation at create time was skipped.
//
// Atomicity contract is single-TX, all-or-nothing — matching the existing
// UpdateEntityCollection precedent. The response wire format
// {transactionId, entityIds[]} is unchanged.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// collectionCreate posts a JSON array of {model, payload} items to
// POST /api/entity/JSON and returns the parsed response body together with the
// HTTP status code. It does not fail the test on non-200 status — callers
// that expect failure inspect the status themselves.
func collectionCreate(t *testing.T, items []string) (int, string) {
	t.Helper()
	body := "[" + strings.Join(items, ",") + "]"
	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", body)
	respBody := readBody(t, resp)
	return resp.StatusCode, respBody
}

// collectionItem builds a single item literal for the collection-create body.
// The payload is JSON-escaped so it can be embedded as a string within the
// outer JSON envelope, mirroring the pattern used in other collection tests.
func collectionItem(model string, modelVersion int, payload string) string {
	escaped := strings.ReplaceAll(payload, `"`, `\"`)
	return fmt.Sprintf(`{"model":{"name":"%s","version":%d},"payload":"%s"}`, model, modelVersion, escaped)
}

// extractCollectionEntityIDs returns the flattened list of entity IDs from a
// collection-create response (an array of {transactionId, entityIds} objects).
func extractCollectionEntityIDs(t *testing.T, body string) []string {
	t.Helper()
	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		t.Fatalf("failed to parse collection response: %v\nbody: %s", err, body)
	}
	ids := make([]string, 0)
	for _, r := range results {
		raw, _ := r["entityIds"].([]any)
		for _, v := range raw {
			if s, ok := v.(string); ok {
				ids = append(ids, s)
			}
		}
	}
	return ids
}

// --- Test 1: initialState derivation ---
//
// A workflow whose initialState is "PENDING" (with no automated cascade out)
// must yield entities in state PENDING, not the literal "CREATED".
func TestCreateCollection_InitialStateDerivation(t *testing.T) {
	const model = "e2e-coll-wf-initstate"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "initstate-wf", "initialState": "PENDING", "active": true,
			"states": {
				"PENDING": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	items := []string{
		collectionItem(model, 1, `{"name":"P1","amount":1,"status":"new"}`),
		collectionItem(model, 1, `{"name":"P2","amount":2,"status":"new"}`),
	}
	status, body := collectionCreate(t, items)
	if status != http.StatusOK {
		t.Fatalf("collection create: expected 200, got %d: %s", status, body)
	}

	ids := extractCollectionEntityIDs(t, body)
	if len(ids) != 2 {
		t.Fatalf("expected 2 entity IDs, got %d: %s", len(ids), body)
	}

	for i, id := range ids {
		state := getEntityState(t, id)
		if state != "PENDING" {
			t.Errorf("item %d (id=%s): expected state PENDING (workflow initialState), got %q",
				i, id, state)
		}
	}
}

// --- Test 2: state-machine audit events emit per item ---
//
// Single CreateEntity emits STATE_MACHINE_START + STATE_MACHINE_FINISH (and
// at least one TRANSITION_MAKE for each automated cascade) — collection
// create must do the same per item.
func TestCreateCollection_AuditEventsEmitted(t *testing.T) {
	const model = "e2e-coll-wf-audit"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "audit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	items := []string{
		collectionItem(model, 1, `{"name":"A1","amount":10,"status":"new"}`),
		collectionItem(model, 1, `{"name":"A2","amount":20,"status":"new"}`),
		collectionItem(model, 1, `{"name":"A3","amount":30,"status":"new"}`),
	}
	status, body := collectionCreate(t, items)
	if status != http.StatusOK {
		t.Fatalf("collection create: expected 200, got %d: %s", status, body)
	}

	ids := extractCollectionEntityIDs(t, body)
	if len(ids) != 3 {
		t.Fatalf("expected 3 entity IDs, got %d: %s", len(ids), body)
	}

	for i, id := range ids {
		events := getSMAuditEvents(t, id)
		eventTypes := make(map[string]int)
		for _, ev := range events {
			if et, ok := ev["eventType"].(string); ok {
				eventTypes[et]++
			}
		}
		if eventTypes["STATE_MACHINE_START"] < 1 {
			t.Errorf("item %d (id=%s): missing STATE_MACHINE_START event; got %v", i, id, eventTypes)
		}
		if eventTypes["STATE_MACHINE_FINISH"] < 1 {
			t.Errorf("item %d (id=%s): missing STATE_MACHINE_FINISH event; got %v", i, id, eventTypes)
		}
		if eventTypes["TRANSITION_MAKE"] < 1 {
			t.Errorf("item %d (id=%s): expected >= 1 TRANSITION_MAKE event (init), got %d (%v)",
				i, id, eventTypes["TRANSITION_MAKE"], eventTypes)
		}
	}
}

// --- Test 3: automated cascade fires per item ---
//
// A workflow whose initialState has an automated transition out of it must
// see that transition fire — entities must end in the cascaded state, not
// the initialState.
func TestCreateCollection_AutomatedCascadeFires(t *testing.T) {
	const model = "e2e-coll-wf-cascade"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "cascade-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "READY", "manual": false}]},
				"READY": {"transitions": [{"name": "auto", "next": "DONE", "manual": false}]},
				"DONE": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	items := []string{
		collectionItem(model, 1, `{"name":"C1","amount":1,"status":"new"}`),
		collectionItem(model, 1, `{"name":"C2","amount":2,"status":"new"}`),
	}
	status, body := collectionCreate(t, items)
	if status != http.StatusOK {
		t.Fatalf("collection create: expected 200, got %d: %s", status, body)
	}

	ids := extractCollectionEntityIDs(t, body)
	if len(ids) != 2 {
		t.Fatalf("expected 2 entity IDs, got %d: %s", len(ids), body)
	}

	for i, id := range ids {
		state := getEntityState(t, id)
		if state != "DONE" {
			t.Errorf("item %d (id=%s): expected state DONE after auto-cascade, got %q", i, id, state)
		}
	}
}

// --- Test 4: per-item failure semantics — single-TX all-or-nothing ---
//
// The handler runs all items in a single transaction, matching
// UpdateEntityCollection's contract. If any item references a missing model,
// the request fails before TX begin (validation phase) and no entities are
// created.
//
// Picking the failure mode: the validation phase already returns 404
// MODEL_NOT_FOUND for missing-model items pre-fix. The fix must preserve that
// behaviour — and must also not commit any of the well-formed siblings.
func TestCreateCollection_FailureRollsBackBatch(t *testing.T) {
	const goodModel = "e2e-coll-wf-fail-good"
	const badModel = "e2e-coll-wf-fail-missing"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, goodModel, wf)

	// Capture the count of pre-existing entities for the good model — we
	// assert it is unchanged after the failing batch attempt.
	statsPath := fmt.Sprintf("/api/entity/stats/%s/%d", goodModel, 1)
	preResp := doAuth(t, http.MethodGet, statsPath, "")
	preBody := readBody(t, preResp)
	if preResp.StatusCode != http.StatusOK {
		t.Fatalf("pre-stats: expected 200, got %d: %s", preResp.StatusCode, preBody)
	}
	var preStats map[string]any
	json.Unmarshal([]byte(preBody), &preStats)
	preCount, _ := preStats["count"].(float64)

	items := []string{
		collectionItem(goodModel, 1, `{"name":"F1","amount":1,"status":"new"}`),
		collectionItem(badModel, 1, `{"name":"F2","amount":2,"status":"new"}`),
		collectionItem(goodModel, 1, `{"name":"F3","amount":3,"status":"new"}`),
	}
	status, body := collectionCreate(t, items)
	if status == http.StatusOK {
		t.Fatalf("collection create with missing model: expected non-200, got 200: %s", body)
	}
	if status != http.StatusNotFound {
		t.Fatalf("collection create with missing model: expected 404, got %d: %s", status, body)
	}

	// No new entities for the good model.
	postResp := doAuth(t, http.MethodGet, statsPath, "")
	postBody := readBody(t, postResp)
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("post-stats: expected 200, got %d: %s", postResp.StatusCode, postBody)
	}
	var postStats map[string]any
	json.Unmarshal([]byte(postBody), &postStats)
	postCount, _ := postStats["count"].(float64)

	if postCount != preCount {
		t.Errorf("expected entity count unchanged after failed batch (atomicity); pre=%v post=%v",
			preCount, postCount)
	}
}

// --- Test 5: tenant isolation ---
//
// The collection handler must continue to scope entities to the caller's
// tenant after the per-item engine.Execute refactor. The single-tenant E2E
// test harness only authenticates one tenant — verify by reading entities
// back through the same authenticated path and confirming they exist.
func TestCreateCollection_TenantScoped(t *testing.T) {
	const model = "e2e-coll-wf-tenant"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "tenant-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	items := []string{
		collectionItem(model, 1, `{"name":"T1","amount":1,"status":"new"}`),
		collectionItem(model, 1, `{"name":"T2","amount":2,"status":"new"}`),
	}
	status, body := collectionCreate(t, items)
	if status != http.StatusOK {
		t.Fatalf("collection create: expected 200, got %d: %s", status, body)
	}

	ids := extractCollectionEntityIDs(t, body)
	if len(ids) != 2 {
		t.Fatalf("expected 2 entity IDs, got %d: %s", len(ids), body)
	}
	for i, id := range ids {
		path := fmt.Sprintf("/api/entity/%s", id)
		resp := doAuth(t, http.MethodGet, path, "")
		respBody := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("item %d (id=%s): expected 200 on read-back, got %d: %s",
				i, id, resp.StatusCode, respBody)
		}
	}
}

// --- Test 6: model with no workflow falls back to default state ---
//
// When a model has no imported workflow, the engine falls back to the
// default workflow (which produces state "CREATED" via the default flow).
// The handler's empty-state guard then kicks in only if the engine returns
// an empty state — but the default workflow always produces a non-empty
// state, so this primarily verifies the non-workflow fallback path remains
// intact through the engine.
func TestCreateCollection_NoWorkflowDefaultsToCreated(t *testing.T) {
	const model = "e2e-coll-wf-nowf"

	// Model without imported workflow.
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	items := []string{
		collectionItem(model, 1, `{"name":"N1","amount":1,"status":"new"}`),
		collectionItem(model, 1, `{"name":"N2","amount":2,"status":"new"}`),
	}
	status, body := collectionCreate(t, items)
	if status != http.StatusOK {
		t.Fatalf("collection create (no workflow): expected 200, got %d: %s", status, body)
	}

	ids := extractCollectionEntityIDs(t, body)
	if len(ids) != 2 {
		t.Fatalf("expected 2 entity IDs, got %d: %s", len(ids), body)
	}
	for i, id := range ids {
		state := getEntityState(t, id)
		if state != "CREATED" {
			t.Errorf("item %d (id=%s): expected state CREATED (default-workflow fallback), got %q",
				i, id, state)
		}
	}
}
