// internal/domain/model/schema/roundtrip_property_test.go
package schema_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestRoundtripCatalog — I1 master invariant on curated fixtures.
// I1-bis is checked automatically: nil-delta cases must correspond to
// byte-identical schemas.
func TestRoundtripCatalog(t *testing.T) {
	for _, f := range gentree.Catalog {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			old := f.Old
			if old == nil {
				old = schema.NewObjectNode()
			}
			incomingNode, err := importer.Walk(f.Incoming)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			extended, extErr := schema.Extend(old, incomingNode, f.Level)
			if f.ExpectError {
				if extErr == nil {
					t.Fatalf("%s: Extend unexpectedly succeeded at level %q", f.Name, f.Level)
				}
				return
			}
			if extErr != nil {
				t.Fatalf("%s: Extend failed: %v", f.Name, extErr)
			}
			assertRoundTrip(t, old, extended, f.Name)
			if f.ExpectedKinds != nil {
				assertDeltaKinds(t, old, extended, f.ExpectedKinds)
			}
		})
	}
}

// TestRoundtripRandomSeeds — 1000 random seeds; I1 + I1-bis.
func TestRoundtripRandomSeeds(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 1000
	for i := 0; i < N; i++ {
		seed := int64(i + 1)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			old := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			incoming := gentree.GenExtensionPair(r, old, cfg.TargetLevel, cfg)
			incomingNode, err := importer.Walk(incoming)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			extended, err := schema.Extend(old, incomingNode, cfg.TargetLevel)
			if err != nil {
				// Additive extension at Structural should not fail for
				// well-formed generator output.
				t.Fatalf("Extend failed: %v", err)
			}
			assertRoundTrip(t, old, extended, fmt.Sprintf("seed=%d", seed))
		})
	}
}

// assertRoundTrip enforces I1 and I1-bis on a single (old, extended)
// pair. Marshal-equality is byte-level.
func assertRoundTrip(t *testing.T, old, extended *schema.ModelNode, label string) {
	t.Helper()
	delta, err := schema.Diff(old, extended)
	if err != nil {
		t.Fatalf("%s: Diff failed: %v", label, err)
	}
	applied, err := schema.Apply(old, delta)
	if err != nil {
		t.Fatalf("%s: Apply failed: %v", label, err)
	}
	appliedBytes, _ := schema.Marshal(applied)
	extendedBytes, _ := schema.Marshal(extended)
	if string(appliedBytes) != string(extendedBytes) {
		oldB, _ := schema.Marshal(old)
		t.Fatalf("%s: I1 violated\n  old=%s\n  extended=%s\n  applied =%s",
			label, oldB, extendedBytes, appliedBytes)
	}
	// I1-bis: delta==nil iff Marshal(old)==Marshal(extended).
	oldBytes, _ := schema.Marshal(old)
	marshalEqual := string(oldBytes) == string(extendedBytes)
	if (len(delta) == 0) != marshalEqual {
		t.Fatalf("%s: I1-bis violated: delta-nil=%v but Marshal-equal=%v\n  old=%s\n  extended=%s\n  delta=%s",
			label, len(delta) == 0, marshalEqual, oldBytes, extendedBytes, string(delta))
	}
}

// assertDeltaKinds — I6 bidirectional assertion per catalog-declared expected kinds.
func assertDeltaKinds(t *testing.T, old, extended *schema.ModelNode, expected []schema.SchemaOpKind) {
	t.Helper()
	delta, err := schema.Diff(old, extended)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	ops, err := schema.UnmarshalDelta(delta)
	if err != nil {
		t.Fatalf("UnmarshalDelta: %v", err)
	}
	gotKinds := make(map[schema.SchemaOpKind]int)
	for _, op := range ops {
		gotKinds[op.Kind]++
	}
	for _, want := range expected {
		if gotKinds[want] == 0 {
			opsJSON, _ := json.MarshalIndent(ops, "", "  ")
			t.Fatalf("expected kind %q not present in delta ops:\n%s", want, opsJSON)
		}
	}
}
