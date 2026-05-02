package proxy

// director_test.go uses package proxy (white-box) rather than the existing
// http_test.go's package proxy_test (black-box) because makeProxyDirector is
// unexported. The two test files coexist; tests in this file exercise the
// helper directly, while http_test.go covers the public HTTPRouting surface.

import (
	"net/http"
	"net/url"
	"testing"
)

func TestDirector_StripsCORSPreflightHeaders(t *testing.T) {
	target, _ := url.Parse("http://peer-b:8080")
	director := makeProxyDirector(target)

	req, err := http.NewRequest(http.MethodPost, "http://peer-a/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://browser.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization")
	req.Header.Set("Authorization", "Bearer xyz")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "abc-123")
	req.Header.Set("X-Tx-Token", "transaction-token")

	director(req)

	for _, h := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s should be stripped, got %q", h, got)
		}
	}
	// Authorization (and other unrelated headers) must NOT be stripped.
	if got := req.Header.Get("Authorization"); got != "Bearer xyz" {
		t.Errorf("Authorization should be preserved, got %q", got)
	}
	preserved := map[string]string{
		"Content-Type": "application/json",
		"X-Request-ID": "abc-123",
		"X-Tx-Token":   "transaction-token",
	}
	for h, want := range preserved {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s should be preserved as %q, got %q", h, want, got)
		}
	}
	// Director must rewrite scheme/host/Host onto the target.
	if req.URL.Scheme != "http" || req.URL.Host != "peer-b:8080" {
		t.Errorf("URL not rewritten: scheme=%q host=%q", req.URL.Scheme, req.URL.Host)
	}
	if req.Host != "peer-b:8080" {
		t.Errorf("Host not rewritten: %q", req.Host)
	}
}

func TestDirector_StripsAreIdempotent(t *testing.T) {
	target, _ := url.Parse("http://peer-b:8080")
	director := makeProxyDirector(target)

	req, _ := http.NewRequest(http.MethodGet, "http://peer-a/x", nil)
	// No headers set. Strip should not panic and request should remain valid.
	director(req)

	if req.URL.Host != "peer-b:8080" {
		t.Errorf("URL not rewritten: %q", req.URL.Host)
	}
}
