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
	// Losing the nullable marker is the same class of change, and just as
	// unreachable through Merge — stated here so the two halves of what a node
	// declares are checked in one place rather than one of them silently.
	if oldN.Nullable() && !newN.Nullable() && newN.Scalar() == nil {
		return fmt.Errorf("nullable removal at %q is not additive", displayPath(path))
	}
	// Nullability first: a structural position newly observed as nil records
	// the marker, which is a broaden_type of NULL on the node itself.
	if newN.Nullable() && !oldN.Nullable() {
		op, err := NewBroadenType(path, []DataType{Null})
		if err != nil {
			return fmt.Errorf("broaden_type at %q: %w", displayPath(path), err)
		}
		*ops = append(*ops, op)
	}
	// Then the scalar branch. Widening one that already exists is a
	// broaden_type, and so is establishing the first kind on a node that
	// declares none — that is how a null-only path widening to a scalar has
	// always been spelled, and applyBroadenType accepts concrete types in
	// exactly those two cases. A node that already declares another kind is
	// gaining a BRANCH, which is handled below.
	if ns := newN.Scalar(); ns != nil {
		var added []DataType
		switch os := oldN.Scalar(); {
		case os != nil:
			added = typeDifference(ns.Types(), os.Types())
		case len(oldN.Kinds()) == 0:
			added = ns.Types()
		}
		if len(added) > 0 {
			op, err := NewBroadenType(path, added)
			if err != nil {
				return fmt.Errorf("broaden_type at %q: %w", displayPath(path), err)
			}
			*ops = append(*ops, op)
		}
	}
	if newN.Object() != nil {
		if oldN.Object() == nil {
			if err := emitAddKindBranch(path, KindObject, newN, ops); err != nil {
				return err
			}
		} else if err := diffObject(path, oldN, newN, ops); err != nil {
			return err
		}
	}
	if newN.Array() != nil {
		if oldN.Array() == nil {
			if err := emitAddKindBranch(path, KindArray, newN, ops); err != nil {
				return err
			}
		} else if err := diffArray(path, oldN, newN, ops); err != nil {
			return err
		}
	}
	// The scalar branch is the one case broaden_type already covers: on a node
	// that declares no kind at all, adding concrete types establishes it, and
	// that is how a null-only path widening to a scalar has always been
	// spelled. Only a node that already declares another kind needs the branch
	// op.
	if newN.Scalar() != nil && oldN.Scalar() == nil && len(oldN.Kinds()) > 0 {
		if err := emitAddKindBranch(path, KindLeaf, newN, ops); err != nil {
			return err
		}
	}
	return nil
}

// emitAddKindBranch appends an op carrying exactly the one branch `k` that
// newN declares at this path, so replay merges that branch and nothing else.
func emitAddKindBranch(path string, k NodeKind, newN *ModelNode, ops *[]SchemaOp) error {
	raw, err := Marshal(isolateBranch(newN, k))
	if err != nil {
		return fmt.Errorf("marshal %s branch at %q: %w", k, displayPath(path), err)
	}
	*ops = append(*ops, NewAddKindBranch(path, raw))
	return nil
}

// isolateBranch returns a node carrying only n's branch of kind k. The
// nullable marker is deliberately not carried: nullability is diffed at the
// node level, so copying it here would record it twice.
func isolateBranch(n *ModelNode, k NodeKind) *ModelNode {
	out := &ModelNode{}
	out.DeclareKind(k)
	switch k {
	case KindLeaf:
		out.AddScalarTypes(n.Scalar().Types()...)
	case KindObject:
		for name, child := range n.Object().Children() {
			out.SetChild(name, child)
		}
	case KindArray:
		a := n.Array()
		if a.Element() != nil {
			out.SetElement(a.Element())
		}
		out.ObserveArrayWidth(a.MaxWidth())
	}
	return out
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
	// Old was an array observed with no content, so it declared no element
	// type at all; the incoming one is establishing it.
	if oldElem == nil {
		// A scalar element uses the dedicated, cheaper op. Anything else is
		// carried as the array branch itself: Apply materialises the element
		// by merging the branch, because resolvePath cannot descend a "[]"
		// segment into an element that does not exist yet.
		if newElem.Object() != nil || newElem.Array() != nil {
			return emitAddKindBranch(path, KindArray, newN, ops)
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
