package ingest

import (
	"encoding/json"
	"strings"
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

// TestDecodeStoredJSON_ToleratesTrailingContent pins the split between the two
// decoders.
//
// The ingress decoder requires exactly one JSON value. The read decoder must
// not: a row written by an earlier build could carry trailing content, and
// rejecting it on read would turn a historical write defect into a permanent
// 500 — on that entity and, because one unreadable row fails a listing, on its
// whole model.
func TestDecodeStoredJSON_ToleratesTrailingContent(t *testing.T) {
	const stored = `{"x":1}}}`

	if err := DecodeJSONPreservingNumbers([]byte(stored), new(any)); err == nil {
		t.Error("ingress decoder accepted trailing content; it must reject it")
	}

	var v map[string]any
	if err := DecodeStoredJSON([]byte(stored), &v); err != nil {
		t.Fatalf("read decoder rejected stored data: %v — this makes the row, and its model listing, unreadable", err)
	}
	if v["x"] != json.Number("1") {
		t.Errorf("v[x] = %v, want 1", v["x"])
	}
}

// A body whose objects and arrays nest more than 10 000 deep is not read as
// JSON at all (encoding/json's nesting limit), so it is refused before schema
// validation; cyoda help models states the boundary.
func TestDecodeJSONPreservingNumbers_NestingLimit(t *testing.T) {
	// n containers one inside another, alternating object and array.
	nested := func(n int) []byte {
		var open, close strings.Builder
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				open.WriteString(`{"a":`)
				close.WriteString("}")
			} else {
				open.WriteString("[")
				close.WriteString("]")
			}
		}
		closing := []rune(close.String())
		for i, j := 0, len(closing)-1; i < j; i, j = i+1, j-1 {
			closing[i], closing[j] = closing[j], closing[i]
		}
		return []byte(open.String() + "1" + string(closing))
	}
	var v any
	if err := DecodeJSONPreservingNumbers(nested(10000), &v); err != nil {
		t.Errorf("nested 10 000 deep: %v; want decoded", err)
	}
	err := DecodeJSONPreservingNumbers(nested(10001), &v)
	if err == nil || !strings.Contains(err.Error(), "exceeded max depth") {
		t.Errorf("nested 10 001 deep: %v; want the nesting-limit refusal", err)
	}
	// Breadth is not depth: 10 001 objects side by side in one array decode.
	wide := "[" + strings.TrimSuffix(strings.Repeat(`{"a":1},`, 10001), ",") + "]"
	if err := DecodeJSONPreservingNumbers([]byte(wide), &v); err != nil {
		t.Errorf("10 001 objects in one array: %v; want decoded", err)
	}
}
