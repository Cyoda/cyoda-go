package search

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// jsonPathLeader is the mandatory prefix of a wire jsonPath.
const jsonPathLeader = "$."

// The wire jsonPath grammar, enforced at the API boundary:
//
//	jsonPath  = "$." segment ( "." segment )*
//	segment   = name subscript*
//	name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
//	subscript = "[" ( "*" / 1*DIGIT ) "]"
//
// The two character-level predicates — the segment charset and the positional
// subscript — are [schema.IsSegmentNameByte] and [schema.IsArrayIndex]. They
// describe a field path rather than a search, so they live with the schema
// that keys fields by path, and the model-import side checks a bare field name
// against the same charset without depending on this package. What lives HERE
// is the path grammar: the leader, the segment/subscript structure, the two
// error classes, and the lockstep with the SPI translator.
//
// jsonPath is JSON Path nomenclature, so the "$." leader is REQUIRED.
// "$.amount" is a path; a bare "amount" is not one, and is rejected rather
// than tolerated as an alias. So are an empty path, an empty or trailing
// segment ("$..a", "$.a."), bracket-quoted property access ("$['x']",
// "$.['x']", `$.a["b"]`), a bracket spelling outside the two supported
// subscript forms ("$.a[", "$.a]", "$.[0]", "$.a[-1]", "$.a[0:2]",
// "$.a[0,1]", "$.a[?(@.x)]"), and any character outside the segment set —
// including one that FOLLOWS a well-formed subscript ("$.a[0];DROP",
// "$.a[0].xé"). All of these wrap [errInvalidFieldPath], which every transport
// maps to 400 INVALID_FIELD_PATH.
//
// The surfaces share this grammar and differ on ONE point — a WELL-FORMED
// array subscript ("$.tags[*].name", "$.arr[0]", "$.matrix[*][*]"):
//
//   - [ValidateConditionJSONPath], the condition jsonPath surface, ACCEPTS
//     them. They are valid JSON Path that no pushdown filter can express;
//     spi.ConditionToFilter refuses them with a PLAIN error that does not wrap
//     spi.ErrInvalidFilterPath, precisely so the caller falls back to the
//     in-memory evaluator, which serves them.
//   - [ValidateScalarJSONPath] — the groupBy, aggregation-field and SORT-key
//     surfaces — REJECTS them. Those paths must denote a single scalar; a
//     projection has no fallback that could produce one.
//
// A sort key differs from the other two scalar surfaces on the leader alone:
// it is spelled bare over HTTP ("price"), so resolveOrderBy normalises before
// validating rather than requiring "$." of the caller.
//
// Both scan the WHOLE path, subscripts included — the same scan spi's
// stripDollarDot performs — and only differ in what they do with a well-formed
// subscript, so the condition surface classifies every path exactly as the
// translator does. TestValidateCondition_PathGrammarMatchesSPI pins that
// agreement against the translator itself, so a future divergence fails a test
// rather than silently turning working queries into 400s.
//
// The scan used to stop at the first '[' and accept the remainder unread. The
// consequence was not a lenient 400 but a wrong answer: everything after the
// bracket went unvalidated, spi.stripDollarDot short-circuited identically, so
// a malformed path classified as "unpushdownable" and fell back to the
// in-memory evaluator — which resolves none of those spellings and answers an
// empty page for a field that exists. On the two surfaces with no downstream
// schema backstop (a grouped-stats `condition`, which validates against a nil
// model, and workflow criterion import, which is grammar-only) that surfaced
// as a 200 with wrong buckets and a criterion that silently never fires.

// ValidateConditionJSONPath checks a condition's wire jsonPath against the
// grammar above, tolerating WELL-FORMED array-subscript syntax so the
// in-memory fallback still serves it. A malformed subscript is rejected like
// any other syntax error. Errors wrap [ErrInvalidFieldPath].
//
// Exported for callers outside this package that validate a condition path
// without going through [ValidateCondition]; [ValidateCondition] applies it to
// every Simple and Array leaf it walks.
func ValidateConditionJSONPath(path string) error {
	return validateJSONPath(path, true)
}

// ValidateArrayClauseJSONPath checks an `array` clause's jsonPath: the wire
// grammar ([ValidateConditionJSONPath]) plus the clause-shape rule
// path-grammar.md §8 adds on top of it — the path must carry a trailing
// array wildcard ("[*]"). The clause tests elements by position, so its path
// must address elements, not the array itself: a bare path ("$.tags")
// addresses the container and carries no positional test, and a
// positional-only path ("$.tags[0]") names one element rather than testing
// by position across the whole array. Errors wrap [ErrInvalidFieldPath].
//
// This requirement is specific to the array clause: a `simple` clause's bare
// path still legitimately addresses the container (see path_validate.go's
// container acceptance, which "$.tags NOT_NULL" relies on).
//
// Exported so every surface that carries an array clause enforces the same
// shape rule from one implementation: the search condition boundary
// (ValidateCondition's ArrayCondition arm) and workflow-criterion import
// (workflow.walkCriterion) both call this rather than each independently
// deciding what "ends in a trailing wildcard" means. Section 7's
// "grammar only" exemption for a criterion is about the MODEL check — a
// criterion is not validated against the model — not this clause-shape rule,
// which is checkable without the model and belongs with the operator and
// pattern checks workflow import already runs.
func ValidateArrayClauseJSONPath(path string) error {
	if err := ValidateConditionJSONPath(path); err != nil {
		return err
	}
	if !strings.HasSuffix(path, "[*]") {
		return fmt.Errorf(
			"%w: jsonPath %q must end in a trailing array wildcard \"[*]\"; "+
				"an array clause tests elements by position and a bare path addresses the array itself",
			errInvalidFieldPath, path)
	}
	return nil
}

