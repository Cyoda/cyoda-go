package schema

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// changeLevelRank maps each ChangeLevel to its position in the permission hierarchy.
// Higher rank means more permissive. Empty string maps to -1 (nothing allowed).
func changeLevelRank(level spi.ChangeLevel) int {
	switch level {
	case spi.ChangeLevelArrayLength:
		return 0
	case spi.ChangeLevelArrayElements:
		return 1
	case spi.ChangeLevelType:
		return 2
	case spi.ChangeLevelStructural:
		return 3
	default:
		return -1
	}
}

// levelPermits returns true if the configured level permits the required level.
func levelPermits(configured, required spi.ChangeLevel) bool {
	return changeLevelRank(configured) >= changeLevelRank(required)
}

// Extend merges incoming into existing, constrained by the given change level.
// If no changes are needed (incoming conforms to existing), the existing model is returned.
// If the incoming data requires a change that exceeds the permitted level, an error is returned.
func Extend(existing, incoming *ModelNode, level spi.ChangeLevel) (*ModelNode, error) {
	changed, err := checkAndExtend(existing, incoming, level, "", spi.ChangeLevelType)
	if err != nil {
		return nil, err
	}
	if !changed {
		return existing, nil
	}
	return Merge(existing, incoming), nil
}

// checkAndExtend walks both trees comparing the SET of kinds each node
// declares. It returns (true, nil) if changes are needed and permitted,
// (false, nil) if no changes are needed, or (false, error) if a change is
// forbidden.
//
// The gate is a subset test. Every kind the incoming payload carries that the
// model already declares is, by definition, a shape the model admits — so the
// walk descends into that branch and asks only whether its CONTENTS grew.
// Comparing one kind per path instead made a model with a multi-kind field
// refuse half of its own declared data.
//
// scalarLevel is the level a scalar-shaped change costs at this position:
// ChangeLevelType normally, ChangeLevelArrayElements directly on an array's
// element. It is preserved through nested array levels and reset when
// descending into an object's children, which is exactly where the element
// rules stopped applying before.
func checkAndExtend(existing, incoming *ModelNode, level spi.ChangeLevel, path string, scalarLevel spi.ChangeLevel) (bool, error) {
	if existing == nil || incoming == nil {
		return false, nil
	}

	changed := false

	// A path observed as null where the model has no scalar declaration adds
	// the nullable marker, which is a type-level change. A node that already
	// declares a scalar admits null without recording anything.
	if incoming.Nullable() && !existing.Nullable() && existing.Scalar() == nil {
		if !levelPermits(level, scalarLevel) {
			return false, fmt.Errorf("nullable marker at %s requires %s level, but level is %q",
				displayPath(path), scalarLevel, level)
		}
		changed = true
	}

	for _, k := range incoming.Kinds() {
		if existing.Branch(k) == nil {
			// A node that declares NO kind has never been observed as
			// anything, so establishing its first kinds is the
			// nullable-marker promotion, which keeps the level it has always
			// had. Adding a kind to a node that already declares one is a new
			// branch: a union, and strictly more fundamental than a new field.
			required := spi.ChangeLevelStructural
			if len(existing.Kinds()) == 0 {
				required = scalarLevel
			}
			if !levelPermits(level, required) {
				return false, fmt.Errorf("new %s branch at %s requires %s level, but level is %q",
					k, displayPath(path), required, level)
			}
			changed = true
			continue
		}

		branchChanged, err := checkBranch(k, existing, incoming, level, path, scalarLevel)
		if err != nil {
			return false, err
		}
		if branchChanged {
			changed = true
		}
	}

	return changed, nil
}

