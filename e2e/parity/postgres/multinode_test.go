package postgres

import (
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/multinode"
)

// TestMultiNode runs the cluster-shareable scenario set against a 3-node
// postgres-backed cyoda-go cluster. Memory and sqlite have no
// MultiNodeFixture (cannot share state across processes), so they don't
// expose this entry.
//
// This entry uses its own setup (independent of the per-package
// sharedFixture used by TestParity) — the multi-node fixture is
// heavier (3 cyoda-go subprocesses + a postgres container) and should
// not pollute the single-node TestParity run.
//
// Short-mode is handled by TestMain (os.Exit(0) before any test runs).
func TestMultiNode(t *testing.T) {
	fix, cleanup := MustSetupMultiNode(t, 3)
	defer cleanup()
	for _, nt := range multinode.AllTests() {
		t.Run(nt.Name, func(t *testing.T) { nt.Fn(t, fix) })
	}
}
