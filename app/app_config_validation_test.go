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
