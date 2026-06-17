package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DiscoveryDoc is the subset of the OIDC discovery document we depend on at
// runtime. The full document has many fields; we decode only the two we use and
// intentionally do not call DisallowUnknownFields so additional provider fields
// are silently ignored.
type DiscoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// Discovery is the abstract interface for fetching OIDC discovery documents.
// The interface exists to enable mocking in higher-level tests.
type Discovery interface {
	Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error)
}

// DiscoveryConfig holds tunable timeouts for the HTTP discovery client.
// Zero values fall back to 5 s defaults.
type DiscoveryConfig struct {
	ConnectTimeout           time.Duration
	SocketTimeout            time.Duration
	ConnectionRequestTimeout time.Duration
	// AllowPrivateNetworks bypasses the SSRF blocklist check (test/dev only).
	AllowPrivateNetworks bool
}

// HTTPDiscovery fetches OIDC discovery documents over HTTP/HTTPS with fetch-
// time SSRF defence (safeDialContext) and redirect following disabled.
type HTTPDiscovery struct {
	client *http.Client
}

// NewHTTPDiscovery constructs an HTTPDiscovery with the given config.
func NewHTTPDiscovery(cfg DiscoveryConfig) *HTTPDiscovery {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.SocketTimeout <= 0 {
		cfg.SocketTimeout = 5 * time.Second
	}
	if cfg.ConnectionRequestTimeout <= 0 {
		cfg.ConnectionRequestTimeout = 5 * time.Second
	}
	transport := &http.Transport{
		DialContext:           safeDialContext(cfg.AllowPrivateNetworks),
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ResponseHeaderTimeout: cfg.SocketTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.SocketTimeout + cfg.ConnectTimeout + cfg.ConnectionRequestTimeout,
		// Fail-closed: never follow redirects. D10 mitigation.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &HTTPDiscovery{client: client}
}

// Fetch retrieves and validates the OIDC discovery document at uri.
// It returns ErrDiscoveryFailed (wrapped) for any HTTP, parse, or validation
// failure. The caller's ctx deadline is honoured.
func (d *HTTPDiscovery) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrDiscoveryFailed, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDiscoveryFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: HTTP %d (redirects disabled)", ErrDiscoveryFailed, resp.StatusCode)
	}

	var doc DiscoveryDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: malformed JSON: %w", ErrDiscoveryFailed, err)
	}
	if doc.Issuer == "" || doc.JWKSURI == "" {
		return nil, fmt.Errorf("%w: discovery doc missing issuer or jwks_uri", ErrDiscoveryFailed)
	}
	return &doc, nil
}
