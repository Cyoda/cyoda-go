package gentree_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// The generator emitted a value of the SAME kind as the node it was mutating
// at every position, which is why the roundtrip suite can treat any Extend
// error at STRUCTURAL as a failure. The cost is that commutativity,
// permutation, monotonicity and roundtrip never produced a kind conflict, so
// they said nothing at all about the op that records one.
func TestGenExtensionPair_ProducesKindConflicts(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.KindMutationRate = 0.5

	seen := false
	for seed := int64(1); seed <= 300 && !seen; seed++ {
		r := gentree.NewRNG(seed)
		old := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
		v := gentree.GenExtensionPair(r, old, spi.ChangeLevelStructural, cfg)

		node, err := importer.Walk(v)
		if err != nil {
			continue
		}
		extended, err := schema.Extend(old, node, spi.ChangeLevelStructural)
		if err != nil {
			t.Fatalf("seed %d: STRUCTURAL refuses nothing a walk can produce; got: %v", seed, err)
		}
		delta, err := schema.Diff(old, extended)
		if err != nil {
			t.Fatalf("seed %d: Diff: %v", seed, err)
		}
		if delta == nil {
			continue
		}
		ops, err := schema.UnmarshalDelta(delta)
		if err != nil {
			t.Fatalf("seed %d: UnmarshalDelta: %v", seed, err)
		}
		for _, op := range ops {
			if op.Kind == schema.KindAddKindBranch {
				seen = true
			}
		}
	}
	if !seen {
		t.Error("300 seeds at KindMutationRate 0.5 produced no add_kind_branch: the property suites cannot see the new op")
	}
}

// The default stays zero, so every existing caller generates exactly the
// values it generated before.
func TestDefaultConfig_DoesNotMutateKinds(t *testing.T) {
	if got := gentree.DefaultConfig().KindMutationRate; got != 0 {
		t.Errorf("KindMutationRate default = %v, want 0", got)
	}
}
