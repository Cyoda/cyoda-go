package app

import (
	"net"
	"testing"
	"time"
)

// TestApp_StopGRPC_OnlyDrainsOnce pins the invariant that the gRPC
// graceful-stop dance runs at most once across the runServers + Close
// teardown sequence. Were runServers' drain watcher and App.Close each
// to inline their own GracefulStop + deadline budget, a stuck stream
// would burn up to 2× the budget across the two layers.
//
// The test serves a real gRPC server, calls StopGRPC twice, and asserts
// the second call returns immediately — observable proof that the
// sync.Once gate short-circuits the second drain.
func TestApp_StopGRPC_OnlyDrainsOnce(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContextPath = ""
	a := New(cfg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.GRPCServer().Serve(lis) }()

	// No wait for Serve: the drain is once-guarded whether it lands before or
	// after Serve registers the listener, and Serve's error is not inspected.

	// First drain — does the actual graceful-stop work.
	a.StopGRPC()

	// Second drain — must short-circuit. We give it a generous threshold
	// (50ms) so a slow CI box does not flake; the operation under test
	// is a sync.Once.Do that hits the already-done branch and returns.
	start := time.Now()
	a.StopGRPC()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("second StopGRPC took %v; expected near-instant short-circuit via sync.Once", d)
	}

	// Close should also skip the gRPC drain branch. Storage is the only
	// remaining work, and the default in-memory store closes instantly.
	start = time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Close after StopGRPC took %v; expected near-instant since gRPC drain already ran", d)
	}

	select {
	case <-serveErr:
		// expected
	case <-time.After(2 * time.Second):
		t.Error("gRPC Serve did not return after StopGRPC")
	}
}
