package fixtureutil_test

import (
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// awaitIncarnation polls a node's captured log until its scheduler has
// announced an incarnation.
func awaitIncarnation(t *testing.T, logs func() string) uuid.UUID {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		id, err := fixtureutil.IncarnationFromLog(logs())
		if err == nil {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("no scheduler incarnation announced within 30s: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLaunchCyodaNode_RestartsWithANewIncarnation: a node launched on its own
// is healthy, announces one scheduler incarnation in its captured log, and is
// gone after Kill; a second launch on the same env is a new incarnation.
func TestLaunchCyodaNode_RestartsWithANewIncarnation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess launch under -short")
	}
	cyodaBin, err := fixtureutil.BuildCyodaBinary()
	if err != nil {
		t.Fatalf("BuildCyodaBinary: %v", err)
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	env := append([]string{"CYODA_STORAGE_BACKEND=memory"}, fixtureutil.TunedServerEnv()...)

	first, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, env, 0)
	if err != nil {
		t.Fatalf("LaunchCyodaNode: %v", err)
	}
	t.Cleanup(first.Kill)
	if first.GRPCEndpoint == "" {
		t.Error("GRPCEndpoint is empty")
	}
	firstID := awaitIncarnation(t, first.Logs.String)
	first.Kill()
	first.Kill() // harmless
	hc := &http.Client{Timeout: 2 * time.Second}
	if resp, err := hc.Get(first.BaseURL + "/api/health"); err == nil {
		resp.Body.Close()
		t.Error("the node still answers after Kill")
	}

	second, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, env, 0)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	t.Cleanup(second.Kill)
	if secondID := awaitIncarnation(t, second.Logs.String); secondID == firstID {
		t.Errorf("the relaunched node reused incarnation %s", firstID)
	}
}

// TestLaunchCluster_PerNodeEnvAndSignals: NodeEnv reaches only the node it
// names (node 1 runs without a scheduler), SignalNode delivers SIGTERM, and
// AwaitNodeExit tells an exited node from a running one.
func TestLaunchCluster_PerNodeEnvAndSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster launch under -short")
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	env := append([]string{"CYODA_STORAGE_BACKEND=memory"}, fixtureutil.TunedClusterEnv()...)
	result, cleanup, err := fixtureutil.LaunchCyodaClusterAndCompute(ks, 2, env, fixtureutil.LaunchOpts{
		NodeEnv: func(i int) []string {
			if i == 1 {
				return []string{"CYODA_SCHEDULER_ENABLED=false"}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("LaunchCyodaClusterAndCompute: %v", err)
	}
	t.Cleanup(cleanup)

	awaitIncarnation(t, result.NodeLogs[0].String)
	if strings.Contains(result.NodeLogs[1].String(), `msg="scheduler started"`) {
		t.Error("node 1 started a scheduler; its NodeEnv turned it off")
	}

	if err := result.AwaitNodeExit(1, 0); err == nil {
		t.Fatal("AwaitNodeExit reports a running node as exited")
	}
	if err := result.SignalNode(1, syscall.SIGTERM); err != nil {
		t.Fatalf("SignalNode: %v", err)
	}
	if err := result.AwaitNodeExit(1, 60*time.Second); err != nil {
		t.Fatalf("node 1 did not exit on SIGTERM: %v", err)
	}
	if err := result.AwaitNodeExit(1, 0); err != nil {
		t.Errorf("an exited node checked without waiting: %v", err)
	}
	if err := result.AwaitNodeExit(0, 0); err == nil {
		t.Error("node 0 reported exited; only node 1 was signalled")
	}
	if err := result.SignalNode(5, syscall.SIGTERM); err == nil {
		t.Error("SignalNode accepted a node that does not exist")
	}
}
