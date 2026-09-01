package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestCriterionNOT_ExactlyOneCondition_ImportsAndFires is the positive
// control for Task 12 of the NOT-node plan: a NOT-wrapped group criterion
// with exactly one condition imports cleanly and evaluates through the
// engine like any other criterion. NOT was unconditionally rejected at the
// validator until this task; now it is accepted, but only with exactly one
// entry in `conditions` — see the arity tests below for the two ways
// bad arity is refused.
func TestCriterionNOT_ExactlyOneCondition_ImportsAndFires(t *testing.T) {
	const model = "e2e-crit-not-accept"

	criterion := `{"type":"group","operator":"NOT","conditions":[
		{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}
	]}`
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "crit-not-accept-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, criterion)

	setupModelWithWorkflow(t, model, wf)
	// amount=10 fails GREATER_THAN 50, so NOT(...) is true and the
	// transition fires.
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)
	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf("NOT(amount>50) with amount=10: expected ADVANCED, got %q", state)
	}
}

// TestCriterionNOT_ZeroConditions_RejectedAtImport covers the zero-arity
// rejection: NOT declaring an empty `conditions` array is 400
// VALIDATION_FAILED, the same code every other structural workflow-import
// rule uses, with detail naming the offending workflow/transition.
func TestCriterionNOT_ZeroConditions_RejectedAtImport(t *testing.T) {
	const model = "e2e-crit-not-zero"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)

	criterion := `{"type":"group","operator":"NOT","conditions":[]}`
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "crit-not-zero-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, criterion)

	path := fmt.Sprintf("/api/model/%s/1/workflow/import", model)
	resp := doAuth(t, http.MethodPost, path, wf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
	body := readBody(t, resp)
	if !strings.Contains(body, "crit-not-zero-wf") || !strings.Contains(body, "advance") {
		t.Errorf("detail must name the offending workflow/transition; body: %s", body)
	}
}

// TestCriterionNOT_TwoConditions_RejectedAtImport covers the other-arity
// rejection: NOT declaring two entries in `conditions` is also 400
// VALIDATION_FAILED.
func TestCriterionNOT_TwoConditions_RejectedAtImport(t *testing.T) {
	const model = "e2e-crit-not-two"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)

	criterion := `{"type":"group","operator":"NOT","conditions":[
		{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50},
		{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"X"}
	]}`
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "crit-not-two-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, criterion)

	path := fmt.Sprintf("/api/model/%s/1/workflow/import", model)
	resp := doAuth(t, http.MethodPost, path, wf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
}
