package app_test

import (
	"net"
	"testing"
	"time"

	"github.com/cyoda/cyoda-go/app"
)

// TestApp_Close_GRPCGracefulStopWithDeadline verifies the gRPC drain
// behaviour. The test serves the gRPC server on a
// local listener, then invokes Close() and asserts:
//
//  1. Close returns within the 10-second drain budget (no hang on a stuck
//     stream).
//  2. The server is no longer accepting connections after Close returns.
//
// The "deadline exceeded → hard-stop" branch is exercised by inspection
// of the slog "gRPC graceful stop deadline exceeded" line in production;
// reproducing it deterministically requires a hung in-flight stream
// fixture which is out of scope for this layer.
func TestApp_Close_GRPCGracefulStopWithDeadline(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	addr := lis.Addr().String()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- a.GRPCServer().Serve(lis)
	}()

	// Verify the listener is accepting connections before we close.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gRPC server did not start accepting connections: %v", dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	closeDone := make(chan error, 1)
	closeStart := time.Now()
	go func() {
		closeDone <- a.Close()
	}()

	select {
	case <-closeDone:
		// Close should complete well under the 10s drain budget when there
		// are no in-flight RPCs. Anything over 5s suggests Close is
		// blocking on something other than graceful drain.
		if d := time.Since(closeStart); d > 5*time.Second {
			t.Errorf("Close took %v; expected sub-5s for an idle gRPC server", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not return within 15s — graceful-stop deadline not enforced")
	}

	// Serve should have returned by now (the server is stopped).
	select {
	case <-serveErr:
		// expected — Serve returns when the server is stopped.
	case <-time.After(2 * time.Second):
		t.Error("gRPC Serve did not return after Close")
	}
}
