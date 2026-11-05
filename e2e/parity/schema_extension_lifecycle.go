package parity

import (
	"bytes"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/exporter"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// extendThroughEntity widens a freshly imported model through the
// entity-write ingress and leaves the model LOCKED with no live entities:
// Lock → SetChangeLevel STRUCTURAL → CreateEntity(ext) → DeleteEntity.
// What it leaves behind is one committed schema extension.
func extendThroughEntity(t *testing.T, c *client.Client, modelName string, modelVersion int, ext map[string]string) {
	t.Helper()
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, modelName, modelVersion, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel STRUCTURAL: %v", err)
	}
	raw, _ := json.Marshal(ext)
	id, err := c.CreateEntity(t, modelName, modelVersion, string(raw))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := c.DeleteEntity(t, id); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
}

// expectedSimpleViewAfterReimport is the oracle for an import over an
// existing model: the fold of bodies (see expectedSimpleViewFromBodies)
// merged with the walked sample, as ImportModel merges a sample into the
// schema it reads.
func expectedSimpleViewAfterReimport(t *testing.T, bodies []map[string]string, sample map[string]string, currentState string) []byte {
	t.Helper()
	var current *schema.ModelNode
	for i, body := range bodies {
		raw, _ := json.Marshal(body)
		if current == nil {
			walked, err := importer.NewSampleDataImporter().Import(bytes.NewReader(raw), "JSON")
			if err != nil {
				t.Fatalf("oracle: import body %d: %v", i, err)
			}
			current = walked
			continue
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("oracle: parse body %d: %v", i, err)
		}
		next, err := schema.Extend(current, parsed, spi.ChangeLevelStructural)
		if err != nil {
			t.Fatalf("oracle: extend body %d: %v", i, err)
		}
		current = next
	}
	raw, _ := json.Marshal(sample)
	sampleNode, err := importer.NewSampleDataImporter().Import(bytes.NewReader(raw), "JSON")
	if err != nil {
		t.Fatalf("oracle: import sample: %v", err)
	}
	out, err := exporter.NewSimpleViewExporter(currentState).Export(schema.Merge(current, sampleNode))
	if err != nil {
		t.Fatalf("oracle: export: %v", err)
	}
	return out
}

// RunSchemaExtensionSurvivesUnlockAndReimport asserts that a schema
// extension committed while a model was LOCKED is part of the model's
// schema after Unlock, after a further Lock/Unlock cycle, and after a
// re-import merges a new sample into it.
//
// Scenario: import {field_0} → Lock → STRUCTURAL → create and delete an
// entity carrying field_1 → Unlock → export (field_0, field_1) → Lock →
// Unlock → import {field_2} → export (field_0, field_1, field_2).
func RunSchemaExtensionSurvivesUnlockAndReimport(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "ext-survives-unlock"
	const modelVersion = 1

	seed := map[string]string{"field_0": "seed"}
	seedRaw, _ := json.Marshal(seed)
	if err := c.ImportModel(t, modelName, modelVersion, string(seedRaw)); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	ext := map[string]string{"field_0": "e1", "field_1": "e1"}
	extendThroughEntity(t, c, modelName, modelVersion, ext)
	if err := c.UnlockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("UnlockModel: %v", err)
	}

	got, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel after unlock: %v", err)
	}
	want, err := expectedSimpleViewFromBodies([]map[string]string{seed, ext}, "UNLOCKED")
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("schema after Unlock lost the extension committed while LOCKED\n  got:  %s\n  want: %s", got, want)
	}

	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("second LockModel: %v", err)
	}
	if err := c.UnlockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("second UnlockModel: %v", err)
	}
	sample := map[string]string{"field_2": "s2"}
	sampleRaw, _ := json.Marshal(sample)
	if err := c.ImportModel(t, modelName, modelVersion, string(sampleRaw)); err != nil {
		t.Fatalf("re-ImportModel: %v", err)
	}

	got, err = c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel after re-import: %v", err)
	}
	want = expectedSimpleViewAfterReimport(t, []map[string]string{seed, ext}, sample, "UNLOCKED")
	if !bytes.Equal(got, want) {
		t.Errorf("schema after re-import is not the extended schema merged with the sample\n  got:  %s\n  want: %s", got, want)
	}
}

// RunSchemaExtensionDoesNotOutliveModel asserts that a model imported
// under the reference of a deleted model has exactly the imported
// schema: nothing of the deleted model's extensions carries over.
//
// Scenario: import {field_0} → Lock → STRUCTURAL → create and delete an
// entity carrying field_1 → Unlock → DeleteModel → import {other} →
// export (other only).
func RunSchemaExtensionDoesNotOutliveModel(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "ext-outlives-model"
	const modelVersion = 1

	seed := map[string]string{"field_0": "seed"}
	seedRaw, _ := json.Marshal(seed)
	if err := c.ImportModel(t, modelName, modelVersion, string(seedRaw)); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	extendThroughEntity(t, c, modelName, modelVersion, map[string]string{"field_0": "e1", "field_1": "e1"})
	if err := c.UnlockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("UnlockModel: %v", err)
	}
	if err := c.DeleteModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}

	fresh := map[string]string{"other": "x"}
	freshRaw, _ := json.Marshal(fresh)
	if err := c.ImportModel(t, modelName, modelVersion, string(freshRaw)); err != nil {
		t.Fatalf("ImportModel after delete: %v", err)
	}
	got, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	want, err := expectedSimpleViewFromBodies([]map[string]string{fresh}, "UNLOCKED")
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("model imported over a deleted model's reference carries the deleted model's extensions\n  got:  %s\n  want: %s", got, want)
	}
}
