package search

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// jsonPathLeader is the mandatory prefix of a wire jsonPath.
const jsonPathLeader = "$."

// The wire jsonPath grammar, enforced at the API boundary:
//
//	jsonPath  = "$." segment ( "." segment )*
//	segment   = name subscript*
//	name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
//	subscript = "[" ( "*" / 1*DIGIT ) "]"          ; the digit run must fit an int32
//
// This is exactly [spi.ParseFilterPath]'s grammar for the leader-stripped
// remainder, so validateJSONPath strips the "$." leader and its own
// bracket-quote pre-check (below) and then delegates the whole scan —
// segment charset, subscript well-formedness INCLUDING the magnitude bound on
// a positional index, balanced brackets, trailing-garbage rejection — to that
// one parser rather than keeping a second, independently-maintained scan of
// the same grammar. What lives HERE is the wire-specific layer on top: the
// leader requirement, the bracket-quote diagnostic, and the per-surface
// subscript-tolerance policy below.
//
// jsonPath is JSON Path nomenclature, so the "$." leader is REQUIRED.
// "$.amount" is a path; a bare "amount" is not one, and is rejected rather
// than tolerated as an alias. So are an empty path, an empty or trailing
// segment ("$..a", "$.a."), bracket-quoted property access ("$['x']",
// "$.['x']", `$.a["b"]`), a bracket spelling outside the two supported
// subscript forms ("$.a[", "$.a]", "$.[0]", "$.a[-1]", "$.a[0:2]",
// "$.a[0,1]", "$.a[?(@.x)]"), an index too large to fit an int32
// ("$.tags[2147483648]", "$.tags[99999999999999999999]"), and any character outside the segment
// set — including one that FOLLOWS a well-formed subscript ("$.a[0];DROP",
// "$.a[0].xé"). All of these wrap [errInvalidFieldPath], which every transport
// maps to 400 INVALID_FIELD_PATH.
//
// The surfaces share this grammar and differ on ONE point — a WELL-FORMED
// array subscript ("$.tags[*].name", "$.arr[0]", "$.matrix[*][*]"):
//
//   - [ValidateConditionJSONPath], the condition jsonPath surface, ACCEPTS
//     them. They are valid JSON Path, and the kernel resolves a subscripted
//     path directly (see [spi.ResolvePath]) — a positional index pushes down
//     as a JSON array access on every backend, while a wildcard leaf has no
//     SQL form on either backend until a quantifier node exists (each SQL
//     planner's isLeafPushable routes it to the residual, same as a shape
//     neither backend can push down at all, e.g. a
//     [predicate.FunctionCondition]) and falls back to the in-memory
//     evaluator instead. Either way the request is served, not refused.
//   - [ValidateScalarJSONPath] — the groupBy, aggregation-field and SORT-key
//     surfaces — REJECTS them. Those paths must denote a single scalar; a
//     projection or a positional element has no single-value meaning there.
//
// A sort key differs from the other two scalar surfaces on the leader alone:
// it is spelled bare over HTTP ("price"), so resolveOrderBy normalises before
// validating rather than requiring "$." of the caller.
//
// Both scan the WHOLE path, subscripts included, through the one shared
// parser, and only differ in what they do with a well-formed subscript, so
// the condition surface classifies every path exactly as the translator
// does. TestValidateCondition_PathGrammarMatchesSPI pins that agreement
// against the translator itself, so a future divergence fails a test rather
// than silently turning working queries into 400s (or the reverse: silently
// accepting a path the translator then can never resolve).
//
// The scan used to stop at the first '[' and accept the remainder unread. The
// consequence was not a lenient 400 but a wrong answer: everything after the
// bracket went unvalidated, spi.stripDollarDot short-circuited identically, so
// a malformed path classified as "unpushdownable" and fell back to the
// in-memory evaluator — which resolves none of those spellings and answers an
// empty page for a field that exists. On the two surfaces with no downstream
// schema backstop (a grouped-stats `condition`, which validates against a nil
// model, and workflow criterion import, which is grammar-only) that surfaced
// as a 200 with wrong buckets and a criterion that silently never fires. A
// second, later drift of the same shape — this package's own subscript check
// carrying the digit-class test only, with no magnitude bound, while
// [spi.ParseFilterPath] additionally required the run to fit an int — let an
// overflowing index through the boundary that no resolver could ever resolve;
// building directly on [spi.ParseFilterPath] instead of a hand-ported copy of
// its grammar closes that class of drift rather than patching this one
// instance of it.

