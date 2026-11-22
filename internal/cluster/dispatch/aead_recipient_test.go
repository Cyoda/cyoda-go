package dispatch

import (
	"bytes"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// A hand-over names the node it was sealed for. A capture replayed to any other
// node fails to open there: that node builds the associated data with its own
// id, which is not the one the owner sealed under. Without the binding the same
// bytes verify on every node holding the cluster key, and the answer that node
// seals — "nothing was handed over" — opens under the owner's binding as though
// it came from the node the owner asked.
func TestAEADRequest_SealedForOneNodeRefusedByAnother(t *testing.T) {
	owner, err := NewAEADPeerAuth(testSecret32, "node-owner", 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	peerA, err := NewAEADPeerAuth(testSecret32, "node-a", 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	peerB, err := NewAEADPeerAuth(testSecret32, "node-b", 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, handOverPath, nil)
	wire, _, err := owner.Sign(req, "node-a", []byte(`{"q":1}`))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ts := req.Header.Get(DispatchTimestampHdr)

	build := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
		r.Header.Set(DispatchTimestampHdr, ts)
		r.Header.Set("Content-Type", DispatchContentType)
		return r
	}

	if _, _, _, err := peerB.Verify(build()); err == nil {
		t.Fatal("a hand-over sealed for node-a verified on node-b")
	}
	// Not vacuous: the node it was sealed for opens it.
	if _, _, _, err := peerA.Verify(build()); err != nil {
		t.Fatalf("the node the hand-over was sealed for must open it: %v", err)
	}
}

// The other half: even where an attacker gets a second node to answer, that
// node's answer does not open under the binding the owner made for the node it
// asked. The recipient is in the answer's associated data too.
func TestAEADResponse_FromAnotherNodeDoesNotOpen(t *testing.T) {
	owner, err := NewAEADPeerAuth(testSecret32, "node-owner", 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	peerB, err := NewAEADPeerAuth(testSecret32, "node-b", 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	// The owner asks node-a; the capture is delivered to node-b, which — were
	// the request binding absent — would answer under its own binding.
	req := httptest.NewRequest(http.MethodPost, handOverPath, nil)
	_, ownerBinding, err := owner.Sign(req, "node-a", []byte(`{"q":1}`))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Node B's binding for the very same request bytes, differing only in the
	// recipient: this is what it would seal its answer under.
	bBinding := ResponseBinding{recipient: "node-b", path: ownerBinding.path, nonce: ownerBinding.nonce, ts: ownerBinding.ts}
	h := http.Header{}
	sealed, err := peerB.SealResponse(h, bBinding, []byte(`{"outcome":"no_handoff","triesUsed":0}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	if _, err := owner.OpenResponse(h, ownerBinding, sealed); err == nil {
		t.Fatal("an answer sealed by node-b opened under the binding made for node-a")
	}
}

// A hand-over with no recipient named is refused before anything leaves this
// node: an empty recipient would seal under an associated data no node builds,
// and the failure must be the sender's, not a lost answer.
func TestAEADSign_RefusesAnUnnamedRecipient(t *testing.T) {
	a := newAEAD(t)
	req := httptest.NewRequest(http.MethodPost, handOverPath, nil)
	if _, _, err := a.Sign(req, "", []byte(`{}`)); err == nil {
		t.Fatal("signed a hand-over that names no recipient")
	}
}

// A node with no id of its own cannot tell a hand-over sealed for it from one
// sealed for another node: the constructor refuses.
func TestNewAEADPeerAuth_RefusesAnUnnamedNode(t *testing.T) {
	if _, err := NewAEADPeerAuth(testSecret32, "", 30*time.Second); err == nil {
		t.Fatal("constructed a peer auth with no node id")
	}
}

// One wire form, not two: a request in the format of before the recipient was
// bound does not verify. There is no acceptance of the older associated data
// beside the current one.
func TestAEADRequest_WithoutTheRecipientRefused(t *testing.T) {
	a := newAEAD(t)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	withoutRecipient := []byte("request\n" + http.MethodPost + "\n" + handOverPath + "\n" + ts)
	wire := append(append([]byte(nil), nonce...), a.gcm.Seal(nil, nonce, []byte(`{}`), withoutRecipient)...)

	req := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	req.Header.Set(DispatchTimestampHdr, ts)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("a request sealed without a recipient verified")
	}
}
