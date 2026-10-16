package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// exampleCheck is one example found in the spec and the schema it must satisfy.
type exampleCheck struct {
	where  string
	value  any
	schema *openapi3.Schema
	text   bool // a non-JSON media type: the example is text, not a JSON value
}

// collectExamples gathers every example of every schema (nested ones too),
// media type and parameter in the document.
func collectExamples(doc *openapi3.T) []exampleCheck {
	var out []exampleCheck
	seen := map[*openapi3.Schema]bool{}

	var walkSchema func(where string, s *openapi3.Schema)
	walkSchema = func(where string, s *openapi3.Schema) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		if s.Example != nil {
			out = append(out, exampleCheck{where: where + " example", value: s.Example, schema: s})
		}
		names := make([]string, 0, len(s.Properties))
		for n := range s.Properties {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			walkSchema(where+"."+n, s.Properties[n].Value)
		}
		if s.Items != nil {
			walkSchema(where+"[]", s.Items.Value)
		}
		if s.AdditionalProperties.Schema != nil {
			walkSchema(where+"{}", s.AdditionalProperties.Schema.Value)
		}
		for _, group := range []openapi3.SchemaRefs{s.AllOf, s.OneOf, s.AnyOf} {
			for _, m := range group {
				walkSchema(where+"/member", m.Value)
			}
		}
	}

	mediaType := func(where, ct string, mt *openapi3.MediaType) {
		if mt == nil || mt.Schema == nil || mt.Schema.Value == nil {
			return
		}
		text := !strings.Contains(ct, "json")
		if mt.Example != nil {
			out = append(out, exampleCheck{where: where + " example", value: mt.Example, schema: mt.Schema.Value, text: text})
		}
		for name, ex := range mt.Examples {
			if ex.Value != nil && ex.Value.Value != nil {
				out = append(out, exampleCheck{where: where + " examples[" + name + "]", value: ex.Value.Value, schema: mt.Schema.Value, text: text})
			}
		}
	}

	param := func(where string, p *openapi3.Parameter) {
		if p == nil || p.Schema == nil || p.Schema.Value == nil {
			return
		}
		if p.Example != nil {
			out = append(out, exampleCheck{where: where + " example", value: p.Example, schema: p.Schema.Value})
		}
		for name, ex := range p.Examples {
			if ex.Value != nil && ex.Value.Value != nil {
				out = append(out, exampleCheck{where: where + " examples[" + name + "]", value: ex.Value.Value, schema: p.Schema.Value})
			}
		}
	}

	for name, ref := range doc.Components.Schemas {
		walkSchema("schema "+name, ref.Value)
	}
	for path, item := range doc.Paths.Map() {
		for _, p := range item.Parameters {
			param(path+" param "+p.Value.Name, p.Value)
		}
		for method, op := range item.Operations() {
			where := method + " " + path
			for _, p := range op.Parameters {
				param(where+" param "+p.Value.Name, p.Value)
			}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				for ct, mt := range op.RequestBody.Value.Content {
					mediaType(where+" request "+ct, ct, mt)
					walkSchema(where+" request "+ct, schemaOf(mt))
				}
			}
			for code, r := range op.Responses.Map() {
				if r.Value == nil {
					continue
				}
				for ct, mt := range r.Value.Content {
					mediaType(where+" "+code+" "+ct, ct, mt)
					walkSchema(where+" "+code+" "+ct, schemaOf(mt))
				}
			}
		}
	}
	return out
}

func schemaOf(mt *openapi3.MediaType) *openapi3.Schema {
	if mt == nil || mt.Schema == nil {
		return nil
	}
	return mt.Schema.Value
}

func TestEveryExampleValidates(t *testing.T) {
	doc := viewedSpec(t)
	checks := collectExamples(doc)
	if len(checks) < 100 {
		t.Fatalf("found only %d examples; the walk is not reaching the spec", len(checks))
	}
	for _, c := range checks {
		value := c.value
		schema := c.schema
		if c.text {
			// A non-JSON example is text, not a JSON value: it is checked
			// as the string it is.
			if _, ok := value.(string); !ok {
				t.Errorf("%s: a non-JSON example must be text, got %T", c.where, value)
				continue
			}
			schema = openapi3.NewStringSchema()
		}
		// Normalise through JSON so a YAML-decoded value is what a client sends.
		raw, err := json.Marshal(value)
		if err != nil {
			t.Errorf("%s: example is not JSON: %v", c.where, err)
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: %v", c.where, err)
			continue
		}
		for mode, opts := range schemaValidators {
			if err := schema.VisitJSON(v, opts...); err != nil {
				t.Errorf("%s: %s: example does not match its schema: %v", mode, c.where, err)
			}
		}
	}
}

func TestBearerAuthHasNoName(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if name := doc.Components.SecuritySchemes["bearerAuth"].Value.Name; name != "" {
		t.Errorf("bearerAuth is an http scheme and has no name; got %q", name)
	}
}
