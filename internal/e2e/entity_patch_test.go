//go:build !short

package e2e_test

// TestE2E_EntityPatch_* tests cover the PATCH /api/entity/{format}/{entityId}
// and PATCH /api/entity/{format}/{entityId}/{transition} endpoints through the
// full HTTP stack (PostgreSQL + httptest.Server + JWT auth).
//
// Coverage:
//   - Merge happy-path (field preserved, field changed).
//   - Error preconditions: 415 (XML format), 415 (wrong Content-Type),
//     428 (missing If-Match), 501 (json-patch+json), 412 (stale token).
//   - PATCH with a named transition (state advance + merge).
//   - Merge-then-transition ordering: PATCH sets a field; a SYNC processor on
//     the transition overwrites it. Processor value must win (full E2E,
//     TestE2E_EntityPatch_MergeOrdering_WithProcessor).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// patchEntity issues an authenticated PATCH request to path with the given
// Content-Type, If-Match (non-empty), and body. It returns the raw response.
func patchEntity(t *testing.T, path, contentType, ifMatch, body string) *http.Response {
	t.Helper()
	token := getToken(t, "testclient", "testsecret")
	req, err := e2eNewRequest(t, http.MethodPatch, serverURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("patchEntity newRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patchEntity do: %v", err)
	}
	return resp
}

// patchEntityNoIfMatch issues an authenticated PATCH request without the
// If-Match header, used to exercise the 428 precondition-required path.
func patchEntityNoIfMatch(t *testing.T, path, contentType, body string) *http.Response {
	t.Helper()
	token := getToken(t, "testclient", "testsecret")
	req, err := e2eNewRequest(t, http.MethodPatch, serverURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("patchEntityNoIfMatch newRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	// deliberately omit If-Match
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patchEntityNoIfMatch do: %v", err)
	}
	return resp
}

// TestE2E_EntityPatch_MergeAndPreconditions exercises the PATCH
// /api/entity/JSON/{entityId} loopback path through the full HTTP stack.
//
// Covered:
//  1. Merge happy-path: patch one field, assert the other is preserved.
//  2. XML format → 415.
//  3. Wrong Content-Type (application/json) → 415.
//  4. Missing If-Match → 428.
//  5. application/json-patch+json → 501.
//  6. Stale If-Match token → 412.
func TestE2E_EntityPatch_MergeAndPreconditions(t *testing.T) {
	const model = "e2e-patch-precond"

	// Set up model+workflow; workflowSampleModel has {name, amount, status}.
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "patch-precond-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	// Create an entity and capture the post-create transactionId.
	entityID, createTxID := createEntityE2EWithTxID(t, model, 1,
		`{"name":"Alice","amount":30,"status":"draft"}`)

	// ── 1. Merge happy-path ───────────────────────────────────────────────
	// createEntityE2EWithTxID returns the POST-response transactionId, which
	// equals the If-Match token required by the next write.
	resp := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s", entityID),
		"application/merge-patch+json",
		createTxID,
		`{"amount":31}`,
	)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge happy-path: got %d; body: %s", resp.StatusCode, body)
	}

	// Verify merge: name preserved, amount changed.
	// getEntityData uses json.Unmarshal, so numerics arrive as float64.
	data := getEntityData(t, entityID, "")
	if data["name"] != "Alice" {
		t.Errorf("merge: name not preserved; got %v", data["name"])
	}
	if amt, _ := data["amount"].(float64); amt != 31 {
		t.Errorf("merge: amount not updated; got %v (type %T)", data["amount"], data["amount"])
	}

	// ── 2. XML format → 415 ──────────────────────────────────────────────
	r415xml := patchEntity(t,
		fmt.Sprintf("/api/entity/XML/%s", entityID),
		"application/merge-patch+json",
		"*",
		`{}`,
	)
	readBody(t, r415xml)
	if r415xml.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("XML format: expected 415, got %d", r415xml.StatusCode)
	}

	// ── 3. Wrong Content-Type → 415 ──────────────────────────────────────
	r415ct := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s", entityID),
		"application/json",
		"*",
		`{}`,
	)
	readBody(t, r415ct)
	if r415ct.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("wrong Content-Type: expected 415, got %d", r415ct.StatusCode)
	}

	// ── 4. Missing If-Match → 428 ─────────────────────────────────────────
	r428 := patchEntityNoIfMatch(t,
		fmt.Sprintf("/api/entity/JSON/%s", entityID),
		"application/merge-patch+json",
		`{}`,
	)
	readBody(t, r428)
	if r428.StatusCode != http.StatusPreconditionRequired {
		t.Errorf("missing If-Match: expected 428, got %d", r428.StatusCode)
	}

	// ── 5. application/json-patch+json → 501 ─────────────────────────────
	r501 := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s", entityID),
		"application/json-patch+json",
		"*",
		`[]`,
	)
	readBody(t, r501)
	if r501.StatusCode != http.StatusNotImplemented {
		t.Errorf("json-patch+json: expected 501, got %d", r501.StatusCode)
	}

	// ── 6. Stale If-Match token → 412 ─────────────────────────────────────
	// createTxID is now stale (the happy-path patch above advanced the entity).
	r412 := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s", entityID),
		"application/merge-patch+json",
		createTxID,
		`{"amount":99}`,
	)
	body412 := readBody(t, r412)
	if r412.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: expected 412, got %d; body: %s", r412.StatusCode, body412)
	}
}

