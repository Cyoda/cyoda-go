package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
//
// The import-time boundary does not retire the save-time rollback property,
// though — it only makes an unsupported operator harder to reach. A stored
// workflow is never re-validated (path-grammar.md's own rationale for
// checking a criterion only at import: "A stored workflow is not
// re-checked"), and workflow/engine.go calls match.Prepare on every save
// with no revalidation — the same fact that keeps internal/match's
// prepareLifecycle guard unconditional (see prepared.go). A criterion
// imported before a validation tightening, or through any future gap in the
// operator mapping, still reaches that path today.
// TestCriterion_LegacyUnevaluableCriterion_FailsSaveAndRollsBack pins that
// property directly: it seeds a criterion shaped exactly like
// unevaluableCriterionWorkflow's, but writes it straight to the
// WorkflowStore — bypassing the HTTP import boundary entirely, the same
// deliberate-bypass pattern internal/match's own equivalence tests use to
// reach match.Prepare with an input the validation boundary would refuse —
// and then drives a save over HTTP and asserts both halves: the save fails,
// and nothing is persisted.
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

// legacyStorageTenantCtx builds a background context carrying the bootstrap
// tenant's user context, matching the tenant the HTTP client's M2M token
// authenticates as, so a workflow saved directly through this context is
// visible to a subsequent HTTP-driven save against the same model.
func legacyStorageTenantCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "test-admin",
		UserName: "Test Admin",
		Tenant:   spi.Tenant{ID: "test-tenant", Name: "test-tenant"},
		Roles:    []string{"ROLE_ADMIN"},
	})
}

// seedLegacyUnevaluableCriterionWorkflow writes a workflow carrying
// unevaluableCriterionWorkflow's AND[state == "NEVER_REACHED", $.amount
// FROBNICATE 1] criterion directly to the WorkflowStore, bypassing the HTTP
// import boundary (and therefore search.ValidateCriterionCondition)
// entirely. This is the deliberate-bypass pattern internal/match's own
// equivalence tests use for the temporal-meta case: the only way to
// construct the shape a "stored before a validation tightening, never
// re-validated" criterion is in today, since import itself now refuses it.
func seedLegacyUnevaluableCriterionWorkflow(t *testing.T, entityName, wfName string) {
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

	defs := []spi.WorkflowDefinition{{
		Version: "1.1", Name: wfName, InitialState: "NONE", Active: true,
		States: map[string]spi.StateDefinition{
			"NONE": {Transitions: []spi.TransitionDefinition{
				{Name: "init", Next: "CREATED", Manual: false},
			}},
			"CREATED": {Transitions: []spi.TransitionDefinition{
				{Name: "advance", Next: "ADVANCED", Manual: false, Criterion: criterion},
			}},
			"ADVANCED": {},
		},
	}}

	ctx := legacyStorageTenantCtx()
	wfStore, err := testApp.StoreFactory().WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: entityName, ModelVersion: "1"}
	if err := wfStore.Save(ctx, ref, defs); err != nil {
		t.Fatalf("seed legacy workflow directly: %v", err)
	}
}

// TestCriterion_LegacyUnevaluableCriterion_FailsSaveAndRollsBack pins the
// property TestCriterion_UnevaluableOperator_RejectedAtImport's import-time
// rejection does NOT retire: a stored workflow is never re-validated
// (path-grammar.md: "A stored workflow is not re-checked"), and
// workflow/engine.go calls match.Prepare on every save with no
// revalidation — the same fact that keeps internal/match's prepareLifecycle
// guard unconditional. A criterion imported before a validation tightening,
// or through any future gap in the operator-name mapping, still reaches
// that path today and must still fail closed with a full rollback, exactly
// as it did before the import-time check existed.
//
// The criterion here could never have been imported through the HTTP
// boundary this batch adds — that is the point — so it is seeded straight
// into the WorkflowStore, simulating exactly that "already stored, never
// re-validated" criterion.
func TestCriterion_LegacyUnevaluableCriterion_FailsSaveAndRollsBack(t *testing.T) {
	const model = "e2e-criterion-legacy-unevaluable"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)
	seedLegacyUnevaluableCriterionWorkflow(t, model, "criterion-legacy-unevaluable-wf")

	status, body := createEntityRaw(t, model, 1, `{"name":"X","amount":1}`)
	if status != 400 {
		t.Fatalf("create against a stored unevaluable criterion: status = %d, want 400\n  body: %s\n"+
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

	// Nothing was persisted. Search the model and require an empty result set.
	found := countEntitiesInModel(t, model, 1)
	if found != 0 {
		t.Errorf("model holds %d entities after a rolled-back save, want 0", found)
	}
}
