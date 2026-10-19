package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// StrictDateTimeQuery refuses a date-time query parameter that
// common.ParseDateTime does not read, with the answer of any other malformed
// parameter. The generated binding parses a date-time with time.Parse, which
// takes a comma before the fractional seconds, and falls back to a bare date;
// a body's date-time member takes neither. The parameters checked are the
// ones the request's operation declares with format date-time; an operation
// ignores a parameter it does not declare, whatever its name.
func StrictDateTimeQuery(spec *openapi3.T) genapi.MiddlewareFunc {
	byOperation := dateTimeQueryParams(spec)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The chi mux registers each operation under its spec path, so
			// the route pattern is that path as dateTimeKey writes it.
			names := byOperation[r.Method+" "+chi.RouteContext(r.Context()).RoutePattern()]
			query := r.URL.Query()
			for _, name := range names {
				for _, v := range query[name] {
					if _, err := common.ParseDateTime(v); err != nil {
						BindingErrorHandler(w, r, &genapi.InvalidParamFormatError{ParamName: name, Err: err})
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// dateTimeQueryParams maps each operation, as "METHOD /path/template", to the
// date-time query parameters it declares, sorted so a request with two bad
// ones is answered the same way every time.
func dateTimeQueryParams(spec *openapi3.T) map[string][]string {
	byOperation := map[string][]string{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			var names []string
			for _, ref := range append(slices.Clone(item.Parameters), op.Parameters...) {
				p := ref.Value
				if p == nil || p.In != openapi3.ParameterInQuery || p.Schema == nil || p.Schema.Value == nil {
					continue
				}
				if p.Schema.Value.Format == "date-time" && !slices.Contains(names, p.Name) {
					names = append(names, p.Name)
				}
			}
			if len(names) > 0 {
				slices.Sort(names)
				byOperation[dateTimeKey(method, path)] = names
			}
		}
	}
	return byOperation
}

// dateTimeKey is an operation's key: its method and the chi route pattern of
// its spec path, which drops a trailing "/" (chi's RoutePattern does) so a
// trailing-slash operation is not missed and left unchecked.
func dateTimeKey(method, path string) string {
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	return method + " " + path
}
