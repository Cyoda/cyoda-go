package api

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/getkin/kin-openapi/openapi3"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// QueryRules holds, for each spec operation, the query parameters it
// declares and which of them are date-times. A request names only parameters
// its operation declares, or is refused, as a body naming a member its shape
// does not declare is: a misspelt parameter is never silently dropped.
type QueryRules map[string]operationQuery

type operationQuery struct {
	declared  map[string]bool // true for an array parameter, which may repeat
	dateTimes []string        // sorted, so two bad ones are answered the same way every time
}

// NewQueryRules reads every operation's query parameters from the spec,
// keyed "METHOD /path" as the generated code registers its routes.
func NewQueryRules(spec *openapi3.T) QueryRules {
	rules := QueryRules{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			q := operationQuery{declared: map[string]bool{}}
			for _, ref := range append(slices.Clone(item.Parameters), op.Parameters...) {
				// The loader resolves every $ref or fails, so Value is set; a
				// parameter declared with content has no Schema.
				p := ref.Value
				if p.In != openapi3.ParameterInQuery {
					continue
				}
				q.declared[p.Name] = p.Schema != nil && p.Schema.Value.Type.Is("array")
				if p.Schema != nil && p.Schema.Value.Format == "date-time" && !slices.Contains(q.dateTimes, p.Name) {
					q.dateTimes = append(q.dateTimes, p.Name)
				}
			}
			slices.Sort(q.dateTimes)
			rules[method+" "+path] = q
		}
	}
	return rules
}

// Route checks the query of every request to the spec operation named by
// method and path, before the handler or the generated binding reads it. It
// panics on an operation the spec does not have: that is a wiring mistake,
// found at startup.
func (q QueryRules) Route(method, path string) func(http.Handler) http.Handler {
	op, ok := q[method+" "+path]
	if !ok {
		panic("api: no spec operation " + method + " " + path)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if op.check(w, r) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// check refuses a query that does not parse, a query parameter the operation
// does not declare, a parameter other than an array given more than once, and
// a date-time parameter that common.ParseDateTime does not read, and reports
// whether the request may go on. url.Values drops a pair it cannot parse, and
// the generated binding reads a date-time with time.Parse, which takes a comma
// before the fractional seconds, a one-digit hour and +24:00, and falls back
// to a bare date; each would otherwise change the request without a word.
func (op operationQuery) check(w http.ResponseWriter, r *http.Request) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "malformed query string"))
		return false
	}
	for _, name := range slices.Sorted(maps.Keys(query)) {
		isArray, declared := op.declared[name]
		msg := ""
		switch {
		case !declared:
			msg = fmt.Sprintf("unknown query parameter %q", name)
		case !isArray && len(query[name]) > 1:
			msg = fmt.Sprintf("query parameter %q is given more than once", name)
		default:
			continue
		}
		appErr := common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, msg)
		appErr.Props = map[string]any{"parameter": name}
		common.WriteError(w, r, appErr)
		return false
	}
	for _, name := range op.dateTimes {
		if v, ok := query[name]; ok {
			if _, err := common.ParseDateTime(v[0]); err != nil {
				BindingErrorHandler(w, r, &genapi.InvalidParamFormatError{ParamName: name, Err: err})
				return false
			}
		}
	}
	return true
}
