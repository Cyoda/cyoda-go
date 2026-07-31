package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// A `function` clause is a workflow/transition-criterion shape: the engine
// intercepts it and dispatches it to a compute member. Search has no
// dispatcher — it used to reach the in-memory evaluator and surface as a 500
// on sync search, grouped stats and conditional delete, and as a silently
// FAILED job on async submit. Every search-shaped entry point must now reject
// it at the boundary with 400 INVALID_CONDITION.

const functionCondition = `{
	"type": "function",
	"function": {
		"name": "approval-check",
		"config": {"calculationNodesTags": "approval-service", "attachEntity": true}
	}
}`

// functionConditionNestedInGroup buries the clause two groups deep — the
// walker must find it at any depth, exactly as it does a malformed regex.
const functionConditionNestedInGroup = `{
	"type": "group",
	"operator": "AND",
	"conditions": [
		{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10},
		{
			"type": "group",
			"operator": "OR",
			"conditions": [
				{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"},
				{"type":"function","function":{"name":"approval-check"}}
			]
		}
	]
}`

// expectFunctionConditionRejected asserts the 400 INVALID_CONDITION contract
// and that the detail names the reason rather than leaking internals.
func expectFunctionConditionRejected(t *testing.T, method, path, body string) {
	t.Helper()
	resp := doAuth(t, method, path, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%s %s: expected 400, got %d; body: %s", method, path, resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
	if detail := readBody(t, resp); !strings.Contains(detail, "function") {
		t.Errorf("%s %s: detail should name the offending condition type; body: %s", method, path, detail)
	}
}

func TestSearch_Sync_FunctionCondition_Returns400(t *testing.T) {
	const model = "e2e-search-fn-sync"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	expectFunctionConditionRejected(t, http.MethodPost,
		fmt.Sprintf("/api/search/direct/%s/1", model), functionCondition)
}

// The async path is the one that failed most quietly: it used to return 200
// with a job id and the job later ended FAILED with no reason surfaced. No job
// may be created for a condition that can never execute.
func TestSearch_AsyncSubmit_FunctionCondition_Returns400(t *testing.T) {
	const model = "e2e-search-fn-async"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":42,"status":"active"}`)

	expectFunctionConditionRejected(t, http.MethodPost,
		fmt.Sprintf("/api/search/async/%s/1", model), functionCondition)
}

func TestGroupedStats_FunctionCondition_Returns400(t *testing.T) {
	const model = "e2e-stats-fn"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	expectFunctionConditionRejected(t, http.MethodPost,
		fmt.Sprintf("/api/entity/stats/%s/1/query", model),
		`{"groupBy":["$.name"],"condition":`+functionCondition+`}`)
}

// Conditional delete selects via the same search service. Rejecting at the
// boundary also means no transaction is opened and rolled back for a request
// that could never have succeeded.
func TestDeleteConditional_FunctionCondition_Returns400(t *testing.T) {
	const model = "e2e-delete-fn"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	expectFunctionConditionRejected(t, http.MethodDelete,
		fmt.Sprintf("/api/entity/%s/1", model), functionCondition)

	// The entity must still be there — a rejected condition deletes nothing.
	status, results := directSearch(t, model, 1,
		`{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`)
	if status != http.StatusOK {
		t.Fatalf("post-reject search: expected 200, got %d", status)
	}
	if len(results) != 1 {
		t.Errorf("expected the entity to survive a rejected conditional delete, got %d results", len(results))
	}
}

func TestSearch_Sync_FunctionConditionNestedInGroup_Returns400(t *testing.T) {
	const model = "e2e-search-fn-nested"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	expectFunctionConditionRejected(t, http.MethodPost,
		fmt.Sprintf("/api/search/direct/%s/1", model), functionConditionNestedInGroup)
}

// The accept-side counterpart: rejecting function clauses in search must not
// touch the other condition types travelling the same validator.
func TestSearch_Sync_NonFunctionCondition_StillReturns200(t *testing.T) {
	const model = "e2e-search-fn-accept"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"active"}`)

	status, results := directSearch(t, model, 1, `{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"},
			{"type":"lifecycle","field":"state","operatorType":"NOT_EQUAL","value":"NOPE"}
		]
	}`)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 match, got %d", len(results))
	}
}
