package schema

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ApplyMany is the apply function the composition root wires into the
// storage plugins: it decodes the persisted schema base once, replays every
// delta in order onto that one tree, and encodes it once. The result is the
// bytes that replaying the deltas one ApplyMany call each gives; an empty
// list gives the canonical encoding of base. A delta that cannot apply fails
// the whole call.
//
// The tree is decoded here and owned by this call, so the deltas apply to it
// in place. Neither base nor the deltas are written to.
func ApplyMany(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
	root, err := Unmarshal(base)
	if err != nil {
		return nil, fmt.Errorf("schema.ApplyMany: unmarshal base: %w", err)
	}
	for i, delta := range deltas {
		if err := applyInPlace(root, delta); err != nil {
			return nil, fmt.Errorf("schema.ApplyMany: delta %d: %w", i, err)
		}
	}
	return Marshal(root)
}

// applyInPlace replays delta's ops onto root, mutating it. An empty delta
// changes nothing.
func applyInPlace(root *ModelNode, delta spi.SchemaDelta) error {
	if len(delta) == 0 {
		return nil
	}
	ops, err := UnmarshalDelta(delta)
	if err != nil {
		return fmt.Errorf("decode delta: %w", err)
	}
	for i, op := range ops {
		if err := applyOp(root, op); err != nil {
			return fmt.Errorf("op %d (%s %q): %w", i, op.Kind, op.Path, err)
		}
	}
	return nil
}

func applyOp(root *ModelNode, op SchemaOp) error {
	switch op.Kind {
	case KindAddProperty:
		return applyAddProperty(root, op)
	case KindBroadenType:
		return applyBroadenType(root, op)
	case KindAddArrayItemType:
		return applyAddArrayItemType(root, op)
	case KindAddKindBranch:
		return applyAddKindBranch(root, op)
	default:
		return fmt.Errorf("unknown op kind %q", op.Kind)
	}
}

func applyAddProperty(root *ModelNode, op SchemaOp) error {
	parent, err := resolvePath(root, op.Path)
	if err != nil {
		return fmt.Errorf("resolve parent: %w", err)
	}
	if parent.Object() == nil {
		return fmt.Errorf("parent at %q does not declare an object (kinds=%s)", op.Path, kindNames(parent))
	}
	if op.Name == "" {
		return fmt.Errorf("add_property requires a non-empty Name")
	}
	incoming, err := Unmarshal(op.Payload)
	if err != nil {
		return fmt.Errorf("decode subtree: %w", err)
	}
	if existing := parent.Object().Child(op.Name); existing != nil {
		parent.SetChild(op.Name, Merge(existing, incoming))
		return nil
	}
	parent.SetChild(op.Name, incoming)
	return nil
}

func applyBroadenType(root *ModelNode, op SchemaOp) error {
	target, err := resolvePath(root, op.Path)
	if err != nil {
		return fmt.Errorf("resolve target: %w", err)
	}
	// broaden_type widens the target node's scalar declaration: the primitive
	// types where the node already declares a scalar, the nullable marker where
	// it declares none, and the scalar branch itself where the node so far
	// declares only a container.
	//
	// That last case is not a malformed delta, and it must not be refused. Ops
	// in this catalog commute — two deltas diffed from the SAME model version
	// are folded in whatever order they arrive — so an add_kind_branch that
	// gave this path a container may already have been replayed. The end state
	// is one a commuted add_kind_branch(LEAF) reaches anyway, so accepting it
	// forfeits nothing: it is the same model either way, which is exactly what
	// commutativity means.
	types, err := DecodeTypeNames(op.Payload)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	target.AddScalarTypes(types...)
	return nil
}

func applyAddArrayItemType(root *ModelNode, op SchemaOp) error {
	target, err := resolvePath(root, op.Path)
	if err != nil {
		return fmt.Errorf("resolve array: %w", err)
	}
	if target.Array() == nil {
		return fmt.Errorf("target at %q does not declare an array (kinds=%s)", op.Path, kindNames(target))
	}
	types, err := DecodeTypeNames(op.Payload)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	elem := target.Array().Element()
	if elem == nil {
		// Target was an empty-array seed (no observed element yet).
		// Materialize a fresh LEAF element seeded with the first
		// payload type; the loop below unions in the remainder.
		if len(types) == 0 {
			return fmt.Errorf("array at %q has no element and payload is empty", op.Path)
		}
		elem = NewLeafNode(types[0])
		elem.AddScalarTypes(types[1:]...)
		target.SetElement(elem)
		return nil
	}
	// The op widens the element's SCALAR declaration, and it does so whatever
	// else the element declares. An element already observed as a container
	// takes the widening for the same reason the node-level broaden_type does:
	// ops commute, so the branch that made it a union may already have been
	// replayed, and refusing here would make the fold order decide whether a
	// legal pair of deltas applies.
	elem.AddScalarTypes(types...)
	return nil
}

// applyAddKindBranch merges one encoded branch into the target node's branch
// set, in place. Merging rather than assigning is what makes replay idempotent
// and order-insensitive: a branch already present unions with itself, and the
// same op applied twice leaves the same model.
func applyAddKindBranch(root *ModelNode, op SchemaOp) error {
	target, err := resolvePath(root, op.Path)
	if err != nil {
		return fmt.Errorf("resolve target: %w", err)
	}
	incoming, err := Unmarshal(op.Payload)
	if err != nil {
		return fmt.Errorf("decode branch: %w", err)
	}
	kinds := incoming.Kinds()
	if len(kinds) != 1 {
		return fmt.Errorf("add_kind_branch carries exactly one branch, got %s", kindNames(incoming))
	}

	// Declared first, so a branch that carries no payload — an object observed
	// with no children, an array observed with no content — still lands.
	k := kinds[0]
	target.DeclareKind(k)
	switch k {
	case KindLeaf:
		target.AddScalarTypes(incoming.Scalar().Types()...)
	case KindObject:
		for name, child := range incoming.Object().Children() {
			if existing := target.Object().Child(name); existing != nil {
				child = Merge(existing, child)
			}
			target.SetChild(name, child)
		}
	case KindArray:
		a := incoming.Array()
		if a.Element() != nil {
			target.SetElement(Merge(target.Array().Element(), a.Element()))
		}
	}
	return nil
}

// resolvePath walks root along a slash-separated path of child names.
// The empty path returns root. A missing segment returns a descriptive
// error rather than a nil node — ApplyMany surfaces the segment name so
// the error identifies the stale-schema case cleanly.
func resolvePath(root *ModelNode, path string) (*ModelNode, error) {
	if path == "" {
		return root, nil
	}
	parts := strings.Split(path, "/")
	cur := root
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("empty path segment in %q", path)
		}
		// "[]" descends into an ARRAY's element. Produced by Diff when
		// an additive change lives inside an array-of-objects or a
		// nested array.
		if part == "[]" {
			if cur.Array() == nil {
				return nil, fmt.Errorf("cannot descend into the element of a node that declares %s at segment %q", kindNames(cur), part)
			}
			elem := cur.Array().Element()
			if elem == nil {
				return nil, fmt.Errorf("array has no element at segment %q", part)
			}
			cur = elem
			continue
		}
		if cur.Object() == nil {
			return nil, fmt.Errorf("cannot descend through a node that declares %s at segment %q", kindNames(cur), part)
		}
		next := cur.Object().Child(part)
		if next == nil {
			return nil, fmt.Errorf("missing segment %q under %q", part, path)
		}
		cur = next
	}
	return cur, nil
}
