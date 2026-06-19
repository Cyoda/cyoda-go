package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
)

// TestDispatchHelpAction_HandlesFailure verifies that an action handler
// returning rc != 0 yields HTTP 500 and does NOT leak partial output.
func TestDispatchHelpAction_HandlesFailure(t *testing.T) {
	t.Parallel()
	failing := help.ActionEntry{
		Handler: func(w io.Writer) int {
			_, _ = io.WriteString(w, "partial output before failure")
			return 1
		},
		ContentType: "application/json",
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/help/test/topic/action", nil)
	dispatchHelpAction(rec, req, "test.topic", "action", failing)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "partial output") {
		t.Fatalf("failure response leaked partial output from handler: %s", rec.Body.String())
	}
}

// TestDispatchHelpAction_WritesContentLength verifies that a successful action
// response includes Content-Length and the full body.
func TestDispatchHelpAction_WritesContentLength(t *testing.T) {
	t.Parallel()
	ok := help.ActionEntry{
		Handler: func(w io.Writer) int {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return 0
		},
		ContentType: "application/json",
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/help/test/topic/action", nil)
	dispatchHelpAction(rec, req, "test.topic", "action", ok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q; want 11", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q; want application/json", got)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q; want %q", rec.Body.String(), `{"ok":true}`)
	}
}

// TestHelpActionMirror — every registered topic action is reachable
// via HTTP at GET /help/<topic-with-dots-or-slashes>/<action> with
// its declared Content-Type.
func TestHelpActionMirror(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	RegisterHelpRoutes(mux, help.DefaultTree, "", "test-version")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cases := []struct {
		path        string
		wantStatus  int
		wantPrefix  string // first few bytes of body, format-specific
		contentType string
	}{
		// Existing actions — confirms the mirror is generic.
		{"/help/grpc/proto", http.StatusOK, "//", "text/plain; charset=utf-8"},
		{"/help/grpc/json", http.StatusOK, "{", "application/json"},
		{"/help/openapi/json", http.StatusOK, "{", "application/json"},
		{"/help/cloudevents/json", http.StatusOK, "{", "application/json"},
		{"/help/workflows/schema-version/versions", http.StatusOK, "{", "application/json"},
		// Equivalent with dotted separators.
		{"/help/grpc.json", http.StatusOK, "{", "application/json"},
		// Unknown action — falls through to topic-not-found.
		{"/help/grpc/nonsense", http.StatusNotFound, "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s status = %d; want %d", tc.path, resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			ct := resp.Header.Get("Content-Type")
			if ct != tc.contentType {
				t.Fatalf("GET %s Content-Type = %q; want %q", tc.path, ct, tc.contentType)
			}
			buf := make([]byte, 64)
			n, _ := resp.Body.Read(buf)
			if !strings.HasPrefix(string(buf[:n]), tc.wantPrefix) {
				t.Fatalf("GET %s body prefix = %q; want %q", tc.path, string(buf[:n]), tc.wantPrefix)
			}
		})
	}
}
