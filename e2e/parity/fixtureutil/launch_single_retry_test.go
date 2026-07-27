package fixtureutil_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestLaunchCyodaSingleNode_FastFailsOnEarlyExit pins the single-node
// analogue of the cluster launcher's TOCTOU self-healing: when the cyoda
// child dies before it can become healthy (the observable symptom of a
// FreePort() bind collision — "bind: address already in use"), the launcher
// must detect the exit and give up fast, NOT block the full readiness
// timeout waiting for a health endpoint that can never come up.
//
// Regression guard: before the single-node path raced health against the
// process exit (mirroring nodeOutcome in the cluster path), a dead child
// stalled the whole ReadinessTimeout — turning a transient collision into a
// 2-minute fixture-setup hang and a red parity job. Here we launch a binary
// that exits immediately and assert the launcher returns well inside the
// timeout, reporting the early exit rather than a blind health timeout.
func TestLaunchCyodaSingleNode_FastFailsOnEarlyExit(t *testing.T) {
	falsePath, err := exec.LookPath("false")
	if err != nil {
		t.Skipf("no `false` binary on PATH to simulate an immediately-exiting cyoda child: %v", err)
	}

	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}

	// A generous readiness timeout: if the launcher blindly waited on health
	// (the pre-fix behaviour) it would burn the full 20s. The fast-exit race
	// must return in a small fraction of that. `false` exits 1 the instant it
	// starts, so all retry attempts collapse near-instantly.
	const readiness = 20 * time.Second
	start := time.Now()
	result, cleanup, err := fixtureutil.LaunchCyodaAndComputeWithBinaries(
		falsePath, falsePath, ks,
		[]string{"CYODA_STORAGE_BACKEND=memory"},
		fixtureutil.LaunchOpts{ReadinessTimeout: readiness},
	)
	elapsed := time.Since(start)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}

	if err == nil {
		t.Fatalf("expected launch to fail when the cyoda child exits immediately, got result=%+v", result)
	}
	if elapsed >= readiness {
		t.Fatalf("launcher blocked ~%s (>= readiness %s) instead of fast-failing on the child's early exit", elapsed, readiness)
	}
	// Guard rail: even the retry budget (a few near-instant attempts) must stay
	// far under the readiness window. Half is a wide, non-flaky margin.
	if elapsed >= readiness/2 {
		t.Errorf("launcher took %s — expected a near-instant fast-fail, well under half the %s readiness window", elapsed, readiness)
	}
	if !strings.Contains(err.Error(), "exited before becoming healthy") {
		t.Errorf("error should report the early process exit, got: %v", err)
	}
}
