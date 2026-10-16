package openapivalidator

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMiddleware_ForwardsResponseAndCaptures(t *testing.T) {
	v := newFixtureValidator(t)
	defaultCollector = newCollector() // reset for test

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"transactionId":"abc","entityIds":["e1"]}`)
	})
	wrapped := NewMiddleware(v)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/single", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("forwarded status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"transactionId"`) {
		t.Errorf("body not forwarded: %q", rec.Body.String())
	}
	exercised := defaultCollector.exerciseSet()
	if !exercised["getSingle"] {
		t.Errorf("getSingle not recorded as exercised: %v", exercised)
	}
	if got := len(defaultCollector.drain()); got != 0 {
		t.Errorf("expected 0 mismatches, got %d", got)
	}
}

func TestMiddleware_RecordsMismatchOnDriftedResponse(t *testing.T) {
	v := newFixtureValidator(t)
	defaultCollector = newCollector()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[{"transactionId":"abc","entityIds":["e1"]}]`) // array, but spec says single object
	})
	wrapped := NewMiddleware(v)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/single", nil)
	wrapped.ServeHTTP(rec, req)

	mismatches := defaultCollector.drain()
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for array-vs-object response, got none")
	}
	if mismatches[0].Operation != "getSingle" {
		t.Errorf("operation = %q, want getSingle", mismatches[0].Operation)
	}
}

// TestMiddleware_SkipsOperationalPaths confirms that requests to genuinely
// non-spec operational/admin paths (health checks, docs UI, admin endpoints,
// OAuth discovery) are not recorded as mismatches.
//
// Note: /api/oauth/keys/ is NOT in this list — those paths ARE declared in the
// spec and must pass through the validator. The conformance gate in
// zzz_openapi_conformance_test.go requires each such op to be either exercised
// or marked not-live via `x-cyoda-status` in api/openapi.yaml.
func TestMiddleware_SkipsOperationalPaths(t *testing.T) {
	v := newFixtureValidator(t)
	defaultCollector = newCollector()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"UP"}`)
	})
	wrapped := NewMiddleware(v)(inner)

	for _, path := range []string{
		"/api/health",
		"/api/admin/log-level",
		"/api/.well-known/openid-configuration",
		"/api/docs",
		"/api/openapi.json",
		"/api/help",
		"/api/help/workflows",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", path, nil)
			wrapped.ServeHTTP(rec, req)
		})
	}

	if got := len(defaultCollector.drain()); got != 0 {
		t.Errorf("operational paths produced %d mismatches; expected 0", got)
	}
}

// recordingReporter stands in for the requesting test's *testing.T, so that
// the test can observe a reported failure without failing itself.
type recordingReporter struct {
	name string
	mu   sync.Mutex
	errs []string
}

