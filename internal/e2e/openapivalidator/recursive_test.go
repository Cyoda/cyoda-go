package openapivalidator

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// recursiveFixtureYAML has a recursive condition schema shaped like the
// spec's GroupConditionDto as the validator's view presents it: a group's
// conditions are a discriminated oneOf (the view's replacement of a use of
// the base QueryConditionDto) that includes the group itself. /flat has no
// recursion.
const recursiveFixtureYAML = `openapi: 3.1.0
info: { title: recursive fixture, version: "1" }
paths:
  /group:
    get:
      operationId: getGroup
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Group'
  /flat:
    get:
      operationId: getFlat
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [outer]
                properties:
                  outer:
                    type: object
                    required: [inner]
                    properties:
                      inner:
                        type: object
                        required: [count]
                        properties:
                          count: { type: integer }
components:
  schemas:
    Condition:
      oneOf:
        - $ref: '#/components/schemas/Simple'
        - $ref: '#/components/schemas/Group'
      discriminator:
        propertyName: type
        mapping:
          simple: '#/components/schemas/Simple'
          group: '#/components/schemas/Group'
    Simple:
      type: object
      required: [type, value]
      properties:
        type: { type: string, enum: [simple] }
        value: { type: integer }
    Group:
      type: object
      required: [type, conditions]
      properties:
        type: { type: string, enum: [group] }
        conditions:
          type: array
          items:
            $ref: '#/components/schemas/Condition'
`

// recursiveFixtureDiscriminator is the discriminator block of the fixture's
// Condition union.
const recursiveFixtureDiscriminator = `      discriminator:
        propertyName: type
        mapping:
          simple: '#/components/schemas/Simple'
          group: '#/components/schemas/Group'
`

func loadSpec(t *testing.T, yaml string) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(yaml))
	if err != nil {
		t.Fatalf("load fixture spec: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate fixture spec: %v", err)
	}
	return doc
}

func newSpecValidator(t *testing.T, yaml string) *Validator {
	t.Helper()
	v, err := NewValidator(loadSpec(t, yaml))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func validateBody(t *testing.T, v *Validator, path, body string) []Mismatch {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://x"+path, nil)
	return v.Validate(context.Background(), req, mkResp(200, "application/json", body))
}

func validateRecursiveFixture(t *testing.T, path, body string) []Mismatch {
	t.Helper()
	return validateBody(t, newSpecValidator(t, recursiveFixtureYAML), path, body)
}

// A group inside a group inside a group, whose innermost simple condition
// has a value of the wrong type. kin applies the group schema again to each
// nested value and reports the defect once, as a chain of pointers, each
// relative to the one before.
func TestValidator_RecursiveSchema_DeepMismatchReported(t *testing.T) {
	body := `{"type":"group","conditions":[
		{"type":"group","conditions":[
			{"type":"group","conditions":[
				{"type":"simple","value":"not-an-integer"}
			]}
		]}
	]}`
	mismatches := validateRecursiveFixture(t, "/group", body)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch for the innermost value, got %d: %+v", len(mismatches), mismatches)
	}
	m := mismatches[0]
	if m.Operation != "getGroup" || m.Status != 200 {
		t.Errorf("mismatch identity = (%q, %d), want (getGroup, 200)", m.Operation, m.Status)
	}
	if want := strings.Repeat(`Error at "/conditions/0": doesn't match schema due to: `, 3) + `Error at "/value": value must be an integer`; !strings.Contains(m.Reason, want) {
		t.Errorf("mismatch reason does not locate the innermost value: %s", m.Reason)
	}
}

func TestValidator_RecursiveSchema_ValidDeepBodyPasses(t *testing.T) {
	body := `{"type":"group","conditions":[
		{"type":"simple","value":1},
		{"type":"group","conditions":[
			{"type":"group","conditions":[
				{"type":"simple","value":2},
				{"type":"group","conditions":[]}
			]}
		]}
	]}`
	if mismatches := validateRecursiveFixture(t, "/group", body); len(mismatches) != 0 {
		t.Fatalf("expected no mismatch for a valid deep body, got %+v", mismatches)
	}
}

