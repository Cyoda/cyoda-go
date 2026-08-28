package schema_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// mergeKind had to pick one label when two kinds met, and its OBJECT-wins
// tiebreak destroyed information the payload kept: an object-and-array union
// marshalled to {"kind":"OBJECT"} and lost the array branch outright.
func TestMerge_KeepsAnEmptyArrayBranchOfAUnion(t *testing.T) {
	n := schema.Merge(schema.NewObjectNode(), schema.NewArrayNode(nil))
	if n.Array() == nil {
		t.Fatalf("the array branch survives the union; kinds=%v", n.Kinds())
	}

	raw, err := schema.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	back, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Array() == nil {
		t.Errorf("the array branch did not survive persistence: %s", raw)
	}
}

// Set union is commutative by construction; mergeKind's precedence achieved
// that only by accident.
func TestMerge_BranchUnionIsCommutative(t *testing.T) {
	kinds := func(n *schema.ModelNode) string {
		s := ""
		for _, k := range n.Kinds() {
			s += k.String() + ","
		}
		return s
	}
	cases := []struct {
		name string
		a, b func() *schema.ModelNode
	}{
		{"scalar+array",
			func() *schema.ModelNode { return schema.NewLeafNode(schema.String) },
			func() *schema.ModelNode { return schema.NewArrayNode(schema.NewLeafNode(schema.String)) }},
		{"scalar+object",
			func() *schema.ModelNode { return schema.NewLeafNode(schema.String) },
			func() *schema.ModelNode { return schema.NewObjectNode() }},
		{"object+array",
			func() *schema.ModelNode { return schema.NewObjectNode() },
			func() *schema.ModelNode { return schema.NewArrayNode(schema.NewLeafNode(schema.Integer)) }},
		{"null+array",
			func() *schema.ModelNode { return schema.NewLeafNode(schema.Null) },
			func() *schema.ModelNode { return schema.NewArrayNode(schema.NewLeafNode(schema.String)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ab, ba := kinds(schema.Merge(c.a(), c.b())), kinds(schema.Merge(c.b(), c.a())); ab != ba {
				t.Errorf("not commutative: a+b=%s b+a=%s", ab, ba)
			}
		})
	}
}

// Merge built its result from NewObjectNode(), so every merged node carried an
// empty children map whatever kind it claimed to be.
func TestMerge_TwoLeavesDeclareOnlyTheScalarBranch(t *testing.T) {
	n := schema.Merge(schema.NewLeafNode(schema.String), schema.NewLeafNode(schema.Integer))
	if n.Object() != nil || n.Array() != nil {
		t.Errorf("leaf+leaf declares only the scalar branch; kinds=%v", n.Kinds())
	}
	if n.Scalar() == nil {
		t.Fatal("leaf+leaf declares the scalar branch")
	}
}

// The nullable marker merged with a concrete kind yields that kind, still
// nullable — and merged with a scalar yields the scalar, with the marker
// collapsed exactly as TypeSet.Add collapses it.
func TestMerge_NullableMarker(t *testing.T) {
	arr := schema.Merge(schema.NewLeafNode(schema.Null), schema.NewArrayNode(schema.NewLeafNode(schema.String)))
	if arr.Array() == nil || arr.Scalar() != nil || !arr.Nullable() {
		t.Errorf("null+array is a nullable array; kinds=%v nullable=%v", arr.Kinds(), arr.Nullable())
	}

	str := schema.Merge(schema.NewLeafNode(schema.Null), schema.NewLeafNode(schema.String))
	if str.Scalar() == nil || str.Nullable() {
		t.Errorf("null+string is a string; kinds=%v nullable=%v", str.Kinds(), str.Nullable())
	}
	if got := str.DeclaredTypes(); len(got) != 1 || got[0] != schema.String {
		t.Errorf("DeclaredTypes() = %v, want [STRING]", got)
	}
}
