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
		"version": "1.1",
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

	// Create entity v1: amount=100, status="v1".
	entityID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Temporal","amount":100,"status":"v1"}`)
	if err != nil {
		t.Fatalf("CreateEntity v1: %v", err)
	}
	t1 := LatestChangeTime(t, c, entityID)

	// Sleeps space consecutive versions into distinct instants; the boundaries
	// themselves are derived from the server's clock — see pit_time.go.
	time.Sleep(50 * time.Millisecond)

	// Update entity v2: amount=200, status="v2".
	if err := c.UpdateEntity(t, entityID, "UPDATE",
		`{"name":"Temporal","amount":200,"status":"v2"}`); err != nil {
		t.Fatalf("UpdateEntity v2: %v", err)
	}
	t2 := LatestChangeTime(t, c, entityID)

	time.Sleep(50 * time.Millisecond)

	// Update entity v3: amount=300, status="v3".
	if err := c.UpdateEntity(t, entityID, "UPDATE",
		`{"name":"Temporal","amount":300,"status":"v3"}`); err != nil {
		t.Fatalf("UpdateEntity v3: %v", err)
	}
	t3 := LatestChangeTime(t, c, entityID)

	// Boundaries between consecutive versions, plus one just before the entity
	// existed — 1ms, not a wide margin, so the 404 below still exercises the
	// exclusion boundary rather than a trivially distant past.
	afterCreate := MidpointBetween(t, t1, t2)
	afterUpdate1 := MidpointBetween(t, t2, t3)
	beforeCreate := t1.Add(-time.Millisecond)

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
			"version": "1.1",
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

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"MetaTest","value":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// The newest version's own server-stamped time; as-at that instant resolves
	// to it under the canonical inclusive <= rule — see pit_time.go.
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	tCreate := MinChangeTime(t, changes)
	tLatest := MaxChangeTime(t, changes)

	// The point-in-time read.
	got, err := c.GetEntityAt(t, entityID, tLatest)
	if err != nil {
		t.Fatalf("GetEntityAt(tLatest): %v", err)
	}

	// Assert Meta.State is populated (was "" before the fix).
	if got.Meta.State != "DRAFT" {
		t.Errorf("Meta.State: got %q, want %q", got.Meta.State, "DRAFT")
	}

	// Assert Meta.CreationDate is non-zero and in the expected window.
	// CreationDate must not postdate the entity's FIRST version, and must sit
	// close to it — both values come from the backend's own clock, so this is a
	// real ordering invariant rather than a clock-tolerance window. (Backends
	// differ on the sub-millisecond gap: postgres stamps both from one
	// CURRENT_TIMESTAMP, while memory stamps CreationDate at construction and
	// the version at save, a few hundred µs later.)
	if got.Meta.CreationDate.IsZero() {
		t.Error("Meta.CreationDate is the zero time -- not populated")
	} else if got.Meta.CreationDate.After(tCreate.Add(time.Millisecond)) {
		// The 1ms forward tolerance is for backends that re-stamp CreationDate at
		// save time from a slightly later reading than the version's own — the
		// check targets an unpopulated or nonsense value, not sub-ms ordering.
		t.Errorf("Meta.CreationDate %s postdates the entity's first version time %s",
			got.Meta.CreationDate.Format(time.RFC3339Nano), tCreate.Format(time.RFC3339Nano))
	} else if gap := tCreate.Sub(got.Meta.CreationDate); gap > time.Second {
		t.Errorf("Meta.CreationDate %s is %s before the first version time %s -- implausible",
			got.Meta.CreationDate.Format(time.RFC3339Nano), gap, tCreate.Format(time.RFC3339Nano))
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

// RunPITDeletedSinceInstant asserts that an entity deleted AFTER a captured
// instant is still part of the model's snapshot at that instant, and absent
// from the current (no pointInTime) listing. This is the edge a postgres
// point-in-time read that enumerates live entities and probes each one's
// revision — rather than walking every revision of every entity — must not
// get wrong: a since-deleted entity has no live row to enumerate today, so
// the read must still surface it for an instant that predates the delete.
func RunPITDeletedSinceInstant(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "pit-deleted-since"
	const modelVersion = 1
	setupTemporalWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Temporal","amount":0,"status":"init"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	instant := LatestChangeTime(t, c, id)

	if err := c.DeleteEntity(t, id); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	atInstant, err := c.ListEntitiesByModelAt(t, modelName, modelVersion, instant)
	if err != nil {
		t.Fatalf("ListEntitiesByModelAt(instant): %v", err)
	}
	if len(atInstant) != 1 {
		t.Fatalf("at the instant (before the delete): got %d entities, want 1", len(atInstant))
	}
	if atInstant[0].Meta.ID != id.String() {
		t.Errorf("at the instant: got entity %q, want %q", atInstant[0].Meta.ID, id.String())
	}

	now, err := c.ListEntitiesByModel(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("ListEntitiesByModel (current): %v", err)
	}
	if len(now) != 0 {
		t.Fatalf("now (after the delete): got %d entities, want 0", len(now))
	}
}

// RunPITCreatedAfterInstant asserts that an entity created AFTER a captured
// instant is absent from the model's snapshot at that instant. This is the
// complementary edge to RunPITDeletedSinceInstant: an entity created after
// the requested instant must not appear just because it is currently live.
func RunPITCreatedAfterInstant(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "pit-created-after"
	const modelVersion = 1
	setupTemporalWorkflow(t, c, modelName, modelVersion)

	seed, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Temporal","amount":0,"status":"init"}`)
	if err != nil {
		t.Fatalf("CreateEntity (seed): %v", err)
	}
	instant := LatestChangeTime(t, c, seed)

	// Space the second create ≥2ms past the instant so it lands in a distinct
	// millisecond on the commercial backend, which stores at millisecond
	// precision — see pit_boundary.go for the rationale.
	time.Sleep(2 * time.Millisecond)
	later, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Temporal","amount":1,"status":"init"}`)
	if err != nil {
		t.Fatalf("CreateEntity (later): %v", err)
	}

	atInstant, err := c.ListEntitiesByModelAt(t, modelName, modelVersion, instant)
	if err != nil {
		t.Fatalf("ListEntitiesByModelAt(instant): %v", err)
	}
	if len(atInstant) != 1 {
		t.Fatalf("at the instant (before the second create): got %d entities, want 1", len(atInstant))
	}
	for _, e := range atInstant {
		if e.Meta.ID == later.String() {
			t.Fatalf("an entity created after the instant (%s) appeared in the snapshot at it", later)
		}
	}
	if atInstant[0].Meta.ID != seed.String() {
		t.Errorf("at the instant: got entity %q, want the seed %q", atInstant[0].Meta.ID, seed.String())
	}
}
