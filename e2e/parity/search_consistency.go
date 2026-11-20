package parity

import (
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunSearchIndexImmediateConsistency creates an entity then immediately
// (no sleep, no polling) searches for it via /search/direct/. The search
// MUST return the entity. This proves the immediate read-after-write
// contract on the search index path. Particularly important for backends
// where the index may be eventually consistent.
func RunSearchIndexImmediateConsistency(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "search-immediate-test"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	// Create entity with a distinctive field.
	entityID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Searchable","amount":777,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Immediately search (NO sleep, NO polling) for entities with
	// amount == 777. The search MUST return the entity we just created.
	condition := `{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":777}`
	results, err := c.SyncSearch(t, modelName, modelVersion, condition)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Meta.ID == entityID.String() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SyncSearch did not return entity %s immediately after creation; got %d results", entityID, len(results))
	}
}
