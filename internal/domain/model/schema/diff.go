package schema

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Diff returns a SchemaDelta such that Apply(old, Diff(old, new)) ≡ new,
// provided new differs from old only by additive extension within the
// op catalog (add_property, broaden_type, add_array_item_type).
//
// Returns (nil, nil) on semantic no-op.
// Returns an error if the change is not expressible — kind changes,
// property removal, or a replacement of a sub-tree that would require
// a non-additive op.
func Diff(oldN, newN *ModelNode) (spi.SchemaDelta, error) {
	if oldN == nil || newN == nil {
		return nil, fmt.Errorf("schema.Diff: nil input")
	}
	var ops []SchemaOp
	if err := diffNode("", oldN, newN, &ops); err != nil {
		return nil, fmt.Errorf("schema.Diff: %w", err)
	}
	if len(ops) == 0 {
		return nil, nil
	}
	return MarshalDelta(ops)
}

func diffNode(path string, oldN, newN *ModelNode, ops *[]SchemaOp) error {
	for _, k := range oldN.Kinds() {
		if newN.Branch(k) == nil {
			return fmt.Errorf("kind removal at %q: %s is no longer declared (not additive)",
				displayPath(path), k)
		}
	}
	// A node declares its own types: a scalar branch holds the primitive data
	// types, and a node with no scalar branch holds the nullable marker when
	// the structural position has been observed as nil. Diffing DeclaredTypes
	// covers both, so nullable-marker growth on a container surfaces as a
	// KindBroadenType op rather than being silently dropped.
	if added := typeDifference(newN.DeclaredTypes(), oldN.DeclaredTypes()); len(added) > 0 {
		op, err := NewBroadenType(path, added)
		if err != nil {
			return fmt.Errorf("broaden_type at %q: %w", displayPath(path), err)
		}
		*ops = append(*ops, op)
	}
	if newN.Object() != nil {
		if oldN.Object() == nil {
			return fmt.Errorf("adding an %s branch at %q is not yet expressible", KindObject, displayPath(path))
		}
		if err := diffObject(path, oldN, newN, ops); err != nil {
			return err
		}
	}
	if newN.Array() != nil {
		if oldN.Array() == nil {
			return fmt.Errorf("adding an %s branch at %q is not yet expressible", KindArray, displayPath(path))
		}
		if err := diffArray(path, oldN, newN, ops); err != nil {
			return err
		}
	}
	if newN.Scalar() != nil && oldN.Scalar() == nil && len(oldN.Kinds()) > 0 {
		return fmt.Errorf("adding a %s branch at %q is not yet expressible", KindLeaf, displayPath(path))
	}
	return nil
}

func diffObject(path string, oldN, newN *ModelNode, ops *[]SchemaOp) error {
	newChildren := newN.Object().Children()
	for name, newChild := range newChildren {
		oldChild := oldN.Object().Child(name)
		if oldChild == nil {
			raw, err := Marshal(newChild)
			if err != nil {
				return fmt.Errorf("marshal subtree %q: %w", joinSchemaPath(path, name), err)
			}
			*ops = append(*ops, NewAddProperty(path, name, raw))
			continue
		}
		if err := diffNode(joinSchemaPath(path, name), oldChild, newChild, ops); err != nil {
			return err
		}
	}
	for name := range oldN.Object().Children() {
		if _, ok := newChildren[name]; !ok {
			return fmt.Errorf("property removal at %q is not additive", joinSchemaPath(path, name))
		}
	}
	return nil
}

func diffArray(path string, oldN, newN *ModelNode, ops *[]SchemaOp) error {
	oldElem := oldN.Array().Element()
	newElem := newN.Array().Element()
	// Both nil: no element ever observed — nothing to emit.
	if oldElem == nil && newElem == nil {
		return nil
	}
	// Incoming element disappeared — not additive.
	if newElem == nil {
		return fmt.Errorf("array element removed at %q", displayPath(path))
	}
	// Old was an empty array (no observed element yet). Treat this as an
	// "unobserved element" transitioning to a concrete one. Only the
	// LEAF case is expressible via the current op catalog.
	// TODO(A.3 / issue #85): when oldElem is nil and newElem is non-LEAF
	// (OBJECT/ARRAY element first seen via polymorphic write), the transition
	// needs a new op kind beyond add_array_item_type. Tracked in Sub-project
	// A.3 (polymorphic-slot kind conflicts).
	if oldElem == nil {
		if newElem.Object() != nil || newElem.Array() != nil {
			return fmt.Errorf("array element materialization at %q requires a scalar element; got %s (extend to a scalar element first)",
				displayPath(path), kindNames(newElem))
		}
		op, err := NewAddArrayItemType(path, newElem.DeclaredTypes())
		if err != nil {
			return fmt.Errorf("add_array_item_type at %q: %w", displayPath(path), err)
		}
		*ops = append(*ops, op)
		return nil
	}
	// LEAF-element arrays use the dedicated widening op (cheapest and
	// most common shape from schema.Extend at ChangeLevelArrayElements).
	if isScalarOnly(oldElem) && isScalarOnly(newElem) {
		added := typeDifference(newElem.DeclaredTypes(), oldElem.DeclaredTypes())
		if len(added) == 0 {
			return nil
		}
		op, err := NewAddArrayItemType(path, added)
		if err != nil {
			return fmt.Errorf("add_array_item_type at %q: %w", displayPath(path), err)
		}
		*ops = append(*ops, op)
		return nil
	}
	// OBJECT- or ARRAY-element: descend into the element using the "[]"
	// path segment. Subsequent ops (add_property, broaden_type, etc.)
	// carry paths like "parent/[]/child" that Apply's resolvePath
	// follows by traversing the array's Element().
	return diffNode(joinSchemaPath(path, "[]"), oldElem, newElem, ops)
}

// typeDifference returns the DataTypes present in `b` but not in `a`, in
// stable canonical order (so the resulting op payload is deterministic).
func typeDifference(b, a []DataType) []DataType {
	in := make(map[DataType]struct{}, len(a))
	for _, dt := range a {
		in[dt] = struct{}{}
	}
	var added []DataType
	for _, dt := range b {
		if _, ok := in[dt]; !ok {
			added = append(added, dt)
		}
	}
	return added
}

// isScalarOnly reports whether a node declares a scalar and no container — the
// shape the dedicated element-widening op covers.
func isScalarOnly(n *ModelNode) bool {
	return n.Object() == nil && n.Array() == nil
}

// joinSchemaPath returns the slash-joined child path used by the op
// catalog (distinct from validate.go's dotted display paths).
func joinSchemaPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

// displayPath renders an empty path as "(root)" for error messages.
func displayPath(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}
