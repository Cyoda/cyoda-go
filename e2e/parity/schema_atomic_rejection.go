package parity

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunSchemaExtensionAtomicRejection asserts B-I6: when a
// ChangeLevel-violating extension is attempted, no partial state
// is persisted. Asserted by HTTP round-trip: the SIMPLE_VIEW bytes
// are byte-identical before and after the rejected CreateEntity call.
//
// The model is locked with ChangeLevel=ARRAY_LENGTH, which forbids
// adding new structural fields. A CreateEntity that introduces
// "newField" must be rejected; post-rejection the schema bytes must
// be indistinguishable from the pre-call snapshot.
func RunSchemaExtensionAtomicRejection(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "b-i6-atomic-reject"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"x":"v"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// ARRAY_LENGTH forbids adding new top-level fields — any structural
	// shape-change rejects. See api/generated.go:SetEntityModelChangeLevelParamsChangeLevel.
	if err := c.SetChangeLevel(t, modelName, modelVersion, "ARRAY_LENGTH"); err != nil {
		t.Fatalf("SetChangeLevel ARRAY_LENGTH: %v", err)
	}

	preSchema, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("pre ExportModel: %v", err)
	}

	structural, _ := json.Marshal(map[string]any{"x": "v", "newField": "appear"})
	if _, err := c.CreateEntity(t, modelName, modelVersion, string(structural)); err == nil {
		t.Fatal("CreateEntity with structural shape-change under ChangeLevel=ARRAY_LENGTH must fail")
	}

	postSchema, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("post ExportModel: %v", err)
	}
	if !bytes.Equal(preSchema, postSchema) {
		t.Errorf("%s: rejected extension mutated schema\n  pre:  %s\n  post: %s",
			t.Name(), string(preSchema), string(postSchema))
	}
}
