package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	logger *slog.Logger
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
	return &HTTPDiscovery{client: client, logger: slog.Default()}
}

// Fetch retrieves and validates the OIDC discovery document at uri.
// It returns ErrDiscoveryFailed (wrapped) for any HTTP, parse, or validation
// failure. The caller's ctx deadline is honoured.
//
// Security: all error paths that wrap network or upstream-response details are
// sanitized here. The public error string exposes class-only information (error
// category + HTTP status code where applicable). Verbose detail is captured in a
// DEBUG log using classifyHTTPError so operators can triage failures without
// exposing raw addresses or OS-level strings to callers — which may surface them
// in logs at higher levels or in HTTP response bodies under verbose error mode.
func (d *HTTPDiscovery) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
	uriHash := sha256Hex(uri)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		// build-request errors contain the raw URI (which may expose internal
		// targets); log the class only and return a sanitized error.
		d.logger.Debug("oidc discovery: build request failed",
			"pkg", "oidc", "uri_hash", uriHash,
			"error_class", classifyHTTPError(err))
		return nil, fmt.Errorf("%w: build request failed", ErrDiscoveryFailed)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		// Network errors (DNS, TCP connect, TLS, timeout) are logged at DEBUG
		// with a class-only classifier. The public error carries no upstream
		// address or OS string. HTTP status codes are NOT included here (this
		// path covers transport-layer failures, not HTTP-level errors).
		d.logger.Debug("oidc discovery: http request failed",
			"pkg", "oidc", "uri_hash", uriHash,
			"error_class", classifyHTTPError(err))
		return nil, fmt.Errorf("%w: http request failed", ErrDiscoveryFailed)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// HTTP status codes are operational (non-sensitive): administrators
		// need them to diagnose IdP-side problems (5xx, misrouting, etc.).
		return nil, fmt.Errorf("%w: HTTP %d (redirects disabled)", ErrDiscoveryFailed, resp.StatusCode)
	}

	var doc DiscoveryDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		// JSON decode errors can contain fragments of the upstream response body;
		// log class only and return a sanitized error.
		d.logger.Debug("oidc discovery: json decode failed",
			"pkg", "oidc", "uri_hash", uriHash,
			"error_class", fmt.Sprintf("%T", err))
		return nil, fmt.Errorf("%w: malformed json in discovery document", ErrDiscoveryFailed)
	}
	if doc.Issuer == "" || doc.JWKSURI == "" {
		return nil, fmt.Errorf("%w: discovery doc missing issuer or jwks_uri", ErrDiscoveryFailed)
	}
	return &doc, nil
}

// classifyHTTPError returns a short, safe string describing the category of an
// HTTP-transport-level error without exposing IP addresses, hostnames, ports, or
// OS-level error strings. It is used in DEBUG logs so operators can triage
// failures while the public error string remains sanitized.
func classifyHTTPError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Op is safe to expose: "dial", "read", "write", etc.
		return "net:" + opErr.Op
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Op is safe: "Get", "Post", etc.
		return "url:" + urlErr.Op
	}
	return fmt.Sprintf("%T", err)
}