// One defect gives one row: a union whose discriminator does not resolve (here
// the member has no "type").
func TestValidator_RecursiveSchema_UnresolvedDiscriminatorReportedOnce(t *testing.T) {
	body := `{"type":"group","conditions":[{"value":1}]}`
	mismatches := validateRecursiveFixture(t, "/group", body)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
	}
	if want := `input does not contain the discriminator property "type"`; !strings.Contains(mismatches[0].Reason, want) {
		t.Errorf("mismatch reason = %s, want it to contain %s", mismatches[0].Reason, want)
	}

	// The same at the root: the response schema is the union itself.
	rootPath := "paths:\n  /condition:\n    get:\n      operationId: getCondition\n      responses:\n        \"200\":\n          description: ok\n          content:\n            application/json:\n              schema:\n                $ref: '#/components/schemas/Condition'\n"
	yaml := strings.Replace(recursiveFixtureYAML, "paths:\n", rootPath, 1)
	if yaml == recursiveFixtureYAML {
		t.Fatal("fixture paths block not found")
	}
	root := validateBody(t, newSpecValidator(t, yaml), "/condition", `{"value":1}`)
	if len(root) != 1 {
		t.Fatalf("root union: expected exactly 1 mismatch, got %d: %+v", len(root), root)
	}
}

// One defect gives one row: a defect one level down.
func TestValidator_RecursiveSchema_DepthOneMismatchReportedOnce(t *testing.T) {
	body := `{"type":"group","conditions":[{"type":"simple","value":"not-an-integer"}]}`
	mismatches := validateRecursiveFixture(t, "/group", body)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
	}
}

// One defect gives one row: a null no union member accepts.
func TestValidator_RecursiveSchema_NullMemberReportedOnce(t *testing.T) {
	mismatches := validateRecursiveFixture(t, "/group", `{"type":"group","conditions":[null]}`)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
	}
	if want := `Error at "/conditions/0": doesn't match schema due to: Value is not nullable`; !strings.Contains(mismatches[0].Reason, want) {
		t.Errorf("mismatch reason = %s, want it to contain %s", mismatches[0].Reason, want)
	}
}

func TestValidator_NonRecursiveSchema_Unaffected(t *testing.T) {
	if mismatches := validateRecursiveFixture(t, "/flat", `{"outer":{"inner":{"count":3}}}`); len(mismatches) != 0 {
		t.Fatalf("expected no mismatch for a valid flat body, got %+v", mismatches)
	}
	mismatches := validateRecursiveFixture(t, "/flat", `{"outer":{"inner":{"count":"three"}}}`)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch for an invalid flat body, got %d: %+v", len(mismatches), mismatches)
	}
}

// The usual polymorphism shape: the base has the discriminator and a oneOf of
// subtypes, and each subtype is allOf the base. The base recurs at the same
// pointer, where its outer visit already checks the value, so kin does not
// check it again.
func TestValidator_RecursiveSchema_SamePointerReentryReportedOnce(t *testing.T) {
	const yaml = `openapi: 3.1.0
info: { title: polymorphism fixture, version: "1" }
paths:
  /base:
    get:
      operationId: getBase
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Base'
components:
  schemas:
    Base:
      type: object
      required: [kind]
      properties:
        kind: { type: string }
      discriminator:
        propertyName: kind
        mapping:
          a: '#/components/schemas/SubA'
          b: '#/components/schemas/SubB'
      oneOf:
        - $ref: '#/components/schemas/SubA'
        - $ref: '#/components/schemas/SubB'
    SubA:
      allOf:
        - $ref: '#/components/schemas/Base'
        - type: object
          properties:
            n: { type: integer }
    SubB:
      allOf:
        - $ref: '#/components/schemas/Base'
        - type: object
          properties:
            s: { type: string }
`
	mismatches := validateBody(t, newSpecValidator(t, yaml), "/base", `{"kind":"a","n":"not-an-integer"}`)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
	}
}

