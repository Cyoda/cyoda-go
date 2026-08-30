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
// slice ("[0:2]"), a union ("[0,1]"), or an unclosed bracket — is left
// verbatim. There is no key it could canonicalise to, and no evaluator in the
// stack resolves it.
//
// The path is canonicalised for LOOKUP only. Callers must keep the caller's
// original spelling for anything they echo back or report, so a diagnostic
// names the path the request actually sent.
func CanonicalFieldPath(path string) string {
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
			b.WriteString(path[i:])
			break
		}
		if IsArrayIndex(path[i+1 : i+rel]) {
			b.WriteString("[*]")
		} else {
			b.WriteString(path[i : i+rel+1])
		}
		i += rel + 1
	}
	return b.String()
}

// IsArrayIndex reports whether s is the content of a subscript that addresses
// a single array element: a non-negative decimal integer. It delegates to
// [spi.IsArrayIndex] — the SPI's single definition of a well-formed array
// index (see that function's doc comment) — rather than keeping a second copy
// that could drift from it. This is the digit-class check only, the same
// contract spi.IsArrayIndex documents: it says nothing about whether the run
// fits an int, which is spi.ParseFilterPath's concern where magnitude matters.
func IsArrayIndex(s string) bool {
	return spi.IsArrayIndex(s)
}
