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

// convertJSONPath converts JSONPath notation to gjson path format.
// Examples:
//
//	"$.name"                    → "name"
//	"$.laureates[*].motivation" → "laureates.#.motivation"
//	"$.arr[0]"                  → "arr.0"
//	"$.address.street"          → "address.street"
//	"name"                      → "name" (already gjson format)
//
// gjson has no bracket syntax at all: it addresses an array element with a
// numeric path segment ("arr.0") and every element with "#". A subscript left
// unrewritten therefore reaches gjson as a request for a key literally spelled
// "arr[0]", misses, and makes the leaf false for every entity — an empty page,
// or a criterion that never fires, for a field that holds the value. Since a
// subscripted path is accepted by the boundary grammar
// (search.ValidateConditionJSONPath) and refused by spi.ConditionToFilter with
// a plain non-ErrInvalidFilterPath error — which every call site reads as "not
// pushdownable, evaluate in memory" — this evaluator is the ONLY one that ever
// sees it, and is obliged to resolve it.
func convertJSONPath(jsonPath string) string {
	path := jsonPath

	// Strip leading "$." or "$"
	if strings.HasPrefix(path, "$.") {
		path = path[2:]
	} else if strings.HasPrefix(path, "$") {
		path = path[1:]
	}

	// Convert array subscripts to gjson notation: [*] → .#, [N] → .N.
	path = rewriteSubscripts(path)

	// Clean up any double dots from the conversion: a "[...]" rewritten to a
	// dotted gjson segment leaves "a..#" when the caller wrote "a.[*]". The
	// boundary grammar now rejects that spelling, but convertJSONPath is also
	// reached from callers that never passed one (prepared criteria built
	// in-process, FieldsMap keys), so the normalisation stays.
	for strings.Contains(path, "..") {
		path = strings.ReplaceAll(path, "..", ".")
	}

	return path
}

// rewriteSubscripts rewrites each "[...]" of a leader-stripped path into the
// equivalent gjson segment: "[*]" → ".#" (every element) and "[N]" → ".N"
// (the Nth element, N a non-negative decimal integer).
//
// Any other bracket content is copied VERBATIM rather than half-translated.
// The JSON Path shapes that fall here — a negative index ("[-1]"), a slice
// ("[0:2]"), a union ("[0,1]") and a filter expression ("[?(@.x)]") — have no
// gjson equivalent, so there is nothing to translate them into; emitting a
// guess would answer with the wrong element, which is worse than not answering.
//
// The boundary grammar no longer admits any of them: it scans the whole path
// and accepts only "[*]" and a non-negative decimal index, in lock-step with
// spi.ConditionToFilter (pinned by
// TestValidateCondition_PathGrammarMatchesSPI). So this arm is unreachable for
// a path that came through a condition surface, and remains only as the
// total-function answer for an internally constructed path.
func rewriteSubscripts(path string) string {
	if !strings.ContainsRune(path, '[') {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); {
		if path[i] != '[' {
			b.WriteByte(path[i])
			i++
			continue
		}
		rel := strings.IndexByte(path[i:], ']')
		if rel < 0 {
			// Unclosed subscript — nothing to rewrite; copy the remainder.
			b.WriteString(path[i:])
			break
		}
		inner := path[i+1 : i+rel]
		switch {
		case inner == "*":
			b.WriteString(".#")
		case schema.IsArrayIndex(inner):
			b.WriteByte('.')
			b.WriteString(inner)
		default:
			b.WriteString(path[i : i+rel+1])
		}
		i += rel + 1
	}
	return b.String()
}

// fieldMapKey normalises a condition's jsonPath to the form every caller's
// FieldsMap is keyed by: "$."-prefixed, with every array hop spelled "[*]". A
// prefix-less path is accepted everywhere else in the stack, and a positional
// subscript ("$.arr[0]") is accepted by the boundary grammar, so looking either
// up raw silently resolves to no declared type — and a type-directed
// comparison with no declared type expands into nothing and never matches,
// making the leaf false for every entity. The schema records an array's
// ELEMENT type once, under the wildcard key; there is no per-index entry to
// find. arrayElementFieldPath already normalised the prefix for array
// conditions; simple leaves went without, and neither handled subscripts.
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

// arrayElementFieldPath returns the FieldsMap key that addresses an
// ArrayCondition's element type. ArrayCondition names a container path
// ("$.tags"); the model records the element type under the same path with a
// trailing "[*]" ("$.tags[*]"), matching the pushdown translator's own
// arrayElementPath (cyoda-go-spi condition_filter.go) so the predicate
// evaluator and the search pushdown stamp the same declared types on
// positional array leaves.
//
// It goes through fieldMapKey, so a container path that itself carries a
// subscript ("$.a[1].b") canonicalises before the "[*]" is appended
// ("$.a[*].b[*]"). The translator has no counterpart to that, and needs none:
// its path parser short-circuits at the first '[' and declines the whole
// condition as non-pushdownable, so a subscripted container never reaches a
// filter — it reaches this evaluator instead.
func arrayElementFieldPath(raw string) string {
	p := fieldMapKey(raw)
	if p == "" {
		return ""
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
