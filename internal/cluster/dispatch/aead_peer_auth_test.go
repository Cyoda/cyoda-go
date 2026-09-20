package dispatch_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

// testSecret is a 32-byte shared secret used across AEAD tests. Not the
// real production key — purely for deterministic test construction.
var testSecret = bytes.Repeat([]byte{0xAB}, 32)

// aeadFixedClock returns a stable time function for tests that manipulate time.
func aeadFixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// signAndBuildRequest signs body and returns an http.Request ready to Verify.
// Mirrors the real forwarder.forward path without needing a server.
func signAndBuildRequest(t *testing.T, auth dispatch.PeerAuth, method, path string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	wire, _, err := auth.Sign(req, body)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	// Install the wire body as the request body (Sign doesn't attach it —
	// that's the caller's responsibility, matching forwarder.forward).
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req
}

func TestAEADPeerAuth_RoundTrip(t *testing.T) {
	a, err := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	plaintext := []byte(`{"hello":"world"}`)
	req := signAndBuildRequest(t, a, "POST", "/internal/dispatch/processor", plaintext)

	got, id, _, err := a.Verify(req)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, plaintext)
	}
	if id.AuthMethod() != "aead-v1" {
		t.Fatalf("identity auth method = %q, want aead-v1", id.AuthMethod())
	}
	if id.NodeID() != "" {
		t.Fatalf("shared-key identity should have empty NodeID, got %q", id.NodeID())
	}
}

func TestAEADPeerAuth_RejectsShortSecret(t *testing.T) {
	short := bytes.Repeat([]byte{0xAB}, 31)
	if _, err := dispatch.NewAEADPeerAuth(short, 30*time.Second); err == nil {
		t.Fatal("expected error for secret < 32 bytes")
	}
}

func TestAEADPeerAuth_TamperedCiphertextFails(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := signAndBuildRequest(t, a, "POST", "/internal/dispatch/processor", []byte(`{"a":1}`))

	wire, _ := io.ReadAll(req.Body)
	// Flip a byte in the ciphertext region (past the 12-byte nonce prefix).
	wire[len(wire)-1] ^= 0x01
	req.Body = io.NopCloser(bytes.NewReader(wire))

	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestAEADPeerAuth_TamperedNonceFails(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := signAndBuildRequest(t, a, "POST", "/internal/dispatch/processor", []byte(`{"a":1}`))
	wire, _ := io.ReadAll(req.Body)
	wire[0] ^= 0xFF
	req.Body = io.NopCloser(bytes.NewReader(wire))

	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("tampered nonce was accepted")
	}
}

func TestAEADPeerAuth_CrossEndpointReplayRejected(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	// Sign for processor, try to verify as criteria.
	req := signAndBuildRequest(t, a, "POST", "/internal/dispatch/processor", []byte(`{"a":1}`))
	wire, _ := io.ReadAll(req.Body)

	replay := httptest.NewRequest("POST", "/internal/dispatch/criteria", bytes.NewReader(wire))
	replay.Header.Set("X-Dispatch-Timestamp", req.Header.Get("X-Dispatch-Timestamp"))
	replay.Header.Set("Content-Type", req.Header.Get("Content-Type"))

	if _, _, _, err := a.Verify(replay); err == nil {
		t.Fatal("cross-endpoint replay (processor → criteria) was accepted")
	}
}

func TestAEADPeerAuth_MethodChangeRejected(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := signAndBuildRequest(t, a, "POST", "/internal/dispatch/processor", []byte(`{"a":1}`))
	wire, _ := io.ReadAll(req.Body)

	replay := httptest.NewRequest("PUT", "/internal/dispatch/processor", bytes.NewReader(wire))
	replay.Header.Set("X-Dispatch-Timestamp", req.Header.Get("X-Dispatch-Timestamp"))
	replay.Header.Set("Content-Type", req.Header.Get("Content-Type"))

	if _, _, _, err := a.Verify(replay); err == nil {
		t.Fatal("method change (POST → PUT) was accepted")
	}
}

