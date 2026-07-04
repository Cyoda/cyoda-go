package parity

import (
	"fmt"
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

// RunEntityConditionalDeleteInTx proves (E2): a SYNC processor's callback
// carrying X-Tx-Token issues the conditional delete inside the joined
// transaction T (the path where SearchService.Search bypasses pushdown, §6.1).
// After commit, only the non-matching subset survives on every backend.
//
// The cb-conditional-delete processor (inside T):
//  1. Creates a "to-delete" entity (status = deleteMarker, uncommitted in T).
//  2. Creates a "to-keep" entity (status = deleteMarker+"-keep", uncommitted in T).
//  3. Issues DELETE /api/entity/{secondary}/1 with condition status==deleteMarker.
//
// Because SearchService.Search uses the tx-aware path inside T, it sees the
// uncommitted "to-delete" entity and deletes it. "to-keep" is not matched and
// commits as a durable entity. If the search fell back to the committed view, it
// would find 0 candidates and delete nothing; the "to-delete" entity would
// survive — detected by the zero-hit assertion on the delete marker.
func RunEntityConditionalDeleteInTx(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-cdel-secondary"
	const primary = "cbtj-cdel-primary"
	const deleteMarker = "cbtj-cdel-delete"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-cdel-wf", "cb-conditional-delete", "SYNC", cbContext(secondary, deleteMarker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE", prim.Meta.State)
	}
	if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
		t.Errorf("SYNC dispatch: tokenWasEmpty=true; want false (real tx-token must be attached)")
	}
	if prim.Data["deleteEntityId"] == "" || prim.Data["keepEntityId"] == "" {
		t.Fatalf("primary data missing entity IDs after processor: %+v", prim.Data)
	}

	// After commit: entities with deleteMarker status must be gone —
	// the conditional delete inside T saw them (tx-aware search, §6.1) and
	// removed them. A non-zero count here means the in-T search did not see
	// the uncommitted entity, so the delete was a no-op (join broken).
	delHits, err := c.SyncSearch(t, secondary, 1,
		fmt.Sprintf(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":%q}`, deleteMarker))
	if err != nil {
		t.Fatalf("search for deleted marker: %v", err)
	}
	if len(delHits) != 0 {
		t.Errorf("found %d entities with delete marker after commit; want 0 — conditional delete inside T was a no-op (search did not see uncommitted entity; §6.1 join broken)", len(delHits))
	}

	// After commit: the "to-keep" entity (non-matching subset) must be durable.
	// At least 1 hit expected: the keeper created in this run.
	keepHits, err := c.SyncSearch(t, secondary, 1,
		fmt.Sprintf(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":%q}`, deleteMarker+"-keep"))
	if err != nil {
		t.Fatalf("search for keep marker: %v", err)
	}
	if len(keepHits) == 0 {
		t.Errorf("found 0 entities with keep marker after commit; want >= 1 — keeper was incorrectly deleted (conditional delete is not selective)")
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
