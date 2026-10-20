package api

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// QueryRules holds, for each spec operation, the query parameters it
// declares and which of them are date-times. Every operation is in it, those
// that declare none included, so a request names a parameter its operation
// declares or is refused, as a body naming a member its shape does not
// declare is: a misspelt parameter is never silently dropped.
type QueryRules map[string]operationQuery

type operationQuery struct {
	declared  map[string]bool // true for an array parameter, which may repeat
	dateTimes []string        // sorted, so two bad ones are answered the same way every time
}

// NewQueryRules reads every operation's query parameters from the spec.
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
			rules[operationKey(method, path)] = q
		}
	}
	return rules
}

// Generated checks the query of a request on a generated route. The chi mux
// registers each operation under its spec path, so the route pattern is that
// path as operationKey writes it.
func (q QueryRules) Generated() genapi.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if q.check(w, r, r.Method+" "+chi.RouteContext(r.Context()).RoutePattern()) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// Route checks the query of a request on a hand-written route serving the
// spec operation named by method and path. It panics on an operation the spec
// does not have: that is a wiring mistake, found at startup.
func (q QueryRules) Route(method, path string) func(http.Handler) http.Handler {
	key := operationKey(method, path)
	if _, ok := q[key]; !ok {
		panic("api: no spec operation " + key)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if q.check(w, r, key) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// check refuses a query that does not parse, a query parameter the operation
// does not declare, a parameter other than an array given more than once, and
// a date-time parameter that common.ParseDateTime does not read, and reports
// whether the request may go on. url.Values drops a pair it cannot parse, and
// the binding reads the first of repeated values, so each of these would
// otherwise change the request without a word. The generated binding parses a date-time with
// time.Parse, which takes a comma before the fractional seconds, a one-digit
// hour and +24:00, and falls back to a bare date; a body's date-time member
// takes none of these.
func (q QueryRules) check(w http.ResponseWriter, r *http.Request, key string) bool {
	op := q[key]
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
		for _, v := range query[name] {
			if _, err := common.ParseDateTime(v); err != nil {
				BindingErrorHandler(w, r, &genapi.InvalidParamFormatError{ParamName: name, Err: err})
				return false
			}
		}
	}
	return true
}

// operationKey is an operation's key: its method and the chi route pattern of
// its spec path, which drops a trailing "/" (chi's RoutePattern does) so a
// trailing-slash operation is not missed.
func operationKey(method, path string) string {
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	return method + " " + path
}
