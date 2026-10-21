package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// newGeneratedHandler serves every generated route through ChiMux, as app.go
// does, on a Server with no domain handlers: a request that passes the query
// check reaches the binding and then answers 501.
func newGeneratedHandler(t *testing.T) http.Handler {
	t.Helper()
	spec, err := genapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	return genapi.HandlerWithOptions(NewServer(), genapi.StdHTTPServerOptions{
		BaseRouter:       NewChiMux(NewQueryRules(spec)),
		ErrorHandlerFunc: BindingErrorHandler,
	})
}

func withM2M(ctx context.Context) context.Context {
	return spi.WithUserContext(ctx, &spi.UserContext{UserID: "caller", Tenant: spi.Tenant{ID: "acme", Name: "acme"}, Roles: []string{"ROLE_M2M"}})
}

// TestQueryRules_CheckRunsBeforeBinding: every generated route has its
// operation's rules (ChiMux panics at registration otherwise), and the check
// runs before the generated binding, so a refusal reads the same on every
// route whatever the parameter's type.
func TestQueryRules_CheckRunsBeforeBinding(t *testing.T) {
	h := newGeneratedHandler(t)
	for _, c := range []struct{ method, target, want string }{
		{http.MethodPost, "/search/direct/X/1?limit=1&limit=2", `query parameter \"limit\" is given more than once`},
		{http.MethodPost, "/search/direct/X/1?pointInTime=2024-01-02T03:04:05Z&pointInTime=2024-01-02T03:04:05Z", `query parameter \"pointInTime\" is given more than once`},
		{http.MethodPost, "/search/direct/X/1?Limit=1", `unknown query parameter \"Limit\"`},
		{http.MethodPost, "/search/direct/X/1?limit=1;x=2", "malformed query string"},
		{http.MethodGet, "/model/?x=1", `unknown query parameter \"x\"`},
	} {
		req := httptest.NewRequest(c.method, c.target, nil)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(withM2M(req.Context())))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s %s: %d %s, want 400 %s", c.method, c.target, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// TestQueryRules_SpecShapes: the check reads a query parameter by its name,
// so every query parameter is a scalar or a form-exploded array, whose
// repeated key is that name; and every operation cyoda-go serves declares the
// 400 the check answers.
func TestQueryRules_SpecShapes(t *testing.T) {
	spec, err := genapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../api/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		OutputOptions struct {
			ExcludeTags []string `yaml:"exclude-tags"`
		} `yaml:"output-options"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	excluded := map[string]bool{}
	for _, tag := range cfg.OutputOptions.ExcludeTags {
		excluded[tag] = true
	}
	if len(excluded) == 0 {
		t.Fatal("api/config.yaml excludes no tag; this test reads output-options.exclude-tags")
	}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if slices.ContainsFunc(op.Tags, func(tag string) bool { return excluded[tag] }) {
				continue // excluded from the generated server; not served
			}
			if op.Responses.Value("400") == nil {
				t.Errorf("%s %s declares no 400", method, path)
			}
			for _, ref := range append(slices.Clone(item.Parameters), op.Parameters...) {
				p := ref.Value
				if p.In != openapi3.ParameterInQuery {
					continue
				}
				if p.Schema == nil {
					t.Errorf("%s %s parameter %q has no schema", method, path, p.Name)
					continue
				}
				s := p.Schema.Value
				if s.Type.Is("object") || (s.Type.Is("array") && ((p.Style != "" && p.Style != "form") || (p.Explode != nil && !*p.Explode))) {
					t.Errorf("%s %s parameter %q: a shape whose query keys are not its name", method, path, p.Name)
				}
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
