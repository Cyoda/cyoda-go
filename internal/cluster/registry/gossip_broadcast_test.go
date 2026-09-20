package registry_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
)

// TestGossipBroadcaster_TwoNodeRoundTrip verifies that Broadcast from one node
// reaches a Subscribe'd handler on another node over real memberlist gossip.
// It's eventually-consistent, so we poll with a timeout rather than asserting
// single-shot delivery.
func TestGossipBroadcaster_TwoNodeRoundTrip(t *testing.T) {
	ctx := context.Background()

	r1, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "bcast-1",
		NodeAddr:        "localhost:18180",
		BindAddr:        "127.0.0.1",
		BindPort:        18046,
		StabilityWindow: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip bcast-1: %v", err)
	}
	defer r1.Deregister(ctx, "bcast-1")
	if err := r1.Register(ctx, "bcast-1", "localhost:18180"); err != nil {
		t.Fatalf("Register bcast-1: %v", err)
	}

	r2, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "bcast-2",
		NodeAddr:        "localhost:18181",
		BindAddr:        "127.0.0.1",
		BindPort:        18047,
		Seeds:           []string{"127.0.0.1:18046"},
		StabilityWindow: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip bcast-2: %v", err)
	}
	defer r2.Deregister(ctx, "bcast-2")
	if err := r2.Register(ctx, "bcast-2", "localhost:18181"); err != nil {
		t.Fatalf("Register bcast-2: %v", err)
	}

	// Wait for cluster to stabilize before broadcasting.
	time.Sleep(500 * time.Millisecond)

	var (
		mu       sync.Mutex
		received [][]byte
	)
	r2.Subscribe("test.topic", func(payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		// Copy; the payload slice is owned by the receive goroutine.
		cp := make([]byte, len(payload))
		copy(cp, payload)
		received = append(received, cp)
	})

	// Broadcast from node 1; node 2 should see it eventually.
	r1.Broadcast("test.topic", []byte("hello-world"))
	// Broadcast on an unrelated topic to verify topic isolation.
	r1.Broadcast("other.topic", []byte("should-not-deliver"))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(received)
		}()
		if n >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("no broadcasts received within deadline")
	}
	found := false
	for _, msg := range received {
		if string(msg) == "hello-world" {
			found = true
		}
		if string(msg) == "should-not-deliver" {
			t.Errorf("received message from unsubscribed topic: %q", msg)
		}
	}
	if !found {
		t.Errorf("expected hello-world in received, got %q", received)
	}
}

// TestGossipBroadcaster_SingleNodeSelfDoesNotEcho verifies that Broadcast on a
// single-node cluster does not loop back to the sender. This matches
// memberlist semantics: broadcasts go to *peers*, not self.
func TestGossipBroadcaster_SingleNodeSelfDoesNotEcho(t *testing.T) {
	ctx := context.Background()

	r, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "solo",
		NodeAddr:        "localhost:18190",
		BindAddr:        "127.0.0.1",
		BindPort:        18048,
		StabilityWindow: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip solo: %v", err)
	}
	defer r.Deregister(ctx, "solo")
	if err := r.Register(ctx, "solo", "localhost:18190"); err != nil {
		t.Fatalf("Register solo: %v", err)
	}

	var got int32
	r.Subscribe("echo.test", func(_ []byte) {
		got++
	})
	r.Broadcast("echo.test", []byte("ignored"))
	time.Sleep(500 * time.Millisecond)
	if got != 0 {
		t.Errorf("expected 0 self-deliveries, got %d", got)
	}
}
