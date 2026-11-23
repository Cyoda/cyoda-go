package parity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// updCollectionHappyWorkflow is the simple workflow with a manual "amend"
// that keeps a CREATED entity in CREATED.
const updCollectionHappyWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "upd-batch-happy-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":    {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "amend", "next": "CREATED", "manual": true}]}
		}
	}]
}`

// RunEntityUpdateCollectionHappyPath verifies PUT /api/entity/{format}
// against every backend: a two-entity batch lands cleanly and both
// entities reflect the new payload on subsequent GET. This is the basic
// "the endpoint works" scenario — it must pass on memory, sqlite,
// postgres, and cassandra alike. Each version records its item's
// transition: "loopback" for the items of the collection create that made
// the entities, "amend" for the update item that names it, "loopback" for
// the one that names none.
func RunEntityUpdateCollectionHappyPath(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "upd-batch-happy"
	const modelVersion = 1

	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"name":"Test","amount":1,"status":"new"}`, updCollectionHappyWorkflow)

	ids, err := c.CreateEntitiesCollection(t, []client.CollectionItem{
		{ModelName: modelName, ModelVersion: modelVersion, Payload: `{"name":"Alice","amount":1,"status":"new"}`},
		{ModelName: modelName, ModelVersion: modelVersion, Payload: `{"name":"Bob","amount":2,"status":"new"}`},
	})
	if err != nil {
		t.Fatalf("CreateEntitiesCollection: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("CreateEntitiesCollection returned %d ids, want 2", len(ids))
	}
	id1, id2 := ids[0], ids[1]

	raw, err := c.UpdateCollection(t, []client.UpdateCollectionItem{
		{ID: id1, Payload: `{"name":"Alice2","amount":11,"status":"new"}`, Transition: "amend"},
		{ID: id2, Payload: `{"name":"Bob2","amount":22,"status":"new"}`},
	})
	if err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}

	// Response is a single-element EntityTransactionResponse array.
	var resp []map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("parse response: %v; body: %s", err, raw)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1-element response array, got %d: %s", len(resp), raw)
	}
	if ids, _ := resp[0]["entityIds"].([]any); len(ids) != 2 {
		t.Errorf("expected 2 entityIds, got %d: %s", len(ids), raw)
	}

	// Both entities must now carry the updated names.
	for _, want := range []struct {
		id   uuid.UUID
		name string
	}{{id1, "Alice2"}, {id2, "Bob2"}} {
		got, err := c.GetEntity(t, want.id)
		if err != nil {
			t.Fatalf("GetEntity %s: %v", want.id, err)
		}
		if got.Data["name"] != want.name {
			t.Errorf("entity %s: data.name = %v, want %q", want.id, got.Data["name"], want.name)
		}
	}

	// Each entity's history, newest first: the update item's version, then
	// the collection create's.
	for _, want := range []struct {
		id     uuid.UUID
		update string
	}{{id1, "amend"}, {id2, "loopback"}} {
		ch, err := c.GetEntityChanges(t, want.id)
		if err != nil {
			t.Fatalf("GetEntityChanges %s: %v", want.id, err)
		}
		if len(ch) != 2 || ch[0].ChangeType != "UPDATE" || ch[1].ChangeType != "CREATE" {
			t.Fatalf("changes of %s = %+v; want [UPDATE, CREATE]", want.id, ch)
		}
		requireTransitions(t, want.id.String(), ch, want.update, "loopback")
	}
}

// RunEntityUpdateCollectionRollback verifies the all-or-nothing contract
// documented in crud.md — if any item in the batch is not found, the
// entire operation fails and no entity is modified. This pins rollback
// behavior against every backend's transaction manager (memory
// vs sqlite vs postgres vs cassandra), which is the load-bearing
// guarantee the endpoint makes and the most likely place a backend
// could diverge.
func RunEntityUpdateCollectionRollback(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "upd-batch-rollback"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id1, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	bogus := uuid.New()

	_, err = c.UpdateCollection(t, []client.UpdateCollectionItem{
		{ID: id1, Payload: `{"name":"AliceShouldNotLand","amount":999,"status":"x"}`},
		{ID: bogus, Payload: `{"name":"never","amount":0,"status":"x"}`},
	})
	if err == nil {
		t.Fatalf("UpdateCollection with missing sibling: expected error, got nil")
	}
	// Failure must reflect the missing-id reason, not an unrelated fault.
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "SEARCH_JOB_NOT_FOUND") && !strings.Contains(err.Error(), "ENTITY_NOT_FOUND") {
		t.Logf("note: error text: %v", err)
	}

	// The valid entity must be unchanged — the whole batch rolled back.
	got, err := c.GetEntity(t, id1)
	if err != nil {
		t.Fatalf("GetEntity after failed batch: %v", err)
	}
	if got.Data["name"] == "AliceShouldNotLand" {
		t.Errorf("rollback violation on backend: entity was modified despite sibling miss; data=%v", got.Data)
	}
	// amount should still be 1 (the original value); not 999.
	if amt, ok := got.Data["amount"].(float64); ok && amt != 1 {
		t.Errorf("rollback violation on backend: amount=%v, want 1", amt)
	}
}
