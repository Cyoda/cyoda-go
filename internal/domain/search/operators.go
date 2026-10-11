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
// (shape) validation: an unknown or missing operatorType, an operand that is
// an object/map, an array operand under an operator other than BETWEEN /
// BETWEEN_INCLUSIVE, a BETWEEN operand that is not a two-element pair of
// non-null bounds, an unknown group operator or a NOT group without exactly
// one child, and an array clause that is malformed or tests nothing. An object denotes no scalar value any
// operator could evaluate, so it is rejected as INVALID_CONDITION (spec
// §6/§8: a shape/arity error, not a type mismatch) regardless of the operator
// or the field's declared types — unlike CONDITION_TYPE_MISMATCH, which
// requires a known field with declared types to compare against. Handlers
// check errors.Is(err, ErrInvalidCondition) to emit HTTP 400 with the
// INVALID_CONDITION code.
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
// Built from [spi.OperatorNames] rather than a second, hand-maintained
// literal: the SPI already exports its operator table for exactly this
// reason (see that function's doc comment), and an independent copy here is
// the same drift class path-grammar.md and operator-semantics.md exist to
// close everywhere else — nothing would notice the two silently diverging.
// TestCanonicalOperators_MatchesSPI pins this by construction.
//
// The set must include every operator the runtime matcher
// (internal/match/operators.go) accepts — otherwise previously-valid
// requests that would have matched correctly in-memory are rejected at
// the API boundary. Rejecting at the boundary closed the "silently falls through to
// regex" gap at the default; the set must still admit every operator
// the system actually supports.
var canonicalOperators = func() map[string]struct{} {
	names := spi.OperatorNames()
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}()

