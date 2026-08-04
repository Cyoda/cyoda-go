package grpc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

// grpcSelectionWorkflows imports two active workflows that declare THE SAME
// STATE NAMES, distinguished only by their `criterion` — the normal shape for
// a per-kind machine. kind-a-wf is declared first, so a resolver that returns
// "the first active workflow declaring the entity's current state" picks it
// for every entity; the differently-named target states make the observed
// state identify which definition ran.
const grpcSelectionWorkflows = `{
	"importMode": "REPLACE",
	"workflows": [
		{
			"version": "1.1", "name": "kind-a-wf", "initialState": "NONE", "active": true,
			"criterion": {"type": "simple", "jsonPath": "$.kind", "operatorType": "EQUALS", "value": "a"},
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "VALIDATE", "manual": false}]},
				"VALIDATE": {"transitions": [{"name": "check", "next": "A_CHECKED", "manual": true}]},
				"A_CHECKED": {}
			}
		},
		{
			"version": "1.1", "name": "kind-b-wf", "initialState": "NONE", "active": true,
			"criterion": {"type": "simple", "jsonPath": "$.kind", "operatorType": "EQUALS", "value": "b"},
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "VALIDATE", "manual": false}]},
				"VALIDATE": {"transitions": [{"name": "check", "next": "B_CHECKED", "manual": true}]},
				"B_CHECKED": {}
			}
		}
	]
}`

// TestRPC_EntityTransition_ResolvesWorkflowByCriterion drives a manual
// transition through the gRPC entry point (EntityManage /
// EntityTransitionRequest) on a multi-workflow model. gRPC and HTTP are
// separate entry points onto the same engine, so the selection contract is
// pinned on both: the transition must run the definition the entity's
// criterion selects (B_CHECKED), not the first one declaring VALIDATE
// (A_CHECKED).
func TestRPC_EntityTransition_ResolvesWorkflowByCriterion(t *testing.T) {
	const modelName = "grpc-workflow-selection"
	svc, wfHandler, ctx := newTestEnvWithWorkflow(t)

	importAndLockModel(t, svc, ctx, modelName, "1",
		map[string]any{"name": "Test", "kind": "a"})
	req := httptest.NewRequest(http.MethodPost, "/api/model/"+modelName+"/1/workflow/import",
		bytes.NewReader([]byte(grpcSelectionWorkflows))).WithContext(ctx)
	rec := httptest.NewRecorder()
	wfHandler.ImportEntityModelWorkflow(rec, req, modelName, 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("workflow import: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test", "kind": "b"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	transitionCE := makeCE(EntityTransitionRequest, map[string]any{
		"id":         "transition-1",
		"entityId":   entityID,
		"transition": "check",
	})
	resp, err := svc.EntityManage(ctx, transitionCE)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("transition failed: %+v", typed.Error)
	}

	envelope, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("failed to re-fetch entity: %v", err)
	}
	if state, _ := envelope.Meta["state"].(string); state != "B_CHECKED" {
		t.Errorf("state after gRPC transition = %q, want B_CHECKED (kind-b-wf selected by criterion)", state)
	}
}

// TestRPC_EntityTransition_UnevaluableSelectionCriterionFailsClosed covers
// the selection-failure error class on the gRPC surface. When a workflow's
// SELECTION criterion cannot be evaluated the engine must reject rather than
// answer from another definition, and the rejection must reach the caller as
// a domain error in the CloudEvent envelope — not as a success, and not as a
// transport error.
func TestRPC_EntityTransition_UnevaluableSelectionCriterionFailsClosed(t *testing.T) {
	const modelName = "grpc-workflow-selection-failclosed"
	svc, wfHandler, ctx := newTestEnvWithWorkflow(t)

	importAndLockModel(t, svc, ctx, modelName, "1",
		map[string]any{"name": "Test", "kind": "a"})

	importWF := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/model/"+modelName+"/1/workflow/import",
			bytes.NewReader([]byte(body))).WithContext(ctx)
		rec := httptest.NewRecorder()
		wfHandler.ImportEntityModelWorkflow(rec, req, modelName, 1)
		if rec.Code != http.StatusOK {
			t.Fatalf("workflow import: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// Create under a criterion-free workflow so the entity exists and rests
	// in VALIDATE.
	importWF(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "plain-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "VALIDATE", "manual": false}]},
				"VALIDATE": {"transitions": [{"name": "check", "next": "CHECKED", "manual": true}]},
				"CHECKED": {}
			}
		}]
	}`)

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test", "kind": "a"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	txInfo := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	// Now put an unevaluable FUNCTION selection criterion in front of the
	// same states — no external processing service is configured in this
	// harness, so resolution fails outright.
	importWF(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "plain-wf", "initialState": "NONE", "active": true,
			"criterion": {"type": "function", "function": {"name": "no-such-selector"}},
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "VALIDATE", "manual": false}]},
				"VALIDATE": {"transitions": [{"name": "check", "next": "CHECKED", "manual": true}]},
				"CHECKED": {}
			}
		}]
	}`)

	transitionCE := makeCE(EntityTransitionRequest, map[string]any{
		"id":         "transition-1",
		"entityId":   entityID,
		"transition": "check",
	})
	resp, err := svc.EntityManage(ctx, transitionCE)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected the unevaluable selection criterion to fail the transition")
	}
	if typed.Error == nil {
		t.Fatal("expected the error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("envelope code = %s, want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "WORKFLOW_FAILED") {
		t.Errorf("expected WORKFLOW_FAILED in the envelope message, got %s", typed.Error.Message)
	}

	envelope, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("failed to re-fetch entity: %v", err)
	}
	if state, _ := envelope.Meta["state"].(string); state != "VALIDATE" {
		t.Errorf("state = %q, want VALIDATE (a rejected transition must not advance the entity)", state)
	}
}
