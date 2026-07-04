package e2e_test

// unique_keys_write_variants_test.go — E5 full-coverage completion.
//
// Task 11 documented the composite-unique-key codes (409 UNIQUE_VIOLATION,
// 422 INVALID_UNIQUE_KEY) across the full entity write family in
// api/openapi.yaml. The core scenarios in unique_keys_test.go already prove
// the create / createCollection / updateSingleWithLoopback / patchSingleWithLoopback
// happy-and-error paths, but several documented (op, code) pairs still lacked a
// producing running-backend test. The full-coverage bar
// (.claude/rules/test-coverage.md) requires a producing test for every
// documented status/error code, else the code is fictional.
//
// This file supplies the missing producing tests, one per gap:
//
//   createCollection          422 INVALID_UNIQUE_KEY  — partial key in a batch item
//   updateSingleWithLoopback  422 INVALID_UNIQUE_KEY  — PUT /entity/JSON/{id} nulls a key
//   updateSingle              422 INVALID_UNIQUE_KEY  — PUT /entity/JSON/{id}/{transition} nulls a key
//   patchSingleWithLoopback   409 UNIQUE_VIOLATION    — PATCH /entity/JSON/{id} to a duplicate key
//   updateCollection          409 UNIQUE_VIOLATION    — PUT /entity/JSON updates a key to a duplicate
//   updateCollection          422 INVALID_UNIQUE_KEY  — PUT /entity/JSON with a partial key
//   patchSingle               409 UNIQUE_VIOLATION    — PATCH /entity/JSON/{id}/{transition} to a duplicate
//   patchSingle               422 INVALID_UNIQUE_KEY  — PATCH /entity/JSON/{id}/{transition} nulls a key
//
// The matrix-tracked ops (createCollection, updateSingleWithLoopback,
// updateSingle, patchSingleWithLoopback) gain their new cells in
// zzz_errorcode_matrix_test.go; updateCollection and patchSingle are covered by
// the explicit assertions here (their full transition-error surface is left to
// a follow-on — see the matrix comment).

import (
	"fmt"
	"net/http"
	"testing"
)

// setupUKModelWithWorkflow imports a unique-key model from ukSampleData, declares
// the given keys while the model is UNLOCKED, locks it, and imports the supplied
// workflow. Unlike setupUKModel (which hard-codes a keyless auto-init workflow)
// it takes a caller-supplied workflow so tests can add a manual transition for
// the with-transition write endpoints (updateSingle, patchSingle).
func setupUKModelWithWorkflow(t *testing.T, entityName, keysJSON, workflowJSON string) {
	t.Helper()
	importModelWithSample(t, entityName, 1, ukSampleData)
	status, body := setUniqueKeysE2E(t, entityName, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("setUniqueKeys on unlocked model %s: expected 200, got %d: %s", entityName, status, body)
	}
	lockModelE2E(t, entityName, 1)
	status, body = importWorkflowE2E(t, entityName, 1, workflowJSON)
	if status != http.StatusOK {
		t.Fatalf("importWorkflow %s: expected 200, got %d: %s", entityName, status, body)
	}
}

