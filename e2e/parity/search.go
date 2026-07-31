package parity

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// searchWorkflowJSON provides a workflow with a manual "approve" transition.
// Search scenarios that need lifecycle state changes or manual transitions
// reuse this workflow.
const searchWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "search-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}
	}]
}`

// setupSearchModel imports a model, locks it, and imports the search
// workflow (with a manual "approve" transition).
func setupSearchModel(t *testing.T, c *client.Client, modelName string, modelVersion int) {
	t.Helper()
	setupModelWithWorkflow(t, c, modelName, modelVersion, searchWorkflowJSON)
}

// RunSearchSimpleCondition creates 3 entities with different statuses,
// searches for status=="active", and asserts 2 results.
// Port of internal/e2e TestSearch_SimpleCondition.
func RunSearchSimpleCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-simple"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"inactive"}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","amount":200,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Carol: %v", err)
	}

	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"}`
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results (Alice, Carol), got %d", len(results))
	}
}

// RunSearchBoolCondition creates 3 entities with a boolean field and searches
// with a JSON boolean value (EQUALS true, then NOT_EQUALS true). Guards the
// postgres bool->text encode bug: a raw Go bool bound against the text-typed
// doc->>'path' extraction failed to encode ("cannot find encode plan"), 500ing
// the search. Memory and sqlite always handled it; this asserts all backends agree.
func RunSearchBoolCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-bool"
	const modelVersion = 1
	// Own model import so the schema declares the boolean `active` field
	// (the shared search model has no bool field).
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Sample","active":true}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","active":true}`); err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","active":false}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","active":true}`); err != nil {
		t.Fatalf("CreateEntity Carol: %v", err)
	}

	// EQUALS a JSON boolean true -> Alice, Carol.
	eqCond := `{"type":"simple","jsonPath":"$.active","operatorType":"EQUALS","value":true}`
	eqResults, err := c.SyncSearch(t, modelName, modelVersion, eqCond)
	if err != nil {
		t.Fatalf("SyncSearch (EQUALS true): %v", err)
	}
	if len(eqResults) != 2 {
		t.Errorf("EQUALS true: expected 2 results (Alice, Carol), got %d", len(eqResults))
	}

	// NOT_EQUALS a JSON boolean true -> Bob (same text-branch encode path).
	neCond := `{"type":"simple","jsonPath":"$.active","operatorType":"NOT_EQUAL","value":true}`
	neResults, err := c.SyncSearch(t, modelName, modelVersion, neCond)
	if err != nil {
		t.Fatalf("SyncSearch (NOT_EQUAL true): %v", err)
	}
	if len(neResults) != 1 {
		t.Errorf("NOT_EQUAL true: expected 1 result (Bob), got %d", len(neResults))
	}
}

// RunSearchLifecycleCondition creates 2 entities, fires a manual transition
// on one to move it to APPROVED, then searches by lifecycle state==APPROVED
// and asserts 1 result.
// Port of internal/e2e TestSearch_LifecycleCondition.
func RunSearchLifecycleCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-lifecycle"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"new"}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// Approve Alice via manual transition.
	if err := c.UpdateEntity(t, entityID, "approve", `{"name":"Alice","amount":100,"status":"approved"}`); err != nil {
		t.Fatalf("UpdateEntity (approve Alice): %v", err)
	}

	// Search for entities in APPROVED state.
	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"APPROVED"}`
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 APPROVED entity, got %d", len(results))
	}
}

// RunSearchGroupCondition creates 3 entities, searches with AND condition
// (status=="active" AND amount>75), and asserts 1 result.
// Port of internal/e2e TestSearch_GroupCondition.
func RunSearchGroupCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-group"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","amount":200,"status":"inactive"}`); err != nil {
		t.Fatalf("CreateEntity Carol: %v", err)
	}

	cond := `{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"},
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":75}
		]
	}`
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result (Alice), got %d", len(results))
	}
}

