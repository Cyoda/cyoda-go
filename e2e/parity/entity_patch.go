package parity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// patchWorkflowJSON is a workflow with two states:
//
//	NONE --(init, auto)--> CREATED --(approve, manual)--> APPROVED
//
// The loopback PUT in setupPatchModel advances the token without firing
// "approve". The manual "approve" transition is exercised by
// RunEntityPatchWithTransition and used only there via PatchEntityMerge
// with the transition path segment.
const patchWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1",
		"name": "patch-wf",
		"initialState": "NONE",
		"active": true,
		"states": {
			"NONE": {
				"transitions": [{"name": "init", "next": "CREATED", "manual": false}]
			},
			"CREATED": {
				"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]
			},
			"APPROVED": {}
		}
	}]
}`

// setupPatchModel imports a model with fields {name:string, amount:int,
// status:string}, locks it, and imports patchWorkflowJSON. Returns a
// ready Client. modelName must be unique per scenario to avoid cross-test
// collisions on the shared backend.
func setupPatchModel(t *testing.T, fixture BackendFixture, modelName string) (cli *client.Client, tenantToken string) {
	t.Helper()
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"x","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("setupPatchModel ImportModel %q: %v", modelName, err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("setupPatchModel LockModel %q: %v", modelName, err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, patchWorkflowJSON); err != nil {
		t.Fatalf("setupPatchModel ImportWorkflow %q: %v", modelName, err)
	}
	return c, tenant.Token
}

// setupPatchModelCustomSample is like setupPatchModel but accepts an
// arbitrary sample document. Used by scenarios that need a schema with a
// nested object or an array field.
func setupPatchModelCustomSample(t *testing.T, fixture BackendFixture, modelName, sampleDoc string) *client.Client {
	t.Helper()
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, sampleDoc); err != nil {
		t.Fatalf("setupPatchModelCustomSample ImportModel %q: %v", modelName, err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("setupPatchModelCustomSample LockModel %q: %v", modelName, err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, patchWorkflowJSON); err != nil {
		t.Fatalf("setupPatchModelCustomSample ImportWorkflow %q: %v", modelName, err)
	}
	return c
}

// --- Normal-operation scenarios ---

// RunEntityPatchMergePreservesFields verifies that a merge-patch
// updates only the patched fields and leaves other fields intact.
func RunEntityPatchMergePreservesFields(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-merge-preserve")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-merge-preserve", modelVersion, `{"name":"Alice","amount":30,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"amount":31}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge: status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-patch): %v", err)
	}
	if got.Data["name"] != "Alice" {
		t.Errorf("data.name = %v, want %q (preserved field mutated)", got.Data["name"], "Alice")
	}
	if got.Data["amount"] != float64(31) {
		t.Errorf("data.amount = %v, want 31 (patched field not updated)", got.Data["amount"])
	}
}

// RunEntityPatchNullDeletesKey verifies that a null value in the merge
// patch removes the corresponding key from the stored document.
func RunEntityPatchNullDeletesKey(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-null-delete")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-null-delete", modelVersion, `{"name":"Bob","amount":10,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"status":null}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge: status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-patch): %v", err)
	}
	if _, exists := got.Data["status"]; exists {
		t.Errorf("data.status key still present after null-patch; want it deleted, got %v", got.Data["status"])
	}
	if got.Data["name"] != "Bob" {
		t.Errorf("data.name = %v, want %q (sibling field mutated by null-patch)", got.Data["name"], "Bob")
	}
	if got.Data["amount"] != float64(10) {
		t.Errorf("data.amount = %v, want 10 (sibling field mutated by null-patch)", got.Data["amount"])
	}
}

// RunEntityPatchNestedMerge verifies RFC 7396 recursive merge: a nested
// object patch preserves untouched keys and updates patched keys within
// the locked schema. Only fields declared in the sample document (and
// therefore in the locked schema) are used — adding new keys would
// trigger strict schema validation rejection on a locked model.
func RunEntityPatchNestedMerge(t *testing.T, fixture BackendFixture) {
	t.Helper()
	// Separate model with nested object in schema. Fields a, b, and c are
	// all declared in the sample so they are known to the locked schema.
	c := setupPatchModelCustomSample(t, fixture, "patch-nested-merge", `{"name":"x","meta":{"a":1,"b":2,"c":0}}`)
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-nested-merge", modelVersion, `{"name":"N","meta":{"a":1,"b":2,"c":0}}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	// Merge patch: preserve a (untouched), update b to 9, update c to 3.
	// All three keys are in the locked schema so strict validation passes.
	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"meta":{"b":9,"c":3}}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge: status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-patch): %v", err)
	}
	metaAny, ok := got.Data["meta"]
	if !ok {
		t.Fatal("data.meta key missing after nested patch")
	}
	meta, ok := metaAny.(map[string]any)
	if !ok {
		t.Fatalf("data.meta is %T, want map[string]any", metaAny)
	}
	if meta["a"] != float64(1) {
		t.Errorf("data.meta.a = %v, want 1 (should be preserved — not in patch delta)", meta["a"])
	}
	if meta["b"] != float64(9) {
		t.Errorf("data.meta.b = %v, want 9 (should be updated by patch)", meta["b"])
	}
	if meta["c"] != float64(3) {
		t.Errorf("data.meta.c = %v, want 3 (should be updated by patch)", meta["c"])
	}
}

