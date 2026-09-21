package dispatch

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const handOverPath = "/internal/dispatch/callout"

// newBoundRequest signs body as the owner would and returns the request the peer
// receives, the bytes that went on the wire, and the binding the owner keeps.
func newBoundRequest(t *testing.T, a *AEADPeerAuth, path string, body []byte) (*http.Request, []byte, ResponseBinding) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	wire, binding, err := a.Sign(req, testSelfNodeID, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req, wire, binding
}

func TestAEADResponse_RoundTrip(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	req, _, ownerBinding := newBoundRequest(t, owner, handOverPath, []byte(`{"q":1}`))

	_, _, peerBinding, err := peer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	h := http.Header{}
	wire, err := peer.SealResponse(h, peerBinding, []byte(`{"a":2}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	if got := h.Get("Content-Type"); got != DispatchContentType {
		t.Errorf("Content-Type = %q, want %q", got, DispatchContentType)
	}
	if bytes.Contains(wire, []byte(`"a"`)) {
		t.Error("the answer is on the wire in the clear")
	}
	got, err := owner.OpenResponse(h, ownerBinding, wire)
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	if string(got) != `{"a":2}` {
		t.Errorf("opened %q", got)
	}
}

func TestAEADResponse_Refused(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	stranger, err := NewAEADPeerAuth(bytes.Repeat([]byte{0xCD}, 32), testSelfNodeID, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	reqA, wireA, bindingA := newBoundRequest(t, owner, handOverPath, []byte(`{"q":"A"}`))
	_, _, peerBindingA, err := peer.Verify(reqA)
	if err != nil {
		t.Fatalf("Verify A: %v", err)
	}
	_, _, bindingB := newBoundRequest(t, owner, handOverPath, []byte(`{"q":"B"}`))

	sealed := func(a *AEADPeerAuth, b ResponseBinding) (http.Header, []byte) {
		h := http.Header{}
		wire, err := a.SealResponse(h, b, []byte(`{"outcome":"no_handoff"}`))
		if err != nil {
			t.Fatalf("SealResponse: %v", err)
		}
		return h, wire
	}
	goodHeader, good := sealed(peer, peerBindingA)
	tampered := append([]byte(nil), good...)
	tampered[len(tampered)-1] ^= 0x01
	_, forged := sealed(stranger, peerBindingA)

	tests := []struct {
		name    string
		header  http.Header
		binding ResponseBinding
		wire    []byte
	}{
		{"forged with another key", goodHeader, bindingA, forged},
		{"tampered", goodHeader, bindingA, tampered},
		{"the request reflected back as its own answer", goodHeader, bindingA, wireA},
		{"replayed onto another request", goodHeader, bindingB, good},
		{"truncated", goodHeader, bindingA, good[:len(good)/2]},
		{"truncated above the envelope minimum, so the auth tag fails", goodHeader, bindingA, good[:len(good)-5]},
		{"shorter than an envelope", goodHeader, bindingA, []byte{1, 2, 3}},
		{"not marked as a dispatch envelope", http.Header{"Content-Type": {"application/json"}}, bindingA, good},
		{"opened without a binding", goodHeader, ResponseBinding{}, good},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := owner.OpenResponse(tt.header, tt.binding, tt.wire); err == nil {
				t.Fatal("OpenResponse accepted it")
			}
		})
	}
	// and the good one still opens — the table above is not vacuous
	if _, err := owner.OpenResponse(goodHeader, bindingA, good); err != nil {
		t.Fatalf("the genuine answer must open: %v", err)
	}
}

func TestAEADResponse_CannotBePresentedAsARequest(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	req, _, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	ts := req.Header.Get(DispatchTimestampHdr)
	_, _, binding, err := peer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wire, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	reflected := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	reflected.Header.Set("Content-Type", DispatchContentType)
	reflected.Header.Set(DispatchTimestampHdr, ts)
	if _, _, _, err := owner.Verify(reflected); err == nil {
		t.Fatal("an answer was accepted as a request")
	}
}

func TestAEADResponse_NonceIsFreshEveryTime(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	req, reqWire, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	_, _, binding, err := peer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	first, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	second, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	const nonceSize = 12
	if bytes.Equal(first[:nonceSize], reqWire[:nonceSize]) {
		t.Error("the answer reuses the request's nonce under the same key")
	}
	if bytes.Equal(first[:nonceSize], second[:nonceSize]) {
		t.Error("two answers share a nonce")
	}
}

func TestAEADResponse_SealNeedsABinding(t *testing.T) {
	if _, err := newAEAD(t).SealResponse(http.Header{}, ResponseBinding{}, []byte(`{}`)); err == nil {
		t.Fatal("sealed an answer bound to no request")
	}
}

// A request in the format of before the direction label no longer verifies.
func TestAEADRequest_WithoutDirectionLabelRefused(t *testing.T) {
	a := newAEAD(t)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	unlabelled := []byte(http.MethodPost + "\n" + handOverPath + "\n" + ts)
	wire := append(append([]byte(nil), nonce...), a.gcm.Seal(nil, nonce, []byte(`{}`), unlabelled)...)

	req := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	req.Header.Set(DispatchTimestampHdr, ts)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("a request without the direction label verified")
	}
}

// A replayed nonce yields NO binding. There is no verified request to bind a
// sealed answer to a replay of: an answer bound to the captured request's nonce
// is indistinguishable from the genuine one, so an attacker who captures a
// request, lets it run and replays it could deliver "nothing was handed over"
// in place of the real answer. The refusal is a bare status.
func TestAEADVerify_ReplayedNonceYieldsNoBinding(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	first, wire, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	if _, _, _, err := peer.Verify(first); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	again := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	again.Header.Set(DispatchTimestampHdr, first.Header.Get(DispatchTimestampHdr))

	body, identity, binding, err := peer.Verify(again)
	if !errors.Is(err, ErrNonceReplayed) {
		t.Fatalf("err = %v, want ErrNonceReplayed", err)
	}
	if errors.Is(err, ErrReplayCacheFull) {
		t.Error("a replayed nonce must not be reported as a full cache")
	}
	if body != nil {
		t.Error("a refused request must not yield its body")
	}
	if identity != (PeerIdentity{}) {
		t.Errorf("identity = %+v, want the zero value", identity)
	}
	if _, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`)); err == nil {
		t.Error("a binding came back for a replayed nonce")
	}
}

