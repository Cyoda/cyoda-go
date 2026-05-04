package openapivalidator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// fixtureSpec returns a tiny in-memory OpenAPI 3.1 spec used by fixture tests.
func fixtureSpec(t *testing.T) *openapi3.T {
	t.Helper()
	const yaml = `openapi: 3.1.0
info: { title: fixture, version: "1" }
paths:
  /single:
    get:
      operationId: getSingle
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [transactionId]
                properties:
                  transactionId: { type: string }
                  entityIds: { type: array, items: { type: string } }
  /array:
    get:
      operationId: getArray
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: array
                items:
                  type: object
                  required: [transactionId]
                  properties:
                    transactionId: { type: string }
                    entityIds: { type: array, items: { type: string } }
  /envelope:
    get:
      operationId: getEnvelope
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [type, data, meta]
                properties:
                  type: { type: string }
                  data:
                    type: object
                    additionalProperties: true
                  meta:
                    type: object
                    additionalProperties: true
  /poly:
    get:
      operationId: getPoly
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                additionalProperties: true
  /audit:
    get:
      operationId: getAudit
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                oneOf:
                  - $ref: '#/components/schemas/SMEvent'
                  - $ref: '#/components/schemas/CHEvent'
                discriminator:
                  propertyName: type
                  mapping:
                    StateMachine: '#/components/schemas/SMEvent'
                    EntityChange: '#/components/schemas/CHEvent'
components:
  schemas:
    SMEvent:
      type: object
      required: [type, transition]
      properties:
        type: { type: string, enum: [StateMachine] }
        transition: { type: string }
    CHEvent:
      type: object
      required: [type, fieldPath]
      properties:
        type: { type: string, enum: [EntityChange] }
        fieldPath: { type: string }
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

func newFixtureValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator(fixtureSpec(t))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func mkResp(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Fixture #1 — POST returns array, spec says single object.
// Mirrors #21 confirmed defect.
func TestValidator_PostArrayShape(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/single", nil)
	resp := mkResp(200, "application/json", `[{"transactionId":"abc","entityIds":["e1"]}]`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for array-vs-object body, got none")
	}
}

// Fixture #2 — Spec declares envelope shape; server returns raw entity (missing required fields).
func TestValidator_EnvelopeMissingFields(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/envelope", nil)
	resp := mkResp(200, "application/json", `{"category":"physics"}`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for missing required envelope fields, got none")
	}
}

// Fixture #3 — JSON-in-string content. Spec declares object; server returns
// a JSON string holding the object's representation.
func TestValidator_JSONInStringContent(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/single", nil)
	resp := mkResp(200, "application/json", `"{\"transactionId\":\"abc\"}"`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for stringified object body, got none")
	}
}

// Fixture #4 — undeclared status code. Catches the IncludeResponseStatus
// load-bearing claim.
func TestValidator_UndeclaredStatus(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/single", nil)
	resp := mkResp(418, "application/json", `{}`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for undeclared status 418, got none")
	}
}

// Fixture #5 — polymorphic body (additionalProperties: true) accepts ANY
// JSON shape. The validator must NOT raise a mismatch for arbitrary content.
func TestValidator_PolymorphicAcceptsAnyJSON(t *testing.T) {
	v := newFixtureValidator(t)
	for _, body := range []string{
		`{}`,
		`{"a":1,"b":[1,2,3],"c":{"nested":"object"}}`,
		`{"surprising":["completely","unexpected","fields"]}`,
	} {
		req, _ := http.NewRequest("GET", "http://x/poly", nil)
		resp := mkResp(200, "application/json", body)
		mismatches := v.Validate(context.Background(), req, resp)
		if len(mismatches) > 0 {
			t.Errorf("body %q raised %d mismatches; expected 0", body, len(mismatches))
			for _, m := range mismatches {
				t.Errorf("  - %s", m.Reason)
			}
		}
	}
}

// Fixture #6 — discriminator union accepts each declared variant; rejects
// undeclared.
func TestValidator_DiscriminatorVariants(t *testing.T) {
	v := newFixtureValidator(t)
	cases := []struct {
		name         string
		body         string
		wantMismatch bool
	}{
		{"sm", `{"type":"StateMachine","transition":"approve"}`, false},
		{"ch", `{"type":"EntityChange","fieldPath":"a.b"}`, false},
		{"undeclared", `{"type":"System","payload":"x"}`, true},
		{"sm-missing-required", `{"type":"StateMachine"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "http://x/audit", nil)
			resp := mkResp(200, "application/json", tc.body)
			mismatches := v.Validate(context.Background(), req, resp)
			if tc.wantMismatch && len(mismatches) == 0 {
				t.Errorf("expected mismatch for body %q, got none", tc.body)
			}
			if !tc.wantMismatch && len(mismatches) > 0 {
				t.Errorf("unexpected mismatch for body %q: %+v", tc.body, mismatches)
			}
		})
	}
}

