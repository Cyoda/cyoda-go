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
// The compile call MUST mirror the kernel's own pattern derivation, so
// validation accepts exactly the patterns evaluation can run (no accept/reject
// skew). The kernel anchors the pattern and compiles it inside ExpandLeaf
// (cyoda-go-spi eval_leaf.go); this validator must apply the same anchoring to
// the same input.
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

// compileRegexPattern mirrors the kernel's pattern derivation and compile
// call: fmt.Sprintf("%v", value) then regexp.Compile. The returned error is
// regexp.Compile's own (e.g. "error parsing regexp: ...") so callers can
// format it into their own message without double-wrapping.
func compileRegexPattern(value any) error {
	pattern := fmt.Sprintf("%v", value)
	_, err := regexp.Compile(pattern)
	return err
}
