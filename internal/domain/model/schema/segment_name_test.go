package schema

import "testing"

// TestIsSegmentNameByte pins the charset itself, byte by byte, across the whole
// 0-255 range. The class is the platform's single answer to "can a field ever
// be addressed?", so an accidental widening (a stray punctuation byte) or
// narrowing (dropping "-") is a silent contract change on both the model side
// and the query side — the exhaustive sweep is what makes that impossible to
// slip through.
func TestIsSegmentNameByte(t *testing.T) {
	admits := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
			b >= '0' && b <= '9' || b == '_' || b == '-'
	}
	for i := 0; i < 256; i++ {
		b := byte(i)
		if got, want := IsSegmentNameByte(b), admits(b); got != want {
			t.Errorf("IsSegmentNameByte(%q) = %v, want %v", b, got, want)
		}
	}
}

// TestIsSegmentName covers the whole-string predicate: non-empty and every
// byte admissible. The rejected cases are the spellings that reach it in
// practice — a dotted name (which would spell two segments), a subscript, a
// bracket-quoted key, and non-ASCII text — each of which must be refused
// rather than recorded as a field nothing could query.
func TestIsSegmentName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"lower", "amount", true},
		{"upper", "AMOUNT", true},
		{"mixed case", "firstName", true},
		{"underscore only", "_", true},
		{"hyphen only", "-", true},
		{"leading underscore", "_meta", true},
		{"snake case", "first_name", true},
		{"hyphenated", "first-name", true},
		{"digits", "123", true},
		{"leading digit", "0abc", true},
		{"mixed charset", "x_1-2A", true},

		{"empty", "", false},
		{"dot", "first.name", false},
		{"leading dot", ".name", false},
		{"trailing dot", "name.", false},
		{"space", "first name", false},
		{"subscript", "a[0]", false},
		{"wildcard subscript", "a[*]", false},
		{"bracket quoted", "['x']", false},
		{"dollar", "$ref", false},
		{"leader", "$.a", false},
		{"at sign", "@type", false},
		{"colon", "ns:field", false},
		{"slash", "a/b", false},
		{"single quote", "it's", false},
		{"double quote", `he"llo`, false},
		{"asterisk", "a*", false},
		{"non-ascii accent", "café", false},
		{"non-ascii cjk", "日本", false},
		{"nul byte", "a\x00", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSegmentName(tt.in); got != tt.want {
				t.Errorf("IsSegmentName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
