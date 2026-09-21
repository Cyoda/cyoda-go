package dispatch

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/hkdf"
)

// Wire-level constants. Versioned in the Content-Type so a future envelope
// format can coexist with this one if a migration is ever required.
const (
	DispatchContentType  = "application/cyoda-dispatch-v1"
	DispatchTimestampHdr = "X-Dispatch-Timestamp"

	// MaxEnvelopeSize caps how much an attacker can force either end to
	// buffer before the envelope is rejected. Exported because every leg that
	// reads a peer envelope — callout dispatch and the scheduler RPC alike —
	// bounds its read by the one ceiling.
	//
	// It sits above the 10 MiB an entity write may carry, with room for the
	// meta, the definition, the roles, the tags and the envelope's own 28
	// bytes, so that an entity the API stores can always be handed over: at the
	// same ceiling, an entity within a few KiB of the limit would make a
	// callout that no peer could take, while the same callout served by a local
	// compute member succeeds. Above this an envelope is refused by whichever
	// end builds it, which for a hand-over is Terminal and uses no try.
	MaxEnvelopeSize = 12 * 1024 * 1024

	// nonceCacheCapacity is the replay-cache ceiling — see nonceCache.
	nonceCacheCapacity = 100_000

	// hkdfInfo separates the dispatch key from memberlist's gossip key
	// derived from the same shared secret. Do not change: it's the binding
	// between key material versions.
	hkdfInfo = "cyoda-dispatch-v1"

	authMethodAEADv1 = "aead-v1"
)

// ErrSharedSecretTooShort is returned by NewAEADPeerAuth when the provided
// shared secret is under the 32-byte minimum.
var ErrSharedSecretTooShort = errors.New("shared secret must be at least 32 bytes")

// ErrNodeIDRequired is returned by NewAEADPeerAuth when no node id is given. A
// node that cannot name itself cannot tell an envelope sealed for it from one
// sealed for another node, which is the whole of what the recipient binding is
// for.
var ErrNodeIDRequired = errors.New("peer auth needs this node's id")

// ErrRecipientRequired is returned by Sign when no recipient is named. The
// envelope would be sealed under associated data no node builds, so it is
// refused here rather than sent to fail at the far end as a lost answer.
var ErrRecipientRequired = errors.New("an envelope must name the node it is sealed for")

// AEADPeerAuth implements PeerAuth using AES-256-GCM with an HKDF-derived key.
//
// On the wire, a body is [nonce(12) || ciphertext||tag], in both directions.
// The associated data of a request binds a direction label, the HTTP method,
// the path, the timestamp and the id of the node it is sealed FOR; that of an
// answer binds the other direction label, the path, the timestamp, the same
// recipient and the nonce of the request it answers. So an envelope cannot be
// replayed across endpoints, reflected back in the other direction, moved onto
// another request, or — the reason the recipient is in both — delivered to
// another node of the cluster, which holds the same key and would otherwise
// both accept it and seal an answer the owner could not tell from the genuine
// one. The recipient is never on the wire: the sender names the node it looked
// up, the receiver names itself, and the two agree only when the envelope
// reached the node it was meant for.
//
// A sliding nonce cache rejects repeated requests within the skew window;
// answers need none, being bound to a request nonce their receiver chose.
type AEADPeerAuth struct {
	gcm        cipher.AEAD
	selfNodeID string
	nonces     *nonceCache
	skew       time.Duration
	clockFn    func() time.Time
}

// NewAEADPeerAuth returns an AEADPeerAuth keyed by HKDF-SHA256 over the
// given shared secret, for the node named selfNodeID. The secret is the same
// cluster-wide value as CYODA_HMAC_SECRET; HKDF separates the dispatch key from
// the memberlist gossip key so a compromise of one primitive does not extend to
// the other. selfNodeID is this node's own id, which is what every inbound
// envelope must have been sealed for.
func NewAEADPeerAuth(sharedSecret []byte, selfNodeID string, skew time.Duration) (*AEADPeerAuth, error) {
	return newAEADPeerAuth(sharedSecret, selfNodeID, skew, time.Now)
}

// NewAEADPeerAuthWithClockForTesting is an AEADPeerAuth whose notion of "now"
// is controlled by the caller. Reserved for tests that exercise timestamp
// skew and replay-TTL behaviour. Production code MUST use NewAEADPeerAuth.
func NewAEADPeerAuthWithClockForTesting(sharedSecret []byte, selfNodeID string, skew time.Duration, clockFn func() time.Time) (*AEADPeerAuth, error) {
	return newAEADPeerAuth(sharedSecret, selfNodeID, skew, clockFn)
}

