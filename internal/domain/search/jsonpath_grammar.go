package search

import (
	"fmt"
	"strings"
)

// jsonPathLeader is the mandatory prefix of a wire jsonPath.
const jsonPathLeader = "$."

// The wire jsonPath grammar, enforced at the API boundary:
//
//	jsonPath = "$." segment ( "." segment )*
//	segment  = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
//
// jsonPath is JSON Path nomenclature, so the "$." leader is REQUIRED.
// "$.amount" is a path; a bare "amount" is not one, and is rejected rather
// than tolerated as an alias. So are an empty path, an empty or trailing
// segment ("$..a", "$.a."), bracket-quoted property access ("$['x']",
// "$.['x']") and any character outside the segment set. All of these wrap
// [errInvalidFieldPath], which every transport maps to 400 INVALID_FIELD_PATH.
//
// Two surfaces share this grammar and differ on ONE point — array subscripts
// ("$.tags[*].name", "$.arr[0]"):
//
//   - [ValidateConditionJSONPath], the condition jsonPath surface, ACCEPTS
//     them. They are valid JSON Path that no pushdown filter can express;
//     spi.ConditionToFilter refuses them with a PLAIN error that does not wrap
//     spi.ErrInvalidFilterPath, precisely so the caller falls back to the
//     in-memory evaluator, which serves them.
//   - [ValidateScalarJSONPath], the groupBy / aggregation-field surface,
//     REJECTS them. Those paths must denote a single scalar; a projection has
//     no fallback that could produce one.
//
// Both stop at the first '[' or ']' — the same short-circuit spi's
// stripDollarDot performs — and only differ in what they do there, so the
// condition surface classifies every path exactly as the translator does.
// TestValidateCondition_PathGrammarMatchesSPI pins that agreement against the
// translator itself, so a future divergence fails a test rather than silently
// turning working queries into 400s.

// ValidateConditionJSONPath checks a condition's wire jsonPath against the
// grammar above, tolerating array-subscript syntax so the in-memory fallback
// still serves it. Errors wrap [ErrInvalidFieldPath].
//
// Exported for callers outside this package that validate a condition path
// without going through [ValidateCondition]; [ValidateCondition] applies it to
// every Simple and Array leaf it walks.
func ValidateConditionJSONPath(path string) error {
	return validateJSONPath(path, true)
}

// ValidateScalarJSONPath checks a path that must denote a single scalar —
// a grouped-stats groupBy entry or aggregation field — against the same
// grammar, additionally rejecting array projections and subscripts. Errors
// wrap [ErrInvalidFieldPath].
//
// Without it a malformed path is caught only when pushdown is actually used:
// the grouped-stats service falls through to the in-process streaming tally
// whenever a backend declines pushdown (a residual filter, a point-in-time
// query, sqlite declining stdev), and there the path is resolved with gjson,
// misses, and buckets every entity as null — a wrong-but-available 200 rather
// than an error. Validating at the boundary makes both execution paths reject
// identically and leaves each plugin's own check as a backstop.
func ValidateScalarJSONPath(path string) error {
	return validateJSONPath(path, false)
}

func validateJSONPath(path string, allowSubscript bool) error {
	if !strings.HasPrefix(path, jsonPathLeader) {
		return invalidJSONPathError(path,
			`must be a JSON Path: expected the "$." leader (e.g. "$.amount")`)
	}
	rest := path[len(jsonPathLeader):]
	if rest == "" {
		return invalidJSONPathError(path, `addresses no field: nothing follows the "$." leader`)
	}
	// Bracket-quoted property access denotes the same node as dotted access but
	// is not the model's syntax, and NO evaluator in the stack resolves it —
	// pushdown rejects it and the in-memory fallback misses, answering an empty
	// page for a field that exists. Named separately from the subscript arm
	// below so the diagnostic can say what to write instead.
	if strings.Contains(rest, "['") || strings.Contains(rest, "']") {
		return invalidJSONPathError(path,
			`bracket-quoted property access is not supported; use dotted access (e.g. "$.a.b")`)
	}
	segStart := 0
	for i, c := range rest {
		if c == '.' {
			if i == segStart {
				return invalidJSONPathError(path, "contains an empty path segment")
			}
			segStart = i + 1
			continue
		}
		if c == '[' || c == ']' {
			if allowSubscript {
				return nil
			}
			return invalidJSONPathError(path,
				"array subscripts and projections are not supported here; the path must address a single scalar")
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
			// safe
		default:
			return invalidJSONPathError(path, fmt.Sprintf("contains disallowed character %q", c))
		}
	}
	if segStart == len(rest) {
		return invalidJSONPathError(path, "ends in a trailing dot")
	}
	return nil
}

// invalidJSONPathError builds the errInvalidFieldPath-wrapping diagnostic for
// a path outside the model's JSON Path syntax, echoing the offending path so
// the caller can correct it.
func invalidJSONPathError(path, reason string) error {
	return fmt.Errorf("%w: jsonPath %q %s", errInvalidFieldPath, path, reason)
}
