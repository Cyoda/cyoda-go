//go:build !windows

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// freePortIO returns an unused TCP port at call time. Same caveat as
// freePort in run_test.go.
func freePortIO(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePortIO: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestShutdown_OnSIGTERM_DrainsBothServers builds the cyoda binary,
// starts it, sends SIGTERM after a brief warm-up, and asserts the
// process exits cleanly within the drain budget.
//
// This is the end-to-end pin for the #26 graceful-shutdown work: it
// catches regressions where (a) the signal handler is not wired,
// (b) servers fail to drain on signal, or (c) deferred OTel flush is
// bypassed by os.Exit-from-goroutine.
func TestShutdown_OnSIGTERM_DrainsBothServers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess shutdown test in -short mode")
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "cyoda-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build cyoda: %v", err)
	}

	httpPort := freePortIO(t)
	grpcPort := freePortIO(t)
	adminPort := freePortIO(t)

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CYODA_HTTP_PORT="+strconv.Itoa(httpPort),
		"CYODA_GRPC_PORT="+strconv.Itoa(grpcPort),
		"CYODA_ADMIN_PORT="+strconv.Itoa(adminPort),
		"CYODA_ADMIN_BIND_ADDRESS=127.0.0.1",
		"CYODA_SUPPRESS_BANNER=true",
		"CYODA_LOG_LEVEL=info",
		"CYODA_OTEL_ENABLED=false",
		"CYODA_IAM_MODE=mock",
	)
	// Put the child in its own process group so SIGTERM lands on the
	// child only, not on this test process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		// Best-effort cleanup if the test fails out before SIGTERM lands.
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
	}()

	// Wait until the admin /livez endpoint responds, signalling the
	// child has bound its listeners.
	deadline := time.Now().Add(15 * time.Second)
	adminAddr := "127.0.0.1:" + strconv.Itoa(adminPort)
	for {
		c, err := net.DialTimeout("tcp", adminAddr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not start admin listener: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Send SIGTERM and assert clean exit within the drain budget.
	sendStart := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	select {
	case err := <-exitCh:
		if err != nil {
			// Exit status non-zero is a regression: the signal path
			// should yield a clean exit.
			t.Errorf("child exited with error: %v", err)
		}
		// Sub-15s is generous; with idle servers we expect <2s in practice.
		if d := time.Since(sendStart); d > 15*time.Second {
			t.Errorf("child took %v to exit on SIGTERM; expected sub-15s", d)
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		t.Fatal("child did not exit within 20s of SIGTERM — graceful shutdown not wired")
	}
}