// RunSearchNoMatches creates an entity, searches for a nonexistent value,
// and asserts 0 results (not an error).
// Port of internal/e2e TestSearch_NoMatches.
func RunSearchNoMatches(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-nomatch"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"nonexistent"}`
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// RunSearchAfterUpdate creates an entity with amount=50, searches for
// amount>75 (0 results), updates to amount=100 via data-only update,
// searches again (1 result).
// Port of internal/e2e TestSearch_AfterUpdate.
func RunSearchAfterUpdate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-afterupdate"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":50,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Search for amount > 75 — should find nothing.
	cond := `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":75}`
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch (before update): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results before update, got %d", len(results))
	}

	// Update amount to 100 via data-only update (no transition).
	if err := c.UpdateEntityData(t, entityID, `{"name":"Alice","amount":100,"status":"draft"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}

	// Search again — should find Alice.
	results, err = c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch (after update): %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result after update, got %d", len(results))
	}
}

// RunSearchDirectBoundedOrFail asserts the bounded-or-fail contract on every
// backend's direct-search path: the limit is a cap on the matched set, not a
// page size. A matched set larger than the limit is a 400, never a truncated
// prefix. The omitted-limit case is pinned from both sides: 1001 matches
// (amount==1 or amount==2) exceed the default and 400, while the 1000-strong
// amount==1 subset alone is under it and succeeds — so this scenario proves
// the default is exactly 1000, not merely "1000 or smaller".
func RunSearchDirectBoundedOrFail(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-search-bounded-or-fail"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// 1001 matching entities: one more than the documented default of 1000.
	// The last one carries amount:2 so a narrower condition (amount==1) can
	// isolate exactly 1000 of them without a second seeding pass.
	for i := 0; i < 1001; i++ {
		amount := 1
		if i == 1000 {
			amount = 2
		}
		if _, err := c.CreateEntity(t, modelName, modelVersion,
			fmt.Sprintf(`{"name":"n%d","amount":%d,"status":"new"}`, i, amount)); err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
	}

	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"new"}`

	// Omitted limit → the 1000 default applies, and 1001 matches exceed it.
	status, body, err := c.SyncSearchRawLimit(t, modelName, modelVersion, cond, -1)
	if err != nil {
		t.Fatalf("SyncSearch (omitted limit): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("omitted limit: got status %d, want 400; body=%s", status, body)
	}
	if !containsErrorCode(body, "SEARCH_RESULT_LIMIT") {
		t.Errorf("omitted limit: expected errorCode SEARCH_RESULT_LIMIT, body=%s", body)
	}

	// Omitted limit, narrowed to the 1000-strong amount==1 subset → under the
	// default, so it succeeds and returns all 1000. Without this the previous
	// 400 would equally pass under any default <= 1000 (e.g. 500); this pins
	// the default from below and makes it exactly 1000.
	amountOneCond := `{"type":"group","operator":"AND","conditions":[` +
		`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"new"},` +
		`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":1}]}`
	amountOneResults, err := c.SyncSearch(t, modelName, modelVersion, amountOneCond) // no limit param, 200 required
	if err != nil {
		t.Fatalf("SyncSearch (omitted limit, amount==1 subset): %v", err)
	}
	if len(amountOneResults) != 1000 {
		t.Errorf("omitted limit, amount==1 subset: got %d results, want 1000", len(amountOneResults))
	}

	// Explicit limit one short of the match count → same outcome.
	status, body, err = c.SyncSearchRawLimit(t, modelName, modelVersion, cond, 1000)
	if err != nil {
		t.Fatalf("SyncSearch (limit=1000): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("limit=1000: got status %d, want 400; body=%s", status, body)
	}
	if !containsErrorCode(body, "SEARCH_RESULT_LIMIT") {
		t.Errorf("limit=1000: expected errorCode SEARCH_RESULT_LIMIT, body=%s", body)
	}

	// Exactly at the match count → the whole set comes back.
	results, err := c.SyncSearchSortedLimit(t, modelName, modelVersion, cond, nil, 1001)
	if err != nil {
		t.Fatalf("SyncSearch (limit=1001): %v", err)
	}
	if len(results) != 1001 {
		t.Errorf("limit=1001: got %d results, want 1001", len(results))
	}

	// limit=0 means unbounded at the SPI, so the transport must reject it
	// rather than hand out an unbounded synchronous search.
	status, body, err = c.SyncSearchRawLimit(t, modelName, modelVersion, cond, 0)
	if err != nil {
		t.Fatalf("SyncSearch (limit=0): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("limit=0: got status %d, want 400; body=%s", status, body)
	}
	if !containsErrorCode(body, "BAD_REQUEST") {
		t.Errorf("limit=0: expected errorCode BAD_REQUEST, body=%s", body)
	}
}

// RunWorkflowCriteriaSelectingWorkflow verifies workflow selection by
// criterion. Two workflows on the same model: the first has a criterion
// that always evaluates false (amount > 1000, but we create with amount=50),
// the second has a criterion that always evaluates true. The entity
// should reach the second workflow's end state (STD_CREATED).
// Port of internal/e2e TestWorkflowProc_WorkflowSelection.
func RunWorkflowCriteriaSelectingWorkflow(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-wf-selection"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.1", "name": "premium-wf", "initialState": "NONE", "active": true,
				"criterion": {"type": "function", "function": {"name": "select-premium"}},
				"states": {
					"NONE": {"transitions": [{"name": "init", "next": "PREMIUM_CREATED", "manual": false}]},
					"PREMIUM_CREATED": {}
				}
			},
			{
				"version": "1.1", "name": "standard-wf", "initialState": "NONE", "active": true,
				"criterion": {"type": "function", "function": {"name": "select-standard"}},
				"states": {
					"NONE": {"transitions": [{"name": "init", "next": "STD_CREATED", "manual": false}]},
					"STD_CREATED": {}
				}
			}
		]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":50,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	if got.Meta.State != "STD_CREATED" {
		t.Errorf("expected STD_CREATED (standard workflow selected), got %s", got.Meta.State)
	}
}

// RunSearchFunctionCondition400 asserts a `function` clause in a search
// condition is rejected with HTTP 400 and errorCode INVALID_CONDITION on
// every backend, at the ValidateCondition boundary — before any store is
// touched, so no backend gets a chance to diverge on it.
//
// A function clause is a workflow/transition-criterion shape: the engine
// dispatches it to a compute member. Search has no dispatcher, so before this
// rejection the clause fell through to the in-memory evaluator and produced a
// 500 on every backend identically — but the divergence risk is real, because
// the clause also defeats pushdown translation and each backend reaches the
// evaluator by its own route.
func RunSearchFunctionCondition400(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-function-400"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	const cond = `{"type":"function","function":{"name":"approval-check","config":{"calculationNodesTags":"approval-service"}}}`
	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearchRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "INVALID_CONDITION") {
		t.Errorf("expected errorCode INVALID_CONDITION, body=%s", body)
	}
}

// RunSearchFunctionConditionNestedInGroup400 is the depth counterpart: the
// walker must find a function clause buried inside a group on every backend.
// Left unrejected, an OR group whose earlier child matched short-circuited
// past the function clause entirely and returned 200 with results — a
// silently wrong answer rather than a loud failure.
func RunSearchFunctionConditionNestedInGroup400(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-function-nested-400"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	const cond = `{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"},
			{"type":"function","function":{"name":"approval-check"}}
		]
	}`
	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearchRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "INVALID_CONDITION") {
		t.Errorf("expected errorCode INVALID_CONDITION, body=%s", body)
	}
}
