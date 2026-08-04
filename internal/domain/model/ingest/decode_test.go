package ingest

import (
	"encoding/json"
	"testing"
)

// TestDecodeJSONPreservingNumbers_RejectsTrailingContent pins the boundary
// contract: a request body must be exactly one JSON value. json.Decoder.Decode
// stops at the end of the first value and ignores whatever follows, so without
// an explicit end-of-stream check a body like `{"x":1}}}` parses "successfully"
// — and the raw bytes that get persisted then differ from the value that was
// validated, surfacing as a 500 from storage instead of a 400 at the boundary.
func TestDecodeJSONPreservingNumbers_RejectsTrailingContent(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// Valid: exactly one JSON value, optionally surrounded by whitespace.
		{"object", `{"x":1}`, false},
		{"array", `[1,2,3]`, false},
		{"string", `"hello"`, false},
		{"number", `42`, false},
		{"null", `null`, false},
		{"leading-whitespace", "  \n\t" + `{"x":1}`, false},
		{"trailing-whitespace", `{"x":1}` + "  \n\t", false},

		// Invalid: more than one value, or junk after the first.
		{"trailing-braces", `{"x":1}}}`, true},
		{"two-objects", `{"x":1}{"y":2}`, true},
		{"object-then-garbage", `{"x":1} nonsense`, true},
		{"array-then-object", `[1,2] {"y":2}`, true},
		{"number-then-number", `1 2`, true},

		// Invalid: not a complete JSON value at all.
		{"truncated", `{"x":1`, true},
		{"not-json", `this is not json`, true},
		{"empty", ``, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			err := DecodeJSONPreservingNumbers([]byte(tc.input), &v)
			if tc.wantErr && err == nil {
				t.Fatalf("decode(%q) = nil error; want an error", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("decode(%q) = %v; want no error", tc.input, err)
			}
		})
	}
}

// TestDecodeJSONPreservingNumbers_KeepsNumberPrecision guards the property the
// helper exists for in the first place, so the trailing-content check cannot
// regress it.
func TestDecodeJSONPreservingNumbers_KeepsNumberPrecision(t *testing.T) {
	const big = `{"n":123456789012345678901234567890.123456789}`
	var v map[string]any
	if err := DecodeJSONPreservingNumbers([]byte(big), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	n, ok := v["n"].(json.Number)
	if !ok {
		t.Fatalf("v[n] is %T, want json.Number — UseNumber lost", v["n"])
	}
	if n.String() != "123456789012345678901234567890.123456789" {
		t.Errorf("n = %s; precision lost", n.String())
	}
}
