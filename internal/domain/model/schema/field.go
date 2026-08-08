package schema

import "sort"

// cachedFields holds the lazily-computed flat field view.
type cachedFields struct {
	list   []FieldDescriptor
	byPath map[string]FieldDescriptor
}

// Fields returns a flat list of all leaf fields, cached after first call.
func (n *ModelNode) Fields() []FieldDescriptor {
	if cached := n.fieldCache.Load(); cached != nil {
		return cached.list
	}
	cf := n.buildFieldCache()
	n.fieldCache.CompareAndSwap(nil, cf)
	return n.fieldCache.Load().list
}

// FieldsMap returns a map from path to FieldDescriptor, cached alongside Fields.
func (n *ModelNode) FieldsMap() map[string]FieldDescriptor {
	if cached := n.fieldCache.Load(); cached != nil {
		return cached.byPath
	}
	cf := n.buildFieldCache()
	n.fieldCache.CompareAndSwap(nil, cf)
	return n.fieldCache.Load().byPath
}

func (n *ModelNode) buildFieldCache() *cachedFields {
	var list []FieldDescriptor
	collectFields(n, "$", false, &list)
	// Sort for deterministic output
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })

	byPath := make(map[string]FieldDescriptor, len(list))
	for _, f := range list {
		byPath[f.Path] = f
	}
	return &cachedFields{list: list, byPath: byPath}
}

// concreteTypes returns ts's DataTypes with the NULL marker removed. A TypeSet
// is either the NULL-only marker or a set of concrete types (TypeSet.Add drops
// NULL when a concrete type is present), so this yields nil for a nil/empty set
// or a NULL-only set, and the concrete types otherwise. It backs the
// object-node self-leaf emit: only a genuine object-or-scalar polymorphism (a
// concrete scalar observed at an object path) becomes a searchable leaf; a bare
// nullable-object stays a pure container.
func concreteTypes(ts *TypeSet) []DataType {
	if ts == nil || ts.IsEmpty() {
		return nil
	}
	all := ts.Types()
	out := all[:0:0]
	for _, dt := range all {
		if dt == Null {
			continue
		}
		out = append(out, dt)
	}
	return out
}

// collectFields walks the ModelNode tree recursively, appending leaf descriptors.
func collectFields(n *ModelNode, prefix string, inArray bool, out *[]FieldDescriptor) {
	switch n.kind {
	case KindLeaf:
		*out = append(*out, FieldDescriptor{
			Path:    prefix,
			Types:   n.types.Types(), // returns a copy
			IsArray: inArray,
		})
	case KindObject:
		// A node observed as BOTH an object and a bare CONCRETE scalar (a
		// polymorphic object-or-scalar shape that Merge unions onto a KindObject
		// node) carries a concrete scalar TypeSet in addition to its children.
		// Emit a leaf descriptor for the object node's OWN path so the
		// scalar-valued observations are directly searchable with a scalar
		// operand — in ADDITION to recursing into its children below (the
		// object-valued observations are reached via the child leaves). A PURE
		// object (substructure only, empty TypeSet) emits no self descriptor:
		// it has no scalar to compare against. A NULL-only TypeSet is the
		// nullable marker, not a concrete scalar observation — it likewise emits
		// no self descriptor, so an object node that was merely also seen as
		// null stays a pure container (and, e.g., a unique key over it is still
		// rejected as non-scalar-leaf).
		if concrete := concreteTypes(n.types); len(concrete) > 0 {
			*out = append(*out, FieldDescriptor{
				Path:    prefix,
				Types:   concrete,
				IsArray: inArray,
			})
		}
		// Sort child keys for deterministic order.
		keys := make([]string, 0, len(n.children))
		for k := range n.children {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectFields(n.children[k], prefix+"."+k, false, out)
		}
	case KindArray:
		if n.element != nil {
			arrayPath := prefix + "[*]"
			maxW := 0
			if n.info != nil {
				maxW = n.info.MaxWidth()
			}
			if n.element.kind == KindLeaf {
				*out = append(*out, FieldDescriptor{
					Path:     arrayPath,
					Types:    n.element.types.Types(),
					IsArray:  true,
					MaxWidth: maxW,
				})
			} else {
				// For arrays of objects/arrays, recurse with the array path prefix.
				// The inArray flag is false for nested fields inside array objects.
				collectFields(n.element, arrayPath, false, out)
			}
		}
	}
}
