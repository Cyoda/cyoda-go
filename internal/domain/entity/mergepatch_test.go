package entity

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustParse(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := decodeJSONPreservingNumbers([]byte(s), &v); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func TestApplyMergePatch_RFC7386_AppendixA(t *testing.T) {
	cases := []struct{ name, target, patch, want string }{
		{name: "replace-field", target: `{"a":"b"}`, patch: `{"a":"c"}`, want: `{"a":"c"}`},
		{name: "add-field", target: `{"a":"b"}`, patch: `{"b":"c"}`, want: `{"a":"b","b":"c"}`},
		{name: "delete-field", target: `{"a":"b"}`, patch: `{"a":null}`, want: `{}`},
		{name: "delete-one-of-two", target: `{"a":"b","b":"c"}`, patch: `{"a":null}`, want: `{"b":"c"}`},
		{name: "array-replaces", target: `{"a":["b"]}`, patch: `{"a":"c"}`, want: `{"a":"c"}`},
		{name: "scalar-replaces-array", target: `{"a":"c"}`, patch: `{"a":["b"]}`, want: `{"a":["b"]}`},
		{name: "nested-merge", target: `{"a":{"b":"c"}}`, patch: `{"a":{"b":"d","c":null}}`, want: `{"a":{"b":"d"}}`},
		{name: "array-wholesale", target: `{"a":[{"b":"c"}]}`, patch: `{"a":[1]}`, want: `{"a":[1]}`},
		{name: "non-object-patch-replaces", target: `{"a":"b"}`, patch: `["c"]`, want: `["c"]`},
		{name: "empty-patch-noop", target: `{"a":"b"}`, patch: `{}`, want: `{"a":"b"}`},
		{name: "null-creates-nothing", target: `{"a":"foo"}`, patch: `null`, want: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeMergePatch(json.RawMessage(tc.target), mustParse(t, tc.patch))
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			gotBytes, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}
			var gotN, wantN any
			if err := decodeJSONPreservingNumbers(gotBytes, &gotN); err != nil {
				t.Fatalf("decode got: %v", err)
			}
			if err := decodeJSONPreservingNumbers([]byte(tc.want), &wantN); err != nil {
				t.Fatalf("decode want: %v", err)
			}
			if !reflect.DeepEqual(gotN, wantN) {
				t.Errorf("got %s, want %s", gotBytes, tc.want)
			}
		})
	}
}

func TestApplyMergePatch_NumberFidelity(t *testing.T) {
	// int64 above 2^53 must survive without float64 coercion.
	target := json.RawMessage(`{"big":1}`)
	patch := mustParse(t, `{"big":9007199254740993}`)
	got, err := mergeMergePatch(target, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(b) != `{"big":9007199254740993}` {
		t.Errorf("number fidelity lost: got %s", b)
	}
}
