package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// --- Helpers ---

// getEntityState retrieves an entity and returns its state from the meta envelope.
func getEntityState(t *testing.T, entityID string) string {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s", entityID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntity %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var result map[string]any
	json.Unmarshal([]byte(body), &result)
	meta, _ := result["meta"].(map[string]any)
	state, _ := meta["state"].(string)
	return state
}

// getSMAuditEvents retrieves state machine audit events for an entity.
func getSMAuditEvents(t *testing.T, entityID string) []map[string]any {
	t.Helper()
	path := fmt.Sprintf("/api/audit/entity/%s?eventType=StateMachine", entityID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var auditResp map[string]any
	json.Unmarshal([]byte(body), &auditResp)
	items, _ := auditResp["items"].([]any)
	var events []map[string]any
	for _, item := range items {
		if ev, ok := item.(map[string]any); ok {
			events = append(events, ev)
		}
	}
	return events
}

// setupModelWithWorkflow imports a model, locks it, and imports a workflow.
func setupModelWithWorkflow(t *testing.T, entityName string, workflowJSON string) {
	t.Helper()
	importModelE2E(t, entityName, 1)
	lockModelE2E(t, entityName, 1)
	status, body := importWorkflowE2E(t, entityName, 1, workflowJSON)
	if status != http.StatusOK {
		t.Fatalf("workflow import for %s: expected 200, got %d: %s", entityName, status, body)
	}
}

// --- Test 1.6: Loopback after data update ---

func TestWorkflowProc_Loopback(t *testing.T) {
	const model = "e2e-wfproc-6"

	// Criteria that checks if amount > 100.
	procSvc.RegisterCriteria("high-value", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		amount, _ := data["amount"].(float64)
		return amount > 100, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "loopback-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type": "function", "function": {"name": "high-value"}}
				}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Create with amount=50 — should stay CREATED.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":50,"status":"new"}`)
	state := getEntityState(t, entityID)
	if state != "CREATED" {
		t.Fatalf("expected CREATED with amount=50, got %s", state)
	}

	// Update with amount=200 via loopback (PUT without transition name).
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test","amount":200,"status":"new"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback update: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// After loopback, criteria should match and entity moves to PREMIUM.
	state = getEntityState(t, entityID)
	if state != "PREMIUM" {
		t.Errorf("expected PREMIUM after loopback with amount=200, got %s", state)
	}
}

// --- Test 1.8: Default workflow fallback ---

func TestWorkflowProc_DefaultWorkflowFallback(t *testing.T) {
	const model = "e2e-wfproc-8"

	// No workflow imported — default NONE→CREATED→DELETED applies.
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`)

	state := getEntityState(t, entityID)
	if state != "CREATED" {
		t.Errorf("expected CREATED from default workflow, got %s", state)
	}
}

// --- Test 1.9: Cascade depth limit ---

func TestWorkflowProc_CascadeDepthLimit(t *testing.T) {
	const model = "e2e-wfproc-9"

	// Use criteria-gated transitions that always match — this bypasses the
	// static loop detection at import time but still loops at runtime.
	procSvc.RegisterCriteria("loop-gate", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		return true, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "loop-wf", "initialState": "A", "active": true,
			"states": {
				"A": {"transitions": [{"name": "to-b", "next": "B", "manual": false,
					"criterion": {"type": "function", "function": {"name": "loop-gate"}}
				}]},
				"B": {"transitions": [{"name": "to-a", "next": "A", "manual": false,
					"criterion": {"type": "function", "function": {"name": "loop-gate"}}
				}]}
			}
		}]
	}`

	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	if status != http.StatusOK {
		t.Fatalf("workflow import: expected 200, got %d: %s", status, body)
	}

	// Entity creation should fail due to cascade depth exceeded.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	respBody := readBody(t, resp)

	// Should return an error (500 or 400) — not hang.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error for infinite cascade, but got 200: %s", respBody)
	}
}

// --- Test 1.10: Processor modifies entity data ---

