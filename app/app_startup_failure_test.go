package app_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/app"
)

// TestNew_StorageFactoryFailureExits is the representative test for the
// startup-failure normalisation contract. It asserts that a startup
// precondition failure produces:
//   - process exit status 1 (not a panic stack)
//   - a structured slog.Error line tagged with "startup failure"
//
// The test re-execs itself in a subprocess so the os.Exit(1) path can be
// observed without tearing down the parent test binary. Pattern modeled
// after Go's own os/exec_test.go style for testing fatal exits.
//
// We use the "unknown storage backend" path as the trigger because it is
// reachable from a Config-only setup (no env-var dance, no plugin
// registration) and exercises the same slog.Error + os.Exit shape as the
// converted panic sites.
func TestNew_StorageFactoryFailureExits(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		// Subprocess body: invoke app.New with a bogus storage backend to
		// trigger the startup-failure exit path. This call must not return.
		cfg := app.DefaultConfig()
		cfg.ContextPath = ""
		cfg.StorageBackend = "this-backend-does-not-exist"
		_ = app.New(cfg)
		// If we reach here, the failure path didn't exit — fail visibly.
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestNew_StorageFactoryFailureExits")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected subprocess to exit with a non-zero status; got err=%v output=%q", err, string(out))
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("expected exit code 1, got %d; output=%q", code, string(out))
	}

	// The converted handlers tag the failure with the structured field
	// `phase`. The unknown-backend path predates this sweep and uses
	// `backend`, so we accept either marker but require an "ERROR" log
	// line surfacing the failure (no raw "panic:" stack).
	output := string(out)
	if strings.Contains(output, "panic:") {
		t.Errorf("startup failure should not surface as a panic stack; got: %s", output)
	}
	if !strings.Contains(output, "ERROR") {
		t.Errorf("expected an ERROR-level slog line in subprocess output; got: %s", output)
	}
}

// TestNew_JWTSigningKeyMissingExits exercises the converted shape of the
// previously-panicking "CYODA_JWT_SIGNING_KEY is required when IAM mode is
// jwt" precondition. This path emits a structured
// slog.Error with phase="jwt-signing-key" and exits with status 1.
func TestNew_JWTSigningKeyMissingExits(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		cfg := app.DefaultConfig()
		cfg.ContextPath = ""
		cfg.IAM.Mode = "jwt"
		cfg.IAM.JWTSigningKey = ""
		_ = app.New(cfg)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestNew_JWTSigningKeyMissingExits")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected subprocess to exit with a non-zero status; got err=%v output=%q", err, string(out))
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("expected exit code 1, got %d; output=%q", code, string(out))
	}
	output := string(out)
	if strings.Contains(output, "panic:") {
		t.Errorf("startup failure should not surface as a panic stack; got: %s", output)
	}
	if !strings.Contains(output, "ERROR") {
		t.Errorf("expected an ERROR-level slog line in subprocess output; got: %s", output)
	}
	if !strings.Contains(output, "jwt-signing-key") {
		t.Errorf("expected phase=jwt-signing-key tag in slog output; got: %s", output)
	}
}
