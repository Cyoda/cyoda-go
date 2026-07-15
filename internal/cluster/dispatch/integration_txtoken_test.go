package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// capturingLocalDispatcher is a stub ExternalProcessingService that records
// the context it receives from the handler. A mutex guards capturedCtx so the
// race detector does not flag the cross-goroutine access between the HTTP
// handler goroutine and the test goroutine.
type capturingLocalDispatcher struct {
	mu          sync.Mutex
	capturedCtx context.Context
	resp        *spi.Entity
	matches     bool
}

func (d *capturingLocalDispatcher) DispatchProcessor(
	ctx context.Context,
	_ *spi.Entity,
	_ spi.ProcessorDefinition,
	_, _, _ string,
) (*spi.Entity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.capturedCtx = ctx
	return d.resp, nil
}

func (d *capturingLocalDispatcher) DispatchCriteria(
	ctx context.Context,
	_ *spi.Entity,
	_ json.RawMessage,
	_, _, _, _, _ string,
) (bool, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.capturedCtx = ctx
	return d.matches, "", nil
}

func (d *capturingLocalDispatcher) captured() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.capturedCtx
}

// TestIntegration_HandlerReinjectsTxToken_Processor asserts that the tx-token
// minted by the forwarding node (node-A) survives the HTTP hop and is
// re-injected into the context by DispatchHandler (handler.go line 52) before
// calling the peer's local dispatcher. The test would fail if the
// WithTxToken call in handleProcessor were removed.
func TestIntegration_HandlerReinjectsTxToken_Processor(t *testing.T) {
	signer, err := token.NewSigner(testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)

	// Node B: local dispatcher captures the context for assertion.
	nodeBLocal := &capturingLocalDispatcher{
		resp: &spi.Entity{
			Meta: testEntity().Meta,
			Data: []byte(`{"result":"from-peer-processor"}`),
		},
	}
	handler := NewDispatchHandler(nodeBLocal, auth)
	mux := http.NewServeMux()
	handler.Register(mux)
	nodeBServer := httptest.NewServer(mux)
	defer nodeBServer.Close()

	// Node A: local dispatcher has no matching member — forces forward to peer.
	nodeALocal := &stubDispatcher{noMember: true}
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "node-A", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{}},
			{NodeID: "node-B", Addr: nodeBServer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()
	d := NewClusterDispatcher(nodeALocal, registry, "node-A", selector, forwarder, 2*time.Second, signer, time.Minute)

	const txID = "tx-token-integration-proc"
	result, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", txID)
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
	if string(result.Data) != `{"result":"from-peer-processor"}` {
		t.Fatalf("unexpected result data: %s", result.Data)
	}

	capturedCtx := nodeBLocal.captured()
	if capturedCtx == nil {
		t.Fatal("capturedCtx is nil — handler did not call local dispatcher")
	}

	tok := internalgrpc.TxTokenFromContext(capturedCtx)
	if tok == "" {
		t.Fatal("tx-token not present in ctx handed to peer local dispatcher (internalgrpc.WithTxToken re-injection missing or token not forwarded)")
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("token Verify failed: %v", err)
	}
	if claims.NodeID != "node-A" {
		t.Errorf("token NodeID = %q, want %q", claims.NodeID, "node-A")
	}
	if claims.TxRef != txID {
		t.Errorf("token TxRef = %q, want %q", claims.TxRef, txID)
	}
}

// TestIntegration_HandlerReinjectsTxToken_Criteria asserts that the tx-token
// minted by the forwarding node (node-A) survives the HTTP hop and is
// re-injected into the context by DispatchHandler (handler.go line 89) before
// calling the peer's local dispatcher. The test would fail if the
// WithTxToken call in handleCriteria were removed.
func TestIntegration_HandlerReinjectsTxToken_Criteria(t *testing.T) {
	signer, err := token.NewSigner(testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)

	// Node B: local dispatcher captures the context for assertion.
	nodeBLocal := &capturingLocalDispatcher{matches: true}
	handler := NewDispatchHandler(nodeBLocal, auth)
	mux := http.NewServeMux()
	handler.Register(mux)
	nodeBServer := httptest.NewServer(mux)
	defer nodeBServer.Close()

	// Node A: local dispatcher has no matching member — forces forward to peer.
	nodeALocal := &stubDispatcher{noMember: true}
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "node-A", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{}},
			{NodeID: "node-B", Addr: nodeBServer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()
	d := NewClusterDispatcher(nodeALocal, registry, "node-A", selector, forwarder, 2*time.Second, signer, time.Minute)

	const txID = "tx-token-integration-crit"
	matches, _, err := d.DispatchCriteria(testContext(), testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", txID)
	if err != nil {
		t.Fatalf("DispatchCriteria: %v", err)
	}
	if !matches {
		t.Fatal("expected matches=true from peer")
	}

	capturedCtx := nodeBLocal.captured()
	if capturedCtx == nil {
		t.Fatal("capturedCtx is nil — handler did not call local dispatcher")
	}

	tok := internalgrpc.TxTokenFromContext(capturedCtx)
	if tok == "" {
		t.Fatal("tx-token not present in ctx handed to peer local dispatcher (internalgrpc.WithTxToken re-injection missing or token not forwarded)")
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("token Verify failed: %v", err)
	}
	if claims.NodeID != "node-A" {
		t.Errorf("token NodeID = %q, want %q", claims.NodeID, "node-A")
	}
	if claims.TxRef != txID {
		t.Errorf("token TxRef = %q, want %q", claims.TxRef, txID)
	}
}