func (r *recordingReporter) Name() string { return r.name }

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// A request crosses a real TCP connection, as every e2e request does: the
// test's identity reaches the middleware only as the header the client's
// Transport adds, never as a context value. In a -run-filtered enforce-mode
// run the mismatch fails the requesting test, and the report names it.
func TestMiddleware_FilteredRunFailsTheRequestingTestOverTheWire(t *testing.T) {
	v := newFixtureValidator(t)
	defaultCollector = newCollector()
	if err := flag.Set("test.run", "Forced"); err != nil {
		t.Fatalf("set test.run: %v", err)
	}
	defer flag.Set("test.run", "")

	srv := httptest.NewServer(NewMiddleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(TestNameHeader); got != "" {
			t.Errorf("the application saw the test-name header %q; the middleware must remove it", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[{"oops":"array"}]`) // the spec says one object
	})))
	defer srv.Close()

	rep := &recordingReporter{name: "TestRequester/sub"}
	req, err := http.NewRequestWithContext(WithTestT(context.Background(), rep), http.MethodGet, srv.URL+"/single", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	client := &http.Client{Transport: Transport(http.DefaultTransport)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if errs := rep.errors(); len(errs) == 0 {
		t.Error("the requesting test was not failed for the mismatch")
	}
	ms := defaultCollector.drain()
	if len(ms) == 0 {
		t.Fatal("no mismatch was collected")
	}
	if ms[0].TestName != "TestRequester/sub" {
		t.Errorf("mismatch TestName = %q, want TestRequester/sub", ms[0].TestName)
	}
}

// A request that names a test no Transport registered — in this process, no
// test of that name sent a request — is reported under the name it carries,
// not as "unknown".
func TestMiddleware_UnregisteredTestNameIsKept(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/single", nil)
	req.Header.Set(TestNameHeader, "TestNeverRegistered/sub")
	ms := serveFixture(t, req)
	if len(ms) == 0 {
		t.Fatal("no mismatch was collected")
	}
	if ms[0].TestName != "TestNeverRegistered/sub" {
		t.Errorf("mismatch TestName = %q, want TestNeverRegistered/sub", ms[0].TestName)
	}
}

// A mismatch nobody reported — a filtered run that did not include the
// conformance report test — turns a passing exit code into a failing one.
func TestFailOnUnreported(t *testing.T) {
	defaultCollector = newCollector()
	if got := FailOnUnreported(0); got != 0 {
		t.Errorf("FailOnUnreported(0) with no mismatches = %d, want 0", got)
	}
	defaultCollector.append(Mismatch{Operation: "getSingle", Method: "GET", Path: "/single", Status: 200, Reason: "drift", TestName: "unknown"})
	if got := FailOnUnreported(0); got != 1 {
		t.Errorf("FailOnUnreported(0) with a mismatch = %d, want 1", got)
	}
	if left := defaultCollector.drain(); len(left) != 0 {
		t.Errorf("FailOnUnreported left %d mismatches in the collector", len(left))
	}
	defaultCollector.append(Mismatch{Operation: "getSingle", Reason: "drift"})
	if got := FailOnUnreported(3); got != 3 {
		t.Errorf("FailOnUnreported(3) = %d, want the run's own code 3", got)
	}
}

// A path that merely starts with an operational prefix's letters is not
// operational: /api/helpers is not under /api/help.
func TestIsOperationalPath_HelpRootNotItsLookalikes(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/help":           true,
		"/api/help/":          true,
		"/api/help/workflows": true,
		"/api/helpers":        false,
	} {
		if got := isOperationalPath(p); got != want {
			t.Errorf("isOperationalPath(%q) = %t, want %t", p, got, want)
		}
	}
}

// serveFixture sends req through the middleware over the fixture spec, with
// a handler answering 204, and returns the mismatches it collected.
func serveFixture(t *testing.T, req *http.Request, opts ...MiddlewareOption) []Mismatch {
	t.Helper()
	v := newFixtureValidator(t)
	defaultCollector = newCollector()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	NewMiddleware(v, opts...)(inner).ServeHTTP(httptest.NewRecorder(), req)
	return defaultCollector.drain()
}

// A CORS preflight is answered by the CORS layer, not by an API operation:
// it is not validated. An OPTIONS that is not a preflight still is.
func TestMiddleware_SkipsCORSPreflight(t *testing.T) {
	preflight := httptest.NewRequest(http.MethodOptions, "/single", nil)
	preflight.Header.Set("Origin", "http://localhost:3000")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodGet)
	if ms := serveFixture(t, preflight); len(ms) != 0 {
		t.Errorf("a CORS preflight produced %d mismatches, want 0: %+v", len(ms), ms)
	}

	for name, hdr := range map[string]string{"plain": "", "Origin only": "Origin", "request method only": "Access-Control-Request-Method"} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/single", nil)
			if hdr != "" {
				req.Header.Set(hdr, "x")
			}
			if ms := serveFixture(t, req); len(ms) == 0 {
				t.Error("an OPTIONS that is not a preflight was not reported as an undeclared route")
			}
		})
	}
}

// A test that probes an undeclared route on purpose marks the request; the
// middleware removes the mark and does not report the route as undeclared.
// The mark does not hide a declared route: its response is still validated.
func TestMiddleware_UndeclaredRouteProbe(t *testing.T) {
	probe := httptest.NewRequest(http.MethodDelete, "/single", nil)
	MarkUndeclaredRouteProbe(probe)
	if ms := serveFixture(t, probe); len(ms) != 0 {
		t.Errorf("a marked probe of an undeclared route produced %d mismatches, want 0: %+v", len(ms), ms)
	}

	unmarked := httptest.NewRequest(http.MethodDelete, "/single", nil)
	if ms := serveFixture(t, unmarked); len(ms) == 0 {
		t.Error("an unmarked request to an undeclared route was not reported")
	}

	declared := httptest.NewRequest(http.MethodGet, "/single", nil)
	MarkUndeclaredRouteProbe(declared)
	if ms := serveFixture(t, declared); len(ms) == 0 {
		t.Error("a marked request to a declared route was not validated (204 is not a declared status)")
	}

	var seen string
	v := newFixtureValidator(t)
	defaultCollector = newCollector()
	req := httptest.NewRequest(http.MethodDelete, "/single", nil)
	MarkUndeclaredRouteProbe(req)
	NewMiddleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(UndeclaredRouteProbeHeader)
		w.WriteHeader(http.StatusMethodNotAllowed)
	})).ServeHTTP(httptest.NewRecorder(), req)
	if seen != "" {
		t.Errorf("the application saw the probe header %q; the middleware must remove it", seen)
	}
}

// A server outside the error-code matrix still validates and records
// coverage, but records no (operation, status, errorCode) triple.
func TestMiddleware_OutsideErrorCodeMatrix(t *testing.T) {
	problem := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"properties":{"errorCode":"NOPE"}}`)
	}
	for name, tc := range map[string]struct {
		opts       []MiddlewareOption
		wantTriple bool
	}{
		"default":                {nil, true},
		"OutsideErrorCodeMatrix": {[]MiddlewareOption{OutsideErrorCodeMatrix()}, false},
	} {
		t.Run(name, func(t *testing.T) {
			v := newFixtureValidator(t)
			defaultCollector = newCollector()
			NewMiddleware(v, tc.opts...)(http.HandlerFunc(problem)).
				ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/single", nil))
			got := defaultCollector.observedErrorTriples()
			if tc.wantTriple != (len(got) == 1) {
				t.Errorf("triples = %+v, want a triple: %t", got, tc.wantTriple)
			}
			if !defaultCollector.exerciseSet()["getSingle"] {
				t.Error("getSingle was not recorded as exercised")
			}
			if len(defaultCollector.drain()) == 0 {
				t.Error("the undeclared 404 was not reported: the response was not validated")
			}
		})
	}
}
