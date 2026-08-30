package match

import (
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// FieldTypes resolves a condition's JSONPath (in "$."-prefixed FieldsMap key
// form, with "[*]" marking array-wildcard hops) to the field's declared model
// DataTypes. It is the plumbing that makes the predicate-tree evaluator
// type-directed, exactly like the search pushdown: a temporal data field whose
// stored JSON is an ISO string is only known to compare temporally (not
// lexically) because the model declares its type here.
//
// A nil FieldTypes, or a lookup that returns nil for an unknown/untyped path,
// is tolerated: comparison/range leaves on that path degrade to non-match
// (they cannot be typed), while string and null-test leaves are unaffected.
type FieldTypes func(jsonPath string) []spi.DataType

// stripLeader removes the wire-form "$." (or bare "$") leader from a
// jsonPath, leaving the filter-path spelling [spi.ParseFilterPath] accepts. A
// path with no leader — an internally constructed FieldsMap key, or a bare
// name some callers pass — is left as is.
func stripLeader(jsonPath string) string {
	if strings.HasPrefix(jsonPath, "$.") {
		return jsonPath[2:]
	}
	if strings.HasPrefix(jsonPath, "$") {
		return jsonPath[1:]
	}
	return jsonPath
}

// fieldMapKey normalises a condition's jsonPath to the form every caller's
// FieldsMap is keyed by: "$."-prefixed, with every array hop spelled "[*]". A
// prefix-less path is accepted everywhere else in the stack, and a positional
// subscript ("$.arr[0]") is accepted by the boundary grammar, so looking either
// up raw silently resolves to no declared type — and a type-directed
// comparison with no declared type expands into nothing and never matches,
// making the leaf false for every entity. The schema records an array's
// ELEMENT type once, under the wildcard key; there is no per-index entry to
// find.
func fieldMapKey(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return p
	}
	if !strings.HasPrefix(p, "$") {
		p = "$." + p
	}
	return schema.CanonicalFieldPath(p)
}

// isTemporalOperator reports whether op is a valid operator on a temporal
// field: the six comparisons, the two range ops, and the two null tests.
// String operators are excluded so a temporal field never substring-matches
// its formatted representation.
func isTemporalOperator(op string) bool {
	switch op {
	case "EQUALS", "NOT_EQUAL",
		"GREATER_THAN", "LESS_THAN", "GREATER_OR_EQUAL", "LESS_OR_EQUAL",
		"BETWEEN", "BETWEEN_INCLUSIVE",
		"IS_NULL", "NOT_NULL":
		return true
	default:
		return false
	}
}