// ukManualTransitionWF returns a workflow whose entities auto-init NONE→CREATED
// on create and then expose a single manual "modify" transition CREATED→UPDATED,
// used to reach the with-transition write endpoints (updateSingle = PUT
// /entity/JSON/{id}/{transition}; patchSingle = PATCH /entity/JSON/{id}/{transition}).
func ukManualTransitionWF(entityName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init",   "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "modify", "next": "UPDATED", "manual": true}]},
				"UPDATED": {}
			}
		}]
	}`, entityName)
}

// updateEntityRawTr issues PUT /entity/JSON/{entityId}/{transition} (updateSingle)
// and returns (statusCode, body) without asserting.
func updateEntityRawTr(t *testing.T, entityID, transition, payload string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%s", entityID, transition)
	resp := doAuth(t, http.MethodPut, path, payload)
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// --- createCollection 422 INVALID_UNIQUE_KEY ---

// TestUniqueKeys_CollectionPartialKeyCreate verifies that a collection create
// (POST /entity/JSON) whose item omits part of a composite key returns
// 422 INVALID_UNIQUE_KEY — the createCollection twin of TestUniqueKeys_PartialKeyCreate.
func TestUniqueKeys_CollectionPartialKeyCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-coll-partial"

	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, model, keysJSON)

	// One batch item is missing "amount" → partial composite key.
	item := collectionItem(model, 1, `{"name":"Batch","status":"draft"}`)
	status, body := collectionCreate(t, []string{item})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("collection partial key: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// --- updateSingleWithLoopback 422 INVALID_UNIQUE_KEY ---

// TestUniqueKeys_LoopbackUpdatePartialKey verifies that a loopback update
// (PUT /entity/JSON/{id}) whose full-replacement payload drops a composite-key
// field returns 422 INVALID_UNIQUE_KEY.
func TestUniqueKeys_LoopbackUpdatePartialKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-loop-partial"

	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, model, keysJSON)

	status, body := createEntityRaw(t, model, 1, `{"name":"Paula","amount":10,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", status, body)
	}
	entityID := extractEntityID(t, body)

	// Full-replacement PUT omits "name" → composite key becomes partial → 422.
	status, body = updateEntityRaw(t, entityID, `{"amount":10,"status":"updated"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("loopback update partial key: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// --- updateSingle 422 INVALID_UNIQUE_KEY ---

// TestUniqueKeys_TransitionUpdatePartialKey verifies that a with-transition
// update (PUT /entity/JSON/{id}/{transition}) whose full-replacement payload
// drops a composite-key field returns 422 INVALID_UNIQUE_KEY.
func TestUniqueKeys_TransitionUpdatePartialKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-tr-update-partial"

	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModelWithWorkflow(t, model, keysJSON, ukManualTransitionWF(model))

	entityID := createEntityE2E(t, model, 1, `{"name":"Quinn","amount":10,"status":"draft"}`)

	// PUT via the manual "modify" transition, dropping "name" → partial key → 422.
	status, body := updateEntityRawTr(t, entityID, "modify", `{"amount":10,"status":"updated"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("transition update partial key: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// --- patchSingleWithLoopback 409 UNIQUE_VIOLATION ---

// TestUniqueKeys_LoopbackPatchDuplicate verifies that a merge-patch loopback
// (PATCH /entity/JSON/{id}) that moves a key field onto a value already held by
// another entity returns 409 UNIQUE_VIOLATION — the patch twin of
// TestUniqueKeys_UpdateMovesKey's collision leg.
func TestUniqueKeys_LoopbackPatchDuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-loop-patch-dup"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	// entity1 holds name="Rita".
	status, body := createEntityRaw(t, model, 1, `{"name":"Rita","amount":1,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity1: expected 200, got %d: %s", status, body)
	}

	// entity2 holds name="Sam".
	status, body = createEntityRaw(t, model, 1, `{"name":"Sam","amount":2,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity2: expected 200, got %d: %s", status, body)
	}
	entity2ID := extractEntityID(t, body)
	entity2Tx := extractTxID(t, body)

	// PATCH entity2's name → "Rita" collides with entity1 → 409 UNIQUE_VIOLATION.
	status, body = patchEntityMerge(t, entity2ID, entity2Tx, `{"name":"Rita"}`)
	if status != http.StatusConflict {
		t.Fatalf("loopback patch duplicate: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
}

// --- updateCollection 409 UNIQUE_VIOLATION ---

// TestUniqueKeys_CollectionUpdateDuplicate verifies that a collection update
// (PUT /entity/JSON) moving an item's key onto a duplicate value returns
// 409 UNIQUE_VIOLATION. A UNIQUE_VIOLATION is not the per-item-isolated
// ENTITY_MODIFIED rollback case: it rolls the chunk back and, on the first
// chunk, surfaces as an HTTP 409.
func TestUniqueKeys_CollectionUpdateDuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-coll-update-dup"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	_ = createEntityE2E(t, model, 1, `{"name":"Tara","amount":1,"status":"draft"}`)
	id2 := createEntityE2E(t, model, 1, `{"name":"Uma","amount":2,"status":"draft"}`)

	// Move id2's name → "Tara" (already held by id1) → 409 UNIQUE_VIOLATION.
	item := updateCollectionItem(id2, `{"name":"Tara","amount":2,"status":"upd"}`, "")
	status, body := putUpdateCollection(t, []string{item}, "")
	if status != http.StatusConflict {
		t.Fatalf("collection update duplicate: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
}

// --- updateCollection 422 INVALID_UNIQUE_KEY ---

// TestUniqueKeys_CollectionUpdatePartialKey verifies that a collection update
// (PUT /entity/JSON) whose full-replacement payload drops a composite-key field
// returns 422 INVALID_UNIQUE_KEY (chunk-0 partial key → HTTP 422).
func TestUniqueKeys_CollectionUpdatePartialKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-coll-update-partial"

	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, model, keysJSON)

	id := createEntityE2E(t, model, 1, `{"name":"Vera","amount":10,"status":"draft"}`)

	// Full-replacement update omits "amount" → partial composite key → 422.
	item := updateCollectionItem(id, `{"name":"Vera","status":"upd"}`, "")
	status, body := putUpdateCollection(t, []string{item}, "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("collection update partial key: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// --- patchSingle 409 UNIQUE_VIOLATION ---

// TestUniqueKeys_TransitionPatchDuplicate verifies that a with-transition
// merge-patch (PATCH /entity/JSON/{id}/{transition}) that moves a key field onto
// a value already held by another entity returns 409 UNIQUE_VIOLATION.
func TestUniqueKeys_TransitionPatchDuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-tr-patch-dup"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModelWithWorkflow(t, model, keysJSON, ukManualTransitionWF(model))

	_ = createEntityE2E(t, model, 1, `{"name":"Wade","amount":1,"status":"draft"}`)
	id2 := createEntityE2E(t, model, 1, `{"name":"Xena","amount":2,"status":"draft"}`)
	id2Tx := getEntityTxID(t, id2)

	// PATCH id2's name → "Wade" via the manual "modify" transition → 409.
	resp := patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s/modify", id2),
		"application/merge-patch+json", id2Tx, `{"name":"Wade"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("transition patch duplicate: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
}

// --- patchSingle 422 INVALID_UNIQUE_KEY ---

// TestUniqueKeys_TransitionPatchPartialKey verifies that a with-transition
// merge-patch (PATCH /entity/JSON/{id}/{transition}) that nulls a composite-key
// field returns 422 INVALID_UNIQUE_KEY.
func TestUniqueKeys_TransitionPatchPartialKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-uk-tr-patch-partial"

	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModelWithWorkflow(t, model, keysJSON, ukManualTransitionWF(model))

	id := createEntityE2E(t, model, 1, `{"name":"Yuri","amount":10,"status":"draft"}`)
	idTx := getEntityTxID(t, id)

	// PATCH sets name=null via the manual "modify" transition → partial key → 422.
	resp := patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s/modify", id),
		"application/merge-patch+json", idTx, `{"name":null}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("transition patch partial key: expected 422, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}
