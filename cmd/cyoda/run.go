package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/cyoda-platform/cyoda-go/app"
)

// shutdownDrainBudget bounds graceful HTTP/admin server drain. Matches the
// gRPC graceful-stop budget in app.App.Close so total stop time is
// predictable as ~max(http, admin, grpc) drain.
const shutdownDrainBudget = 10 * time.Second

// serverListeners are the bound sockets runServers serves on. The caller
// binds all three before any server starts, so a port that cannot be bound
// fails startup outright instead of surfacing once sibling servers are
// already accepting.
type serverListeners struct {
	grpc, http, admin net.Listener
}

// close releases every socket that is bound, skipping a surface that never
// was.
func (ls serverListeners) close() {
	for _, l := range []net.Listener{ls.grpc, ls.http, ls.admin} {
		if l != nil {
			_ = l.Close()
		}
	}
}

// listenAll binds the gRPC, HTTP and admin sockets from cfg, each on its own
// configured bind address, which is a bare host — an IPv6 literal goes in
// without brackets. The error names the surface that could not be bound; the address
// is already in the underlying net error. On failure nothing stays bound:
// the caller's teardown takes seconds, and a socket left listening through
// it would go on completing handshakes for a process that will never serve.
func listenAll(cfg app.Config) (serverListeners, error) {
	var ls serverListeners
	listen := func(name, host string, port int) (net.Listener, error) {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			ls.close()
			return nil, fmt.Errorf("%s listener: %w", name, err)
		}
		return l, nil
	}

	var err error
	if ls.grpc, err = listen("grpc", cfg.GRPC.BindAddress, cfg.GRPC.Port); err != nil {
		return serverListeners{}, err
	}
	if ls.http, err = listen("http", cfg.HTTP.BindAddress, cfg.HTTPPort); err != nil {
		return serverListeners{}, err
	}
	if ls.admin, err = listen("admin", cfg.Admin.BindAddress, cfg.Admin.Port); err != nil {
		return serverListeners{}, err
	}
	return ls, nil
}

// warnMockIAMExposure logs a WARN for each API listener that other hosts can
// reach while IAM is in mock mode, where every caller is the mock principal.
// It judges the socket actually bound, so a wildcard and a host name that
// resolves off loopback both count. It only warns: a container in mock mode
// must bind 0.0.0.0 for its port mapping to reach it, and the published port
// is then the boundary. The admin listener is not checked: mock IAM does not
// govern it.
func warnMockIAMExposure(iamMode string, ls serverListeners) {
	if iamMode != "mock" {
		return
	}
	for _, s := range []struct {
		name, setting string
		l             net.Listener
	}{{"http", "CYODA_HTTP_BIND_ADDRESS", ls.http}, {"grpc", "CYODA_GRPC_BIND_ADDRESS", ls.grpc}} {
		if addr, ok := s.l.Addr().(*net.TCPAddr); ok && !addr.IP.IsLoopback() {
			slog.Warn("mock IAM is active on a listener that is not on loopback: every caller that reaches it acts as the mock principal",
				"listener", s.name, "addr", addr.String(), "setting", s.setting)
		}
	}
}

