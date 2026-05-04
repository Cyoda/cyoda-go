package fixtureutil_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestLaunchCyodaClusterAndComputeWithBinaries — issue #157.
//
// Out-of-tree consumers (plugins maintained in their own repos, e.g.
// cyoda-go-cassandra) need to drive the shared parity scenario suite
// against *their* cmd/cyoda-go binary that blank-imports their backend.
// The single-node analogue LaunchCyodaAndComputeWithBinaries already
// exists; this test pins the cluster symmetry.
//
// The function must accept caller-supplied binary paths and otherwise
// reuse the same cluster-bootstrap logic as LaunchCyodaClusterAndCompute
// (port allocation × 4 per node, gossip-seed CSV, HMAC secret derivation,
// concurrent health probing, compute-client wiring to node 0).
func TestLaunchCyodaClusterAndComputeWithBinaries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster launch under -short")
	}

	cyodaBin, err := fixtureutil.BuildCyodaBinary()
	if err != nil {
		t.Fatalf("BuildCyodaBinary: %v", err)
	}
	computeBin, err := fixtureutil.BuildComputeBinary()
	if err != nil {
		t.Fatalf("BuildComputeBinary: %v", err)
	}

	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}

	const n = 2
	result, cleanup, err := fixtureutil.LaunchCyodaClusterAndComputeWithBinaries(
		cyodaBin, computeBin, ks, n,
		[]string{"CYODA_STORAGE_BACKEND=memory"},
	)
	if err != nil {
		t.Fatalf("LaunchCyodaClusterAndComputeWithBinaries: %v", err)
	}
	t.Cleanup(cleanup)

	if got := len(result.BaseURLs); got != n {
		t.Fatalf("BaseURLs: got %d entries, want %d", got, n)
	}
	if result.GRPCEndpoint == "" {
		t.Errorf("GRPCEndpoint is empty")
	}
	if len(result.CyodaCmds) != n {
		t.Errorf("CyodaCmds: got %d, want %d", len(result.CyodaCmds), n)
	}
	if result.ComputeCmd == nil {
		t.Errorf("ComputeCmd is nil")
	}

	// Each node should be reachable on /api/health. Compute-client and
	// node-0 readiness are already verified internally before the launcher
	// returns; confirm the *other* nodes too so cluster-mode bootstrap
	// (gossip seed CSV, HMAC sharing) actually succeeded for them.
	client := &http.Client{Timeout: 2 * time.Second}
	for i, baseURL := range result.BaseURLs {
		resp, err := client.Get(baseURL + "/api/health")
		if err != nil {
			t.Errorf("node %d health: %v", i, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("node %d health: got status %d, want 200", i, resp.StatusCode)
		}
	}
}