func TestWorkflowProc_ProcessorModifiesData(t *testing.T) {
	const model = "e2e-wfproc-10"

	procSvc.RegisterProcessor("compute-total", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		amount, _ := data["amount"].(float64)
		data["total"] = amount * 1.1 // add 10% tax
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "modify-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "compute-total", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":100,"status":"new"}`)

	data := getEntityData(t, entityID, "")
	total, _ := data["total"].(float64)
	if total < 109.99 || total > 110.01 {
		t.Errorf("expected total≈110, got %v", data["total"])
	}
}

// --- Test 1.11: Multiple processors on single transition ---

func TestWorkflowProc_MultipleProcessorsSameTransition(t *testing.T) {
	const model = "e2e-wfproc-11"

	procSvc.RegisterProcessor("step-1", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["step1"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	procSvc.RegisterProcessor("step-2", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		// step-2 should see step-1's output.
		if data["step1"] != true {
			return nil, fmt.Errorf("step-2 did not see step-1's output")
		}
		data["step2"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "multi-proc-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [
						{"type": "calculator", "name": "step-1", "executionMode": "SYNC",
							"config": {"attachEntity": true, "calculationNodesTags": ""}},
						{"type": "calculator", "name": "step-2", "executionMode": "SYNC",
							"config": {"attachEntity": true, "calculationNodesTags": ""}}
					]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`)

	data := getEntityData(t, entityID, "")
	if data["step1"] != true {
		t.Error("expected step1=true")
	}
	if data["step2"] != true {
		t.Error("expected step2=true")
	}
}

// --- Test 1.12: Audit trail for full workflow ---

func TestWorkflowProc_FullAuditTrail(t *testing.T) {
	const model = "e2e-wfproc-12"

	procSvc.RegisterProcessor("audit-proc", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return entity, nil // no-op processor
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "audit-trail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "audit-proc", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {"transitions": [{"name": "finish", "next": "DONE", "manual": false}]},
				"DONE": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`)

	events := getSMAuditEvents(t, entityID)

	// Expect at minimum: START, WORKFLOW_FOUND, TRANSITION_MAKE (x2: init + finish), FINISH.
	eventTypes := make(map[string]int)
	for _, ev := range events {
		if et, ok := ev["eventType"].(string); ok {
			eventTypes[et]++
		}
	}

	if eventTypes["STATE_MACHINE_START"] < 1 {
		t.Error("missing STATE_MACHINE_START event")
	}
	if eventTypes["STATE_MACHINE_FINISH"] < 1 {
		t.Error("missing STATE_MACHINE_FINISH event")
	}
	if eventTypes["TRANSITION_MAKE"] < 2 {
		t.Errorf("expected >= 2 TRANSITION_MAKE events (init + finish), got %d", eventTypes["TRANSITION_MAKE"])
	}

	state := getEntityState(t, entityID)
	if state != "DONE" {
		t.Errorf("expected final state DONE, got %s", state)
	}
}

// createEntityE2EWithTxID creates a single entity via the REST API and returns
// both the entity ID and the transactionId from the POST response.
func createEntityE2EWithTxID(t *testing.T, entityName string, modelVersion int, payload string) (entityID, txID string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, payload)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createEntity %s/%d: expected 200, got %d: %s", entityName, modelVersion, resp.StatusCode, body)
	}

	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		t.Fatalf("createEntity: failed to parse response: %v\nbody: %s", err, body)
	}
	if len(results) == 0 {
		t.Fatalf("createEntity: expected at least one result")
	}

	ids, ok := results[0]["entityIds"].([]any)
	if !ok || len(ids) == 0 {
		t.Fatalf("createEntity: expected entityIds array, got: %v", results[0])
	}
	entityID, _ = ids[0].(string)

	txID, _ = results[0]["transactionId"].(string)
	if txID == "" {
		t.Fatal("createEntity: expected non-empty transactionId in response")
	}
	return entityID, txID
}

// --- Test: PUT /entity/{id}/{transition} with COMMIT_BEFORE_DISPATCH durably commits TX_post (issue #27, Task 13) ---

// TestWorkflowProc_UpdateWithCBD_DurablyCommitsPostCascadeState verifies
// that an UpdateEntity-driven cascade containing a COMMIT_BEFORE_DISPATCH
// processor durably commits its post-cascade state. Before Task 13 the
// engine left TX_post open and the handler committed the now-closed TX_pre,
// so the apply-result was never durable. After Task 13 the handler consumes
// EngineResult.FinalCtx/FinalTxID and commits TX_post.
//
// Test contract: a PUT update transitions the entity via a CBD processor
// that mutates Data. After the PUT returns, a fresh GET must observe both
// the post-cascade state AND the processor-applied data — durably.
func TestWorkflowProc_UpdateWithCBD_DurablyCommitsPostCascadeState(t *testing.T) {
	const model = "e2e-wfproc-cbd-update"

	procSvc.RegisterProcessor("cbd-enrich", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		data["enrichedAmount"] = 999.0
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Workflow: NONE -init-> PENDING -approve-> APPROVED, with the approve
	// transition's processor in COMMIT_BEFORE_DISPATCH mode. The init
	// transition is automated; approve is manual (driven by PUT).
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "cbd-update-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "cbd-enrich",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Create — lands in PENDING via the automated init.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":100,"status":"new"}`)
	if state := getEntityState(t, entityID); state != "PENDING" {
		t.Fatalf("post-create state = %q; want PENDING", state)
	}

	// PUT with named transition "approve" — fires the CBD processor and
	// transitions to APPROVED.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test","amount":100,"status":"approved"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT approve: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Fresh GET — durability check. State must be APPROVED AND the CBD
	// processor's enrichment must be persisted (proves TX_post committed).
	state := getEntityState(t, entityID)
	if state != "APPROVED" {
		t.Errorf("post-cascade state = %q; want APPROVED (TX_post commit leak?)", state)
	}

	data := getEntityData(t, entityID, "")
	if data["enriched"] != true {
		t.Errorf("expected enriched=true in durable data, got %v (TX_post commit leak?)", data["enriched"])
	}
	if v, _ := data["enrichedAmount"].(float64); v != 999.0 {
		t.Errorf("expected enrichedAmount=999, got %v", data["enrichedAmount"])
	}
}

