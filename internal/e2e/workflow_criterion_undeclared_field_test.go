package e2e_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// Task 7 (spec §5): a query never executes against a field the model does
// not declare. These full-HTTP-stack tests (running Postgres) prove the
// contract at the boundary that matters to a client: 400 WORKFLOW_FAILED,
// and — the correctness-over-availability half — a genuine rollback, not
// merely a non-2xx status. See internal/domain/workflow/criterion_model_boundary_test.go
// for the unit-level coverage of the same rule (all disposition groups,
// the lifecycle-only/no-model-read gate, the temporal-meta refusal, and the
// bounded peer-refresh).

// TestWorkflowCriterionUndeclaredField_AbortsCreationAndRollsBack covers the
// creation-time abort: workflowSampleModel declares {name, amount, status}
// only. The auto transition's criterion names "score" — a field the model
// never declared — via GREATER_THAN (Group A). The entity write must be
// rolled back entirely: zero rows in the entities table, not merely a
// non-2xx response.
func TestWorkflowCriterionUndeclaredField_AbortsCreationAndRollsBack(t *testing.T) {
	const model = "e2e-crit-undeclared-1"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "undeclared-field-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.score","operatorType":"GREATER_THAN","value":3}
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"Test","amount":10,"status":"new"}`)
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400 for an undeclared-field criterion, got %d: %s", resp.StatusCode, body)
	}
	// ExpectErrorCode re-buffers the body, so it's safe to read again after.
	commontest.ExpectErrorCode(t, resp, "WORKFLOW_FAILED")
	readBody(t, resp)

	// The rollback proof: no entity write at all, not merely a non-2xx.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("expected 0 entities after the undeclared-field criterion aborted, got %d", count)
	}
}

// TestWorkflowCriterionUndeclaredField_ManualTransitionAbortsAndLeavesStateUnchanged
// covers the same rule for a Group B operator (CONTAINS — one of the
// eighteen that answer a real true/false today for an undeclared field,
// per the task's defect description) on an ALREADY-EXISTING entity's manual
// transition. This isolates the rollback half from "no entity write": the
// entity already exists, so the proof here is that the attempted transition
// leaves its state and data untouched — no partial effect — rather than
// silently treating the criterion as "not satisfied" and reporting success.
func TestWorkflowCriterionUndeclaredField_ManualTransitionAbortsAndLeavesStateUnchanged(t *testing.T) {
	const model = "e2e-crit-undeclared-2"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "undeclared-field-manual-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"criterion": {"type":"simple","jsonPath":"$.nickname","operatorType":"CONTAINS","value":"x"}
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":50,"status":"draft"}`)
	if s := getEntityState(t, entityID); s != "CREATED" {
		t.Fatalf("expected CREATED after creation, got %s", s)
	}

	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Alice","amount":50,"status":"draft"}`)
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400 for an undeclared-field criterion on the manual transition, got %d: %s", resp.StatusCode, body)
	}
	// ExpectErrorCode re-buffers the body, so it's safe to read again after.
	commontest.ExpectErrorCode(t, resp, "WORKFLOW_FAILED")
	readBody(t, resp)

	// Rollback proof: the entity's state must be exactly what it was before
	// the aborted transition attempt — no partial advance to APPROVED.
	if s := getEntityState(t, entityID); s != "CREATED" {
		t.Errorf("expected state to remain CREATED after the aborted transition, got %s", s)
	}
}
