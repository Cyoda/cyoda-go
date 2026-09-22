package dispatch_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

// testSharedSecret is a 32-byte shared secret used across tests that need to
// construct a PeerAuth. Not the real production key.
var testSharedSecret = bytes.Repeat([]byte{0xAB}, 32)

// testNodeID is the node id these tests' PeerAuth answers to, and the one they
// seal their hand-overs for: one node standing in for both ends, as the tests
// that need two distinguishable nodes do not live here.
const testNodeID = "node-self"

// newTestPeerAuth builds a default PeerAuth (AEAD, 30s skew) from
// testSharedSecret, for testNodeID. Test helper; production uses
// NewAEADPeerAuth in app.go.
func newTestPeerAuth(t *testing.T) dispatch.PeerAuth {
	t.Helper()
	auth, err := dispatch.NewAEADPeerAuth(testSharedSecret, testNodeID, 30*time.Second)
	if err != nil {
		t.Fatalf("newTestPeerAuth: %v", err)
	}
	return auth
}
