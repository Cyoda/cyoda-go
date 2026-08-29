package schema_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestNewObjectNode(t *testing.T) {
	node := schema.NewObjectNode()
	if node.Object() == nil {
		t.Errorf("expected the object branch, got %v", node.Kinds())
	}
	if len(node.Object().Children()) != 0 {
		t.Error("new object node should have no children")
	}
}

func TestNewLeafNode(t *testing.T) {
	node := schema.NewLeafNode(schema.String)
	if node.Scalar() == nil {
		t.Errorf("expected the scalar branch, got %v", node.Kinds())
	}
	types := node.DeclaredTypes()
	if len(types) != 1 || types[0] != schema.String {
		t.Errorf("expected [STRING], got %v", types)
	}
}

func TestNewArrayNode(t *testing.T) {
	elem := schema.NewLeafNode(schema.Integer)
	node := schema.NewArrayNode(elem)
	if node.Array() == nil {
		t.Errorf("expected the array branch, got %v", node.Kinds())
	}
	if node.Array().Element() == nil {
		t.Fatal("array node should have an element descriptor")
	}
}

func TestObjectNodeAddChild(t *testing.T) {
	root := schema.NewObjectNode()
	child := schema.NewLeafNode(schema.String)
	root.SetChild("name", child)

	got := root.Object().Child("name")
	if got == nil {
		t.Fatal("expected child 'name'")
	}
	if got.Scalar() == nil {
		t.Errorf("expected the scalar branch, got %v", got.Kinds())
	}
}

func TestArrayBranchTracksWidth(t *testing.T) {
	node := schema.NewArrayNode(schema.NewLeafNode(schema.Integer))
	if node.Array().MaxWidth() != 0 {
		t.Errorf("a fresh array branch has observed no width, got %d", node.Array().MaxWidth())
	}
	node.ObserveArrayWidth(3)
	if got := node.Array().MaxWidth(); got != 3 {
		t.Errorf("expected max width 3, got %d", got)
	}
	node.ObserveArrayWidth(5)
	if got := node.Array().MaxWidth(); got != 5 {
		t.Errorf("expected max width 5, got %d", got)
	}
	node.ObserveArrayWidth(2)
	if got := node.Array().MaxWidth(); got != 5 {
		t.Errorf("the widest observed wins, got %d", got)
	}
}
