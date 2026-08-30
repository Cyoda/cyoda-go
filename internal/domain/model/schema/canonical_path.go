package schema

import (
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// CanonicalFieldPath rewrites a wire jsonPath into the form [ModelNode.FieldsMap]
// keys it by: every array hop spelled "[*]".
//
//	"$.amount"        → "$.amount"        (unchanged)
//	"$.tags[*]"       → "$.tags[*]"       (unchanged)
//	"$.arr[0]"        → "$.arr[*]"
//	"$.items[1].name" → "$.items[*].name"
//
// A wire path may address one array element positionally, but the schema
// records the ELEMENT's declared type once, under the wildcard key — there is
// no per-index entry and never will be, since the schema describes the shape,
// not the data. Every declared-type lookup in the stack (the predicate
// evaluator's leaf typing, search's field-existence check) indexes that same
// map, so a positional path must be canonicalised before the lookup or it
// misses: the leaf then carries no declared type, a type-directed comparison
// expands into nothing, and the leaf is false for every entity — an empty page
// for a field that holds the value.
//
// Bracket content that names no single element — a negative index ("[-1]"), a
// slice ("[0:2]"), a union ("[0,1]"), an unclosed bracket, or a digit run too
// large to fit an int — is left verbatim. There is no key it could
// canonicalise to, and no evaluator in the stack resolves it.
//
// The path is canonicalised for LOOKUP only. Callers must keep the caller's
// original spelling for anything they echo back or report, so a diagnostic
// names the path the request actually sent.
//
// Built on [spi.ParseFilterPath] — the SPI's one parser for this grammar —
// rather than a byte-scan with its own copy of "what counts as a well-formed
// subscript". A second, independent copy of that predicate is exactly what
// let a subscript's digit run overflow an int and still fold to "[*]" here
// while [spi.ParseFilterPath] rejected the same string outright: two
// definitions of "well-formed" that disagreed, the same C1 defect class
// path-grammar.md §9 and §10 close everywhere else. A leader is optional: a
// "$."-prefixed path folds after the leader; a bare, leader-less path (the
// [Filter.Path] convention) folds directly. Either way a parse failure —
// which should not happen in practice, since every caller has already run the
// path through the wire grammar — fails safe by returning path unfolded
// rather than panicking.
func CanonicalFieldPath(path string) string {
	if !strings.ContainsRune(path, '[') {
		return path
	}
	prefix, rest := "", path
	if strings.HasPrefix(path, jsonPathLeader) {
		prefix, rest = jsonPathLeader, path[len(jsonPathLeader):]
	}
	hops, err := spi.ParseFilterPath(rest)
	if err != nil {
		return path
	}
	return prefix + foldHopsToWildcard(hops)
}

// jsonPathLeader is the mandatory prefix of a wire jsonPath. CanonicalFieldPath
// tolerates both forms it is called with: a full wire jsonPath ("$.arr[0]")
// and the bare, leader-less [Filter.Path] convention ("arr[0]").
const jsonPathLeader = "$."

// foldHopsToWildcard renders parsed hops back into the dotted "name[sub][sub]"
// form, folding every subscript — positional or wildcard — to "[*]": the
// schema records an array's element type once, under the wildcard key, never
// once per index.
func foldHopsToWildcard(hops []spi.PathHop) string {
	var b strings.Builder
	for i, hop := range hops {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(hop.Name)
		for range hop.Subs {
			b.WriteString("[*]")
		}
	}
	return b.String()
}