func TestAEADPeerAuth_StaleTimestampRejected(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	a, _ := dispatch.NewAEADPeerAuthWithClockForTesting(testSecret, 30*time.Second, aeadFixedClock(base))

	// Sign at T.
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", nil)
	wire, _, err := a.Sign(req, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ts := req.Header.Get("X-Dispatch-Timestamp")

	// Advance clock past skew window (60s > 30s).
	a.SetClockForTesting(aeadFixedClock(base.Add(60 * time.Second)))

	// Verify at T+60s — outside 30s skew.
	req2 := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader(wire))
	req2.Header.Set("X-Dispatch-Timestamp", ts)
	req2.Header.Set("Content-Type", "application/cyoda-dispatch-v1")

	_, _, _, err = a.Verify(req2)
	if err == nil {
		t.Fatal("expected stale-timestamp rejection, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timestamp") {
		t.Fatalf("expected timestamp error, got: %v", err)
	}
}

func TestAEADPeerAuth_ReplayWithinWindowRejected(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)

	req := httptest.NewRequest("POST", "/internal/dispatch/processor", nil)
	wire, _, err := a.Sign(req, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ts := req.Header.Get("X-Dispatch-Timestamp")

	buildReq := func() *http.Request {
		r := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader(wire))
		r.Header.Set("X-Dispatch-Timestamp", ts)
		r.Header.Set("Content-Type", "application/cyoda-dispatch-v1")
		return r
	}

	if _, _, _, err := a.Verify(buildReq()); err != nil {
		t.Fatalf("first verify should succeed: %v", err)
	}
	if _, _, _, err := a.Verify(buildReq()); err == nil {
		t.Fatal("replay within window should be rejected, got nil error")
	}
}

func TestAEADPeerAuth_DifferentSecretsIncompatible(t *testing.T) {
	a1, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	otherSecret := bytes.Repeat([]byte{0xCD}, 32)
	a2, _ := dispatch.NewAEADPeerAuth(otherSecret, 30*time.Second)

	req := httptest.NewRequest("POST", "/internal/dispatch/processor", nil)
	wire, _, _ := a1.Sign(req, []byte(`{"a":1}`))
	ts := req.Header.Get("X-Dispatch-Timestamp")

	recv := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader(wire))
	recv.Header.Set("X-Dispatch-Timestamp", ts)
	recv.Header.Set("Content-Type", "application/cyoda-dispatch-v1")

	if _, _, _, err := a2.Verify(recv); err == nil {
		t.Fatal("a2 accepted an envelope sealed by a1 with a different secret")
	}
}

func TestAEADPeerAuth_DerivedKeyDiffersFromRawSecret(t *testing.T) {
	// Ensures HKDF is actually running. If the impl ever regresses to using
	// the raw secret directly, this test fails.
	derived := dispatch.DeriveDispatchKeyForTesting(testSecret)
	if bytes.Equal(derived, testSecret) {
		t.Fatal("derived dispatch key equals raw secret — HKDF not applied")
	}
	if len(derived) != 32 {
		t.Fatalf("derived key length = %d, want 32", len(derived))
	}
}

// TestAEADPeerAuth_SkewBoundaryReplayStillRejected pins the relationship
// between the skew window (30s) and the nonce-cache TTL (skew*2 = 60s) with
// observed=tsTime indexing: an envelope stamped at T and first verified
// anywhere within [T-skew, T+skew], then replayed anywhere else within the
// same window, MUST be rejected. Code review flagged a suspected eviction
// window where the cache would evict before skew could reject; this test
// locks in the invariant that that window does not exist.
func TestAEADPeerAuth_SkewBoundaryReplayStillRejected(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := base
	clockFn := func() time.Time { return clock }

	a, err := dispatch.NewAEADPeerAuthWithClockForTesting(testSecret, 30*time.Second, clockFn)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	// Sign at T.
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", nil)
	wire, _, err := a.Sign(req, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ts := req.Header.Get(dispatch.DispatchTimestampHdr)

	build := func() *http.Request {
		r := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader(wire))
		r.Header.Set(dispatch.DispatchTimestampHdr, ts)
		r.Header.Set("Content-Type", dispatch.DispatchContentType)
		return r
	}

	// First verify at T+25s (inside skew). Must succeed.
	clock = base.Add(25 * time.Second)
	if _, _, _, err := a.Verify(build()); err != nil {
		t.Fatalf("first verify at T+25s should succeed: %v", err)
	}

	// Replay at T+30s (skew boundary). Must be rejected as replay, not
	// accepted due to premature eviction.
	clock = base.Add(30 * time.Second)
	if _, _, _, err := a.Verify(build()); err == nil {
		t.Fatal("replay at skew boundary was accepted — nonce cache evicted too aggressively")
	}

	// Replay at T+31s (just past skew boundary). Must be rejected — now by
	// the skew check, not the nonce cache. Either rejection is correct;
	// the invariant is that it's NOT accepted.
	clock = base.Add(31 * time.Second)
	if _, _, _, err := a.Verify(build()); err == nil {
		t.Fatal("replay just past skew boundary was accepted")
	}
}