// --- Test: PUT /entity/{id}/{transition} with stale If-Match aborts CBD cascade BEFORE dispatch (issue #27, Task 15) ---

// TestWorkflowProc_UpdateWithCBD_StaleIfMatchAbortsBeforeDispatch is the e2e
// counterpart to engine_ifmatch_test.go's
// TestManualTransitionWithIfMatch_CBDCascadeStaleAbortsBeforeDispatch.
//
// Spec §4.1 "strictly-earlier-enforcement": when an UpdateEntity carries an
// If-Match precondition and the cascade contains a COMMIT_BEFORE_DISPATCH
// processor, a stale If-Match must abort the cascade BEFORE any external
// dispatch fires — i.e. the precondition is enforced at the first segment's
// flush, strictly earlier than the dispatch boundary.
//
// Test contract: a PUT update carrying a deliberately stale If-Match header
// must (a) return 412 PreconditionFailed with ENTITY_MODIFIED, (b) leave the
// CBD processor's external dispatch counter at zero, and (c) leave the
// entity's state unchanged on a fresh GET (no commit of TX_pre, no advance
// to APPROVED).
func TestWorkflowProc_UpdateWithCBD_StaleIfMatchAbortsBeforeDispatch(t *testing.T) {
	const model = "e2e-wfproc-cbd-ifmatch-stale"

	var dispatchCount atomic.Int32
	procSvc.RegisterProcessor("cbd-enrich-counted", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		dispatchCount.Add(1)
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Workflow mirrors the Task-13 happy-path test: NONE -init-> PENDING
	// -approve-> APPROVED, with the manual approve transition's processor in
	// COMMIT_BEFORE_DISPATCH mode.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "cbd-ifmatch-stale-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "cbd-enrich-counted",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Create — lands in PENDING via the automated init.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":100,"status":"new"}`)
	if state := getEntityState(t, entityID); state != "PENDING" {
		t.Fatalf("post-create state = %q; want PENDING", state)
	}

	// PUT approve with deliberately stale If-Match — must surface 412 and
	// must NOT trigger the CBD external dispatch.
	const staleIfMatch = "00000000-0000-0000-0000-000000000000"
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	req := authRequest(t, http.MethodPut, path, strings.NewReader(`{"name":"Test","amount":100,"status":"approved"}`))
	req.Header.Set("If-Match", staleIfMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT approve with stale If-Match failed: %v", err)
	}
	body := readBody(t, resp)

	// (a) 412 PreconditionFailed with ENTITY_MODIFIED.
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want %d (PreconditionFailed); body=%s",
			resp.StatusCode, http.StatusPreconditionFailed, body)
	}
	if !strings.Contains(body, "ENTITY_MODIFIED") {
		t.Errorf("body missing ENTITY_MODIFIED error code: %s", body)
	}

	// (b) Strictly-earlier-enforcement: the dispatch must NOT have fired.
	if c := dispatchCount.Load(); c != 0 {
		t.Errorf("CBD dispatch fired %d time(s) despite stale If-Match — spec §4.1 strictly-earlier-enforcement violated", c)
	}

	// (c) The entity must be unchanged: still PENDING (TX_pre not committed,
	// cascade did not advance), and not enriched.
	if state := getEntityState(t, entityID); state != "PENDING" {
		t.Errorf("post-failure state = %q; want PENDING (cascade should have aborted before any commit)", state)
	}
	data := getEntityData(t, entityID, "")
	if data["enriched"] == true {
		t.Errorf("entity data shows enriched=true; processor must not have run on stale-If-Match abort")
	}
}

