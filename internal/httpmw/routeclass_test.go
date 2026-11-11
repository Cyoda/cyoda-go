package httpmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// groupedStatsRoute is the one data route app.go registers by hand, on the
// standard ServeMux, with the same middleware chain as the generated routes.
const groupedStatsRoute = "POST /entity/stats/{entityName}/{modelVersion}/query"

// recordingMux records every route the generated code registers.
type recordingMux struct {
	http.Handler
	routes []string
}

func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.routes = append(m.routes, pattern)
}

// everyDataRoute is every route that reaches the join layer.
func everyDataRoute(t *testing.T) []string {
	t.Helper()
	rec := &recordingMux{}
	genapi.HandlerWithOptions(nil, genapi.StdHTTPServerOptions{BaseRouter: rec})
	if len(rec.routes) == 0 {
		t.Fatal("the generated code registered no route")
	}
	return append(rec.routes, groupedStatsRoute)
}

var wildcard = regexp.MustCompile(`\{[^}]+\}`)

// Every route that reaches the join layer has a class, read from the route
// the router actually picked; the spec's write routes are writes, and the
// routes that write outside the transaction are outside it.
func TestJoinedRequestClass_EveryRouteClassified(t *testing.T) {
	spec, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	type seen struct {
		pattern string
		class   txcoord.RequestClass
	}
	var got seen
	probe := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.Pattern
			if _, path, ok := strings.Cut(p, " "); ok {
				p = path
			}
			got = seen{pattern: routeKey(r.Method, p), class: joinedRequestClass(r)}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	chiRoutes := genapi.HandlerWithOptions(nil, genapi.StdHTTPServerOptions{
		BaseRouter: internalapi.NewChiMux(internalapi.NewQueryRules(spec), probe),
	})
	std := http.NewServeMux()
	std.Handle(groupedStatsRoute, probe(nil))
	m2m := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: "t"}, Roles: []string{"ROLE_M2M"},
	})

	routes := everyDataRoute(t)
	for _, route := range routes {
		method, pattern, _ := strings.Cut(route, " ")
		got = seen{}
		r := httptest.NewRequest(method, wildcard.ReplaceAllString(pattern, "x1"), nil).WithContext(m2m)
		w := httptest.NewRecorder()
		if route == groupedStatsRoute {
			std.ServeHTTP(w, r)
		} else {
			chiRoutes.ServeHTTP(w, r)
		}
		wantPattern := routeKey(method, pattern)
		if got.pattern != wantPattern {
			t.Errorf("%s: routed to %q; want the route itself", route, got.pattern)
			continue
		}
		if got.class == 0 {
			t.Errorf("%s has no class: add it to routeClasses", route)
		}
	}

	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		method, pattern, _ := strings.Cut(route, " ")
		registered[routeKey(method, pattern)] = true
	}
	var stale []string
	for route := range routeClasses {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("routeClasses names routes no router registers: %v", stale)
	}

	for _, route := range []string{
		"POST /entity/{format}/{entityName}/{modelVersion}",
		"POST /entity/{format}",
		"PUT /entity/{format}",
		"PUT /entity/{format}/{entityId}",
		"PATCH /entity/{format}/{entityId}",
		"PUT /entity/{format}/{entityId}/{transition}",
		"PATCH /entity/{format}/{entityId}/{transition}",
		"DELETE /entity/{entityId}",
		"DELETE /entity/{entityName}/{modelVersion}",
	} {
		if c := classOf(route); c != txcoord.Write {
			t.Errorf("%s: class %d; want Write", route, c)
		}
	}
	for _, route := range []string{
		"POST /message/new/{subject}",
		"DELETE /message/{messageId}",
		"DELETE /message",
		"POST /search/async/{entityName}/{modelVersion}",
		"PUT /search/async/{jobId}/cancel",
		"POST /clients",
		"POST /oauth/keys/keypair",
		"POST /oauth/keys/trusted",
	} {
		if c := classOf(route); c != txcoord.Outside {
			t.Errorf("%s: class %d; want Outside", route, c)
		}
	}
	for route, class := range routeClasses {
		if class == txcoord.Write && strings.HasPrefix(route, "GET ") {
			t.Errorf("%s: a GET is never a write", route)
		}
	}
}

// A request whose route the router did not record has no class.
func TestJoinedRequestClass_UnknownRouteIsZero(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/nowhere", nil)
	r.Pattern = "/nowhere"
	if c := joinedRequestClass(r); c != 0 {
		t.Fatalf("class = %d; want 0", c)
	}
}

// classOf is the class routeClasses gives route ("METHOD /pattern").
func classOf(route string) txcoord.RequestClass {
	method, pattern, _ := strings.Cut(route, " ")
	return routeClasses[routeKey(method, pattern)]
}