// TestE2E_EntityPatch_WithTransition patches an entity AND names a manual
// transition through the PATCH /api/entity/JSON/{entityId}/{transition} route.
// The test verifies that the transition fires (state advances) and the merge
// is applied correctly.
func TestE2E_EntityPatch_WithTransition(t *testing.T) {
	const model = "e2e-patch-transition"

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "patch-trans-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`)

	entityID, createTxID := createEntityE2EWithTxID(t, model, 1,
		`{"name":"Bob","amount":50,"status":"draft"}`)

	if s := getEntityState(t, entityID); s != "CREATED" {
		t.Fatalf("pre-patch state: expected CREATED, got %s", s)
	}

	// PATCH with the "approve" transition — should advance the state and merge
	// the delta ({amount:75}) onto the stored data.
	resp := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s/approve", entityID),
		"application/merge-patch+json",
		createTxID,
		`{"amount":75}`,
	)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH with transition: got %d; body: %s", resp.StatusCode, body)
	}

	// State advanced.
	if s := getEntityState(t, entityID); s != "APPROVED" {
		t.Errorf("post-patch state: expected APPROVED, got %s", s)
	}
	// Merge applied: name preserved, amount updated.
	data := getEntityData(t, entityID, "")
	if data["name"] != "Bob" {
		t.Errorf("PATCH+transition: name not preserved; got %v", data["name"])
	}
	if amt, _ := data["amount"].(float64); amt != 75 {
		t.Errorf("PATCH+transition: amount not updated to 75; got %v", data["amount"])
	}
}

