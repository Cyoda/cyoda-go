package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const testTenant = spi.TenantID("cluster-sched-tenant")

func ctxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{UserID: "test-user", Tenant: spi.Tenant{ID: tid}, Roles: []string{"USER"}}
	return spi.WithUserContext(context.Background(), uc)
}

// setupRealEngine builds a real memory-backed workflow.Engine pinned to a
// fixed clock, for tests that need to prove a fire actually happened
// (entity state advanced), not just that a mock was called.
func setupRealEngine(t *testing.T, nowMs int64) (*workflow.Engine, spi.StoreFactory) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	at := time.UnixMilli(nowMs)
	engine := workflow.NewEngine(factory, uuids, txMgr, workflow.WithScheduledClock(func() time.Time { return at }))
	return engine, factory
}

// TestExecutor_LocalWhenSelf proves ClusterExecutor's target==selfID branch
// really fires the transition — not just that some function was invoked —
// by wiring a real memory-backed engine and asserting the entity's state
// advanced and the task row was resolved.
func TestExecutor_LocalWhenSelf(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupRealEngine(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sched-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "SchedWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"CLOSED": {},
		},
	}
	ws, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := ws.Save(ctx, modelRef, []spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("save workflow: %v", err)
	}

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", TenantID: testTenant, ModelRef: modelRef, State: "OPEN", TransactionID: "seed-tx"},
		Data: []byte(`{}`),
	}
	if _, err := es.Save(ctx, entity); err != nil {
		t.Fatalf("save entity: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	const taskID = "e1:OPEN:AutoClose"
	if err := sts.Upsert(ctx, spi.ScheduledTask{
		ID: taskID, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: nowMs, EntityID: "e1", Transition: "AutoClose", SourceState: "OPEN",
		ArmedAt: nowMs - 1000,
	}); err != nil {
		t.Fatalf("arm task: %v", err)
	}

	exec := NewClusterExecutor(NewSchedulerEngine(engine), "self-node", nil, nil)
	exec.Execute(context.Background(), spi.ScheduledTask{ID: taskID, TenantID: testTenant}, "self-node")

	got, err := es.Get(ctx, "e1")
	if err != nil {
		t.Fatalf("re-read entity: %v", err)
	}
	if got.Meta.State != "CLOSED" {
		t.Fatalf("entity state = %q, want CLOSED — local fire did not happen", got.Meta.State)
	}
	if _, found, _ := sts.Get(ctx, taskID); found {
		t.Error("expected the scheduled task row resolved (deleted) after firing")
	}
}

// fakeSchedEngine is a scheduler.Engine test double for the peer-RPC path,
// recording what it was called with.
type fakeSchedEngine struct {
	calls   int
	gotTask spi.ScheduledTask
	gotCtx  context.Context
	outcome string
	err     error
}

func (f *fakeSchedEngine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (string, error) {
	f.calls++
	f.gotTask = task
	f.gotCtx = ctx
	return f.outcome, f.err
}

var _ scheduler.Engine = (*fakeSchedEngine)(nil)

// fakeRegistry implements contract.NodeRegistry with a single resolvable
// peer address, for tests that don't need real cluster membership.
type fakeRegistry struct {
	addr  string
	alive bool
}

func (r *fakeRegistry) Register(context.Context, string, string) error { return nil }
func (r *fakeRegistry) Lookup(context.Context, string) (string, bool, error) {
	return r.addr, r.alive, nil
}
func (r *fakeRegistry) List(context.Context) ([]contract.NodeInfo, error) { return nil, nil }
func (r *fakeRegistry) Deregister(context.Context, string) error          { return nil }

var _ contract.NodeRegistry = (*fakeRegistry)(nil)

var testSecret32 = bytes.Repeat([]byte{0xCD}, 32)

// TestExecutor_ForwardsWithPeerAuth proves a non-self target is forwarded
// over the PeerAuth-authenticated channel end-to-end (real HTTP round trip
// via httptest, real AEAD signing/verification) and that an unauthenticated
// call to the same server handler is rejected — no unauthenticated path
// exists to reach the engine.
func TestExecutor_ForwardsWithPeerAuth(t *testing.T) {
	auth, err := dispatch.NewAEADPeerAuth(testSecret32, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	fake := &fakeSchedEngine{outcome: "fired"}
	handler := NewSchedulerRPCHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := NewSchedulerRPCClient(auth, 5*time.Second).AllowLoopbackForTesting()
	registry := &fakeRegistry{addr: srv.URL, alive: true}

	exec := NewClusterExecutor(fake, "self-node", registry, client)

	task := spi.ScheduledTask{ID: "peer-task-1", TenantID: spi.TenantID("tenant-y"), EntityID: "e9"}
	exec.Execute(context.Background(), task, "peer-node")

	if fake.calls != 1 {
		t.Fatalf("expected the peer's engine to be invoked once via the authenticated RPC, got %d", fake.calls)
	}
	if fake.gotTask.ID != "peer-task-1" {
		t.Errorf("forwarded task mismatch: got %+v", fake.gotTask)
	}
	uc := spi.GetUserContext(fake.gotCtx)
	if uc == nil {
		t.Fatal("server handler did not build a UserContext for the fire")
	}
	if uc.Tenant.ID != "tenant-y" {
		t.Errorf("server handler's context tenant = %q, want %q", uc.Tenant.ID, "tenant-y")
	}
}

// TestSchedulerRPCHandler_RejectsUnauthenticated proves the server handler
// is gated by PeerAuth.Verify — a plain, unsigned POST is rejected with 403
// and never reaches the engine. This is the "no new unauthenticated
// surface" guarantee (Gate 3): the scheduled-task RPC rides the identical
// authentication as processor/criteria dispatch.
func TestSchedulerRPCHandler_RejectsUnauthenticated(t *testing.T) {
	auth, err := dispatch.NewAEADPeerAuth(testSecret32, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	fake := &fakeSchedEngine{}
	handler := NewSchedulerRPCHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	body, _ := json.Marshal(SchedulerTaskRequest{Task: spi.ScheduledTask{ID: "x", TenantID: "t"}})
	req := httptest.NewRequest(http.MethodPost, schedulerTaskPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately not signed via auth.Sign — no AEAD envelope, no
	// timestamp header.

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unauthenticated call, got %d: %s", rec.Code, rec.Body.String())
	}
	if fake.calls != 0 {
		t.Error("engine must not be invoked for an unauthenticated request")
	}
}

// TestExecutor_PeerLookupFailure_DropsWithoutLocalFallback proves a
// dispatch to an unresolvable peer does NOT silently fall back to firing
// locally (which would defeat the coordinator's chosen distribution
// target) — it drops, relying on the scan loop's at-least-once redispatch.
func TestExecutor_PeerLookupFailure_DropsWithoutLocalFallback(t *testing.T) {
	fake := &fakeSchedEngine{}
	registry := &fakeRegistry{addr: "", alive: false}
	auth, err := dispatch.NewAEADPeerAuth(testSecret32, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	client := NewSchedulerRPCClient(auth, time.Second).AllowLoopbackForTesting()

	exec := NewClusterExecutor(fake, "self-node", registry, client)
	exec.Execute(context.Background(), spi.ScheduledTask{ID: "orphan-task", TenantID: "t"}, "unreachable-peer")

	if fake.calls != 0 {
		t.Error("an unresolvable peer target must not fall back to a local fire")
	}
}
