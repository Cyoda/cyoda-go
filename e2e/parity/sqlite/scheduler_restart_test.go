package sqlite

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// restartTask reads the entity's Fire task through GET /scheduled-tasks.
func restartTask(t *testing.T, c *client.Client, id uuid.UUID) *client.ScheduledTask {
	t.Helper()
	page, err := c.ListScheduledTasks(t, url.Values{"entityId": {id.String()}})
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	for i := range page.Items {
		if page.Items[i].Transition == "Fire" {
			return &page.Items[i]
		}
	}
	return nil
}

func awaitRestartTask(t *testing.T, c *client.Client, id uuid.UUID, within time.Duration, what string, cond func(*client.ScheduledTask) bool) *client.ScheduledTask {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if task := restartTask(t, c, id); cond(task) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task of %s: %s not seen within %s; last %+v", id, what, within, restartTask(t, c, id))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSchedulerRestart_ReclaimsOwnRunningTasks: one SQLite pnode runs two
// tasks whose processors stall — R1 idempotent, R2 unsafe (a mark is set).
// The process is killed and started again on the same file. The new process
// is a new incarnation; after its own heartbeats have run a stale period it
// claims both as lost owners. R2's mark survived the restart: FAILED
// UNSAFE_WORK_NOT_COMPLETED, never sent again. R1 runs again and fires.
func TestSchedulerRestart_ReclaimsOwnRunningTasks(t *testing.T) {
	t.Parallel()
	cyodaBin, err := fixtureutil.BuildCyodaBinary()
	if err != nil {
		t.Fatalf("build cyoda: %v", err)
	}
	computeBin, err := fixtureutil.BuildComputeBinary()
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("key set: %v", err)
	}
	env := append([]string{
		"CYODA_STORAGE_BACKEND=sqlite",
		"CYODA_SQLITE_PATH=" + filepath.Join(t.TempDir(), "restart.db"),
		"CYODA_SQLITE_AUTO_MIGRATE=true",
		"CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS=300000",
	}, fixtureutil.TunedServerEnv()...)

	first, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, env, 0)
	if err != nil {
		t.Fatalf("launch the first process: %v", err)
	}
	t.Cleanup(first.Kill)
	tenant := fixtureutil.MintTenantJWT(t, ks)
	start := func(node *fixtureutil.NodeProc, tag, behaviour string) parity.ComputeClient {
		cc := fixtureutil.StartComputeClientForFixture(t, ks, computeBin, node.GRPCEndpoint, node.BaseURL,
			parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
		t.Cleanup(cc.Stop)
		return cc
	}
	tag1, tag2 := "r1-"+uuid.NewString()[:6], "r2-"+uuid.NewString()[:6]
	stall1 := start(first, tag1, parity.ComputeBehaviourStall)
	stall2 := start(first, tag2, parity.ComputeBehaviourStall)

	c := client.NewClient(first.BaseURL, tenant.Token)
	setup := func(model, tag string, idempotent bool) uuid.UUID {
		wf := restartWorkflow(model+"-wf", tag, idempotent)
		if err := c.ImportModel(t, model, 1, `{"k":1}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, model, 1); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, model, 1, wf); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		id, err := c.CreateEntity(t, model, 1, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		return id
	}
	r1 := setup("sq-restart-1", tag1, true)
	r2 := setup("sq-restart-2", tag2, false)
	for _, id := range []uuid.UUID{r1, r2} {
		awaitRestartTask(t, c, id, 15*time.Second, "running", func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "RUNNING" })
	}
	parity.AwaitReceived(t, stall1, 1, 10*time.Second)
	parity.AwaitReceived(t, stall2, 1, 10*time.Second)

	first.Kill()
	second, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, env, 0)
	if err != nil {
		t.Fatalf("launch the second process: %v", err)
	}
	t.Cleanup(second.Kill)
	restartAt := time.Now()
	c2 := client.NewClient(second.BaseURL, tenant.Token)
	hold1 := start(second, tag1, parity.ComputeBehaviourHold)
	fresh2 := start(second, tag2, parity.ComputeBehaviourCatalog)

	within := fixtureutil.TunedStaleAfter + 30*time.Second
	failed := awaitRestartTask(t, c2, r2, within, "R2 FAILED", func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if since := time.Since(restartAt); since < fixtureutil.TunedStaleAfter-2*time.Second {
		t.Errorf("R2 was decided %s after the restart; the new process may not claim a lost owner's task before a stale period of its own heartbeats", since)
	}
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || failed.LostOwners != 1 {
		t.Errorf("R2 = %+v; want FAILED UNSAFE_WORK_NOT_COMPLETED, lostOwners 1", *failed)
	}

	awaitRestartTask(t, c2, r1, within, "R1 reclaimed", func(tk *client.ScheduledTask) bool {
		return tk != nil && tk.Status == "RUNNING" && tk.LostOwners == 1
	})
	parity.AwaitReceived(t, hold1, 1, 10*time.Second)
	hold1.Release(t)
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := c2.GetEntity(t, r1)
		if err == nil && got.Meta.State == "Done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("R1 did not fire after its reclaim; last %+v err %v", got.Meta, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(3 * fixtureutil.TunedRetryDelay)
	if n := len(stall2.Received(t)); n != 1 {
		t.Errorf("R2's unsafe processor reached the first process's client %d times; want 1", n)
	}
	if n := len(fresh2.Received(t)); n != 0 {
		t.Errorf("R2's unsafe processor was sent %d times after the restart; want 0", n)
	}
	if n := len(stall1.Received(t)); n != 1 {
		t.Errorf("R1's processor reached the first process's client %d times; want 1", n)
	}
}

// restartWorkflow is Open -[Fire, 300ms]-> Done with one processor on tag.
func restartWorkflow(wfName, tag string, idempotent bool) string {
	return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"` + wfName + `","initialState":"Open","active":true,"states":{` +
		`"Open":{"transitions":[{"name":"Fire","next":"Done","manual":false,"schedule":{"delayMs":300},"processors":[` +
		`{"type":"calculator","name":"noop","executionMode":"SYNC","config":{"attachEntity":true,"calculationNodesTags":"` + tag +
		`","retryPolicy":"NONE","responseTimeoutMs":240000,"idempotent":` + boolJSON(idempotent) + `}}]}]},"Done":{}}}]}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
