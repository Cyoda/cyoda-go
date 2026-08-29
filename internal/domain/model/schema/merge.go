package schema

// Merge combines two ModelNode trees into a new tree.
// Both inputs are consumed and must not be used after the call (Ownership Rule 7).
// When one input is nil, the non-nil input is returned directly. The caller must
// not retain a separate reference to the input, per Ownership Rule 7.
// Returns nil if both inputs are nil.
//
// A node holds the set of kinds it has been observed as, so merging is set
// union: each branch merges with its counterpart, and a branch only one side
// carries is kept. There is no tiebreak to make — the predecessor had to pick
// one label when two kinds met, and the branch it did not name was invisible to
// every reader that dispatched on the label, though the payload held it all
// along. Union is also commutative by construction rather than by accident.
func Merge(a, b *ModelNode) *ModelNode {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	result := &ModelNode{}

	if as, bs := a.Scalar(), b.Scalar(); as != nil || bs != nil {
		result.DeclareKind(KindLeaf)
		if as != nil {
			result.AddScalarTypes(as.Types()...)
		}
		if bs != nil {
			result.AddScalarTypes(bs.Types()...)
		}
	}

	if ao, bo := a.Object(), b.Object(); ao != nil || bo != nil {
		result.DeclareKind(KindObject)
		if ao != nil {
			for name, child := range ao.Children() {
				result.SetChild(name, child)
			}
		}
		if bo != nil {
			for name, child := range bo.Children() {
				if existing := result.Object().Child(name); existing != nil {
					child = Merge(existing, child)
				}
				result.SetChild(name, child)
			}
		}
	}

	if aa, ba := a.Array(), b.Array(); aa != nil || ba != nil {
		result.DeclareKind(KindArray)
		var elem *ModelNode
		width := 0
		if aa != nil {
			elem = aa.Element()
			width = aa.MaxWidth()
		}
		if ba != nil {
			elem = Merge(elem, ba.Element())
			if w := ba.MaxWidth(); w > width {
				width = w
			}
		}
		if elem != nil {
			result.SetElement(elem)
		}
		result.ObserveArrayWidth(width)
	}

	// A concrete scalar observation on either side collapses the marker, which
	// AddScalarTypes has already applied; SetNullable is a no-op there.
	if a.Nullable() || b.Nullable() {
		result.SetNullable()
	}

	return result
}
