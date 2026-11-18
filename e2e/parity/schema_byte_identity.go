package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunSchemaExtensionCrossBackendByteIdentity drives a deterministic
// 20-field-widening sequence through the HTTP layer and asserts the
// returned bytes match a canonical oracle computed via the in-memory
// schema package + SimpleViewExporter. Cross-backend byte identity
// holds when all three backends produce the same bytes as the oracle.
// This asserts B-I1 at the observable HTTP boundary.
func RunSchemaExtensionCrossBackendByteIdentity(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "b-i1-byte-identity"
	const modelVersion = 1

	// Body 0 seeds the schema with field_0. Matches the oracle sequence.
	if err := c.ImportModel(t, modelName, modelVersion, `{"field_0":"v0"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// STRUCTURAL is required for new fields to widen the schema on entity
	// create. Order mirrors RunSchemaExtensionsSequentialFoldAcrossRequests.
	if err := c.SetChangeLevel(t, modelName, modelVersion, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel STRUCTURAL: %v", err)
	}

	// Drive 19 more widening entity creates (field_0..field_19 present after).
	for i := 1; i < 20; i++ {
		body := map[string]string{}
		for j := 0; j <= i; j++ {
			body[fmt.Sprintf("field_%d", j)] = fmt.Sprintf("v%d", j)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body %d: %v", i, err)
		}
		if _, err := c.CreateEntity(t, modelName, modelVersion, string(raw)); err != nil {
			t.Fatalf("CreateEntity #%d: %v", i, err)
		}
	}

	// Retrieve the current schema via SIMPLE_VIEW.
	got, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}

	// Canonical expected bytes: the oracle computes the same fold via
	// the in-memory schema package + SimpleViewExporter, so any backend
	// that faithfully implements the fold contract will byte-match.
	// Model is locked → "LOCKED" is what the exporter carries in
	// currentState when called server-side (see internal/domain/model/service.go).
	expected, err := expectedSimpleViewFromSequence(20, "LOCKED")
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}

	if !bytes.Equal(got, expected) {
		t.Errorf("cross-backend byte identity failed for %s\n  got:      %s\n  expected: %s",
			t.Name(), string(got), string(expected))
	}
}
