//go:build !windows

package main

import (
	"syscall"
	"testing"
	"time"
)

// TestShutdown_OnSIGTERM_DrainsBothServers builds the cyoda binary,
// starts it, sends SIGTERM once it is serving, and asserts the
// process exits cleanly within the drain budget.
//
// This is the end-to-end pin for the graceful-shutdown work: it
// catches regressions where (a) the signal handler is not wired,
// (b) servers fail to drain on signal, or (c) deferred OTel flush is
// bypassed by os.Exit-from-goroutine.
func TestShutdown_OnSIGTERM_DrainsBothServers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess shutdown test in -short mode")
	}

	child := startCyoda(t)

	// Any server's start line will do: the child wires its signal handling
	// and binds all three listeners before it logs the first of them.
	child.loopbackAddr(t, "admin")

	// Send SIGTERM and assert clean exit within the drain budget.
	sendStart := time.Now()
	if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	// Exit status non-zero is a regression: the signal path should yield
	// a clean exit.
	if code := child.wait(t, 20*time.Second); code != 0 {
		t.Errorf("child exited with status %d on SIGTERM; want 0", code)
	}
	// Sub-15s is generous; with idle servers we expect <2s in practice.
	if d := time.Since(sendStart); d > 15*time.Second {
		t.Errorf("child took %v to exit on SIGTERM; expected sub-15s", d)
	}
}
