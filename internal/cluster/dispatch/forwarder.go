package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DispatchForwarder sends a callout dispatch request to a peer node.
type DispatchForwarder interface {
	ForwardCallout(ctx context.Context, addr string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error)
}

// ForwardStage says how far a hand-over got before it failed. It is what lets
// the owner tell "nothing left this pnode" from "the peer may have the work".
type ForwardStage int

const (
	// StageBeforeConnect: the request could not be built, marshalled or
	// signed. No connection was attempted, and the same would happen towards
	// every peer.
	StageBeforeConnect ForwardStage = iota
	// StageNotConnected: nothing was sent to this peer — its address failed
	// validation, or the connection could not be opened (a dial error, the
	// connect timeout included). Another peer may still take the work.
	StageNotConnected
	// StageAfterConnect: anything later — a transport error, the wait running
	// out, a non-2xx status, an answer that is truncated or does not open.
	StageAfterConnect
)

// ForwardError is the error of a hand-over that produced no decoded answer.
type ForwardError struct {
	Stage ForwardStage
	Err   error
}

func (e *ForwardError) Error() string { return e.Err.Error() }
func (e *ForwardError) Unwrap() error { return e.Err }

func stageErr(stage ForwardStage, err error) error { return &ForwardError{Stage: stage, Err: err} }

// isDialError reports whether err is the connection failing to open. With a
// proxy the same failure is reported as "proxyconnect", and a failed TLS
// handshake is not a dial error either: both count as after-connect, which errs
// on the safe side.
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
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
			Timeout:       timeout,
			CheckRedirect: refuseRedirects,
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
		// A property of this one peer, not of the callout: nothing was sent,
		// and another peer may still take the work.
		return nil, stageErr(StageNotConnected, err)
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
// encoding, POSTs the resulting bytes, and decodes the JSON response. The
// answer comes back sealed for this request and is opened with the binding
// Sign returned.
func (f *HTTPForwarder) forward(ctx context.Context, url string, reqBody any, respBody any) error {
	plain, err := json.Marshal(reqBody)
	if err != nil {
		return stageErr(StageBeforeConnect, fmt.Errorf("dispatch forward: marshal request: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return stageErr(StageBeforeConnect, fmt.Errorf("dispatch forward: build request: %w", err))
	}

	// Default Content-Type is application/json — the plaintext format the
	// handler parses after Verify. PeerAuth impls MAY override this in Sign
	// if they use a wire format that supersedes JSON (e.g. AEADPeerAuth sets
	// application/cyoda-dispatch-v1); a pure-transport-auth impl like future
	// mTLS can leave it alone.
	httpReq.Header.Set("Content-Type", "application/json")

	wire, binding, err := f.auth.Sign(httpReq, plain)
	if err != nil {
		return stageErr(StageBeforeConnect, fmt.Errorf("dispatch forward: sign body: %w", err))
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(wire))
	httpReq.ContentLength = int64(len(wire))

	httpResp, err := f.client.Do(httpReq)
	if err != nil {
		stage := StageAfterConnect
		if isDialError(err) {
			stage = StageNotConnected
		}
		return stageErr(stage, fmt.Errorf("dispatch forward: HTTP POST %s: %w", url, err))
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return stageErr(StageAfterConnect, fmt.Errorf("dispatch forward: peer returned %d: %s", httpResp.StatusCode, raw))
	}

	sealed, err := io.ReadAll(io.LimitReader(httpResp.Body, MaxEnvelopeSize+1))
	if err != nil {
		return stageErr(StageAfterConnect, fmt.Errorf("dispatch forward: read response from %s: %w", url, err))
	}
	opened, err := f.auth.OpenResponse(httpResp.Header, binding, sealed)
	if err != nil {
		return stageErr(StageAfterConnect, fmt.Errorf("dispatch forward: open response from %s: %w", url, err))
	}
	if err := json.Unmarshal(opened, respBody); err != nil {
		return stageErr(StageAfterConnect, fmt.Errorf("dispatch forward: decode response from %s: %w", url, err))
	}
	return nil
}
