package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestCriterionOperator_UnknownOperatorRejectedAtImport covers the HTTP
// surface for the same fix internal/domain/workflow/criterion_operator_test.go
// pins at the unit level: an unknown criterion operatorType previously
// imported cleanly (walkCriterion checked jsonPath grammar and lifecycle
// type-soundness, never the operator), and the transition it guards then
// silently never fired on every subsequent evaluation — the failure mode
// with no result page to look wrong. operator-semantics.md §4: "An operator
// name outside this set is 400 INVALID_CONDITION, on every surface that
// carries a condition, workflow import included." Workflow import answers
// 400 VALIDATION_FAILED — the one code that surface uses for every content
// rule — with detail naming the offending operator, workflow and transition.
//
// Workflow import is HTTP only; there is no gRPC twin to cover.
func TestCriterionOperator_UnknownOperatorRejectedAtImport(t *testing.T) {
	const model = "e2e-crit-operator-reject"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)

	criterion, err := json.Marshal(map[string]any{
		"type":         "simple",
		"jsonPath":     "$.amount",
		"operatorType": "NOT_EQUALS",
		"value":        1,
	})
	if err != nil {
		t.Fatalf("marshal simple criterion: %v", err)
	}
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "crit-operator-reject-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, string(criterion))

	path := fmt.Sprintf("/api/model/%s/1/workflow/import", model)
	resp := doAuth(t, http.MethodPost, path, wf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	// ExpectErrorCode re-buffers the body, so it's safe to read again after.
	commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
	body := readBody(t, resp)
	if !strings.Contains(body, "NOT_EQUALS") {
		t.Errorf("detail must name the offending operator; body: %s", body)
	}
	if !strings.Contains(body, "crit-operator-reject-wf") || !strings.Contains(body, "advance") {
		t.Errorf("detail must name the offending workflow/transition; body: %s", body)
	}
}

// TestCriterionOperator_KnownOperatorImportsAndFires is the positive control:
// a criterion using a canonical operator still imports and still fires,
// unaffected by the new operator check.
func TestCriterionOperator_KnownOperatorImportsAndFires(t *testing.T) {
	const model = "e2e-crit-operator-accept"

	criterion, err := json.Marshal(map[string]any{
		"type":         "simple",
		"jsonPath":     "$.amount",
		"operatorType": "GREATER_THAN",
		"value":        50,
	})
	if err != nil {
		t.Fatalf("marshal simple criterion: %v", err)
	}
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "crit-operator-accept-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, string(criterion))

	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":100}`)

	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf("criterion with a known operator (amount=100 > 50): expected ADVANCED, got %q", state)
	}
}
