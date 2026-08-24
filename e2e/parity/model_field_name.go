package parity

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunModelFieldNameRejected asserts that every backend refuses a field name the
// wire jsonPath grammar cannot address, on BOTH doors that establish a model's
// field set: the explicit sample-data import, and the ChangeLevel-driven schema
// extension performed by an entity write.
//
// The rule is engine-layer — it runs above the SPI, before any store is called
// — so no backend may answer differently. That is precisely why it is worth a
// parity scenario: a backend that accepted such a name would be recording a
// field its own query surface can never resolve, and the divergence would show
// up as an empty page rather than an error.
func RunModelFieldNameRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	// --- Door 1: explicit model import ---
	const importModel = "fieldname-import"
	status, body, err := c.ImportModelRaw(t, importModel, 1, `{"first name":"x"}`)
	if err != nil {
		t.Fatalf("ImportModelRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("import with unaddressable field name: status=%d, want 400; body: %s", status, body)
	}
	if !containsErrorCode(body, "VALIDATION_FAILED") {
		t.Errorf("import rejection must carry VALIDATION_FAILED; body: %s", body)
	}
	if !bytes.Contains(body, []byte("first name")) {
		t.Errorf("import rejection must name the offending field; body: %s", body)
	}
	// Fail closed: nothing was recorded, so the model must not exist.
	if _, err := c.ExportModel(t, "SIMPLE_VIEW", importModel, 1); err == nil {
		t.Error("a rejected import must leave no model behind")
	}

	// --- Door 2: schema extension on an entity write ---
	const extendModel = "fieldname-extend"
	if err := c.ImportModel(t, extendModel, 1, `{"name":"x"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, extendModel, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, extendModel, 1, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}

	preSchema, err := c.ExportModel(t, "SIMPLE_VIEW", extendModel, 1)
	if err != nil {
		t.Fatalf("pre ExportModel: %v", err)
	}

	status, body, err = c.CreateEntityRaw(t, extendModel, 1, `{"name":"x","first name":"y"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("entity write extending with an unaddressable field name: status=%d, want 400; body: %s", status, body)
	}
	if !containsErrorCode(body, "VALIDATION_FAILED") {
		t.Errorf("extension rejection must carry VALIDATION_FAILED; body: %s", body)
	}
	if !bytes.Contains(body, []byte("first name")) {
		t.Errorf("extension rejection must name the offending field; body: %s", body)
	}

	postSchema, err := c.ExportModel(t, "SIMPLE_VIEW", extendModel, 1)
	if err != nil {
		t.Fatalf("post ExportModel: %v", err)
	}
	if !bytes.Equal(preSchema, postSchema) {
		t.Errorf("rejected extension mutated the schema\n  pre:  %s\n  post: %s", preSchema, postSchema)
	}

	// --- Positive control: the whole legal charset still works on both doors ---
	const okModel = "fieldname-ok"
	const okSample = `{"_":1,"-":2,"_meta":3,"first-name":"x","first_name":"y",` +
		`"camelCase":1,"UPPER":1,"0abc":1,"x_1-2A":1,"nested":{"b-c":[{"d_1":1}]},"tags":["a"]}`
	if err := c.ImportModel(t, okModel, 1, okSample); err != nil {
		t.Fatalf("addressable field names must import: %v", err)
	}
	if err := c.LockModel(t, okModel, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, okModel, 1, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	extended := `{"_":1,"-":2,"_meta":3,"first-name":"x","first_name":"y",` +
		`"camelCase":1,"UPPER":1,"0abc":1,"x_1-2A":1,"nested":{"b-c":[{"d_1":1}]},"tags":["a"],"new-field_9":"yes"}`
	if _, err := c.CreateEntity(t, okModel, 1, extended); err != nil {
		t.Fatalf("an addressable new field must still extend the schema: %v", err)
	}
	grown, err := c.ExportModel(t, "SIMPLE_VIEW", okModel, 1)
	if err != nil {
		t.Fatalf("post ExportModel: %v", err)
	}
	if !bytes.Contains(grown, []byte("new-field_9")) {
		t.Errorf("expected the extended schema to carry new-field_9; got %s", grown)
	}
}
