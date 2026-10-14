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
// spec's GroupConditionDto: a group's conditions are a discriminated oneOf
// that includes the group itself. /flat has no recursion.
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
// has a value of the wrong type. kin skips the nested groups, because the
// group schema is already being visited.
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
	if !strings.Contains(m.Reason, "/conditions/0/conditions/0") || !strings.Contains(m.Reason, `"/value": value must be an integer`) {
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

// A mismatch kin already reports (no recursive re-entry on its path) is
// reported once. The re-entry case of the same rule is
// TestValidator_RecursiveSchema_SamePointerReentryReportedOnce.
func TestValidator_RecursiveSchema_DepthOneMismatchReportedOnce(t *testing.T) {
	body := `{"type":"group","conditions":[{"type":"simple","value":"not-an-integer"}]}`
	mismatches := validateRecursiveFixture(t, "/group", body)
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %+v", len(mismatches), mismatches)
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
// subtypes, and each subtype is allOf the base. The base re-enters at the
// same pointer, where its outer visit already checks the value, so the walk
// does not check it again.
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

// Without a discriminator, kin accepts the group member of the union without
// looking at the value (the group schema is already being visited), and so
// accepts the union, while a fresh check rejects every member. The walk then
// checks the union itself.
func TestValidator_RecursiveSchema_UnionWithoutDiscriminatorFailsClosed(t *testing.T) {
	yaml := strings.Replace(recursiveFixtureYAML, recursiveFixtureDiscriminator, "", 1)
	if yaml == recursiveFixtureYAML {
		t.Fatal("fixture discriminator block not found")
	}
	// No valid-body case here: for a valid simple condition kin itself reports
	// "matches more than one oneOf schemas", because it accepts the group
	// member unseen.
	body := `{"type":"group","conditions":[{"type":"simple","value":"not-an-integer"}]}`
	mismatches := validateBody(t, newSpecValidator(t, yaml), "/group", body)
	// The union at the re-entered pointer is checked afresh, and it fails
	// because every member rejects the value: one row per member, in the
	// union's member order — the simple member on its value, the group
	// member on its type.
	want := []string{
		`recursive schema at "/conditions/0": Error at "/value": value must be an integer`,
		`recursive schema at "/conditions/0": Error at "/type": value is not one of the allowed values ["group"]`,
	}
	if len(mismatches) != len(want) {
		t.Fatalf("expected %d mismatches, one per union member, got %d: %+v", len(want), len(mismatches), mismatches)
	}
	for i, m := range mismatches {
		if m.Operation != "getGroup" || m.Status != 200 {
			t.Errorf("mismatch %d identity = (%q, %d), want (getGroup, 200)", i, m.Operation, m.Status)
		}
		if !strings.HasPrefix(m.Reason, want[i]) {
			t.Errorf("mismatch %d reason = %s\nwant prefix %s", i, m.Reason, want[i])
		}
	}
}

// anyOf is satisfied by its first member that accepts the value, as kin
// checks it. A later member that also passes the fresh check, but has an
// error under a re-entry, does not make a valid value invalid.
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

// With ExcludeResponseBody kin does not validate the body; the walk does not
// either.
func TestValidator_RecursiveSchema_ExcludeResponseBodyRespected(t *testing.T) {
	v := newSpecValidator(t, recursiveFixtureYAML)
	v.opts.ExcludeResponseBody = true
	body := `{"type":"group","conditions":[{"type":"group","conditions":[{"type":"group","conditions":[{"type":"simple","value":"x"}]}]}]}`
	if mismatches := validateBody(t, v, "/group", body); len(mismatches) != 0 {
		t.Fatalf("expected no mismatch with ExcludeResponseBody, got %+v", mismatches)
	}
}

// Only schemas from which a schema cycle is reachable are walked.
func TestRecursiveSchemas(t *testing.T) {
	doc := loadSpec(t, recursiveFixtureYAML)
	schemas := doc.Components.Schemas
	flat := doc.Paths.Value("/flat").Get.Responses.Status(200).Value.Content.Get("application/json").Schema.Value
	got := recursiveSchemas(doc)
	for name, want := range map[string]bool{"Condition": true, "Group": true, "Simple": false} {
		if got[schemas[name].Value] != want {
			t.Errorf("recursiveSchemas[%s] = %v, want %v", name, got[schemas[name].Value], want)
		}
	}
	if !got[schemas["Group"].Value.Properties["conditions"].Value] {
		t.Error("recursiveSchemas[Group.conditions] = false, want true")
	}
	if got[flat] {
		t.Error("recursiveSchemas[/flat response] = true, want false")
	}
}
