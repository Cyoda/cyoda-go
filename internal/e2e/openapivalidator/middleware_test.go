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