// ValidateCondition walks a parsed condition tree and returns an error
// identifying any unknown operator, malformed operand shape/arity, or
// jsonPath that is not JSON Path nomenclature. The returned error text lists
// the canonical set (operators) or the offending path and reason so callers
// can self-correct.
//
// A jsonPath failure wraps [ErrInvalidFieldPath] — see
// [ValidateConditionJSONPath] for the grammar and for why array-subscripted
// paths are deliberately accepted here. Callers classify it via
// the structural classifier in [ValidateConditionForModel], which maps it to
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
		if err := validateOperandShape(c.OperatorType, c.Value); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.LifecycleCondition:
		if err := validateOperator(c.OperatorType); err != nil {
			return err
		}
		if err := validateOperandShape(c.OperatorType, c.Value); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.ArrayCondition:
		// The clause as a whole: its path names the elements of one array.
		if err := ValidateArrayClauseJSONPath(c.JsonPath); err != nil {
			return err
		}
		// Its meaning is its desugared leaves (spi.DesugarCondition); each is
		// held to the simple-clause rules.
		d, err := desugarArrayClause(c)
		if err != nil {
			return err
		}
		return validateConditionAtDepth(d, depth+1)
	case *predicate.GroupCondition:
		// A group operator other than exactly "AND"/"OR"/"NOT" previously
		// cleared validation and then behaved differently depending on which
		// execution path the query took: spi.ConditionToFilter's
		// groupToFilter maps anything non-"OR" (matched case-insensitively)
		// to FilterAnd, silently answering 200 with the wrong rows, while
		// match.Prepare requires exactly one of the closed set and returns a
		// bare "unknown group operator" error that surfaces as a 500 on a
		// client-supplied condition. Reject it here — the one boundary every
		// search-shaped entry point funnels through — the same way the
		// FunctionCondition arm below closes its own 500-on-client-input
		// class. Case-sensitive: the predicate parser and match.Prepare both
		// require uppercase, so lowercase "or"/"not" is rejected too rather
		// than preserved to match the pushdown translator's looser check.
		// validateGroupOperator also enforces NOT's arity-exactly-one
		// contract — see its doc for why zero or two-or-more is rejected the
		// same way, rather than left for match.Prepare/groupToFilter to
		// stumble on downstream.
		if err := validateGroupOperator(c.Operator, len(c.Conditions)); err != nil {
			return err
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

// ValidateCriterionCondition performs the operator, operand-shape and
// BETWEEN-arity checks ValidateCondition performs on a search condition —
// WITHOUT the FunctionCondition rejection, because a criterion legitimately
// carries a FUNCTION clause: the workflow engine dispatches it to a compute
// member at evaluation time rather than evaluating it as a search
// predicate (see the FunctionCondition arm of validateConditionAtDepth for
// why search itself must reject it).
//
// Shares canonicalOperators/validateOperator/validateOperandShape/
// validateBetweenArity with ValidateCondition — one operator table for both
// entry points, per operator-semantics.md §4: "An operator name outside
// this set is 400 INVALID_CONDITION, on every surface that carries a
// condition, workflow import included." structuralConditionErrCode
// classifies an unknown or missing operator from either entry point
// identically to the other structural condition failures (object or array
// operand shape, malformed BETWEEN arity) this file already routes to
// INVALID_CONDITION, rather than falling through to a coarser BAD_REQUEST.
//
// Path grammar is deliberately not checked here. workflow.walkCriterion runs
// its own path check first (ValidateConditionJSONPath / lifecycle field
// check), because a criterion accepts a path shape (the wildcard subscript)
// a search condition's scalar surfaces do not — see path-grammar.md §7.
func ValidateCriterionCondition(cond predicate.Condition) error {
	return validateCriterionConditionAtDepth(cond, 0)
}

func validateCriterionConditionAtDepth(cond predicate.Condition, depth int) error {
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
		if err := validateOperandShape(c.OperatorType, c.Value); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.LifecycleCondition:
		if err := validateOperator(c.OperatorType); err != nil {
			return err
		}
		if err := validateOperandShape(c.OperatorType, c.Value); err != nil {
			return err
		}
		return validateBetweenArity(c.OperatorType, c.Value)
	case *predicate.ArrayCondition:
		// Held to the rules of its desugared leaves; the path shape is checked
		// by the caller (workflow's walkCriterion) before this runs.
		d, err := desugarArrayClause(c)
		if err != nil {
			return err
		}
		return validateCriterionConditionAtDepth(d, depth+1)
	case *predicate.GroupCondition:
		// Shares validateGroupOperator with validateConditionAtDepth's own
		// GroupCondition arm — see that arm's doc for the operator-name and
		// NOT-arity rationale. Both surfaces must accept and reject the same
		// group shapes identically.
		if err := validateGroupOperator(c.Operator, len(c.Conditions)); err != nil {
			return err
		}
		for _, child := range c.Conditions {
			if err := validateCriterionConditionAtDepth(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.FunctionCondition:
		// A criterion legitimately carries a FUNCTION clause — nothing to
		// check here; this is the one arm that differs from
		// validateConditionAtDepth.
		return nil
	default:
		return nil
	}
}

// desugarArrayClause expands an array clause into its positional simple
// clauses (spi.DesugarCondition) for ValidateCondition and
// ValidateCriterionCondition. A clause that tests no position is
// INVALID_CONDITION; the message names `values`, so a client that sent
// `value` (or no entries) learns which member to supply.
func desugarArrayClause(c *predicate.ArrayCondition) (predicate.Condition, error) {
	d, err := spi.DesugarCondition(c)
	if errors.Is(err, spi.ErrArrayClauseTestsNothing) {
		return nil, fmt.Errorf("%w: %w; an array clause must carry `values` with at least one non-null entry",
			ErrInvalidCondition, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCondition, err)
	}
	return d, nil
}

// validateGroupOperator rejects a GroupCondition.Operator outside the closed
// AND/OR/NOT set and, for NOT, enforces its arity-exactly-one contract.
// Shared by validateConditionAtDepth (ValidateCondition's search-condition
// walk) and validateCriterionConditionAtDepth (ValidateCriterionCondition's
// criterion walk) so a NOT group is accepted or rejected identically on
// both surfaces.
//
// NOT's arity check belongs here, not in predicate.ParseCondition: a
// criterion that fails to parse is left alone by validateCriterion (the
// workflow-import boundary) rather than rejected, so a parser-level arity
// check would let a malformed NOT criterion import with 200 and then fail
// every subsequent save on that transition, permanently, with no error ever
// surfaced anywhere. This validator is the one boundary every search- and
// criterion-shaped entry point funnels through instead — the same reasoning
// the operator-name rejection above it already relies on.
//
// n is len(GroupCondition.Conditions). Zero or two-or-more is rejected as
// ErrInvalidCondition — the same sentinel every other structural rejection
// in this file wraps — rather than left for match.Prepare's own FilterNot
// arity check (or groupToFilter's translation) to reject downstream, where
// an unclassified failure would surface as a 500 on client-supplied input
// exactly like the bare group-operator mismatch this function's caller
// already closes.
func validateGroupOperator(operator string, n int) error {
	switch operator {
	case "AND", "OR":
		return nil
	case "NOT":
		if n != 1 {
			return fmt.Errorf("%w: NOT requires exactly one condition, got %d", ErrInvalidCondition, n)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown group operator %q; valid: AND, OR, NOT", ErrInvalidCondition, operator)
	}
}

// validateOperator rejects an operatorType outside canonicalOperators — a
// missing operatorType and an unrecognised one both wrap ErrInvalidCondition,
// per operator-semantics.md §4. Shared by ValidateCondition (the search
// entry point) and validateCriterionConditionAtDepth (ValidateCriterionCondition's
// recursive walk), so both surfaces reject the same operator set identically.
func validateOperator(op string) error {
	if op == "" {
		return fmt.Errorf("%w: missing operatorType; valid: %s", ErrInvalidCondition, canonicalOperatorList())
	}
	if _, ok := canonicalOperators[op]; !ok {
		return fmt.Errorf("%w: unknown operatorType %q; valid: %s", ErrInvalidCondition, op, canonicalOperatorList())
	}
	return nil
}

// validateOperandShape rejects an operand whose shape no operator can
// evaluate. A SimpleCondition or LifecycleCondition leaf's value is a scalar
// or a null, except under BETWEEN / BETWEEN_INCLUSIVE, where it is the
// [low, high] pair (validateBetweenArity checks the pair itself). So:
//
//   - an object (map[string]any) is refused for every operator. It denotes no
//     value and would otherwise slip past a parse-based type check (a map
//     stringifies via fmt.Sprint into text that can "parse" as a STRING
//     field's operand);
//   - an array ([]any) is refused for every operator other than BETWEEN /
//     BETWEEN_INCLUSIVE. No other operator takes a set of values, and the
//     backends evaluate such an operand differently: memory compares the
//     stringified array, sqlite matches every row on NOT_EQUAL and none on
//     EQUALS. On a conditional delete that is the difference between
//     deleting nothing and deleting nearly everything.
//
// Both are shape errors — INVALID_CONDITION, not CONDITION_TYPE_MISMATCH —
// and apply whether or not the field or model is known. An array clause
// reaches this check through its desugared positional leaves, so an entry
// is held to the same rule as a simple clause's value.
func validateOperandShape(op string, value any) error {
	switch value.(type) {
	case map[string]any:
		return fmt.Errorf("condition value is an object, which is not a valid operand: %w", ErrInvalidCondition)
	case []any:
		if op != "BETWEEN" && op != "BETWEEN_INCLUSIVE" {
			return fmt.Errorf("%w: operator %q takes a single value, not an array; only BETWEEN and BETWEEN_INCLUSIVE take a [low, high] pair", ErrInvalidCondition, op)
		}
	}
	return nil
}

// validateBetweenArity enforces that a BETWEEN / BETWEEN_INCLUSIVE
// condition's value is exactly a 2-element array (the [lo, hi] bounds pair)
// with no null bound.
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
		return fmt.Errorf("%w: operator %q requires exactly two operands, got %T", ErrInvalidCondition, op, value)
	}
	if len(vals) != 2 {
		return fmt.Errorf("%w: operator %q requires exactly two operands, got %d", ErrInvalidCondition, op, len(vals))
	}
	for _, b := range vals {
		if b == nil {
			return fmt.Errorf("%w: operator %q bounds must not be null", ErrInvalidCondition, op)
		}
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
