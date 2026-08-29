// internal/domain/model/schema/permutation_property_test.go
package schema_test

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// TestPermutationInvariance — I5: every permutation of 3 deltas applied
// to a shared base yields a Marshal-equal result.
func TestPermutationInvariance(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	// Let the generator propose a kind the node does not declare, so this
	// property covers add_kind_branch and not only the three ops that
	// existed when it was written.
	cfg.KindMutationRate = 0.3
	const N = 200
	for i := 0; i < N; i++ {
		seed := int64(i + 60_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			deltas := make([]spi.SchemaDelta, 0, 3)
			for k := 0; k < 3; k++ {
				d := gentree.GenExtensionPair(r, base, cfg.TargetLevel, cfg)
				node, err := importer.Walk(d)
				if err != nil {
					t.Fatal(err)
				}
				ext, err := schema.Extend(base, node, cfg.TargetLevel)
				if err != nil {
					t.Skip(err)
				}
				delta, _ := schema.Diff(base, ext)
				deltas = append(deltas, delta)
			}
			perms := [][]int{
				{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
			}
			var canon string
			for idx, p := range perms {
				cur := base
				for _, j := range p {
					next, err := schema.Apply(cur, deltas[j])
					if err != nil {
						t.Fatal(err)
					}
					cur = next
				}
				b, _ := schema.Marshal(cur)
				if idx == 0 {
					canon = string(b)
					continue
				}
				if string(b) != canon {
					t.Fatalf("I5 violated\n  perm %v: %s\n  canon: %s", p, b, canon)
				}
			}
		})
	}
}
