package schema

import "testing"

// The scalar branch of an array-or-scalar union is searchable on the field's
// own path, exactly as it is for an object-or-scalar union (see
// TestFieldsMixedObjectOrScalar). Both are the same shape — a container node
// carrying concrete scalar types — and both really do accept a bare scalar, so
// a predicate on the field must find a declared type to compare against.
func TestFieldsMixedArrayOrScalar(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("poly", Merge(NewLeafNode(String), NewArrayNode(NewLeafNode(String))))

	m := root.FieldsMap()

	own, ok := m["$.poly"]
	if !ok {
		t.Fatalf("expected leaf descriptor for the scalar branch $.poly; got %v", m)
	}
	if len(own.Types) != 1 || own.Types[0] != String {
		t.Errorf("$.poly: types = %v, want [STRING]", own.Types)
	}
	if own.IsArray {
		t.Error("$.poly: the scalar branch is not an array field")
	}

	elem, ok := m["$.poly[*]"]
	if !ok {
		t.Fatalf("expected element descriptor $.poly[*]; got %v", m)
	}
	if !elem.IsArray {
		t.Error("$.poly[*]: want IsArray")
	}
}

// A pure array grows no scalar branch, and a merely-nullable one grows none
// either: NULL is the absence of a value, not a scalar observation.
func TestFieldsPureArrayNoScalarLeaf(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("tags", NewArrayNode(NewLeafNode(String)))
	nullable := NewArrayNode(NewLeafNode(String))
	nullable.AddScalarTypes(Null)
	root.SetChild("maybe", nullable)

	m := root.FieldsMap()
	if _, ok := m["$.tags"]; ok {
		t.Error("pure array path $.tags must NOT emit a scalar leaf descriptor")
	}
	if _, ok := m["$.maybe"]; ok {
		t.Error("nullable array path $.maybe must NOT emit a scalar leaf descriptor")
	}
}

// A field observed as both an object and an array carries both branches; Merge
// records that as a KindObject node that kept its element, so the array branch
// is reached independently of the node's Kind.
func TestFieldsMixedObjectOrArray(t *testing.T) {
	obj := NewObjectNode()
	obj.SetChild("k", NewLeafNode(String))
	root := NewObjectNode()
	root.SetChild("both", Merge(obj, NewArrayNode(NewLeafNode(Integer))))

	m := root.FieldsMap()
	if _, ok := m["$.both.k"]; !ok {
		t.Errorf("expected object-branch leaf $.both.k; got %v", m)
	}
	elem, ok := m["$.both[*]"]
	if !ok {
		t.Fatalf("expected array-branch element $.both[*]; got %v", m)
	}
	if len(elem.Types) != 1 || elem.Types[0] != Integer {
		t.Errorf("$.both[*]: types = %v, want [INTEGER]", elem.Types)
	}
}
