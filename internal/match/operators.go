package match

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// applyOperator evaluates a single predicate leaf against a stored gjson value
// by routing through the type-directed EvalLeaf kernel (spi.EvalLeafString) —
// the same comparison core the search pushdown and spi.MatchFilter use, so the
// predicate-tree evaluator and the Filter evaluator agree bit-for-bit.
//
// declared carries the leaf field's model DataTypes. The kernel is
// type-directed: comparison/range operators (EQUALS, the four orderings,
// BETWEEN) need declared to parse the operand and classify the stored value;
// with an empty declared set they degrade to non-match (an unknown/untyped
// field simply doesn't match a typed comparison). String operators and the
// unary null tests are declared-independent.
//
// A kernel expansion error (e.g. an operand that parses into no declared type)
// is treated as a per-entity non-match rather than a hard error: the search /
// grouped-stats / workflow validation boundaries reject genuinely invalid
// conditions up front, so a residual error here means "this entity doesn't
// match", not "fail the request". A genuinely unsupported operator name
// (IS_CHANGED / IS_UNCHANGED / unknown) is still a hard error.
func applyOperator(operatorType string, actual gjson.Result, expected any, declared []spi.DataType) (bool, error) {
	op, ok := opNameToFilterOp(operatorType)
	if !ok {
		return false, fmt.Errorf("unsupported operator: %s", operatorType)
	}

	var values []string
	if op == spi.FilterBetween || op == spi.FilterBetweenInclusive {
		values = betweenBounds(expected)
	}

	matched, err := spi.EvalLeafString(op, operandToString(expected), values, declared, actual)
	if err != nil {
		return false, nil
	}
	return matched, nil
}

// opNameToFilterOp maps a predicate operator NAME to the corresponding kernel
// spi.FilterOp. The boolean is false for operator names with no kernel op:
// IS_CHANGED / IS_UNCHANGED (deliberately unimplemented) and any unknown
// name.
func opNameToFilterOp(op string) (spi.FilterOp, bool) {
	switch op {
	case "EQUALS":
		return spi.FilterEq, true
	case "NOT_EQUAL":
		return spi.FilterNe, true
	case "GREATER_THAN":
		return spi.FilterGt, true
	case "LESS_THAN":
		return spi.FilterLt, true
	case "GREATER_OR_EQUAL":
		return spi.FilterGte, true
	case "LESS_OR_EQUAL":
		return spi.FilterLte, true
	case "CONTAINS":
		return spi.FilterContains, true
	case "STARTS_WITH":
		return spi.FilterStartsWith, true
	case "ENDS_WITH":
		return spi.FilterEndsWith, true
	case "LIKE":
		return spi.FilterLike, true
	case "MATCHES_PATTERN":
		return spi.FilterMatchesRegex, true
	case "IS_NULL":
		return spi.FilterIsNull, true
	case "NOT_NULL":
		return spi.FilterNotNull, true
	case "BETWEEN":
		return spi.FilterBetween, true
	case "BETWEEN_INCLUSIVE":
		return spi.FilterBetweenInclusive, true
	case "IEQUALS":
		return spi.FilterIEq, true
	case "INOT_EQUAL":
		return spi.FilterINe, true
	case "ICONTAINS":
		return spi.FilterIContains, true
	case "INOT_CONTAINS":
		return spi.FilterINotContains, true
	case "NOT_CONTAINS":
		return spi.FilterNotContains, true
	case "ISTARTS_WITH":
		return spi.FilterIStartsWith, true
	case "INOT_STARTS_WITH":
		return spi.FilterINotStartsWith, true
	case "NOT_STARTS_WITH":
		return spi.FilterNotStartsWith, true
	case "IENDS_WITH":
		return spi.FilterIEndsWith, true
	case "INOT_ENDS_WITH":
		return spi.FilterINotEndsWith, true
	case "NOT_ENDS_WITH":
		return spi.FilterNotEndsWith, true
	default:
		return "", false
	}
}

// operandToString renders a predicate operand as the string form the kernel
// parses per declared type. json.Number keeps its exact textual form (no
// float round-trip), booleans render "true"/"false", and other numerics use
// fmt.Sprint. A nil operand renders as the empty string.
func operandToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// operandsToStrings renders each element of a BETWEEN operand slice.
func operandsToStrings(vs []any) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = operandToString(v)
	}
	return out
}

// betweenBounds extracts the two BETWEEN bounds as kernel operand strings.
// The canonical (and only validation-accepted) shape is a two-element []any;
// a legacy "lo,hi" comma string is also tolerated for in-process callers that
// bypass validation. Any other shape yields nil, which the kernel's
// expandBetween rejects into a per-entity non-match.
func betweenBounds(expected any) []string {
	switch v := expected.(type) {
	case []any:
		if len(v) != 2 {
			return nil
		}
		return operandsToStrings(v)
	case string:
		parts := strings.SplitN(v, ",", 2)
		if len(parts) != 2 {
			return nil
		}
		return []string{strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])}
	default:
		return nil
	}
}
