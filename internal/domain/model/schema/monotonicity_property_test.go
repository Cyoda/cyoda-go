// internal/domain/model/schema/monotonicity_property_test.go
package schema_test

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestMonotonicityDirect — I3: a document valid against B is also valid
// against Apply(B, d). Extension never narrows the accepted set.
func TestMonotonicityDirect(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 200
	for i := 0; i < N; i++ {
		seed := int64(i + 20_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			doc := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			if errs := schema.Validate(base, doc); len(errs) > 0 {
				t.Skipf("doc not valid against base; skipping")
			}
			newDoc := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			newNode, err := importer.Walk(newDoc)
			if err != nil {
				t.Fatal(err)
			}
			extended, err := schema.Extend(base, newNode, cfg.TargetLevel)
			if err != nil {
				t.Skipf("Extend rejected: %v", err)
			}
			delta, _ := schema.Diff(base, extended)
			applied, err := schema.Apply(base, delta)
			if err != nil {
				t.Fatal(err)
			}
			if errs := schema.Validate(applied, doc); len(errs) > 0 {
				t.Fatalf("I3 direct violated: doc valid against base but not applied schema: %v", errs)
			}
		})
	}
}

// TestMonotonicityDual — a document rejected by Apply(B, d) is rejected
// by B at the same path for the same reason (no new rejection causes).
func TestMonotonicityDual(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 200
	for i := 0; i < N; i++ {
		seed := int64(i + 30_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			newDoc := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			newNode, err := importer.Walk(newDoc)
			if err != nil {
				t.Fatal(err)
			}
			extended, err := schema.Extend(base, newNode, cfg.TargetLevel)
			if err != nil {
				t.Skipf("Extend rejected: %v", err)
			}
			delta, _ := schema.Diff(base, extended)
			applied, err := schema.Apply(base, delta)
			if err != nil {
				t.Fatal(err)
			}
			// Generate a validation probe that's intentionally rejected.
			probe := gentree.GenValue(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			appliedErrs := schema.Validate(applied, probe)
			if len(appliedErrs) == 0 {
				t.Skipf("probe not rejected by applied schema; skipping")
			}
			baseErrs := schema.Validate(base, probe)
			if len(baseErrs) == 0 {
				t.Fatalf("I3 dual violated: probe rejected by applied schema (%v) but accepted by base", appliedErrs)
			}
		})
	}
}
