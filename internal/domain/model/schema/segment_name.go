package schema

// The character class a path segment name is drawn from: ASCII letters,
// digits, "_" and "-". It is the platform's single answer to "can this field
// ever be addressed?", and both halves must agree on it:
//
//   - The MODEL side records a field only if its name is spellable as a
//     segment. A name outside the class would be stored and never queryable —
//     silently wrong rather than unavailable.
//   - The QUERY side admits a wire jsonPath only if every segment is drawn
//     from it, so a path the grammar accepts is one an evaluator resolves.
//
// It lives here, beside [CanonicalFieldPath] and [IsArrayIndex], because it is
// a property of a field path — not of searching. The path GRAMMAR (the "$."
// leader, dotted segments, subscripts, and their diagnostics) stays in
// internal/domain/search, which owns the wire surface and pins its agreement
// with the SPI translator; that grammar consults these predicates the same way
// it consults [IsArrayIndex]. Model import depends on the character class
// alone, so it depends on this package alone.
//
// What the class EXCLUDES is deliberate, not a by-product of writing it as an
// allowlist. Two spellings collapse a name onto a nested path: "." and — in
// the in-memory evaluator — "|", which gjson reads as an alternative segment
// separator, so a field named "a|b" is answered by a nested a→b if the
// document has one and by nothing if it does not. The rest are read as
// instructions rather than names: "*" and "?" are key wildcards that match a
// DIFFERENT key than the one written, "@" introduces a modifier, "!"
// introduces a literal ("!true" yields true whatever the document holds), "#"
// is the array count/projection segment, so the same name means "this key"
// over an object and "how many elements" over an array, and a backslash is the
// escape, so an unescaped one addresses a different key.
//
// None of them merely fails to find the field. "|" and the backslash resolve
// to a different node outright; "*" and "?" do whenever a sibling key matches
// the glob; "!" ignores the document; and "#" is answered correctly over an
// object and as a count over an array, so one name means two things. A silent
// wrong answer is possible in every case, which is why the class refuses them
// at the model door instead of leaving each path-building site to escape
// them. Widening it is therefore not a charset
// edit: it obliges an escape at each such site, in both SQL plugins' path
// literals, and a wire syntax able to spell the segment at all.

// IsSegmentNameByte reports whether b is admissible inside a path segment name.
func IsSegmentNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_' || b == '-'
}

// IsSegmentName reports whether s is a complete, well-formed segment name: at
// least one byte, every byte admissible. A "." fails it like any other
// disallowed byte, which is what makes it the right check for a bare FIELD
// name — a dotted name would otherwise spell two segments and name a field no
// lookup could distinguish from a nested one.
func IsSegmentName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !IsSegmentNameByte(s[i]) {
			return false
		}
	}
	return true
}
