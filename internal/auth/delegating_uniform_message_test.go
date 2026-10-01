package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestDelegatingAuthenticator_UniformClientMessage pins the user-enumeration
// mitigation: every Authenticate failure must return
// an error whose Error() is exactly the generic string "authentication failed",
// with no per-branch suffix that would let a probing client distinguish the
// failure mode (e.g. "missing header" vs "token invalid").
//
// The middleware translates this error into the response body verbatim, so
// pinning err.Error() at this layer pins the client-visible string.
func TestDelegatingAuthenticator_UniformClientMessage(t *testing.T) {
	validator := auth.NewValidatorFromSource(staticKeySource{}, "cyoda")
	authn := auth.NewDelegatingAuthenticator(validator)

	cases := []struct {
		name      string
		setHeader func(r *http.Request)
	}{
		{
			name:      "missing-header",
			setHeader: func(r *http.Request) {},
		},
		{
			name: "invalid-format",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
			},
		},
		{
			name: "empty-bearer",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer ")
			},
		},
		{
			name: "token-invalid",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer not.a.real.token")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			tc.setHeader(req)

			_, err := authn.Authenticate(context.Background(), req)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, auth.ErrAuthenticationFailed) {
				t.Errorf("errors.Is(err, ErrAuthenticationFailed) = false; err = %v", err)
			}
			if err.Error() != "authentication failed" {
				t.Errorf("err.Error() = %q; want exactly \"authentication failed\" (no per-branch suffix that could enable user enumeration)", err.Error())
			}
		})
	}
}

// TestDelegatingAuthenticator_LogsStructuredReason pins the operator-side
// observability mirror to TestDelegatingAuthenticator_UniformClientMessage:
// even though the client-facing error string is uniform, server-side each
// failure must emit exactly one slog.Warn record with a structured `reason`
// field carrying the specific failure mode slug.
func TestDelegatingAuthenticator_LogsStructuredReason(t *testing.T) {
	validator := auth.NewValidatorFromSource(staticKeySource{}, "cyoda")
	authn := auth.NewDelegatingAuthenticator(validator)

	cases := []struct {
		name       string
		setHeader  func(r *http.Request)
		wantReason string
	}{
		{
			name:       "missing-header",
			setHeader:  func(r *http.Request) {},
			wantReason: "missing-header",
		},
		{
			name: "invalid-format",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
			},
			wantReason: "invalid-format",
		},
		{
			name: "empty-bearer",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer ")
			},
			wantReason: "empty-bearer",
		},
		{
			name: "token-invalid",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer not.a.real.token")
			},
			wantReason: "token-invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			req.RemoteAddr = "192.0.2.7:54321"
			tc.setHeader(req)

			_, err := authn.Authenticate(context.Background(), req)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}

			// Parse exactly the warn-level lines emitted by Authenticate.
			lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
			var records []map[string]any
			for _, ln := range lines {
				if ln == "" {
					continue
				}
				var rec map[string]any
				if err := json.Unmarshal([]byte(ln), &rec); err != nil {
					t.Fatalf("unparseable log line %q: %v", ln, err)
				}
				if lvl, _ := rec["level"].(string); lvl == "WARN" {
					records = append(records, rec)
				}
			}
			if len(records) != 1 {
				t.Fatalf("expected exactly 1 WARN log record, got %d; buf=%s", len(records), buf.String())
			}
			rec := records[0]
			if got, _ := rec["reason"].(string); got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
			// remote_addr context is required for operator triage.
			if got, _ := rec["remote_addr"].(string); got != "192.0.2.7:54321" {
				t.Errorf("remote_addr = %q, want %q", got, "192.0.2.7:54321")
			}
			// pkg label per logging.md.
			if got, _ := rec["pkg"].(string); got != "auth" {
				t.Errorf("pkg = %q, want \"auth\"", got)
			}
			// No PII / token material may appear in the log message or fields.
			full := buf.String()
			if strings.Contains(full, "not.a.real.token") {
				t.Errorf("log leaked raw token: %s", full)
			}
		})
	}
}
