package app_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// TestNew_InvalidSearchAsyncConfigExits asserts app.New itself refuses a
// config whose async-search sizing it cannot honour, instead of building the
// pool from it. Before this, the checks lived only in cmd/cyoda/main.go, so
// any in-process embedder of app.New reached make(chan jobFunc, -1) and
// panicked — a startup crash with a runtime stack instead of a named,
// actionable configuration error.
//
// Subprocess re-exec (same pattern as TestNew_StorageFactoryFailureExits) so
// the os.Exit(1) path is observable without killing the parent test binary.
func TestNew_InvalidSearchAsyncConfigExits(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		cfg := app.DefaultConfig()
		cfg.ContextPath = ""
		cfg.SearchAsync.QueueLen = -1
		_ = app.New(cfg)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestNew_InvalidSearchAsyncConfigExits")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()
	output := string(out)

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("app.New accepted an invalid SearchAsync config; err=%v output=%q", err, output)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1; output=%q", code, output)
	}
	if strings.Contains(output, "panic:") {
		t.Errorf("invalid config must fail as a named startup error, not a panic stack: %s", output)
	}
	if !strings.Contains(output, "CYODA_SEARCH_ASYNC_QUEUE") {
		t.Errorf("startup error must name the offending setting; got: %s", output)
	}
}

// TestNew_UnknownIAMModeExits asserts app.New itself refuses an unrecognised
// CYODA_IAM_MODE instead of silently wiring mock auth for it. Before this,
// ValidateIAM was only called from cmd/cyoda/main.go, so an in-process
// embedder of app.New (the cyoda-go-cassandra binary, any other host built
// on app.New) that skipped that call would reach app.New's `if mode ==
// "jwt" { ... } else { mock }` branch and run every request as the mock
// admin for a typo'd mode such as "JWT".
func TestNew_UnknownIAMModeExits(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		cfg := app.DefaultConfig()
		cfg.ContextPath = ""
		cfg.IAM.Mode = "JWT"
		_ = app.New(cfg)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestNew_UnknownIAMModeExits")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()
	output := string(out)

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("app.New accepted an unknown IAM mode; err=%v output=%q", err, output)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1; output=%q", code, output)
	}
	if strings.Contains(output, "panic:") {
		t.Errorf("invalid config must fail as a named startup error, not a panic stack: %s", output)
	}
	if !strings.Contains(output, "CYODA_IAM_MODE") {
		t.Errorf("startup error must name the offending setting; got: %s", output)
	}
}

// TestNew_RequireJWTWithMockModeExits asserts app.New itself refuses
// CYODA_REQUIRE_JWT=true combined with CYODA_IAM_MODE=mock, rather than
// silently starting in mock (unauthenticated-by-default) mode. Same
// embedder-bypass rationale as TestNew_UnknownIAMModeExits: only
// cmd/cyoda/main.go called ValidateIAM before this fix.
func TestNew_RequireJWTWithMockModeExits(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		cfg := app.DefaultConfig()
		cfg.ContextPath = ""
		cfg.IAM.Mode = "mock"
		cfg.IAM.RequireJWT = true
		_ = app.New(cfg)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestNew_RequireJWTWithMockModeExits")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()
	output := string(out)

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("app.New accepted CYODA_REQUIRE_JWT=true with mock mode; err=%v output=%q", err, output)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1; output=%q", code, output)
	}
	if strings.Contains(output, "panic:") {
		t.Errorf("invalid config must fail as a named startup error, not a panic stack: %s", output)
	}
	if !strings.Contains(output, "CYODA_REQUIRE_JWT") {
		t.Errorf("startup error must name the offending setting; got: %s", output)
	}
}
