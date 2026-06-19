package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// temporalWorkflowJSON is a workflow with NONE->CREATED (auto) and
// CREATED->CREATED (manual "UPDATE") transitions. Used by temporal
// scenarios that need to update entities to create multiple versions.
const temporalWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.0",
		"name": "temporal-workflow",
		"initialState": "NONE",
		"active": true,
		"states": {
			"NONE":    {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "UPDATE", "next": "CREATED", "manual": true}]}
		}
	}]
}`

// setupTemporalWorkflow imports a model, locks it, and imports the
// temporal workflow (NONE->CREATED auto, CREATED->CREATED manual UPDATE).
func setupTemporalWorkflow(t *testing.T, c *client.Client, modelName string, modelVersion int) {
	t.Helper()

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Temporal","amount":0,"status":"init"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, temporalWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// RunTemporalPointInTimeRetrieval creates an entity, updates it multiple times,
// and verifies that GetEntityAt returns the correct version for each point in
// time. This is the core test for the bi-temporal entity versioning model.
//
// Ported from internal/e2e/TestTemporal_PointInTimeRetrieval.
func RunTemporalPointInTimeRetrieval(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "temporal-pit-test"
	const modelVersion = 1

	setupTemporalWorkflow(t, c, modelName, modelVersion)

	// Record time before entity creation.
	beforeCreate := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	// Create entity v1: amount=100, status="v1".
	entityID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Temporal","amount":100,"status":"v1"}`)
	if err != nil {
		t.Fatalf("CreateEntity v1: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	afterCreate := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	// Update entity v2: amount=200, status="v2".
	if err := c.UpdateEntity(t, entityID, "UPDATE",
		`{"name":"Temporal","amount":200,"status":"v2"}`); err != nil {
		t.Fatalf("UpdateEntity v2: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	afterUpdate1 := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	// Update entity v3: amount=300, status="v3".
	if err := c.UpdateEntity(t, entityID, "UPDATE",
		`{"name":"Temporal","amount":300,"status":"v3"}`); err != nil {
		t.Fatalf("UpdateEntity v3: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Current (no pointInTime) should be v3.
	current, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity current: %v", err)
	}
	if s, _ := current.Data["status"].(string); s != "v3" {
		t.Errorf("current entity: expected status=v3, got %q", s)
	}
	if a, _ := current.Data["amount"].(float64); a != 300 {
		t.Errorf("current entity: expected amount=300, got %v", a)
	}

	// GetEntityAt(afterCreate) should be v1.
	v1, err := c.GetEntityAt(t, entityID, afterCreate)
	if err != nil {
		t.Fatalf("GetEntityAt(afterCreate): %v", err)
	}
	if s, _ := v1.Data["status"].(string); s != "v1" {
		t.Errorf("as-at afterCreate: expected status=v1, got %q", s)
	}
	if a, _ := v1.Data["amount"].(float64); a != 100 {
		t.Errorf("as-at afterCreate: expected amount=100, got %v", a)
	}

	// GetEntityAt(afterUpdate1) should be v2.
	v2, err := c.GetEntityAt(t, entityID, afterUpdate1)
	if err != nil {
		t.Fatalf("GetEntityAt(afterUpdate1): %v", err)
	}
	if s, _ := v2.Data["status"].(string); s != "v2" {
		t.Errorf("as-at afterUpdate1: expected status=v2, got %q", s)
	}
	if a, _ := v2.Data["amount"].(float64); a != 200 {
		t.Errorf("as-at afterUpdate1: expected amount=200, got %v", a)
	}

	// GetEntityAt(beforeCreate) should return 404 -- entity didn't exist yet.
	status, _, err := c.GetEntityAtRaw(t, entityID, beforeCreate)
	if err != nil {
		t.Errorf("as-at beforeCreate: transport error: %v", err)
	}
	if status != 404 {
		t.Errorf("as-at beforeCreate: expected status 404, got %d", status)
	}

	// Verify entity version history via GetEntityChanges (replaces queryDB).
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	if len(changes) < 3 {
		t.Errorf("GetEntityChanges: expected >= 3 entries, got %d", len(changes))
	}
}

// RunTemporalGetAsAtPopulatesFullMeta pins the contract that GetAsAt returns
// a fully populated meta envelope (state, creationDate, lastUpdateTime,
// transactionId, id) across all backends. Historically some wire-level
// stores returned Meta.State and Meta.CreationDate as zero values on the
// point-in-time read path.
func RunTemporalGetAsAtPopulatesFullMeta(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "temporal-meta-test"
	const modelVersion = 1

	// Setup: model + workflow with NONE->DRAFT (auto transition).
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"MetaTest","value":1}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	draftWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0",
			"name": "draft-wf",
			"initialState": "NONE",
			"active": true,
			"states": {
				"NONE":  {"transitions": [{"name": "create", "next": "DRAFT", "manual": false}]},
				"DRAFT": {}
			}
		}]
	}`
	if err := c.ImportWorkflow(t, modelName, modelVersion, draftWF); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	beforeCreate := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"MetaTest","value":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	afterCreate := time.Now().UTC()

	// The point-in-time read.
	got, err := c.GetEntityAt(t, entityID, afterCreate)
	if err != nil {
		t.Fatalf("GetEntityAt(afterCreate): %v", err)
	}

	// Assert Meta.State is populated (was "" before the fix).
	if got.Meta.State != "DRAFT" {
		t.Errorf("Meta.State: got %q, want %q", got.Meta.State, "DRAFT")
	}

	// Assert Meta.CreationDate is non-zero and in the expected window.
	if got.Meta.CreationDate.IsZero() {
		t.Error("Meta.CreationDate is the zero time -- not populated")
	} else {
		lower := beforeCreate.Add(-1 * time.Second)
		upper := afterCreate.Add(1 * time.Second)
		if got.Meta.CreationDate.Before(lower) || got.Meta.CreationDate.After(upper) {
			t.Errorf("Meta.CreationDate %v outside expected window [%v, %v]",
				got.Meta.CreationDate, lower, upper)
		}
	}

	// Assert Meta.LastUpdateTime is non-zero.
	if got.Meta.LastUpdateTime.IsZero() {
		t.Error("Meta.LastUpdateTime is the zero time -- not populated")
	}

	// Assert Meta.TransactionID is non-empty.
	if got.Meta.TransactionID == "" {
		t.Error("Meta.TransactionID is empty -- not populated")
	}

	// Assert Meta.ID matches.
	if got.Meta.ID != entityID.String() {
		t.Errorf("Meta.ID: got %q, want %q", got.Meta.ID, entityID.String())
	}
}
