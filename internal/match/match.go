package match

import (
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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

// convertJSONPath converts JSONPath notation to gjson path format.
// Examples:
//
//	"$.name"                    → "name"
//	"$.laureates[*].motivation" → "laureates.#.motivation"
//	"$.address.street"          → "address.street"
//	"name"                      → "name" (already gjson format)
func convertJSONPath(jsonPath string) string {
	path := jsonPath

	// Strip leading "$." or "$"
	if strings.HasPrefix(path, "$.") {
		path = path[2:]
	} else if strings.HasPrefix(path, "$") {
		path = path[1:]
	}

	// Convert array wildcard [*] to gjson # notation.
	path = strings.ReplaceAll(path, "[*]", ".#")

	// Clean up any double dots from the conversion.
	for strings.Contains(path, "..") {
		path = strings.ReplaceAll(path, "..", ".")
	}

	return path
}

// fieldMapKey normalises a condition's jsonPath to the form every caller's
// FieldsMap is keyed by: "$."-prefixed. A prefix-less path is accepted
// everywhere else in the stack, so looking it up raw silently resolves to no
// declared type — and a type-directed comparison with no declared type expands
// into nothing and never matches, making the leaf false for every entity.
// arrayElementFieldPath already normalised for array conditions; simple leaves
// went without.
func fieldMapKey(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" || strings.HasPrefix(p, "$") {
		return p
	}
	return "$." + p
}

// arrayElementFieldPath returns the FieldsMap key that addresses an
// ArrayCondition's element type. ArrayCondition names a container path
// ("$.tags"); the model records the element type under the same path with a
// trailing "[*]" ("$.tags[*]"), mirroring search.arrayElementPath so the
// predicate evaluator and the search pushdown stamp the same declared types on
// positional array leaves.
func arrayElementFieldPath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "$") {
		p = "$." + p
	}
	if strings.HasSuffix(p, "[*]") {
		return p
	}
	return p + "[*]"
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