// ValidateConditionJSONPath checks a condition's wire jsonPath against the
// grammar above, tolerating WELL-FORMED array-subscript syntax — the kernel
// resolves it directly, see [spi.ResolvePath]. A malformed subscript is
// rejected like any other syntax error. Errors wrap [ErrInvalidFieldPath].
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
	// page for a field that exists. Both quoting styles: spi.ParseFilterPath
	// below would reject either anyway (as an unsupported subscript or a
	// disallowed character), but naming them first lets the diagnostic say
	// what to write instead of a more oblique bracket complaint.
	if strings.Contains(rest, "['") || strings.Contains(rest, "']") ||
		strings.Contains(rest, `["`) || strings.Contains(rest, `"]`) {
		return invalidJSONPathError(path,
			`bracket-quoted property access is not supported; use dotted access (e.g. "$.a.b")`)
	}
	// spi.ParseFilterPath is the one parser for this grammar — the leader-
	// stripped remainder here is byte-for-byte the plugin-facing filter-path
	// form it parses — so the boundary check is built on it rather than a
	// second, hand-ported scan that could silently drift from what the
	// translator and the resolver actually accept (as the digit-class-only
	// subscript check this replaced did: it had no magnitude bound, so it
	// accepted an overflowing index spi.ParseFilterPath, and therefore every
	// resolver, always rejected).
	hops, err := spi.ParseFilterPath(rest)
	if err != nil {
		return invalidJSONPathError(path, filterPathFailureReason(rest, err))
	}
	hasSubscript := false
	for _, hop := range hops {
		if len(hop.Subs) > 0 {
			hasSubscript = true
			break
		}
	}
	if hasSubscript && !allowSubscript {
		return invalidJSONPathError(path,
			"array subscripts and projections are not supported here; the path must address a single scalar")
	}
	return nil
}

// invalidJSONPathError builds the errInvalidFieldPath-wrapping diagnostic for
// a path outside the model's JSON Path syntax, echoing the offending path so
// the caller can correct it.
func invalidJSONPathError(path, reason string) error {
	return fmt.Errorf("%w: jsonPath %q %s", errInvalidFieldPath, path, reason)
}

// filterPathFailureReason extracts the REASON half of a spi.ParseFilterPath
// failure rather than passing its whole formatted Error() through as the
// reason. err.Error() already carries the SPI's own wrapper
// (invalidFilterPathError, filter_path.go): "invalid filter path: filter
// path %q <reason>". Embedding that inside invalidJSONPathError's "invalid
// field path: jsonPath %q <reason>" doubled both the sentinel phrase and the
// path — quoted twice, under two different names ("jsonPath" vs. "filter
// path") and two different spellings (the wire "$."-led path here, the
// leader-stripped rest inside the SPI's own message). Stripping the known
// prefix leaves just the reason, so the wire body carries ONE prefix and
// ONE quoted path.
//
// It also retargets one specific reason shape: see
// [overflowingIndexReason].
func filterPathFailureReason(rest string, err error) string {
	prefix := fmt.Sprintf("%s: filter path %q ", spi.ErrInvalidFilterPath, rest)
	reason := err.Error()
	if strings.HasPrefix(reason, prefix) {
		reason = reason[len(prefix):]
	}
	if bound, ok := overflowingIndexReason(reason); ok {
		return bound
	}
	return reason
}

// overflowingIndexReason detects the one "unsupported array subscript"
// shape that is not actually a syntax error: a subscript that is a
// well-formed run of decimal digits too large to fit the int32 magnitude
// bound spi.ParseFilterPath enforces (this file's doc comment, and
// path-grammar.md §2/§9 — int32 is the intersection every in-tree backend
// can address). spi.ParseFilterPath reports that shape with the same
// generic wording a genuinely malformed subscript ("[-1]", "[0:2]",
// "[?(@.x)]") gets, which is actively misleading for an overflow:
// 2147483648 IS a non-negative index, so telling the caller only the
// wildcard and a non-negative index are supported names a rule the input
// already satisfies without ever stating the real reason — the bound.
//
// Returns the replacement diagnostic and true when reason matches that
// shape; otherwise ok is false and the caller keeps the original reason,
// which is correct for every other malformed-subscript spelling.
func overflowingIndexReason(reason string) (string, bool) {
	const prefix, suffix = `has an unsupported array subscript "[`,
		`]"; only the wildcard [*] and a non-negative index (e.g. [0]) are supported`
	if !strings.HasPrefix(reason, prefix) || !strings.HasSuffix(reason, suffix) {
		return "", false
	}
	digits := reason[len(prefix) : len(reason)-len(suffix)]
	if !spi.IsArrayIndex(digits) {
		return "", false
	}
	if _, err := strconv.ParseInt(digits, 10, 32); err == nil {
		// A digit run that DOES fit int32 is not this failure — spi.ParseFilterPath
		// would have accepted it. Stays defensive rather than mis-attributing
		// some other "unsupported array subscript" cause to a range overflow.
		return "", false
	}
	return fmt.Sprintf(
		"has an array index %q too large to be supported; the index must fit a 32-bit signed integer (maximum %d)",
		digits, math.MaxInt32,
	), true
}
