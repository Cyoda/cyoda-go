package schema_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestRoundTripFlatObject(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	node.SetChild("age", schema.NewLeafNode(schema.Integer))

	data, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	restored, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.Object().Child("name") == nil {
		t.Error("expected 'name' child after round-trip")
	}
	nameTypes := restored.Object().Child("name").DeclaredTypes()
	if len(nameTypes) != 1 || nameTypes[0] != schema.String {
		t.Errorf("expected [STRING], got %v", nameTypes)
	}
	if restored.Object().Child("age") == nil {
		t.Error("expected 'age' child after round-trip")
	}
}

func TestRoundTripNestedWithArray(t *testing.T) {
	elem := schema.NewLeafNode(schema.String)
	arr := schema.NewArrayNode(elem)

	inner := schema.NewObjectNode()
	inner.SetChild("city", schema.NewLeafNode(schema.String))

	root := schema.NewObjectNode()
	root.SetChild("tags", arr)
	root.SetChild("address", inner)

	data, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	restored, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tags := restored.Object().Child("tags")
	if tags == nil || tags.Array() == nil {
		t.Fatal("expected 'tags' as ARRAY")
	}
	if tags.Array().Element() == nil {
		t.Fatal("expected element descriptor for tags")
	}
	addr := restored.Object().Child("address")
	if addr == nil || addr.Object().Child("city") == nil {
		t.Fatal("expected 'address.city'")
	}
}

func TestRoundTripEmptyArrayElementNil(t *testing.T) {
	n := schema.NewArrayNode(nil)
	b, err := schema.Marshal(n)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	decoded, err := schema.Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Array() == nil {
		t.Fatalf("decoded kinds = %v, want the array branch", decoded.Kinds())
	}
	if decoded.Array().Element() != nil {
		t.Fatalf("decoded.Array().Element() = %v, want nil (preserved across round-trip)", decoded.Array().Element())
	}
}

func TestRoundTripPolymorphic(t *testing.T) {
	node := schema.NewObjectNode()
	leaf := schema.NewLeafNode(schema.Integer)
	leaf.AddScalarTypes(schema.String)
	node.SetChild("score", leaf)

	data, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	restored, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	types := restored.Object().Child("score").DeclaredTypes()
	if len(types) != 2 {
		t.Fatalf("expected 2 polymorphic types, got %d", len(types))
	}
}
