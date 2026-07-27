package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DispatchForwarder sends a callout dispatch request to a peer node.
type DispatchForwarder interface {
	ForwardCallout(ctx context.Context, addr string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error)
}

// HTTPForwarder implements DispatchForwarder over HTTP with a PeerAuth
// message-authentication wrapper. The auth impl — today AEADPeerAuth over
// a shared secret, tomorrow potentially an mTLS variant — owns signing and
// verification; the forwarder itself is transport plumbing.
type HTTPForwarder struct {
	auth          PeerAuth
	client        *http.Client
	allowLoopback bool
}

// NewHTTPForwarder constructs an HTTPForwarder. Loopback peer addresses
// are rejected by default; see AllowLoopbackForTesting.
func NewHTTPForwarder(auth PeerAuth, timeout time.Duration) *HTTPForwarder {
	return &HTTPForwarder{
		auth: auth,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// AllowLoopbackForTesting opts the forwarder out of the loopback SSRF
// guard so unit and integration tests can target an httptest.Server on
// 127.0.0.1. Link-local, unspecified, and multicast addresses are still
// rejected. Never call this in production — it re-opens the SSRF pivot
// the guard was written to close. Returns the receiver for fluent use
// at construction sites: `NewHTTPForwarder(...).AllowLoopbackForTesting()`.
func (f *HTTPForwarder) AllowLoopbackForTesting() *HTTPForwarder {
	f.allowLoopback = true
	return f
}

// ForwardCallout POSTs a callout dispatch request to the peer at addr and returns the response.
func (f *HTTPForwarder) ForwardCallout(ctx context.Context, addr string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	if err := validatePeerAddress(addr, f.allowLoopback); err != nil {
		return nil, err
	}
	var resp DispatchCalloutResponse
	if err := f.forward(ctx, ensureScheme(addr)+"/internal/dispatch/callout", &req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ensureScheme prepends http:// if the address has no scheme.
func ensureScheme(addr string) string {
	if !strings.Contains(addr, "://") {
		return "http://" + addr
	}
	return addr
}

// forward marshals reqBody as JSON, hands it to the PeerAuth for wire
// encoding, POSTs the resulting bytes, and decodes the (plaintext) JSON
// response. Response bodies are not AEAD-wrapped — integrity of the
// peer's reply relies on TLS (future) or the trust boundary that auth
// established for the request.
func (f *HTTPForwarder) forward(ctx context.Context, url string, reqBody any, respBody any) error {
	plain, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("dispatch forward: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("dispatch forward: build request: %w", err)
	}

	// Default Content-Type is application/json — the plaintext format the
	// handler parses after Verify. PeerAuth impls MAY override this in Sign
	// if they use a wire format that supersedes JSON (e.g. AEADPeerAuth sets
	// application/cyoda-dispatch-v1); a pure-transport-auth impl like future
	// mTLS can leave it alone.
	httpReq.Header.Set("Content-Type", "application/json")

	wire, err := f.auth.Sign(httpReq, plain)
	if err != nil {
		return fmt.Errorf("dispatch forward: sign body: %w", err)
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(wire))
	httpReq.ContentLength = int64(len(wire))

	httpResp, err := f.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("dispatch forward: HTTP POST %s: %w", url, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return fmt.Errorf("dispatch forward: peer returned %d: %s", httpResp.StatusCode, raw)
	}

	if err := json.NewDecoder(httpResp.Body).Decode(respBody); err != nil {
		return fmt.Errorf("dispatch forward: decode response from %s: %w", url, err)
	}
	return nil
}
