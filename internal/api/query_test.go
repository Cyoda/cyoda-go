package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestQueryRules_KeysAreRoutePatterns: the middleware finds an operation's
// query rules by the chi route pattern of the request, and a key that matches
// no pattern would refuse every query parameter of that operation. Every key
// must be the pattern a request to its operation resolves to, so register each
// spec operation on chi, as the generated routes are, and compare.
func TestQueryRules_KeysAreRoutePatterns(t *testing.T) {
	spec, err := genapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	rules := NewQueryRules(spec)
	var resolved string
	r := chi.NewRouter()
	for path, item := range spec.Paths.Map() {
		for method := range item.Operations() {
			r.Method(method, path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resolved = r.Method + " " + chi.RouteContext(r.Context()).RoutePattern()
			}))
		}
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	for path, item := range spec.Paths.Map() {
		for method := range item.Operations() {
			resolved = ""
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, param.ReplaceAllString(path, "x"), nil))
			want := operationKey(method, path)
			if resolved != want {
				t.Errorf("%s %s resolves to %q, key %q", method, path, resolved, want)
			}
			if _, ok := rules[want]; !ok {
				t.Errorf("%s %s has no rules", method, path)
			}
		}
	}
}

// TestQueryRules_NoUncheckedDateTime: the query check sees a
// date-time only as a query parameter whose own schema has that format. A
// date-time anywhere else in a parameter — a header or path parameter, an
// array's items, a composed schema, a content-typed parameter — would be bound
// by the lenient generated parse and go unchecked, so the spec must have none.
func TestQueryRules_NoUncheckedDateTime(t *testing.T) {
	spec, err := genapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			for _, ref := range append(slices.Clone(item.Parameters), op.Parameters...) {
				p := ref.Value
				checked := p.In == openapi3.ParameterInQuery && p.Schema != nil && p.Schema.Value.Format == "date-time"
				var schemas []*openapi3.SchemaRef
				if p.Schema != nil {
					if checked {
						schemas = nestedSchemas(p.Schema.Value)
					} else {
						schemas = []*openapi3.SchemaRef{p.Schema}
					}
				}
				for _, mt := range p.Content {
					schemas = append(schemas, mt.Schema)
				}
				for _, s := range schemas {
					if hasDateTime(s, map[*openapi3.Schema]bool{}) {
						t.Errorf("%s %s parameter %q (in %s) holds a date-time the middleware does not check", method, path, p.Name, p.In)
					}
				}
			}
		}
	}
}

func nestedSchemas(s *openapi3.Schema) []*openapi3.SchemaRef {
	var out []*openapi3.SchemaRef
	if s.Items != nil {
		out = append(out, s.Items)
	}
	out = append(out, s.AnyOf...)
	out = append(out, s.OneOf...)
	out = append(out, s.AllOf...)
	if s.Not != nil {
		out = append(out, s.Not)
	}
	for _, p := range s.Properties {
		out = append(out, p)
	}
	return out
}

func hasDateTime(ref *openapi3.SchemaRef, seen map[*openapi3.Schema]bool) bool {
	if ref == nil || ref.Value == nil || seen[ref.Value] {
		return false
	}
	seen[ref.Value] = true
	if ref.Value.Format == "date-time" {
		return true
	}
	for _, n := range nestedSchemas(ref.Value) {
		if hasDateTime(n, seen) {
			return true
		}
	}
	return false
}
