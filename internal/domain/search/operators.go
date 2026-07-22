package search

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// MaxConditionDepth caps recursion in the condition validators
// (ValidateCondition, ValidateConditionValueTypes) to defend against stack
// exhaustion from deeply nested predicate trees. The HTTP parser
// (predicate.ParseCondition) already caps incoming requests at a smaller
// depth, but in-process callers (workflow engine criteria, programmatic
// constructions) bypass that cap and can otherwise pass an arbitrarily
// nested tree directly to the walkers. 256 is well above any realistic
// query nesting and well below the goroutine stack-blow threshold.
const MaxConditionDepth = 256

// canonicalOperators is the single source of truth for the valid
// `operatorType` values accepted by Simple / Lifecycle / Array conditions.
// The list is mirrored in cmd/cyoda/help/content/search.md and in the
// OpenAPI schema (api/generated.go `*OperatorType` enum values). Any
// change to one must be reflected in the others.
//
// The set must include every operator the runtime matcher
// (internal/match/operators.go) accepts — otherwise previously-valid
// requests that would have matched correctly in-memory are rejected at
// the API boundary. Issue #90 closed the "silently falls through to
// regex" gap at the default; the set must still admit every operator
// the system actually supports.
var canonicalOperators = map[string]struct{}{
	"EQUALS":            {},
	"NOT_EQUAL":         {},
	"GREATER_THAN":      {},
	"LESS_THAN":         {},
	"GREATER_OR_EQUAL":  {},
	"LESS_OR_EQUAL":     {},
	"CONTAINS":          {},
	"NOT_CONTAINS":      {},
	"STARTS_WITH":       {},
	"NOT_STARTS_WITH":   {},
	"ENDS_WITH":         {},
	"NOT_ENDS_WITH":     {},
	"LIKE":              {},
	"IS_NULL":           {},
	"NOT_NULL":          {},
	"BETWEEN":           {},
	"BETWEEN_INCLUSIVE": {},
	"MATCHES_PATTERN":   {},
	"IEQUALS":           {},
	"INOT_EQUAL":        {},
	"ICONTAINS":         {},
	"INOT_CONTAINS":     {},
	"ISTARTS_WITH":      {},
	"INOT_STARTS_WITH":  {},
	"IENDS_WITH":        {},
	"INOT_ENDS_WITH":    {},
}

// ValidateCondition walks a parsed condition tree and returns an error
// identifying any unknown operator. The returned error text lists the
// canonical set so callers can self-correct.
func ValidateCondition(cond predicate.Condition) error {
	return validateConditionAtDepth(cond, 0)
}

func validateConditionAtDepth(cond predicate.Condition, depth int) error {
	if cond == nil {
		return nil
	}
	if depth >= MaxConditionDepth {
		return fmt.Errorf("condition depth exceeded (max %d)", MaxConditionDepth)
	}
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		if err := validateOperator(c.OperatorType); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.LifecycleCondition:
		if err := validateOperator(c.OperatorType); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.ArrayCondition:
		// ArrayCondition doesn't carry an operator — each positional value
		// becomes an equality check in arrayToFilter. Nothing to validate.
		_ = c
		return nil
	case *predicate.GroupCondition:
		for _, child := range c.Conditions {
			if err := validateConditionAtDepth(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.FunctionCondition:
		// Function conditions are not operator-typed; nothing to check.
		return nil
	default:
		return nil
	}
}

func validateOperator(op string) error {
	if op == "" {
		return fmt.Errorf("missing operatorType; valid: %s", canonicalOperatorList())
	}
	if _, ok := canonicalOperators[op]; !ok {
		return fmt.Errorf("unknown operatorType %q; valid: %s", op, canonicalOperatorList())
	}
	return nil
}

// validateBetweenArity enforces that a BETWEEN / BETWEEN_INCLUSIVE
// condition's value is exactly a 2-element array (the [lo, hi] bounds pair).
// Model-independent and shared by SimpleCondition (data leaves) and
// LifecycleCondition (meta leaves, including temporal fields) — the same
// malformed-arity bug affects both.
//
// A scalar or a 1- or 3-element array previously slipped past validation:
// betweenValues (filter_translate.go) rejects anything but a 2-element []any
// and leaves spi.Filter.Values nil, and that nil-Values filter reaches the
// storage plugins with catastrophically divergent behavior — postgres
// panicked indexing f.Values[0] with no length guard, sqlite's BETWEEN
// fallback emitted a match-all "1=1", and only memory's spi.MatchFilter
// correctly excluded. Rejecting the malformed condition here, at the single
// validation boundary every transport (HTTP, gRPC) funnels through, closes
// the gap before any of that divergence can occur.
func validateBetweenArity(op string, value any) error {
	if op != "BETWEEN" && op != "BETWEEN_INCLUSIVE" {
		return nil
	}
	vals, ok := value.([]any)
	if !ok {
		return fmt.Errorf("operator %q requires exactly two operands, got %T", op, value)
	}
	if len(vals) != 2 {
		return fmt.Errorf("operator %q requires exactly two operands, got %d", op, len(vals))
	}
	return nil
}

// canonicalOperatorList returns a deterministic comma-separated list of
// canonical operators for inclusion in error responses.
func canonicalOperatorList() string {
	ops := make([]string, 0, len(canonicalOperators))
	for k := range canonicalOperators {
		ops = append(ops, k)
	}
	sort.Strings(ops)
	return strings.Join(ops, ", ")
}
