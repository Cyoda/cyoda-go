package dispatch

import (
	"context"
	"errors"
	"net/http"
)

// ErrNonceReplayed is returned by Verify for a request whose nonce was seen
// before. No binding comes with it: there is no request to bind an answer to
// that is not also the request the replay copies. An answer sealed under it
// would be indistinguishable from the genuine one, so an attacker who captures
// a hand-over, lets the peer run it, replays it and delivers the replay's
// answer could have the owner give a non-repeat-safe callout to a second
// compute member. A bare status leaves that attacker able only to lose the
// answer, as before.
var ErrNonceReplayed = errors.New("request nonce was seen before")

// ErrReplayCacheFull is returned by Verify when a request opened and
// authenticated and only the replay cache's capacity refused it. Nothing ran,
// and only a holder of the key can fill the cache, so the binding returned
// beside it is valid: the handler answers under seal rather than with a bare
// status, which would fail an operation that is not repeat-safe.
var ErrReplayCacheFull = errors.New("replay cache is full")

// ResponseBinding is what ties an answer to the one request it answers: the
// request's path, nonce and timestamp, and the node it was sealed for. Sign
// returns it to the sender, naming the node it asked; Verify returns the same
// value to the receiver, naming itself. The two agree only where the request
// reached the node it was meant for, so an answer from any other node of the
// cluster — which holds the same key — does not open. The zero value binds to
// nothing and neither seals nor opens.
type ResponseBinding struct {
	recipient string
	path      string
	nonce     []byte
	ts        string
}

// PeerAuth authenticates inter-node dispatch HTTP requests at the message
// layer. Implementations wrap outbound bodies on the client and verify them
// on the server, returning the authenticated plaintext plus a PeerIdentity.
//
// The interface is deliberately split so future transports (e.g. mTLS, where
// authentication is transport-layer) can implement Sign as a no-op and derive
// identity from tls.ConnectionState in Verify — without disturbing the
// forwarder or handler call sites.
//
// Contract between PeerAuth and HTTPForwarder.forward:
//   - forward sets Content-Type: application/json as the default BEFORE
//     calling Sign. Impls using a different wire format (e.g. AEAD) override
//     it inside Sign. Transport-auth-only impls (e.g. mTLS carrying plain
//     JSON) can leave it alone.
//   - forward replaces req.Body with the returned wireBody after Sign.
//     Impls may return body unchanged if authentication is fully transport
//     layer.
//   - forward keeps the binding Sign returned and opens the answer with it; a
//     transport-auth-only impl returns the body unchanged from both
//     SealResponse and OpenResponse.
type PeerAuth interface {
	// Sign transforms the plaintext body into an on-the-wire body sealed for
	// recipientNodeID — the id of the node the caller resolved the address of,
	// which only that node can open the envelope under — setting any required
	// headers on req, and returns the binding under which the answer to this
	// request must be opened. An empty recipientNodeID is refused: the caller
	// has not established which node it is talking to. The returned slice
	// replaces req.Body at the call site. Implementations MAY write to
	// req.Header (including overriding Content-Type) but MUST NOT capture or
	// retain req past the call.
	Sign(req *http.Request, recipientNodeID string, body []byte) (wireBody []byte, binding ResponseBinding, err error)

	// Verify reads the request body, validates it — including that it was
	// sealed for THIS node — and returns the authenticated plaintext, the
	// peer's identity and the binding for the answer. A non-nil error means the
	// request is refused, an envelope sealed for another node included. The
	// binding is
	// valid when err is nil and when errors.Is(err, ErrReplayCacheFull); for
	// any other error — ErrNonceReplayed included — it is the zero value and
	// the caller must respond with 403.
	// identity is meaningful exactly when binding is: for any other error it is
	// also the zero value and MUST NOT be acted on or attached to a context —
	// the caller has not authenticated a peer. The returned identity MUST be
	// populated (even if degenerate) so handlers can attach it to the request
	// context unconditionally in the cases where binding is valid.
	Verify(r *http.Request) (body []byte, identity PeerIdentity, binding ResponseBinding, err error)

	// SealResponse wraps an answer for the request binding names, setting the
	// headers the wire format needs on h.
	SealResponse(h http.Header, binding ResponseBinding, body []byte) (wireBody []byte, err error)

	// OpenResponse is the inverse, on the side that sent the request. It
	// fails for an answer that was not sealed, with this key, for exactly the
	// request binding names.
	OpenResponse(h http.Header, binding ResponseBinding, wireBody []byte) (body []byte, err error)
}

// PeerIdentity describes an authenticated cluster peer. Today (shared-key
// AEAD) identity is degenerate — the sender held the derived dispatch key,
// nothing more. In a future mTLS transport, NodeID carries the specific
// node identifier from the peer certificate's CommonName. The abstraction
// is stable across both so callers that care about origin can start reading
// it today without having to be rewritten when transport changes.
type PeerIdentity struct {
	authMethod string
	nodeID     string
}

// AuthMethod returns the auth-mechanism tag (e.g. "aead-v1", "mtls").
// Used primarily for diagnostics and audit trails.
func (i PeerIdentity) AuthMethod() string { return i.authMethod }

// NodeID returns the specific peer node ID if the auth mechanism proves it,
// or the empty string otherwise. Callers MUST treat empty NodeID as
// "authenticated cluster member, identity unknown" rather than "anonymous".
func (i PeerIdentity) NodeID() string { return i.nodeID }

// NewPeerIdentityForTesting constructs a PeerIdentity with the given fields.
// Production PeerAuth implementations have their own internal constructors
// so the zero-value invariant stays meaningful; tests use this helper.
func NewPeerIdentityForTesting(authMethod, nodeID string) PeerIdentity {
	return PeerIdentity{authMethod: authMethod, nodeID: nodeID}
}

type peerIdentityCtxKey struct{}

// WithPeerIdentity returns ctx annotated with the authenticated peer identity.
// Handlers call this after Verify succeeds so downstream code can audit the
// origin without re-running authentication.
func WithPeerIdentity(ctx context.Context, id PeerIdentity) context.Context {
	return context.WithValue(ctx, peerIdentityCtxKey{}, id)
}

// PeerIdentityFromContext retrieves the peer identity placed by
// WithPeerIdentity. The boolean is false if no identity was set — callers
// that expect one (i.e. inside a dispatch handler) should treat absence
// as a bug rather than as "anonymous caller".
func PeerIdentityFromContext(ctx context.Context) (PeerIdentity, bool) {
	id, ok := ctx.Value(peerIdentityCtxKey{}).(PeerIdentity)
	return id, ok
}
