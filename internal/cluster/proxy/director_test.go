package proxy

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
