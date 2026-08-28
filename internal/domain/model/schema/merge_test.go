package schema_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestMergeDisjointChildren(t *testing.T) {
	a := schema.NewObjectNode()
	a.SetChild("name", schema.NewLeafNode(schema.String))

	b := schema.NewObjectNode()
	b.SetChild("age", schema.NewLeafNode(schema.Integer))

	merged := schema.Merge(a, b)
	if merged.Object().Child("name") == nil {
		t.Error("expected 'name' child after merge")
	}
	if merged.Object().Child("age") == nil {
		t.Error("expected 'age' child after merge")
	}
}

func TestMergeOverlappingChildrenUnionTypes(t *testing.T) {
	a := schema.NewObjectNode()
	a.SetChild("score", schema.NewLeafNode(schema.Integer))

	b := schema.NewObjectNode()
	b.SetChild("score", schema.NewLeafNode(schema.String))

	merged := schema.Merge(a, b)
	score := merged.Object().Child("score")
	if score == nil {
		t.Fatal("expected 'score' child after merge")
	}
	types := score.DeclaredTypes()
	if len(types) != 2 {
		t.Fatalf("expected 2 types (polymorphic), got %d: %v", len(types), types)
	}
}

func TestMergeNestedObjects(t *testing.T) {
	a := schema.NewObjectNode()
	addr := schema.NewObjectNode()
	addr.SetChild("city", schema.NewLeafNode(schema.String))
	a.SetChild("address", addr)

	b := schema.NewObjectNode()
	addr2 := schema.NewObjectNode()
	addr2.SetChild("zip", schema.NewLeafNode(schema.String))
	b.SetChild("address", addr2)

	merged := schema.Merge(a, b)
	mergedAddr := merged.Object().Child("address")
	if mergedAddr == nil {
		t.Fatal("expected 'address' child")
	}
	if mergedAddr.Object().Child("city") == nil {
		t.Error("expected 'city' under address")
	}
	if mergedAddr.Object().Child("zip") == nil {
		t.Error("expected 'zip' under address")
	}
}

func TestMergeArrayElementTypes(t *testing.T) {
	elemA := schema.NewLeafNode(schema.Integer)
	a := schema.NewObjectNode()
	a.SetChild("tags", schema.NewArrayNode(elemA))

	elemB := schema.NewLeafNode(schema.String)
	b := schema.NewObjectNode()
	b.SetChild("tags", schema.NewArrayNode(elemB))

	merged := schema.Merge(a, b)
	tags := merged.Object().Child("tags")
	if tags == nil {
		t.Fatal("expected 'tags' child")
	}
	if tags.Array().Element() == nil {
		t.Fatal("expected array element descriptor")
	}
	types := tags.Array().Element().DeclaredTypes()
	if len(types) != 2 {
		t.Fatalf("expected 2 element types, got %d", len(types))
	}
}

func TestMergeKindConflict(t *testing.T) {
	// Object node with a child.
	obj := schema.NewObjectNode()
	obj.SetChild("x", schema.NewLeafNode(schema.String))

	// Array node with an element.
	arr := schema.NewArrayNode(schema.NewLeafNode(schema.Integer))

	merged := schema.Merge(obj, arr)

	// The merged node declares BOTH kinds. It used to claim one label —
	// OBJECT, by an arbitrary tiebreak — while carrying both payloads, which
	// is what made every reader that dispatched on the label lose a branch.
	if merged.Object() == nil || merged.Array() == nil {
		t.Errorf("object+array merge declares both kinds, got %v", merged.Kinds())
	}
	if !merged.IsPolymorphic() {
		t.Error("a node carrying two branches is polymorphic")
	}
	if merged.Array().Element() == nil {
		t.Error("expected element to be preserved after object+array merge")
	}
	if merged.Object().Child("x") == nil {
		t.Error("expected child 'x' to be preserved after object+array merge")
	}
}

func TestMergeKeepsTheWidestObservedWidth(t *testing.T) {
	a := schema.NewArrayNode(schema.NewLeafNode(schema.Integer))
	a.ObserveArrayWidth(3)

	b := schema.NewArrayNode(schema.NewLeafNode(schema.Integer))
	b.ObserveArrayWidth(5)

	merged := schema.Merge(a, b)
	if merged.Array() == nil {
		t.Fatal("the merged node declares the array branch")
	}
	if got := merged.Array().MaxWidth(); got != 5 {
		t.Errorf("expected maxWidth 5, got %d", got)
	}
}

func TestMergeNilInputs(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("x", schema.NewLeafNode(schema.String))

	if got := schema.Merge(nil, node); got.Object().Child("x") == nil {
		t.Error("Merge(nil, node) should return node's structure")
	}
	if got := schema.Merge(node, nil); got.Object().Child("x") == nil {
		t.Error("Merge(node, nil) should return node's structure")
	}
	if got := schema.Merge(nil, nil); got != nil {
		t.Error("Merge(nil, nil) should return nil")
	}
}
