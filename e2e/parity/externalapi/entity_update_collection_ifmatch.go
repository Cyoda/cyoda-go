package externalapi

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	// External API scenario suite — issue #228 contract surface.
	// Per-item ifMatch isolation on UpdateCollection plus the
	// chunk-rollback contract for non-conflict per-item failures.
	parity.Register(
		parity.NamedTest{
			Name: "ExternalAPI_05_BulkUpdateIfMatchPerItemIsolation",
			Fn:   RunExternalAPI_05_BulkUpdateIfMatchPerItemIsolation,
		},
		parity.NamedTest{
			Name: "ExternalAPI_05_BulkUpdateChunkRollback",
			Fn:   RunExternalAPI_05_BulkUpdateChunkRollback,
		},
	)
}

// RunExternalAPI_05_BulkUpdateIfMatchPerItemIsolation pins the per-item
// ENTITY_MODIFIED isolation contract on UpdateCollection (issue #228):
// when one item in a bulk update carries a stale ifMatch, that item
// surfaces in the chunk's `failed` array while its successful siblings
// still commit.
//
// Steps:
//  1. Register 3 entities and capture each entity's current
//     transactionId via GetEntity (used as the ifMatch token).
//  2. Modify entity[1] independently via single PUT (loopback) so its
//     transactionId rolls forward — the original token is now stale.
//  3. POST a bulk-update of all three items where items 0 and 2 carry
//     their original (still-current) ifMatch tokens, and item 1 carries
//     its original (now-stale) token.
//  4. Assert HTTP 200 with one chunk element that has:
//       - non-empty transactionId
//       - entityIds == [id0, id2]
//       - failed == [{entityId: id1, error.code: ENTITY_MODIFIED,
//                     error.itemIndex: 1}]
//  5. Read entities back: id0 and id2 reflect the bulk-update payload;
//     id1 reflects the intervening modification (NOT the bulk-update).
func RunExternalAPI_05_BulkUpdateIfMatchPerItemIsolation(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	const (
		modelName    = "bulk-ifmatch-iso"
		modelVersion = 1
	)
	if err := d.CreateModelFromSample(modelName, modelVersion,
		`{"name":"x","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel(modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// A trivial workflow with initialState=CREATED and no transitions
	// admits arbitrary loopback updates without complicating the
	// ifMatch routing surface — same shape as the e2e test fixture
	// for this contract (collection_update_ifmatch_test.go).
	if err := d.ImportWorkflow(modelName, modelVersion, trivialWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// 1. Seed 3 entities. We use single-item creates because
	// CreateEntity returns the new entity ID directly; a bulk-create
	// would force us to parse the chunk array just for the ID slice.
	ids := make([]uuid.UUID, 3)
	for i := range ids {
		id, err := d.CreateEntity(modelName, modelVersion,
			`{"name":"orig","amount":1,"status":"new"}`)
		if err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
		ids[i] = id
	}

	// Capture each entity's pre-bulk transactionId. Used both as the
	// ifMatch tokens and as the source of the staleness for ids[1].
	originalTxIDs := make([]string, 3)
	for i, id := range ids {
		got, err := d.GetEntity(id)
		if err != nil {
			t.Fatalf("GetEntity[%d] for txid: %v", i, err)
		}
		if got.Meta.TransactionID == "" {
			t.Fatalf("entity %s: GetEntity returned empty meta.transactionId", id)
		}
		originalTxIDs[i] = got.Meta.TransactionID
	}

	// 2. Bump ids[1]'s transactionId by an independent loopback PUT.
	// After this, originalTxIDs[1] is stale; the other tokens are still
	// current.
	if err := d.UpdateEntityData(ids[1],
		`{"name":"intervening","amount":42,"status":"upd"}`); err != nil {
		t.Fatalf("UpdateEntityData (intervening on ids[1]): %v", err)
	}

	// 3. Bulk-update all three with per-item ifMatch.
	bulkItems := []driver.UpdateCollectionItem{
		{ID: ids[0], Payload: `{"name":"bulk-0","amount":10,"status":"upd"}`, IfMatch: originalTxIDs[0]},
		{ID: ids[1], Payload: `{"name":"bulk-1","amount":11,"status":"upd"}`, IfMatch: originalTxIDs[1]},
		{ID: ids[2], Payload: `{"name":"bulk-2","amount":12,"status":"upd"}`, IfMatch: originalTxIDs[2]},
	}
	rawBody, err := d.UpdateEntitiesCollection(bulkItems)
	if err != nil {
		t.Fatalf("UpdateEntitiesCollection: %v", err)
	}

	// 4. Assert response shape. One chunk; id0 and id2 in entityIds;
	// id1 isolated into failed[].
	type itemErr struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		ItemIndex int    `json:"itemIndex"`
	}
	type itemFailure struct {
		EntityID string  `json:"entityId"`
		Error    itemErr `json:"error"`
	}
	type chunk struct {
		TransactionID string        `json:"transactionId"`
		EntityIDs     []string      `json:"entityIds"`
		Failed        []itemFailure `json:"failed"`
	}
	var chunks []chunk
	if err := json.Unmarshal(rawBody, &chunks); err != nil {
		t.Fatalf("decode chunked response: %v; body=%s", err, rawBody)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body=%s", len(chunks), rawBody)
	}
	c := chunks[0]
	if c.TransactionID == "" {
		t.Errorf("chunk.transactionId is empty; body=%s", rawBody)
	}
	wantIDs := map[string]bool{ids[0].String(): true, ids[2].String(): true}
	if len(c.EntityIDs) != 2 {
		t.Errorf("chunk.entityIds: got %d entries, want 2; body=%s", len(c.EntityIDs), rawBody)
	}
	for _, gotID := range c.EntityIDs {
		if !wantIDs[gotID] {
			t.Errorf("chunk.entityIds: unexpected id %q; want one of [%s, %s]",
				gotID, ids[0], ids[2])
		}
	}
	if len(c.Failed) != 1 {
		t.Fatalf("chunk.failed: got %d entries, want 1; body=%s", len(c.Failed), rawBody)
	}
	if c.Failed[0].EntityID != ids[1].String() {
		t.Errorf("chunk.failed[0].entityId: got %q, want %q",
			c.Failed[0].EntityID, ids[1])
	}
	if c.Failed[0].Error.Code != "ENTITY_MODIFIED" {
		t.Errorf("chunk.failed[0].error.code: got %q, want ENTITY_MODIFIED",
			c.Failed[0].Error.Code)
	}
	if c.Failed[0].Error.ItemIndex != 1 {
		t.Errorf("chunk.failed[0].error.itemIndex: got %d, want 1",
			c.Failed[0].Error.ItemIndex)
	}

	// 5. Read entities back. id0 and id2 reflect the bulk update;
	// id1 reflects the intervening modification, NOT the bulk update.
	for _, want := range []struct {
		id      uuid.UUID
		gotName string
	}{
		{ids[0], "bulk-0"},
		{ids[2], "bulk-2"},
	} {
		got, err := d.GetEntity(want.id)
		if err != nil {
			t.Fatalf("GetEntity post-bulk %s: %v", want.id, err)
		}
		if got.Data["name"] != want.gotName {
			t.Errorf("entity %s: data.name = %v, want %q (bulk-update did not land)",
				want.id, got.Data["name"], want.gotName)
		}
	}
	got1, err := d.GetEntity(ids[1])
	if err != nil {
		t.Fatalf("GetEntity post-bulk %s: %v", ids[1], err)
	}
	if got1.Data["name"] != "intervening" {
		t.Errorf("entity %s: data.name = %v, want %q (stale-ifMatch update should NOT have landed)",
			ids[1], got1.Data["name"], "intervening")
	}
}

// RunExternalAPI_05_BulkUpdateChunkRollback pins the chunk-rollback
// contract from issue #228: per-item ifMatch isolation is reserved for
// ENTITY_MODIFIED conflicts; any OTHER per-item failure (validation,
// missing entity, engine error like an invalid transition) still rolls
// the entire chunk back. With one chunk total, that surfaces as a 4xx
// response and none of the items in the batch are persisted.
//
// Steps:
//  1. Register 3 entities and capture their pre-bulk payloads.
//  2. POST a bulk-update of 3 items where item 1 carries an invalid
//     transition name (a name absent from the model's workflow). The
//     engine returns TRANSITION_NOT_FOUND, NOT ErrConflict.
//  3. Assert HTTP 4xx (chunk 0 rolled back).
//  4. Read all three entities back and confirm none of them reflect the
//     bulk-update payload — the rollback covered every item.
func RunExternalAPI_05_BulkUpdateChunkRollback(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	const (
		modelName    = "bulk-chunk-rollback"
		modelVersion = 1
	)
	if err := d.CreateModelFromSample(modelName, modelVersion,
		`{"name":"x","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel(modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := d.ImportWorkflow(modelName, modelVersion, trivialWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// 1. Seed 3 entities and capture original payloads.
	ids := make([]uuid.UUID, 3)
	originalNames := []string{"orig-0", "orig-1", "orig-2"}
	for i := range ids {
		id, err := d.CreateEntity(modelName, modelVersion,
			`{"name":"`+originalNames[i]+`","amount":`+strconv.Itoa(i)+`,"status":"new"}`)
		if err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
		ids[i] = id
	}

	// 2. Bulk-update with an invalid transition on item 1.
	// "no-such-transition" is not declared anywhere in trivialWorkflowJSON.
	bulkItems := []driver.UpdateCollectionItem{
		{ID: ids[0], Payload: `{"name":"bulk-0","amount":10,"status":"upd"}`},
		{ID: ids[1], Payload: `{"name":"bulk-1","amount":11,"status":"upd"}`, Transition: "no-such-transition"},
		{ID: ids[2], Payload: `{"name":"bulk-2","amount":12,"status":"upd"}`},
	}
	status, body, err := d.UpdateEntitiesCollectionRawWithWindow(bulkItems, 0)
	if err != nil {
		t.Fatalf("UpdateEntitiesCollectionRawWithWindow: %v", err)
	}

	// 3. Chunk-0 rollback surfaces as a 4xx response per the contract
	// in cmd/cyoda/help/content/crud.md and OpenAPI.
	if status < 400 || status >= 500 {
		t.Fatalf("status: got %d, want 4xx (invalid-transition rolls chunk 0 back); body=%s",
			status, body)
	}

	// 4. None of the three entities should reflect the bulk-update
	// payload — the chunk rolled back atomically.
	for i, id := range ids {
		got, err := d.GetEntity(id)
		if err != nil {
			t.Fatalf("GetEntity[%d] %s: %v", i, id, err)
		}
		if got.Data["name"] != originalNames[i] {
			t.Errorf("entity %s: data.name = %v, want %q (chunk rollback failed to cover item %d)",
				id, got.Data["name"], originalNames[i], i)
		}
	}
}

// trivialWorkflowJSON is a workflow that admits arbitrary loopback
// updates and exposes no manual transitions: initialState=CREATED with
// no declared transitions. Used by issue-#228 ifMatch parity scenarios
// because they pin the ifMatch routing, not workflow behavior.
const trivialWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1",
		"name": "trivial-wf",
		"initialState": "CREATED",
		"active": true,
		"states": {
			"CREATED": {}
		}
	}]
}`

