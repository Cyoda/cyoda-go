package schema

import (
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrPolymorphicSlot is returned by Extend when the incoming payload
// carries a different node kind at the same path as the registered schema
// (LEAF vs OBJECT, OBJECT vs ARRAY, etc.).
//
// It is about CREATING such a slot, not about having one. A model may declare
// several kinds for a path — sample-data import merges them, Validate admits
// every declared kind, and the exports name each branch. What a schema
// extension cannot do is add a kind to a path, at any ChangeLevel: Extend
// compares one kind per path, so it also refuses a write matching a declared
// but non-dominant branch. That is why a model with multi-kind fields is used
// with ChangeLevel unset.
//
// The sentinel lets handler layers distinguish polymorphic-slot rejections
// — which the caller cannot resolve by raising ChangeLevel — from genuine
// change-level violations (new field at TYPE, widening at ArrayLength,
// etc.) so the user-facing error message is not misleading.
//
// The LEAF[NULL] nullable-marker path is NOT classified as a polymorphic
// slot: Extend accepts LEAF[NULL] against an existing ARRAY/OBJECT (and
// vice versa) as a nullable marker, matching the Diff/Apply broaden_type
// contract.
//
// TODO(#85): decommission this sentinel + common.ErrCodePolymorphicSlot +
// the isNullOnlyLeaf carve-outs in checkAndExtend/checkElementWidening
// once Sub-project A.3 lands Cloud-parity polymorphic-slot semantics.
// Grep this identifier to find all transitional surfaces that must go.
var ErrPolymorphicSlot = errors.New("a schema extension cannot create a polymorphic slot")

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
	changed, err := checkAndExtend(existing, incoming, level, "")
	if err != nil {
		return nil, err
	}
	if !changed {
		return existing, nil
	}
	return Merge(existing, incoming), nil
}

// checkAndExtend walks both trees recursively comparing nodes.
// It returns (true, nil) if changes are needed and permitted,
// (false, nil) if no changes are needed, or (false, error) if a change is forbidden.
func checkAndExtend(existing, incoming *ModelNode, level spi.ChangeLevel, path string) (bool, error) {
	if existing == nil || incoming == nil {
		return false, nil
	}

	// Kind mismatches are not additive — Diff already documents this contract.
	// Silently accepting them would let Merge union TypeSets across kinds,
	// producing e.g. an OBJECT node carrying primitive DataTypes, which
	// violates the OBJECT-only-NULL invariant that Apply enforces at replay.
	//
	// Exception: a LEAF carrying ONLY {NULL} is a nullable marker. JSON null
	// against an existing ARRAY/OBJECT path (or an existing null slot later
	// seeing a concrete array/object) is a legitimate TYPE-level change —
	// Merge promotes to the non-LEAF kind and Union adds NULL to the target's
	// TypeSet. The exception is strictly null-only: LEAF carrying any other
	// primitive type is still a genuine kind mismatch and is rejected.
	if existing.Kind() != incoming.Kind() {
		if isNullOnlyLeaf(existing) || isNullOnlyLeaf(incoming) {
			if !levelPermits(level, spi.ChangeLevelType) {
				return false, fmt.Errorf("nullable marker at %s requires TYPE level, but level is %q", path, level)
			}
			return true, nil
		}
		return false, fmt.Errorf("%w at %q: the model declares %s here, the payload carries %s. Send the declared kind, or re-establish the model from sample data covering both kinds while it is UNLOCKED (a model may declare several kinds for a path; leave changeLevel unset to write them)",
			ErrPolymorphicSlot, path, existing.Kind(), incoming.Kind())
	}

	changed := false

	// Check children: new fields in incoming that don't exist in existing
	for name, inChild := range incoming.Children() {
		childPath := path + "." + name
		exChild := existing.Child(name)
		if exChild == nil {
			// New field — requires STRUCTURAL
			if !levelPermits(level, spi.ChangeLevelStructural) {
				return false, fmt.Errorf("new field %q at %s requires STRUCTURAL level, but level is %q", name, childPath, level)
			}
			changed = true
			continue
		}

		// Both exist — recurse
		childChanged, err := checkAndExtend(exChild, inChild, level, childPath)
		if err != nil {
			return false, err
		}
		if childChanged {
			changed = true
		}
	}

	// Check leaf type widening: both are leaves with different types
	if existing.Kind() == KindLeaf && incoming.Kind() == KindLeaf {
		if !existing.Types().Equal(incoming.Types()) {
			if !levelPermits(level, spi.ChangeLevelType) {
				return false, fmt.Errorf("type change at %s requires TYPE level, but level is %q", path, level)
			}
			changed = true
		}
	}

	// Check array-specific changes
	if existing.Kind() == KindArray && incoming.Kind() == KindArray {
		// Element type widening
		if existing.Element() != nil && incoming.Element() != nil {
			elemChanged, err := checkElementWidening(existing.Element(), incoming.Element(), level, path)
			if err != nil {
				return false, err
			}
			if elemChanged {
				changed = true
			}
		}

		// Array width change
		if existing.Info() != nil && incoming.Info() != nil {
			if incoming.Info().MaxWidth() > existing.Info().MaxWidth() {
				if !levelPermits(level, spi.ChangeLevelArrayLength) {
					return false, fmt.Errorf("array width change at %s requires ARRAY_LENGTH level, but level is %q", path, level)
				}
				changed = true
			}
		}
	}

	return changed, nil
}

