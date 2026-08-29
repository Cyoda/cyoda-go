package schema

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Apply replays the opaque SchemaDelta bytes onto base, returning a
// new *ModelNode. The same function is used by plugins (via factory
// injection) to fold the extension log on Get, and by tests to verify
// commutativity and validation-monotonicity.
//
// Apply does not mutate base — a fresh tree is produced via the
// codec's Marshal/Unmarshal round-trip. Note that this round-trip
// drops the observed array widths, which the persistence format does
// not carry.
//
// base must be non-nil. An empty delta yields a clean clone of base.
func Apply(base *ModelNode, delta spi.SchemaDelta) (*ModelNode, error) {
	if base == nil {
		return nil, fmt.Errorf("schema.Apply: base is nil")
	}

	root, err := cloneNode(base)
	if err != nil {
		return nil, fmt.Errorf("schema.Apply: clone base: %w", err)
	}

	if len(delta) == 0 {
		return root, nil
	}

	ops, err := UnmarshalDelta(delta)
	if err != nil {
		return nil, fmt.Errorf("schema.Apply: decode delta: %w", err)
	}

	for i, op := range ops {
		if err := applyOp(root, op); err != nil {
			return nil, fmt.Errorf("schema.Apply: op %d (%s %q): %w", i, op.Kind, op.Path, err)
		}
	}
	return root, nil
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
	// broaden_type widens the target node's own TypeSet. For LEAF
	// targets this widens the primitive data types; for OBJECT/ARRAY
	// targets it adds nullable markers (typically NULL). Both
	// semantics are additive and handled identically by TypeSet.Add.
	types, err := DecodeTypeNames(op.Payload)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	// A node carrying a scalar branch — or none at all, the nullable marker,
	// where one is being established — takes concrete types. Anywhere else the
	// only meaningful addition is NULL: adding a scalar KIND to a node that
	// already declares another is a branch addition, which has its own op.
	if target.Scalar() == nil && len(target.Kinds()) > 0 {
		for _, dt := range types {
			if dt != Null {
				return fmt.Errorf("broaden_type on a %s target at %q may only add NULL, got %s",
					kindNames(target), op.Path, dt)
			}
		}
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
	// The op widens the element's SCALAR types, so what it needs is a scalar
	// branch — not the absence of every other branch. An element observed as
	// both a scalar and a container still takes the widening, and must: ops
	// commute, so the branch that made it a union may well have been replayed
	// first. Only an element that declares a kind but no scalar is refused,
	// because giving it one would be adding a branch, which has its own op.
	if elem.Scalar() == nil && len(elem.Kinds()) > 0 {
		return fmt.Errorf("array element at %q declares %s and no scalar", op.Path, kindNames(elem))
	}
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
		target.ObserveArrayWidth(a.MaxWidth())
	}
	return nil
}

// resolvePath walks root along a slash-separated path of child names.
// The empty path returns root. A missing segment returns a descriptive
// error rather than a nil node — Apply surfaces the segment name so
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

// cloneNode produces an independent copy of node via the codec
// round-trip. Observed array widths are not preserved (mirrors the
// persistence format).
func cloneNode(node *ModelNode) (*ModelNode, error) {
	raw, err := Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	out, err := Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}
