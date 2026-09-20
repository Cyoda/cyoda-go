package fixtureutil_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	t.Fatalf("%s is not set in %v", key, env)
	return ""
}

func TestTunedEnv_PatienceIsShortAndValid(t *testing.T) {
	single, err := time.ParseDuration(envValue(t, fixtureutil.TunedServerEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT"))
	if err != nil || single <= 0 || single > 500*time.Millisecond {
		t.Errorf("single-node patience = %v (err %v); want within (0, 500ms]", single, err)
	}
	cluster, err := time.ParseDuration(envValue(t, fixtureutil.TunedClusterEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT"))
	if err != nil || cluster < time.Second || cluster >= 5*time.Second {
		t.Errorf("cluster patience = %v (err %v); want within [1s, 5s): room for gossip, below the default", cluster, err)
	}
	if scan, err := time.ParseDuration(envValue(t, fixtureutil.TunedServerEnv(), "CYODA_SCHEDULER_SCAN_INTERVAL")); err != nil || scan != 50*time.Millisecond {
		t.Errorf("scan interval = %v (err %v); want 50ms", scan, err)
	}
}

// TestFixturesTakeTheirTuningFromOnePlace keeps the in-tree fixtures in
// lockstep: each calls the shared function, none spells a tuned variable out.
func TestFixturesTakeTheirTuningFromOnePlace(t *testing.T) {
	root := fixtureutil.FindModuleRoot()
	cases := []struct{ file, call string }{
		{"e2e/parity/memory/fixture.go", "fixtureutil.TunedServerEnv()"},
		{"e2e/parity/sqlite/fixture.go", "fixtureutil.TunedServerEnv()"},
		{"e2e/parity/postgres/fixture.go", "fixtureutil.TunedServerEnv()"},
		{"e2e/parity/postgres/multinode_fixture.go", "fixtureutil.TunedClusterEnv()"},
	}
	for _, tc := range cases {
		raw, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		src := string(raw)
		if !strings.Contains(src, tc.call) {
			t.Errorf("%s does not call %s", tc.file, tc.call)
		}
		for _, literal := range []string{"CYODA_SCHEDULER_SCAN_INTERVAL", "CYODA_DISPATCH_WAIT_TIMEOUT"} {
			if strings.Contains(src, literal) {
				t.Errorf("%s spells out %s; it belongs in fixtureutil/tuned_env.go only", tc.file, literal)
			}
		}
	}
}
