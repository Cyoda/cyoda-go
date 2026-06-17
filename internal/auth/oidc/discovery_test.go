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
