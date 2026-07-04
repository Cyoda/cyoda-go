package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunEntityMetaShape asserts the canonical meta shape on every backend (E1):
//   - single-entity GET: id/state/creationDate/lastUpdateTime always present,
//     modelKey populated (uniform across all read paths, A2 abandoned).
//   - list GET (getAllEntities): modelKey also present, locking the
//     uniform-modelKey contract cross-backend.
func RunEntityMetaShape(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-meta-shape"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":30,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.ID != id.String() {
		t.Errorf("meta.id = %q, want %q", got.Meta.ID, id)
	}
	if got.Meta.State == "" {
		t.Error("meta.state empty")
	}
	if got.Meta.CreationDate.IsZero() {
		t.Error("meta.creationDate zero")
	}
	if got.Meta.LastUpdateTime.IsZero() {
		t.Error("meta.lastUpdateTime zero")
	}
	if got.Meta.ModelKey == nil || got.Meta.ModelKey.Name != modelName {
		t.Errorf("meta.modelKey missing/wrong on single-entity GET: %+v", got.Meta.ModelKey)
	}

	// Lock the uniform-modelKey contract on list reads (A2 abandoned).
	entities, err := c.ListEntitiesByModel(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(entities) == 0 {
		t.Fatal("ListEntitiesByModel: expected at least 1 entity")
	}
	found := false
	for _, e := range entities {
		if e.Meta.ID == id.String() {
			found = true
			if e.Meta.ModelKey == nil || e.Meta.ModelKey.Name != modelName {
				t.Errorf("meta.modelKey missing/wrong on list GET: %+v", e.Meta.ModelKey)
			}
			break
		}
	}
	if !found {
		t.Errorf("ListEntitiesByModel: created entity %s not found in list", id)
	}
}

// RunGetAllEntitiesAsAt asserts the model-scoped list read honours pointInTime
// on every backend (E3).
func RunGetAllEntitiesAsAt(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "getall-asat-parity"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":1,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	midpoint := time.Now().UTC()
	time.Sleep(10 * time.Millisecond)
	if err := c.UpdateEntityData(t, id, `{"name":"Bob","amount":2,"status":"active"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}

	asAt, err := c.ListEntitiesByModelAt(t, modelName, modelVersion, midpoint)
	if err != nil {
		t.Fatalf("ListEntitiesByModelAt: %v", err)
	}
	if len(asAt) != 1 {
		t.Fatalf("expected 1 entity as-at midpoint, got %d", len(asAt))
	}
	if got := asAt[0].Data["amount"]; got != float64(1) {
		t.Errorf("as-at amount = %v, want 1 (pre-update)", got)
	}
	if asAt[0].Meta.PointInTime == nil {
		t.Error("meta.pointInTime not populated on as-at list read")
	}
}