// TestE2E_EntityPatch_MergeOrdering_WithProcessor asserts the merge-then-processor
// ordering invariant end-to-end: a PATCH that sets amount=75 and names a
// transition whose SYNC processor overwrites amount=999 — the processor's value
// must win (merge runs first, processors run after inside updateEntityCore).
func TestE2E_EntityPatch_MergeOrdering_WithProcessor(t *testing.T) {
	const model = "e2e-patch-order"

	// Register an in-process processor that records the incoming amount into
	// seenAmount (proving what the merge produced) then overwrites amount to 999.
	const procName = "patch-order-overwriter"
	procSvc.RegisterProcessor(procName, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		if err := json.Unmarshal(entity.Data, &data); err != nil {
			return nil, err
		}
		data["seenAmount"] = data["amount"] // snapshot what the processor received
		data["amount"] = float64(999)
		updated, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Workflow: NONE -init-> CREATED -approve-> APPROVED, with a SYNC processor
	// on the approve transition that overwrites amount.
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "patch-order-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "%s",
						"executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`, procName)
	setupModelWithWorkflow(t, model, wf)

	entityID, createTxID := createEntityE2EWithTxID(t, model, 1,
		`{"name":"Charlie","amount":50,"status":"draft"}`)

	if s := getEntityState(t, entityID); s != "CREATED" {
		t.Fatalf("pre-patch state: expected CREATED, got %s", s)
	}

	// PATCH: set amount=75 (merge) + name the approve transition (processor sets amount=999).
	// Post-condition: processor wins → amount must be 999, not 75.
	resp := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s/approve", entityID),
		"application/merge-patch+json",
		createTxID,
		`{"amount":75}`,
	)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH with processor: got %d; body: %s", resp.StatusCode, body)
	}

	// State advanced.
	if s := getEntityState(t, entityID); s != "APPROVED" {
		t.Errorf("post-patch state: expected APPROVED, got %s", s)
	}

	// Processor value wins over the merge value.
	// seenAmount proves the processor received the MERGED value (75), not the
	// pre-patch value (50) — i.e. the merge ran before the processor.
	data := getEntityData(t, entityID, "")
	if data["name"] != "Charlie" {
		t.Errorf("name not preserved; got %v", data["name"])
	}
	if seen, _ := data["seenAmount"].(float64); seen != 75 {
		t.Errorf("merge must run before processor: processor saw amount=%v, want 75 (the merged value)", data["seenAmount"])
	}
	if amt, _ := data["amount"].(float64); amt != 999 {
		t.Errorf("processor must win: expected amount=999, got %v", data["amount"])
	}
}

// TestE2E_EntityPatch_ConcurrentConflict fires two concurrent PATCHes
// against the same entity using the same If-Match txId. Exactly one must
// succeed (200) and the other must be rejected (409 or 412). The final
// entity state must reflect the winner's value with no torn write.
//
// This test is isolated here (not in the shared cross-backend parity suite)
// because goroutine-storm concurrency tests can tip an unrelated later
// scenario over the parity HTTP client's timeout on the shared in-memory
// store. Running it here exercises only the real PostgreSQL backend.
func TestE2E_EntityPatch_ConcurrentConflict(t *testing.T) {
	const model = "e2e-patch-concurrent"

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "patch-concurrent-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	entityID, createTxID := createEntityE2EWithTxID(t, model, 1,
		`{"name":"Race","amount":1,"status":"draft"}`)

	type patchResult struct {
		status int
		amount int // 100 or 200 — which value this goroutine tried to set
	}
	results := [2]patchResult{}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		resp := patchEntity(t,
			fmt.Sprintf("/api/entity/JSON/%s", entityID),
			"application/merge-patch+json",
			createTxID,
			`{"amount":100}`,
		)
		readBody(t, resp)
		results[0] = patchResult{status: resp.StatusCode, amount: 100}
	}()
	go func() {
		defer wg.Done()
		resp := patchEntity(t,
			fmt.Sprintf("/api/entity/JSON/%s", entityID),
			"application/merge-patch+json",
			createTxID,
			`{"amount":200}`,
		)
		readBody(t, resp)
		results[1] = patchResult{status: resp.StatusCode, amount: 200}
	}()
	wg.Wait()

	// Exactly one goroutine must have won (200); the other must have been
	// rejected with 409 (Conflict) or 412 (Precondition Failed).
	var winnerAmount int
	var successCount int
	for _, r := range results {
		switch r.status {
		case http.StatusOK:
			successCount++
			winnerAmount = r.amount
		case http.StatusConflict, http.StatusPreconditionFailed:
			// expected loser codes
		default:
			t.Errorf("concurrent PATCH goroutine (amount=%d): unexpected status %d; want 200, 409, or 412",
				r.amount, r.status)
		}
	}
	if successCount != 1 {
		t.Fatalf("concurrent PATCH: %d goroutines succeeded, want exactly 1; results=%v", successCount, results)
	}

	// Final entity must reflect the winner's amount with no torn write.
	data := getEntityData(t, entityID, "")
	if amt, _ := data["amount"].(float64); int(amt) != winnerAmount {
		t.Errorf("post-concurrent amount = %v, want %d (winner's value); torn write or wrong commit",
			data["amount"], winnerAmount)
	}
}