// ValidateScalarJSONPath checks a path that must denote a single scalar — a
// grouped-stats groupBy entry, an aggregation field, or a sort key — against
// the same grammar, additionally rejecting array projections and subscripts.
// Errors wrap [ErrInvalidFieldPath].
//
// Without it a malformed path is caught only when pushdown is actually used:
// the grouped-stats service falls through to the in-process streaming tally
// whenever a backend declines pushdown (a residual filter, a point-in-time
// query, sqlite declining stdev), and there the path is resolved with gjson,
// misses, and buckets every entity as null — a wrong-but-available 200 rather
// than an error. Validating at the boundary makes both execution paths reject
// identically and leaves each plugin's own check as a backstop.
//
// resolveOrderBy applies it to a SORT key for the same reason, and there the
// in-memory branch has no plugin backstop at all: the engine sorts the drained
// slice itself, so an unvalidated projection was answered as an unsorted page
// rather than refused.
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
	// page for a field that exists. Both quoting styles: the subscript scan
	// below would reject either anyway, but naming them first lets the
	// diagnostic say what to write instead.
	if strings.Contains(rest, "['") || strings.Contains(rest, "']") ||
		strings.Contains(rest, `["`) || strings.Contains(rest, `"]`) {
		return invalidJSONPathError(path,
			`bracket-quoted property access is not supported; use dotted access (e.g. "$.a.b")`)
	}
	hasSubscript, err := scanJSONPathBody(rest, func(reason string) error {
		return invalidJSONPathError(path, reason)
	})
	if err != nil {
		return err
	}
	if hasSubscript && !allowSubscript {
		return invalidJSONPathError(path,
			"array subscripts and projections are not supported here; the path must address a single scalar")
	}
	return nil
}

// scanJSONPathBody validates the leader-stripped remainder of a wire jsonPath
// against the segment grammar documented above and reports whether it uses
// array-subscript syntax. Diagnostics are built by mkInvalid so the caller can
// echo the full path alongside the reason.
//
// It is a deliberate line-by-line port of spi.scanWirePathBody — the two must
// classify identically, which TestValidateCondition_PathGrammarMatchesSPI pins
// against the translator itself.
func scanJSONPathBody(body string, mkInvalid func(reason string) error) (bool, error) {
	hasSubscript := false
	i, n := 0, len(body)
	for {
		nameStart := i
		for i < n && schema.IsSegmentNameByte(body[i]) {
			i++
		}
		if i == nameStart {
			if i == n {
				return false, mkInvalid("ends in a trailing dot")
			}
			switch body[i] {
			case '.':
				return false, mkInvalid("contains an empty path segment")
			case '[':
				return false, mkInvalid("has an array subscript with no field name before it")
			case ']':
				return false, mkInvalid(`contains an unmatched "]"`)
			default:
				return false, mkInvalid(disallowedCharReason(body[i:]))
			}
		}
		for i < n && body[i] == '[' {
			rel := strings.IndexByte(body[i:], ']')
			if rel < 0 {
				return false, mkInvalid("has an unclosed array subscript")
			}
			inner := body[i+1 : i+rel]
			if !isSupportedSubscript(inner) {
				return false, mkInvalid(fmt.Sprintf(
					"has an unsupported array subscript %q; only the wildcard [*] and a non-negative index (e.g. [0]) are supported",
					"["+inner+"]"))
			}
			hasSubscript = true
			i += rel + 1
		}
		if i == n {
			return hasSubscript, nil
		}
		switch body[i] {
		case '.':
			i++
			if i == n {
				return false, mkInvalid("ends in a trailing dot")
			}
		case ']':
			return false, mkInvalid(`contains an unmatched "]"`)
		default:
			return false, mkInvalid(disallowedCharReason(body[i:]))
		}
	}
}

// isSupportedSubscript reports whether the text between "[" and "]" is one of
// the two forms the stack can resolve: the wildcard, or a non-negative decimal
// index. Everything else (a slice, a union, a filter expression, a negative or
// signed index, whitespace) has no equivalent in either evaluator — neither
// pushdown nor spi.ResolvePath, the one in-memory resolver both
// internal/match and the SPI kernel share, has any notation for it.
//
// The index arm delegates to [schema.IsArrayIndex], which itself delegates to
// spi.IsArrayIndex — the SPI's single definition of a well-formed subscript,
// the same one spi.ParseFilterPath consults when it parses the hops
// spi.ResolvePath then walks. Sharing it is what makes "the boundary accepts
// it" and "the resolver resolves it" the same question: a second spelling of
// the test could admit a subscript the resolver then never matches, which
// resolves to nothing — the exact failure this grammar tightening exists to
// close.
func isSupportedSubscript(inner string) bool {
	return inner == "*" || schema.IsArrayIndex(inner)
}

// disallowedCharReason renders the diagnostic for the first rune of s, which
// the grammar does not admit. It decodes a rune rather than a byte so a
// non-ASCII character is echoed whole rather than as a mojibake fragment.
func disallowedCharReason(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	return fmt.Sprintf("contains disallowed character %q", r)
}

// invalidJSONPathError builds the errInvalidFieldPath-wrapping diagnostic for
// a path outside the model's JSON Path syntax, echoing the offending path so
// the caller can correct it.
func invalidJSONPathError(path, reason string) error {
	return fmt.Errorf("%w: jsonPath %q %s", errInvalidFieldPath, path, reason)
}
