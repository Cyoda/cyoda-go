package e2e_test

// unique_keys_test.go — HTTP e2e coverage for composite unique-key feature.
//
// All scenarios exercise the real postgres backend via the in-process
// httptest.Server wired in TestMain. Coverage matrix (task 8.1):
//
//   Declare unique keys (unlocked) → 200
//   Declare unique keys (locked)   → 409 MODEL_ALREADY_LOCKED
//   Bad field path                 → 422 INVALID_UNIQUE_KEY_DEFINITION
//   Duplicate create               → 409 UNIQUE_VIOLATION
//   Partial key create             → 422 INVALID_UNIQUE_KEY
//   Over-bound numeric             → 422 INVALID_UNIQUE_KEY
//   Update moves key (frees old)   → 200; collides with existing → 409 UNIQUE_VIOLATION
//   PATCH nulls key field          → 422 INVALID_UNIQUE_KEY
//   Soft-delete frees value        → re-create succeeds → 200
//   DeleteAll frees values         → re-create succeeds → 200
//   Collection intra-batch dup     → 409 UNIQUE_VIOLATION (no partial commit)
//   Mixed-model batch              → per-item key enforcement
//   Multiple independent keys      → each enforced independently
//   Schema-extend after lock       → unique keys preserved; duplicate still 409
//   Model export includes keys     → uniqueKeys field present

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// --- Helpers local to unique_keys_test.go ---

// ukSampleData is the sample entity payload used to infer the model schema.
// Uses name+status (string) and amount (number) so we can declare unique keys on them.
const ukSampleData = `{"name":"Test","amount":100,"status":"draft"}`

// importModelWithSample imports an entity model using the given sample JSON.
func importModelWithSample(t *testing.T, entityName string, modelVersion int, sample string) {
	t.Helper()
	path := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, sample)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("importModelWithSample %s/%d: expected 200, got %d: %s", entityName, modelVersion, resp.StatusCode, body)
	}
}

// setUniqueKeysE2E calls PUT /model/{entityName}/{modelVersion}/unique-keys and
// returns the HTTP status code and response body without asserting success.
// Callers check the status themselves for both happy-path and error scenarios.
func setUniqueKeysE2E(t *testing.T, entityName string, modelVersion int, keysJSON string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/unique-keys", entityName, modelVersion)
	resp := doAuth(t, http.MethodPut, path, keysJSON)
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// assertErrorCode checks that the response body's properties.errorCode matches
// the expected code. The body must be parseable JSON.
func assertErrorCode(t *testing.T, body, expectedCode string) {
	t.Helper()
	var problem struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("assertErrorCode: failed to parse body as JSON: %v\nbody: %s", err, body)
	}
	got, _ := problem.Properties["errorCode"].(string)
	if got != expectedCode {
		t.Errorf("assertErrorCode: expected %q, got %q\nbody: %s", expectedCode, got, body)
	}
}