// RunEntityPatchArrayWholesaleReplace verifies that an array field in a
// merge patch replaces the stored array wholesale (RFC 7396 — arrays are
// atomic values, not merged recursively).
func RunEntityPatchArrayWholesaleReplace(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c := setupPatchModelCustomSample(t, fixture, "patch-array-replace", `{"name":"x","tags":["a","b"]}`)
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-array-replace", modelVersion, `{"name":"T","tags":["a","b"]}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"tags":["z"]}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge: status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-patch): %v", err)
	}
	tagsAny, ok := got.Data["tags"]
	if !ok {
		t.Fatal("data.tags key missing after patch")
	}
	tags, ok := tagsAny.([]any)
	if !ok {
		t.Fatalf("data.tags is %T, want []any", tagsAny)
	}
	if len(tags) != 1 || tags[0] != "z" {
		t.Errorf("data.tags = %v, want [\"z\"] (array should be wholesale-replaced)", tags)
	}
}

// RunEntityPatchEmptyNoOp verifies that an empty merge patch ({}) commits
// a new transaction (so the txId advances) but leaves the entity data
// unchanged.
func RunEntityPatchEmptyNoOp(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-empty-noop")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-empty-noop", modelVersion, `{"name":"C","amount":5,"status":"ok"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID1 := ent.Meta.TransactionID
	if txID1 == "" {
		t.Fatal("GetEntity: meta.transactionId is empty (pre-patch)")
	}

	status, body, err := c.PatchEntityMerge(t, id, "", txID1, `{}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge empty: status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-patch): %v", err)
	}
	if got.Data["name"] != "C" {
		t.Errorf("data.name = %v, want %q (empty patch mutated data)", got.Data["name"], "C")
	}
	if got.Data["amount"] != float64(5) {
		t.Errorf("data.amount = %v, want 5 (empty patch mutated data)", got.Data["amount"])
	}
	// A new transaction must be committed even for an empty patch.
	if got.Meta.TransactionID == txID1 {
		t.Errorf("meta.transactionId unchanged after empty patch (want a new txId, got same %q)", txID1)
	}
}