// runServers serves gRPC, HTTP, and admin on the listeners it is handed and
// blocks until rootCtx is cancelled (typically by SIGINT/SIGTERM via
// signal.NotifyContext in runServe). On cancel it first drains the scheduler
// through drainScheduler, while every server still serves, and then drains
// each server within shutdownDrainBudget, invokes a.Shutdown() to release
// background goroutines and cluster resources, and a.Close() to release the
// storage factory + run the gRPC graceful-stop dance with deadline. Every
// server closes its own listener on the way out, on the serve-failure paths
// included.
//
// All three servers are coordinated by an errgroup whose context is derived
// from context.Background(), not rootCtx, so a signal does not itself cancel
// them — the watcher goroutine below drains the scheduler first and only
// then cancels stopCtx. A failure in any server (e.g. a fatal Accept error)
// still cancels the group at once and surfaces as the returned error.
//
// runServers does not call os.Exit. Its caller, runServe, turns the returned
// error into an exit status that it returns in turn, so the cleanups it has
// deferred — the OTel flush among them — run before the process exits.
func runServers(
	rootCtx context.Context,
	a *app.App,
	cfg app.Config,
	ls serverListeners,
	drainScheduler func(context.Context),
) error {
	g, gctx := errgroup.WithContext(context.Background())
	// stopCtx ends when the servers must stop. On a signal the scheduler drains
	// first, while its runs still have the compute-node streams and the
	// callback routes; then the servers drain. A server that fails stops the
	// others at once, and a.Shutdown drains the scheduler after them.
	stopCtx, stopServers := context.WithCancel(gctx)
	defer stopServers()
	g.Go(func() error {
		select {
		case <-rootCtx.Done():
			drainScheduler(context.Background())
			stopServers()
		case <-gctx.Done():
		}
		return nil
	})

	// gRPC server. Serve does not honour ctx by itself, so a watcher
	// goroutine triggers GracefulStop on cancel. We graceful-stop here
	// (not in App.Close) because Serve must return before errgroup.Wait
	// can — otherwise the group blocks forever and the post-Wait cleanup
	// (a.Shutdown / a.Close) never runs. The deadline-bounded fallback
	// to hard Stop lives in App.StopGRPC, which the watcher below calls;
	// App.Close calls it again after Wait and finds the drain already done.
	g.Go(func() error {
		slog.Info("gRPC server starting", "addr", ls.grpc.Addr().String())
		// A stop that lands once Serve has registered its listener makes
		// Serve return nil; one that lands before that makes it return
		// ErrServerStopped. The watcher below is a sibling goroutine, so
		// a cancel during startup can take either path, and both are the
		// same clean shutdown — the gRPC counterpart of
		// http.ErrServerClosed. Anything else is a real failure (e.g. a
		// fatal Accept error); propagate it to cancel the group.
		err := a.GRPCServer().Serve(ls.grpc)
		if err == nil || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("grpc serve: %w", err)
	})
	g.Go(func() error {
		<-stopCtx.Done()
		// Drain gRPC with the same deadline budget as HTTP/admin so total
		// shutdown is predictable. The drain is gated by a sync.Once on
		// App so the second invocation in a.Close() is a no-op rather
		// than re-entering the deadline branch.
		// Concrete sequence on signal:
		//   1. rootCtx.Done fires (signal.NotifyContext); the scheduler
		//      drains (a.DrainScheduler); stopCtx is cancelled
		//   2. http.Shutdown / admin.Shutdown drain in their own goroutines
		//   3. this watcher graceful-stops gRPC so Serve returns
		//   4. errgroup.Wait returns
		//   5. caller invokes a.Shutdown() then a.Close()
		//   6. a.Close()'s StopGRPC short-circuits via the once.
		a.StopGRPC()
		return nil
	})

	// HTTP server (the application surface).
	httpServer := newHTTPServer(a.Handler(), cfg.HTTP)
	g.Go(func() error {
		slog.Info("HTTP server starting", "addr", ls.http.Addr().String())
		if err := httpServer.Serve(ls.http); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-stopCtx.Done()
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainBudget)
		defer cancel()
		if err := httpServer.Shutdown(drainCtx); err != nil {
			slog.Error("HTTP server shutdown failed", "error", err)
		}
		return nil
	})

	// Admin server (/livez, /readyz, /metrics).
	adminServer := newHTTPServer(newAdminHandler(a.ReadinessCheck, cfg.Admin.MetricsBearerToken), cfg.HTTP)
	g.Go(func() error {
		slog.Info("admin server starting", "addr", ls.admin.Addr().String())
		if err := adminServer.Serve(ls.admin); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("admin serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-stopCtx.Done()
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainBudget)
		defer cancel()
		if err := adminServer.Shutdown(drainCtx); err != nil {
			slog.Error("admin server shutdown failed", "error", err)
		}
		return nil
	})

	// Block until either the context is cancelled (signal handler) or one
	// of the goroutines returns a non-nil error.
	err := g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("server group exited with error", "error", err)
	} else {
		slog.Info("shutdown requested, servers stopped")
	}

	// Background goroutines (reapers, gossip deregister, store close).
	a.Shutdown()
	// Storage close + gRPC graceful-stop with deadline.
	if closeErr := a.Close(); closeErr != nil {
		slog.Error("app close failed", "error", closeErr)
	}
	slog.Info("shutdown complete")

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
