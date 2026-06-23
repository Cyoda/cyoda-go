package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/muesli/termenv"
	"golang.org/x/term"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
	"github.com/cyoda-platform/cyoda-go/internal/observability"

	// Stock storage plugins — blank-imported so their init() runs
	// and they register themselves with the spi registry.
	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
	_ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
	_ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// printVersion writes a one-line parse-friendly version summary.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "cyoda version %s (commit %s, built %s)\n", version, commit, buildDate)
}

// runHelpCmd is the entry point for `cyoda help [args...]`.
func runHelpCmd(args []string) int {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	style := ""
	if isTTY {
		if termenv.NewOutput(os.Stdout).HasDarkBackground() {
			style = "dark"
		} else {
			style = "light"
		}
	}
	// Wire the ldflag-injected version into help actions that stamp it
	// into their output envelopes (e.g. cloudevents json). RunHelp also
	// takes version as a parameter for the topic-JSON flow; the setter
	// feeds the action dispatch path.
	help.SetBinaryVersion(func() string { return version })
	return help.RunHelp(help.DefaultTree, args, os.Stdout, version, isTTY, style)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h":
			// Delegate to the help subsystem so there is a single source
			// of truth. No positional args → writeTreeSummary with USAGE +
			// FLAGS + TOPICS block. Users can still run 'cyoda help cli'
			// for the full CLI reference.
			os.Exit(runHelpCmd(nil))
		case "--version", "-v":
			printVersion(os.Stdout)
			return
		case "help":
			os.Exit(runHelpCmd(os.Args[2:]))
		case "init":
			os.Exit(runInit(os.Args[2:]))
		case "health":
			os.Exit(runHealth(os.Args[2:]))
		case "migrate":
			os.Exit(runMigrate(os.Args[2:]))
		}
	}

	app.LoadEnvFiles()
	cfg := app.DefaultConfig()
	cfg.Version = version
	logging.Init(cfg.LogLevel)

	if err := app.ValidateIAM(cfg.IAM); err != nil {
		slog.Error("IAM validation failed", "error", err)
		os.Exit(1)
	}
	if err := app.ValidateCORS(cfg.CORS); err != nil {
		slog.Error("CORS validation failed", "error", err)
		os.Exit(1)
	}
	logCORSMode(cfg.CORS)

	printBanner(cfg)
	printMockAuthWarningTo(os.Stdout, cfg)

	// OTel: the Prometheus scrape pipeline is always initialized (so /metrics
	// carries app metrics with no collector); OTLP push + tracing are gated
	// inside Init by the flag.
	nodeID := cfg.Cluster.NodeID
	if nodeID == "" {
		nodeID = "standalone"
	}
	shutdown, err := observability.Init(context.Background(), "cyoda", nodeID, cfg.OTelEnabled)
	if err != nil {
		slog.Error("failed to initialize OTel", "error", err)
		os.Exit(1)
	}
	defer shutdown(context.Background())

	a := app.New(cfg)

	// Ignore SIGPIPE: when piped through tee (./bin/cyoda | tee log),
	// Ctrl+C kills tee first, breaking the pipe. Go's default SIGPIPE behavior
	// for stdout writes is to exit immediately — before our SIGINT handler can
	// send LeaveGroup. Ignoring SIGPIPE lets the broken-pipe write fail silently
	// while the SIGINT handler runs the graceful shutdown.
	signal.Ignore(syscall.SIGPIPE)

	// Graceful shutdown: SIGINT (Ctrl+C) and SIGTERM cancel rootCtx; the
	// errgroup in runServers picks that up and drains every server.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	// Second-signal escape hatch: signal.NotifyContext only cancels on
	// the first signal; subsequent signals are no-ops, leaving the
	// operator with no recourse if the graceful drain hangs (stuck
	// in-flight RPC, slow-closing storage pool). Register the hard-exit
	// channel up-front so it is already armed when the first signal
	// arrives — otherwise the second signal can race the goroutine
	// setup and be lost. The goroutine only acts once rootCtx is
	// cancelled, so the first signal still flows through the graceful
	// path; only signals received AFTER cancellation force os.Exit(2).
	hardExitCh := make(chan os.Signal, 1)
	signal.Notify(hardExitCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-rootCtx.Done()
		// Drain any signal that landed in the buffered channel before
		// rootCtx was cancelled — that one is the first signal and is
		// already being handled by signal.NotifyContext.
		select {
		case <-hardExitCh:
		default:
		}
		<-hardExitCh
		slog.Warn("hard exit forced by second signal")
		os.Exit(2)
	}()

	// gRPC listen happens here (before runServers) so a bind error fails
	// fast with a clear non-zero exit — the deferred OTel flush still
	// runs because we use os.Exit only after the deferred-flush guard.
	grpcAddr := fmt.Sprintf(":%d", cfg.GRPC.Port)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		slog.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}

	if err := runServers(rootCtx, a, cfg, lis); err != nil {
		// runServers has already triggered a.Shutdown / a.Close before
		// returning; surface the failure as a non-zero exit code.
		os.Exit(1)
	}
}

