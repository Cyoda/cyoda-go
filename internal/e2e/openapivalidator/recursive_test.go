package openapivalidator

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// recursiveFixtureSpec has a recursive condition schema shaped like the
// spec's GroupConditionDto: a group's conditions are a discriminated oneOf
// that includes the group itself. /flat has no recursion.
func recursiveFixtureSpec(t *testing.T) *openapi3.T {
	t.Helper()
	const yaml = `openapi: 3.1.0
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

func validateRecursiveFixture(t *testing.T, path, body string) []Mismatch {
	t.Helper()
	v, err := NewValidator(recursiveFixtureSpec(t))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	req, _ := http.NewRequest("GET", "http://x"+path, nil)
	return v.Validate(context.Background(), req, mkResp(200, "application/json", body))
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
	if !strings.Contains(m.Reason, "/conditions/0/conditions/0") || !strings.Contains(m.Reason, "value") {
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
// reported once, not again by the recursive walk.
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
