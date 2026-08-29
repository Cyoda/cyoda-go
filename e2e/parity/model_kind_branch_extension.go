package parity

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunModelKindBranchExtension asserts that every backend answers the same way
// when a write proposes a kind for a path, and when it sends one the path
// already declares.
//
// Engine-layer like its neighbours: the extension gate and the delta it emits
// run above the SPI, before any store is asked to persist anything. It is worth
// a parity scenario because the two halves land in different places — the gate
// decides in the engine, but the delta is appended through the plugin's own
// extension log and folded back on read, so a backend that replayed it
// differently would disagree with the engine about which kinds a field
// declares, and search reads exactly that.
func RunModelKindBranchExtension(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model = "kind-branch-extension"
	if err := c.ImportModel(t, model, 1, `{"s":"x"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	// Below STRUCTURAL, giving a declared path a second kind is refused — as a
	// change-level violation naming the level that resolves it, since raising
	// the level is exactly what does.
	if err := c.SetChangeLevel(t, model, 1, "TYPE"); err != nil {
		t.Fatalf("SetChangeLevel(TYPE): %v", err)
	}
	preSchema, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("pre ExportModel: %v", err)
	}
	status, body, err := c.CreateEntityRaw(t, model, 1, `{"s":["A"]}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("adding a kind at TYPE: status=%d, want 400; body: %s", status, body)
	}
	if !bytes.Contains(body, []byte("STRUCTURAL")) {
		t.Errorf("the rejection must name the level that resolves it; body: %s", body)
	}
	postSchema, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("post ExportModel: %v", err)
	}
	if !bytes.Equal(preSchema, postSchema) {
		t.Errorf("a rejected write mutated the schema\n  pre:  %s\n  post: %s", preSchema, postSchema)
	}

	// At STRUCTURAL the write is accepted and the extension records the branch.
	if err := c.SetChangeLevel(t, model, 1, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel(STRUCTURAL): %v", err)
	}
	if _, err := c.CreateEntity(t, model, 1, `{"s":["A"]}`); err != nil {
		t.Fatalf("adding a kind at STRUCTURAL must be accepted: %v", err)
	}

	// The export names both branches — which is the delta having survived the
	// plugin's extension log and been folded back on read.
	schema, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	for _, want := range []string{`".s":"STRING"`, `".s[*]":"STRING"`} {
		if !bytes.Contains(schema, []byte(want)) {
			t.Errorf("export must name the %s branch; got %s", want, schema)
		}
	}

	// And a write of EITHER declared kind is admissible at a level that permits
	// no schema change: neither proposes one any more. This is the limitation
	// that made a multi-kind model unusable with a changeLevel set — half of its
	// own declared data was refused.
	if err := c.SetChangeLevel(t, model, 1, "TYPE"); err != nil {
		t.Fatalf("SetChangeLevel(TYPE): %v", err)
	}
	for _, doc := range []string{`{"s":"plain"}`, `{"s":["A","B"]}`} {
		if _, err := c.CreateEntity(t, model, 1, doc); err != nil {
			t.Errorf("a write matching a declared kind must be accepted (%s): %v", doc, err)
		}
	}
}
