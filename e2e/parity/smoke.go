package parity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSmokeTest validates the basic create-and-read pipeline against any
// backend. It is intentionally minimal: import a model, lock it, define
// a workflow with a single auto-transition to a CREATED state, create
// an entity, read it back, assert state == "CREATED" and the data
// round-trips. The point of the smoke test is not to exercise the
// system; the point is to prove the parity infrastructure (subprocess
// launch, JWT auth, HTTP client, fixture lifecycle) actually works
// against a real cyoda binary.
//
// Every parity scenario follows roughly this template:
//  1. Mint a fresh tenant via fix.NewTenant(t).
//  2. Build a parity HTTP client with the tenant's token.
//  3. Set up the test scenario (model, workflow, entities, etc.).
//  4. Exercise the operation under test.
//  5. Assert the observable result through the API only.
//
// Tenant isolation between tests is provided by NewTenant returning a
// fresh tenant ID per call — tests cannot pollute each other.
func RunSmokeTest(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	const modelName = "smoke-test"
	const modelVersion = 1

	// 1. Import model from a sample document.
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Smoke","amount":1}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}

	// 2. Lock the model so workflows can attach.
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	// 3. Import a trivial workflow: NONE -> CREATED via an automatic
	//    transition with no processors and no criteria.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0",
			"name": "smoke-wf",
			"initialState": "NONE",
			"active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	if err := c.ImportWorkflow(t, modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// 4. Create an entity.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":42}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if entityID == uuid.Nil {
		t.Fatal("CreateEntity returned nil entity ID")
	}

	// 5. Read it back. State must be CREATED, data must round-trip.
	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	if got.Meta.State != "CREATED" {
		t.Errorf("Meta.State: got %q, want %q", got.Meta.State, "CREATED")
	}
	if got.Meta.ID != entityID.String() {
		t.Errorf("Meta.ID: got %q, want %q", got.Meta.ID, entityID.String())
	}
	if got.Meta.CreationDate.IsZero() {
		t.Error("Meta.CreationDate is the zero time — not populated")
	}
	if got.Data["name"] != "Alice" {
		t.Errorf("data.name: got %v, want \"Alice\"", got.Data["name"])
	}
	if got.Data["amount"] != float64(42) {
		t.Errorf("data.amount: got %v, want 42", got.Data["amount"])
	}
}