// Without a discriminator, the union below a group is decided by trying each
// member, the group itself among them: a value no member accepts is refused
// with each member's reason, and a valid simple or nested group condition
// matches exactly one member.
func TestValidator_RecursiveSchema_UnionWithoutDiscriminatorFailsClosed(t *testing.T) {
	yaml := strings.Replace(recursiveFixtureYAML, recursiveFixtureDiscriminator, "", 1)
	if yaml == recursiveFixtureYAML {
		t.Fatal("fixture discriminator block not found")
	}
	v := newSpecValidator(t, yaml)
	body := `{"type":"group","conditions":[{"type":"simple","value":"not-an-integer"}]}`
	mismatches := validateBody(t, v, "/group", body)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
	}
	m := mismatches[0]
	if m.Operation != "getGroup" || m.Status != 200 {
		t.Errorf("mismatch identity = (%q, %d), want (getGroup, 200)", m.Operation, m.Status)
	}
	for _, want := range []string{
		`Error at "/conditions/0": doesn't match schema due to: Error at "/value": value must be an integer`,
		`Error at "/type": value is not one of the allowed values ["group"]`,
	} {
		if !strings.Contains(m.Reason, want) {
			t.Errorf("mismatch reason = %s\nwant it to contain %s", m.Reason, want)
		}
	}

	for _, valid := range []string{
		`{"type":"group","conditions":[{"type":"simple","value":1}]}`,
		`{"type":"group","conditions":[{"type":"group","conditions":[{"type":"simple","value":1}]}]}`,
	} {
		if mismatches := validateBody(t, v, "/group", valid); len(mismatches) != 0 {
			t.Errorf("valid body %s refused: %+v", valid, mismatches)
		}
	}
}

