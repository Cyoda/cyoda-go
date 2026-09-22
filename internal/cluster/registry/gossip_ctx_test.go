package registry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
)

// TestGossipRegistry_RegisterHonorsContextDeadline is the regression test for
// Register honouring its caller's deadline. Previously Register hardcoded a
// 2-minute retry deadline and ignored the context entirely, so operators who
// set CYODA_STARTUP_TIMEOUT could still hang for up to 2 minutes when seeds
// were unreachable.
//
// Register must abort with a context error once the caller's deadline elapses.
func TestGossipRegistry_RegisterHonorsContextDeadline(t *testing.T) {
	// 127.0.0.1:1 is a privileged port nothing is listening on — Join will
	// fail with ECONNREFUSED on every attempt.
	r, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "ctx-deadline-node",
		NodeAddr:        "127.0.0.1:28000",
		BindAddr:        "127.0.0.1",
		BindPort:        0,
		Seeds:           []string{"127.0.0.1:1"},
		StabilityWindow: 0,
	})
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	defer r.Deregister(context.Background(), "ctx-deadline-node")

	const deadline = 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	err = r.Register(ctx, "ctx-deadline-node", "127.0.0.1:28000")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Register: expected error, got nil after %v", elapsed)
	}
	// Allow one extra backoff cycle for the in-flight attempt to unwind.
	if elapsed > deadline+2*time.Second {
		t.Errorf("Register took %v; expected to return within ~%v of ctx deadline — does it still ignore the context?", elapsed, deadline)
	}
	// The error must reflect the context cancellation, not some unrelated
	// memberlist failure.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Register err = %v; want wrapped context.DeadlineExceeded", err)
	}
}

// TestGossipRegistry_RegisterHonorsCanceledContext verifies that a context
// canceled before or during Register causes the call to return promptly
// with context.Canceled.
func TestGossipRegistry_RegisterHonorsCanceledContext(t *testing.T) {
	r, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          "ctx-cancel-node",
		NodeAddr:        "127.0.0.1:28010",
		BindAddr:        "127.0.0.1",
		BindPort:        0,
		Seeds:           []string{"127.0.0.1:1"},
		StabilityWindow: 0,
	})
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	defer r.Deregister(context.Background(), "ctx-cancel-node")

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after Register begins so we can observe the abort path.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err = r.Register(ctx, "ctx-cancel-node", "127.0.0.1:28010")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Register: expected error, got nil after %v", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Register took %v after cancel; expected prompt return", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Register err = %v; want wrapped context.Canceled", err)
	}
}
