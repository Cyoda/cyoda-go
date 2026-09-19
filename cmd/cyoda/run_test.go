package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// newRunServersFixture builds an App plus the three loopback listeners
// runServers is handed, so every test starts from one wiring. The sockets are
// bound here and stay bound until a server closes them: no port is ever
// released and claimed again, so no neighbouring test can take it in between.
func newRunServersFixture(t *testing.T) (*app.App, app.Config, serverListeners) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)

	listen := func(name string) net.Listener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("%s listen: %v", name, err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	return a, cfg, serverListeners{grpc: listen("grpc"), http: listen("http"), admin: listen("admin")}
}

// assertListenersClosed fails the test for every listener still accepting. It
// asks the listener itself rather than dialling its address: a freed
// ephemeral port can be re-bound by a neighbouring test, so a dial proves
// nothing about this socket.
func assertListenersClosed(t *testing.T, ls serverListeners) {
	t.Helper()
	for name, l := range map[string]net.Listener{"grpc": ls.grpc, "http": ls.http, "admin": ls.admin} {
		tl := l.(*net.TCPListener)
		_ = tl.SetDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := tl.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s listener not closed after runServers returned: Accept error = %v", name, err)
		}
	}
}

// TestRunServers_CtxCancelDrainsBothServers exercises the SIGTERM path
// without spawning a subprocess: we cancel the root context the
// way signal.NotifyContext would on SIGTERM, and assert runServers
// returns within the drain budget after stopping every listener.
func TestRunServers_CtxCancelDrainsBothServers(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)

	rootCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- runServers(rootCtx, a, cfg, ls)
	}()

	// Probe HTTP /health to confirm the server is actually serving on the
	// listener it was handed before we trigger shutdown. The loop leaves as
	// soon as HTTP answers, so the generous deadline costs nothing and
	// spares a starved race-detector run.
	deadline := time.Now().Add(15 * time.Second)
	httpAddr := "http://" + ls.http.Addr().String()
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

	assertListenersClosed(t, ls)
}

// TestRunServers_ShutdownBeforeGRPCServeIsClean pins an ordering runServers
// does not control. The goroutine that calls Serve and the watcher that
// graceful-stops on cancel are siblings, so a cancel landing during startup
// can stop the gRPC server before Serve is entered; grpc-go then has Serve
// return ErrServerStopped. That is a shutdown that won a race, not a serve
// failure, and runServers must report it as the clean shutdown it is.
//
// The interleaving is constructed, not raced: the server is stopped before
// runServers starts, which is exactly the state the losing schedule leaves
// for Serve to find.
func TestRunServers_ShutdownBeforeGRPCServeIsClean(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)

	a.StopGRPC()
	rootCtx, cancel := context.WithCancel(context.Background())
	cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- runServers(rootCtx, a, cfg, ls)
	}()

	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runServers returned error for a shutdown that preceded gRPC Serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return within 20s of context cancel")
	}

	// Every server closes the listener it is handed even when it declines
	// to serve, so a shutdown that wins the race leaks no socket.
	assertListenersClosed(t, ls)
}

// TestRunServers_GRPCServeFailureIsReported pins the other half of the
// contract: only a stop is a clean exit. A listener that cannot accept makes
// Serve fail with no stop requested. That must surface as runServers' error,
// and it must cancel the group — runServers returns on its own rather than
// going on serving HTTP with no gRPC server behind it.
func TestRunServers_GRPCServeFailureIsReported(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)
	if err := ls.grpc.Close(); err != nil {
		t.Fatalf("close grpc listener: %v", err)
	}

	runDone := make(chan error, 1)
	go func() {
		runDone <- runServers(context.Background(), a, cfg, ls)
	}()

	select {
	case err := <-runDone:
		if err == nil {
			t.Fatal("runServers returned nil for a gRPC listener that cannot accept")
		}
		if !strings.Contains(err.Error(), "grpc serve:") {
			t.Errorf("runServers error = %q; want it to name the gRPC serve failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return after the gRPC server failed to serve")
	}

	assertListenersClosed(t, ls)
}

// TestServerListeners_CloseReleasesWhatIsBound pins the release listenAll
// relies on when a later bind fails: every bound socket is closed, and a
// surface that was never bound is skipped rather than dereferenced.
func TestServerListeners_CloseReleasesWhatIsBound(t *testing.T) {
	_, _, ls := newRunServersFixture(t)
	partial := serverListeners{grpc: ls.grpc, http: ls.http}

	partial.close()

	for name, l := range map[string]net.Listener{"grpc": ls.grpc, "http": ls.http} {
		tl := l.(*net.TCPListener)
		_ = tl.SetDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := tl.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s listener still open after close: Accept error = %v", name, err)
		}
	}
}

// TestListenAll_BindsEveryListenerFromConfig pins where each socket is bound:
// the application and gRPC surfaces on every interface, the admin surface on
// its configured bind address only.
func TestListenAll_BindsEveryListenerFromConfig(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.HTTPPort, cfg.GRPC.Port, cfg.Admin.Port = 0, 0, 0
	cfg.Admin.BindAddress = "127.0.0.1"

	ls, err := listenAll(cfg)
	if err != nil {
		t.Fatalf("listenAll: %v", err)
	}
	t.Cleanup(ls.close)

	if ip := ls.admin.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("admin listener bound %v; want the configured loopback bind address", ip)
	}
	for name, l := range map[string]net.Listener{"grpc": ls.grpc, "http": ls.http} {
		if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsUnspecified() {
			t.Errorf("%s listener bound %v; want every interface", name, ip)
		}
	}
}

// TestListenAll_AdminBindAddressMayBeIPv6 pins that the admin bind address is
// joined to its port as a host, not pasted in front of it: an IPv6 literal
// needs its brackets.
func TestListenAll_AdminBindAddressMayBeIPv6(t *testing.T) {
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	_ = probe.Close()

	cfg := app.DefaultConfig()
	cfg.HTTPPort, cfg.GRPC.Port, cfg.Admin.Port = 0, 0, 0
	cfg.Admin.BindAddress = "::1"

	ls, err := listenAll(cfg)
	if err != nil {
		t.Fatalf("listenAll with an IPv6 admin bind address: %v", err)
	}
	t.Cleanup(ls.close)
	if ip := ls.admin.Addr().(*net.TCPAddr).IP; !ip.Equal(net.IPv6loopback) {
		t.Errorf("admin listener bound %v; want ::1", ip)
	}
}

// TestListenAll_BindFailureNamesTheListener pins the fail-fast contract: a
// port that cannot be bound is an error before any server starts, and the
// error says which surface it was.
func TestListenAll_BindFailureNamesTheListener(t *testing.T) {
	// Every interface, as the HTTP surface binds: a loopback-only holder
	// would not collide with a wildcard bind on every platform.
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	cfg := app.DefaultConfig()
	cfg.GRPC.Port, cfg.Admin.Port = 0, 0
	cfg.Admin.BindAddress = "127.0.0.1"
	cfg.HTTPPort = taken.Addr().(*net.TCPAddr).Port

	ls, err := listenAll(cfg)
	if err == nil {
		ls.close()
		t.Fatal("listenAll succeeded on a port that is already bound")
	}
	if !strings.Contains(err.Error(), "http listener") {
		t.Errorf("listenAll error = %q; want it to name the HTTP listener", err)
	}
}
