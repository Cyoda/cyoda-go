package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestDeleteConditional_NOT_TemporalTextOperator_TypeCheckRuns proves that
// deleteConditionTypeCheck's temporal type-soundness check
// (search.ValidateConditionValueTypes -> validateLifecycleType) actually
// runs on the public DELETE /entity/... surface for a NOT-wrapped lifecycle
// condition, and refuses it — CONTAINS is not a valid operator on the
// temporal meta field creationDate, NOT wrapper or not. Before Task 12,
// search.ValidateCondition rejected "NOT" outright before
// deleteConditionTypeCheck ever ran, so no request carrying this shape
// could reach it through this endpoint at all; this is the first case that
// can.
//
// What this test does NOT prove: the specific nil-schema-node gating fix
// (commit "never gate condition type-validation on a model read" —
// deleteConditionTypeCheck must run the type check whether or not a model
// read happened, not skip it when the read is gated off). The model here
// is imported via SAMPLE_DATA with an empty object (`{}`), which still
// persists a non-empty schema — the only public model-creating route,
// POST /model/import/..., always writes one — so this is NOT the
// schema-less (node == nil) case the gating fix addresses; it is confirmed
// by reverting deleteConditionTypeCheck's own type-check call (stubbing it
// to `return nil`) and observing this test starts failing at all — see the
// task-12-report.md's revert-under-test section for that run: 200, 3 of 3
// entities deleted. That failure shows the check runs on this surface; it
// does not discriminate the gating path from an ordinary always-run check.
//
// The nil-node gating case specifically is not reachable through the
// public API at all (no import route can produce a model with a truly
// empty/nil schema) and is covered instead at the unit level, directly
// against deleteConditionTypeCheck with a hand-built model-store double, in
// internal/domain/entity/delete_condition_type_gating_test.go
// (TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_NoSchema_Refused
// and its siblings).
//
// The assertion that matters here is "no rows deleted", not merely
// "non-2xx": a fail-open on this check would delete every entity in the
// model with a response the caller might not even check carefully.
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
