package parity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

const modelSampleDoc = `{"name": "Test", "amount": 100}`

// RunModelImportAndExport verifies that a model can be imported and then
// exported with SIMPLE_VIEW, and that it appears in the model list.
func RunModelImportAndExport(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "model-import-export-test"
	const modelVersion = 1

	// 1. Import model with sample data.
	if err := c.ImportModel(t, modelName, modelVersion, modelSampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}

	// 2. Export with SIMPLE_VIEW and verify the response has required fields.
	raw, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}

	var exportBody map[string]any
	if err := json.Unmarshal(raw, &exportBody); err != nil {
		t.Fatalf("ExportModel: failed to parse JSON: %v", err)
	}
	if _, ok := exportBody["currentState"]; !ok {
		t.Errorf("ExportModel: expected 'currentState' field in response, got: %v", exportBody)
	}
	if _, ok := exportBody["model"]; !ok {
		t.Errorf("ExportModel: expected 'model' field in response, got: %v", exportBody)
	}

	// 3. Verify the model appears in ListModels (replaces DB-peek).
	models, err := c.ListModels(t)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	found := false
	for _, m := range models {
		if m.ModelName == modelName && m.ModelVersion == modelVersion {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListModels: model %s/%d not found in list", modelName, modelVersion)
	}
}

// RunModelLockAndUnlock verifies the full lock/unlock lifecycle and that the
// currentState field reflects the correct state after each operation.
func RunModelLockAndUnlock(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "model-lock-test"
	const modelVersion = 1

	// 1. Import model.
	if err := c.ImportModel(t, modelName, modelVersion, modelSampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}

	// 2. Lock the model.
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	// 3. Verify state via ListModels — must be LOCKED.
	assertModelState(t, c, modelName, modelVersion, "LOCKED")

	// 4. Unlock the model.
	if err := c.UnlockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("UnlockModel: %v", err)
	}

	// 5. Verify state via ListModels — must be UNLOCKED.
	assertModelState(t, c, modelName, modelVersion, "UNLOCKED")
}

// RunModelListModels verifies that importing two models with different names
// results in both appearing in the list endpoint response.
func RunModelListModels(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelVersion = 1
	names := []string{"model-list-test-a", "model-list-test-b"}

	// 1. Import two models with different names.
	for _, name := range names {
		if err := c.ImportModel(t, name, modelVersion, modelSampleDoc); err != nil {
			t.Fatalf("ImportModel %s: %v", name, err)
		}
	}

	// 2. List models — verify both appear.
	models, err := c.ListModels(t)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	found := make(map[string]bool)
	for _, m := range models {
		found[m.ModelName] = true
	}
	for _, name := range names {
		if !found[name] {
			t.Errorf("ListModels: expected to find model %q in list", name)
		}
	}
}

// RunModelDelete verifies that a model can be deleted and is no longer
// accessible via the export endpoint after deletion.
func RunModelDelete(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "model-delete-test"
	const modelVersion = 1

	// 1. Import model.
	if err := c.ImportModel(t, modelName, modelVersion, modelSampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}

	// 2. Confirm it exists via export.
	if _, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion); err != nil {
		t.Fatalf("ExportModel before delete: %v", err)
	}

	// 3. Delete the model.
	if err := c.DeleteModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}

	// 4. Export should now fail (non-200).
	_, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err == nil {
		t.Error("ExportModel after delete: expected error, got success (model should be gone)")
	}
}

// assertModelState finds the model in ListModels and asserts its CurrentState.
func assertModelState(t *testing.T, c *client.Client, modelName string, modelVersion int, wantState string) {
	t.Helper()
	models, err := c.ListModels(t)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	for _, m := range models {
		if m.ModelName == modelName && m.ModelVersion == modelVersion {
			if m.CurrentState != wantState {
				t.Errorf("model %s/%d: got state %q, want %q", modelName, modelVersion, m.CurrentState, wantState)
			}
			return
		}
	}
	t.Fatalf("model %s/%d not found in ListModels", modelName, modelVersion)
}
