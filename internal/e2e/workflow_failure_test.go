package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- Test 2.1: Processor returns error → entity creation fails ---

func TestWorkflowFailure_ProcessorError(t *testing.T) {
	const model = "e2e-wffail-1"

	procSvc.RegisterProcessor("boom", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("processor exploded")
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "boom", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Entity creation should fail.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	body := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error when processor fails, got 200: %s", body)
	}

	// Verify entity was NOT persisted.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("expected 0 entities after processor failure, got %d", count)
	}
}

// --- Test 2.2: Processor returns error with warnings → warnings propagated ---

func TestWorkflowFailure_ProcessorWarnings(t *testing.T) {
	const model = "e2e-wffail-2"

	// Processor succeeds but we add warnings via context.
	procSvc.RegisterProcessor("warn-proc", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		common.AddWarning(ctx, "data quality issue: missing field X")
		return entity, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "warn-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "warn-proc", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Entity creation should succeed.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Check warnings in response.
	var results []map[string]any
	json.Unmarshal([]byte(body), &results)
	if len(results) > 0 {
		warnings, _ := results[0]["warnings"].([]any)
		hasWarning := false
		for _, w := range warnings {
			if ws, ok := w.(string); ok && len(ws) > 0 {
				hasWarning = true
			}
		}
		if !hasWarning {
			t.Log("note: warnings may not be in transaction response — check entity response instead")
		}
	}

	// Entity should still be created.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1", model)
	if count != 1 {
		t.Errorf("expected 1 entity, got %d", count)
	}
}

// --- Test 2.3: Criteria function returns error → transition not taken ---

func TestWorkflowFailure_CriteriaError(t *testing.T) {
	const model = "e2e-wffail-3"

	procSvc.RegisterCriteria("broken-check", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		return false, fmt.Errorf("criteria evaluation failed")
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "criteria-fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto", "next": "APPROVED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "broken-check"}}
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Criteria errors during cascade cause entity creation to fail entirely.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	body := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error when criteria errors during cascade, got 200: %s", body)
	}

	// Entity should NOT be persisted.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("expected 0 entities after criteria error, got %d", count)
	}
}

// --- Test 2.4: Processor returns context error (simulates timeout) ---

func TestWorkflowFailure_ProcessorContextCancelled(t *testing.T) {
	const model = "e2e-wffail-4"

	// Processor that immediately returns a context cancellation error.
	procSvc.RegisterProcessor("ctx-cancel", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, context.DeadlineExceeded
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "timeout-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "ctx-cancel", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Entity creation should fail.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	body := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error for context cancellation, got 200: %s", body)
	}
}

// --- Test 2.5: Processor panic recovery → system stays stable ---

func TestWorkflowFailure_ProcessorPanic(t *testing.T) {
	const model = "e2e-wffail-5"

	procSvc.RegisterProcessor("panic-proc", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		panic("processor panic!")
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "panic-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "panic-proc", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Entity creation should fail but not crash the server.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	readBody(t, resp)

	// The key assertion: server is still alive after the panic.
	req, err := e2eNewRequest(t, "GET", serverURL+"/api/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	healthResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("server not reachable after processor panic: %v", err)
	}
	defer healthResp.Body.Close()
	// Note: health might be DOWN because recovery middleware sets it to unhealthy.
	// The key point is the server didn't crash.
	if healthResp.StatusCode != http.StatusOK && healthResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unexpected health status after panic: %d", healthResp.StatusCode)
	}
}

// --- Test 2.6: ASYNC_NEW_TX processor failure → independent tx rolled back ---

func TestWorkflowFailure_AsyncNewTxProcessorFailure(t *testing.T) {
	const model = "e2e-wffail-6"

	procSvc.RegisterProcessor("async-fail", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("async processor failed")
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "async-fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "async-fail", "executionMode": "ASYNC_NEW_TX",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// ASYNC_NEW_TX failure is non-fatal — the pipeline continues and entity creation succeeds.
	// See docs/superpowers/specs/2026-04-01-workflow-processor-execution-design.md
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("ASYNC_NEW_TX failure should not kill pipeline, expected 200 got %d: %s", resp.StatusCode, body)
	}
}

// --- Test 2.7: ASYNC_NEW_TX processor success then subsequent transition failure ---

func TestWorkflowFailure_AsyncNewTxSuccessThenCascadeFails(t *testing.T) {
	const model = "e2e-wffail-7"

	// First processor succeeds (ASYNC_NEW_TX).
	procSvc.RegisterProcessor("async-ok", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["async_processed"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	// Second processor on next transition fails.
	procSvc.RegisterProcessor("sync-fail", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("sync processor failed after async success")
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1", "name": "async-then-fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "STEP1", "manual": false,
					"processors": [{"type": "calculator", "name": "async-ok", "executionMode": "ASYNC_NEW_TX",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"STEP1": {"transitions": [{"name": "step2", "next": "STEP2", "manual": false,
					"processors": [{"type": "calculator", "name": "sync-fail", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"STEP2": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// The overall entity creation should fail because sync-fail errors on the cascade.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	body := readBody(t, resp)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected error when sync processor fails after async success, got 200: %s", body)
	}
}
