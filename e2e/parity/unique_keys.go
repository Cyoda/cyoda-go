package parity

// Composite unique-key parity scenarios (Task 8.3).
//
// These scenarios pin the OBSERVABLE behaviour of the composite unique-key
// feature across every storage backend (memory / sqlite / postgres / any
// out-of-tree plugin). All scenarios are black-box: they drive the public
// HTTP API and assert status codes + error codes in the response body.
//
// Capability gate (every scenario):
//
//	After ImportModel (while the model is still unlocked), SetUniqueKeysRaw
//	is called. If the backend returns 422 COMPOSITE_KEY_UNSUPPORTED the
//	scenario is skipped cleanly — the commercial backend can catch up on its
//	next dependency update. All three in-repo backends (memory, sqlite,
//	postgres) support the feature and will run the assertions.
//
// Scenario list (spec §8.3 matrix):
//  1. UniqueKeys_CreateDuplicate        — duplicate create → 409 UNIQUE_VIOLATION
//  2. UniqueKeys_SoftDeleteFreesValue   — delete, then re-create same value → 201
//  3. UniqueKeys_PartialKey             — some-but-not-all key fields present → 422 INVALID_UNIQUE_KEY
//  4. UniqueKeys_AllNullExempt          — all key fields absent → both creates succeed (201)
//  5. UniqueKeys_DeleteAllFreesValues   — DeleteAll, then re-create same values → 201
//  6. UniqueKeys_MultipleKeys           — two independent keys, each enforced separately
//
// What this suite does NOT cover:
//   - Same-transaction delete+reclaim: backend-divergent, out of scope.
//   - Concurrency/race tests: isolated single-backend (task 8.4).
//   - COMPOSITE_KEY_UNSUPPORTED coverage: all in-repo backends support it;
//     the negative case is covered by a unit test with a fake StoreFactory (task 8).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// ukSampleJSON is the canonical sample payload imported to infer the model
// schema. The fields "name" (string) and "amount" (number) are the composite
// key candidates used across all scenarios.
const ukSampleJSON = `{"name":"Sample","amount":1,"status":"draft"}`