// setupUKModel sets up a model for unique key testing:
//  1. Import model from ukSampleData (UNLOCKED).
//  2. Declare unique keys (while UNLOCKED).
//  3. Lock the model.
//  4. Import a simple workflow so entities can be created.
func setupUKModel(t *testing.T, entityName string, keysJSON string) {
	t.Helper()
	importModelWithSample(t, entityName, 1, ukSampleData)

	status, body := setUniqueKeysE2E(t, entityName, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("setUniqueKeys on unlocked model %s: expected 200, got %d: %s", entityName, status, body)
	}

	lockModelE2E(t, entityName, 1)

	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`, entityName)
	status, body = importWorkflowE2E(t, entityName, 1, wf)
	if status != http.StatusOK {
		t.Fatalf("importWorkflow %s: expected 200, got %d: %s", entityName, status, body)
	}
}

// createEntityRaw issues POST /entity/JSON/{name}/{version} and returns
// (statusCode, body) without asserting. Callers check status themselves.
func createEntityRaw(t *testing.T, entityName string, modelVersion int, payload string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, payload)
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// updateEntityRaw issues PUT /entity/JSON/{entityId} and returns
// (statusCode, body) without asserting.
func updateEntityRaw(t *testing.T, entityID, payload string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, payload)
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// patchEntityMerge issues PATCH /entity/JSON/{entityId} with merge-patch+json.
func patchEntityMerge(t *testing.T, entityID, ifMatch, patch string) (int, string) {
	t.Helper()
	token := getToken(t, "testclient", "testsecret")
	req, err := e2eNewRequest(t, http.MethodPatch,
		serverURL+fmt.Sprintf("/api/entity/JSON/%s", entityID),
		strings.NewReader(patch))
	if err != nil {
		t.Fatalf("patchEntityMerge new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patchEntityMerge do: %v", err)
	}
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// deleteEntityRaw issues DELETE /entity/{entityId}.
func deleteEntityRaw(t *testing.T, entityID string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s", entityID)
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// deleteAllEntitiesRaw issues DELETE /entity/{entityName}/{modelVersion}.
func deleteAllEntitiesRaw(t *testing.T, entityName string, modelVersion int) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	return resp.StatusCode, body
}

// extractEntityID extracts the first entityId from a successful POST /entity response body.
func extractEntityID(t *testing.T, body string) string {
	t.Helper()
	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		t.Fatalf("extractEntityID: failed to parse response: %v\nbody: %s", err, body)
	}
	if len(results) == 0 {
		t.Fatalf("extractEntityID: empty results array")
	}
	ids, _ := results[0]["entityIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("extractEntityID: no entityIds in results: %v", results[0])
	}
	id, _ := ids[0].(string)
	if id == "" {
		t.Fatalf("extractEntityID: empty entity ID")
	}
	return id
}

// extractTxID extracts the transactionId from a successful POST /entity response.
func extractTxID(t *testing.T, body string) string {
	t.Helper()
	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		t.Fatalf("extractTxID: failed to parse response: %v\nbody: %s", err, body)
	}
	if len(results) == 0 {
		t.Fatalf("extractTxID: empty results array")
	}
	tx, _ := results[0]["transactionId"].(string)
	if tx == "" {
		t.Fatalf("extractTxID: empty transactionId in results: %v", results[0])
	}
	return tx
}

// ---  Core unique key scenarios ---

// TestUniqueKeys_DeclareOnUnlocked verifies that unique keys can be declared on
// an UNLOCKED model (→ 200) and that the same call on a LOCKED model returns
// 409 MODEL_ALREADY_LOCKED.
func TestUniqueKeys_DeclareOnUnlocked(t *testing.T) {
	const model = "e2e-uk-declare"

	importModelWithSample(t, model, 1, ukSampleData)

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`

	// Declare on UNLOCKED model → 200.
	status, body := setUniqueKeysE2E(t, model, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("declare on unlocked: expected 200, got %d: %s", status, body)
	}

	// Lock the model.
	lockModelE2E(t, model, 1)

	// Declare on LOCKED model → 409 MODEL_ALREADY_LOCKED.
	status, body = setUniqueKeysE2E(t, model, 1, keysJSON)
	if status != http.StatusConflict {
		t.Fatalf("declare on locked: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}

// TestUniqueKeys_BadFieldPath verifies that declaring a unique key referencing
// a field that does not exist in the model schema returns
// 422 INVALID_UNIQUE_KEY_DEFINITION.
func TestUniqueKeys_BadFieldPath(t *testing.T) {
	const model = "e2e-uk-badfield"

	importModelWithSample(t, model, 1, ukSampleData)

	// "$.nonexistent" is not in the model schema.
	keysJSON := `{"uniqueKeys":[{"id":"bad-key","fields":["$.nonexistent"]}]}`
	status, body := setUniqueKeysE2E(t, model, 1, keysJSON)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad field path: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY_DEFINITION")
}

// TestUniqueKeys_CreateDuplicate verifies that creating a second entity whose
// composite unique key values match an existing entity returns
// 409 UNIQUE_VIOLATION.
func TestUniqueKeys_CreateDuplicate(t *testing.T) {
	const model = "e2e-uk-dup"

	// Composite key over (name, amount).
	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, model, keysJSON)

	// First entity → success.
	status, body := createEntityRaw(t, model, 1, `{"name":"Alice","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("first create: expected 200, got %d: %s", status, body)
	}

	// Second entity with same (name, amount) → 409 UNIQUE_VIOLATION.
	status, body = createEntityRaw(t, model, 1, `{"name":"Alice","amount":100,"status":"draft"}`)
	if status != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
}

// TestUniqueKeys_PartialKeyCreate verifies that providing only some fields of a
// composite key (partial match) returns 422 INVALID_UNIQUE_KEY.
func TestUniqueKeys_PartialKeyCreate(t *testing.T) {
	const model = "e2e-uk-partial"

	// Composite key over (name, amount).
	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, model, keysJSON)

	// Only "name" present, "amount" is missing → partial key.
	status, body := createEntityRaw(t, model, 1, `{"name":"Bob","status":"draft"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("partial key create: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// TestUniqueKeys_OverBoundNumeric verifies that a key field value with a huge
// numeric exponent (beyond ComputeClaims' maxNumExp=6144) returns
// 422 INVALID_UNIQUE_KEY.
//
// Setup note: the model must have an UNBOUND_INTEGER schema for the `amount`
// field so schema validation does not reject the large value before the unique-
// key check runs. We achieve this by seeding the model with an astronomically
// large integer literal (>2^128 → UNBOUND_INTEGER). Then the test entity uses
// 1e6145 (exponent 6145 > maxNumExp=6144): schema validation accepts it
// (UNBOUND_INTEGER ← UNBOUND_INTEGER) but ComputeClaims rejects it
// (over-bound exponent → ErrPartialUniqueKey → 422 INVALID_UNIQUE_KEY).
//
// NOTE: using 1e1000000000 directly would cause the schema validator to hang
// because inferDataType computes 10^1000000000 (a big.Int with ~415 MB) before
// the unique-key check runs. That is a pre-existing server-side bug outside the
// scope of this task. 1e6145 (10^6145 ≈ 2.5 KB big.Int) is fast and safe, and
// still exercises the ComputeClaims over-bound rejection path end-to-end.
func TestUniqueKeys_OverBoundNumeric(t *testing.T) {
	const model = "e2e-uk-overbound"

	// Seed model with an UNBOUND_INTEGER amount so schema validation passes a
	// large-exponent value. 50 decimal digits > 2^128 → UNBOUND_INTEGER.
	const unboundSample = `{"name":"Test","amount":12345678901234567890123456789012345678901234567890,"status":"draft"}`
	importModelWithSample(t, model, 1, unboundSample)

	// Key over the UNBOUND_INTEGER field.
	keysJSON := `{"uniqueKeys":[{"id":"amount-key","fields":["$.amount"]}]}`
	status, body := setUniqueKeysE2E(t, model, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("setUniqueKeys for overbound model: expected 200, got %d: %s", status, body)
	}

	lockModelE2E(t, model, 1)
	wfStatus, wfBody := importWorkflowE2E(t, model, 1, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`, model))
	if wfStatus != http.StatusOK {
		t.Fatalf("importWorkflow overbound model: expected 200, got %d: %s", wfStatus, wfBody)
	}

	// 1e6145 has exponent 6145 > maxNumExp(6144): ComputeClaims rejects it as
	// ErrPartialUniqueKey → service returns 422 INVALID_UNIQUE_KEY.
	// Schema validation accepts it: UNBOUND_INTEGER is assignable to UNBOUND_INTEGER.
	status, body = createEntityRaw(t, model, 1, `{"name":"Test","amount":1e6145,"status":"draft"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("over-bound numeric: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// TestUniqueKeys_UpdateMovesKey verifies that updating an entity to a new
// key value frees the old value and a second entity can then claim it.
// Also verifies that updating to a value already held by another entity
// returns 409 UNIQUE_VIOLATION.
func TestUniqueKeys_UpdateMovesKey(t *testing.T) {
	const model = "e2e-uk-update"

	// Unique key over "name".
	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	// Create entity1 with name="Alice".
	status, body := createEntityRaw(t, model, 1, `{"name":"Alice","amount":10,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity1: expected 200, got %d: %s", status, body)
	}
	entity1ID := extractEntityID(t, body)

	// Create entity2 with name="Bob".
	status, body = createEntityRaw(t, model, 1, `{"name":"Bob","amount":20,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity2: expected 200, got %d: %s", status, body)
	}
	// Update entity1 to name="Bob" → collides with entity2 → 409 UNIQUE_VIOLATION.
	status, body = updateEntityRaw(t, entity1ID, `{"name":"Bob","amount":10,"status":"updated"}`)
	if status != http.StatusConflict {
		t.Fatalf("update collide: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")

	// Update entity1 to name="Charlie" → frees "Alice".
	status, body = updateEntityRaw(t, entity1ID, `{"name":"Charlie","amount":10,"status":"updated"}`)
	if status != http.StatusOK {
		t.Fatalf("update move: expected 200, got %d: %s", status, body)
	}

	// Create entity3 with name="Alice" → should now succeed (freed by entity1's move).
	status, body = createEntityRaw(t, model, 1, `{"name":"Alice","amount":30,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("re-claim freed key: expected 200, got %d: %s", status, body)
	}
}

// TestUniqueKeys_PatchNullsKeyField verifies that a PATCH that sets a composite
// key field to null (making the key partial) returns 422 INVALID_UNIQUE_KEY.
func TestUniqueKeys_PatchNullsKeyField(t *testing.T) {
	const model = "e2e-uk-patch-null"

	// Composite key over (name, amount).
	keysJSON := `{"uniqueKeys":[{"id":"name-amount-key","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, model, keysJSON)

	// Create a valid entity.
	status, body := createEntityRaw(t, model, 1, `{"name":"Dave","amount":50,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", status, body)
	}
	entityID := extractEntityID(t, body)
	txID := extractTxID(t, body)

	// PATCH that sets name=null → key becomes partial → 422 INVALID_UNIQUE_KEY.
	status, body = patchEntityMerge(t, entityID, txID, `{"name":null}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("patch nulls key field: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// TestUniqueKeys_SoftDeleteFreesValue verifies that soft-deleting an entity
// releases its unique key value so another entity can claim it.
func TestUniqueKeys_SoftDeleteFreesValue(t *testing.T) {
	const model = "e2e-uk-softdel"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	// Create entity with name="Eve".
	status, body := createEntityRaw(t, model, 1, `{"name":"Eve","amount":10,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", status, body)
	}
	entityID := extractEntityID(t, body)

	// Soft-delete the entity → 200.
	status, body = deleteEntityRaw(t, entityID)
	if status != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", status, body)
	}

	// Re-create with same name → should succeed (key freed by delete).
	status, body = createEntityRaw(t, model, 1, `{"name":"Eve","amount":20,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("re-create after delete: expected 200, got %d: %s", status, body)
	}
}

// TestUniqueKeys_DeleteAllFreesValues verifies that DeleteAll releases all
// unique key values for the model so they can be reclaimed.
func TestUniqueKeys_DeleteAllFreesValues(t *testing.T) {
	const model = "e2e-uk-deleteall"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	// Create two entities.
	for _, name := range []string{"Frank", "Grace"} {
		status, body := createEntityRaw(t, model, 1,
			fmt.Sprintf(`{"name":%q,"amount":1,"status":"draft"}`, name))
		if status != http.StatusOK {
			t.Fatalf("create %s: expected 200, got %d: %s", name, status, body)
		}
	}

	// DeleteAll → 200.
	status, body := deleteAllEntitiesRaw(t, model, 1)
	if status != http.StatusOK {
		t.Fatalf("deleteAll: expected 200, got %d: %s", status, body)
	}

	// Re-create with the same names → should succeed (keys freed).
	for _, name := range []string{"Frank", "Grace"} {
		status, body := createEntityRaw(t, model, 1,
			fmt.Sprintf(`{"name":%q,"amount":1,"status":"draft"}`, name))
		if status != http.StatusOK {
			t.Fatalf("re-create %s after deleteAll: expected 200, got %d: %s", name, status, body)
		}
	}
}

// TestUniqueKeys_CollectionIntraBatchDuplicate verifies that a collection
// create with two items sharing the same composite key value returns
// 409 UNIQUE_VIOLATION with no partial commit (both items must be rolled back).
func TestUniqueKeys_CollectionIntraBatchDuplicate(t *testing.T) {
	const model = "e2e-uk-batchdup"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	// Two items in the same batch share name="Henry".
	item := func(name string) string {
		return collectionItem(model, 1,
			fmt.Sprintf(`{"name":%q,"amount":1,"status":"draft"}`, name))
	}
	status, body := collectionCreate(t, []string{item("Henry"), item("Henry")})
	if status != http.StatusConflict {
		t.Fatalf("intra-batch dup: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")

	// Verify no partial commit — neither entity should exist.
	// Count entities via GET /entity/{name}/1 (page-0, size-1).
	listPath := fmt.Sprintf("/api/entity/%s/1?pageSize=100&pageNumber=0", model)
	listResp := doAuth(t, http.MethodGet, listPath, "")
	listBody := readBody(t, listResp)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list after rollback: expected 200, got %d: %s", listResp.StatusCode, listBody)
	}
	var entities []map[string]any
	if err := json.Unmarshal([]byte(listBody), &entities); err != nil {
		t.Fatalf("list after rollback: parse: %v; body: %s", err, listBody)
	}
	if len(entities) != 0 {
		t.Errorf("expected 0 entities after rollback (no partial commit), got %d", len(entities))
	}
}

// TestUniqueKeys_MixedModelBatch verifies that a collection create containing
// items for two different models enforces unique keys per-model independently.
func TestUniqueKeys_MixedModelBatch(t *testing.T) {
	const modelA = "e2e-uk-mixed-a"
	const modelB = "e2e-uk-mixed-b"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, modelA, keysJSON)
	setupUKModel(t, modelB, keysJSON)

	// Pre-seed modelA with name="Ivan" so a second item with the same name
	// in the same batch would cause a violation.
	status, body := createEntityRaw(t, modelA, 1, `{"name":"Ivan","amount":1,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("seed modelA: expected 200, got %d: %s", status, body)
	}

	// Batch: modelB item (unique "Ivan" in modelB) + modelA item (dup "Ivan" in modelA).
	itemA := collectionItem(modelA, 1, `{"name":"Ivan","amount":2,"status":"draft"}`)  // dup in modelA
	itemB := collectionItem(modelB, 1, `{"name":"Ivan","amount":1,"status":"draft"}`)  // unique in modelB

	// The modelA item duplicates, so the batch must fail.
	status, body = collectionCreate(t, []string{itemB, itemA})
	if status != http.StatusConflict {
		t.Fatalf("mixed batch with dup: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")

	// A batch with two items that don't collide should succeed.
	itemB2 := collectionItem(modelB, 1, `{"name":"Judy","amount":2,"status":"draft"}`)
	itemA2 := collectionItem(modelA, 1, `{"name":"Karl","amount":3,"status":"draft"}`)
	status, body = collectionCreate(t, []string{itemB2, itemA2})
	if status != http.StatusOK {
		t.Fatalf("mixed batch no-dup: expected 200, got %d: %s", status, body)
	}
}

// TestUniqueKeys_MultipleKeys verifies that multiple independent unique keys on
// one model are each independently enforced.
func TestUniqueKeys_MultipleKeys(t *testing.T) {
	const model = "e2e-uk-multikey"

	// Two separate keys: one over "name", one over "amount".
	keysJSON := `{"uniqueKeys":[
		{"id":"name-key","fields":["$.name"]},
		{"id":"amount-key","fields":["$.amount"]}
	]}`
	setupUKModel(t, model, keysJSON)

	// First entity → success.
	status, body := createEntityRaw(t, model, 1, `{"name":"Lena","amount":111,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("first: expected 200, got %d: %s", status, body)
	}

	// Different name, SAME amount → violates amount-key → 409.
	status, body = createEntityRaw(t, model, 1, `{"name":"Mike","amount":111,"status":"draft"}`)
	if status != http.StatusConflict {
		t.Fatalf("dup amount: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")

	// SAME name, different amount → violates name-key → 409.
	status, body = createEntityRaw(t, model, 1, `{"name":"Lena","amount":222,"status":"draft"}`)
	if status != http.StatusConflict {
		t.Fatalf("dup name: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")

	// Different name AND different amount → success.
	status, body = createEntityRaw(t, model, 1, `{"name":"Nina","amount":333,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("distinct: expected 200, got %d: %s", status, body)
	}
}

// TestUniqueKeys_SchemaExtendAfterLockPreservesKeys verifies that an additive
// entity write (schema extension via ChangeLevel=STRUCTURAL) does NOT drop the
// declared unique keys: a duplicate is still rejected with 409 UNIQUE_VIOLATION.
func TestUniqueKeys_SchemaExtendAfterLockPreservesKeys(t *testing.T) {
	const model = "e2e-uk-schemaext"

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, model, keysJSON)

	// Set STRUCTURAL ChangeLevel so schema extensions are accepted.
	setChangeLevelE2E(t, model, 1, "STRUCTURAL")

	// Create first entity (new field "extra" → schema extends).
	status, body := createEntityRaw(t, model, 1, `{"name":"Oscar","amount":1,"status":"draft","extra":"x"}`)
	if status != http.StatusOK {
		t.Fatalf("structural extend: expected 200, got %d: %s", status, body)
	}

	// Create second entity with same name → must still trigger 409 UNIQUE_VIOLATION.
	status, body = createEntityRaw(t, model, 1, `{"name":"Oscar","amount":2,"status":"draft","extra":"y"}`)
	if status != http.StatusConflict {
		t.Fatalf("dup after schema extend: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
}

// TestUniqueKeys_ExportIncludesKeys verifies that the model export response
// includes the uniqueKeys field containing the declared keys.
func TestUniqueKeys_ExportIncludesKeys(t *testing.T) {
	const model = "e2e-uk-export"

	importModelWithSample(t, model, 1, ukSampleData)

	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]},{"id":"amount-key","fields":["$.amount"]}]}`
	status, body := setUniqueKeysE2E(t, model, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("setUniqueKeys: expected 200, got %d: %s", status, body)
	}

	lockModelE2E(t, model, 1)

	// Export using SIMPLE_VIEW.
	exported := exportModelE2E(t, model, 1)

	rawUK, ok := exported["uniqueKeys"]
	if !ok {
		t.Fatalf("export missing 'uniqueKeys' field; got keys: %v", mapKeys(exported))
	}

	uks, ok := rawUK.([]any)
	if !ok {
		t.Fatalf("uniqueKeys is not an array; got %T: %v", rawUK, rawUK)
	}
	if len(uks) != 2 {
		t.Errorf("expected 2 unique keys in export, got %d: %v", len(uks), uks)
	}

	// Verify the IDs are present.
	ids := make(map[string]bool)
	for _, raw := range uks {
		if m, ok := raw.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids[id] = true
			}
		}
	}
	for _, wantID := range []string{"name-key", "amount-key"} {
		if !ids[wantID] {
			t.Errorf("export missing uniqueKey id=%q; found ids: %v", wantID, ids)
		}
	}
}

// TestUniqueKeys_ExportOmitsKeysWhenNoneDeclared verifies that a model with no
// composite unique keys exports WITHOUT a uniqueKeys field (omitempty
// semantics) — the field appears only when keys are declared, keeping the
// export of a keyless model byte-identical to a pre-feature model.
func TestUniqueKeys_ExportOmitsKeysWhenNoneDeclared(t *testing.T) {
	const model = "e2e-uk-export-empty"

	importModelWithSample(t, model, 1, ukSampleData)
	// Deliberately do NOT declare any unique keys.
	lockModelE2E(t, model, 1)

	exported := exportModelE2E(t, model, 1)

	if _, ok := exported["uniqueKeys"]; ok {
		t.Errorf("keyless model export must omit 'uniqueKeys'; got keys: %v", mapKeys(exported))
	}
}

// mapKeys returns the keys of a map[string]any for error messages.
func mapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
