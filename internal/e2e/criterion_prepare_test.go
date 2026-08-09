package e2e_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The one client-visible behaviour change of the prepare/execute split.
//
// Workflow import validates regex patterns but NOT operator names, so a
// criterion carrying an unsupported operator stores cleanly. Before the split,
// the tree walk was lazy: AND[state == "X", $.amount FROBNICATE 1] short-
// circuited on the first conjunct for any entity outside state X and never
// reached the bad operator, so the transition silently did not fire and the
// save returned 2xx.
//
// Preparation walks the whole condition, so the fault is now reported from the
// criterion's own shape. A criterion that cannot be evaluated must not be
// silently read as "not satisfied".
// ---------------------------------------------------------------------------

// countEntitiesInModel returns the number of entities in a model matching
// every row (matchAllCond), reusing the suite's existing direct-search
// helper and match-all condition rather than inventing a new HTTP idiom.
func countEntitiesInModel(t *testing.T, entityName string, modelVersion int) int {
	t.Helper()
	status, results := directSearch(t, entityName, modelVersion, matchAllCond)
	if status != 200 {
		t.Fatalf("count search for %s: expected 200, got %d", entityName, status)
	}
	return len(results)
}

// unevaluableCriterionWorkflow builds a workflow whose CREATED state has one
// automated transition guarded by AND[state == "NEVER_REACHED", $.amount
// FROBNICATE 1]. NONE -> CREATED is unconditioned and automated, so creating an
// entity cascades into CREATED and evaluates the guarded criterion in the same
// request — with the entity in state CREATED, i.e. outside the state the first
// conjunct names.
func unevaluableCriterionWorkflow(t *testing.T, wfName string) string {
	t.Helper()
	criterion, err := json.Marshal(map[string]any{
		"type":     "group",
		"operator": "AND",
		"conditions": []any{
			map[string]any{
				"type": "lifecycle", "field": "state",
				"operatorType": "EQUALS", "value": "NEVER_REACHED",
			},
			map[string]any{
				"type": "simple", "jsonPath": "$.amount",
				"operatorType": "FROBNICATE", "value": 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal criterion: %v", err)
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

// TestCriterion_UnevaluableOperator_FailsTheSave pins the declared change: a
// criterion carrying an unsupported operator name fails the save with 400
// WORKFLOW_FAILED and rolls the transaction back, even for an entity whose
// state makes the sibling conjunct false.
//
// Before the split this returned 2xx and left the entity at CREATED.
func TestCriterion_UnevaluableOperator_FailsTheSave(t *testing.T) {
	const model = "e2e-criterion-unevaluable"

	// Import must SUCCEED — this is the premise. Workflow import does not check
	// operator names, which is why the criterion is storable at all.
	setupModelWithWorkflow(t, model, unevaluableCriterionWorkflow(t, "criterion-unevaluable-wf"))

	status, body := createEntityRaw(t, model, 1, `{"name":"X","amount":1}`)

	if status != 400 {
		t.Fatalf("create with an unevaluable criterion: status = %d, want 400\n  body: %s\n"+
			"a criterion that cannot be evaluated must fail the save, not be read as 'not satisfied'",
			status, body)
	}
	if !strings.Contains(body, "WORKFLOW_FAILED") {
		t.Errorf("create response body = %s, want it to carry error code WORKFLOW_FAILED", body)
	}
	if !strings.Contains(body, "FROBNICATE") {
		t.Errorf("create response body = %s, want it to name the unsupported operator "+
			"(4xx responses carry full domain detail)", body)
	}
}

// TestCriterion_UnevaluableOperator_RollsBackTheWrite pins that the failed save
// leaves nothing behind: a criterion evaluation failure rolls the whole
// transaction back, so the entity write is discarded rather than committed with
// the transition skipped.
func TestCriterion_UnevaluableOperator_RollsBackTheWrite(t *testing.T) {
	const model = "e2e-criterion-unevaluable-rollback"

	setupModelWithWorkflow(t, model, unevaluableCriterionWorkflow(t, "criterion-unevaluable-rollback-wf"))

	status, _ := createEntityRaw(t, model, 1, `{"name":"X","amount":1}`)
	if status != 400 {
		t.Fatalf("precondition: create status = %d, want 400", status)
	}

	// Nothing was persisted. Search the model and require an empty result set.
	found := countEntitiesInModel(t, model, 1)
	if found != 0 {
		t.Errorf("model holds %d entities after a rolled-back save, want 0", found)
	}
}

// TestCriterion_EvaluableCriterionStillShortCircuits is the control: the same
// workflow shape with a SUPPORTED operator on the second conjunct still saves
// cleanly and still leaves the entity at CREATED, because the criterion is
// genuinely false. Without this row, the two tests above would pass on an
// engine that failed every save.
func TestCriterion_EvaluableCriterionStillShortCircuits(t *testing.T) {
	const model = "e2e-criterion-evaluable-control"

	criterion, err := json.Marshal(map[string]any{
		"type":     "group",
		"operator": "AND",
		"conditions": []any{
			map[string]any{"type": "lifecycle", "field": "state",
				"operatorType": "EQUALS", "value": "NEVER_REACHED"},
			map[string]any{"type": "simple", "jsonPath": "$.amount",
				"operatorType": "GREATER_THAN", "value": 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal criterion: %v", err)
	}
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "criterion-evaluable-control-wf",
			"initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, string(criterion))

	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":1}`)

	if state := getEntityState(t, entityID); state != "CREATED" {
		t.Errorf("state = %q, want CREATED: the criterion is false, so the transition must not fire", state)
	}
}
