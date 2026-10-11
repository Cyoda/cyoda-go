package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestDeleteConditional_NOT_TemporalTextOperator_TypeCheckRuns pins that the
// type check of the shared condition sequence (search.ValidateConditionForModel,
// whose type step is search.ValidateConditionValueTypes) runs on the public
// DELETE /entity/{entityName}/{modelVersion} surface for a NOT-wrapped
// lifecycle condition, and refuses it: CONTAINS does not apply to the
// temporal meta field creationDate, inside a NOT or not.
//
// The condition names no data path, so the sequence never loads the model's
// fields; the lifecycle type check needs none. The no-data-path case is also
// pinned at unit level, against the same sequence, in
// internal/domain/entity/delete_condition_type_gating_test.go
// (TestDeleteConditionValidation_NotWrappedTemporalTextOperator_NoDataPath_Refused
// and its siblings).
//
// The assertion that matters here is "no rows deleted", not merely
// "non-2xx": a NOT around a leaf that matches nothing selects every entity,
// so a fail-open on this check would delete the whole model.
func TestDeleteConditional_NOT_TemporalTextOperator_TypeCheckRuns(t *testing.T) {
	const model = "e2e-delete-not-temporal-typecheck"

	importModelSampleE2E(t, model, 1, `{}`)
	lockModelE2E(t, model, 1)

	// A minimal workflow so a created entity has lifecycle metadata
	// (creationDate, state) to evaluate against.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "delete-not-temporal-typecheck-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false}]},
				"ACTIVE": {}
			}
		}]
	}`
	status, body := importWorkflowE2E(t, model, 1, wf)
	if status != http.StatusOK {
		t.Fatalf("import workflow: expected 200, got %d: %s", status, body)
	}

	createEntityE2E(t, model, 1, `{}`)
	createEntityE2E(t, model, 1, `{}`)
	createEntityE2E(t, model, 1, `{}`)

	before := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model)
	if before != 3 {
		t.Fatalf("setup: expected 3 live entities before the delete attempt, got %d", before)
	}

	cond := `{"type":"group","operator":"NOT","conditions":[
		{"type":"lifecycle","field":"creationDate","operatorType":"CONTAINS","value":"2024"}
	]}`
	path := fmt.Sprintf("/api/entity/%s/1", model)
	resp := doAuth(t, http.MethodDelete, path, cond)
	respBody := readBody(t, resp)

	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("NOT(creationDate CONTAINS \"2024\"): expected a 4xx refusal, got %d: %s", resp.StatusCode, respBody)
	}

	// The proof that matters: no row was removed, not merely a non-2xx
	// status the caller might not check.
	after := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model)
	if after != 3 {
		t.Errorf("NOT(creationDate CONTAINS \"2024\") must delete nothing; "+
			"before=3 live entities, after=%d — response was %d: %s", after, resp.StatusCode, respBody)
	}
}
