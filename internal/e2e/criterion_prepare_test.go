package e2e_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Two superseded client-visible behaviour changes, in sequence.
//
// Originally (the prepare/execute split): workflow import validated regex
// patterns but not operator names, so a criterion carrying an unsupported
// operator stored cleanly. Before THAT split, the tree walk was lazy:
// AND[state == "X", $.amount FROBNICATE 1] short-circuited on the first
// conjunct for any entity outside state X and never reached the bad
// operator, so the transition silently did not fire and the save returned
// 2xx. The split fixed that: preparation walks the whole condition, so the
// fault was reported from the criterion's own shape at SAVE time instead —
// a criterion that cannot be evaluated must not be silently read as "not
// satisfied".
//
// A later change (workflow import now checks criterion operators against the
// same canonical set search.ValidateCondition enforces) moved the fault
// earlier again, to IMPORT time: an unsupported operator name — the one
// structural fault category Prepare could still raise from otherwise
// well-formed, already-imported input — is now rejected 400
// VALIDATION_FAILED before the workflow is ever stored, so the "fails the
// save" behaviour below is no longer reachable through an unknown operator
// name. TestCriterion_UnevaluableOperator_RejectedAtImport is this file's
// regression guard for THAT boundary, in the same AND-short-circuit shape
// this file has always used: the validator walks the whole tree structurally
// and must catch the bad operator regardless of what any particular entity's
// state would make the sibling conjunct do.
// ---------------------------------------------------------------------------

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

// TestCriterion_UnevaluableOperator_RejectedAtImport pins the current
// boundary: a criterion carrying an unsupported operator name fails the
// WORKFLOW IMPORT itself with 400 VALIDATION_FAILED, naming the offending
// operator — before the workflow is ever stored, so no entity created
// against this model can ever reach the old "unevaluable operator fails the
// save" behaviour through this path. The operator is nested as the SECOND
// conjunct of an AND whose first conjunct names a state no entity created
// here will ever be in, proving the validator walks the whole condition tree
// structurally rather than depending on what any particular entity's data
// would make the first conjunct evaluate to.
func TestCriterion_UnevaluableOperator_RejectedAtImport(t *testing.T) {
	const model = "e2e-criterion-unevaluable"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)

	status, body := importWorkflowE2E(t, model, 1, unevaluableCriterionWorkflow(t, "criterion-unevaluable-wf"))
	if status != 400 {
		t.Fatalf("workflow import with an unsupported criterion operator: status = %d, want 400\n  body: %s",
			status, body)
	}
	if !strings.Contains(body, "VALIDATION_FAILED") {
		t.Errorf("import response body = %s, want it to carry error code VALIDATION_FAILED", body)
	}
	if !strings.Contains(body, "FROBNICATE") {
		t.Errorf("import response body = %s, want it to name the unsupported operator "+
			"(4xx responses carry full domain detail)", body)
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
