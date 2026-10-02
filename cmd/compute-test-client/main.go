// Package main is the compute-test-client binary used by the parity E2E
// suite. It connects to a running cyoda-go gRPC endpoint as a
// calculation member, registers a fixed catalog of named processors
// and criteria, and serves them indefinitely until SIGTERM.
//
// The binary is launched as a subprocess by per-backend test fixtures
// (e2e/parity/{memory,sqlite,postgres}). Each fixture passes the cyoda gRPC
// endpoint via the CYODA_COMPUTE_GRPC_ENDPOINT environment variable, the cyoda
// HTTP base via CYODA_COMPUTE_HTTP_BASE, and the credentials of the M2M client
// the binary authenticates as via CYODA_COMPUTE_CLIENT_ID and
// CYODA_COMPUTE_CLIENT_SECRET.
//
// A separate local HTTP endpoint (/healthz for readiness; /record and
// /release for a scenario to read what the client received, to trigger a
// late-callback client's callbacks, and to make a hold client answer what it
// holds) on an ephemeral port (printed to
// stdout at startup) lets the fixture's readiness probe confirm the
// compute client is connected and ready before running scenarios.
//
// Two optional variables let a fixture start further clients for one
// scenario: CYODA_TEST_COMPUTE_TAGS (comma-separated join tags) and
// CYODA_TEST_COMPUTE_BEHAVIOUR (stall, fail, fail-retryable, late-callback,
// drop, hold). Unset, the client joins as `compute-test-client` and serves its
// catalog.
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

	// The client authenticates as a stored M2M client: it fetches its bearer
	// from CYODA_COMPUTE_HTTP_BASE's token endpoint with these credentials and
	// refreshes it before it expires. The secret is never logged.
	clientID := os.Getenv("CYODA_COMPUTE_CLIENT_ID")
	clientSecret := os.Getenv("CYODA_COMPUTE_CLIENT_SECRET")
	httpBase := os.Getenv("CYODA_COMPUTE_HTTP_BASE")
	if clientID == "" || clientSecret == "" || httpBase == "" {
		slog.Error("CYODA_COMPUTE_CLIENT_ID, CYODA_COMPUTE_CLIENT_SECRET and CYODA_COMPUTE_HTTP_BASE must be set", "pkg", "compute-test-client")
		os.Exit(1)
	}
	tokens := newTokenSource(httpBase, clientID, clientSecret)

	// HTTP callbacks for callback-join processors go to the same instance, as
	// the same client (same tenant as dispatch).
	cb := newCallbackClient(httpBase, tokens.Token)

	// gRPC EntityManage callback client (cross-node gRPC callback).
	// It dials the same gRPC endpoint the member streams from; when that node is a
	// non-owner for a forwarded dispatch, the callback forwards B→A to the owner.
	gcb, err := newGRPCCallbackClient(endpoint, tokens.Token)
	if err != nil {
		slog.Error("gRPC callback client failed", "pkg", "compute-test-client", "error", err)
		os.Exit(1)
	}
	defer gcb.close()

	cat := newCatalog(cb, gcb)
	slog.Info("catalog loaded", "pkg", "compute-test-client",
		"processors", len(cat.processors), "criteria", len(cat.criteria), "functions", len(cat.functions),
		"callbackProcessors", len(cat.callbackProcessors), "callbackCriteria", len(cat.callbackCriteria))

	tags := parseTags(os.Getenv("CYODA_TEST_COMPUTE_TAGS"))
	beh, err := parseBehaviour(os.Getenv("CYODA_TEST_COMPUTE_BEHAVIOUR"))
	if err != nil {
		slog.Error("invalid CYODA_TEST_COMPUTE_BEHAVIOUR", "pkg", "compute-test-client", "error", err)
		os.Exit(1)
	}

	rec := newRecorder()
	disp := newDispatcher(endpoint, tokens.Token, cat, gcb, tags, beh, rec)
	slog.Info("behaviour", "pkg", "compute-test-client", "tags", tags, "behaviour", string(beh))

	// Start the health server first so the fixture can poll it before
	// the gRPC connection settles.
	hs, err := newHealthServer(rec, disp.release)
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
