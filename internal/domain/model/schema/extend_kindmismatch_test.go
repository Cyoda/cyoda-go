package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// These two cases used to assert that a path declaring one kind REFUSED a
// payload of another at every level, STRUCTURAL included. That was the
// limitation: only a sample-data import could establish a second kind for a
// path, so an entity write could never record what the model was perfectly
// able to describe.
//
// A write can add a kind now, at STRUCTURAL. The concern the original cases
// guarded — that a node holding both a container and primitive types must not
// be something Apply cannot replay — is what they assert instead, end to end.

func TestExtend_ScalarPathGainsAnObjectBranch(t *testing.T) {
	existing := NewObjectNode()
	existing.SetChild("f0", NewLeafNode(Integer))

	incoming := NewObjectNode()
	incomingF0 := NewObjectNode()
	incomingF0.SetChild("k0", NewLeafNode(Double))
	incoming.SetChild("f0", incomingF0)

	assertDeclaresBothKinds(t, existing, incoming, "f0")
}

func TestExtend_ObjectPathGainsAScalarBranch(t *testing.T) {
	existingF0 := NewObjectNode()
	existingF0.SetChild("k0", NewLeafNode(Double))
	existing := NewObjectNode()
	existing.SetChild("f0", existingF0)

	incoming := NewObjectNode()
	incoming.SetChild("f0", NewLeafNode(Integer))

	assertDeclaresBothKinds(t, existing, incoming, "f0")
}

// assertDeclaresBothKinds checks that the extension is accepted at STRUCTURAL
// and that the named child ends up declaring both kinds. That the delta then
// replays to exactly this model — the property the original rejection was
// protecting — is asserted end to end in add_kind_branch_test.go.
func assertDeclaresBothKinds(t *testing.T, existing, incoming *ModelNode, child string) {
	t.Helper()

	extended, err := Extend(existing, incoming, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("Extend at STRUCTURAL: %v", err)
	}
	got := extended.Object().Child(child)
	if got.Object() == nil || got.Scalar() == nil {
		t.Fatalf("%q must declare both kinds; kinds=%v", child, got.Kinds())
	}
}