// TestValidator_OverlappingPathTemplates verifies the fallback matcher
// handles paths where multiple templates could match but only one's
// parameter constraints are satisfied by the actual request segments.
func TestValidator_OverlappingPathTemplates(t *testing.T) {
	const yaml = `openapi: 3.1.0
info: { title: overlap-fixture, version: "1" }
paths:
  /entity/{format}:
    post:
      operationId: createCollection
      parameters:
        - name: format
          in: path
          required: true
          schema:
            type: string
            enum: [JSON, XML]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
  /entity/{entityId}:
    get:
      operationId: getOneEntity
      parameters:
        - name: entityId
          in: path
          required: true
          schema:
            type: string
            format: uuid
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
`
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(yaml))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate fixture: %v", err)
	}
	v, err := NewValidator(doc)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// GET /entity/<UUID> — should match getOneEntity, NOT createCollection
	req, _ := http.NewRequest("GET", "http://x/entity/123e4567-e89b-12d3-a456-426614174000", nil)
	resp := mkResp(200, "application/json", `{}`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) != 0 {
		t.Errorf("GET /entity/<UUID> should match getOneEntity (no mismatches); got %d:\n%+v", len(mismatches), mismatches)
	}

	// POST /entity/JSON — should match createCollection
	req2, _ := http.NewRequest("POST", "http://x/entity/JSON", nil)
	resp2 := mkResp(200, "application/json", `{}`)
	mismatches2 := v.Validate(context.Background(), req2, resp2)
	if len(mismatches2) != 0 {
		t.Errorf("POST /entity/JSON should match createCollection (no mismatches); got %d:\n%+v", len(mismatches2), mismatches2)
	}

	// POST /entity/<UUID> — should NOT match anything (UUID isn't in format enum)
	req3, _ := http.NewRequest("POST", "http://x/entity/123e4567-e89b-12d3-a456-426614174000", nil)
	resp3 := mkResp(200, "application/json", `{}`)
	mismatches3 := v.Validate(context.Background(), req3, resp3)
	if len(mismatches3) == 0 {
		t.Errorf("POST /entity/<UUID> should not match any spec route; got 0 mismatches")
	}
}

// Helpers used by the fixture tests above.
var _ = bytes.NewReader
var _ = json.Marshal

// streamingFixtureSpec returns a tiny spec with one operation that declares
// application/x-ndjson for its 200 response.
func streamingFixtureSpec(t *testing.T) *openapi3.T {
	t.Helper()
	const yaml = `openapi: 3.1.0
info: { title: streaming-fixture, version: "1" }
paths:
  /stream:
    get:
      operationId: getStream
      responses:
        "200":
          description: ok
          content:
            application/x-ndjson:
              schema:
                type: array
                items: { type: object }
`
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(yaml))
	if err != nil {
		t.Fatalf("load streaming fixture spec: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate streaming fixture spec: %v", err)
	}
	return doc
}

// TestValidator_StreamingResponseDoesNotPanic — kin-openapi panics when
// input.Body is nil (the `defer body.Close()` line in
// validate_response.go:128). The streaming branch must pass a non-nil body
// and set ExcludeResponseBody so the body is never read.
func TestValidator_StreamingResponseDoesNotPanic(t *testing.T) {
	v, err := NewValidator(streamingFixtureSpec(t))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	req, _ := http.NewRequest("GET", "http://x/stream", nil)
	resp := mkResp(200, "application/x-ndjson", `{"a":1}`+"\n"+`{"a":2}`+"\n")
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) > 0 {
		t.Errorf("streaming response should pass validation; got mismatches: %+v", mismatches)
	}
}
