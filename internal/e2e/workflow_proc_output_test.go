package e2e_test

// A processor's returned data is subject to the same checks as a client write:
// the storability guard, and schema validation or extension governed by the
// model's changeLevel. The engine gets no special rights — a processor may
// extend an entity beyond its model exactly when a client could, and never
// otherwise.
//
// Before this, a processor could write anything: content no backend could store
// (surfacing as 500), or fields the model does not declare — leaving an entity
// the API would return but refuse to accept back on a PUT, and that the model
// export did not mention.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// procOutputWorkflow is a workflow whose single automatic transition runs one
// named processor.
func procOutputWorkflow(procName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "proc-output-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"CREATED": {}
			}
		}]
	}`, procName)
}

// registerRewriter registers a processor that replaces the entity's data wholesale.
func registerRewriter(t *testing.T, name, data string) {
	t.Helper()
	procSvc.RegisterProcessor(name, func(ctx context.Context, e *spi.Entity, p spi.ProcessorDefinition) (*spi.Entity, error) {
		return &spi.Entity{Meta: e.Meta, Data: []byte(data)}, nil
	})
}

// TestWorkflowProcOutput_OffModelFieldRejectedOnStrictModel asserts a processor
// cannot introduce a field the model does not declare when the model has no
// changeLevel — the same answer a client gets.
func TestWorkflowProcOutput_OffModelFieldRejectedOnStrictModel(t *testing.T) {
	const model = "e2e-procout-strict"
	registerRewriter(t, "procout-adds-field", `{"name":"x","amount":1,"status":"new","undeclared":"v"}`)
	defer procSvc.Reset()

	setupModelWithWorkflow(t, model, procOutputWorkflow("procout-adds-field"))

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
		`{"name":"x","amount":1,"status":"new"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 — a processor may not write a field a client could not; body: %s",
			resp.StatusCode, body)
	}
	assertErrorCode(t, body, "WORKFLOW_FAILED")

	// The transition failed, so nothing may have been committed.
	if count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model); count != 0 {
		t.Errorf("expected no entity after the rejected transition, got %d", count)
	}
	// And the model must not have been widened.
	if raw := fmt.Sprintf("%v", exportModelE2E(t, model, 1)); strings.Contains(raw, "undeclared") {
		t.Errorf("model gained the rejected field: %s", raw)
	}
}

// TestWorkflowProcOutput_OffModelFieldExtendsOnStructuralModel is the other half
// of the same rule: where a client's write would extend the model, so does a
// processor's.
func TestWorkflowProcOutput_OffModelFieldExtendsOnStructuralModel(t *testing.T) {
	const model = "e2e-procout-structural"
	registerRewriter(t, "procout-extends", `{"name":"x","amount":1,"status":"new","procAdded":"v"}`)
	defer procSvc.Reset()

	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "STRUCTURAL")
	status, body := importWorkflowE2E(t, model, 1, procOutputWorkflow("procout-extends"))
	if status != http.StatusOK {
		t.Fatalf("workflow import: %d %s", status, body)
	}

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
		`{"name":"x","amount":1,"status":"new"}`)
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 — STRUCTURAL permits the extension; body: %s", resp.StatusCode, respBody)
	}
	if raw := fmt.Sprintf("%v", exportModelE2E(t, model, 1)); !strings.Contains(raw, "procAdded") {
		t.Errorf("model was not extended with the processor's field: %s", raw)
	}
}

// TestWorkflowProcOutput_UnstorableRejected asserts unstorable content from a
// processor is a failed transition (400), not a storage failure (500).
func TestWorkflowProcOutput_UnstorableRejected(t *testing.T) {
	const model = "e2e-procout-unstorable"
	registerRewriter(t, "procout-surrogate",
		"{\"name\":\"a\\ud800b\",\"amount\":1,\"status\":\"new\"}")
	defer procSvc.Reset()

	setupModelWithWorkflow(t, model, procOutputWorkflow("procout-surrogate"))

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
		`{"name":"x","amount":1,"status":"new"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (was 500 before the guard existed); body: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "WORKFLOW_FAILED")
}

// TestWorkflowProcOutput_ExtensionRollsBackWithTheTransition asserts that a
// schema extension made by one processor does not survive a later processor
// failing the same transition.
//
// Both processors here are SYNC, so this exercises the ordinary in-transaction
// path. The commit-before-dispatch placement — where the check must run on
// TX_post rather than where the data arrives — is pinned at the unit level by
// TestProcessorOutput_OffModelFieldRejectedInEveryExecutionMode, which drives
// all three execution modes.
func TestWorkflowProcOutput_ExtensionRollsBackWithTheTransition(t *testing.T) {
	const model = "e2e-procout-rollback"
	registerRewriter(t, "procout-p1", `{"name":"x","amount":1,"status":"new","p1Field":"v"}`)
	procSvc.RegisterProcessor("procout-p2", func(ctx context.Context, e *spi.Entity, p spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("deliberate failure after the extension")
	})
	defer procSvc.Reset()

	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "STRUCTURAL")
	status, body := importWorkflowE2E(t, model, 1, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "procout-rollback-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [
						{"type": "calculator", "name": "procout-p1", "executionMode": "SYNC",
							"config": {"attachEntity": true, "calculationNodesTags": ""}},
						{"type": "calculator", "name": "procout-p2", "executionMode": "SYNC",
							"config": {"attachEntity": true, "calculationNodesTags": ""}}
					]}]},
				"CREATED": {}
			}
		}]
	}`))
	if status != http.StatusOK {
		t.Fatalf("workflow import: %d %s", status, body)
	}

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
		`{"name":"x","amount":1,"status":"new"}`)
	respBody := readBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected the transition to fail on the second processor; body: %s", respBody)
	}
	if raw := fmt.Sprintf("%v", exportModelE2E(t, model, 1)); strings.Contains(raw, "p1Field") {
		t.Errorf("the first processor's schema extension survived a rolled-back transition: %s", raw)
	}
}

// TestWorkflowProcOutput_EntityRemainsRoundTrippable is the defect this rule
// closes, stated as a property: whatever the engine stores, a client must be
// able to send back.
//
// Previously a processor could add a field the model did not declare; the
// entity then read back with that field, but PUTting the very same document
// returned 400 because the model had never heard of it.
func TestWorkflowProcOutput_EntityRemainsRoundTrippable(t *testing.T) {
	const model = "e2e-procout-roundtrip"
	registerRewriter(t, "procout-enrich", `{"name":"x","amount":1,"status":"new","enriched":true}`)
	defer procSvc.Reset()

	setupModelSampleWithWorkflow(t, model,
		`{"name":"x","amount":1,"status":"new","enriched":false}`,
		procOutputWorkflow("procout-enrich"))

	entityID := createEntityE2E(t, model, 1, `{"name":"x","amount":1,"status":"new"}`)

	data := getEntityData(t, entityID, "")
	if data["enriched"] != true {
		t.Fatalf("processor did not run: %v", data)
	}

	// Send the stored document straight back.
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", entityID), string(raw))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT of the entity's own stored document: status=%d, want 200 — what the engine stores the API must accept; body: %s",
			resp.StatusCode, body)
	}
}
