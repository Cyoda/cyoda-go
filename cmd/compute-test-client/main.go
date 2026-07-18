// Package main is the compute-test-client binary used by the parity E2E
// suite. It connects to a running cyoda-go gRPC endpoint as a
// calculation member, registers a fixed catalog of named processors
// and criteria, and serves them indefinitely until SIGTERM.
//
// The binary is launched as a subprocess by per-backend test fixtures
// (e2e/parity/{memory,postgres}). Each fixture passes the cyoda gRPC
// endpoint via the CYODA_COMPUTE_GRPC_ENDPOINT environment variable.
//
// A separate /healthz HTTP endpoint on an ephemeral port (printed to
// stdout at startup) lets the fixture's readiness probe confirm the
// compute client is connected and ready before running scenarios.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	flag.Parse()

	endpoint := os.Getenv("CYODA_COMPUTE_GRPC_ENDPOINT")
	if endpoint == "" {
		slog.Error("CYODA_COMPUTE_GRPC_ENDPOINT must be set", "pkg", "compute-test-client")
		os.Exit(1)
	}

	token := os.Getenv("CYODA_COMPUTE_TOKEN")
	if token == "" {
		slog.Error("CYODA_COMPUTE_TOKEN must be set", "pkg", "compute-test-client")
		os.Exit(1)
	}

	// Optional HTTP base URL for feature #287 callback-join processors. When
	// unset, callback processors report a clear error rather than panicking;
	// the non-callback catalog still serves. The M2M token doubles as the
	// callback bearer (same tenant as dispatch).
	httpBase := os.Getenv("CYODA_COMPUTE_HTTP_BASE")
	cb := newCallbackClient(httpBase, token)

	// gRPC EntityManage callback client (feature #287 cross-node gRPC callback).
	// It dials the same gRPC endpoint the member streams from; when that node is a
	// non-owner for a forwarded dispatch, the callback forwards B→A to the owner.
	gcb, err := newGRPCCallbackClient(endpoint, token)
	if err != nil {
		slog.Error("gRPC callback client failed", "pkg", "compute-test-client", "error", err)
		os.Exit(1)
	}
	defer gcb.close()

	cat := newCatalog(cb, gcb)
	slog.Info("catalog loaded", "pkg", "compute-test-client",
		"processors", len(cat.processors), "criteria", len(cat.criteria), "functions", len(cat.functions),
		"callbackProcessors", len(cat.callbackProcessors), "callbackCriteria", len(cat.callbackCriteria),
		"callbackEnabled", cb != nil, "grpcCallbackEnabled", gcb != nil)

	// Start the health server first so the fixture can poll it before
	// the gRPC connection settles.
	hs, err := newHealthServer()
	if err != nil {
		slog.Error("health server failed", "pkg", "compute-test-client", "error", err)
		os.Exit(1)
	}
	hs.start()
	defer hs.stop()

	// Print the health endpoint address to stdout so the fixture can
	// parse it and probe the right port.
	fmt.Printf("HEALTH_ADDR=%s\n", hs.addr())

	// Connect to cyoda gRPC and start dispatching.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	disp := newDispatcher(endpoint, token, cat)

	stream, err := disp.connect(ctx)
	if err != nil {
		slog.Error("dispatcher connect failed", "pkg", "compute-test-client", "error", err)
		os.Exit(1)
	}
	defer disp.close()

	// Mark health-ready only after the gRPC stream is established and
	// the greet event has been received.
	hs.markReady()

	go func() {
		if err := disp.run(ctx, stream); err != nil {
			slog.Error("dispatch loop ended", "pkg", "compute-test-client", "error", err)
		}
	}()

	// Wait for SIGTERM/SIGINT.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	slog.Info("shutting down", "pkg", "compute-test-client")
}
