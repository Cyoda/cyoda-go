package e2e_test

import (
	"encoding/json"
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// Workflow-criterion field-path coverage.
//
// A criterion's jsonPath may legitimately be written without the "$." prefix —
// nothing rejects it, at import or at evaluation. But the declared-type lookup
// indexes a FieldsMap whose keys always carry the prefix, so a prefix-less path
// resolved to no declared type. The kernel is type-directed: a comparison leaf
// with no declared type expands into nothing and never matches.
//
// Workflow criteria always evaluate through internal/match and never through
// the pushdown translator, so this arm was strictly worse than the search one —
// not an empty result page, but a transition that silently never fired for any
// entity. This is the e2e proof through the full HTTP stack.
// ---------------------------------------------------------------------------

// dataCriterionWorkflow builds a workflow whose CREATED state has one automated
// transition to ADVANCED, gated by a SimpleCondition comparing a data field.
// NONE -> CREATED is unconditioned and automated, so creating an entity
// cascades through CREATED and evaluates the guarded transition in the same
// request. jsonPath is supplied verbatim so a test can vary the spelling.
func dataCriterionWorkflow(t *testing.T, wfName, jsonPath string) string {
	t.Helper()
	criterion, err := json.Marshal(map[string]any{
		"type":         "simple",
		"jsonPath":     jsonPath,
		"operatorType": "GREATER_THAN",
		"value":        50,
	})
	if err != nil {
		t.Fatalf("marshal simple criterion: %v", err)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, wfName, string(criterion))
}

// TestCriterionPathKey_PrefixlessPathFiresTransition is the regression guard.
// The criterion names "amount" rather than "$.amount"; the entity's amount is
// 100, comfortably over the threshold, so the transition must fire. Before the
// fix the leaf resolved to no declared type, evaluated false, and the entity
// stayed at CREATED — for every entity, forever, with no error anywhere.
func TestCriterionPathKey_PrefixlessPathFiresTransition(t *testing.T) {
	const model = "e2e-crit-path-prefixless"

	setupModelWithWorkflow(t, model, dataCriterionWorkflow(t, "crit-path-prefixless-wf", "amount"))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":100}`)

	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf("criterion on prefix-less path \"amount\" with amount=100 > 50: expected ADVANCED, got %q — the leaf resolved to no declared type and evaluated false", state)
	}
}

// The prefixed spelling must behave identically — it always worked, and is the
// control proving the test above measures the spelling and not something else.
func TestCriterionPathKey_PrefixedPathFiresTransition(t *testing.T) {
	const model = "e2e-crit-path-prefixed"

	setupModelWithWorkflow(t, model, dataCriterionWorkflow(t, "crit-path-prefixed-wf", "$.amount"))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":100}`)

	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf("criterion on \"$.amount\" with amount=100 > 50: expected ADVANCED, got %q", state)
	}
}

// And the criterion must still be able to evaluate FALSE on a prefix-less path.
// Without this, a fix that made every prefix-less leaf match unconditionally
// would pass the guard above.
func TestCriterionPathKey_PrefixlessPathDoesNotFireBelowThreshold(t *testing.T) {
	const model = "e2e-crit-path-prefixless-false"

	setupModelWithWorkflow(t, model, dataCriterionWorkflow(t, "crit-path-prefixless-false-wf", "amount"))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	if state := getEntityState(t, entityID); state != "CREATED" {
		t.Errorf("criterion on prefix-less path with amount=10 < 50: expected CREATED (criterion false, transition does not fire), got %q", state)
	}
}
