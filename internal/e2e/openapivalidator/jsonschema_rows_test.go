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
          items: { type: integer }
        v:
          oneOf:
            - { type: object, required: [y] }
            - $ref: '#/components/schemas/M'
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

// Rows are sorted by JSON pointer, with array indexes in numeric order, and
// each pointer escapes "~" and "/" (RFC 6901).
func TestJSONSchemaRows_SortedAndEscaped(t *testing.T) {
	list := make([]any, 11)
	for i := range list {
		list[i] = i
	}
	list[2], list[10] = "two", "ten"
	body, _ := json.Marshal(map[string]any{"list": list, "m": map[string]any{"b": "s", "a/b~c": "s"}})
	want := []string{
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
