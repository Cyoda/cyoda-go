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
		{"replace-field", `{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{"add-field", `{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{"delete-field", `{"a":"b"}`, `{"a":null}`, `{}`},
		{"delete-one-of-two", `{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{"array-replaces", `{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{"scalar-replaces-array", `{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{"nested-merge", `{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{"array-wholesale", `{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{"non-object-patch-replaces", `{"a":"b"}`, `["c"]`, `["c"]`},
		{"empty-patch-noop", `{"a":"b"}`, `{}`, `{"a":"b"}`},
		{"null-creates-nothing", `{"a":"foo"}`, `null`, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeMergePatch(json.RawMessage(tc.target), mustParse(t, tc.patch))
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			gotBytes, _ := json.Marshal(got)
			var gotN, wantN any
			_ = decodeJSONPreservingNumbers(gotBytes, &gotN)
			_ = decodeJSONPreservingNumbers([]byte(tc.want), &wantN)
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
	b, _ := json.Marshal(got)
	if string(b) != `{"big":9007199254740993}` {
		t.Errorf("number fidelity lost: got %s", b)
	}
}
