package search

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// ValidatePatterns rejects a condition carrying a pattern operand the kernel
// cannot evaluate — MATCHES_PATTERN and LIKE alike — before the filter tree is
// built. It walks SimpleCondition and LifecycleCondition leaves; a lifecycle
// leaf is pushed down through the identical FilterMatchesRegex/FilterLike path
// and carries the identical exposure.
//
// Left unvalidated, an uncompilable operand does not surface as an error:
// Prepare's contract makes it a leaf that never matches, so the caller gets a
// 200 and an empty page — and the backends disagree about it, the in-tree
// evaluators returning empty where the commercial async evaluator fails the
// job. Rejecting here, in the backend-independent domain layer, makes every
// backend reject identically with 400 INVALID_CONDITION before any store or
// plugin code runs.
//
// The accept/reject set is the kernel's own, not a second copy of the grammar:
// spi.ValidateConditionPatterns routes through compileLeafPattern, the single
// derivation ExpandLeaf itself evaluates with. Do not hand-roll the anchor
// wrapper (`\A(?:pattern)\z`) here — two derivations in two repositories is
// precisely the skew this call removes, in which a bare compile accepted an
// operand the anchored compile could not parse (an unterminated `\Q` swallows
// the appended `)\z`), so the client was told at the boundary they would not
// hit an error they then hit.
func ValidatePatterns(cond predicate.Condition) error {
	return spi.ValidateConditionPatterns(cond)
}
