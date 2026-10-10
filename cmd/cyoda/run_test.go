package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
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
		runDone <- runServers(rootCtx, a, cfg, ls, a.DrainScheduler)
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
		runDone <- runServers(rootCtx, a, cfg, ls, a.DrainScheduler)
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
		runDone <- runServers(context.Background(), a, cfg, ls, a.DrainScheduler)
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
// every surface on its own configured bind address. The two cases swap which
// API surface listens on every interface, so a surface that read another
// one's setting, or none, fails one of them.
func TestListenAll_BindsEveryListenerFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name, httpHost, grpcHost string
	}{
		{"http loopback, grpc every interface", "127.0.0.1", "0.0.0.0"},
		{"http every interface, grpc loopback", "0.0.0.0", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := app.DefaultConfig()
			cfg.HTTPPort, cfg.GRPC.Port, cfg.Admin.Port = 0, 0, 0
			cfg.HTTP.BindAddress = tc.httpHost
			cfg.GRPC.BindAddress = tc.grpcHost
			cfg.Admin.BindAddress = "127.0.0.1"

			ls, err := listenAll(cfg)
			if err != nil {
				t.Fatalf("listenAll: %v", err)
			}
			t.Cleanup(ls.close)

			for _, c := range []struct {
				name string
				l    net.Listener
				host string
			}{
				{"http", ls.http, tc.httpHost},
				{"grpc", ls.grpc, tc.grpcHost},
				{"admin", ls.admin, "127.0.0.1"},
			} {
				// Go serves 0.0.0.0 on a dual-stack socket that reports ::,
				// so a wildcard matches any unspecified address.
				ip, want := c.l.Addr().(*net.TCPAddr).IP, net.ParseIP(c.host)
				if !ip.Equal(want) && !(want.IsUnspecified() && ip.IsUnspecified()) {
					t.Errorf("%s listener bound %v; want its configured bind address %s", c.name, ip, c.host)
				}
			}
		})
	}
}

// TestListenAll_BindAddressesMayBeIPv6 pins that every bind address is joined
// to its port as a host, not pasted in front of it: an IPv6 literal needs its
// brackets.
func TestListenAll_BindAddressesMayBeIPv6(t *testing.T) {
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	_ = probe.Close()

	cfg := app.DefaultConfig()
	cfg.HTTPPort, cfg.GRPC.Port, cfg.Admin.Port = 0, 0, 0
	cfg.HTTP.BindAddress, cfg.GRPC.BindAddress, cfg.Admin.BindAddress = "::1", "::1", "::1"

	ls, err := listenAll(cfg)
	if err != nil {
		t.Fatalf("listenAll with IPv6 bind addresses: %v", err)
	}
	t.Cleanup(ls.close)
	for name, l := range map[string]net.Listener{"grpc": ls.grpc, "http": ls.http, "admin": ls.admin} {
		if ip := l.Addr().(*net.TCPAddr).IP; !ip.Equal(net.IPv6loopback) {
			t.Errorf("%s listener bound %v; want ::1", name, ip)
		}
	}
}

// TestListenAll_BindFailureNamesTheListener pins the fail-fast contract: a
// port that cannot be bound is an error before any server starts, and the
// error says which surface it was.
func TestListenAll_BindFailureNamesTheListener(t *testing.T) {
	// The very address the HTTP surface is configured to bind: a holder on a
	// different host of the same port does not collide on every platform.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	cfg := app.DefaultConfig()
	cfg.GRPC.Port, cfg.Admin.Port = 0, 0
	cfg.HTTP.BindAddress, cfg.GRPC.BindAddress, cfg.Admin.BindAddress = "127.0.0.1", "127.0.0.1", "127.0.0.1"
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

// waitHTTPUp polls /health until the HTTP server answers.
func waitHTTPUp(t *testing.T, httpAddr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(httpAddr + "/health")
		if err == nil {
			resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP server did not come up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunServers_SignalDrainsTheSchedulerBeforeTheServers pins the §6.4
// order: on a signal the scheduler drains while the servers still serve, so
// its runs keep the compute-node streams and the callback routes.
func TestRunServers_SignalDrainsTheSchedulerBeforeTheServers(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)
	httpAddr := "http://" + ls.http.Addr().String()
	var servedDuringDrain atomic.Bool
	drained := make(chan struct{})
	drain := func(context.Context) {
		defer close(drained)
		resp, err := http.Get(httpAddr + "/health")
		if err == nil {
			resp.Body.Close()
			servedDuringDrain.Store(resp.StatusCode == http.StatusOK)
		}
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runServers(rootCtx, a, cfg, ls, drain) }()
	waitHTTPUp(t, httpAddr)

	cancel()
	select {
	case <-drained:
	case <-time.After(20 * time.Second):
		t.Fatal("the scheduler drain never ran on the signal path")
	}
	if !servedDuringDrain.Load() {
		t.Error("HTTP stopped serving before the scheduler finished draining")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runServers returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return after the drain")
	}
	assertListenersClosed(t, ls)
}

// TestRunServers_ServerFailureDoesNotWaitForTheSchedulerDrain: a failed server
// stops the others at once; a.Shutdown drains the scheduler after them.
func TestRunServers_ServerFailureDoesNotWaitForTheSchedulerDrain(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)
	if err := ls.grpc.Close(); err != nil {
		t.Fatalf("close grpc listener: %v", err)
	}
	var drainCalled atomic.Bool
	runDone := make(chan error, 1)
	go func() {
		runDone <- runServers(context.Background(), a, cfg, ls, func(context.Context) { drainCalled.Store(true) })
	}()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "grpc serve:") {
			t.Errorf("runServers error = %v; want the gRPC serve failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return after the gRPC server failed")
	}
	if drainCalled.Load() {
		t.Error("the signal-path drain ran on a server failure; a.Shutdown drains the scheduler there")
	}
}
