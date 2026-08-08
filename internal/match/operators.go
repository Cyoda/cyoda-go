package match

import (
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

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

// operandsToStrings renders each element of a BETWEEN operand slice through
// spi.OperandString (the single shared operand→string normalization).
func operandsToStrings(vs []any) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = spi.OperandString(v)
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
