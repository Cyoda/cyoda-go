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

// DispatchForwarder sends a callout dispatch request to a peer node. The peer
// is named twice: by its node id, which the envelope is sealed for, and by the
// address the registry gave for it, which is dialled.
type DispatchForwarder interface {
	ForwardCallout(ctx context.Context, peerNodeID, addr string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error)
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
	// validation, the hand-over's time was spent before anything was written,
	// or the connection could not be opened (a dial error, the connect timeout
	// included). Another peer may still take the work.
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
	auth PeerAuth
	// connectTimeout bounds opening the connection on the dialer, and the name
	// lookup that precedes it: a resolver that is slow or down must not hold
	// the owner's goroutine past what dialling the same peer would cost.
	connectTimeout time.Duration
	client         *http.Client
	allowLoopback  bool
}

// NewHTTPForwarder constructs an HTTPForwarder. connectTimeout bounds opening
// the connection — the TCP connect and, for an https:// node address, the TLS
// handshake — and nothing else: how long to wait for the answer is the
// deadline on the context of each hand-over. Loopback peer addresses are
// rejected by default; see AllowLoopbackForTesting.
func NewHTTPForwarder(auth PeerAuth, connectTimeout time.Duration) *HTTPForwarder {
	dialer := &net.Dialer{Timeout: connectTimeout}
	return &HTTPForwarder{
		auth:           auth,
		connectTimeout: connectTimeout,
		client: &http.Client{
			// No Timeout: a hand-over may rightly take several answer limits.
			CheckRedirect: refuseRedirects,
			Transport: &http.Transport{
				// Every hand-over opens its own connection. On a kept-alive one
				// a peer that died is discovered only when the read fails —
				// after the whole wait, and indistinguishable from a peer that
				// took the work and then died. On a fresh one "could not
				// connect" is proof that nothing left this pnode.
				DisableKeepAlives: true,
				DialContext:       dialer.DialContext,
				// A failed handshake is not a dial error; it is at least
				// bounded like one.
				TLSHandshakeTimeout: connectTimeout,
				// Never a proxy: through one, a peer that is down is reported
				// as "proxyconnect", not "dial", and the rule above is lost.
				Proxy: nil,
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

// ForwardCallout POSTs a callout dispatch request to the peer at addr, sealed
// for peerNodeID, and returns the response.
func (f *HTTPForwarder) ForwardCallout(ctx context.Context, peerNodeID, addr string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	if peerNodeID == "" {
		// A peer that cannot be named cannot be sealed for. Like an address the
		// guard refuses, that is a property of this one peer: nothing is sent,
		// no try is used, and the loop goes on to the next peer. Sign would
		// refuse it too, but as a request that could not be built — which is the
		// reading reserved for what would fail towards every peer.
		return nil, stageErr(StageNotConnected, errors.New("the peer node has no id to seal the hand-over for"))
	}
	// The lookup a hostname address needs is bounded like the connection it
	// precedes, and never outlives the hand-over's own deadline.
	lookupCtx, cancel := context.WithTimeout(ctx, f.connectTimeout)
	defer cancel()
	if err := validatePeerAddress(lookupCtx, addr, f.allowLoopback); err != nil {
		// A property of this one peer, not of the callout: nothing was sent,
		// and another peer may still take the work.
		return nil, stageErr(StageNotConnected, err)
	}
	var resp DispatchCalloutResponse
	if err := f.forward(ctx, peerNodeID, ensureScheme(addr)+"/internal/dispatch/callout", &req, &resp); err != nil {
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
// encoding sealed for peerNodeID, POSTs the resulting bytes, and decodes the
// JSON response. The answer comes back sealed for this request and is opened
// with the binding Sign returned.
func (f *HTTPForwarder) forward(ctx context.Context, peerNodeID, url string, reqBody any, respBody any) error {
	plain, err := encodePeerBody(reqBody)
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

	wire, binding, err := f.auth.Sign(httpReq, peerNodeID, plain)
	if err != nil {
		return stageErr(StageBeforeConnect, fmt.Errorf("dispatch forward: sign body: %w", err))
	}
	if len(wire) > MaxEnvelopeSize {
		// Provable before connecting, and true of every peer: each reads at
		// most the ceiling, so these bytes would be truncated and refused
		// wherever they were sent. Terminal, not a lost answer — a lost answer
		// would spend a try and be retried identically on the next peer.
		return stageErr(StageBeforeConnect, fmt.Errorf("dispatch forward: the hand-over is %d bytes sealed and the envelope holds %d", len(wire), MaxEnvelopeSize))
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(wire))
	httpReq.ContentLength = int64(len(wire))

	if err := ctx.Err(); err != nil {
		// The time was already spent before anything was written: nothing left
		// this node, so no compute member can have the work. RoundTrip would
		// report the same context error, but not as a dial error, and the
		// hand-over would be read as an answer lost over a request never made —
		// one try spent, and a callout that is not repeat-safe failed. The
		// proof does not depend on every caller checking its context first.
		return stageErr(StageNotConnected, fmt.Errorf("dispatch forward: the hand-over's time was spent before it was sent: %w", err))
	}
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

	// One byte past the ceiling is read so that an answer above it can be told
	// apart from one at it: the extra byte is the whole of the difference, and
	// without the check below an oversized answer arrives truncated and is
	// blamed on the cipher.
	sealed, err := io.ReadAll(io.LimitReader(httpResp.Body, MaxEnvelopeSize+1))
	if err != nil {
		return stageErr(StageAfterConnect, fmt.Errorf("dispatch forward: read response from %s: %w", url, err))
	}
	if len(sealed) > MaxEnvelopeSize {
		return stageErr(StageAfterConnect, fmt.Errorf("dispatch forward: the answer from %s is too large for the envelope, which holds %d bytes", url, MaxEnvelopeSize))
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
