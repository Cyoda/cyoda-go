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

func mustDuration(t *testing.T, env []string, key string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(envValue(t, env, key))
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return d
}

func TestTunedEnv_PatienceIsShortAndValid(t *testing.T) {
	single := mustDuration(t, fixtureutil.TunedServerEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT")
	if single <= 0 || single > 500*time.Millisecond {
		t.Errorf("single-node patience = %v; want within (0, 500ms]", single)
	}
	cluster := mustDuration(t, fixtureutil.TunedClusterEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT")
	if cluster < time.Second || cluster >= 5*time.Second {
		t.Errorf("cluster patience = %v; want within [1s, 5s): room for gossip, below the default", cluster)
	}
}

// TestTunedEnv_SchedulerTimingIsShortAndValid pins the scheduler timing every
// parity and multi-node fixture runs with. The server refuses to start with a
// STALE_AFTER below 50s + 3 x HEARTBEAT_INTERVAL (the watchdog margin: a
// 30s commit budget, 10s slack, one heartbeat budget, three intervals), so
// the tuned value is that floor and no lower.
func TestTunedEnv_SchedulerTimingIsShortAndValid(t *testing.T) {
	for name, env := range map[string][]string{
		"server":  fixtureutil.TunedServerEnv(),
		"cluster": fixtureutil.TunedClusterEnv(),
	} {
		t.Run(name, func(t *testing.T) {
			if scan := mustDuration(t, env, "CYODA_SCHEDULER_SCAN_INTERVAL"); scan != 50*time.Millisecond {
				t.Errorf("scan interval = %v; want 50ms", scan)
			}
			hb := mustDuration(t, env, "CYODA_SCHEDULER_HEARTBEAT_INTERVAL")
			stale := mustDuration(t, env, "CYODA_SCHEDULER_STALE_AFTER")
			retry := mustDuration(t, env, "CYODA_SCHEDULER_RETRY_DELAY")
			retryMax := mustDuration(t, env, "CYODA_SCHEDULER_RETRY_DELAY_MAX")
			if hb != fixtureutil.TunedHeartbeatInterval || stale != fixtureutil.TunedStaleAfter ||
				retry != fixtureutil.TunedRetryDelay || retryMax != fixtureutil.TunedRetryDelayMax {
				t.Errorf("env and exported constants disagree: hb=%v stale=%v retry=%v retryMax=%v", hb, stale, retry, retryMax)
			}
			if hb <= 0 || hb > time.Second {
				t.Errorf("heartbeat interval = %v; want within (0, 1s]", hb)
			}
			if floor := 50*time.Second + 3*hb; stale < floor || stale > floor+5*time.Second {
				t.Errorf("stale after = %v; want within [%v, %v]: the server's floor, and no slower", stale, floor, floor+5*time.Second)
			}
			if retry <= 0 || retry > time.Second || retryMax < retry || retryMax > 5*time.Second {
				t.Errorf("retry delay %v / max %v; want a delay within (0, 1s] and a max within [delay, 5s]", retry, retryMax)
			}
		})
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
		for _, literal := range []string{
			"CYODA_SCHEDULER_SCAN_INTERVAL", "CYODA_DISPATCH_WAIT_TIMEOUT",
			"CYODA_SCHEDULER_HEARTBEAT_INTERVAL", "CYODA_SCHEDULER_STALE_AFTER",
			"CYODA_SCHEDULER_RETRY_DELAY",
		} {
			if strings.Contains(src, literal) {
				t.Errorf("%s spells out %s; it belongs in fixtureutil/tuned_env.go only", tc.file, literal)
			}
		}
	}
}
