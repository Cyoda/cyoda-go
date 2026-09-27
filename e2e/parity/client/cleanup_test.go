package client_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// cleanupHelpers are the *OnCleanup helpers, each with the request it must
// send. A resource a scenario creates on a shared server must be removed
// even when the scenario fails, and t.Context() is cancelled before cleanups
// run — so each request has to reach the server on its own context.
func cleanupHelpers() []struct {
	name, method, path string
	register           func(c *client.Client, t testing.TB)
} {
	entity := uuid.MustParse("8f7c1d2e-0000-4000-8000-000000000001")
	return []struct {
		name, method, path string
		register           func(c *client.Client, t testing.TB)
	}{
		{"DeleteKeyPairOnCleanup", http.MethodDelete, "/api/oauth/keys/keypair/k1",
			func(c *client.Client, t testing.TB) { c.DeleteKeyPairOnCleanup(t, "k1") }},
		{"DeleteEntityOnCleanup", http.MethodDelete, "/api/entity/" + entity.String(),
			func(c *client.Client, t testing.TB) { c.DeleteEntityOnCleanup(t, entity) }},
	}
}

// recordingServer answers every request with status and records each
// "METHOD path" plus its Authorization header.
type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	auth     []string
}

func newRecordingServer(t *testing.T, status int) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		func() {
			rs.mu.Lock()
			defer rs.mu.Unlock()
			rs.requests = append(rs.requests, r.Method+" "+r.URL.Path)
			rs.auth = append(rs.auth, r.Header.Get("Authorization"))
		}()
		w.WriteHeader(status)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) snapshot() ([]string, []string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.requests...), append([]string(nil), rs.auth...)
}

func TestOnCleanup_SendsAfterTheTestBodyEnds(t *testing.T) {
	for _, h := range cleanupHelpers() {
		t.Run(h.name, func(t *testing.T) {
			srv := newRecordingServer(t, http.StatusOK)
			c := client.NewClient(srv.URL, "tok")

			t.Run("scenario", func(t *testing.T) {
				h.register(c, t)
				if reqs, _ := srv.snapshot(); len(reqs) != 0 {
					t.Fatalf("sent before the scenario ended: %v", reqs)
				}
			})
			reqs, auth := srv.snapshot()
			if want := h.method + " " + h.path; len(reqs) != 1 || reqs[0] != want {
				t.Fatalf("requests after cleanup: %v, want [%s]", reqs, want)
			}
			if auth[0] != "Bearer tok" {
				t.Errorf("cleanup request is not authenticated with the client's token")
			}
		})
	}
}

// fakeTB collects cleanups and errors so a test can run them and inspect
// what a helper reported without failing itself.
type fakeTB struct {
	testing.TB
	cleanups []func()
	errs     []string
}

func (f *fakeTB) Helper()                        {}
func (f *fakeTB) Cleanup(fn func())              { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(format string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(format, a...)) }
func (f *fakeTB) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func TestOnCleanup_ReportsUnexpectedStatus(t *testing.T) {
	cases := []struct {
		status  int
		reports bool
	}{
		{http.StatusOK, false},
		{http.StatusNoContent, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, true},
		{http.StatusConflict, true},
		{http.StatusInternalServerError, true},
	}
	for _, h := range cleanupHelpers() {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/%d", h.name, tc.status), func(t *testing.T) {
				srv := newRecordingServer(t, tc.status)
				f := &fakeTB{}
				h.register(client.NewClient(srv.URL, "secret-token"), f)
				f.runCleanups()
				if got := len(f.errs) > 0; got != tc.reports {
					t.Fatalf("status %d: reported=%t (%v), want %t", tc.status, got, f.errs, tc.reports)
				}
				for _, e := range f.errs {
					if !strings.Contains(e, fmt.Sprint(tc.status)) || !strings.Contains(e, h.path) {
						t.Errorf("report does not name the status and path: %q", e)
					}
					if strings.Contains(e, "secret-token") {
						t.Errorf("report leaks the bearer token")
					}
				}
			})
		}
	}
}

func TestOnCleanup_ReportsTransportFailure(t *testing.T) {
	for _, h := range cleanupHelpers() {
		t.Run(h.name, func(t *testing.T) {
			srv := newRecordingServer(t, http.StatusOK)
			url := srv.URL
			srv.Close()
			f := &fakeTB{}
			h.register(client.NewClient(url, "tok"), f)
			f.runCleanups()
			if len(f.errs) != 1 {
				t.Fatalf("transport failure reported %d times (%v), want once", len(f.errs), f.errs)
			}
		})
	}
}
