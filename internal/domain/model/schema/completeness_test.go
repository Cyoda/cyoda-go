package schema

import (
	"reflect"
	"testing"
)

// catalogCoverage — one entry per SchemaOpKind.
// A new SchemaOpKind constant without an entry here will fail
// TestDiffCoversCatalog below, which is the point of this gate.
var catalogCoverage = map[SchemaOpKind]struct {
	old func() *ModelNode
	new func() *ModelNode
}{
	KindAddProperty: {
		old: func() *ModelNode { return NewObjectNode() },
		new: func() *ModelNode {
			n := NewObjectNode()
			n.SetChild("added", NewLeafNode(String))
			return n
		},
	},
	KindBroadenType: {
		old: func() *ModelNode {
			n := NewObjectNode()
			n.SetChild("x", NewLeafNode(String))
			return n
		},
		new: func() *ModelNode {
			n := NewObjectNode()
			leaf := NewLeafNode(String)
			// Add a concrete non-numeric type so the TypeSet actually broadens;
			// NULL alone would drop under the collapse rule.
			leaf.AddScalarTypes(Integer)
			n.SetChild("x", leaf)
			return n
		},
	},
	KindAddArrayItemType: {
		old: func() *ModelNode {
			n := NewObjectNode()
			n.SetChild("tags", NewArrayNode(NewLeafNode(Integer)))
			return n
		},
		new: func() *ModelNode {
			n := NewObjectNode()
			elem := NewLeafNode(Integer)
			elem.AddScalarTypes(String)
			n.SetChild("tags", NewArrayNode(elem))
			return n
		},
	},
}

// TestDiffCoversCatalog asserts that every declared SchemaOpKind has
// a coverage entry AND that Diff emits that kind from its
// (old, new) sample. Fails if a new kind is added without a matching
// registry entry.
func TestDiffCoversCatalog(t *testing.T) {
	known := declaredKinds()
	if len(known) != len(catalogCoverage) {
		missing := []SchemaOpKind{}
		for _, k := range known {
			if _, ok := catalogCoverage[k]; !ok {
				missing = append(missing, k)
			}
		}
		t.Fatalf("catalog has %d kinds, coverage has %d. Missing registry entries for: %v",
			len(known), len(catalogCoverage), missing)
	}
	for _, kind := range known {
		t.Run(string(kind), func(t *testing.T) {
			sample := catalogCoverage[kind]
			delta, err := Diff(sample.old(), sample.new())
			if err != nil {
				t.Fatalf("Diff(%s): %v", kind, err)
			}
			ops, err := UnmarshalDelta(delta)
			if err != nil {
				t.Fatalf("UnmarshalDelta: %v", err)
			}
			found := false
			for _, op := range ops {
				if op.Kind == kind {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected Diff to emit kind %s from coverage sample, got ops: %+v", kind, ops)
			}
			// And the round trip holds (redundant with diff_test but
			// explicit here as the completeness invariant).
			applied, err := Apply(sample.old(), delta)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			wantBytes, _ := Marshal(sample.new())
			gotBytes, _ := Marshal(applied)
			if !reflect.DeepEqual(string(wantBytes), string(gotBytes)) {
				t.Errorf("round-trip mismatch for %s:\nwant: %s\ngot:  %s", kind, wantBytes, gotBytes)
			}
		})
	}
}

// declaredKinds returns the authoritative set of catalog kinds.
// Update this function when adding a new SchemaOpKind constant.
// The TestDiffCoversCatalog gate then fails if catalogCoverage above
// is not updated to match.
func declaredKinds() []SchemaOpKind {
	return []SchemaOpKind{
		KindAddProperty,
		KindBroadenType,
		KindAddArrayItemType,
	}
}
