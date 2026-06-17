package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPDiscovery_FetchSuccess(t *testing.T) {
	body := `{"issuer":"https://idp.example","jwks_uri":"https://idp.example/jwks","other":"ignored"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	doc, err := d.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.Issuer != "https://idp.example" {
		t.Errorf("Issuer = %q, want https://idp.example", doc.Issuer)
	}
	if doc.JWKSURI != "https://idp.example/jwks" {
		t.Errorf("JWKSURI = %q, want https://idp.example/jwks", doc.JWKSURI)
	}
}

func TestHTTPDiscovery_DoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"compromised","jwks_uri":"x"}`))
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	_, err := d.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error on redirect, got nil")
	}
}

func TestHTTPDiscovery_BlockedHostByDialer(t *testing.T) {
	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: false})
	_, err := d.Fetch(context.Background(), "https://127.0.0.1/.well-known/openid-configuration")
	if err == nil {
		t.Fatal("expected fetch-time SSRF block, got nil")
	}
}

func TestHTTPDiscovery_HonoursContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"issuer":"x","jwks_uri":"y"}`))
	}))
	defer srv.Close()

	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := d.Fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected ctx.Deadline error")
	}
}

func TestHTTPDiscovery_MissingFieldsRejected(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"missing-issuer", `{"jwks_uri":"https://idp.example/jwks"}`},
		{"missing-jwks-uri", `{"issuer":"https://idp.example"}`},
		{"empty-issuer", `{"issuer":"","jwks_uri":"https://idp.example/jwks"}`},
		{"empty-jwks-uri", `{"issuer":"https://idp.example","jwks_uri":""}`},
		{"malformed-json", `not json at all`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
			_, err := d.Fetch(context.Background(), srv.URL)
			if err == nil {
				t.Errorf("expected error for %s, got nil", c.name)
			}
		})
	}
}

func TestHTTPDiscovery_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream broken`))
	}))
	defer srv.Close()
	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	_, err := d.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error mentioning 500, got %v", err)
	}
}

// TestHTTPDiscovery_FetchError_DoesNotLeakUpstreamDetails verifies that network
// errors (connection refused, DNS failure, etc.) are sanitized at the Fetch
// boundary. The returned error must not contain:
//   - raw OS error strings like "connection refused"
//   - IP addresses or hostnames
//   - port numbers from the upstream target
//
// This is the A1+B1 audit finding: without sanitization the raw net.Error
// propagates through the chain and can reach HTTP response bodies (verbose mode)
// or log entries at WARN level.
func TestHTTPDiscovery_FetchError_DoesNotLeakUpstreamDetails(t *testing.T) {
	// Start and immediately close a server to get a guaranteed "connection
	// refused" target on a real ephemeral port so the test is deterministic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	targetURL := srv.URL
	srv.Close() // close before Fetch so the dial will fail

	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	_, err := d.Fetch(context.Background(), targetURL)
	if err == nil {
		t.Fatal("expected an error for closed server, got nil")
	}

	msg := err.Error()

	// The public error must NOT contain raw OS-level error strings.
	leaked := []string{
		"connection refused",
		"connection reset",
		"no such host",
		"i/o timeout",
		"EOF",
	}
	for _, s := range leaked {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(s)) {
			t.Errorf("error leaks upstream detail %q: %q", s, msg)
		}
	}

	// Must still identify as a discovery failure.
	if !strings.Contains(msg, "discovery") && !strings.Contains(msg, "failed") {
		t.Errorf("error should contain discovery class wording, got: %q", msg)
	}
}

// TestHTTPDiscovery_FetchHTTPStatus_StatusInError verifies that HTTP status
// codes (which are non-sensitive operational information) are preserved in the
// public error so administrators can diagnose IdP-side problems.
func TestHTTPDiscovery_FetchHTTPStatus_StatusInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502
		_, _ = w.Write([]byte("bad gateway"))
	}))
	defer srv.Close()

	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	_, err := d.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 502, got nil")
	}

	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should contain HTTP status code 502, got: %q", err.Error())
	}
}

// TestHTTPDiscovery_BuildRequestError_DoesNotLeakRawError verifies that an
// invalid URI (which triggers http.NewRequestWithContext failure) is sanitized.
// The error must not expose the raw url.Error detail string.
func TestHTTPDiscovery_BuildRequestError_DoesNotLeakRawError(t *testing.T) {
	d := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	// A URL with a control character is rejected by NewRequestWithContext.
	_, err := d.Fetch(context.Background(), "http://\x00bad")
	if err == nil {
		t.Fatal("expected error for invalid URI, got nil")
	}

	msg := err.Error()
	// Must not expose the raw url.Error() string which contains the invalid
	// URI verbatim. The sanitized message should only contain class wording.
	if strings.Contains(msg, "\x00") {
		t.Errorf("error leaks control character from URI: %q", msg)
	}
	if !strings.Contains(msg, "discovery") && !strings.Contains(msg, "failed") {
		t.Errorf("error should contain discovery class wording, got: %q", msg)
	}
}
