package schema

import (
	"testing"
)

func TestFieldsFlatObject(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("name", NewLeafNode(String))
	root.SetChild("age", NewLeafNode(Integer))

	fields := root.Fields()
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}

	m := root.FieldsMap()
	nameF, ok := m["$.name"]
	if !ok {
		t.Fatal("expected field $.name")
	}
	if len(nameF.Types) != 1 || nameF.Types[0] != String {
		t.Errorf("expected [STRING], got %v", nameF.Types)
	}
	if nameF.IsArray {
		t.Error("$.name should not be an array field")
	}

	ageF, ok := m["$.age"]
	if !ok {
		t.Fatal("expected field $.age")
	}
	if len(ageF.Types) != 1 || ageF.Types[0] != Integer {
		t.Errorf("expected [INTEGER], got %v", ageF.Types)
	}
}

func TestFieldsNestedObject(t *testing.T) {
	root := NewObjectNode()
	address := NewObjectNode()
	address.SetChild("city", NewLeafNode(String))
	root.SetChild("address", address)

	fields := root.Fields()
	if len(fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(fields))
	}
	if fields[0].Path != "$.address.city" {
		t.Errorf("expected path $.address.city, got %s", fields[0].Path)
	}
}

func TestFieldsArray(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("tags", NewArrayNode(NewLeafNode(String)))

	m := root.FieldsMap()
	f, ok := m["$.tags[*]"]
	if !ok {
		t.Fatal("expected field $.tags[*]")
	}
	if !f.IsArray {
		t.Error("$.tags[*] should be an array field")
	}
	if len(f.Types) != 1 || f.Types[0] != String {
		t.Errorf("expected [STRING], got %v", f.Types)
	}
}

func TestFieldsArrayOfObjects(t *testing.T) {
	root := NewObjectNode()
	item := NewObjectNode()
	item.SetChild("name", NewLeafNode(String))
	item.SetChild("price", NewLeafNode(Double))
	root.SetChild("items", NewArrayNode(item))

	m := root.FieldsMap()
	if _, ok := m["$.items[*].name"]; !ok {
		t.Error("expected field $.items[*].name")
	}
	if _, ok := m["$.items[*].price"]; !ok {
		t.Error("expected field $.items[*].price")
	}
	for _, f := range root.Fields() {
		if f.IsArray {
			t.Errorf("field %s should not be marked as array (leaf inside array)", f.Path)
		}
	}
}

func TestFieldsCachedOnSecondCall(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("x", NewLeafNode(Integer))

	f1 := root.Fields()
	f2 := root.Fields()

	// Cached: same length and first element should be identical.
	if len(f1) != len(f2) {
		t.Fatalf("expected same length, got %d vs %d", len(f1), len(f2))
	}
	if len(f1) > 0 && f1[0].Path != f2[0].Path {
		t.Error("expected Fields() to return cached result on second call")
	}
}

func TestFieldsPolymorphic(t *testing.T) {
	root := NewObjectNode()
	leaf := NewLeafNode(String)
	leaf.Types().Add(Integer)
	root.SetChild("val", leaf)

	m := root.FieldsMap()
	f := m["$.val"]
	if len(f.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(f.Types))
	}
}

// TestFieldsMixedObjectOrScalar covers a node observed as BOTH an object
// (with substructure) and a bare scalar at the same path — the polymorphic
// object-or-string shape that schema.Merge unions into a KindObject node
// carrying a non-empty scalar TypeSet. collectFields must emit a leaf
// descriptor for the object node's OWN path (carrying the scalar types) in
// ADDITION to recursing into its children, so the mixed field is searchable
// via a scalar operand while its child leaves remain independently searchable.
func TestFieldsMixedObjectOrScalar(t *testing.T) {
	root := NewObjectNode()
	// some-object: observed as an object {some-key: string} AND as the bare
	// string "abc" — Merge yields a KindObject node whose own TypeSet holds
	// STRING.
	mixed := NewObjectNode()
	mixed.SetChild("some-key", NewLeafNode(String))
	mixed.Types().Add(String)
	root.SetChild("some-object", mixed)

	m := root.FieldsMap()

	// The object node's own path is a searchable leaf carrying [STRING].
	own, ok := m["$.some-object"]
	if !ok {
		t.Fatal("expected leaf descriptor for the mixed object path $.some-object")
	}
	if len(own.Types) != 1 || own.Types[0] != String {
		t.Errorf("$.some-object: expected [STRING], got %v", own.Types)
	}

	// The child leaf remains independently searchable.
	child, ok := m["$.some-object.some-key"]
	if !ok {
		t.Fatal("expected child leaf descriptor $.some-object.some-key")
	}
	if len(child.Types) != 1 || child.Types[0] != String {
		t.Errorf("$.some-object.some-key: expected [STRING], got %v", child.Types)
	}
}

// TestFieldsPureObjectNoLeaf confirms a PURE object node (substructure only,
// no scalar observation) emits NO descriptor for its own path — only its
// children. This is the negative counterpart to the mixed case above and the
// precondition that lets search reject a scalar compare on such a container.
func TestFieldsPureObjectNoLeaf(t *testing.T) {
	root := NewObjectNode()
	pure := NewObjectNode()
	pure.SetChild("some-key", NewLeafNode(String))
	root.SetChild("some-object", pure)

	m := root.FieldsMap()
	if _, ok := m["$.some-object"]; ok {
		t.Error("pure object path $.some-object must NOT be a leaf descriptor")
	}
	if _, ok := m["$.some-object.some-key"]; !ok {
		t.Error("expected child leaf $.some-object.some-key")
	}
}

// TestFieldsNullOnlyObjectNoLeaf confirms an object node whose only TypeSet
// member is NULL (the nullable marker, e.g. a null-only leaf widened to an
// object) does NOT emit a self leaf descriptor — NULL is not a concrete scalar
// observation, so the path stays a pure container. This keeps a unique key over
// such a widened path correctly classified as a non-scalar-leaf.
func TestFieldsNullOnlyObjectNoLeaf(t *testing.T) {
	root := NewObjectNode()
	obj := NewObjectNode()
	obj.SetChild("sub", NewLeafNode(String))
	obj.Types().Add(Null) // observed as null too, but no concrete scalar
	root.SetChild("score", obj)

	m := root.FieldsMap()
	if _, ok := m["$.score"]; ok {
		t.Error("null-only object path $.score must NOT emit a scalar leaf descriptor")
	}
	if _, ok := m["$.score.sub"]; !ok {
		t.Error("expected child leaf $.score.sub")
	}
}

func TestFieldsMap(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("a", NewLeafNode(Boolean))
	nested := NewObjectNode()
	nested.SetChild("b", NewLeafNode(Long))
	root.SetChild("n", nested)

	m := root.FieldsMap()
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if _, ok := m["$.a"]; !ok {
		t.Error("missing $.a")
	}
	if _, ok := m["$.n.b"]; !ok {
		t.Error("missing $.n.b")
	}
}

func TestFieldsArrayMaxWidth(t *testing.T) {
	root := NewObjectNode()
	arrNode := NewArrayNode(NewLeafNode(Integer))
	arrNode.Info().Observe(5)
	root.SetChild("nums", arrNode)

	m := root.FieldsMap()
	f := m["$.nums[*]"]
	if f.MaxWidth != 5 {
		t.Errorf("expected MaxWidth 5, got %d", f.MaxWidth)
	}
}
