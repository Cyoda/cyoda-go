package parity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunEntityUnstorableTextRejected asserts every backend rejects text that no
// store can persist — an unpaired UTF-16 surrogate escape and a byte sequence
// that is not valid UTF-8 — identically, with 400 BAD_REQUEST.
//
// Both are accepted by Go's JSON parser. PostgreSQL text/jsonb rejects them, so
// they used to return 500 there while memory and sqlite accepted them, making
// the storable value set backend-dependent. This scenario is the convergence
// guard: it fails on all three backends if the boundary check is removed.
//
// Note the fix cannot be "decode and re-serialise": Go's decoder rewrites both
// forms to U+FFFD, so that would silently store a character the client never
// sent. The guard therefore reads the raw request bytes.
func RunEntityUnstorableTextRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-unstorable-text-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	cases := []struct {
		name    string
		payload string
	}{
		{"lone-high-surrogate", "{\"name\":\"a\\ud800b\",\"amount\":1,\"status\":\"active\"}"},
		{"lone-low-surrogate", "{\"name\":\"a\\udc00b\",\"amount\":1,\"status\":\"active\"}"},
		{"invalid-utf8", "{\"name\":\"a\xffb\",\"amount\":1,\"status\":\"active\"}"},
	}

	for _, tc := range cases {
		status, raw, err := c.CreateEntityRaw(t, modelName, modelVersion, tc.payload)
		if err != nil {
			t.Fatalf("%s: CreateEntityRaw: %v", tc.name, err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 — text no backend can store must be rejected at the boundary; body: %s",
				tc.name, status, raw)
			continue
		}
		var pd struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &pd); err != nil {
			t.Errorf("%s: decode problem detail %q: %v", tc.name, raw, err)
			continue
		}
		if got, _ := pd.Properties["errorCode"].(string); got != "BAD_REQUEST" {
			t.Errorf("%s: errorCode=%q, want BAD_REQUEST; body: %s", tc.name, got, raw)
		}
		if strings.ContainsRune(string(raw), 0) {
			t.Errorf("%s: response body echoes a raw NUL byte; body: %q", tc.name, raw)
		}
	}

	// A correctly paired surrogate is ordinary text and must still be accepted,
	// so the guard is not simply rejecting non-ASCII.
	okStatus, okRaw, err := c.CreateEntityRaw(t, modelName, modelVersion,
		"{\"name\":\"a\\ud83d\\ude00b\",\"amount\":1,\"status\":\"active\"}")
	if err != nil {
		t.Fatalf("CreateEntityRaw (valid pair): %v", err)
	}
	if okStatus != http.StatusOK {
		t.Fatalf("valid surrogate pair: status=%d, want 200; body: %s", okStatus, okRaw)
	}
}

// RunEntityEmptyDocumentRoundTrips asserts an empty payload is storable and
// readable on every backend, including via the model-wide list.
//
// On postgres this used to write successfully and then fail every subsequent
// read with 500 — for the entity AND for the whole model's list — because the
// stored document consisted only of the _meta block the plugin merges in, and
// removing _meta on read left no data to decode. memory and sqlite were
// unaffected, so this is a backend-divergence guard as much as a regression
// test.
func RunEntityEmptyDocumentRoundTrips(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-empty-doc-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	status, raw, err := c.CreateEntityRaw(t, modelName, modelVersion, `{}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw({}): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("create {}: status=%d, want 200; body: %s", status, raw)
	}

	// The entity itself must be readable.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{}`)
	if err != nil {
		t.Fatalf("CreateEntity({}): %v", err)
	}
	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity on an empty document: %v", err)
	}
	if len(got.Data) != 0 {
		t.Errorf("empty document read back with data %v, want an empty payload", got.Data)
	}

	// The model-wide list is what a single empty document used to take down:
	// one unreadable row failed the whole listing, not just its own GET.
	entities, err := c.ListEntitiesByModel(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("listing a model containing an empty document: %v", err)
	}
	if len(entities) < 2 {
		t.Errorf("list returned %d entities; two empty documents were created", len(entities))
	}
}

// RunEntityNumberOutOfRangeRejected asserts every backend rejects a JSON number
// that PostgreSQL's numeric type cannot hold, rather than one backend failing
// inside the store while the others accept it.
//
// The model's sample matters: on a plain INTEGER field an over-range literal is
// rejected earlier by type validation and never reaches storage, so the scenario
// would pass for the wrong reason. A sample carrying a >64-bit literal makes the
// field UNBOUND_INTEGER, which is the shape that actually reaches the store.
func RunEntityNumberOutOfRangeRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-number-range-test"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"x","big":1e400,"dec":1e-400,"status":"d"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, simpleWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	for _, tc := range []struct{ name, payload string }{
		{"weight-over", `{"name":"x","big":1e131072,"dec":1,"status":"d"}`},
		{"scale-over", `{"name":"x","big":1,"dec":1.5e-16383,"status":"d"}`},
	} {
		status, raw, err := c.CreateEntityRaw(t, modelName, modelVersion, tc.payload)
		if err != nil {
			t.Fatalf("%s: CreateEntityRaw: %v", tc.name, err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 — a number no backend can store must be rejected at the boundary; body: %s",
				tc.name, status, raw)
		}
	}

	// At the limit the number is storable and must still be accepted.
	okStatus, okRaw, err := c.CreateEntityRaw(t, modelName, modelVersion,
		`{"name":"x","big":1e131071,"dec":1e-16383,"status":"d"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw (at limit): %v", err)
	}
	if okStatus != http.StatusOK {
		t.Fatalf("number at the storable limit: status=%d, want 200; body: %s", okStatus, okRaw)
	}
}

// RunEntityDuplicateKeysRejected asserts every backend rejects a payload that
// repeats a name within one object.
//
// Left accepted, such a payload is read as the LAST occurrence by schema
// validation and the GET response and as the FIRST by the criterion evaluator
// and search, so an entity is reported as holding one value while its workflow
// transition was decided on another. That happens on all three backends — the
// criterion runs against the client bytes before any store normalisation — so
// this is a correctness guard, not a storage-format guard.
func RunEntityDuplicateKeysRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-duplicate-keys-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	status, raw, err := c.CreateEntityRaw(t, modelName, modelVersion,
		`{"name":"x","amount":"not-a-number","amount":5,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate name: status=%d, want 400 — the value read depends on which subsystem reads it; body: %s",
			status, raw)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("decode problem detail %q: %v", raw, err)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != "BAD_REQUEST" {
		t.Errorf("errorCode=%q, want BAD_REQUEST; body: %s", got, raw)
	}

	// A clean payload must still be accepted, so the guard is not simply
	// rejecting the model. (Per-object scoping — the same name in two different
	// objects — is covered at the unit level, where a nested sample is free.)
	okStatus, okRaw, err := c.CreateEntityRaw(t, modelName, modelVersion,
		`{"name":"x","amount":5,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw (clean): %v", err)
	}
	if okStatus != http.StatusOK {
		t.Fatalf("clean payload: status=%d, want 200; body: %s", okStatus, okRaw)
	}
}