func newAEADPeerAuth(sharedSecret []byte, selfNodeID string, skew time.Duration, clockFn func() time.Time) (*AEADPeerAuth, error) {
	if len(sharedSecret) < 32 {
		return nil, ErrSharedSecretTooShort
	}
	if selfNodeID == "" {
		return nil, ErrNodeIDRequired
	}
	key := deriveDispatchKey(sharedSecret)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build AES-GCM: %w", err)
	}
	return &AEADPeerAuth{
		gcm:        gcm,
		selfNodeID: selfNodeID,
		nonces:     newNonceCache(skew*2, nonceCacheCapacity, clockFn),
		skew:       skew,
		clockFn:    clockFn,
	}, nil
}

// SetClockForTesting swaps the clock function for both the AEAD and its
// nonce cache. Reserved for tests that need to manipulate observed time.
// Production code MUST NOT call this.
func (a *AEADPeerAuth) SetClockForTesting(clockFn func() time.Time) {
	a.clockFn = clockFn
	a.nonces.nowFn = clockFn
}

// DeriveDispatchKeyForTesting exposes the HKDF key derivation for a single
// test that proves derivation actually runs. Production code uses it
// internally only.
func DeriveDispatchKeyForTesting(sharedSecret []byte) []byte {
	return deriveDispatchKey(sharedSecret)
}

func deriveDispatchKey(sharedSecret []byte) []byte {
	r := hkdf.New(sha256.New, sharedSecret, nil, []byte(hkdfInfo))
	out := make([]byte, 32)
	if _, err := io.ReadFull(r, out); err != nil {
		// hkdf.New's Reader cannot fail to produce 32 bytes from any
		// non-empty secret; guard is belt-and-suspenders.
		panic(fmt.Sprintf("hkdf derivation failed: %v", err))
	}
	return out
}

// The direction labels that keep an envelope in the leg it was sealed for.
const (
	directionRequest  = "request"
	directionResponse = "response"
)

// Sign wraps body in an AEAD envelope sealed for recipientNodeID, sets the
// Content-Type and timestamp headers, and returns the wire bytes and the
// binding for the answer.
func (a *AEADPeerAuth) Sign(req *http.Request, recipientNodeID string, body []byte) ([]byte, ResponseBinding, error) {
	if recipientNodeID == "" {
		return nil, ResponseBinding{}, ErrRecipientRequired
	}
	ts := strconv.FormatInt(a.clockFn().Unix(), 10)

	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ResponseBinding{}, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ct := a.gcm.Seal(nil, nonce, body, buildRequestAD(req.Method, req.URL.Path, ts, recipientNodeID))
	wire := make([]byte, 0, len(nonce)+len(ct))
	wire = append(wire, nonce...)
	wire = append(wire, ct...)

	req.Header.Set("Content-Type", DispatchContentType)
	req.Header.Set(DispatchTimestampHdr, ts)
	return wire, ResponseBinding{recipient: recipientNodeID, path: req.URL.Path, nonce: nonce, ts: ts}, nil
}