// RunEntityPatchNumberFidelity verifies that a large integer value
// (greater than 2^53, which float64 cannot represent exactly) is stored
// and returned faithfully. Assertion is done on the raw response body to
// avoid float64 precision loss in the Go json.Decoder.
//
// The sample document uses 2^53+1 (9007199254740993) as the initial value
// so the schema infers the LONG type family. A LONG field accepts a
// subsequent LONG-valued patch without type rejection.
func RunEntityPatchNumberFidelity(t *testing.T, fixture BackendFixture) {
	t.Helper()
	// Sample uses 2^53+1 so the classifier infers LONG, not INTEGER.
	// The post-A.1 numeric classifier no longer widens LONG values into an
	// INTEGER schema, so the sample and all patch values must be LONG-family.
	const bigVal = "9007199254740993" // 2^53 + 1 — not representable as float64
	c := setupPatchModelCustomSample(t, fixture, "patch-num-fidelity",
		`{"name":"x","big":`+bigVal+`}`)
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-num-fidelity", modelVersion,
		`{"name":"x","big":`+bigVal+`}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	// Patch with the same LONG-family value to exercise precision round-trip.
	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"big":`+bigVal+`}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge: status %d, want 200; body=%s", status, body)
	}

	// Use GetEntityBodyRaw so we can inspect the raw JSON text without
	// float64 precision loss.
	rawStatus, rawBody, err := c.GetEntityBodyRaw(t, id)
	if err != nil {
		t.Fatalf("GetEntityBodyRaw transport: %v", err)
	}
	if rawStatus != 200 {
		t.Fatalf("GetEntityBodyRaw: status %d, want 200; body=%s", rawStatus, rawBody)
	}
	if !strings.Contains(string(rawBody), bigVal) {
		t.Errorf("GetEntityBodyRaw body does not contain literal %q (big-integer precision lost); body=%s",
			bigVal, rawBody)
	}
}

// RunEntityPatchStarUnconditional verifies that If-Match: * bypasses the
// CAS check and the patch lands unconditionally.
func RunEntityPatchStarUnconditional(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-star-unconditional")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-star-unconditional", modelVersion, `{"name":"D","amount":0,"status":"x"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	status, body, err := c.PatchEntityMerge(t, id, "", "*", `{"amount":42}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge star: status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-patch): %v", err)
	}
	if got.Data["amount"] != float64(42) {
		t.Errorf("data.amount = %v, want 42 (star unconditional patch did not land)", got.Data["amount"])
	}
}

// RunEntityPatchWithTransition verifies that a PATCH with a named
// transition fires the transition, changes state, and applies the merge
// patch to the entity data.
func RunEntityPatchWithTransition(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-with-transition")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-with-transition", modelVersion, `{"name":"E","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-create): %v", err)
	}
	if ent.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED after init, got %s", ent.Meta.State)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	// Fire the "approve" manual transition via PATCH.
	status, body, err := c.PatchEntityMerge(t, id, "approve", txID, `{"amount":75}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge (approve) transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("PatchEntityMerge (approve): status %d, want 200; body=%s", status, body)
	}

	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-transition): %v", err)
	}
	if got.Meta.State != "APPROVED" {
		t.Errorf("meta.state = %q, want APPROVED after approve transition", got.Meta.State)
	}
	if got.Data["amount"] != float64(75) {
		t.Errorf("data.amount = %v, want 75 (merge-patch not applied)", got.Data["amount"])
	}
}

// --- Error scenarios ---

// RunEntityPatchNotFound verifies that PATCHing a non-existent entity
// returns 404.
func RunEntityPatchNotFound(t *testing.T, fixture BackendFixture) {
	t.Helper()
	// Use any tenant so we have a valid auth token; the entity uuid is fresh/random.
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	// A randomly-minted UUID cannot exist in the backend.
	bogusID := uuid.New()
	status, body, err := c.PatchEntityMerge(t, bogusID, "", "*", `{"amount":1}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 404 {
		t.Errorf("PatchEntityMerge (non-existent): status %d, want 404; body=%s", status, body)
	}
}

// RunEntityPatchStaleTokenIs412 verifies that supplying a txId that has
// been superseded by a subsequent update returns 412 Precondition Failed.
func RunEntityPatchStaleTokenIs412(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-stale-412")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-stale-412", modelVersion, `{"name":"F","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-advance): %v", err)
	}
	staleTxID := ent.Meta.TransactionID
	if staleTxID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	// Advance the txId so staleTxID is now stale.
	if err := c.UpdateEntityData(t, id, `{"name":"F","amount":2,"status":"new"}`); err != nil {
		t.Fatalf("UpdateEntityData (advance): %v", err)
	}

	// PATCH with the stale token must fail.
	status, body, err := c.PatchEntityMerge(t, id, "", staleTxID, `{"amount":5}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 412 {
		t.Errorf("PatchEntityMerge (stale): status %d, want 412; body=%s", status, body)
	}

	// Confirm the stale patch did not land.
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (post-stale-attempt): %v", err)
	}
	if got.Data["amount"] != float64(2) {
		t.Errorf("data.amount = %v, want 2 (stale patch must not land); got stale-patch value", got.Data["amount"])
	}
}

// RunEntityPatchXMLFormatIs415 verifies that requesting a PATCH against
// the XML format segment returns 415 Unsupported Media Type.
func RunEntityPatchXMLFormatIs415(t *testing.T, fixture BackendFixture) {
	t.Helper()
	// Use any tenant so we have a valid auth token; we do not need a real entity.
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	bogusID := uuid.New()
	// format="XML", contentType="application/merge-patch+json", ifMatch="*"
	status, body, err := c.PatchEntityRaw(t, bogusID, "XML", "", "application/merge-patch+json", "*", `{"amount":1}`)
	if err != nil {
		t.Fatalf("PatchEntityRaw transport: %v", err)
	}
	if status != 415 {
		t.Errorf("PatchEntityRaw (XML format): status %d, want 415; body=%s", status, body)
	}
}

// RunEntityPatchWrongContentTypeIs415 verifies that sending
// Content-Type: application/json (not merge-patch+json) returns 415.
func RunEntityPatchWrongContentTypeIs415(t *testing.T, fixture BackendFixture) {
	t.Helper()
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	bogusID := uuid.New()
	// format="JSON", contentType="application/json" (wrong), ifMatch="*"
	status, body, err := c.PatchEntityRaw(t, bogusID, "JSON", "", "application/json", "*", `{"amount":1}`)
	if err != nil {
		t.Fatalf("PatchEntityRaw transport: %v", err)
	}
	if status != 415 {
		t.Errorf("PatchEntityRaw (wrong content-type): status %d, want 415; body=%s", status, body)
	}
}

// RunEntityPatchMissingIfMatchIs428 verifies that omitting the If-Match
// header returns 428 Precondition Required.
func RunEntityPatchMissingIfMatchIs428(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-missing-ifmatch")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-missing-ifmatch", modelVersion, `{"name":"G","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// ifMatch="" omits the If-Match header entirely.
	status, body, err := c.PatchEntityMerge(t, id, "", "", `{"amount":9}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 428 {
		t.Errorf("PatchEntityMerge (no If-Match): status %d, want 428; body=%s", status, body)
	}
}

// RunEntityPatchJSONPatchNotImplemented verifies that
// application/json-patch+json returns 501 Not Implemented.
func RunEntityPatchJSONPatchNotImplemented(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-jsonpatch-501")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-jsonpatch-501", modelVersion, `{"name":"H","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// application/json-patch+json (RFC 6902) is scaffolded but not implemented.
	status, body, err := c.PatchEntityRaw(t, id, "JSON", "", "application/json-patch+json", "*", `[]`)
	if err != nil {
		t.Fatalf("PatchEntityRaw transport: %v", err)
	}
	if status != 501 {
		t.Errorf("PatchEntityRaw (json-patch+json): status %d, want 501; body=%s", status, body)
	}
}

// RunEntityPatchTypeMismatchIs400 verifies that a merge patch that would
// produce an invalid typed field (e.g. string for an int field) returns
// 400 with a client validation error.
func RunEntityPatchTypeMismatchIs400(t *testing.T, fixture BackendFixture) {
	t.Helper()
	// The standard patch model has amount typed as INTEGER.
	c, _ := setupPatchModel(t, fixture, "patch-type-mismatch")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-type-mismatch", modelVersion, `{"name":"I","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	// Patch "amount" with a string — violates the INTEGER schema field.
	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"amount":"not-a-number"}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 400 {
		t.Errorf("PatchEntityMerge (type mismatch): status %d, want 400; body=%s", status, body)
	}
	// The error body is an RFC 7807 Problem Details envelope. The canonical
	// error code (INCOMPATIBLE_TYPE, VALIDATION_FAILED, or BAD_REQUEST) is
	// nested under the "properties" map, not at the top level.
	var problem struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("type-mismatch 400 body is not JSON: %v; body=%s", err, body)
	}
	switch problem.Properties.ErrorCode {
	case "INCOMPATIBLE_TYPE", "VALIDATION_FAILED", "BAD_REQUEST":
		// acceptable client validation codes
	default:
		t.Errorf("type-mismatch: properties.errorCode = %q; want INCOMPATIBLE_TYPE / VALIDATION_FAILED / BAD_REQUEST; body=%s",
			problem.Properties.ErrorCode, body)
	}
}

