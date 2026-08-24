package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// historyReadsWorkflowJSON is a workflow with NONE->CREATED (auto) and
// CREATED->CREATED (manual "UPDATE") transitions — enough to drive a
// cascading create plus repeated loopback updates on one entity.
const historyReadsWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1",
		"name": "history-reads-workflow",
		"initialState": "NONE",
		"active": true,
		"states": {
			"NONE":    {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "UPDATE", "next": "CREATED", "manual": true}]}
		}
	}]
}`

// RunHistoryReadsChangesMetadataAndTransactionLookup seeds 3 saves (a
// cascading create + 2 manual updates) and 1 delete against a single
// entity, then asserts the cross-backend contract for the two purposed
// history reads task E6 rewired onto spi.EntityStore.GetVersionMetadata /
// GetVersionByTransaction (replacing the deleted GetVersionHistory):
//
//   - GET /entity/{id}/changes (getEntityChangesMetadata) returns exactly 4
//     entries, newest-first. spi.EntityStore.GetVersionMetadata's contract
//     is "newest first, ties broken by Version DESC" — a real cross-backend
//     ordering guarantee WITHIN one entity's own history (entity-ID
//     ordering ACROSS different entities is per-engine and deliberately not
//     asserted anywhere in this suite — see GetPage's doc comment and
//     RunListEntitiesPagingConsistency).
//   - The DELETE tombstone row has HasEntity=false
//     (internal/domain/entity/service.go: HasEntity is derived as
//     !v.Deleted from the metadata DTO, a parity fix for a prior
//     backend-divergent v.Entity!=nil probe). HasEntity=false is observed
//     on the wire as an OMITTED transactionId — the handler only sets
//     transactionId when HasEntity is true (see
//     cmd/cyoda/help/content/crud.md). The 3 non-tombstone rows all carry a
//     non-empty transactionId.
//   - GET /entity/{id}?transactionId=<txID> ("by-transaction lookup",
//     entity.getEntityByTransactionID -> store.GetVersionByTransaction)
//     resolves each save's own transaction ID to that save's own data, with
//     no cross-transaction bleed, and 404s for the delete's transaction ID
//     — a DELETED tombstone carries no entity payload for
//     GetVersionByTransaction to surface, so it never matches (see that
//     method's doc comment on cyoda-go-spi's EntityStore interface).
func RunHistoryReadsChangesMetadataAndTransactionLookup(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "history-reads-parity"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, historyReadsWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// Save 1: cascading create (NONE -auto-> CREATED) — one version row at
	// the final, post-cascade state (a non-segmenting cascade with no
	// processor callout persists once, not once per intermediate state).
	entityID, createTxID, err := c.CreateEntityWithTxID(t, modelName, modelVersion,
		`{"name":"v1","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID: %v", err)
	}

	// Saves 2 and 3: two manual loopback updates, each its own transaction.
	update1TxID, err := c.UpdateEntityWithTxID(t, entityID, "UPDATE", `{"name":"v2","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("UpdateEntityWithTxID v2: %v", err)
	}
	update2TxID, err := c.UpdateEntityWithTxID(t, entityID, "UPDATE", `{"name":"v3","amount":3,"status":"new"}`)
	if err != nil {
		t.Fatalf("UpdateEntityWithTxID v3: %v", err)
	}

	// Delete: the tombstone.
	deleteTxID, err := c.DeleteEntityWithTxID(t, entityID)
	if err != nil {
		t.Fatalf("DeleteEntityWithTxID: %v", err)
	}

	txIDs := map[string]bool{createTxID: true, update1TxID: true, update2TxID: true, deleteTxID: true}
	if len(txIDs) != 4 {
		t.Fatalf("expected 4 distinct transaction IDs (create=%s update1=%s update2=%s delete=%s), got %d distinct",
			createTxID, update1TxID, update2TxID, deleteTxID, len(txIDs))
	}

	// --- changes-metadata shape + newest-first ordering ---
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	if len(changes) != 4 {
		t.Fatalf("expected exactly 4 change entries (3 saves + 1 delete), got %d: %+v", len(changes), changes)
	}

	wantOrder := []string{"DELETE", "UPDATE", "UPDATE", "CREATE"}
	for i, want := range wantOrder {
		if got := changes[i].ChangeType; got != want {
			t.Errorf("changes[%d].changeType = %q, want %q (newest-first order): %+v", i, got, want, changes)
		}
	}

	// Tombstone row: HasEntity=false is observed as an omitted transactionId.
	if changes[0].TransactionID != "" {
		t.Errorf("DELETE (tombstone) row: transactionId = %q, want empty (HasEntity=false omits it)", changes[0].TransactionID)
	}
	// The 3 non-tombstone rows all carry a non-empty transactionId (HasEntity=true).
	for i := 1; i < len(changes); i++ {
		if changes[i].TransactionID == "" {
			t.Errorf("changes[%d] (%s): transactionId is empty, want non-empty (HasEntity=true)", i, changes[i].ChangeType)
		}
	}

	// --- by-transaction lookup: each txID resolves to its own save's data ---
	byCreate, err := c.GetEntityByTransactionID(t, entityID, createTxID)
	if err != nil {
		t.Fatalf("GetEntityByTransactionID(create): %v", err)
	}
	if byCreate.Data["name"] != "v1" {
		t.Errorf("GetEntityByTransactionID(create).Data.name = %v, want %q", byCreate.Data["name"], "v1")
	}

	byUpdate1, err := c.GetEntityByTransactionID(t, entityID, update1TxID)
	if err != nil {
		t.Fatalf("GetEntityByTransactionID(update1): %v", err)
	}
	if byUpdate1.Data["name"] != "v2" {
		t.Errorf("GetEntityByTransactionID(update1).Data.name = %v, want %q", byUpdate1.Data["name"], "v2")
	}

	byUpdate2, err := c.GetEntityByTransactionID(t, entityID, update2TxID)
	if err != nil {
		t.Fatalf("GetEntityByTransactionID(update2): %v", err)
	}
	if byUpdate2.Data["name"] != "v3" {
		t.Errorf("GetEntityByTransactionID(update2).Data.name = %v, want %q", byUpdate2.Data["name"], "v3")
	}

	// The delete's own transaction ID never matches — the tombstone has no
	// entity payload for GetVersionByTransaction to surface.
	status, _, err := c.GetEntityByTransactionIDRaw(t, entityID, deleteTxID)
	if err != nil {
		t.Fatalf("GetEntityByTransactionIDRaw(delete): %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("GetEntityByTransactionIDRaw(delete) status = %d, want %d (tombstone never matches)", status, http.StatusNotFound)
	}
}