// Verify validates the request's timestamp skew, AEAD envelope, and nonce
// freshness. On success it returns the decrypted plaintext, a PeerIdentity
// describing the authenticated peer, and the binding for the answer.
func (a *AEADPeerAuth) Verify(r *http.Request) ([]byte, PeerIdentity, ResponseBinding, error) {
	none := ResponseBinding{}
	tsStr := r.Header.Get(DispatchTimestampHdr)
	if tsStr == "" {
		return nil, PeerIdentity{}, none, errors.New("missing X-Dispatch-Timestamp header")
	}
	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return nil, PeerIdentity{}, none, fmt.Errorf("malformed timestamp: %w", err)
	}
	tsTime := time.Unix(tsUnix, 0)

	// Skew check happens first — cheap rejection of stale/future envelopes
	// before we touch the body.
	diff := a.clockFn().Sub(tsTime)
	if diff < 0 {
		diff = -diff
	}
	if diff > a.skew {
		return nil, PeerIdentity{}, none, fmt.Errorf("timestamp outside skew window: %v > %v", diff, a.skew)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxEnvelopeSize))
	if err != nil {
		return nil, PeerIdentity{}, none, fmt.Errorf("failed to read body: %w", err)
	}
	nonceSize := a.gcm.NonceSize()
	if len(body) < nonceSize+a.gcm.Overhead() {
		return nil, PeerIdentity{}, none, errors.New("body too short for AEAD envelope")
	}
	// The binding outlives body, so it keeps a copy of the nonce rather than a
	// slice of the buffer the answer is sealed from.
	nonce := append([]byte(nil), body[:nonceSize]...)

	// The associated data names THIS node as the recipient. A hand-over sealed
	// for another node — a capture delivered here by an attacker on the path
	// between nodes — fails to open, although this node holds the same key.
	pt, err := a.gcm.Open(nil, nonce, body[nonceSize:], buildRequestAD(r.Method, r.URL.Path, tsStr, a.selfNodeID))
	if err != nil {
		return nil, PeerIdentity{}, none, fmt.Errorf("AEAD open failed: %w", err)
	}
	identity := PeerIdentity{authMethod: authMethodAEADv1}
	binding := ResponseBinding{recipient: a.selfNodeID, path: r.URL.Path, nonce: nonce, ts: tsStr}

	// Record the nonce only after successful decrypt. A flood of bogus
	// nonces that fail AEAD.Open never enters the cache. From here on the
	// sender is known to hold the key, so the cache being full can be answered
	// under seal — but a replay cannot: see ErrNonceReplayed.
	switch a.nonces.checkAndRecord(nonce, tsTime) {
	case nonceDuplicate:
		return nil, PeerIdentity{}, none, ErrNonceReplayed
	case nonceCacheFull:
		return nil, identity, binding, ErrReplayCacheFull
	}
	return pt, identity, binding, nil
}

// SealResponse wraps an answer under a nonce of its own — the request's nonce
// must never be used twice under the one key — and binds it to the request.
func (a *AEADPeerAuth) SealResponse(h http.Header, binding ResponseBinding, body []byte) ([]byte, error) {
	if len(binding.nonce) == 0 {
		return nil, errors.New("no request to bind the answer to")
	}
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}
	ct := a.gcm.Seal(nil, nonce, body, buildResponseAD(binding))
	wire := make([]byte, 0, len(nonce)+len(ct))
	wire = append(wire, nonce...)
	wire = append(wire, ct...)
	h.Set("Content-Type", DispatchContentType)
	return wire, nil
}

// OpenResponse opens an answer sealed for the request binding names.
func (a *AEADPeerAuth) OpenResponse(h http.Header, binding ResponseBinding, wireBody []byte) ([]byte, error) {
	if len(binding.nonce) == 0 {
		return nil, errors.New("no request the answer could be bound to")
	}
	if ct := h.Get("Content-Type"); ct != DispatchContentType {
		return nil, fmt.Errorf("answer is not a dispatch envelope: Content-Type %q", ct)
	}
	nonceSize := a.gcm.NonceSize()
	if len(wireBody) < nonceSize+a.gcm.Overhead() {
		return nil, errors.New("answer too short for AEAD envelope")
	}
	pt, err := a.gcm.Open(nil, wireBody[:nonceSize], wireBody[nonceSize:], buildResponseAD(binding))
	if err != nil {
		return nil, fmt.Errorf("AEAD open failed: %w", err)
	}
	return pt, nil
}

// buildRequestAD is the associated data of a request. Binding the direction,
// method, path and timestamp to the ciphertext prevents reflection,
// cross-endpoint and timestamp-strip replays without an outer signature; the
// recipient's node id binds it to the one node of the cluster it was sent to.
// The recipient comes last, so that a node id carrying the separator cannot
// shift the meaning of any field before it.
func buildRequestAD(method, path, ts, recipient string) []byte {
	return []byte(directionRequest + "\n" + method + "\n" + path + "\n" + ts + "\n" + recipient)
}

// buildResponseAD is the associated data of an answer: the other direction
// label, the path, the timestamp of the request it answers, the node that
// answers it, and that request's nonce. The nonce comes last because it is
// binary and of fixed length, which is also what keeps the recipient before it
// unambiguous.
func buildResponseAD(b ResponseBinding) []byte {
	ad := []byte(directionResponse + "\n" + b.path + "\n" + b.ts + "\n" + b.recipient + "\n")
	return append(ad, b.nonce...)
}
