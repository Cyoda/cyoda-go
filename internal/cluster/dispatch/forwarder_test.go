package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

func makeProcessorReq() dispatch.DispatchCalloutRequest {
	processor := spi.ProcessorDefinition{
		Type: "HTTP",
		Name: "calc",
	}
	return dispatch.DispatchCalloutRequest{
		Kind:           "processor",
		Entity:         json.RawMessage(`{"amount":100}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-1", TenantID: "t1"},
		Processor:      &processor,
		WorkflowName:   "wf",
		TransitionName: "run",
		TxID:           "tx-1",
		TenantID:       "t1",
	}
}

func makeCriteriaReq() dispatch.DispatchCalloutRequest {
	return dispatch.DispatchCalloutRequest{
		Kind:           "criteria",
		Entity:         json.RawMessage(`{"status":"pending"}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-2", TenantID: "t2"},
		Criterion:      json.RawMessage(`{"type":"ALWAYS_TRUE"}`),
		Target:         "TRANSITION",
		WorkflowName:   "wf",
		TransitionName: "approve",
		TxID:           "tx-2",
		TenantID:       "t2",
	}
}

// verifyAEADHeaders confirms the forwarder set the expected AEAD envelope
// headers. Replaces the pre-AEAD verifyHMAC helper.
func verifyAEADHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Content-Type"); got != dispatch.DispatchContentType {
		t.Errorf("Content-Type = %q, want %q", got, dispatch.DispatchContentType)
	}
	if r.Header.Get(dispatch.DispatchTimestampHdr) == "" {
		t.Errorf("%s header missing", dispatch.DispatchTimestampHdr)
	}
	if r.Header.Get("X-Dispatch-HMAC") != "" {
		t.Errorf("legacy X-Dispatch-HMAC header still set; should have been removed with AEAD migration")
	}
}

// sealingHandler verifies the request and seals whatever answer returns, as
// the real handler does.
func sealingHandler(t *testing.T, auth dispatch.PeerAuth, answer func(r *http.Request, plain []byte) any) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain, _, binding, err := auth.Verify(r)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		out, err := json.Marshal(answer(r, plain))
		if err != nil {
			t.Errorf("marshal answer: %v", err)
			return
		}
		wire, err := auth.SealResponse(w.Header(), binding, out)
		if err != nil {
			t.Errorf("SealResponse: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wire)
	})
}

// sealingPeer is a running peer built from sealingHandler.
func sealingPeer(t *testing.T, auth dispatch.PeerAuth, answer func(r *http.Request, plain []byte) any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(sealingHandler(t, auth, answer))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPForwarder_ProcessorSuccess(t *testing.T) {
	wantResp := dispatch.DispatchCalloutResponse{
		EntityData: []byte(`{"amount":200}`),
		Success:    true,
		Warnings:   []string{"adjusted"},
	}

	srv := sealingPeer(t, newTestPeerAuth(t), func(r *http.Request, plain []byte) any {
		if r.URL.Path != "/internal/dispatch/callout" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		verifyAEADHeaders(t, r)
		return wantResp
	})

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	resp, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq())
	if err != nil {
		t.Fatalf("ForwardCallout: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false, want true")
	}
	if string(resp.EntityData) != `{"amount":200}` {
		t.Errorf("EntityData = %s, want {\"amount\":200}", resp.EntityData)
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0] != "adjusted" {
		t.Errorf("Warnings = %v, want [adjusted]", resp.Warnings)
	}
}

func TestHTTPForwarder_CriteriaSuccess(t *testing.T) {
	matches := true
	wantResp := dispatch.DispatchCalloutResponse{
		Matches: &matches,
		Success: true,
	}

	srv := sealingPeer(t, newTestPeerAuth(t), func(r *http.Request, plain []byte) any {
		if r.URL.Path != "/internal/dispatch/callout" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		verifyAEADHeaders(t, r)
		return wantResp
	})

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	resp, err := f.ForwardCallout(context.Background(), srv.URL, makeCriteriaReq())
	if err != nil {
		t.Fatalf("ForwardCallout: %v", err)
	}
	if resp.Matches == nil || !*resp.Matches {
		t.Errorf("Matches = %v, want true", resp.Matches)
	}
	if !resp.Success {
		t.Errorf("Success = false, want true")
	}
}

