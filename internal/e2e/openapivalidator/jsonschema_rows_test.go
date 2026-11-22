package openapivalidator

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// rowsFixtureYAML reaches a JSON Schema 2020-12 keyword (propertyNames) from
// every response, so kin checks each with its JSON Schema 2020-12 validator.
const rowsFixtureYAML = `openapi: 3.1.0
info: { title: rows fixture, version: "1" }
paths:
  /holder:
    get:
      operationId: getHolder
      responses:
        "200":
          description: ok
          headers:
            X-Count:
              schema: { type: integer, minimum: 0 }
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Holder'
components:
  schemas:
    Holder:
      type: object
      propertyNames: {pattern: '^[a-z]+$'}
      properties:
        m:
          type: object
          additionalProperties: { type: integer }
        list:
          type: array
          maxItems: 11
          items: { type: integer }
        v:
          oneOf:
            - { type: object, required: [y] }
            - $ref: '#/components/schemas/M'
        w:
          oneOf:
            - { type: object, required: [y] }
            - anyOf:
                - { type: string }
                - { type: object, required: [x] }
        ww:
          oneOf:
            - { type: object, required: [y] }
            - oneOf:
                - { type: string }
                - { type: object, required: [x] }
        u:
          $ref: '#/components/schemas/K'
    K:
      type: object
      required: [kind]
      properties:
        kind: { type: string }
      discriminator:
        propertyName: kind
        mapping:
          a: '#/components/schemas/A'
          b: '#/components/schemas/B'
    A:
      allOf:
        - $ref: '#/components/schemas/K'
        - type: object
          properties:
            sub: { type: object, required: [kind] }
    B:
      allOf:
        - $ref: '#/components/schemas/K'
        - type: object
          required: [s]
          properties:
            s: { type: string }
    M:
      oneOf:
        - { type: string }
        - type: object
          required: [x]
          properties:
            x: { type: integer }
`

func holderRows(t *testing.T, body string, header http.Header) []string {
	t.Helper()
	v := newSpecValidator(t, rowsFixtureYAML)
	req, _ := http.NewRequest("GET", "http://x/holder", nil)
	resp := mkResp(200, "application/json", body)
	for k, vs := range header {
		resp.Header[k] = vs
	}
	var out []string
	for _, m := range v.Validate(context.Background(), req, resp) {
		out = append(out, m.Reason)
	}
	return out
}

// Rows are sorted by JSON pointer, a pointer before the pointers below it and
// array indexes in numeric order, and each pointer escapes "~" and "/"
// (RFC 6901).
func TestJSONSchemaRows_SortedAndEscaped(t *testing.T) {
	list := make([]any, 12)
	for i := range list {
		list[i] = i
	}
	list[2], list[10] = "two", "ten"
	body, _ := json.Marshal(map[string]any{"list": list, "m": map[string]any{"b": "s", "a/b~c": "s"}})
	want := []string{
		`Error at "/list": maxItems: got 12, want 11`,
		`Error at "/list/2": got string, want integer`,
		`Error at "/list/10": got string, want integer`,
		`Error at "/m/a~1b~0c": got string, want integer`,
		`Error at "/m/b": got string, want integer`,
	}
	if got := holderRows(t, string(body), nil); !slices.Equal(got, want) {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
}

// A union member that holds a union of its own is kept when one of its own
// members refuses the value's kind: the value may be its other member.
func TestJSONSchemaRows_NestedUnionMemberKept(t *testing.T) {
	got := holderRows(t, `{"v":{"x":"s"}}`, nil)
	if !slices.ContainsFunc(got, func(r string) bool { return strings.HasPrefix(r, `Error at "/v/x": `) }) {
		t.Errorf("the defect at /v/x is not reported: %q", got)
	}
}

// A header's rows name the header.
func TestJSONSchemaRows_HeaderRowNamesTheHeader(t *testing.T) {
	got := holderRows(t, `{}`, http.Header{"X-Count": {"-1"}})
	if len(got) != 1 || !strings.Contains(got[0], `header "X-Count"`) {
		t.Errorf("want one row naming the header X-Count, got %q", got)
	}
}

// A lone error of kin's own visitor keeps kin's text, which names the body.
func TestValidator_VisitorRowKeepsItsReason(t *testing.T) {
	m := validateRecursiveFixture(t, "/group", `[]`)
	if len(m) != 1 || !strings.Contains(m[0].Reason, "response body doesn't match schema") {
		t.Errorf("want one row naming the response body, got %+v", m)
	}
}

// A union member that is itself a union, inline, is not dropped for one of
// its branches: its own branches are judged, and the defects of those the
// value can be are reported.
func TestJSONSchemaRows_InlineUnionMemberKept(t *testing.T) {
	for _, prop := range []string{"w", "ww"} {
		got := holderRows(t, `{"`+prop+`":{}}`, nil)
		want := []string{
			`Error at "/` + prop + `": missing property 'x'`,
			`Error at "/` + prop + `": missing property 'y'`,
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got  %q\n want %q", prop, got, want)
		}
	}
}

// A member refuses the key only through a failure at the key itself or a
// required list at the union's own value: a nested object that requires a
// property named like the key does not make the member refuse the key.
func TestJSONSchemaRows_KeyNamedPropertyBelowTheMember(t *testing.T) {
	want := []string{`Error at "/u/sub": missing property 'kind'`}
	if got := holderRows(t, `{"u":{"kind":"a","sub":{}}}`, nil); !slices.Equal(got, want) {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
}

// The JSON Schema library checks a property name as a value of its own and
// does not report where the object is (santhosh-tekuri/jsonschema v6.0.3
// sets its location without copying it), so a property name's row says it is
// a property name and gives no pointer.
func TestJSONSchemaRows_PropertyNameRow(t *testing.T) {
	want := []string{`Error at a property name: 'Bad' does not match pattern '^[a-z]+$'`}
	if got := holderRows(t, `{"Bad":1}`, nil); !slices.Equal(got, want) {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
}
