package schema

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A unique key can only be enforced over a path that holds a scalar and
// nothing else: claim computation tokenizes the value, and it refuses an
// object or an array. So a keyed path that DECLARES a container kind is an
// invalid key definition, even though its scalar branch is still there.
//
// This matters because the schema extension is committed before the write's
// transaction is opened. If the widening were allowed through, the model would
// permanently declare a kind that every subsequent write of that kind is then
// refused for — a declared but unwritable kind, which is the defect class this
// whole change exists to remove.
func TestValidateUniqueKeys_RejectsAKeyedPathThatDeclaresAContainer(t *testing.T) {
	keys := []spi.UniqueKey{{ID: "k", Fields: []string{"$.score"}}}

	for _, c := range []struct {
		name string
		node func() *ModelNode
	}{
		{"scalar and object", func() *ModelNode {
			o := NewObjectNode()
			o.SetChild("sub", NewLeafNode(String))
			return Merge(NewLeafNode(String), o)
		}},
		{"scalar and array", func() *ModelNode {
			return Merge(NewLeafNode(String), NewArrayNode(NewLeafNode(String)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := NewObjectNode()
			root.SetChild("score", c.node())

			err := ValidateUniqueKeys(root, keys)
			if err == nil {
				t.Fatal("a keyed path declaring a container kind must be rejected")
			}
			var de *UniqueKeyDefError
			if !errors.As(err, &de) {
				t.Fatalf("want *UniqueKeyDefError, got %T: %v", err, err)
			}
		})
	}
}

// The pure-scalar case is unaffected: that is what a unique key is for.
func TestValidateUniqueKeys_AcceptsAPureScalarLeaf(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("score", NewLeafNode(String))
	nested := NewObjectNode()
	nested.SetChild("city", NewLeafNode(String))
	root.SetChild("address", nested)

	keys := []spi.UniqueKey{{ID: "k", Fields: []string{"$.score", "$.address.city"}}}
	if err := ValidateUniqueKeys(root, keys); err != nil {
		t.Errorf("a pure scalar leaf is a valid key field: %v", err)
	}
}

// The rule stated directly: a keyed path must not be polymorphic. The check is
// written as "declares no container kind" because there are only three kinds
// and one of them is the scalar — so a polymorphic node always declares a
// container. This test pins the two formulations together, so a later edit to
// either cannot drift from the invariant.
func TestValidateUniqueKeys_APolymorphicKeyedPathIsAlwaysRejected(t *testing.T) {
	keys := []spi.UniqueKey{{ID: "k", Fields: []string{"$.f"}}}

	object := func() *ModelNode {
		o := NewObjectNode()
		o.SetChild("sub", NewLeafNode(String))
		return o
	}
	array := func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }

	for _, c := range []struct {
		name string
		node *ModelNode
	}{
		{"scalar and object", Merge(NewLeafNode(String), object())},
		{"scalar and array", Merge(NewLeafNode(String), array())},
		{"object and array", Merge(object(), array())},
		{"all three", Merge(Merge(NewLeafNode(String), object()), array())},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !c.node.IsPolymorphic() {
				t.Fatalf("precondition: this node is polymorphic; kinds=%v", c.node.Kinds())
			}
			root := NewObjectNode()
			root.SetChild("f", c.node)

			if err := ValidateUniqueKeys(root, keys); err == nil {
				t.Errorf("a polymorphic keyed path must be rejected; kinds=%v", c.node.Kinds())
			}
		})
	}
}
