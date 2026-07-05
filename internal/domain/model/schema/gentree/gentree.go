// Package gentree produces random ModelNode trees and JSON-like values
// for property-based testing of the schema transformation pipeline.
// Determinism: use only ordered data structures when emitting tree
// shape. Never `range` over maps in generator paths — see
// TestGeneratorIsMapFree.
package gentree

import (
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// GenConfig holds all configurable knobs for the random tree generator.
type GenConfig struct {
	Seed        int64
	MaxDepth    int
	MaxWidth    int
	KindWeights KindWeights
	// PrimitiveWeights maps each DataType to a relative weight; the generator
	// normalises them — values need not sum to 1.0.
	PrimitiveWeights map[schema.DataType]float64
	AllowNulls       bool
	TargetLevel      spi.ChangeLevel
}

// KindWeights controls the relative probability of generating a leaf,
// object, or array node at each position in the tree.
// KindWeights are relative; the generator normalises them — they need not sum to 1.0.
type KindWeights struct {
	Leaf, Object, Array float64
}

// DefaultConfig returns a GenConfig with sensible defaults for
// property-based tests.
func DefaultConfig() GenConfig {
	return GenConfig{
		Seed:        1,
		MaxDepth:    5,
		MaxWidth:    6,
		KindWeights: KindWeights{Leaf: 0.5, Object: 0.3, Array: 0.2},
		PrimitiveWeights: map[schema.DataType]float64{
			schema.Integer:        5,
			schema.Long:           3,
			schema.BigInteger:     1,
			schema.UnboundInteger: 1,
			schema.Double:         3,
			schema.BigDecimal:     2,
			schema.UnboundDecimal: 1,
			schema.String:         5,
			schema.Boolean:        2,
			schema.Null:           1,
		},
		AllowNulls:  true,
		TargetLevel: spi.ChangeLevelStructural,
	}
}

// NewRNG returns a PCG-seeded *rand.Rand; same seed produces same
// sequence across Go versions.
func NewRNG(seed int64) *rand.Rand {
	// Split int64 into two uint64s for PCG's two-word seed.
	s1 := uint64(seed)
	s2 := uint64(seed) ^ 0x9E3779B97F4A7C15
	return rand.New(rand.NewPCG(s1, s2))
}

// GenValue produces a random JSON-ish value at bounded depth suitable
// for feeding into importer.Walk. At depth=0, leaves only.
func GenValue(r *rand.Rand, depth, maxWidth int, cfg GenConfig) any {
	if depth <= 0 {
		return genLeafValue(r, cfg)
	}
	w := cfg.KindWeights
	total := w.Leaf + w.Object + w.Array
	roll := r.Float64() * total
	switch {
	case roll < w.Leaf:
		return genLeafValue(r, cfg)
	case roll < w.Leaf+w.Object:
		return genObjectValue(r, depth-1, maxWidth, cfg)
	default:
		return genArrayValue(r, depth-1, maxWidth, cfg)
	}
}

func genLeafValue(r *rand.Rand, cfg GenConfig) any {
	dt := pickWeightedType(r, cfg.PrimitiveWeights)
	switch dt {
	case schema.Integer:
		return json.Number(strconv.FormatInt(int64(r.IntN(1<<20)-(1<<19)), 10))
	case schema.Long:
		return json.Number(strconv.FormatInt(int64(r.Uint64()>>1), 10))
	case schema.BigInteger:
		return json.Number("170141183460469231731687303715884105727") // 2^127-1
	case schema.UnboundInteger:
		return json.Number("12345678901234567890123456789012345678901234567890")
	case schema.Double:
		return json.Number("3.14159265358979")
	case schema.BigDecimal:
		return json.Number("1.234567890123456789") // 18 digit
	case schema.UnboundDecimal:
		return json.Number("1.23456789012345678901") // 20 digit
	case schema.String:
		return randString(r, 1+r.IntN(8))
	case schema.Boolean:
		return r.IntN(2) == 1
	case schema.Null:
		if cfg.AllowNulls {
			return nil
		}
		return "nullsub"
	}
	return nil
}

func genObjectValue(r *rand.Rand, depth, maxWidth int, cfg GenConfig) any {
	// Use a sorted key slice — NEVER range over the map while emitting.
	n := 1 + r.IntN(maxWidth)
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "k" + strconv.Itoa(i)
	}
	// Iterate keys in slice order, not map order.
	out := make(map[string]any, n)
	for _, k := range keys {
		out[k] = GenValue(r, depth, maxWidth, cfg)
	}
	return out
}