// isNullOnlyLeaf returns true when n is a LEAF whose TypeSet contains
// exactly {NULL}. Such a node is a nullable marker: merging it against a
// concrete ARRAY/OBJECT promotes to the concrete kind and adds NULL to
// the target's TypeSet via Union. Guards the LEAF[NULL] exception in
// checkAndExtend against genuine kind conflicts (LEAF[primitive] vs
// OBJECT/ARRAY), which must still reject.
func isNullOnlyLeaf(n *ModelNode) bool {
	if n == nil || n.Kind() != KindLeaf {
		return false
	}
	types := n.Types().Types()
	return len(types) == 1 && types[0] == Null
}

// checkElementWidening checks if array element types differ and whether the change is permitted.
func checkElementWidening(existingElem, incomingElem *ModelNode, level spi.ChangeLevel, path string) (bool, error) {
	// Kind mismatches between array elements carry the same contract as at
	// the root (checkAndExtend): reject unless one side is a LEAF[NULL]
	// nullable marker, which Merge promotes to the concrete kind. Without
	// this check, kind-mismatched elements silently passed through and
	// Merge absorbed them into the existing kind — losing the incoming
	// element's type information entirely.
	if existingElem.Kind() != incomingElem.Kind() {
		if isNullOnlyLeaf(existingElem) || isNullOnlyLeaf(incomingElem) {
			if !levelPermits(level, spi.ChangeLevelArrayElements) {
				return false, fmt.Errorf("nullable marker on array element at %s requires ARRAY_ELEMENTS level, but level is %q", path, level)
			}
			return true, nil
		}
		return false, fmt.Errorf("%w at %s[]: the model declares %s elements here, the payload carries %s. Send the declared kind, or re-establish the model from sample data covering both kinds while it is UNLOCKED",
			ErrPolymorphicSlot, path, existingElem.Kind(), incomingElem.Kind())
	}

	// For leaf elements, check type widening at the ARRAY_ELEMENTS level
	if existingElem.Kind() == KindLeaf && incomingElem.Kind() == KindLeaf {
		if !existingElem.Types().Equal(incomingElem.Types()) {
			if !levelPermits(level, spi.ChangeLevelArrayElements) {
				return false, fmt.Errorf("array element type change at %s requires ARRAY_ELEMENTS level, but level is %q", path, level)
			}
			return true, nil
		}
	}

	// For object elements, recurse into children
	if existingElem.Kind() == KindObject && incomingElem.Kind() == KindObject {
		return checkAndExtend(existingElem, incomingElem, level, path+"[]")
	}

	// For array-of-array elements, recurse into the inner element.
	if existingElem.Kind() == KindArray && incomingElem.Kind() == KindArray {
		if existingElem.Element() != nil && incomingElem.Element() != nil {
			return checkElementWidening(existingElem.Element(), incomingElem.Element(), level, path+"[]")
		}
	}

	return false, nil
}
