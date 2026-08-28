package gentree

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestDefaultConfigSane(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxDepth < 3 {
		t.Fatalf("DefaultConfig MaxDepth=%d, want >=3", cfg.MaxDepth)
	}
	if cfg.MaxWidth < 3 {
		t.Fatalf("DefaultConfig MaxWidth=%d, want >=3", cfg.MaxWidth)
	}
	if cfg.Seed == 0 {
		t.Fatalf("DefaultConfig.Seed=0, want non-zero default")
	}
	if cfg.TargetLevel == "" {
		t.Fatalf("DefaultConfig.TargetLevel is empty; want a valid level (got %q, want e.g. %q)", cfg.TargetLevel, spi.ChangeLevelStructural)
	}
}

func TestNewRNGSameSeedSameSequence(t *testing.T) {
	r1 := NewRNG(42)
	r2 := NewRNG(42)
	for i := 0; i < 16; i++ {
		a := r1.Uint64()
		b := r2.Uint64()
		if a != b {
			t.Fatalf("seed 42, step %d: %d != %d", i, a, b)
		}
	}
}

func TestGenValueProducesWalkableOutput(t *testing.T) {
	cfg := DefaultConfig()
	r := NewRNG(7)
	for i := 0; i < 50; i++ {
		v := GenValue(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
		// GenValue output must be accepted by importer.Walk.
		if _, err := importer.Walk(v); err != nil {
			t.Fatalf("sample %d: Walk failed: %v (value: %#v)", i, err, v)
		}
	}
}

func TestGenValueSameSeedSameOutput(t *testing.T) {
	cfg := DefaultConfig()
	v1 := GenValue(NewRNG(99), cfg.MaxDepth, cfg.MaxWidth, cfg)
	v2 := GenValue(NewRNG(99), cfg.MaxDepth, cfg.MaxWidth, cfg)
	b1, _ := json.Marshal(v1)
	b2, _ := json.Marshal(v2)
	if string(b1) != string(b2) {
		t.Fatalf("seed 99 produced divergent output:\n  v1=%s\n  v2=%s", b1, b2)
	}
}

func TestGenModelNodeDeterministicMarshal(t *testing.T) {
	cfg := DefaultConfig()
	n1 := GenModelNode(NewRNG(11), cfg.MaxDepth, cfg.MaxWidth, cfg)
	n2 := GenModelNode(NewRNG(11), cfg.MaxDepth, cfg.MaxWidth, cfg)
	b1, err := schema.Marshal(n1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := schema.Marshal(n2)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("seed 11 produced divergent ModelNode marshal:\n  n1=%s\n  n2=%s", b1, b2)
	}
}

func TestGenExtensionPairProducesExtendableIncoming(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	r := NewRNG(23)
	for i := 0; i < 30; i++ {
		old := GenModelNode(r, 3, 4, cfg)
		incoming := GenExtensionPair(r, old, cfg.TargetLevel, cfg)
		incomingNode, err := importer.Walk(incoming)
		if err != nil {
			t.Fatalf("sample %d: Walk incoming failed: %v", i, err)
		}
		if _, err := schema.Extend(old, incomingNode, cfg.TargetLevel); err != nil {
			// Extend may reject when GenExtensionPair randomly produces
			// incompatible shapes at lower levels; at Structural, everything
			// additive must succeed.
			t.Fatalf("sample %d: Extend at Structural rejected: %v", i, err)
		}
	}
}

// TestGeneratorIsMapFree runs GenModelNode with the same seed twice
// and asserts byte-identical ModelNode.Marshal output. A generator
// that accidentally ranges over a map fails this with high probability.
func TestGeneratorIsMapFree(t *testing.T) {
	cfg := DefaultConfig()
	for _, seed := range []int64{1, 2, 3, 100, 1000, 54321} {
		n1 := GenModelNode(NewRNG(seed), cfg.MaxDepth, cfg.MaxWidth, cfg)
		n2 := GenModelNode(NewRNG(seed), cfg.MaxDepth, cfg.MaxWidth, cfg)
		b1, _ := schema.Marshal(n1)
		b2, _ := schema.Marshal(n2)
		if string(b1) != string(b2) {
			t.Fatalf("seed %d: divergent output — generator is not map-free", seed)
		}
	}
}

// TestCoverageDistribution samples 10_000 GenModelNode outputs and
// asserts each major shape class is produced at minimum frequency.
func TestCoverageDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("coverage distribution is a slow sanity check, skipped under -short")
	}
	cfg := DefaultConfig()
	r := NewRNG(777)
	const N = 10_000
	var leaves, objects, arrays int
	for i := 0; i < N; i++ {
		n := GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
		switch {
		case n.Object() != nil:
			objects++
		case n.Array() != nil:
			arrays++
		default:
			leaves++
		}
	}
	// Each class must be at least 1 in 50 (= 200 in 10k).
	for name, count := range map[string]int{"leaf": leaves, "object": objects, "array": arrays} {
		if count < N/50 {
			t.Errorf("%s frequency %d < threshold %d", name, count, N/50)
		}
	}
}
