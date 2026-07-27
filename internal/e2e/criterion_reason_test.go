package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// smEventReason returns the reason recorded on the first StateMachine audit
// event of the given eventType for the entity, and whether such an event was
// found. Used for the paths whose transaction commits (automated cascade,
// workflow selection), where the event is durable.
func smEventReason(t *testing.T, entityID, eventType string) (string, bool) {
	t.Helper()
	for _, ev := range getSMAuditEvents(t, entityID) {
		if ev["eventType"] == eventType {
			data, _ := ev["data"].(map[string]any)
			if data == nil {
				return "", true
			}
			r, _ := data["reason"].(string)
			return r, true
		}
	}
	return "", false
}

// TestCriterionReason_ManualReject_Reason400 verifies the primary surface for a
// manual explicit transition rejected by its criterion: the reason is carried
// in the direct 400 WORKFLOW_FAILED response. This is the guaranteed delivery
// for manual rejections and does not depend on the audit store — the manual
// path rolls its transaction back (the transactional-audit invariant is left
// unchanged), so the audit copy is intentionally not asserted here.
func TestCriterionReason_ManualReject_Reason400(t *testing.T) {
	const model = "e2e-critreason-manual"
	procSvc.RegisterCriteriaReason("credit-check",
		func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
			return false, "credit score 540 below threshold 600", nil
		})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cr-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"criterion": {"type": "function", "function": {"name": "credit-check"}}}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	// Manual transition rejected by its criterion -> 400 WORKFLOW_FAILED whose
	// detail carries the reason.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"X","amount":10}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "credit score 540 below threshold 600") {
		t.Errorf("400 body missing criterion reason: %s", body)
	}
}

// TestCriterionReason_AutomatedReject_Audit verifies that an automated-cascade
// transition rejected by an external FUNCTION criterion records the reason on
// the durable TRANSITION_NOT_MATCH_CRITERION state-machine audit event. The
// automated path commits (the entity settles at a stable state), so the event
// is durable on a TX-bound backend.
func TestCriterionReason_AutomatedReject_Audit(t *testing.T) {
	const model = "e2e-critreason-auto"
	procSvc.RegisterCriteriaReason("min-amount",
		func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
			return false, "amount 10 below minimum 100", nil
		})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "auto-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type": "function", "function": {"name": "min-amount"}}}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	reason, ok := smEventReason(t, entityID, "TRANSITION_NOT_MATCH_CRITERION")
	if !ok {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION audit event")
	}
	if reason != "amount 10 below minimum 100" {
		t.Errorf("audit reason: got %q, want %q", reason, "amount 10 below minimum 100")
	}
}

// TestCriterionReason_InlineReject_DefaultReason verifies that an inline
// (non-FUNCTION) predicate that evaluates false records the stable default
// reason on the durable automated TRANSITION_NOT_MATCH_CRITERION audit event —
// inline predicates have no external reason source.
func TestCriterionReason_InlineReject_DefaultReason(t *testing.T) {
	const model = "e2e-critreason-inline"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "inline-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	reason, ok := smEventReason(t, entityID, "TRANSITION_NOT_MATCH_CRITERION")
	if !ok {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION audit event")
	}
	if reason != "criterion did not match" {
		t.Errorf("expected default reason, got %q", reason)
	}
}
