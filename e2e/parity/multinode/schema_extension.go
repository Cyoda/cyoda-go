package multinode

import (
	"bytes"
	"testing"
	"time"

	parityclient "github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// schema_extension.go — an extension made on one node is visible on the
// others once the model-cache invalidation broadcast arrives.

func init() {
	Register(
		NamedTest{Name: "SchemaExtension_VisibleOnOtherNode", Fn: RunSchemaExtension_VisibleOnOtherNode},
	)
}

// RunSchemaExtension_VisibleOnOtherNode: node B has the model cached; a write
// on node A adds a field to the locked model; node B's export has the field
// within 5s (the broadcast; the cache lease is far longer).
func RunSchemaExtension_VisibleOnOtherNode(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("need at least 2 nodes, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)
	a := parityclient.NewClient(urls[0], tenant.Token)
	b := parityclient.NewClient(urls[1], tenant.Token)
	const model = "ext-multi"

	if err := a.ImportModel(t, model, 1, `{"name":"seed"}`); err != nil {
		t.Fatalf("import model: %v", err)
	}
	if err := a.LockModel(t, model, 1); err != nil {
		t.Fatalf("lock model: %v", err)
	}
	if err := a.SetChangeLevel(t, model, 1, "STRUCTURAL"); err != nil {
		t.Fatalf("set change level: %v", err)
	}

	// Node B reads the model first, so it holds the schema without the field.
	before, err := b.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("export on node B: %v", err)
	}
	if bytes.Contains(before, []byte("extra")) {
		t.Fatalf("node B's export holds the field before it was added: %s", before)
	}

	if _, err := a.CreateEntity(t, model, 1, `{"name":"a","extra":"x"}`); err != nil {
		t.Fatalf("create on node A: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second) // realtime: poll deadline
	for {
		got, err := b.ExportModel(t, "SIMPLE_VIEW", model, 1)
		if err != nil {
			t.Fatalf("export on node B: %v", err)
		}
		if bytes.Contains(got, []byte("extra")) {
			return
		}
		if time.Now().After(deadline) { // realtime: poll deadline
			t.Fatalf("node B's export lacks the field 5s after node A's write: %s", got)
		}
		time.Sleep(50 * time.Millisecond) // realtime: poll interval
	}
}
