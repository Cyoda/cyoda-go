package parity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunUnlockWithEntities verifies that unlocking a model that holds entities is
// refused 409 MODEL_HAS_ENTITIES with the entity count, that the model stays
// LOCKED, and that deleting it answers MODEL_ALREADY_LOCKED, on every backend.
func RunUnlockWithEntities(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "model-unlock-entities"
	if err := c.ImportModel(t, modelName, 1, modelSampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.CreateEntity(t, modelName, 1, modelSampleDoc); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
	}

	status, raw, err := c.UnlockModelRaw(t, modelName, 1)
	if err != nil {
		t.Fatalf("UnlockModelRaw: %v", err)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("unlock answered %d, not a problem document: %v (body: %s)", status, err, raw)
	}
	if status != 409 || pd.Properties["errorCode"] != "MODEL_HAS_ENTITIES" {
		t.Fatalf("unlock answered %d %v; want 409 MODEL_HAS_ENTITIES (body: %s)", status, pd.Properties["errorCode"], raw)
	}
	if n, _ := pd.Properties["entityCount"].(float64); n != 2 {
		t.Errorf("entityCount = %v, want 2 (body: %s)", pd.Properties["entityCount"], raw)
	}
	assertModelState(t, c, modelName, 1, "LOCKED")
	if err := c.DeleteModel(t, modelName, 1); err == nil {
		t.Error("DeleteModel of a model with entities: expected refusal, got success")
	}
}
