package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// roundTrip verifies Apply(old, Diff(old, new)) ≡ new (by Marshal bytes).
func roundTrip(t *testing.T, oldN, newN *ModelNode) {
	t.Helper()
	delta, err := Diff(oldN, newN)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	applied, err := Apply(oldN, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want, err := Marshal(newN)
	if err != nil {
		t.Fatalf("Marshal new: %v", err)
	}
	got, err := Marshal(applied)
	if err != nil {
		t.Fatalf("Marshal applied: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("round-trip failed:\nwant: %s\ngot:  %s", want, got)
	}
}

// TestDiff_ArrayOfObjects_AddChildInElement reproduces a bug reported
// against a live model: adding a property to an object that lives inside
// an array was rejected with "array element ... is not a leaf; beyond
// catalog". The catalog DOES support this — we just need to recurse
// through the array into the element with a "[]" path segment.
func TestDiff_ArrayOfObjects_AddChildInElement(t *testing.T) {
	oldElem := NewObjectNode()
	oldElem.SetChild("a", NewLeafNode(String))
	oldRoot := NewObjectNode()
	oldRoot.SetChild("items", NewArrayNode(oldElem))

	newElem := NewObjectNode()
	newElem.SetChild("a", NewLeafNode(String))
	newElem.SetChild("b", NewLeafNode(Integer))
	newRoot := NewObjectNode()
	newRoot.SetChild("items", NewArrayNode(newElem))

	roundTrip(t, oldRoot, newRoot)
}

// TestDiff_ArrayOfObjects_BroadenLeafInElement: array of OBJECT, the
// element has a LEAF child whose TypeSet gets widened.
func TestDiff_ArrayOfObjects_BroadenLeafInElement(t *testing.T) {
	oldElem := NewObjectNode()
	oldElem.SetChild("a", NewLeafNode(String))
	oldRoot := NewObjectNode()
	oldRoot.SetChild("items", NewArrayNode(oldElem))

	newElem := NewObjectNode()
	newA := NewLeafNode(String)
	newA.AddScalarTypes(Null)
	newElem.SetChild("a", newA)
	newRoot := NewObjectNode()
	newRoot.SetChild("items", NewArrayNode(newElem))

	roundTrip(t, oldRoot, newRoot)
}

// TestDiff_ArrayOfArray_WidenInnerLeaf: nested arrays — array of
// array of LEAF — extend the inner leaf. Verifies that the "[]"
// segment can appear multiple times in a path.
func TestDiff_ArrayOfArray_WidenInnerLeaf(t *testing.T) {
	oldRoot := NewObjectNode()
	oldRoot.SetChild("grid", NewArrayNode(NewArrayNode(NewLeafNode(Integer))))

	innerLeaf := NewLeafNode(Integer)
	innerLeaf.AddScalarTypes(Null)
	newRoot := NewObjectNode()
	newRoot.SetChild("grid", NewArrayNode(NewArrayNode(innerLeaf)))

	roundTrip(t, oldRoot, newRoot)
}

func TestDiff_NoChange_ReturnsNil(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("name", NewLeafNode(String))

	delta, err := Diff(root, cloneByMarshal(t, root))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if delta != nil {
		t.Errorf("expected nil delta for no-op, got %s", delta)
	}
}

func TestDiff_AddProperty(t *testing.T) {
	oldN := NewObjectNode()
	oldN.SetChild("name", NewLeafNode(String))
	newN := NewObjectNode()
	newN.SetChild("name", NewLeafNode(String))
	newN.SetChild("email", NewLeafNode(String))
	roundTrip(t, oldN, newN)
}

func TestDiff_AddPropertyNested(t *testing.T) {
	oldAddr := NewObjectNode()
	oldAddr.SetChild("street", NewLeafNode(String))
	oldN := NewObjectNode()
	oldN.SetChild("address", oldAddr)

	newAddr := NewObjectNode()
	newAddr.SetChild("street", NewLeafNode(String))
	newAddr.SetChild("zip", NewLeafNode(String))
	newN := NewObjectNode()
	newN.SetChild("address", newAddr)
	roundTrip(t, oldN, newN)
}

func TestDiff_BroadenType(t *testing.T) {
	oldN := NewObjectNode()
	oldN.SetChild("age", NewLeafNode(String))
	newN := NewObjectNode()
	ageNew := NewLeafNode(String)
	ageNew.AddScalarTypes(Null)
	newN.SetChild("age", ageNew)
	roundTrip(t, oldN, newN)
}

func TestDiff_AddArrayItemType(t *testing.T) {
	oldN := NewObjectNode()
	oldN.SetChild("tags", NewArrayNode(NewLeafNode(Integer)))
	newN := NewObjectNode()
	elem := NewLeafNode(Integer)
	elem.AddScalarTypes(String)
	newN.SetChild("tags", NewArrayNode(elem))
	roundTrip(t, oldN, newN)
}

func TestDiff_PropertyRemoval_Error(t *testing.T) {
	oldN := NewObjectNode()
	oldN.SetChild("gone", NewLeafNode(String))
	newN := NewObjectNode()
	_, err := Diff(oldN, newN)
	if err == nil {
		t.Fatal("expected error on property removal")
	}
}

func TestDiff_KindChange_Error(t *testing.T) {
	oldN := NewObjectNode()
	oldN.SetChild("x", NewLeafNode(String))
	newN := NewObjectNode()
	newN.SetChild("x", NewArrayNode(NewLeafNode(String)))
	_, err := Diff(oldN, newN)
	if err == nil {
		t.Fatal("expected error on kind change")
	}
}

func TestDiff_NilInput_Error(t *testing.T) {
	if _, err := Diff(nil, NewObjectNode()); err == nil {
		t.Error("expected error on nil old")
	}
	if _, err := Diff(NewObjectNode(), nil); err == nil {
		t.Error("expected error on nil new")
	}
}

func TestDiff_MultipleOps(t *testing.T) {
	oldN := NewObjectNode()
	oldN.SetChild("a", NewLeafNode(String))
	oldN.SetChild("b", NewArrayNode(NewLeafNode(Integer)))

	newN := NewObjectNode()
	aNew := NewLeafNode(String)
	aNew.AddScalarTypes(Boolean) // broaden with concrete type (NULL would drop)
	newN.SetChild("a", aNew)
	bElem := NewLeafNode(Integer)
	bElem.AddScalarTypes(String) // array-widen
	newN.SetChild("b", NewArrayNode(bElem))
	newN.SetChild("c", NewLeafNode(Boolean)) // add_property
	roundTrip(t, oldN, newN)

	// And check that we got at least 3 ops.
	delta, _ := Diff(oldN, newN)
	ops, err := UnmarshalDelta(delta)
	if err != nil {
		t.Fatalf("UnmarshalDelta: %v", err)
	}
	if len(ops) < 3 {
		t.Errorf("expected at least 3 ops, got %d: %+v", len(ops), ops)
	}
}

func cloneByMarshal(t *testing.T, n *ModelNode) *ModelNode {
	t.Helper()
	raw, err := Marshal(n)
	if err != nil {
		t.Fatalf("Marshal clone: %v", err)
	}
	out, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal clone: %v", err)
	}
	return out
}

// Guard against unused import.
var _ spi.SchemaDelta
