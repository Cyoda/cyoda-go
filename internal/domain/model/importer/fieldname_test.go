package importer_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// decodeJSON parses src the way every ingress does (json.Number preserved) so
// the walker sees exactly the tree production code hands it.
func decodeJSON(t *testing.T, src string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", src, err)
	}
	return v
}

// TestWalk_RejectsUnaddressableFieldName pins the model-side half of the
// platform's one field-name rule: a name the wire jsonPath grammar cannot
// spell is never recorded in a schema, because nothing could ever query it.
func TestWalk_RejectsUnaddressableFieldName(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		// wantIn is a fragment the diagnostic must carry so an operator can
		// tell WHICH key to rename.
		wantIn []string
	}{
		{"non-ascii accent", `{"café": 5}`, []string{`"café"`, `"$"`}},
		{"non-ascii tilde", `{"año": 2024}`, []string{`"año"`}},
		{"non-ascii cjk", `{"日本": 1}`, []string{`"日本"`}},
		{"space", `{"first name": "x"}`, []string{`"first name"`}},
		{"double quote", `{"he\"llo": 1}`, []string{`he\"llo`}},
		{"single quote", `{"it's": 1}`, []string{`"it's"`}},
		{"dot in key", `{"first.name": 1}`, []string{`"first.name"`}},
		{"empty key", `{"": 1}`, []string{`""`}},
		{"bracket subscript", `{"a[0]": 1}`, []string{`"a[0]"`}},
		{"bracket quoted", `{"['x']": 1}`, []string{`"['x']"`}},
		{"dollar", `{"$ref": 1}`, []string{`"$ref"`}},
		{"at sign", `{"@type": 1}`, []string{`"@type"`}},
		{"xml namespace colon", `{"ns:field": 1}`, []string{`"ns:field"`}},
		{"slash", `{"a/b": 1}`, []string{`"a/b"`}},
		// The evaluator's own metacharacters, excluded DELIBERATELY rather than
		// as a side effect of the charset being an allowlist. gjson reads "|" as
		// an alternative segment separator, so "a|b" is the "." collision by
		// another spelling — answered by a nested a→b where one exists. "*" and
		// "?" are key wildcards that match a DIFFERENT key than the one written,
		// "!" introduces a literal, "#" is the array count/projection segment
		// (so the same name means "this key" over an object and "how many
		// elements" over an array), and a backslash is the escape. Each is a
		// silent wrong answer rather than a miss. See schema.IsSegmentNameByte.
		{"gjson key wildcard star", `{"a*b": 1}`, []string{`"a*b"`}},
		{"gjson key wildcard question", `{"a?b": 1}`, []string{`"a?b"`}},
		{"gjson count-or-projection segment", `{"#": 1}`, []string{`"#"`}},
		{"gjson segment separator pipe", `{"a|b": 1}`, []string{`"a|b"`}},
		{"gjson literal bang", `{"!true": 1}`, []string{`"!true"`}},
		{"gjson escape backslash", `{"a\\b": 1}`, []string{`a\\b`}},
		{"nested object", `{"ok": {"bad name": 1}}`, []string{`"bad name"`, `"$.ok"`}},
		{"inside array element", `{"arr": [{"bad name": 1}]}`, []string{`"bad name"`, `"$.arr[*]"`}},
		{"deep nesting", `{"a": {"b": [[{"c d": 1}]]}}`, []string{`"c d"`, `"$.a.b[*][*]"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := importer.Walk(decodeJSON(t, c.doc))
			if err == nil {
				t.Fatalf("Walk(%s) must reject an unaddressable field name", c.doc)
			}
			if !errors.Is(err, importer.ErrInvalidFieldName) {
				t.Fatalf("Walk(%s) error must wrap ErrInvalidFieldName, got %v", c.doc, err)
			}
			for _, want := range c.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("diagnostic must contain %s, got %q", want, err.Error())
				}
			}
			// The operator has to learn what IS allowed, not just what failed.
			if !strings.Contains(err.Error(), "_") || !strings.Contains(err.Error(), "-") {
				t.Errorf("diagnostic must state the allowed character set, got %q", err.Error())
			}
		})
	}
}

// TestWalk_AcceptsAddressableFieldName is the positive control: every name the
// grammar can spell must still import unchanged. A regression here silently
// breaks working tenants, so the table deliberately covers the whole segment
// charset plus nested containers.
func TestWalk_AcceptsAddressableFieldName(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"underscore only", `{"_": 1}`},
		{"hyphen only", `{"-": 1}`},
		{"leading underscore", `{"_meta": 1}`},
		{"xml text marker", `{"_text": "v"}`},
		{"hyphenated", `{"first-name": "x"}`},
		{"snake case", `{"first_name": "x"}`},
		{"camel case", `{"firstName": "x"}`},
		{"upper case", `{"AMOUNT": 1}`},
		{"digits", `{"123": 1}`},
		{"leading digit", `{"0abc": 1}`},
		{"mixed", `{"x_1-2A": 1}`},
		{"nested objects", `{"a": {"b-c": {"d_1": 1}}}`},
		{"nested arrays", `{"a": [[{"b_2": 1}]]}`},
		{"array of scalars", `{"tags": ["a", "b"]}`},
		{"empty object", `{}`},
		{"empty array", `{"a": []}`},
		{"null leaf", `{"a": null}`},
		{"scalar root", `"just a string"`},
		{"array root", `[{"ok": 1}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := importer.Walk(decodeJSON(t, c.doc)); err != nil {
				t.Fatalf("Walk(%s) must succeed, got %v", c.doc, err)
			}
		})
	}
}

// TestWalk_AcceptedNamesAreQueryable closes the loop the rule exists for:
// every name Walk accepts must produce a FieldsMap key the query-side path
// validator accepts. The "[*]" array hops are elided first — a condition path
// may carry a subscript, and the grammar short-circuits there, so removing them
// is what forces EVERY segment of a nested path through the charset check.
func TestWalk_AcceptedNamesAreQueryable(t *testing.T) {
	node, err := importer.Walk(decodeJSON(t,
		`{"_":1,"-":2,"_meta":3,"first-name":"x","first_name":"y","AMOUNT":1,"0abc":1,"x_1-2A":1,"a":{"b-c":[{"d_1":1}]}}`))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	paths := node.FieldsMap()
	if len(paths) == 0 {
		t.Fatal("FieldsMap must not be empty")
	}
	for path := range paths {
		flat := strings.ReplaceAll(path, "[*]", "")
		if err := search.ValidateScalarJSONPath(flat); err != nil {
			t.Errorf("accepted field produced unqueryable path %q: %v", path, err)
		}
	}
}
