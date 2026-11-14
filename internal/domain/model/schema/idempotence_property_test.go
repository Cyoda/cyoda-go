// internal/domain/model/schema/idempotence_property_test.go
package schema_test

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestIdempotenceApply — I4: applyDelta(applyDelta(b, d), d) == applyDelta(b, d).
func TestIdempotenceApply(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 500
	var ran, skipped int
	for i := 0; i < N; i++ {
		seed := int64(i + 40_000)
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
			incoming := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			extended, err := schema.Extend(base, incoming, cfg.TargetLevel)
			if err != nil {
				t.Skip(err)
			}
			delta, _ := schema.Diff(base, extended)
			once, err := applyDelta(base, delta)
			if err != nil {
				t.Fatal(err)
			}
			twice, err := applyDelta(once, delta)
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
	assertSkipRatio(t, ran, skipped, "TestIdempotenceApply")
}

// TestIdempotenceIngest — ingesting the same data twice yields the same
// schema (extension is idempotent, not double-widening). Under the
// value-based Admit rule this is stronger than the old label-based algebra
// made it: the first Extend does not merely record a compatible label, it
// makes the document's actual value HELD, so the second Extend against the
// same document finds nothing left to admit — idempotence by construction,
// not by the two calls happening to agree on a label.
func TestIdempotenceIngest(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 300
	var ran, skipped int
	for i := 0; i < N; i++ {
		seed := int64(i + 50_000)
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
			data := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			e1, err := schema.Extend(base, data, cfg.TargetLevel)
			if err != nil {
				t.Skip(err)
			}
			e2, err := schema.Extend(e1, data, cfg.TargetLevel)
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
	assertSkipRatio(t, ran, skipped, "TestIdempotenceIngest")
}