// ukSimpleWorkflow is a minimal auto-transition workflow (NONE → CREATED) with
// no processors. All unique-key scenarios use this so the test focus stays on
// uniqueness enforcement, not workflow complexity.
const ukSimpleWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1",
		"name": "uk-wf",
		"initialState": "NONE",
		"active": true,
		"states": {
			"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {}
		}
	}]
}`

// ukAssertErrCode decodes the RFC 9457 Problem Details envelope from raw and
// asserts properties.errorCode == wantCode. Used by every negative-path
// unique-key assertion.
func ukAssertErrCode(t *testing.T, raw []byte, wantCode string) {
	t.Helper()
	var envelope struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Errorf("ukAssertErrCode: failed to decode error body %q: %v", string(raw), err)
		return
	}
	if envelope.Properties.ErrorCode != wantCode {
		t.Errorf("properties.errorCode: got %q, want %q (body: %s)",
			envelope.Properties.ErrorCode, wantCode, string(raw))
	}
}

// ukCapabilityGateOrSkip calls SetUniqueKeysRaw on an UNLOCKED model and
// returns (status, body). If the backend returns 422 COMPOSITE_KEY_UNSUPPORTED
// the test is skipped immediately. All in-repo backends support the feature;
// out-of-tree backends may skip cleanly until they adopt the capability.
//
// Call this AFTER ImportModel and BEFORE LockModel. The model name/version
// must match what was imported.
func ukCapabilityGateOrSkip(t *testing.T, c *client.Client, modelName string, modelVersion int, keysJSON string) (int, []byte) {
	t.Helper()
	status, raw, err := c.SetUniqueKeysRaw(t, modelName, modelVersion, keysJSON)
	if err != nil {
		t.Fatalf("SetUniqueKeysRaw transport error: %v", err)
	}
	if status == http.StatusUnprocessableEntity && strings.Contains(string(raw), "COMPOSITE_KEY_UNSUPPORTED") {
		t.Skip("backend does not support composite unique keys")
	}
	return status, raw
}

// setupUKModel imports a model from ukSampleJSON, applies the given unique-key
// declaration (via the capability gate), locks the model, and imports the
// simple workflow. Fatal on any setup failure so the scenario body can focus
// on the assertions.
//
// modelName must be unique within the tenant to avoid cross-scenario
// interference; callers use scenario-specific names.
func setupUKModel(t *testing.T, c *client.Client, modelName string, keysJSON string) {
	t.Helper()

	if err := c.ImportModel(t, modelName, 1, ukSampleJSON); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}

	// Capability gate: skip if the backend doesn't support composite unique keys.
	status, raw := ukCapabilityGateOrSkip(t, c, modelName, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("SetUniqueKeys on unlocked model: expected 200, got %d: %s", status, string(raw))
	}

	if err := c.LockModel(t, modelName, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, 1, ukSimpleWorkflow); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// --- Scenario 1: create-duplicate → 409 UNIQUE_VIOLATION ---

// RunUniqueKeys_CreateDuplicate verifies that creating a second entity whose
// composite unique key value matches an existing entity returns
// 409 UNIQUE_VIOLATION. The key is declared on a two-field composite
// (name + amount) so the test exercises multi-field canonicalisation as well
// as the violation path.
func RunUniqueKeys_CreateDuplicate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "uk-dup"
	keysJSON := `{"uniqueKeys":[{"id":"name-amount","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, c, model, keysJSON)

	// First entity → success.
	_, err := c.CreateEntity(t, model, 1, `{"name":"Alice","amount":100,"status":"draft"}`)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Second entity with same (name, amount) → 409 UNIQUE_VIOLATION.
	status, raw, err := c.CreateEntityRaw(t, model, 1, `{"name":"Alice","amount":100,"status":"pending"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", status, string(raw))
	}
	ukAssertErrCode(t, raw, "UNIQUE_VIOLATION")
}

// --- Scenario 2: soft-delete frees value → re-create succeeds ---

// RunUniqueKeys_SoftDeleteFreesValue verifies that soft-deleting an entity
// releases its unique key claim so a NEW, SEPARATE request can re-create an
// entity with the same key value. The delete and the re-create are issued as
// two distinct HTTP round-trips (no same-transaction semantics involved).
func RunUniqueKeys_SoftDeleteFreesValue(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "uk-softdel"
	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, c, model, keysJSON)

	// Create entity with name="Eve".
	entityID, err := c.CreateEntity(t, model, 1, `{"name":"Eve","amount":10,"status":"draft"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Soft-delete the entity.
	if err := c.DeleteEntity(t, entityID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Re-create with the same name — must succeed (key was freed by delete).
	status, raw, err := c.CreateEntityRaw(t, model, 1, `{"name":"Eve","amount":20,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("re-create after delete: expected 200, got %d: %s", status, string(raw))
	}
}

// --- Scenario 3: partial key → 422 INVALID_UNIQUE_KEY ---

// RunUniqueKeys_PartialKey verifies that providing only some fields of a
// composite unique key returns 422 INVALID_UNIQUE_KEY. The key requires both
// "name" AND "amount"; the entity payload omits "amount".
func RunUniqueKeys_PartialKey(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "uk-partial"
	keysJSON := `{"uniqueKeys":[{"id":"name-amount","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, c, model, keysJSON)

	// "name" present, "amount" absent → partial key → 422 INVALID_UNIQUE_KEY.
	status, raw, err := c.CreateEntityRaw(t, model, 1, `{"name":"Bob","status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error: %v", err)
	}
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("partial key: expected 422, got %d: %s", status, string(raw))
	}
	ukAssertErrCode(t, raw, "INVALID_UNIQUE_KEY")
}

// --- Scenario 4: all-null/absent key → exempt (both creates succeed) ---

// RunUniqueKeys_AllNullExempt verifies that when ALL fields of every declared
// unique key are absent from the entity payload the entity is exempt from
// uniqueness enforcement — the null/absent key is not a violation. Two entities
// both missing the key fields must both be created successfully.
func RunUniqueKeys_AllNullExempt(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "uk-allnull"
	// Composite key over (name, amount). Both entities below omit both fields.
	keysJSON := `{"uniqueKeys":[{"id":"name-amount","fields":["$.name","$.amount"]}]}`
	setupUKModel(t, c, model, keysJSON)

	// First entity — no "name", no "amount" → all-absent → exempt.
	status, raw, err := c.CreateEntityRaw(t, model, 1, `{"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error (first): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("first all-absent create: expected 200, got %d: %s", status, string(raw))
	}

	// Second entity — also no "name", no "amount" → also exempt; must NOT collide.
	status, raw, err = c.CreateEntityRaw(t, model, 1, `{"status":"pending"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error (second): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("second all-absent create: expected 200, got %d: %s", status, string(raw))
	}
}

// --- Scenario 5: DeleteAll frees values → re-create succeeds ---

// RunUniqueKeys_DeleteAllFreesValues verifies that bulk-deleting all entities
// in a model (DELETE /api/entity/{name}/{version}) releases their unique key
// claims, allowing the same values to be reused in subsequent creates.
func RunUniqueKeys_DeleteAllFreesValues(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "uk-delall"
	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	setupUKModel(t, c, model, keysJSON)

	// Create two entities.
	for _, name := range []string{"Frank", "Grace"} {
		body := `{"name":"` + name + `","amount":1,"status":"draft"}`
		if _, err := c.CreateEntity(t, model, 1, body); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	// DeleteAll — removes both entities and their unique key claims.
	if err := c.DeleteEntitiesByModel(t, model, 1); err != nil {
		t.Fatalf("DeleteEntitiesByModel: %v", err)
	}

	// Re-create with the same names — must succeed (keys freed by DeleteAll).
	for _, name := range []string{"Frank", "Grace"} {
		body := `{"name":"` + name + `","amount":2,"status":"draft"}`
		status, raw, err := c.CreateEntityRaw(t, model, 1, body)
		if err != nil {
			t.Fatalf("CreateEntityRaw transport error (%s): %v", name, err)
		}
		if status != http.StatusOK {
			t.Fatalf("re-create %s after DeleteAll: expected 200, got %d: %s", name, status, string(raw))
		}
	}
}

// --- Scenario 6: multiple independent keys — each enforced ---

// RunUniqueKeys_MultipleKeys verifies that when a model declares two
// independent unique keys (one over "name", one over "amount") each is
// enforced separately:
//   - Duplicate "amount" with distinct "name" → 409 UNIQUE_VIOLATION
//   - Duplicate "name" with distinct "amount" → 409 UNIQUE_VIOLATION
//   - Both distinct → 200
func RunUniqueKeys_MultipleKeys(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "uk-multikey"
	keysJSON := `{"uniqueKeys":[
		{"id":"name-key","fields":["$.name"]},
		{"id":"amount-key","fields":["$.amount"]}
	]}`
	setupUKModel(t, c, model, keysJSON)

	// First entity — both fields unique.
	if _, err := c.CreateEntity(t, model, 1, `{"name":"Lena","amount":111,"status":"draft"}`); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Different name, SAME amount → violates amount-key → 409 UNIQUE_VIOLATION.
	status, raw, err := c.CreateEntityRaw(t, model, 1, `{"name":"Mike","amount":111,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error (dup amount): %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("dup amount: expected 409, got %d: %s", status, string(raw))
	}
	ukAssertErrCode(t, raw, "UNIQUE_VIOLATION")

	// SAME name, different amount → violates name-key → 409 UNIQUE_VIOLATION.
	status, raw, err = c.CreateEntityRaw(t, model, 1, `{"name":"Lena","amount":222,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error (dup name): %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("dup name: expected 409, got %d: %s", status, string(raw))
	}
	ukAssertErrCode(t, raw, "UNIQUE_VIOLATION")

	// Both name and amount distinct → success.
	status, raw, err = c.CreateEntityRaw(t, model, 1, `{"name":"Nina","amount":333,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport error (distinct): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("distinct values: expected 200, got %d: %s", status, string(raw))
	}
}