// logCORSMode emits a single startup line describing the resolved CORS
// mode. The allowlist values themselves are logged only at DEBUG —
// origins are operationally sensitive (internal hostnames, customer
// subdomains in multi-tenant SaaS).
func logCORSMode(c app.CORSConfig) {
	switch c.Mode() {
	case "disabled":
		slog.Info("cors: disabled — no Access-Control-* headers will be emitted; configure CORS at your ingress/proxy layer", "pkg", "cors")
	case "wildcard":
		slog.Warn("cors: wildcard mode active (Access-Control-Allow-Origin: *)", "pkg", "cors")
	case "loopback":
		slog.Info("cors: loopback mode active — only http(s)://localhost, 127.0.0.1, [::1] are allowed; set CYODA_CORS_ALLOWED_ORIGINS to permit additional origins", "pkg", "cors")
	case "allowlist":
		// Two calls: count at INFO (always visible), contents at DEBUG (opt-in).
		// Origins are operationally sensitive (internal hostnames, customer
		// subdomains in multi-tenant SaaS) — never log them at INFO.
		slog.Info("cors: allowlist mode active", "pkg", "cors", "origin_count", len(c.AllowedOrigins))
		slog.Debug("cors: allowlist contents", "pkg", "cors", "origins", c.AllowedOrigins)
	default:
		slog.Warn("cors: unknown mode — this is a bug; please report", "pkg", "cors", "mode", c.Mode())
	}
}

func printBanner(cfg app.Config) {
	printBannerTo(os.Stdout, cfg)
}

func printBannerTo(w io.Writer, cfg app.Config) {
	if os.Getenv("CYODA_SUPPRESS_BANNER") == "true" {
		return
	}

	teal := "\033[38;5;80m"
	reset := "\033[0m"

	// Disable color if not a terminal
	if f, ok := w.(*os.File); !ok {
		teal = ""
		reset = ""
	} else if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		teal = ""
		reset = ""
	}

	fmt.Fprintf(w, "%s", teal)
	fmt.Fprintln(w, `   ██████╗██╗   ██╗ ██████╗ ██████╗  █████╗`)
	fmt.Fprintln(w, `  ██╔════╝╚██╗ ██╔╝██╔═══██╗██╔══██╗██╔══██╗`)
	fmt.Fprintln(w, `  ██║      ╚████╔╝ ██║   ██║██║  ██║███████║`)
	fmt.Fprintln(w, `  ██║       ╚██╔╝  ██║   ██║██║  ██║██╔══██║`)
	fmt.Fprintln(w, `  ╚██████╗   ██║   ╚██████╔╝██████╔╝██║  ██║`)
	fmt.Fprintln(w, `   ╚═════╝   ╚═╝    ╚═════╝ ╚═════╝ ╚═╝  ╚═╝`)
	fmt.Fprintf(w, "%s", reset)
	fmt.Fprintf(w, "  Cyoda-Go %s (%s) built %s\n", version, commit, buildDate)
	fmt.Fprintf(w, "  HTTP :%d | gRPC :%d | IAM %s | Path %s | Profiles %s\n\n",
		cfg.HTTPPort, cfg.GRPC.Port, cfg.IAM.Mode, cfg.ContextPath, app.ProfileBanner())
}

// printMockAuthWarningTo is silent unless IAM mode is "mock". Respects
// CYODA_SUPPRESS_BANNER.
func printMockAuthWarningTo(w io.Writer, cfg app.Config) {
	if os.Getenv("CYODA_SUPPRESS_BANNER") == "true" {
		return
	}
	if cfg.IAM.Mode != "mock" {
		return
	}
	yellow := "\033[33m"
	reset := "\033[0m"
	if f, ok := w.(*os.File); !ok {
		yellow = ""
		reset = ""
	} else if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		yellow = ""
		reset = ""
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, yellow+"========================================================================"+reset)
	fmt.Fprintln(w, yellow+"  WARNING: MOCK AUTH IS ACTIVE"+reset)
	fmt.Fprintln(w, yellow+"  All requests are accepted without authentication."+reset)
	fmt.Fprintln(w, yellow+"  This instance MUST NOT be exposed to untrusted networks."+reset)
	fmt.Fprintln(w, yellow+"  Set CYODA_IAM_MODE=jwt and CYODA_JWT_SIGNING_KEY to enable real auth."+reset)
	fmt.Fprintln(w, yellow+"  Suppress this banner with CYODA_SUPPRESS_BANNER=true (CI/tests only)."+reset)
	fmt.Fprintln(w, yellow+"========================================================================"+reset)
	fmt.Fprintln(w)
}

