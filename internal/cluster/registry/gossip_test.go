package registry_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
)

func TestGossipRegistry_TwoNodes(t *testing.T) {
	ctx := context.Background()

	r1, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "node-1",
		NodeAddr:        "localhost:18080",
		BindAddr:        "127.0.0.1",
		BindPort:        17946,
		Seeds:           nil,
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip node-1: %v", err)
	}
	defer r1.Deregister(ctx, "node-1")

	if err := r1.Register(ctx, "node-1", "localhost:18080"); err != nil {
		t.Fatalf("Register node-1: %v", err)
	}

	r2, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "node-2",
		NodeAddr:        "localhost:18081",
		BindAddr:        "127.0.0.1",
		BindPort:        17947,
		Seeds:           []string{"127.0.0.1:17946"},
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip node-2: %v", err)
	}
	defer r2.Deregister(ctx, "node-2")

	if err := r2.Register(ctx, "node-2", "localhost:18081"); err != nil {
		t.Fatalf("Register node-2: %v", err)
	}

	time.Sleep(1 * time.Second)

	addr, alive, err := r1.Lookup(ctx, "node-2")
	if err != nil {
		t.Fatalf("Lookup node-2 from r1: %v", err)
	}
	if !alive {
		t.Error("expected node-2 alive from r1")
	}
	if addr != "localhost:18081" {
		t.Errorf("addr = %q, want %q", addr, "localhost:18081")
	}

	addr, alive, err = r2.Lookup(ctx, "node-1")
	if err != nil {
		t.Fatalf("Lookup node-1 from r2: %v", err)
	}
	if !alive {
		t.Error("expected node-1 alive from r2")
	}
	if addr != "localhost:18080" {
		t.Errorf("addr = %q, want %q", addr, "localhost:18080")
	}

	nodes, err := r1.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("len = %d, want 2", len(nodes))
		for _, n := range nodes {
			fmt.Printf("  node: %s addr: %s alive: %v\n", n.NodeID, n.Addr, n.Alive)
		}
	}
}

func TestGossipRegistry_TagPropagation(t *testing.T) {
	ctx := context.Background()

	r1, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "tag-node-1",
		NodeAddr:        "localhost:19080",
		BindAddr:        "127.0.0.1",
		BindPort:        19946,
		Seeds:           nil,
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip tag-node-1: %v", err)
	}
	defer r1.Deregister(ctx, "tag-node-1")

	if err := r1.Register(ctx, "tag-node-1", "localhost:19080"); err != nil {
		t.Fatalf("Register tag-node-1: %v", err)
	}

	r2, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "tag-node-2",
		NodeAddr:        "localhost:19081",
		BindAddr:        "127.0.0.1",
		BindPort:        19947,
		Seeds:           []string{"127.0.0.1:19946"},
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip tag-node-2: %v", err)
	}
	defer r2.Deregister(ctx, "tag-node-2")

	if err := r2.Register(ctx, "tag-node-2", "localhost:19081"); err != nil {
		t.Fatalf("Register tag-node-2: %v", err)
	}

	// Node 1 sets tags.
	tags := map[string][]string{"tenant-a": {"python", "ml"}}
	if err := r1.UpdateTags(tags); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	// Wait for gossip convergence — poll until node 2 sees node 1's tags.
	var node1Tags map[string][]string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err := r2.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, n := range nodes {
			if n.NodeID == "tag-node-1" && len(n.Tags) > 0 {
				node1Tags = n.Tags
				break
			}
		}
		if node1Tags != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if node1Tags == nil {
		t.Fatal("node 2 never saw node 1's tags")
	}

	got, ok := node1Tags["tenant-a"]
	if !ok {
		t.Fatalf("missing tenant-a in tags: %v", node1Tags)
	}
	wantSet := map[string]bool{"python": true, "ml": true}
	if len(got) != len(wantSet) {
		t.Fatalf("tenant-a tags = %v, want %v", got, wantSet)
	}
	for _, tag := range got {
		if !wantSet[tag] {
			t.Errorf("unexpected tag %q in tenant-a", tag)
		}
	}
}

func TestGossipRegistry_GRPCAddrPropagation(t *testing.T) {
	ctx := context.Background()

	r1, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "grpc-node-1",
		NodeAddr:        "http://localhost:21080",
		GRPCNodeAddr:    "localhost:21090",
		BindAddr:        "127.0.0.1",
		BindPort:        21946,
		Seeds:           nil,
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip grpc-node-1: %v", err)
	}
	defer r1.Deregister(ctx, "grpc-node-1")
	if err := r1.Register(ctx, "grpc-node-1", "http://localhost:21080"); err != nil {
		t.Fatalf("Register grpc-node-1: %v", err)
	}

	r2, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "grpc-node-2",
		NodeAddr:        "http://localhost:21081",
		GRPCNodeAddr:    "",
		BindAddr:        "127.0.0.1",
		BindPort:        21947,
		Seeds:           []string{"127.0.0.1:21946"},
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip grpc-node-2: %v", err)
	}
	defer r2.Deregister(ctx, "grpc-node-2")
	if err := r2.Register(ctx, "grpc-node-2", "http://localhost:21081"); err != nil {
		t.Fatalf("Register grpc-node-2: %v", err)
	}

	// Poll until r2 sees r1's gRPC addr via gossip.
	var grpcAddr string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		nodes, listErr := r2.List(ctx)
		if listErr != nil {
			t.Fatalf("List: %v", listErr)
		}
		for _, n := range nodes {
			if n.NodeID == "grpc-node-1" {
				grpcAddr = n.GRPCAddr
			}
		}
		if grpcAddr != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if grpcAddr != "localhost:21090" {
		t.Fatalf("expected gRPC addr %q, got %q", "localhost:21090", grpcAddr)
	}

	// r2 has no advertised gRPC addr; confirm it round-trips as empty.
	nodes, err := r1.List(ctx)
	if err != nil {
		t.Fatalf("r1.List: %v", err)
	}
	for _, n := range nodes {
		if n.NodeID == "grpc-node-2" && n.GRPCAddr != "" {
			t.Fatalf("expected empty gRPC addr for grpc-node-2, got %q", n.GRPCAddr)
		}
	}
}

func TestGossipRegistry_LookupUnknown(t *testing.T) {
	ctx := context.Background()

	r, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "node-1",
		NodeAddr:        "localhost:18082",
		BindAddr:        "127.0.0.1",
		BindPort:        17948,
		Seeds:           nil,
		StabilityWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	defer r.Deregister(ctx, "node-1")

	if err := r.Register(ctx, "node-1", "localhost:18082"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, alive, err := r.Lookup(ctx, "node-unknown")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if alive {
		t.Error("expected alive=false for unknown node")
	}
}