// RunEntityPatchStrictRejectsUnknownField verifies that on a locked model
// a merge patch containing an unknown field returns 400 (strict schema
// validation rejects extra fields).
//
// If the backend returns 200 instead — meaning the locked model does not
// strictly reject unknown fields on PATCH — this test will FAIL. That
// failure is intentional: it signals a behavioural discrepancy that the
// controller must evaluate, and the assertion must not be silently
// weakened.
func RunEntityPatchStrictRejectsUnknownField(t *testing.T, fixture BackendFixture) {
	t.Helper()
	c, _ := setupPatchModel(t, fixture, "patch-strict-unknown")
	const modelVersion = 1

	id, err := c.CreateEntity(t, "patch-strict-unknown", modelVersion, `{"name":"J","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity (pre-patch): %v", err)
	}
	txID := ent.Meta.TransactionID
	if txID == "" {
		t.Fatal("GetEntity: meta.transactionId is empty")
	}

	// "bogusField" is not declared in the locked schema.
	status, body, err := c.PatchEntityMerge(t, id, "", txID, `{"bogusField":"x"}`)
	if err != nil {
		t.Fatalf("PatchEntityMerge transport: %v", err)
	}
	if status != 400 {
		t.Errorf("PatchEntityMerge (unknown field on locked model): status %d, want 400; body=%s", status, body)
	}
}

// --- Cross-tenant isolation ---

// RunEntityPatchCrossTenantIsNotFound verifies that tenant B cannot PATCH
// an entity owned by tenant A — the PATCH must return 404 (B must not be
// able to patch or even confirm the existence of A's entity).
func RunEntityPatchCrossTenantIsNotFound(t *testing.T, fixture BackendFixture) {
	t.Helper()

	// Tenant A creates an entity.
	tenantA := fixture.NewTenant(t)
	cA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	const modelName = "patch-cross-tenant"
	const modelVersion = 1
	if err := cA.ImportModel(t, modelName, modelVersion, `{"name":"x","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("tenantA ImportModel: %v", err)
	}
	if err := cA.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("tenantA LockModel: %v", err)
	}
	if err := cA.ImportWorkflow(t, modelName, modelVersion, patchWorkflowJSON); err != nil {
		t.Fatalf("tenantA ImportWorkflow: %v", err)
	}
	idA, err := cA.CreateEntity(t, modelName, modelVersion, `{"name":"K","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("tenantA CreateEntity: %v", err)
	}

	// Tenant B mints a fresh identity, imports the same model (so the 404
	// can only be entity-level, not model-level), and attempts to PATCH A's entity.
	tenantB := fixture.NewTenant(t)
	cB := client.NewClient(fixture.BaseURL(), tenantB.Token)
	if err := cB.ImportModel(t, modelName, modelVersion, `{"name":"x","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("tenantB ImportModel: %v", err)
	}
	if err := cB.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("tenantB LockModel: %v", err)
	}
	if err := cB.ImportWorkflow(t, modelName, modelVersion, patchWorkflowJSON); err != nil {
		t.Fatalf("tenantB ImportWorkflow: %v", err)
	}
	status, body, err := cB.PatchEntityMerge(t, idA, "", "*", `{"amount":999}`)
	if err != nil {
		t.Fatalf("tenantB PatchEntityMerge transport: %v", err)
	}
	if status != 404 {
		t.Errorf("cross-tenant PATCH: status %d, want 404 (tenant B must not access tenant A's entity); body=%s",
			status, body)
	}

	// Verify the entity is still intact from tenant A's perspective.
	got, err := cA.GetEntity(t, idA)
	if err != nil {
		t.Fatalf("tenantA GetEntity (after cross-tenant attempt): %v", err)
	}
	if got.Data["amount"] != float64(1) {
		t.Errorf("entity mutated by cross-tenant PATCH: data.amount = %v, want 1", got.Data["amount"])
	}
}

