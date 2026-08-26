package search

import (
	"fmt"
	"regexp"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// ValidateRegexPatterns walks a condition tree and compiles every
// MATCHES_PATTERN pattern it finds (on both SimpleCondition and
// LifecycleCondition — lifecycleToFilter pushes MATCHES_PATTERN down via the
// identical spi.FilterMatchesRegex path, so it carries the same exposure),
// rejecting the request before the filter tree is built if any pattern
// fails to compile.
//
// This closes a fail-open regression: the plugin residual filter evaluators
// delegate to the error-free spi.PreparedFilter.Match kernel, so a malformed
// pattern that reaches the matcher no longer surfaces an error — it
// silently returns under-inclusive (or, for sqlite/postgres, previously an
// opaque 500) results. Rejecting here, in the backend-independent domain
// layer, makes every backend (memory/sqlite/postgres) reject identically
// with 400 INVALID_CONDITION, before any store or plugin code runs.
//
// This validator compiles the pattern bare (regexp.Compile(pattern)); the
// kernel compiles it anchored — regexp.Compile(anchor(pattern)), i.e.
// `\A(?:pattern)\z` — inside ExpandLeaf (cyoda-go-spi eval_leaf.go). The two
// compile calls are NOT guaranteed to agree, and the skew runs both ways.
// Reject-though-valid: the anchor wrapper's own parentheses can rebalance a
// pattern whose parens are unmatched on their own (e.g. ")|(" fails bare but
// compiles once wrapped), so a narrow class of patterns is rejected here even
// though the kernel would accept them. Accept-then-fail — the more serious
// direction, and the one this validator exists to prevent — an unterminated
// \Q...\E literal-quote escape (e.g. the bare pattern "\Q") compiles fine
// here, but the wrapper's appended `)\z` gets swallowed by that same
// unterminated \Q, so ExpandLeaf's anchored compile fails: the client gets a
// 200 and a job id, and the job then fails. Backends also disagree on that
// failure — the in-tree evaluators leave the compiled regex nil and return
// an empty (non-matching) result, while the commercial backend's async
// evaluator propagates the compile error and fails the job. The resolution
// is decided, not open: the validator should derive its accept/reject set from
// the kernel, since anchoring is the correct full-value-match semantics
// (mirroring Cloud's Pattern.matcher(x).matches()) and every evaluator —
// including the commercial one — already applies it; this validator is the
// outlier.
//
// The blocker that kept that fix out of this package is gone. cyoda-go-spi now
// EXPORTS the derivation — ValidateLeafPattern, ValidateConditionPatterns and
// the ErrInvalidPattern sentinel — precisely so a boundary validator asks the
// kernel instead of hand-rolling a second copy of the grammar. Replacing the
// bare compile below with one ValidateConditionPatterns call is the pending
// step, and it covers LIKE as well: LIKE is a glob (see the kernel's
// like_pattern.go), it is NOT validated here at all today, and a malformed LIKE
// operand therefore reaches the evaluator, where Prepare's contract turns it
// into a leaf that never matches — a 200 and an empty page where a 400 belongs.
func ValidateRegexPatterns(cond predicate.Condition) error {
	return walkRegexPatterns(cond, 0)
}

func walkRegexPatterns(cond predicate.Condition, depth int) error {
	if cond == nil {
		return nil
	}
	if depth >= MaxConditionDepth {
		return fmt.Errorf("condition depth exceeded (max %d)", MaxConditionDepth)
	}
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		if c.OperatorType != "MATCHES_PATTERN" {
			return nil
		}
		return compileRegexPattern(c.Value)
	case *predicate.LifecycleCondition:
		if c.OperatorType != "MATCHES_PATTERN" {
			return nil
		}
		return compileRegexPattern(c.Value)
	case *predicate.GroupCondition:
		for _, child := range c.Conditions {
			if err := walkRegexPatterns(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.ArrayCondition, *predicate.FunctionCondition:
		return nil
	default:
		return nil
	}
}

// compileRegexPattern derives the pattern the same way the kernel does —
// fmt.Sprintf("%v", value) — but compiles it bare, not anchored the way
// ExpandLeaf does; see the accept/reject-skew note on ValidateRegexPatterns
// above. The returned error is regexp.Compile's own (e.g. "error parsing
// regexp: ...") so callers can format it into their own message without
// double-wrapping.
func compileRegexPattern(value any) error {
	pattern := fmt.Sprintf("%v", value)
	_, err := regexp.Compile(pattern)
	return err
}
