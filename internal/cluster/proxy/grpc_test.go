package proxy_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestGRPCExtractToken_Present(t *testing.T) {
	md := metadata.Pairs(proxy.GRPCTxTokenKey, "some-token-value")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	tok := proxy.ExtractGRPCToken(ctx)
	if tok != "some-token-value" {
		t.Fatalf("expected 'some-token-value', got %q", tok)
	}
}

func TestGRPCExtractToken_Absent(t *testing.T) {
	ctx := context.Background()
	tok := proxy.ExtractGRPCToken(ctx)
	if tok != "" {
		t.Fatalf("expected empty string, got %q", tok)
	}
}

func TestGRPCResolveTarget_Self(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true})

	tok, err := signer.Issue(token.Claims{NodeID: "node-1", TxRef: "tx-123", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-123", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	addr, shouldProxy, err := proxy.ResolveTarget(context.Background(), signer, reg, "node-1", tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldProxy {
		t.Fatal("expected shouldProxy=false for self")
	}
	if addr != "" {
		t.Fatalf("expected empty addr for self, got %q", addr)
	}
}

func TestGRPCResolveTarget_EmptyToken(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry()

	addr, shouldProxy, err := proxy.ResolveTarget(context.Background(), signer, reg, "node-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldProxy {
		t.Fatal("expected shouldProxy=false for empty token")
	}
	if addr != "" {
		t.Fatalf("expected empty addr, got %q", addr)
	}
}

func TestGRPCResolveTarget_OtherNode(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: "http://localhost:8888", Alive: true},
	)

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-456", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-456", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	addr, shouldProxy, err := proxy.ResolveTarget(context.Background(), signer, reg, "node-1", tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shouldProxy {
		t.Fatal("expected shouldProxy=true for other node")
	}
	if addr != "http://localhost:8888" {
		t.Fatalf("expected 'http://localhost:8888', got %q", addr)
	}
}

func TestGRPCResolveTarget_DeadNode(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: "http://localhost:8888", Alive: false},
	)

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-789", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-789", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = proxy.ResolveTarget(context.Background(), signer, reg, "node-1", tok)
	if err == nil {
		t.Fatal("expected error for dead node")
	}
}
