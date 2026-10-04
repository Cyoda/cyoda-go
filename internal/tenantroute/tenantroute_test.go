package tenantroute_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/tenantroute"
)

// mount builds an outer mux that strips /api, as app.go does, and returns
// what the route saw.
func mount(t *testing.T) (http.Handler, *string) {
	t.Helper()
	var seen string
	inner := http.NewServeMux()
	tenantroute.Handle(inner, "/tenants/{tenant}/oauth/token",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tn, ok := tenantroute.Addressed(r.Context())
			if !ok {
				t.Error("no addressed tenant")
			}
			seen = string(tn)
			w.WriteHeader(http.StatusOK)
		}),
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	outer := http.NewServeMux()
	outer.Handle("/api/", http.StripPrefix("/api", inner))
	return outer, &seen
}

func serve(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://h"+path, nil))
	return rec.Code
}

func TestHandle_AcceptsEveryGrammarCharacter(t *testing.T) {
	h, seen := mount(t)
	for _, tn := range []string{"acme", "acme.eu-1_x", "Acme", "9f8c7b6a", "PLATFORM", "clients", "oauth", "tenants", "model"} {
		if code := serve(h, "/api/tenants/"+tn+"/oauth/token"); code != http.StatusOK || *seen != tn {
			t.Errorf("%q: %d, saw %q", tn, code, *seen)
		}
	}
}

func TestHandle_TenantLengthBound(t *testing.T) {
	h, _ := mount(t)
	if code := serve(h, "/api/tenants/"+strings.Repeat("a", 100)+"/oauth/token"); code != http.StatusOK {
		t.Errorf("100 chars: %d", code)
	}
	if code := serve(h, "/api/tenants/"+strings.Repeat("a", 101)+"/oauth/token"); code != http.StatusBadRequest {
		t.Errorf("101 chars: %d", code)
	}
}

func TestHandle_RefusesBadTenantsAndEncodedPaths(t *testing.T) {
	h, _ := mount(t)
	for _, p := range []string{
		"/api/tenants/SYSTEM/oauth/token",
		"/api/tenants/system/oauth/token",
		"/api/tenants/-x/oauth/token",
		"/api/tenants/%61cme/oauth/token", // encoded tenant
		"/api/%74enants/acme/oauth/token", // encoded literal segment
		"/api/tenants/acme/oauth/%74oken", // encoded literal segment
		"/api/tenants/a%2Fb/oauth/token",  // encoded slash
		"/api/tenants/a%20b/oauth/token",  // encoded reserved char: RawPath empty, grammar refuses
	} {
		if code := serve(h, p); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, code)
		}
	}
}

func TestAddressed_AbsentOutsideTheGroup(t *testing.T) {
	if _, ok := tenantroute.Addressed(httptest.NewRequest(http.MethodGet, "/", nil).Context()); ok {
		t.Fatal("addressed tenant outside the group")
	}
}

func TestHandle_PanicsOnAPatternOutsideTheGroup(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	tenantroute.Handle(http.NewServeMux(), "/oauth/token", http.NotFoundHandler(), func(http.ResponseWriter, *http.Request) {})
}

func TestHandle_PanicsOnANilRefuse(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	tenantroute.Handle(http.NewServeMux(), "/tenants/{tenant}/oauth/token", http.NotFoundHandler(), nil)
}
