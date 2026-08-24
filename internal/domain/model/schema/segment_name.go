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