// --- Test: POST /entity txId works with /audit/entity/{id}/workflow/{txId}/finished (issue #20) ---

func TestWorkflowProc_PostTxIdMatchesAuditEndpoint(t *testing.T) {
	const model = "e2e-wfproc-txid"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "txid-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "finish", "next": "DONE", "manual": false}]},
				"DONE": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID, postTxID := createEntityE2EWithTxID(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`)

	// The POST response's transactionId must be usable to look up the
	// workflow finished event via the audit endpoint.
	path := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, postTxID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		// This is the bug: the audit endpoint returns 404 because the POST
		// txId doesn't match the SM audit events' txId.
		t.Fatalf("expected 200 from audit finished endpoint using POST txId, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("failed to parse audit response: %v", err)
	}

	// The finished event should report success and the final state.
	if result["state"] != "DONE" {
		t.Errorf("expected state=DONE in audit response, got %v", result["state"])
	}
}

// --- Spec §16 case A (create-time CBD happy path): create cascade
// containing a COMMIT_BEFORE_DISPATCH processor durably commits TX_post ---

// TestWorkflowProc_CreateWithCBD_DurablyCommitsPostCascadeState is the
// CREATE counterpart to Task 13's UPDATE happy-path test. The cascade-entry
// is POST /api/entity/JSON/{model}/{version}; the automated init transition
// fires the CBD processor and lands the entity in APPROVED with the
// processor-applied enrichment durable.
//
// Spec §4.3 client-facing txID semantics: the response transactionId is the
// CASCADE-ENTRY txID (TX_pre), not the FINAL txID (TX_post). This preserves
// audit-lookup semantics for /audit/entity/{id}/workflow/{txId}/finished.
//
// Test contract: a POST that triggers a CBD-segmented cascade must
//   (a) succeed (200 OK),
//   (b) advertise the cascade-entry txID in the response (non-empty),
//   (c) leave the entity durably in the post-cascade state with the
//       processor's enrichment visible on a fresh GET.
func TestWorkflowProc_CreateWithCBD_DurablyCommitsPostCascadeState(t *testing.T) {
	const model = "e2e-wfproc-cbd-create"

	procSvc.RegisterProcessor("cbd-enrich-create", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		data["enrichedAmount"] = 777.0
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Workflow: NONE -init-> PENDING -auto-approve-> APPROVED. Both transitions
	// are automated so they fire in the create cascade. The auto-approve
	// transition's processor is COMMIT_BEFORE_DISPATCH.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "cbd-create-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "auto-approve", "next": "APPROVED", "manual": false,
					"processors": [{"type": "calculator", "name": "cbd-enrich-create",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// POST — drives the create cascade through both segments.
	entityID, txID := createEntityE2EWithTxID(t, model, 1, `{"name":"Test","amount":50,"status":"new"}`)

	// (b) Response txID is non-empty (cascade-entry txID per spec §8/§4.3).
	if txID == "" {
		t.Fatal("expected non-empty transactionId in POST response (cascade-entry txID)")
	}

	// (c) Durability check.
	if state := getEntityState(t, entityID); state != "APPROVED" {
		t.Errorf("post-cascade state = %q; want APPROVED (TX_post commit leak?)", state)
	}
	data := getEntityData(t, entityID, "")
	if data["enriched"] != true {
		t.Errorf("expected enriched=true in durable data, got %v (TX_post commit leak?)", data["enriched"])
	}
	if v, _ := data["enrichedAmount"].(float64); v != 777.0 {
		t.Errorf("expected enrichedAmount=777, got %v", data["enrichedAmount"])
	}

	// The audit endpoint must accept the response txID — confirms the
	// response txID is the cascade-entry one (audit events are keyed on it).
	auditPath := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, txID)
	resp := doAuth(t, http.MethodGet, auditPath, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("audit/.../finished using POST txId: status=%d body=%s", resp.StatusCode, body)
	}
}

// --- Spec §16 case B (multi-entity TX_post cascade) — skipped, see reason ---

// TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten covers
// spec §4.4: a CBD processor running with startNewTxOnDispatch=true performs
// transactional CRUD on a SECONDARY entity via TX_post's token; both the
// secondary write and the engine's apply-result land atomically when
// TX_post commits.
//
// Skipped in the E2E layer because the localproc dispatch fake (the
// in-process processor harness used for these tests) does not currently
// expose factory access to processor callbacks. Wiring a factory accessor
// through localproc would be a non-trivial fixture change beyond the scope
// of the per-task implementation. The same invariant is covered at the
// engine layer by
// internal/domain/workflow/engine_test.go::TestEngine_CommitBeforeDispatch_TrueBranch_HappyPath
// which performs the secondary write inside the dispatch fake and asserts
// the in-memory cascade-anchor mutation. Once Task 17's E2E coverage is
// wired, durability of both entities should be asserted via fresh GETs.
//
// TODO(issue-27, Task 17): expose factory access to localproc dispatch
// callbacks and assert durability of e1 (cascade anchor) AND e2 (secondary)
// after the PUT returns.
func TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten(t *testing.T) {
	t.Skip("requires localproc factory-access harness — engine-level coverage at TestEngine_CommitBeforeDispatch_TrueBranch_HappyPath; see issue #27 Task 17 TODO")
}

// --- Spec §16 case C (hot-entity livelock) — skipped, see reason ---

// TestWorkflowProc_UpdateWithCBD_HotEntityConcurrent stresses the spec §11
// livelock attractor: two concurrent PUTs targeting the same anchor entity,
// each driving a CBD cascade. One must win; the other must surface a
// retryable 409 conflict.
//
// Skipped because reliably exercising the hot-entity attractor at the E2E
// layer requires a deterministic synchronisation harness between two
// concurrent client goroutines AND the dispatch fake — the existing
// retry-on-409 helper (doAuth) auto-recovers retryable conflicts before
// the test can observe them, masking exactly the signal under test. The
// engine-level conflict path is covered by
// internal/domain/workflow/engine_test.go's CBD CAS-conflict tests; the
// classification of 409 retryable is covered by the entity service unit
// tests.
//
// TODO(issue-27, Task 18): build a concurrent-client harness that suppresses
// the doAuth retry helper for this test only and uses a synchronisation
// channel between client goroutines and the dispatch fake to enforce
// overlap.
func TestWorkflowProc_UpdateWithCBD_HotEntityConcurrent(t *testing.T) {
	t.Skip("requires concurrent-client harness without doAuth retry-recovery; see issue #27 Task 18 TODO")
}

// --- Spec §16 case D (concurrent search across segment boundary) ---

// TestWorkflowProc_SearchSeesPreCalloutStateDuringDispatch verifies spec
// §4.2 visibility: while a CBD cascade is mid-dispatch (TX_pre committed,
// processor executing, TX_post not yet open in the false branch — or
// open-but-uncommitted in the true branch), an independent reader must
// observe the PRE-CALLOUT state, not the entry state and not the
// post-callout state.
//
// Test contract: a goroutine drives a PUT update through a CBD cascade.
// The dispatch fake blocks until the searcher goroutine has performed its
// read; the searcher must see the entity in PENDING (the pre-callout
// state). Coordination uses deterministic channels — no time.Sleep.
func TestWorkflowProc_SearchSeesPreCalloutStateDuringDispatch(t *testing.T) {
	const model = "e2e-wfproc-cbd-visibility"

	dispatchEntered := make(chan struct{})
	releaseDispatch := make(chan struct{})

	procSvc.RegisterProcessor("cbd-blocker", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		// Signal that we are mid-dispatch (TX_pre committed), then block
		// until the searcher has done its read.
		close(dispatchEntered)
		<-releaseDispatch
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "cbd-vis-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "cbd-blocker",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":100,"status":"new"}`)
	if state := getEntityState(t, entityID); state != "PENDING" {
		t.Fatalf("post-create state = %q; want PENDING", state)
	}

	// Driver goroutine: fires the PUT that triggers the CBD cascade. It
	// will block in the dispatch fake until releaseDispatch is closed.
	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
		resp := doAuth(t, http.MethodPut, path, `{"name":"Test","amount":100,"status":"approved"}`)
		readBody(t, resp)
	}()

	// Wait until the dispatch fake has been entered — TX_pre is committed
	// at this point (CBD pre-flushes its segment before dispatch).
	<-dispatchEntered

	// Independent read: the entity must show its pre-callout state
	// (PENDING) — not NONE (cascade-entry) and not APPROVED (post-callout).
	state := getEntityState(t, entityID)
	if state != "PENDING" {
		t.Errorf("mid-dispatch reader saw state=%q; want PENDING (pre-callout state — TX_pre committed before dispatch)", state)
	}

	// Pre-callout data must NOT show the processor's enrichment yet.
	data := getEntityData(t, entityID, "")
	if data["enriched"] == true {
		t.Errorf("mid-dispatch reader saw enriched=true; processor write must not be visible until TX_post commits")
	}

	// Release the dispatch fake; the cascade will complete and the PUT
	// will return.
	close(releaseDispatch)
	<-driverDone

	// Post-cascade durability: the cascade has fully committed.
	if state := getEntityState(t, entityID); state != "APPROVED" {
		t.Errorf("post-cascade state = %q; want APPROVED", state)
	}
}