func genArrayValue(r *rand.Rand, depth, maxWidth int, cfg GenConfig) any {
	n := r.IntN(maxWidth + 1) // allow empty
	out := make([]any, n)
	for i := 0; i < n; i++ {
		out[i] = GenValue(r, depth, maxWidth, cfg)
	}
	return out
}

// pickWeightedType chooses a DataType by iterating a SORTED key slice
// (deterministic). Never range over the map directly.
func pickWeightedType(r *rand.Rand, w map[schema.DataType]float64) schema.DataType {
	keys := make([]schema.DataType, 0, len(w))
	for k := range w { // one-time build is fine; we immediately sort
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var total float64
	for _, k := range keys {
		total += w[k]
	}
	roll := r.Float64() * total
	var acc float64
	for _, k := range keys {
		acc += w[k]
		if roll < acc {
			return k
		}
	}
	return keys[len(keys)-1]
}

// GenModelNode returns a random *schema.ModelNode at bounded depth.
// Determinism discipline: sorted keys only; never range maps.
func GenModelNode(r *rand.Rand, depth, maxWidth int, cfg GenConfig) *schema.ModelNode {
	if depth <= 0 {
		return schema.NewLeafNode(pickWeightedType(r, cfg.PrimitiveWeights))
	}
	w := cfg.KindWeights
	total := w.Leaf + w.Object + w.Array
	roll := r.Float64() * total
	switch {
	case roll < w.Leaf:
		return schema.NewLeafNode(pickWeightedType(r, cfg.PrimitiveWeights))
	case roll < w.Leaf+w.Object:
		n := schema.NewObjectNode()
		count := 1 + r.IntN(maxWidth)
		for i := 0; i < count; i++ {
			key := "f" + strconv.Itoa(i)
			n.SetChild(key, GenModelNode(r, depth-1, maxWidth, cfg))
		}
		return n
	default:
		return schema.NewArrayNode(GenModelNode(r, depth-1, maxWidth, cfg))
	}
}

func randString(r *rand.Rand, n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.IntN(len(alpha))]
	}
	return string(b)
}

// GenExtensionPair given an existing schema returns a random JSON-like
// value whose Walk output, when fed to Extend(old, ., level), is
// typically accepted at Structural level. Strategy: emit a mutated
// view of the schema (same shape, random additional fields) so the
// extension is additive rather than kind-changing.
func GenExtensionPair(r *rand.Rand, old *schema.ModelNode, level spi.ChangeLevel, cfg GenConfig) any {
	return mutateToValue(r, old, 0, cfg)
}

func mutateToValue(r *rand.Rand, n *schema.ModelNode, depth int, cfg GenConfig) any {
	if n == nil || depth > cfg.MaxDepth {
		return genLeafValue(r, cfg)
	}
	switch n.Kind() {
	case schema.KindLeaf:
		// Emit a value compatible with the widest type in the set, plus
		// occasionally broaden to trigger a broaden_type op.
		return genLeafValue(r, cfg)
	case schema.KindObject:
		out := make(map[string]any)
		for _, name := range sortedChildNames(n) {
			out[name] = mutateToValue(r, n.Child(name), depth+1, cfg)
		}
		// ~30% of the time add a new field to drive AddProperty.
		if r.Float64() < 0.3 {
			out["extra_"+strconv.Itoa(r.IntN(1000))] = genLeafValue(r, cfg)
		}
		return out
	case schema.KindArray:
		m := r.IntN(cfg.MaxWidth + 1)
		out := make([]any, m)
		for i := 0; i < m; i++ {
			out[i] = mutateToValue(r, n.Element(), depth+1, cfg)
		}
		return out
	}
	return genLeafValue(r, cfg)
}

func sortedChildNames(n *schema.ModelNode) []string {
	children := n.Children() // returns map[string]*ModelNode (shallow copy)
	names := make([]string, 0, len(children))
	for k := range children {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
