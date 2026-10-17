package openapivalidator

import (
	"encoding/json"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Two validators, as a 3.1 consumer and the e2e response validator use them.
var viewValidators = map[string][]openapi3.SchemaValidationOption{
	"default":             nil,
	"json-schema-2020-12": {openapi3.EnableJSONSchema2020()},
}

const viewFixture = `openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Shape:
      type: object
      required: [kind]
      properties:
        kind: {type: string}
      discriminator:
        propertyName: kind
        mapping:
          a: '#/components/schemas/A'
          b: '#/components/schemas/B'
          g: '#/components/schemas/G'
    A:
      allOf:
        - $ref: '#/components/schemas/Shape'
        - type: object
          required: [x]
          properties:
            x: {type: integer}
    B:
      allOf:
        - $ref: '#/components/schemas/Shape'
        - type: object
          required: [y]
          properties:
            y: {type: string}
    G:
      allOf:
        - $ref: '#/components/schemas/Shape'
        - type: object
          properties:
            items:
              type: array
              items:
                $ref: '#/components/schemas/Shape'
            opt:
              oneOf:
                - type: "null"
                - $ref: '#/components/schemas/Shape'
    Holder:
      type: object
      properties:
        one:
          $ref: '#/components/schemas/Shape'
        many:
          type: array
          items:
            $ref: '#/components/schemas/Shape'
        opt:
          oneOf:
            - type: "null"
            - $ref: '#/components/schemas/Shape'
        leaf:
          not: {type: object}
        notNull:
          not: {type: [object, "null"]}
`

func viewCheck(t *testing.T, s *openapi3.Schema, doc string, wantOK bool) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("bad test JSON %s: %v", doc, err)
	}
	for mode, opts := range viewValidators {
		err := s.VisitJSON(v, opts...)
		if wantOK && err != nil {
			t.Errorf("%s: %s refused: %v", mode, doc, err)
		}
		if !wantOK && err == nil {
			t.Errorf("%s: %s accepted", mode, doc)
		}
	}
}

func TestApplyView(t *testing.T) {
	load := func(t *testing.T) (*openapi3.T, *openapi3.Schema) {
		t.Helper()
		doc := loadSpec(t, viewFixture)
		if err := ApplyView(doc); err != nil {
			t.Fatalf("ApplyView: %v", err)
		}
		return doc, doc.Components.Schemas["Holder"].Value
	}

	t.Run("member_defect_under_use_is_reported", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"one":{"kind":"a","x":"no"}}`, false)
		viewCheck(t, h, `{"one":{"kind":"a","x":1}}`, true)
	})
	t.Run("unmapped_value_is_reported", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"one":{"kind":"zzz"}}`, false)
	})
	t.Run("defect_in_array_items_and_nested_group_is_reported", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"many":[{"kind":"g","items":[{"kind":"a","x":"no"}]}]}`, false)
		viewCheck(t, h, `{"many":[{"kind":"g","items":[{"kind":"a","x":1}]}]}`, true)
	})
	t.Run("defect_inside_nullable_oneOf_is_reported", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"opt":{"kind":"a","x":"no"}}`, false)
		viewCheck(t, h, `{"opt":null}`, true)
	})
	// kin-openapi accepts any value for a schema it is already visiting
	// higher up, so null reaching a recursive member through a nullable
	// reference matched both branches of the oneOf.
	t.Run("null_passes_nullable_oneOf_under_a_recursive_member", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"many":[{"kind":"g","opt":null}]}`, true)
		viewCheck(t, h, `{"many":[{"kind":"g","opt":{"kind":"a","x":"no"}}]}`, false)
	})
	t.Run("null_passes_untyped_leaf", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"leaf":null}`, true)
		viewCheck(t, h, `{"leaf":{}}`, false)
	})
	t.Run("null_fails_leaf_whose_not_excludes_it", func(t *testing.T) {
		_, h := load(t)
		viewCheck(t, h, `{"notNull":null}`, false)
	})
	t.Run("null_fails_member_and_replaced_use", func(t *testing.T) {
		doc, h := load(t)
		viewCheck(t, h, `{"one":null}`, false)
		viewCheck(t, doc.Components.Schemas["A"].Value, `null`, false)
	})
	t.Run("component_use", func(t *testing.T) {
		doc, _ := load(t)
		base, err := ComponentUse(doc, "Shape")
		if err != nil {
			t.Fatalf("ComponentUse(Shape): %v", err)
		}
		if len(base.OneOf) != 3 || base.Discriminator == nil {
			t.Fatalf("a use of the base Shape is not the oneOf of its mapping: %+v", base)
		}
		viewCheck(t, base, `{"kind":"a","x":1}`, true)
		viewCheck(t, base, `{"kind":"zzz"}`, false)
		viewCheck(t, base, `{"kind":"a","x":"no"}`, false)
		member, err := ComponentUse(doc, "A")
		if err != nil {
			t.Fatalf("ComponentUse(A): %v", err)
		}
		if member != doc.Components.Schemas["A"].Value {
			t.Error("a use of the member A is not A's schema")
		}
		if _, err := ComponentUse(doc, "Nope"); err == nil {
			t.Error("ComponentUse(Nope): no error")
		}
	})
	t.Run("member_allOf_entry_left_alone", func(t *testing.T) {
		doc, _ := load(t)
		if got := doc.Components.Schemas["A"].Value.AllOf[0].Ref; got != "#/components/schemas/Shape" {
			t.Errorf("A.AllOf[0].Ref = %q, want the base left alone", got)
		}
	})
}
