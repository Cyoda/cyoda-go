package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/admin"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// shutdownDrainBudget bounds graceful HTTP/admin server drain. Matches the
// gRPC graceful-stop budget in app.App.Close so total stop time is
// predictable as ~max(http, admin, grpc) drain.
const shutdownDrainBudget = 10 * time.Second

// runServers starts the gRPC, HTTP, and admin listeners and blocks until
// rootCtx is cancelled (typically by SIGINT/SIGTERM via signal.NotifyContext
// in main). On cancel it drains each server within shutdownDrainBudget,
// invokes a.Shutdown() to release background goroutines and cluster
// resources, and a.Close() to release the storage factory + run the gRPC
// graceful-stop dance with deadline.
//
// All three servers are coordinated by an errgroup whose context is the
// caller-supplied rootCtx; cancellation propagates through all goroutines.
// A failure in any server (e.g. listen-port conflict) cancels the group
// and surfaces as the returned error, which lets main() exit cleanly with
// a non-zero status without bypassing deferred OTel flush.
//
// runServers does not call os.Exit. The caller (main) maps the returned
// error to an exit code after deferred cleanups have run.
func runServers(
	rootCtx context.Context,
	a *app.App,
	cfg app.Config,
	grpcListener net.Listener,
) error {
	g, ctx := errgroup.WithContext(rootCtx)

	// gRPC server. Serve does not honour ctx by itself, so a watcher
	// goroutine triggers GracefulStop on cancel. We graceful-stop here
	// (not in App.Close) because Serve must return before errgroup.Wait
	// can — otherwise the group blocks forever and the post-Wait cleanup
	// (a.Shutdown / a.Close) never runs. The deadline-bounded fallback
	// to hard Stop already lives in App.Close, which is invoked after
	// Wait; here we just need GracefulStop to unblock Serve.
	grpcAddr := grpcListener.Addr().String()
	g.Go(func() error {
		slog.Info("gRPC server starting", "addr", grpcAddr)
		if err := a.GRPCServer().Serve(grpcListener); err != nil {
			// Serve returns nil on a clean Stop/GracefulStop. A non-nil
			// error here is a real failure (e.g. bind issue surfaced
			// after Listen succeeded). Propagate to cancel the group.
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		// Drain gRPC with the same deadline budget as HTTP/admin so total
		// shutdown is predictable. The drain is gated by a sync.Once on
		// App so the second invocation in a.Close() is a no-op rather
		// than re-entering the deadline branch (#68 follow-up).
		// Concrete sequence on signal:
		//   1. ctx.Done fires (signal.NotifyContext)
		//   2. http.Shutdown / admin.Shutdown drain in their own goroutines
		//   3. this watcher graceful-stops gRPC so Serve returns
		//   4. errgroup.Wait returns
		//   5. caller invokes a.Shutdown() then a.Close()
		//   6. a.Close()'s StopGRPC short-circuits via the once.
		a.StopGRPC()
		return nil
	})

	// HTTP server (the application surface).
	httpAddr := fmt.Sprintf(":%d", cfg.HTTPPort)
	httpServer := &http.Server{Addr: httpAddr, Handler: a.Handler()}
	g.Go(func() error {
		slog.Info("HTTP server starting", "addr", httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainBudget)
		defer cancel()
		if err := httpServer.Shutdown(drainCtx); err != nil {
			slog.Error("HTTP server shutdown failed", "error", err)
		}
		return nil
	})

	// Admin server (/livez, /readyz, /metrics).
	adminAddr := fmt.Sprintf("%s:%d", cfg.Admin.BindAddress, cfg.Admin.Port)
	adminServer := &http.Server{
		Addr: adminAddr,
		Handler: admin.NewHandler(admin.Options{
			Readiness:          a.ReadinessCheck,
			MetricsBearerToken: cfg.Admin.MetricsBearerToken,
			MetricsHandler:     observability.MetricsHandler(),
		}),
	}
	g.Go(func() error {
		slog.Info("admin server starting", "addr", adminAddr)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("admin serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
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
		slog.Info("received signal, starting graceful shutdown")
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
