package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestDateTimeQueryParams_KeysAreRoutePatterns: the middleware finds an
// operation's date-time parameters by the chi route pattern of the request,
// and a key that matches no pattern checks nothing. Every key must be the
// pattern a request to its operation resolves to, so register each spec
// operation on chi, as the generated routes are, and compare.
func TestDateTimeQueryParams_KeysAreRoutePatterns(t *testing.T) {
	spec, err := genapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	keys := dateTimeQueryParams(spec)
	if len(keys) == 0 {
		t.Fatal("the spec declares no date-time query parameter")
	}
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
			if want := dateTimeKey(method, path); resolved != want {
				t.Errorf("%s %s resolves to %q, key %q", method, path, resolved, want)
			}
		}
	}
}
