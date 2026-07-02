package grpc

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestResolveGRPCAddr_AdvertisedWins(t *testing.T) {
	ni := contract.NodeInfo{
		NodeID:   "node-b",
		Addr:     "http://node-b:8080",
		GRPCAddr: "node-b:9099",
	}
	got, err := resolveGRPCAddr(ni, 9090)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "node-b:9099" {
		t.Fatalf("expected advertised addr %q, got %q", "node-b:9099", got)
	}
}

func TestResolveGRPCAddr_DeriveWhenEmpty(t *testing.T) {
	ni := contract.NodeInfo{
		NodeID: "node-b",
		Addr:   "http://node-b:8080",
	}
	got, err := resolveGRPCAddr(ni, 9090)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "node-b:9090" {
		t.Fatalf("expected derived addr %q, got %q", "node-b:9090", got)
	}
}

func TestResolveGRPCAddr_DeriveStripsSchemeAndPort(t *testing.T) {
	// Verify derive strips the HTTP scheme and replaces the port with the gRPC port.
	ni := contract.NodeInfo{
		NodeID: "node-c",
		Addr:   "https://node-c.internal:443",
	}
	got, err := resolveGRPCAddr(ni, 8443)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "node-c.internal:8443" {
		t.Fatalf("expected %q, got %q", "node-c.internal:8443", got)
	}
}

func TestResolveGRPCAddr_UnparseableAddr_Error(t *testing.T) {
	// An unparseable HTTP addr should return an error, not panic.
	ni := contract.NodeInfo{
		NodeID: "node-bad",
		Addr:   "://bad-url",
	}
	_, err := resolveGRPCAddr(ni, 9090)
	if err == nil {
		t.Fatal("expected error for unparseable addr")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got: %v", err)
	}
}
