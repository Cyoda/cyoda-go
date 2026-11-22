package dispatch

import "time"

// EncodePeerBody exposes the peer-body encoder to this package's external tests.
var EncodePeerBody = encodePeerBody

// MaxEnvelopeSize exposes the envelope ceiling to this package's external tests.
const MaxEnvelopeSize = maxEnvelopeSize

// NewPeerIdentityForTesting constructs a PeerIdentity with the given fields.
// Production PeerAuth implementations have their own internal constructors
// so the zero-value invariant stays meaningful.
func NewPeerIdentityForTesting(authMethod, nodeID string) PeerIdentity {
	return PeerIdentity{authMethod: authMethod, nodeID: nodeID}
}

// NewAEADPeerAuthWithClockForTesting is an AEADPeerAuth whose notion of "now"
// is controlled by the caller, for tests of timestamp skew and replay-TTL
// behaviour.
func NewAEADPeerAuthWithClockForTesting(sharedSecret []byte, selfNodeID string, skew time.Duration, clockFn func() time.Time) (*AEADPeerAuth, error) {
	return newAEADPeerAuth(sharedSecret, selfNodeID, skew, clockFn)
}

// SetClockForTesting swaps the clock function for both the AEAD and its
// nonce cache.
func (a *AEADPeerAuth) SetClockForTesting(clockFn func() time.Time) {
	a.clockFn = clockFn
	a.nonces.nowFn = clockFn
}

// DeriveDispatchKeyForTesting exposes the HKDF key derivation for the test
// that proves derivation actually runs.
func DeriveDispatchKeyForTesting(sharedSecret []byte) []byte {
	return deriveDispatchKey(sharedSecret)
}
