package api

import (
	"net/http"
	"slices"

	"github.com/getkin/kin-openapi/openapi3"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// StrictDateTimeQuery refuses a date-time query parameter that
// common.ParseDateTime does not read, with the answer of any other malformed
// parameter. The generated binding parses a date-time with time.Parse, which
// takes a comma before the fractional seconds and falls back to a bare date;
// a body's date-time member takes neither. The parameters are the spec's own:
// every query parameter whose schema format is date-time.
func StrictDateTimeQuery(spec *openapi3.T) genapi.MiddlewareFunc {
	names := dateTimeQueryParams(spec)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// dateTimeQueryParams names the spec's date-time query parameters, sorted so
// a request with two bad ones is answered the same way every time.
func dateTimeQueryParams(spec *openapi3.T) []string {
	var names []string
	add := func(params openapi3.Parameters) {
		for _, ref := range params {
			p := ref.Value
			if p == nil || p.In != openapi3.ParameterInQuery || p.Schema == nil || p.Schema.Value == nil {
				continue
			}
			if p.Schema.Value.Format == "date-time" && !slices.Contains(names, p.Name) {
				names = append(names, p.Name)
			}
		}
	}
	for _, item := range spec.Paths.Map() {
		add(item.Parameters)
		for _, op := range item.Operations() {
			add(op.Parameters)
		}
	}
	slices.Sort(names)
	return names
}
