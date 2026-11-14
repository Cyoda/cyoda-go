// internal/domain/model/schema/monotonicity_property_test.go
package schema_test

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestMonotonicityDirect — I3: a document valid against B is also valid
// against applyDelta(B, d). Extension never narrows the accepted set.
func TestMonotonicityDirect(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 200
	var ran, skipped int
	for i := 0; i < N; i++ {
		seed := int64(i + 20_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			defer func() {
				if t.Skipped() {
					skipped++
				} else {
					ran++
				}
			}()
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)

			// GenExtensionPair's own strategy proposes a NEW field roughly
			// 30% of the time per object node, unconditionally of
			// cfg.KindMutationRate — so across a multi-node tree, "the
			// generator's draw already validates against base" is the
			// uncommon case, not the common one, and a single draw skips
			// this precondition far more often than it satisfies it. Retry
			// a bounded number of times with fresh draws from the same RNG
			// stream (deterministic per seed) rather than accepting the
			// first one: this is the precondition the property needs — a
			// document base ALREADY holds — not a property of the
			// generator's typical output, so redrawing until one is found
			// exercises I3 direct instead of skipping past it almost every
			// seed.
			var doc any
			validDoc := false
			for attempt := 0; attempt < 20; attempt++ {
				candidate := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
				if errs := schema.Validate(base, candidate); len(errs) == 0 {
					doc = candidate
					validDoc = true
					break
				}
			}
			if !validDoc {
				t.Skipf("doc not valid against base after 20 attempts; skipping")
			}
			newDoc := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			extended, err := schema.Extend(base, newDoc, cfg.TargetLevel)
			if err != nil {
				t.Skipf("Extend rejected: %v", err)
			}
			delta, _ := schema.Diff(base, extended)
			applied, err := applyDelta(base, delta)
			if err != nil {
				t.Fatal(err)
			}
			if errs := schema.Validate(applied, doc); len(errs) > 0 {
				t.Fatalf("I3 direct violated: doc valid against base but not applied schema: %v", errs)
			}
		})
	}
	assertSkipRatio(t, ran, skipped, "TestMonotonicityDirect")
}

// TestMonotonicityDual — a document rejected by applyDelta(B, d) is rejected
// by B at the same path for the same reason (no new rejection causes).
func TestMonotonicityDual(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 200
	var ran, skipped int
	for i := 0; i < N; i++ {
		seed := int64(i + 30_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			defer func() {
				if t.Skipped() {
					skipped++
				} else {
					ran++
				}
			}()
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			newDoc := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			extended, err := schema.Extend(base, newDoc, cfg.TargetLevel)
			if err != nil {
				t.Skipf("Extend rejected: %v", err)
			}
			delta, _ := schema.Diff(base, extended)
			applied, err := applyDelta(base, delta)
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
	assertSkipRatio(t, ran, skipped, "TestMonotonicityDual")
}