func TestHTTPForwarder_PeerUnreachable(t *testing.T) {
	// localhost:1 is guaranteed unreachable (privileged port, never listening)
	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 2*time.Second).AllowLoopbackForTesting()

	_, err := f.ForwardCallout(context.Background(), "http://localhost:1", makeProcessorReq())
	if err == nil {
		t.Fatal("expected error for unreachable peer, got nil")
	}

	_, err = f.ForwardCallout(context.Background(), "http://localhost:1", makeCriteriaReq())
	if err == nil {
		t.Fatal("expected error for unreachable peer, got nil")
	}
}

// TestHTTPForwarder_WireBodyIsEncrypted proves the forwarder ships an AEAD
// envelope — not plaintext JSON. Replaces the pre-AEAD HMAC-signature test.
func TestHTTPForwarder_WireBodyIsEncrypted(t *testing.T) {
	var capturedBody []byte

	// The peer seals its answer, so the request body is read by Verify; the
	// wrapper captures the wire bytes and hands the body back unread.
	sealing := sealingHandler(t, newTestPeerAuth(t), func(r *http.Request, plain []byte) any {
		return dispatch.DispatchCalloutResponse{Success: true}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(capturedBody))
		sealing.ServeHTTP(w, r)
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err != nil {
		t.Fatalf("ForwardCallout: %v", err)
	}

	// Plaintext markers from makeProcessorReq() must not appear on the wire.
	wire := string(capturedBody)
	for _, marker := range []string{`"amount":100`, `"calc"`, `"wf"`, `"t1"`, `"tx-1"`} {
		if strings.Contains(wire, marker) {
			t.Errorf("wire body contains plaintext marker %q — payload is not encrypted", marker)
		}
	}
	// And it must be non-empty (sanity — proves we captured something).
	if len(capturedBody) == 0 {
		t.Fatal("no body captured")
	}
}

// TestHTTPForwarder_AddrWithoutScheme verifies that the forwarder handles
// addresses without http:// scheme (as produced by gossip NODE_ADDR like
// "cyoda-go-node-2:8123"). Regression test for unsupported protocol error.
func TestHTTPForwarder_AddrWithoutScheme(t *testing.T) {
	srv := sealingPeer(t, newTestPeerAuth(t), func(r *http.Request, plain []byte) any {
		return dispatch.DispatchCalloutResponse{Success: true}
	})

	// Strip the "http://" from the test server URL to simulate gossip NODE_ADDR
	addrWithoutScheme := srv.Listener.Addr().String() // e.g., "127.0.0.1:PORT"

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	resp, err := f.ForwardCallout(context.Background(), addrWithoutScheme, makeProcessorReq())
	if err != nil {
		t.Fatalf("ForwardCallout with schemeless addr should work: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

func TestHTTPForwarder_PeerReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	_, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq())
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestHTTPForwarder_PlaintextAnswerRefused(t *testing.T) {
	auth := newTestPeerAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, _, err := auth.Verify(r); err != nil {
			t.Errorf("Verify: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err == nil {
		t.Fatal("an answer that was not sealed was accepted")
	}
}

func TestHTTPForwarder_AnswerSealedForAnotherRequestRefused(t *testing.T) {
	auth := newTestPeerAuth(t)
	var mu sync.Mutex
	var firstWire []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		wire, err := auth.SealResponse(w.Header(), binding, []byte(`{"success":true}`))
		if err != nil {
			t.Errorf("SealResponse: %v", err)
			return
		}
		func() {
			mu.Lock()
			defer mu.Unlock()
			if firstWire == nil {
				firstWire = wire
			}
			wire = firstWire // every later request is answered with the first answer
		}()
		_, _ = w.Write(wire)
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err != nil {
		t.Fatalf("first hand-over: %v", err)
	}
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err == nil {
		t.Fatal("an answer replayed from an earlier request was accepted")
	}
}

func TestHTTPForwarder_TruncatedAnswerRefused(t *testing.T) {
	auth := newTestPeerAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		wire, _ := auth.SealResponse(w.Header(), binding, []byte(`{"success":true,"entityData":"AAAAAAAAAAAAAAAA"}`))
		_, _ = w.Write(wire[:len(wire)/2])
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	_, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq())
	if err == nil {
		t.Fatal("a truncated answer was accepted")
	}
	// A half-envelope is also not valid JSON, so the refusal is pinned to the
	// seal failing to open rather than to the decode that followed it before.
	if !strings.Contains(err.Error(), "open response") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}
