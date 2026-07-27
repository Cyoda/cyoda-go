package dispatch_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHTTPForwarder_ProcessorSuccess(t *testing.T) {
	wantResp := dispatch.DispatchCalloutResponse{
		EntityData: []byte(`{"amount":200}`),
		Success:    true,
		Warnings:   []string{"adjusted"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/dispatch/callout" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		verifyAEADHeaders(t, r)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantResp)
	}))
	defer srv.Close()

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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/dispatch/callout" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		verifyAEADHeaders(t, r)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantResp)
	}))
	defer srv.Close()

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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dispatch.DispatchCalloutResponse{Success: true})
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(dispatch.DispatchCalloutResponse{Success: true})
	}))
	defer srv.Close()

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
