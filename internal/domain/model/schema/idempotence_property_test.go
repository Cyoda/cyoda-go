// internal/domain/model/schema/idempotence_property_test.go
package schema_test

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestIdempotenceApply — I4: Apply(Apply(b, d), d) == Apply(b, d).
func TestIdempotenceApply(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 500
	for i := 0; i < N; i++ {
		seed := int64(i + 40_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			incoming := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			incomingNode, err := importer.Walk(incoming)
			if err != nil {
				t.Fatal(err)
			}
			extended, err := schema.Extend(base, incomingNode, cfg.TargetLevel)
			if err != nil {
				t.Skip(err)
			}
			delta, _ := schema.Diff(base, extended)
			once, err := schema.Apply(base, delta)
			if err != nil {
				t.Fatal(err)
			}
			twice, err := schema.Apply(once, delta)
			if err != nil {
				t.Fatal(err)
			}
			b1, _ := schema.Marshal(once)
			b2, _ := schema.Marshal(twice)
			if string(b1) != string(b2) {
				t.Fatalf("I4 violated\n  once =%s\n  twice=%s", b1, b2)
			}
		})
	}
}

// TestIdempotenceIngest — ingesting the same data twice yields the same
// schema (extension is idempotent, not double-widening).
func TestIdempotenceIngest(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 300
	for i := 0; i < N; i++ {
		seed := int64(i + 50_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			data := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			node, err := importer.Walk(data)
			if err != nil {
				t.Fatal(err)
			}
			e1, err := schema.Extend(base, node, cfg.TargetLevel)
			if err != nil {
				t.Skip(err)
			}
			e2, err := schema.Extend(e1, node, cfg.TargetLevel)
			if err != nil {
				t.Fatal(err)
			}
			b1, _ := schema.Marshal(e1)
			b2, _ := schema.Marshal(e2)
			if string(b1) != string(b2) {
				t.Fatalf("Extend not idempotent\n  once =%s\n  twice=%s", b1, b2)
			}
		})
	}
}
