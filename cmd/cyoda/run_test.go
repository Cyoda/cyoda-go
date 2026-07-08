package main

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// freePort returns a TCP port likely to be unused at the moment of the
// call. The caller must accept that the port may race; in practice for
// short-lived tests this is reliable enough.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestRunServers_CtxCancelDrainsBothServers exercises the SIGTERM path
// (#26) without spawning a subprocess: we cancel the root context the
// way signal.NotifyContext would on SIGTERM, and assert runServers
// returns within the drain budget after stopping every listener.
func TestRunServers_CtxCancelDrainsBothServers(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.HTTPPort = freePort(t)
	cfg.Admin.BindAddress = "127.0.0.1"
	cfg.Admin.Port = freePort(t)
	a := app.New(cfg)

	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- runServers(rootCtx, a, cfg, grpcLis)
	}()

	// Give the listeners a moment to come up, then probe HTTP /health to
	// confirm the server is actually serving before we trigger shutdown.
	deadline := time.Now().Add(2 * time.Second)
	httpAddr := "http://127.0.0.1:" + strconv.Itoa(cfg.HTTPPort)
	for {
		resp, dialErr := http.Get(httpAddr + "/health")
		if dialErr == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-runDone
			t.Fatalf("HTTP server did not come up: %v", dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Trigger graceful shutdown via context cancel (the signal-notify path).
	shutdownStart := time.Now()
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runServers returned error: %v", err)
		}
		// Drain budget per server is 10s; total should still be sub-15s
		// for an idle server with no in-flight requests.
		if d := time.Since(shutdownStart); d > 15*time.Second {
			t.Errorf("graceful shutdown took %v; expected sub-15s", d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return within 20s of context cancel")
	}

	// HTTP listener should be closed (refused connection or fast 5xx).
	if _, err := http.Get(httpAddr + "/health"); err == nil {
		t.Error("HTTP server still serving after runServers returned")
	}
}
