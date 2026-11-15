package parity

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

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

// ovptSample is the model of RunHistoryOneVersionPerTransaction's entity.
const ovptSample = `{"name":"e","amount":1,"status":"new"}`

// ovptWorkflow rests a created entity in OPEN and moves it to DONE through
// the manual transition "go", running procs on the way. A write of the
// entity that names no transition, such as a processor's joined write of
// it, leaves it in OPEN and runs no processor.
func ovptWorkflow(name string, procs []any) string {
	return cbWorkflowDoc(name, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "OPEN", "manual": false}}},
		"OPEN": map[string]any{"transitions": []any{map[string]any{
			"name": "go", "next": "DONE", "manual": true, "processors": procs,
		}}},
		"DONE": map[string]any{},
	})
}

// RunHistoryOneVersionPerTransaction proves, on every backend, that an
// entity has at most one version per transaction and that the by-transaction
// read is committed-only:
//
//   - SYNC: the manual transition's processor writes the entity through a
//     joined callback, and the engine then saves it in DONE — two writes in
//     one transaction X. /changes lists one entry for X, and
//     GET /entity/{id}?transactionId=X returns X's final entity (DONE, with
//     the callback's write), not the callback's intermediate write (OPEN).
//   - SYNC_ThenCommitBeforeDispatch: the same, followed by a
//     COMMIT_BEFORE_DISPATCH processor, so the request runs in two
//     transactions, each writing the entity twice. /changes lists one entry
//     per transaction, and each transaction's by-transaction read is found.
//   - InsideWritingTx: the processor's callback reads the entity with
//     ?transactionId= naming the transaction it joined. X has not committed,
//     so the read is answered 404 ENTITY_NOT_FOUND although it is made inside
//     X; after the commit the same read returns X's final entity.
func RunHistoryOneVersionPerTransaction(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const goBody = `{"name":"e","amount":1,"status":"go"}`

	// setup creates the entity in OPEN under a model whose "go" runs procs,
	// and returns its id.
	setup := func(t *testing.T, procs []any) uuid.UUID {
		t.Helper()
		model := "ovpt-" + uuid.NewString()[:8]
		setupModelWithWorkflow(t, c, model, 1, ovptSample, ovptWorkflow(model+"-wf", procs))
		eID, err := c.CreateEntity(t, model, 1, ovptSample)
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		return eID
	}
	// changes reads /changes and fails unless every entry names a
	// transaction no other entry names.
	changes := func(t *testing.T, eID uuid.UUID) []client.EntityChangeMeta {
		t.Helper()
		ch, err := c.GetEntityChanges(t, eID)
		if err != nil {
			t.Fatalf("GetEntityChanges: %v", err)
		}
		seen := map[string]bool{}
		for i, e := range ch {
			if e.TransactionID == "" {
				t.Fatalf("changes[%d] (%s) has no transactionId: %+v", i, e.ChangeType, ch)
			}
			if seen[e.TransactionID] {
				t.Fatalf("transaction %s has more than one /changes entry; want one version per transaction: %+v", e.TransactionID, ch)
			}
			seen[e.TransactionID] = true
		}
		return ch
	}
	// byTx reads the entity as transaction txID left it.
	byTx := func(t *testing.T, eID uuid.UUID, txID string) client.EntityResult {
		t.Helper()
		e, err := c.GetEntityByTransactionID(t, eID, txID)
		if err != nil {
			t.Fatalf("GetEntityByTransactionID(%s): %v", txID, err)
		}
		return e
	}
	selfWrite := cbProc("cb-write-self", "SYNC", `{"marker":"self"}`, nil)

	t.Run("SYNC", func(t *testing.T) {
		eID := setup(t, []any{selfWrite})
		txID, err := c.UpdateEntityWithTxID(t, eID, "go", goBody)
		if err != nil {
			t.Fatalf("go: %v", err)
		}
		ch := changes(t, eID)
		if len(ch) != 2 || ch[0].TransactionID != txID || ch[0].ChangeType != "UPDATE" || ch[1].ChangeType != "CREATE" {
			t.Fatalf("changes = %+v; want [UPDATE by %s, CREATE]", ch, txID)
		}
		e := byTx(t, eID, txID)
		if e.Meta.State != "DONE" || e.Data["status"] != "self" {
			t.Fatalf("by-transaction read of %s is %s with status %v; want the transaction's final entity, DONE with the callback's write (status self)",
				txID, e.Meta.State, e.Data["status"])
		}
	})

	t.Run("SYNC_ThenCommitBeforeDispatch", func(t *testing.T) {
		eID := setup(t, []any{selfWrite, cbProc("noop", "COMMIT_BEFORE_DISPATCH", "", nil)})
		txID, err := c.UpdateEntityWithTxID(t, eID, "go", goBody)
		if err != nil {
			t.Fatalf("go: %v", err)
		}
		ch := changes(t, eID)
		// CREATE, the segment committed before the dispatch (the request's
		// transaction), and the segment after it.
		if len(ch) != 3 || ch[1].TransactionID != txID {
			t.Fatalf("changes = %+v; want 3 entries, the middle one by the request's transaction %s", ch, txID)
		}
		if e := byTx(t, eID, txID); e.Data["status"] != "self" {
			t.Fatalf("by-transaction read of %s has status %v; want the callback's write (status self)", txID, e.Data["status"])
		}
		if e := byTx(t, eID, ch[0].TransactionID); e.Meta.State != "DONE" || e.Data["status"] != "self" {
			t.Fatalf("by-transaction read of the last segment %s is %s with status %v; want DONE with status self",
				ch[0].TransactionID, e.Meta.State, e.Data["status"])
		}
	})

	t.Run("InsideWritingTx", func(t *testing.T) {
		rec := "ovpt-rec-" + uuid.NewString()[:8]
		marker := "ovpt-" + uuid.NewString()[:8]
		SetupHopRecordModel(t, c, rec)
		eID := setup(t, []any{cbProc("cb-write-self-read-by-tx", "SYNC", HopRecordContext("", marker, rec), nil)})
		txID, err := c.UpdateEntityWithTxID(t, eID, "go", goBody)
		if err != nil {
			t.Fatalf("go: %v", err)
		}
		hop := ReadHopRecord(t, c, rec, marker)
		if hop.TokenWasEmpty {
			t.Fatalf("the callout carried no transaction token; the read was not made inside the transaction")
		}
		var pd struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal([]byte(hop.Body), &pd)
		if code, _ := pd.Properties["errorCode"].(string); hop.Status != http.StatusNotFound || code != "ENTITY_NOT_FOUND" {
			t.Fatalf("by-transaction read inside the uncommitted transaction answered %d %s; want 404 ENTITY_NOT_FOUND: %s", hop.Status, code, hop.Body)
		}
		ch := changes(t, eID)
		if len(ch) != 2 || ch[0].TransactionID != txID {
			t.Fatalf("changes = %+v; want [UPDATE by %s, CREATE]", ch, txID)
		}
		if e := byTx(t, eID, txID); e.Meta.State != "DONE" || e.Data["status"] != marker {
			t.Fatalf("by-transaction read of %s after its commit is %s with status %v; want DONE with status %s", txID, e.Meta.State, e.Data["status"], marker)
		}
	})
}
