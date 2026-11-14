// internal/domain/model/schema/commutativity_property_test.go
package schema_test

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestCommutativityPaired — I2: applyDelta(applyDelta(b,d1),d2) ≡ applyDelta(applyDelta(b,d2),d1)
// for deltas produced from a shared base by two independent generator draws.
func TestCommutativityPaired(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 500
	var ran, skipped int
	for i := 0; i < N; i++ {
		seed := int64(i + 10_000)
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
			incomingA := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
			incomingB := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)

			extA, err := schema.Extend(base, incomingA, cfg.TargetLevel)
			if err != nil {
				t.Skipf("Extend A rejected, skipping seed: %v", err)
			}
			extB, err := schema.Extend(base, incomingB, cfg.TargetLevel)
			if err != nil {
				t.Skipf("Extend B rejected, skipping seed: %v", err)
			}
			dA, _ := schema.Diff(base, extA)
			dB, _ := schema.Diff(base, extB)

			// Apply the deltas in both orders.
			ab, err := applyDelta(base, dA)
			if err != nil {
				t.Fatal(err)
			}
			ab, err = applyDelta(ab, dB)
			if err != nil {
				t.Fatal(err)
			}
			ba, err := applyDelta(base, dB)
			if err != nil {
				t.Fatal(err)
			}
			ba, err = applyDelta(ba, dA)
			if err != nil {
				t.Fatal(err)
			}
			b1, _ := schema.Marshal(ab)
			b2, _ := schema.Marshal(ba)
			if string(b1) != string(b2) {
				t.Fatalf("I2 violated\n  base=%s\n  ab  =%s\n  ba  =%s", mustMarshal(t, base), b1, b2)
			}
		})
	}
	assertSkipRatio(t, ran, skipped, "TestCommutativityPaired")
}

func mustMarshal(t *testing.T, n *schema.ModelNode) string {
	t.Helper()
	b, err := schema.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