func TestAEADPeerAuth_ZeroTimestampRejected(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader([]byte{}))
	req.Header.Set(dispatch.DispatchTimestampHdr, "0")
	req.Header.Set("Content-Type", dispatch.DispatchContentType)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("ts=0 (1970-01-01) was accepted")
	}
}

func TestAEADPeerAuth_NegativeTimestampRejected(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader([]byte{}))
	req.Header.Set(dispatch.DispatchTimestampHdr, "-1")
	req.Header.Set("Content-Type", dispatch.DispatchContentType)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("negative ts was accepted")
	}
}

func TestAEADPeerAuth_FarFutureTimestampRejected(t *testing.T) {
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader([]byte{}))
	// Year 4000 or so.
	req.Header.Set(dispatch.DispatchTimestampHdr, "64060588800")
	req.Header.Set("Content-Type", dispatch.DispatchContentType)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("year-4000 ts was accepted")
	}
}

func TestAEADPeerAuth_EmptyPlaintextRoundTrips(t *testing.T) {
	// Body at the envelope minimum: empty plaintext seals to
	// nonceSize(12) + overhead(16) = 28 bytes on the wire. Confirms the
	// len() >= nonceSize + overhead guard accepts the boundary case.
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", nil)
	wire, _, err := a.Sign(req, []byte{})
	if err != nil {
		t.Fatalf("Sign empty body: %v", err)
	}
	if len(wire) != 12+16 {
		t.Fatalf("expected 28-byte envelope, got %d", len(wire))
	}

	req2 := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader(wire))
	req2.Header.Set(dispatch.DispatchTimestampHdr, req.Header.Get(dispatch.DispatchTimestampHdr))
	req2.Header.Set("Content-Type", dispatch.DispatchContentType)

	pt, _, _, err := a.Verify(req2)
	if err != nil {
		t.Fatalf("Verify at envelope minimum: %v", err)
	}
	if len(pt) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(pt))
	}
}

func TestAEADPeerAuth_ShortBodyRejected(t *testing.T) {
	// One byte below the envelope minimum must be rejected before any
	// decrypt attempt.
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	tooShort := make([]byte, 12+16-1)
	req := httptest.NewRequest("POST", "/internal/dispatch/processor", bytes.NewReader(tooShort))
	req.Header.Set(dispatch.DispatchTimestampHdr, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("Content-Type", dispatch.DispatchContentType)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("short body (below envelope min) was accepted")
	}
}

func TestAEADPeerAuth_WireBodyIsNotPlaintextJSON(t *testing.T) {
	// Confidentiality smoke test: the wire body does not contain the
	// plaintext JSON secret, proving the envelope encrypts rather than
	// merely authenticates.
	a, _ := dispatch.NewAEADPeerAuth(testSecret, 30*time.Second)
	secret := `{"secret_marker":"SENSITIVE_PAYLOAD_7f3a9b"}`

	req := httptest.NewRequest("POST", "/internal/dispatch/processor", nil)
	wire, _, err := a.Sign(req, []byte(secret))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if strings.Contains(string(wire), "SENSITIVE_PAYLOAD_7f3a9b") {
		t.Fatal("wire body contains plaintext marker — payload is not encrypted")
	}
	if strings.Contains(string(wire), "secret_marker") {
		t.Fatal("wire body contains plaintext JSON key — payload is not encrypted")
	}
}