// --- Spec §16 case E (single-segment regression bound) ---

// TestWorkflowProc_UpdateWithoutCBD_RegressionBound is the highest-value
// regression bound for the Task 5/12/13/14 refactor. It asserts that a
// PUT update with a SYNC-only cascade (no CBD processor) preserves the
// pre-refactor behavior:
//
//   - Response txID == cascade-entry txID (single segment, txID not advanced).
//   - All SM audit events for the cascade share that single txID — no
//     fragmentation across segment boundaries.
//   - Final state is durable, processor enrichment applied.
//
// The CBD case (Task 13's TestWorkflowProc_UpdateWithCBD_DurablyCommitsPostCascadeState)
// already covers the segmented path; this test ensures non-segmenting
// cascades remain byte-for-byte identical to the prior behavior.
func TestWorkflowProc_UpdateWithoutCBD_RegressionBound(t *testing.T) {
	const model = "e2e-wfproc-sync-regression"

	procSvc.RegisterProcessor("sync-enrich", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Same workflow shape as Task 13 but with executionMode=SYNC. The
	// cascade does NOT segment — TX_pre and TX_post are the same TX.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "sync-regression-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "sync-enrich",
						"executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":100,"status":"new"}`)

	// PUT approve and capture the response txID.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test","amount":100,"status":"approved"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT approve: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var putResp map[string]any
	if err := json.Unmarshal([]byte(body), &putResp); err != nil {
		t.Fatalf("parse PUT response: %v\nbody: %s", err, body)
	}
	putTxID, _ := putResp["transactionId"].(string)
	if putTxID == "" {
		t.Fatal("PUT response missing transactionId")
	}

	// Durable state.
	if state := getEntityState(t, entityID); state != "APPROVED" {
		t.Errorf("post-cascade state = %q; want APPROVED", state)
	}
	data := getEntityData(t, entityID, "")
	if data["enriched"] != true {
		t.Errorf("expected enriched=true, got %v", data["enriched"])
	}

	// Single audit-event grouping: every SM audit event whose transactionId
	// is associated with this PUT cascade must carry the SAME txID. Pull
	// all SM events for the entity and verify the PUT-cascade events are
	// keyed on putTxID consistently — i.e. the audit lookup endpoint
	// resolves them.
	auditPath := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, putTxID)
	auditResp := doAuth(t, http.MethodGet, auditPath, "")
	auditBody := readBody(t, auditResp)
	if auditResp.StatusCode != http.StatusOK {
		t.Errorf("audit/.../finished using PUT txId: status=%d body=%s — non-CBD cascades must remain audit-discoverable via the response txID", auditResp.StatusCode, auditBody)
	}

	// The full SM event list for this entity must contain at least one
	// event keyed on putTxID (the approve cascade). This confirms no
	// fragmentation: the cascade's events all share the response txID.
	events := getSMAuditEvents(t, entityID)
	matchedPutTx := 0
	for _, ev := range events {
		if txID, _ := ev["transactionId"].(string); txID == putTxID {
			matchedPutTx++
		}
	}
	if matchedPutTx == 0 {
		t.Errorf("expected SM audit events keyed on PUT txID %q, found none across %d events", putTxID, len(events))
	}
}

// --- Spec §16 case F (cluster-mode TX_post pinning) — moved to multi-node harness ---

// TestWorkflowProc_UpdateWithCBD_TxPostPinnedToHomeNode is a placeholder
// kept so anyone searching for spec §4.3 coverage in internal/e2e finds
// the cross-reference. The actual cluster-mode pinning test lives in the
// multi-node parity harness:
//
//	e2e/parity/multinode/cbd_tx_pinning.go
//	  func RunWorkflowProc_CBD_TxPostPinnedToHomeNode
//
// driven by the postgres-backed multi-node entry-point:
//
//	e2e/parity/postgres/multinode_test.go::TestMultiNode
//
// The single-node internal/e2e harness cannot exercise cluster routing —
// it spins one in-process httptest.Server. The multi-node harness spawns
// 3 cyoda-go subprocesses against a shared postgres testcontainer, which
// is the right shape for asserting cluster-level invariants.
func TestWorkflowProc_UpdateWithCBD_TxPostPinnedToHomeNode(t *testing.T) {
	t.Skip("cluster-mode coverage moved to e2e/parity/multinode/cbd_tx_pinning.go — see TestMultiNode/WorkflowProc_CBD_TxPostPinnedToHomeNode")
}

// --- Spec §16 case G (Loopback entry-point coverage) ---

// TestWorkflowProc_LoopbackWithCBD verifies the third engine entry-point
// (Loopback) segments correctly when the entity's current state has an
// outgoing automated transition bearing a CBD processor. Loopback is
// triggered by PUT without a transition name (a data-only update); after
// the data-write the engine re-evaluates automated transitions from the
// current state, which may include CBD-segmented ones.
//
// Test contract: an entity in state PENDING_BIG with a criteria-gated
// automated transition (gated on amount > 100) bearing a CBD processor.
// A PUT loopback update setting amount=200 must (a) pass the criterion,
// (b) fire the CBD processor through the Loopback entry-point, (c) durably
// commit the post-cascade state and the processor's enrichment.
func TestWorkflowProc_LoopbackWithCBD(t *testing.T) {
	const model = "e2e-wfproc-cbd-loopback"

	procSvc.RegisterCriteria("amount-over-100", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		amount, _ := data["amount"].(float64)
		return amount > 100, nil
	})
	procSvc.RegisterProcessor("cbd-loopback-enrich", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		data["upgradedBy"] = "loopback-cbd"
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Workflow: NONE -init-> PENDING_BIG -auto-upgrade(criterion: amount>100,
	// CBD)-> UPGRADED. On create with amount=50 the entity stays in
	// PENDING_BIG (criterion fails). A subsequent loopback PUT with
	// amount=200 re-evaluates the automated transition; criterion now
	// passes, the CBD cascade runs.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "cbd-loopback-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":        {"transitions": [{"name": "init", "next": "PENDING_BIG", "manual": false}]},
				"PENDING_BIG": {"transitions": [{"name": "auto-upgrade", "next": "UPGRADED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "amount-over-100"}},
					"processors": [{"type": "calculator", "name": "cbd-loopback-enrich",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"UPGRADED":    {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Create with amount=50 — criterion fails, entity rests in PENDING_BIG.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test","amount":50,"status":"new"}`)
	if state := getEntityState(t, entityID); state != "PENDING_BIG" {
		t.Fatalf("post-create state = %q; want PENDING_BIG (criterion should have failed)", state)
	}

	// Loopback PUT with amount=200 — no transition name. Engine re-evaluates
	// automated transitions from PENDING_BIG; criterion passes; CBD cascade
	// fires through the Loopback entry-point.
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test","amount":200,"status":"new"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback PUT: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Durability: post-cascade state is UPGRADED, processor's enrichment is
	// visible — proves the Loopback entry-point segments and commits TX_post
	// the same way Execute and ManualTransition do.
	if state := getEntityState(t, entityID); state != "UPGRADED" {
		t.Errorf("post-loopback state = %q; want UPGRADED (Loopback entry-point should drive CBD cascade to completion)", state)
	}
	data := getEntityData(t, entityID, "")
	if data["enriched"] != true {
		t.Errorf("expected enriched=true after loopback CBD, got %v (TX_post commit leak via Loopback entry?)", data["enriched"])
	}
	if data["upgradedBy"] != "loopback-cbd" {
		t.Errorf("expected upgradedBy=loopback-cbd, got %v", data["upgradedBy"])
	}
}

