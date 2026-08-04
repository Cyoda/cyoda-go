package ingest

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzRejectUnstorable exercises the in-place scanner against arbitrary input.
//
// The scanner is hand-written and reads bytes the client controls, so the
// properties worth pinning are the ones a parser gets wrong: it must always
// terminate, never panic, and never disagree with a straightforward reference
// walk about whether a well-formed document contains a NUL.
//
// NUL is the reference property because it is the one condition that survives
// Go's decoder intact — unpaired surrogates and invalid UTF-8 are rewritten to
// U+FFFD on decode, which is exactly why the scanner reads raw bytes and why a
// decoded-value reference cannot check those two.
func FuzzRejectUnstorable(f *testing.F) {
	seeds := []string{
		`{}`, `[]`, `null`, `1`, `"s"`, `true`,
		`{"a":1,"b":[1,2,{"c":"d"}]}`,
		`{"a":""}`,
		`{"a":"\ud800"}`,
		`{"a":"😀"}`,
		`{"a":1,"a":2}`,
		`{"a":1e1000000}`,
		`{"a":"x\\y","b":"q\"r"}`,
		`  {  "a"  :  [ 1 , 2 ]  }  `,
		`{"":""}`,
		`[[[[[[[[[[]]]]]]]]]]`,
		`{"a":{"b":{"c":{"d":1}}}}`,
		"{\"a\":\"\xff\"}",
		`{"a":-0.0e-0}`,
		`{"a":`,
		`}`,
		`"unterminated`,
		`{"a":"\`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic and must terminate. The test framework's own timeout
		// catches non-termination.
		err := RejectUnstorable(data)

		// Beyond that, only well-formed documents have a checkable meaning.
		var v any
		if json.Unmarshal(data, &v) != nil {
			return
		}

		// A well-formed document that is valid UTF-8 and contains no NUL rune
		// and no duplicate-looking construct must not be rejected FOR a NUL.
		if err != nil && strings.Contains(err.Error(), "NUL") {
			if !containsNulRune(v) {
				t.Fatalf("rejected for a NUL that the decoded value does not contain: %q -> %v", data, err)
			}
		}

		// The converse: a decoded NUL must always be caught. (Only meaningful
		// when the input is valid UTF-8; otherwise the whole-document UTF-8
		// check fires first and reports a different reason.)
		if utf8.Valid(data) && containsNulRune(v) && err == nil {
			t.Fatalf("accepted a document whose decoded value contains U+0000: %q", data)
		}
	})
}

// containsNulRune reports whether any string in the decoded value contains
// U+0000, walking keys as well as values.
func containsNulRune(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.ContainsRune(k, 0) || containsNulRune(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if containsNulRune(child) {
				return true
			}
		}
	case string:
		return strings.ContainsRune(t, 0)
	}
	return false
}
