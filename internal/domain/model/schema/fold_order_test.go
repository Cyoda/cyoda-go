package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Two deltas diffed from the SAME model version, folded in both orders.
//
// This is the ordinary state during a cross-node gossip window and under
// concurrent writes against a cached descriptor: each writer diffs against the
// version it holds, and the extension log folds whatever arrives in whatever
// order it arrives. For the two add_kind_branch shapes named below, every
// pair of legal deltas applies in either order and reaches the same model —
// this is a statement about APPLICATION, not about which delta a given
// sequence of writes PRODUCES (§6 accepts order-dependence there: two writers
// racing to extend the same field can each win a different fold, which is a
// property of Extend/Diff, not of ApplyMany). The property suites cover
// application order-independence over random trees; these two cases are here
// by name because they are the shapes add_kind_branch introduced, and each
// was refused in exactly one of the two orders before add_kind_branch existed.
func TestApply_FoldOrderDoesNotDecideWhetherADeltaApplies(t *testing.T) {
	wrap := func(child *ModelNode) *ModelNode {
		r := NewObjectNode()
		r.SetChild("f", child)
		return r
	}

	cases := []struct {
		name       string
		base       func() *ModelNode
		one, other any // the "f" document value each writer observed
	}{
		{
			// A path observed only as null declares no kind at all, so both
			// writers are establishing its first one — one an object, the other
			// a string.
			name:  "branchless marker gains an object branch and a scalar",
			base:  func() *ModelNode { return wrap(NewLeafNode(Null)) },
			one:   map[string]any{"k": "x"},
			other: "x",
		},
		{
			// An array observed with no content declares no element type, so
			// both writers are establishing that.
			name:  "unobserved array element gains object elements and scalar elements",
			base:  func() *ModelNode { return wrap(NewArrayNode(nil)) },
			one:   []any{map[string]any{"k": "x"}},
			other: []any{"x"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			delta := func(fValue any) spi.SchemaDelta {
				t.Helper()
				doc := map[string]any{"f": fValue}
				extended, err := Extend(c.base(), doc, spi.ChangeLevelStructural)
				if err != nil {
					t.Fatalf("Extend: %v", err)
				}
				d, err := Diff(c.base(), extended)
				if err != nil {
					t.Fatalf("Diff: %v", err)
				}
				if d == nil {
					t.Fatal("precondition: this is a change, so the delta is not empty")
				}
				return d
			}
			d1, d2 := delta(c.one), delta(c.other)

			fold := func(first, second spi.SchemaDelta) *ModelNode {
				t.Helper()
				out, err := applyDelta(c.base(), first)
				if err != nil {
					t.Fatalf("apply first: %v", err)
				}
				out, err = applyDelta(out, second)
				if err != nil {
					t.Fatalf("apply second: %v", err)
				}
				return out
			}

			forward, _ := Marshal(fold(d1, d2))
			reverse, _ := Marshal(fold(d2, d1))
			if string(forward) != string(reverse) {
				t.Errorf("fold order changed the model\n  d1,d2 = %s\n  d2,d1 = %s", forward, reverse)
			}
		})
	}
}
