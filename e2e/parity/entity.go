package parity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// simpleWorkflowJSON is a minimal workflow with a single auto-transition
// from NONE to CREATED, no processors, and no criteria. Entity CRUD
// tests use this because they don't need workflow complexity.
const simpleWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.0",
		"name": "simple-wf",
		"initialState": "NONE",
		"active": true,
		"states": {
			"NONE":    {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
			"CREATED": {}
		}
	}]
}`

// setupSimpleWorkflow imports a model, locks it, and imports the simple
// auto-transition workflow. Multiple entity CRUD scenarios reuse this
// to avoid repeating model+workflow setup boilerplate.
func setupSimpleWorkflow(t *testing.T, c *client.Client, modelName string, modelVersion int) {
	t.Helper()

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, simpleWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// RunEntityCreateAndGet verifies that an entity can be created and then
// retrieved by ID. Asserts that the data round-trips, the state machine
// transitions to CREATED, and meta fields (ID, creationDate) are populated.
// Also checks GetEntityChanges for at least one version history entry.
func RunEntityCreateAndGet(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-create-get-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	// Create entity.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":30,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if entityID == uuid.Nil {
		t.Fatal("CreateEntity returned nil entity ID")
	}

	// Get entity by ID — replaces entities table DB-peek.
	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	// Verify domain data round-trips.
	if got.Data["name"] != "Alice" {
		t.Errorf("data.name: got %v, want \"Alice\"", got.Data["name"])
	}
	if got.Data["amount"] != float64(30) {
		t.Errorf("data.amount: got %v, want 30", got.Data["amount"])
	}
	if got.Data["status"] != "active" {
		t.Errorf("data.status: got %v, want \"active\"", got.Data["status"])
	}

	// Verify meta fields.
	if got.Meta.ID != entityID.String() {
		t.Errorf("Meta.ID: got %q, want %q", got.Meta.ID, entityID.String())
	}
	if got.Meta.State != "CREATED" {
		t.Errorf("Meta.State: got %q, want %q", got.Meta.State, "CREATED")
	}
	if got.Meta.CreationDate.IsZero() {
		t.Error("Meta.CreationDate is the zero time - not populated")
	}
	if got.Meta.LastUpdateTime.IsZero() {
		t.Error("Meta.LastUpdateTime is the zero time - not populated")
	}

	// Verify entity version history exists — replaces entity_versions DB-peek.
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	if len(changes) < 1 {
		t.Errorf("GetEntityChanges: expected >= 1 entry, got %d", len(changes))
	}
}

// RunEntityDelete verifies that a deleted entity returns an error (404)
// on subsequent GET.
func RunEntityDelete(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-delete-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	// Create entity.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":25,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Delete entity.
	if err := c.DeleteEntity(t, entityID); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	// Get entity — must fail (404).
	_, err = c.GetEntity(t, entityID)
	if err == nil {
		t.Error("GetEntity after delete: expected error, got success (entity should be gone)")
	}
}

// RunEntityListByModel verifies that creating multiple entities for a model
// results in all of them appearing in the list endpoint response.
func RunEntityListByModel(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-list-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	// Create 2 entities.
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","amount":40,"status":"draft"}`); err != nil {
		t.Fatalf("CreateEntity 1: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Dave","amount":35,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity 2: %v", err)
	}

	// List entities by model.
	entities, err := c.ListEntitiesByModel(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(entities) < 2 {
		t.Errorf("ListEntitiesByModel: expected at least 2 entities, got %d", len(entities))
	}
}
