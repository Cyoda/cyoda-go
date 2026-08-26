package search

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// ErrInvalidCondition is the sentinel for a condition that fails structural
// (shape) validation — currently, an operand that is an object/map rather
// than a scalar or array. An object denotes no scalar value any operator
// (comparison, string, range, or null-presence) could evaluate, so it is
// rejected as INVALID_CONDITION (spec §6/§8: a shape/arity error, not a
// type mismatch) regardless of the operator or the field's declared types —
// unlike CONDITION_TYPE_MISMATCH, which requires a known field with declared
// types to compare against. Handlers check errors.Is(err, ErrInvalidCondition)
// to emit HTTP 400 with the INVALID_CONDITION code, mirroring
// validateBetweenArity's arity rejection.
var ErrInvalidCondition = errors.New("invalid condition")

// MaxConditionDepth caps recursion in the condition validators
// (ValidateCondition, ValidateConditionValueTypes) to defend against stack
// exhaustion from deeply nested predicate trees. The HTTP parser
// (predicate.ParseCondition) already caps incoming requests at a smaller
// depth, but in-process callers (workflow engine criteria, programmatic
// constructions) bypass that cap and can otherwise pass an arbitrarily
// nested tree directly to the walkers. 256 is well above any realistic
// query nesting and well below the goroutine stack-blow threshold.
//
// Taken from the kernel rather than restated, so the boundary's walkers and
// the SPI's own (ValidateConditionOperators, ValidateConditionPatterns) cannot
// drift to different caps.
const MaxConditionDepth = spi.MaxConditionDepth

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
// identifying any unknown operator, malformed operand shape/arity, or
// jsonPath that is not JSON Path nomenclature. The returned error text lists
// the canonical set (operators) or the offending path and reason so callers
// can self-correct.
//
// A jsonPath failure wraps [ErrInvalidFieldPath] — see
// [ValidateConditionJSONPath] for the grammar and for why array-subscripted
// paths are deliberately accepted here. Callers classify it via
// [StructuralConditionErrCode], which maps it to
// INVALID_FIELD_PATH rather than the coarser INVALID_CONDITION/BAD_REQUEST.
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
		if err := ValidateConditionJSONPath(c.JsonPath); err != nil {
			return err
		}
		if err := validateOperator(c.OperatorType); err != nil {
			return err
		}
		if err := validateOperandShape(c.Value); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.LifecycleCondition:
		if err := validateOperator(c.OperatorType); err != nil {
			return err
		}
		if err := validateOperandShape(c.Value); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.ArrayCondition:
		// ArrayCondition doesn't carry an operator — each positional value
		// becomes an equality check in arrayToFilter. Its jsonPath goes through
		// the same wire grammar as a SimpleCondition's (arrayToFilter strips the
		// leader with the identical helper), so it is validated identically.
		return ValidateConditionJSONPath(c.JsonPath)
	case *predicate.GroupCondition:
		// A group operator other than exactly "AND"/"OR" previously cleared
		// validation and then behaved differently depending on which
		// execution path the query took: spi.ConditionToFilter's
		// groupToFilter maps anything non-"OR" (matched case-insensitively)
		// to FilterAnd, silently answering 200 with the wrong rows, while
		// match.Prepare requires exactly "AND"/"OR" and returns a bare
		// "unknown group operator" error that surfaces as a 500 on a
		// client-supplied condition. Reject it here — the one boundary every
		// search-shaped entry point funnels through — the same way the
		// FunctionCondition arm below closes its own 500-on-client-input
		// class. Case-sensitive: the predicate parser and match.Prepare both
		// require uppercase, so lowercase "or" is rejected too rather than
		// preserved to match the pushdown translator's looser check.
		if c.Operator != "AND" && c.Operator != "OR" {
			return fmt.Errorf("%w: unknown group operator %q; valid: AND, OR", ErrInvalidCondition, c.Operator)
		}
		for _, child := range c.Conditions {
			if err := validateConditionAtDepth(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.FunctionCondition:
		// A FUNCTION clause is a criterion shape, not a search shape. The
		// workflow engine intercepts it in evaluateCriterion and dispatches it
		// to a compute member; search has no dispatcher, ConditionToFilter
		// cannot translate it, and match.Prepare has no evaluator for it. Reject
		// it here — the one boundary every search-shaped entry point funnels
		// through — rather than letting it reach the evaluator and surface as a
		// 500 on a client-supplied condition.
		return fmt.Errorf("%w: function conditions are not supported in search; "+
			"they are only valid as workflow or transition criteria", ErrInvalidCondition)
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

// validateOperandShape rejects an operand that is an object (map[string]any)
// for any operator — a SimpleCondition or LifecycleCondition leaf's value is
// always a scalar, a null, or an array (BETWEEN's [lo, hi] pair, or a
// legacy/IN-style positional set); an object denotes no such value and would
// otherwise slip past a bare parse-based type check (a map stringifies via
// fmt.Sprint into something that wrongly "parses" as a STRING field's
// operand). This is a shape/arity error — spec §6/§8 classify it as
// INVALID_CONDITION, not CONDITION_TYPE_MISMATCH, and unlike the type check
// it applies uniformly to every operator, not just the comparison/range
// family, and regardless of whether the field/model is known.
func validateOperandShape(value any) error {
	if _, isObj := value.(map[string]any); isObj {
		return fmt.Errorf("condition value is an object, which is not a valid operand: %w", ErrInvalidCondition)
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
// fallback emitted a match-all "1=1", and only memory's
// spi.Prepare/PreparedFilter.Match correctly excluded. Rejecting the
// malformed condition here, at the single validation boundary every
// transport (HTTP, gRPC) funnels through, closes the gap before any of that
// divergence can occur.
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