// checkBranch compares one branch both nodes declare.
func checkBranch(k NodeKind, existing, incoming *ModelNode, level spi.ChangeLevel, path string, scalarLevel spi.ChangeLevel) (bool, error) {
	switch k {
	case KindLeaf:
		if !widensDeclared(incoming.Scalar().Types(), existing.Scalar().Types()) {
			return false, nil
		}
		if !levelPermits(level, scalarLevel) {
			return false, fmt.Errorf("type change at %s requires %s level, but level is %q",
				displayPath(path), scalarLevel, level)
		}
		return true, nil

	case KindObject:
		changed := false
		for name, inChild := range incoming.Object().Children() {
			childPath := path + "." + name
			exChild := existing.Object().Child(name)
			if exChild == nil {
				if !levelPermits(level, spi.ChangeLevelStructural) {
					return false, fmt.Errorf("new field %q at %s requires STRUCTURAL level, but level is %q",
						name, displayPath(childPath), level)
				}
				changed = true
				continue
			}
			// Children of an object are ordinary positions again: the array
			// element rules do not reach through a nested object.
			childChanged, err := checkAndExtend(exChild, inChild, level, childPath, spi.ChangeLevelType)
			if err != nil {
				return false, err
			}
			if childChanged {
				changed = true
			}
		}
		return changed, nil

	case KindArray:
		changed := false
		exArr, inArr := existing.Array(), incoming.Array()

		switch {
		case exArr.Element() == nil && inArr.Element() != nil:
			// The array was observed, but never with any content, so it
			// declares no element type at all. Learning one is the same
			// promotion a node that declares no kind undergoes, at the level
			// an array element's changes cost. Without this the model kept
			// declaring nothing here and silently never learned the type.
			if !levelPermits(level, spi.ChangeLevelArrayElements) {
				return false, fmt.Errorf("array element type at %s requires ARRAY_ELEMENTS level, but level is %q",
					displayPath(path), level)
			}
			changed = true

		case exArr.Element() != nil && inArr.Element() != nil:
			// The element of an array is where ARRAY_ELEMENTS applies, and it
			// keeps applying through further array levels.
			elemChanged, err := checkAndExtend(exArr.Element(), inArr.Element(), level,
				path+"[]", spi.ChangeLevelArrayElements)
			if err != nil {
				return false, err
			}
			if elemChanged {
				changed = true
			}
		}

		if inArr.MaxWidth() > exArr.MaxWidth() {
			if !levelPermits(level, spi.ChangeLevelArrayLength) {
				return false, fmt.Errorf("array width change at %s requires ARRAY_LENGTH level, but level is %q",
					displayPath(path), level)
			}
			changed = true
		}
		return changed, nil
	}
	return false, nil
}

// widensDeclared reports whether any incoming type is outside what the leaf
// already admits — the only thing that makes a scalar observation a change.
//
// Admission is assignability, not label equality. A leaf declaring DOUBLE
// already admits a whole number in the INTEGER range: the walker classifies it
// INTEGER by value alone (it cannot see the declaration), and INTEGER widens
// into DOUBLE, so Merge collapses it straight back to [DOUBLE] and Validate
// accepts it. Asking for a plain set difference here made that write spend a
// TYPE-level permission to reach a model identical to the one it started from,
// which refused every such write to a DOUBLE leaf below TYPE level. The
// lattice is what bounds this, not "whole number": LONG does not widen into
// DOUBLE, so a larger value is a genuine change and still costs.
//
// This shares its predicate with matchesScalarBranch on the validate path, so
// the two write doors cannot disagree about what a leaf accepts. TypeSet.Add
// answers the merge side by a different route (CollapseNumeric), and the two
// agree exactly rather than by construction — the equivalence is a property to
// preserve, not a structural guarantee. It rests on the leaf's TypeSet being
// normalised: at most one numeric member, and never NULL beside a concrete
// type. If that ever stopped holding, this gate would under-report changes
// where the set difference did not.
func widensDeclared(incoming, declared []DataType) bool {
	for _, dt := range incoming {
		if !assignableToAny(dt, declared) {
			return true
		}
	}
	return false
}
