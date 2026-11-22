package externalapi

import (
	"testing"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_05_01_NestedArrayAppendAndModify", Fn: RunExternalAPI_05_01_NestedArrayAppendAndModify},
		parity.NamedTest{Name: "ExternalAPI_05_02_NestedArrayShrinkAndModify", Fn: RunExternalAPI_05_02_NestedArrayShrinkAndModify},
		parity.NamedTest{Name: "ExternalAPI_05_03_RemoveObjectAndArrayKeepOneField", Fn: RunExternalAPI_05_03_RemoveObjectAndArrayKeepOneField},
		parity.NamedTest{Name: "ExternalAPI_05_04_PopulateMinimalIntoFull", Fn: RunExternalAPI_05_04_PopulateMinimalIntoFull},
		parity.NamedTest{Name: "ExternalAPI_05_05_LoopbackAbsentTransition", Fn: RunExternalAPI_05_05_LoopbackAbsentTransition},
		parity.NamedTest{Name: "ExternalAPI_05_06_UnchangedPayloadStillTransitions", Fn: RunExternalAPI_05_06_UnchangedPayloadStillTransitions},
	)
}

// RunExternalAPI_05_01_NestedArrayAppendAndModify — dictionary 05/01.
// Modify first element's inner field and append a second array element.
// YAML: PUT /entity/JSON/{entityId}/UPDATE with updated payload.
func RunExternalAPI_05_01_NestedArrayAppendAndModify(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	sample := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c"}]}`
	if err := d.CreateModelFromSample("upd01", 1, sample); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("upd01", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("upd01", 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	updated := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c-updated"},{"field3":"d"}]}`
	if err := d.UpdateEntity(id, "UPDATE", updated); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	arr, ok := got.Data["arrayField"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("arrayField after append+modify: got %v, want length 2", got.Data["arrayField"])
	}
}

// RunExternalAPI_05_02_NestedArrayShrinkAndModify — dictionary 05/02.
// Shrink a two-element array to one and change a top-level string.
// YAML: PUT /entity/JSON/{entityId}/UPDATE.
func RunExternalAPI_05_02_NestedArrayShrinkAndModify(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	sample := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c"},{"field3":"d"}]}`
	if err := d.CreateModelFromSample("upd02", 1, sample); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("upd02", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("upd02", 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	updated := `{"field1":"a-updated","objectField":{"field2":"b"},"arrayField":[{"field3":"c-updated"}]}`
	if err := d.UpdateEntity(id, "UPDATE", updated); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Data["field1"] != "a-updated" {
		t.Errorf("field1: got %v, want a-updated", got.Data["field1"])
	}
	arr, ok := got.Data["arrayField"].([]any)
	if !ok || len(arr) != 1 {
		t.Errorf("arrayField after shrink: got %v, want length 1", got.Data["arrayField"])
	}
}

// RunExternalAPI_05_03_RemoveObjectAndArrayKeepOneField — dictionary 05/03.
// Drop objectField and arrayField entirely; only field1 remains.
// YAML: PUT /entity/JSON/{entityId}/UPDATE.
func RunExternalAPI_05_03_RemoveObjectAndArrayKeepOneField(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	sample := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c"},{"field3":"d"}]}`
	if err := d.CreateModelFromSample("upd03", 1, sample); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("upd03", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("upd03", 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := d.UpdateEntity(id, "UPDATE", `{"field1":"a-updated"}`); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Data["field1"] != "a-updated" {
		t.Errorf("field1: got %v, want a-updated", got.Data["field1"])
	}
}

// RunExternalAPI_05_04_PopulateMinimalIntoFull — dictionary 05/04.
// Start with a minimal entity (field1 only), then update to add nested
// object and array.
//
// The model must be created from the full structure so that objectField and
// arrayField are valid fields; the initial *entity* is the minimal one.
// This mirrors the YAML's shared-model context (scenarios 01–04 all use the
// same testTreeNodeEntityUpdate model, so the full schema is registered by
// the time 04 runs). In our isolated per-tenant tests we replicate that by
// registering the full sample once before locking.
// YAML: PUT /entity/JSON/{entityId}/UPDATE.
func RunExternalAPI_05_04_PopulateMinimalIntoFull(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	// Register the model with the full field set so the update is valid.
	fullSample := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c"}]}`
	if err := d.CreateModelFromSample("upd04", 1, fullSample); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("upd04", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// Create the entity with the minimal payload only.
	id, err := d.CreateEntity("upd04", 1, `{"field1":"a"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	updated := `{"field1":"a-updated","objectField":{"field2":"b"},"arrayField":[{"field3":"c-updated"},{"field3":"hello"}]}`
	if err := d.UpdateEntity(id, "UPDATE", updated); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	obj, _ := got.Data["objectField"].(map[string]any)
	if obj == nil || obj["field2"] != "b" {
		t.Errorf("objectField.field2 after populate: got %v", got.Data["objectField"])
	}
	arr, ok := got.Data["arrayField"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("arrayField after populate: got %v, want length 2", got.Data["arrayField"])
	}
}

// RunExternalAPI_05_05_LoopbackAbsentTransition — dictionary 05/05.
// PUT /entity/JSON/{id} (no transition path segment) performs the loopback.
// YAML: update_entity_loopback → PUT /entity/JSON/{entityId}.
func RunExternalAPI_05_05_LoopbackAbsentTransition(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	sample := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c"}]}`
	if err := d.CreateModelFromSample("upd05", 1, sample); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("upd05", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("upd05", 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	updated := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c-updated"},{"field3":"d"}]}`
	if err := d.UpdateEntityData(id, updated); err != nil {
		t.Fatalf("UpdateEntityData (loopback): %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	arr, ok := got.Data["arrayField"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("arrayField after loopback: got %v, want length 2", got.Data["arrayField"])
	}
}

// RunExternalAPI_05_06_UnchangedPayloadStillTransitions — dictionary 05/06.
// PUT identical payload still advances the workflow transition. For
// tranche 2 we verify the call returns nil and entity count stays at 1;
// deeper audit-event assertions are tranche-3 work.
// YAML: update_entity_transition → PUT /entity/JSON/{entityId}/UPDATE.
func RunExternalAPI_05_06_UnchangedPayloadStillTransitions(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	sample := `{"field1":"a","objectField":{"field2":"b"},"arrayField":[{"field3":"c"}]}`
	if err := d.CreateModelFromSample("upd06", 1, sample); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("upd06", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("upd06", 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := d.UpdateEntity(id, "UPDATE", sample); err != nil {
		t.Fatalf("UpdateEntity (same payload): %v", err)
	}
	list, err := d.ListEntitiesByModel("upd06", 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("entity count after unchanged-payload transition: got %d, want 1", len(list))
	}
}
