package parity

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSchemaExtensionLocalCacheInvalidationOnCommit asserts B-I8:
// after an extension commits, the immediate next Get on the same
// node returns the post-extension schema, not stale cached bytes.
// The model cache (internal/cluster/modelcache) invalidates the entry
// after every extension call — this test exercises that invalidation
// end-to-end via the HTTP boundary.
func RunSchemaExtensionLocalCacheInvalidationOnCommit(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "b-i8-cache-inv"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"field_0":"seed"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, modelName, modelVersion, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel STRUCTURAL: %v", err)
	}

	// Warm the local cache by fetching the schema pre-extension.
	schema1, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("warm ExportModel: %v", err)
	}

	// Extend via entity create. The extension call invalidates the
	// model cache entry for this model.
	body, _ := json.Marshal(map[string]any{"field_0": "v", "new_field": "appear"})
	if _, err := c.CreateEntity(t, modelName, modelVersion, string(body)); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Immediate re-fetch must reflect the extension — if the cache
	// returned stale bytes, schema2 would equal schema1.
	schema2, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("post ExportModel: %v", err)
	}
	if bytes.Equal(schema1, schema2) {
		t.Errorf("%s: Get returned stale cached schema after extension — cache not invalidated\n  both: %s",
			t.Name(), string(schema1))
	}
}
