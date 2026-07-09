package parity

import (
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