// --- Spec §16 case H (engine-crash kill-simulation) — skipped, see reason ---

// TestWorkflowProc_UpdateWithCBD_EngineCrashLeavesEntityInPreCalloutState
// covers spec §10.1: if the engine crashes between TX_pre.Commit and
// dispatch (or between dispatch return and TX_post.Begin), the entity is
// durably in the pre-callout state. The workflow author is responsible for
// idempotency on retry.
//
// Skipped because the engine has no panic-injection or kill-simulation
// hook; cleanly simulating a crash without corrupting the test process
// would require structural engine changes (an injected fault-point
// callback) that are outside the scope of this issue. The DURABILITY
// invariant the test would assert is structurally guaranteed by the
// audit-event-placement design pinned by
// internal/domain/workflow/engine_test.go::TestEngine_CommitBeforeDispatch_AuditEventPlacement
// (TX_pre is committed BEFORE dispatch; if the process dies before
// TX_post.Commit, TX_pre's state is durable by definition of the commit
// boundary).
//
// TODO(issue-27, Task 23): if a fault-injection hook is added to the
// engine in a future change, replace this skip with a real test that
// triggers the hook between TX_pre.Commit and dispatch and asserts
// durability via a fresh GET.
func TestWorkflowProc_UpdateWithCBD_EngineCrashLeavesEntityInPreCalloutState(t *testing.T) {
	t.Skip("requires engine-side fault-injection hook — pre-callout durability is structurally guaranteed by TX_pre commit boundary, covered at engine layer by TestEngine_CommitBeforeDispatch_AuditEventPlacement; see issue #27 Task 23 TODO")
}

