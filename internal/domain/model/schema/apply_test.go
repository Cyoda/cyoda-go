package schema

import (
	"strings"
	"testing"
)

func mustMarshalNode(t *testing.T, n *ModelNode) []byte {
	t.Helper()
	b, err := Marshal(n)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

func mustApply(t *testing.T, base *ModelNode, ops []SchemaOp) *ModelNode {
	t.Helper()
	delta, err := MarshalDelta(ops)
	if err != nil {
		t.Fatalf("MarshalDelta: %v", err)
	}
	out, err := Apply(base, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return out
}

func TestApply_EmptyDelta_ReturnsCloneNotSameRef(t *testing.T) {
	base := NewObjectNode()
	base.SetChild("name", NewLeafNode(String))

	out, err := Apply(base, nil)
	if err != nil {
		t.Fatalf("Apply nil: %v", err)
	}
	if out == base {
		t.Error("Apply with empty delta must return a fresh tree, not the same pointer")
	}
	if _, ok := out.Object().Children()["name"]; !ok {
		t.Error("expected child 'name' preserved in clone")
	}
	// Mutating clone must not affect base.
	out.SetChild("extra", NewLeafNode(Integer))
	if base.Object().Child("extra") != nil {
		t.Error("mutating clone altered base")
	}
}

func TestApply_AddProperty_InsertsIntoObjectRoot(t *testing.T) {
	base := NewObjectNode()
	base.SetChild("name", NewLeafNode(String))

	newChild := NewLeafNode(String)
	sub := mustMarshalNode(t, newChild)
	op := NewAddProperty("", "email", sub)

	out := mustApply(t, base, []SchemaOp{op})
	if out.Object().Child("email") == nil {
		t.Fatal("email child missing")
	}
	if out.Object().Child("email").Scalar() == nil {
		t.Errorf("email kinds: got %v", out.Object().Child("email").Kinds())
	}
	// Base must be untouched.
	if base.Object().Child("email") != nil {
		t.Error("Apply mutated input")
	}
}

func TestApply_AddProperty_NestedPath(t *testing.T) {
	addr := NewObjectNode()
	addr.SetChild("street", NewLeafNode(String))
	root := NewObjectNode()
	root.SetChild("address", addr)

	zip := NewLeafNode(String)
	op := NewAddProperty("address", "zip", mustMarshalNode(t, zip))
	out := mustApply(t, root, []SchemaOp{op})

	if out.Object().Child("address").Object().Child("zip") == nil {
		t.Fatal("address/zip missing")
	}
}

func TestApply_AddProperty_ExistingChildMerges(t *testing.T) {
	root := NewObjectNode()
	existing := NewLeafNode(String)
	root.SetChild("age", existing)

	// Incoming subtree is a LEAF{Integer}. Merge → LEAF{INTEGER, STRING}.
	incoming := NewLeafNode(Integer)
	op := NewAddProperty("", "age", mustMarshalNode(t, incoming))
	out := mustApply(t, root, []SchemaOp{op})

	got := out.Object().Child("age").DeclaredTypes()
	if len(got) != 2 {
		t.Fatalf("expected merged TypeSet len 2, got %d (%v)", len(got), got)
	}
}

func TestApply_AddProperty_ParentMustBeObject(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("leafy", NewLeafNode(String))

	op := NewAddProperty("leafy", "x", mustMarshalNode(t, NewLeafNode(Integer)))
	_, err := Apply(root, mustMarshalDeltaT(t, []SchemaOp{op}))
	if err == nil || !strings.Contains(err.Error(), "object") {
		t.Errorf("expected error about non-object parent, got: %v", err)
	}
}

func TestApply_BroadenType_UnionsPrimitives(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("age", NewLeafNode(String))

	// Broaden with a concrete type so the union is observable; NULL now drops
	// when any concrete type is present per the TypeSet.Add collapse rule.
	op, err := NewBroadenType("age", []DataType{Integer})
	if err != nil {
		t.Fatalf("NewBroadenType: %v", err)
	}
	out := mustApply(t, root, []SchemaOp{op})

	types := out.Object().Child("age").DeclaredTypes()
	names := []string{}
	for _, d := range types {
		names = append(names, d.String())
	}
	if !sliceContains(names, "INTEGER") || !sliceContains(names, "STRING") {
		t.Errorf("expected INTEGER+STRING in TypeSet, got %v", names)
	}
}

func TestApply_BroadenType_OnObjectAddsNullableMarker(t *testing.T) {
	// broaden_type widens the target node's own TypeSet. For OBJECT
	// (and ARRAY) targets this is how a nullable marker (NULL) is
	// added when an observation sees the structural position as nil.
	// The round-trip property test drives this contract.
	root := NewObjectNode()
	root.SetChild("addr", NewObjectNode())

	op, err := NewBroadenType("addr", []DataType{Null})
	if err != nil {
		t.Fatalf("NewBroadenType: %v", err)
	}
	out := mustApply(t, root, []SchemaOp{op})

	addr := out.Object().Child("addr")
	if addr == nil {
		t.Fatalf("addr child missing after apply")
	}
	if addr.Object() == nil {
		t.Fatalf("addr lost its object branch: got %v", addr.Kinds())
	}
	found := false
	for _, dt := range addr.DeclaredTypes() {
		if dt == Null {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected NULL in addr.Types(), got %v", addr.DeclaredTypes())
	}
}

func TestApply_BroadenType_RejectsNonNullOnObject(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("addr", NewObjectNode())
	op, err := NewBroadenType("addr", []DataType{String}) // non-NULL on OBJECT
	if err != nil {
		t.Fatalf("NewBroadenType: %v", err)
	}
	delta, err := MarshalDelta([]SchemaOp{op})
	if err != nil {
		t.Fatalf("MarshalDelta: %v", err)
	}
	if _, err := Apply(root, delta); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApply_AddArrayItemType_WidensElementLeaf(t *testing.T) {
	arr := NewArrayNode(NewLeafNode(Integer))
	root := NewObjectNode()
	root.SetChild("tags", arr)

	op, err := NewAddArrayItemType("tags", []DataType{String})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out := mustApply(t, root, []SchemaOp{op})

	elem := out.Object().Child("tags").Array().Element()
	names := typeNames(elem.DeclaredTypes())
	if !sliceContains(names, "INTEGER") || !sliceContains(names, "STRING") {
		t.Errorf("expected INTEGER+STRING in element TypeSet, got %v", names)
	}
}

func TestApply_AddArrayItemType_MustTargetArray(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("notAnArray", NewLeafNode(String))

	op, err := NewAddArrayItemType("notAnArray", []DataType{Integer})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = Apply(root, mustMarshalDeltaT(t, []SchemaOp{op}))
	if err == nil || !strings.Contains(err.Error(), "array") {
		t.Errorf("expected array-target error, got: %v", err)
	}
}

func TestApply_PathSegmentNotFound(t *testing.T) {
	root := NewObjectNode()
	op, err := NewBroadenType("missing", []DataType{Null})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = Apply(root, mustMarshalDeltaT(t, []SchemaOp{op}))
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected missing-segment error, got: %v", err)
	}
}

func TestApply_UnknownOpKind(t *testing.T) {
	// Construct a forward-incompat kind directly.
	op := SchemaOp{Kind: SchemaOpKind("widget_reverse"), Path: ""}
	_, err := Apply(NewObjectNode(), mustMarshalDeltaT(t, []SchemaOp{op}))
	if err == nil || !strings.Contains(err.Error(), "unknown op kind") {
		t.Errorf("expected unknown-kind error, got: %v", err)
	}
}

func TestApply_MalformedDelta(t *testing.T) {
	_, err := Apply(NewObjectNode(), []byte(`{not-json`))
	if err == nil {
		t.Error("expected error on malformed delta")
	}
}

func TestApply_MultipleOps_Replays(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("age", NewLeafNode(String))

	op1 := NewAddProperty("", "email", mustMarshalNode(t, NewLeafNode(String)))
	op2, _ := NewBroadenType("age", []DataType{Null})
	op3, _ := NewBroadenType("age", []DataType{Integer})
	out := mustApply(t, root, []SchemaOp{op1, op2, op3})

	if out.Object().Child("email") == nil {
		t.Error("email missing after multi-op Apply")
	}
	names := typeNames(out.Object().Child("age").DeclaredTypes())
	// After broadening with NULL and INTEGER (on a STRING leaf), NULL drops
	// because concrete types are present. STRING and INTEGER are preserved
	// (cross-kind polymorphism).
	for _, want := range []string{"STRING", "INTEGER"} {
		if !sliceContains(names, want) {
			t.Errorf("expected %s in age types after broadens, got %v", want, names)
		}
	}
	if sliceContains(names, "NULL") {
		t.Errorf("NULL should drop when concrete types present, got %v", names)
	}
}

// --- helpers --- //

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func typeNames(dts []DataType) []string {
	out := make([]string, len(dts))
	for i, d := range dts {
		out[i] = d.String()
	}
	return out
}

func mustMarshalDeltaT(t *testing.T, ops []SchemaOp) []byte {
	t.Helper()
	d, err := MarshalDelta(ops)
	if err != nil {
		t.Fatalf("MarshalDelta: %v", err)
	}
	return d
}
