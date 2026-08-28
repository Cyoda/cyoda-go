package schema

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// These cases used to assert a dedicated polymorphic-slot sentinel, whose whole
// meaning was "raising changeLevel will not help you". Adding a kind to a path
// is a STRUCTURAL change now, so raising the level is exactly what resolves it
// and there is no separate classification to make: each case below is an
// ordinary change-level violation that names the level, and succeeds there.

// TestExtend_AddingAScalarBranchNamesStructural — an array path that the
// payload sends a scalar to is proposing a second kind for that path.
func TestExtend_AddingAScalarBranchNamesStructural(t *testing.T) {
	build := func() (*ModelNode, *ModelNode) {
		existing := NewObjectNode()
		existing.SetChild("x", NewArrayNode(NewLeafNode(String)))
		incoming := NewObjectNode()
		incoming.SetChild("x", NewLeafNode(Integer))
		return existing, incoming
	}

	existing, incoming := build()
	_, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err == nil {
		t.Fatal("adding a branch below STRUCTURAL must error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "STRUCTURAL") {
		t.Errorf("the rejection must name the level that resolves it: %q", msg)
	}
	if !strings.Contains(msg, "branch") {
		t.Errorf("the rejection must say what is being added: %q", msg)
	}

	existing, incoming = build()
	got, err := Extend(existing, incoming, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("raising the level must resolve it; got: %v", err)
	}
	if x := got.Object().Child("x"); x.Array() == nil || x.Scalar() == nil {
		t.Errorf("the extended path declares both kinds; kinds=%v", got.Object().Child("x").Kinds())
	}
}

// TestExtend_ChangeLevelViolation_NamesItsLevel — a new field is still a
// STRUCTURAL change and still says so. This case never involved a kind at all,
// and its answer is unchanged.
func TestExtend_ChangeLevelViolation_NamesItsLevel(t *testing.T) {
	existing := NewObjectNode()
	existing.SetChild("a", NewLeafNode(String))

	incoming := NewObjectNode()
	incoming.SetChild("a", NewLeafNode(String))
	incoming.SetChild("b", NewLeafNode(String)) // new field requires STRUCTURAL

	_, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err == nil {
		t.Fatal("new field at TYPE level must error")
	}
	if !strings.Contains(err.Error(), "STRUCTURAL") {
		t.Errorf("the rejection must name the level that resolves it: %q", err.Error())
	}
}

// TestExtend_AddingABranchToAnArrayElementNamesStructural — an element that
// declares one kind and is sent another follows the same rule, one level down.
func TestExtend_AddingABranchToAnArrayElementNamesStructural(t *testing.T) {
	build := func() (*ModelNode, *ModelNode) {
		existing := NewObjectNode()
		existing.SetChild("items", NewArrayNode(NewObjectNode()))
		incoming := NewObjectNode()
		incoming.SetChild("items", NewArrayNode(NewLeafNode(String)))
		return existing, incoming
	}

	existing, incoming := build()
	_, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err == nil {
		t.Fatal("adding a branch to an array element below STRUCTURAL must error")
	}
	if !strings.Contains(err.Error(), "STRUCTURAL") {
		t.Errorf("the rejection must name the level that resolves it: %q", err.Error())
	}

	existing, incoming = build()
	got, err := Extend(existing, incoming, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("raising the level must resolve it; got: %v", err)
	}
	elem := got.Object().Child("items").Array().Element()
	if elem.Object() == nil || elem.Scalar() == nil {
		t.Errorf("the extended element declares both kinds; kinds=%v", elem.Kinds())
	}
}

// TestExtend_RejectionNeverClaimsTheLevelIsIrrelevant — the old message told
// the client to "send the declared kind", which was the one thing that would
// not help when the model declared several. No rejection says that now.
func TestExtend_RejectionNeverClaimsTheLevelIsIrrelevant(t *testing.T) {
	existing := NewObjectNode()
	existing.SetChild("x", NewArrayNode(NewLeafNode(String)))

	incoming := NewObjectNode()
	incoming.SetChild("x", NewLeafNode(Integer))

	_, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "polymorphic") {
		t.Errorf("no rejection describes a polymorphic slot any more: %q", err.Error())
	}
}
