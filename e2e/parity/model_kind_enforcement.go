package parity

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunModelKindEnforcementRejected asserts that every backend refuses a value
// whose JSON kind is not among the kinds the field declares.
//
// The rule is engine-layer — validation runs above the SPI, before any store is
// called — so no backend may answer differently. It is worth a parity scenario
// because a backend that accepted such a write would be storing a value no
// predicate on that field can address: search compares by the declared type,
// and the declared type says the value cannot be a container.
func RunModelKindEnforcementRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "kind-enforcement"
	if err := c.ImportModel(t, model, 1, `{"s":"x","a":["x"],"o":{"k":"v"}}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	preSchema, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("pre ExportModel: %v", err)
	}

	// Every direction of the same question: is this value's kind declared here?
	for _, tc := range []struct{ name, payload, want string }{
		{"array into a scalar field", `{"s":["A"]}`, "expected scalar, got array"},
		{"object into a scalar field", `{"s":{"k":"v"}}`, "expected scalar, got object"},
		{"object element in an array of scalars", `{"a":[{"k":"v"}]}`, "expected scalar, got object"},
		{"scalar into an array field", `{"a":"x"}`, "expected array, got string"},
		{"array into an object field", `{"o":["x"]}`, "expected object, got array"},
	} {
		status, body, err := c.CreateEntityRaw(t, model, 1, tc.payload)
		if err != nil {
			t.Fatalf("%s: CreateEntityRaw: %v", tc.name, err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400; body: %s", tc.name, status, body)
		}
		if !bytes.Contains(body, []byte(tc.want)) {
			t.Errorf("%s: rejection must explain the kind mismatch (%q); body: %s", tc.name, tc.want, body)
		}
	}

	// Fail closed: no rejected write widened the model.
	postSchema, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("post ExportModel: %v", err)
	}
	if !bytes.Equal(preSchema, postSchema) {
		t.Errorf("a rejected write mutated the schema\n  pre:  %s\n  post: %s", preSchema, postSchema)
	}

	// Positive control: the declared kinds still write.
	if _, err := c.CreateEntity(t, model, 1, `{"s":"ok","a":["y"],"o":{"k":"v"}}`); err != nil {
		t.Fatalf("a payload of the declared kinds must be stored: %v", err)
	}
}

// RunModelSampleDataCollectionImport asserts that every backend reads an array
// sample body as a collection of documents — the same reading the entity
// ingress gives an array body — and refuses a body that is no kind of document
// collection at all.
//
// Engine-layer like the scenario above: the derivation runs before any store is
// called, so a backend answering differently would be registering a different
// model from the same request.
func RunModelSampleDataCollectionImport(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "sample-collection"
	if err := c.ImportModel(t, model, 1, `[{"name":"A","tags":["A","B"]},{"name":"B","sku":1}]`); err != nil {
		t.Fatalf("an array of sample documents must import: %v", err)
	}
	if err := c.LockModel(t, model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	schema, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	for _, want := range []string{`".name":"STRING"`, `".sku":"INTEGER"`, `".tags[*]":"STRING"`} {
		if !bytes.Contains(schema, []byte(want)) {
			t.Errorf("derived model must carry %s; got %s", want, schema)
		}
	}

	// The registered model admits the documents it was derived from.
	for _, doc := range []string{`{"name":"A","tags":["A","B"]}`, `{"name":"B","sku":1}`} {
		if _, err := c.CreateEntity(t, model, 1, doc); err != nil {
			t.Errorf("the model must admit the document it was derived from (%s): %v", doc, err)
		}
	}

	// A body that is not a document, or a collection of them, is refused and
	// leaves no model behind.
	const rejected = "sample-not-a-document"
	status, body, err := c.ImportModelRaw(t, rejected, 1, `["A","B"]`)
	if err != nil {
		t.Fatalf("ImportModelRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("import of a scalar collection: status=%d, want 400; body: %s", status, body)
	}
	if !containsErrorCode(body, "VALIDATION_FAILED") {
		t.Errorf("import rejection must carry VALIDATION_FAILED; body: %s", body)
	}
	if _, err := c.ExportModel(t, "SIMPLE_VIEW", rejected, 1); err == nil {
		t.Error("a rejected import must leave no model behind")
	}
}