// anyOf is satisfied by a member that accepts the value: a later member that
// refuses it below a recursion does not make a valid value invalid.
func TestValidator_RecursiveSchema_AnyOfFollowsFirstAcceptingMember(t *testing.T) {
	const yaml = `openapi: 3.1.0
info: { title: anyOf fixture, version: "1" }
paths:
  /node:
    get:
      operationId: getNode
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Node'
components:
  schemas:
    Node:
      anyOf:
        - $ref: '#/components/schemas/Loose'
        - $ref: '#/components/schemas/Strict'
    Loose:
      type: object
      additionalProperties: true
    Strict:
      type: object
      properties:
        n: { type: integer }
        kids:
          type: array
          items:
            $ref: '#/components/schemas/Strict'
`
	body := `{"kids":[{"kids":[{"n":"not-an-integer"}]}]}`
	if mismatches := validateBody(t, newSpecValidator(t, yaml), "/node", body); len(mismatches) != 0 {
		t.Fatalf("expected no mismatch (the Loose member accepts the value), got %+v", mismatches)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestValidator_BodyReadErrorReported(t *testing.T) {
	v := newSpecValidator(t, recursiveFixtureYAML)
	req, _ := http.NewRequest("GET", "http://x/group", nil)
	resp := mkResp(200, "application/json", "")
	resp.Body = io.NopCloser(failingReader{})
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) != 1 || !strings.Contains(mismatches[0].Reason, "failed to read response body") {
		t.Fatalf("expected 1 read-failure mismatch, got %+v", mismatches)
	}
}

// With ExcludeResponseBody kin does not validate the body, nested values
// included.
func TestValidator_RecursiveSchema_ExcludeResponseBodyRespected(t *testing.T) {
	v := newSpecValidator(t, recursiveFixtureYAML)
	v.opts.ExcludeResponseBody = true
	body := `{"type":"group","conditions":[{"type":"group","conditions":[{"type":"group","conditions":[{"type":"simple","value":"x"}]}]}]}`
	if mismatches := validateBody(t, v, "/group", body); len(mismatches) != 0 {
		t.Fatalf("expected no mismatch with ExcludeResponseBody, got %+v", mismatches)
	}
}

// recursiveFixture2020YAML is the fixture with a JSON Schema 2020-12 keyword
// (propertyNames) on the group. kin then checks a schema that reaches the
// group with its JSON Schema 2020-12 validator instead of its own visitor, as
// it does for the spec's workflow export.
var recursiveFixture2020YAML = strings.Replace(recursiveFixtureYAML,
	"    Group:\n      type: object\n",
	"    Group:\n      type: object\n      propertyNames: {pattern: '^[a-z]+$'}\n", 1)

// On kin's JSON Schema 2020-12 path too, a defect below a recursion gives one
// row and a valid nested body passes.
func TestValidator_RecursiveSchema_JSONSchema2020Path(t *testing.T) {
	if recursiveFixture2020YAML == recursiveFixtureYAML {
		t.Fatal("fixture Group block not found")
	}
	v := newSpecValidator(t, recursiveFixture2020YAML)
	for name, body := range map[string]string{
		"a defect three levels down": `{"type":"group","conditions":[{"type":"group","conditions":[
			{"type":"group","conditions":[{"type":"simple","value":"not-an-integer"}]}]}]}`,
		"null in a group in a group":        `{"type":"group","conditions":[{"type":"group","conditions":[null]}]}`,
		"a property name the group refuses": `{"type":"group","conditions":[{"type":"group","conditions":[],"Bad":1}]}`,
	} {
		if mismatches := validateBody(t, v, "/group", body); len(mismatches) != 1 {
			t.Errorf("%s: expected exactly 1 mismatch, got %d: %+v", name, len(mismatches), mismatches)
		}
	}
	// The rows of kin's JSON Schema 2020-12 validator read "validation failed";
	// its own visitor's read "doesn't match schema". The fixture is on the
	// first path only while this holds.
	if m := validateBody(t, v, "/group", `{"type":"group","conditions":[null]}`); len(m) != 1 || !strings.Contains(m[0].Reason, "validation failed") {
		t.Fatalf("the fixture does not reach kin's JSON Schema 2020-12 validator: %+v", m)
	}
	valid := `{"type":"group","conditions":[{"type":"simple","value":1},
		{"type":"group","conditions":[{"type":"group","conditions":[{"type":"simple","value":2}]}]}]}`
	if mismatches := validateBody(t, v, "/group", valid); len(mismatches) != 0 {
		t.Errorf("valid nested body refused: %+v", mismatches)
	}
}

// A response with no body is validated as an empty body, not a panic.
func TestValidator_NilBodyIsAnEmptyBody(t *testing.T) {
	v := newSpecValidator(t, recursiveFixtureYAML)
	req, _ := http.NewRequest("GET", "http://x/group", nil)
	resp := mkResp(200, "application/json", "")
	resp.Body = nil
	for _, m := range v.Validate(context.Background(), req, resp) {
		if strings.Contains(m.Reason, "panic") {
			t.Fatalf("Validate panicked on a nil body: %s", m.Reason)
		}
	}
}

// One defect gives one row: kin refuses a null no member of a root union
// accepts once per member, with the same pointer and reason.
func TestValidator_RootUnionNullReportedOnce(t *testing.T) {
	rootPath := "paths:\n  /condition:\n    get:\n      operationId: getCondition\n      responses:\n        \"200\":\n          description: ok\n          content:\n            application/json:\n              schema:\n                $ref: '#/components/schemas/Condition'\n"
	yaml := strings.Replace(recursiveFixtureYAML, "paths:\n", rootPath, 1)
	if yaml == recursiveFixtureYAML {
		t.Fatal("fixture paths block not found")
	}
	if mismatches := validateBody(t, newSpecValidator(t, yaml), "/condition", `null`); len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
	}
}
