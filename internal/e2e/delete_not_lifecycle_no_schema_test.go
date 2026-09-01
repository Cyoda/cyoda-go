package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestDeleteConditional_NOT_LifecycleOnly_NoRegisteredSchema_DeletesNothing
// is the end-to-end case the fix in internal/domain/entity/service.go's
// deleteConditionTypeCheck (commit "never gate condition type-validation on
// a model read") could not previously be proven at the public HTTP surface:
// until Task 12 accepted "NOT" as a group operator, search.ValidateCondition
// rejected NOT(...) outright before deleteConditionTypeCheck ever ran, so no
// request carrying this shape could reach it through DELETE /entity/....
//
// The shape matters because of what it inverts. A model with no schema
// registered yet declares no fields — the ordinary "nothing learned about
// this model" state, imported here via SAMPLE_DATA with an empty object
// (`{}`), the closest a caller can get through the public API to "no field
// ever declared". CONTAINS on a temporal meta field (creationDate) is
// something internal/match's prepareLifecycle answers with a permanent,
// deliberate never-match: creationDate is a timestamp, not text, and no
// timestamp ever "contains" a substring. Wrapped in NOT, that permanent
// false inverts into a permanent true — matching every entity — UNLESS
// deleteConditionTypeCheck's temporal type-soundness check
// (search.ValidateConditionValueTypes -> validateLifecycleType) runs and
// refuses the condition before any selection plan is built. That check is
// model-independent (creationDate's type is fixed, not schema-derived), so
// it must run whether or not the model has a registered schema — the
// regression the delete_condition_type_gating_test.go unit tests pin
// directly against deleteConditionTypeCheck, bypassing ValidateCondition's
// then-unconditional NOT rejection. This test is the first one that can
// exercise the identical shape through the real DELETE endpoint, now that
// NOT is structurally accepted.
//
// The assertion that matters is "no rows deleted", not merely "non-2xx": a
// destructive fail-open here would delete every entity in the model with a
// response the caller might not even be checking carefully.
func TestDeleteConditional_NOT_LifecycleOnly_NoRegisteredSchema_DeletesNothing(t *testing.T) {
	const model = "e2e-delete-not-lifecycle-noschema"

	// SAMPLE_DATA with an empty object declares zero fields — no schema
	// learned yet, the state deleteModelSchemaNode's doc calls "the model
	// declares no typed fields, so there is no constraint to apply".
	importModelSampleE2E(t, model, 1, `{}`)
	lockModelE2E(t, model, 1)

	// A minimal workflow so a created entity has lifecycle metadata
	// (creationDate, state) to evaluate against.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "delete-not-lifecycle-noschema-wf", "initialState": "NONE", "active": true,
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

	// The entity payload matches the zero-field schema: an empty object.
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

	// The condition type-check must refuse it: CONTAINS on a temporal meta
	// field is rejected for search AND for the criterion surface alike,
	// regardless of the NOT wrapper or the model's schema state.
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("NOT(creationDate CONTAINS \"2024\") against a schema-less model: "+
			"expected a 4xx refusal, got %d: %s", resp.StatusCode, respBody)
	}

	// The proof that matters: no row was removed, not merely a non-2xx
	// status the caller might not check.
	after := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model)
	if after != 3 {
		t.Errorf("NOT(creationDate CONTAINS \"2024\") against a schema-less model must delete nothing; "+
			"before=3 live entities, after=%d — response was %d: %s", after, resp.StatusCode, respBody)
	}
}
