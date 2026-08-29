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
// order it arrives. So every pair of legal deltas must apply in either order
// and reach the same model. The property suites cover this over random trees;
// these two cases are here by name because they are the shapes add_kind_branch
// introduced, and each was refused in exactly one of the two orders.
func TestApply_FoldOrderDoesNotDecideWhetherADeltaApplies(t *testing.T) {
	wrap := func(child *ModelNode) *ModelNode {
		r := NewObjectNode()
		r.SetChild("f", child)
		return r
	}
	obj := func() *ModelNode {
		o := NewObjectNode()
		o.SetChild("k", NewLeafNode(String))
		return o
	}

	cases := []struct {
		name             string
		base, one, other func() *ModelNode
	}{
		{
			// A path observed only as null declares no kind at all, so both
			// writers are establishing its first one — one an object, the other
			// a string.
			name:  "branchless marker gains an object branch and a scalar",
			base:  func() *ModelNode { return wrap(NewLeafNode(Null)) },
			one:   func() *ModelNode { return wrap(obj()) },
			other: func() *ModelNode { return wrap(NewLeafNode(String)) },
		},
		{
			// An array observed with no content declares no element type, so
			// both writers are establishing that.
			name:  "unobserved array element gains object elements and scalar elements",
			base:  func() *ModelNode { return wrap(NewArrayNode(nil)) },
			one:   func() *ModelNode { return wrap(NewArrayNode(obj())) },
			other: func() *ModelNode { return wrap(NewArrayNode(NewLeafNode(String))) },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			delta := func(incoming *ModelNode) spi.SchemaDelta {
				t.Helper()
				extended, err := Extend(c.base(), incoming, spi.ChangeLevelStructural)
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
			d1, d2 := delta(c.one()), delta(c.other())

			fold := func(first, second spi.SchemaDelta) *ModelNode {
				t.Helper()
				out, err := Apply(c.base(), first)
				if err != nil {
					t.Fatalf("Apply first: %v", err)
				}
				out, err = Apply(out, second)
				if err != nil {
					t.Fatalf("Apply second: %v", err)
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
