package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// assertRoundTripsTo checks the delta from old to extended replays to exactly
// extended — Apply(old, Diff(old, new)) ≡ new, the op catalog's master
// invariant.
func assertRoundTripsTo(t *testing.T, old, extended *ModelNode) {
	t.Helper()
	delta, err := Diff(old, extended)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if delta == nil {
		t.Fatal("this is a change; Diff returned a no-op")
	}
	applied, err := Apply(old, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want, _ := Marshal(extended)
	got, _ := Marshal(applied)
	if string(want) != string(got) {
		t.Errorf("Apply(old, Diff(old,new)) != new\n  new     = %s\n  applied = %s", want, got)
	}
}

// An entity write can create a multi-kind declaration now; only sample-data
// import could before.
func TestExtendDiffApply_AddsABranchEndToEnd(t *testing.T) {
	cases := []struct {
		name               string
		existing, incoming func() *ModelNode
	}{
		{"scalar gains array", func() *ModelNode { return NewLeafNode(String) },
			func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }},
		{"scalar gains object", func() *ModelNode { return NewLeafNode(String) },
			func() *ModelNode { return NewObjectNode() }},
		{"object gains array", func() *ModelNode { return NewObjectNode() },
			func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }},
		{"array gains object", func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) },
			func() *ModelNode { return NewObjectNode() }},
		{"array gains scalar", func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) },
			func() *ModelNode { return NewLeafNode(Integer) }},
		{"object gains scalar", func() *ModelNode { return NewObjectNode() },
			func() *ModelNode { return NewLeafNode(Integer) }},
		{"object with children gains a scalar", func() *ModelNode {
			o := NewObjectNode()
			o.SetChild("k", NewLeafNode(Double))
			return o
		}, func() *ModelNode { return NewLeafNode(Integer) }},
		{"scalar gains an object with children", func() *ModelNode { return NewLeafNode(Integer) },
			func() *ModelNode {
				o := NewObjectNode()
				o.SetChild("k", NewLeafNode(Double))
				return o
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := NewObjectNode()
			old.SetChild("f", c.existing())
			in := NewObjectNode()
			in.SetChild("f", c.incoming())

			extended, err := Extend(old, in, spi.ChangeLevelStructural)
			if err != nil {
				t.Fatalf("Extend: %v", err)
			}
			if !extended.Object().Child("f").IsPolymorphic() {
				t.Fatalf("the extended field declares both kinds; kinds=%v",
					extended.Object().Child("f").Kinds())
			}
			assertRoundTripsTo(t, old, extended)
		})
	}
}

// The commonest of the three widenings that reached the client as a 500: a
// field first written as [], later holding object elements. Extend accepted it
// at every level and Diff then said "kind change ... (not additive)".
func TestExtendDiffApply_EmptyArrayThenObjectElements(t *testing.T) {
	objElem := func() *ModelNode {
		e := NewObjectNode()
		e.SetChild("a", NewLeafNode(Integer))
		return e
	}
	for _, c := range []struct {
		name     string
		incoming func() *ModelNode
	}{
		{"object elements", func() *ModelNode { return NewArrayNode(objElem()) }},
		{"mixed object and scalar elements", func() *ModelNode {
			return NewArrayNode(Merge(objElem(), NewLeafNode(String)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := NewObjectNode()
			old.SetChild("f", NewArrayNode(NewLeafNode(Null))) // what Walk gives for []
			in := NewObjectNode()
			in.SetChild("f", c.incoming())

			extended, err := Extend(old, in, spi.ChangeLevelArrayElements)
			if err != nil {
				t.Fatalf("Extend: %v", err)
			}
			assertRoundTripsTo(t, old, extended)
		})
	}
}

// A field observed only as null, later holding a container. Extend accepted it
// at TYPE and Diff could not express it.
func TestExtendDiffApply_NullableMarkerGainsAContainer(t *testing.T) {
	for _, c := range []struct {
		name     string
		incoming func() *ModelNode
	}{
		{"object", func() *ModelNode {
			o := NewObjectNode()
			o.SetChild("k", NewLeafNode(String))
			return o
		}},
		{"array", func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := NewObjectNode()
			old.SetChild("f", NewLeafNode(Null))
			in := NewObjectNode()
			in.SetChild("f", c.incoming())

			extended, err := Extend(old, in, spi.ChangeLevelType)
			if err != nil {
				t.Fatalf("promoting the marker keeps the TYPE contract; got: %v", err)
			}
			assertRoundTripsTo(t, old, extended)
		})
	}
}

// An array branch whose element was never observed at all. The op has to
// target the array node, because a "[]" segment cannot resolve to an element
// that does not exist yet.
func TestExtendDiffApply_UnobservedElementArrayGainsObjectElements(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", NewArrayNode(nil))

	elem := NewObjectNode()
	elem.SetChild("a", NewLeafNode(Integer))
	in := NewObjectNode()
	in.SetChild("f", NewArrayNode(elem))

	extended, err := Extend(old, in, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	assertRoundTripsTo(t, old, extended)
}

// Replay is a branch union, so it is idempotent and order-insensitive like
// every other op in the catalog.
func TestApply_AddKindBranchIsIdempotent(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", NewLeafNode(String))
	in := NewObjectNode()
	in.SetChild("f", NewArrayNode(NewLeafNode(String)))

	extended, err := Extend(old, in, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := Diff(old, extended)
	if err != nil {
		t.Fatal(err)
	}
	once, err := Apply(old, delta)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Apply(once, delta)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Marshal(once)
	b, _ := Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("replaying add_kind_branch twice changed the model\n  once  = %s\n  twice = %s", a, b)
	}
}

// The op carries exactly one branch; anything else is a delta this version did
// not write, and Apply fails closed on it.
func TestApply_AddKindBranchRejectsAMultiBranchPayload(t *testing.T) {
	base := NewObjectNode()
	base.SetChild("f", NewLeafNode(String))

	multi, err := Marshal(Merge(NewObjectNode(), NewArrayNode(NewLeafNode(String))))
	if err != nil {
		t.Fatal(err)
	}
	delta, err := MarshalDelta([]SchemaOp{NewAddKindBranch("f", multi)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(base, delta); err == nil {
		t.Error("add_kind_branch carries exactly one branch; a multi-branch payload must be rejected")
	}
}

// Adding a branch to a path that does not exist is a stale-delta signal, not a
// silent no-op.
func TestApply_AddKindBranchRejectsAMissingPath(t *testing.T) {
	base := NewObjectNode()
	base.SetChild("f", NewLeafNode(String))

	branch, err := Marshal(NewArrayNode(NewLeafNode(String)))
	if err != nil {
		t.Fatal(err)
	}
	delta, err := MarshalDelta([]SchemaOp{NewAddKindBranch("nope", branch)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(base, delta); err == nil {
		t.Error("add_kind_branch against a missing path must be rejected")
	}
}