// A full cache is this node failing closed on a request it authenticated:
// nothing ran, only a holder of the key could have filled the cache, and the
// owner is told so under seal rather than left with a lost answer.
func TestAEADVerify_FullReplayCacheIsRefusedWithAUsableBinding(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	peer.nonces = newNonceCache(time.Minute, 1, time.Now)

	one, _, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	if _, _, _, err := peer.Verify(one); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	two, _, ownerBinding := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	_, _, binding, err := peer.Verify(two)
	if !errors.Is(err, ErrReplayCacheFull) {
		t.Fatalf("err = %v, want ErrReplayCacheFull", err)
	}
	h := http.Header{}
	sealed, err := peer.SealResponse(h, binding, []byte(`{"outcome":"no_handoff"}`))
	if err != nil {
		t.Fatalf("SealResponse under the refused request's binding: %v", err)
	}
	if _, err := owner.OpenResponse(h, ownerBinding, sealed); err != nil {
		t.Fatalf("the owner cannot open the sealed refusal: %v", err)
	}
}

// A request that does not authenticate yields no binding: there is nothing to
// seal an answer to, and the peer answers a bare 403.
func TestAEADVerify_UnauthenticatedYieldsNoBinding(t *testing.T) {
	a := newAEAD(t)
	req := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	req.Header.Set(DispatchTimestampHdr, strconv.FormatInt(time.Now().Unix(), 10))
	_, _, binding, err := a.Verify(req)
	if err == nil || errors.Is(err, ErrReplayCacheFull) {
		t.Fatalf("err = %v, want an authentication failure", err)
	}
	if _, err := a.SealResponse(http.Header{}, binding, []byte(`{}`)); err == nil {
		t.Error("a binding came back from a request that did not authenticate")
	}
}
