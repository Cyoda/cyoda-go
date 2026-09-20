package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// captureSlog swaps slog.Default with a JSON handler writing to buf at the
// given level for the duration of the test.
func captureSlog(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

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
func (r *fakeRegistry) Changed() <-chan struct{}                          { return nil }

var _ contract.NodeRegistry = (*fakeRegistry)(nil)

var testSecret32 = bytes.Repeat([]byte{0xCD}, 32)

// newTestAuth builds the AEADPeerAuth both ends of the scheduler RPC share in
// these tests. Mirrors dispatch's own newAEAD test helper.
func newTestAuth(t *testing.T) *dispatch.AEADPeerAuth {
	t.Helper()
	auth, err := dispatch.NewAEADPeerAuth(testSecret32, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	return auth
}

// signedSchedulerRequest builds a peer-authenticated scheduled-task request
// ready for the handler to verify, and returns the binding its answer opens
// under.
func signedSchedulerRequest(t *testing.T, auth dispatch.PeerAuth, task spi.ScheduledTask) (*http.Request, dispatch.ResponseBinding) {
	t.Helper()
	plain, err := json.Marshal(SchedulerTaskRequest{Task: task})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, schedulerTaskPath, nil)
	wire, binding, err := auth.Sign(req, plain)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req, binding
}

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

// TestExecutor_MisWiredNonSelfTarget_WarnsThenFiresLocally proves a mis-wire
// (registry or client nil, e.g. a deployment that forgot to pass the
// cluster's NodeRegistry/SchedulerRPCClient into NewClusterExecutor) that
// silently defeats the distribution strategy is observable: Execute still
// fires locally (fail-toward-runs — a due task is never dropped) but emits a
// slog.Warn naming the taskId and target so an operator can see the
// distribution never happened. A self/empty target is the expected
// single-node shape and must NOT warn.
func TestExecutor_MisWiredNonSelfTarget_WarnsThenFiresLocally(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, _ := setupRealEngine(t, nowMs)
	schedEngine := NewSchedulerEngine(engine)

	t.Run("nil registry and client, non-self target warns", func(t *testing.T) {
		buf := captureSlog(t, slog.LevelWarn)
		exec := NewClusterExecutor(schedEngine, "self-node", nil, nil)
		exec.Execute(context.Background(), spi.ScheduledTask{ID: "mis-wired-task", TenantID: testTenant}, "other-node")

		out := buf.String()
		if !strings.Contains(out, "mis-wired-task") || !strings.Contains(out, "other-node") {
			t.Errorf("expected a slog.Warn naming taskId=mis-wired-task and target=other-node, got: %s", out)
		}
	})

	t.Run("self target does not warn", func(t *testing.T) {
		buf := captureSlog(t, slog.LevelWarn)
		exec := NewClusterExecutor(schedEngine, "self-node", nil, nil)
		exec.Execute(context.Background(), spi.ScheduledTask{ID: "self-task", TenantID: testTenant}, "self-node")

		if buf.Len() != 0 {
			t.Errorf("expected no warning for a self target, got: %s", buf.String())
		}
	})

	t.Run("empty target does not warn", func(t *testing.T) {
		buf := captureSlog(t, slog.LevelWarn)
		exec := NewClusterExecutor(schedEngine, "self-node", nil, nil)
		exec.Execute(context.Background(), spi.ScheduledTask{ID: "empty-target-task", TenantID: testTenant}, "")

		if buf.Len() != 0 {
			t.Errorf("expected no warning for an empty target, got: %s", buf.String())
		}
	})
}

// TestSchedulerRPCClient_PlaintextAnswerRefused proves the coordinator trusts
// only an answer sealed for the request it sent. An unsealed `{"success":true}`
// from a peer that never ran the task — anyone on the network path can write
// one without holding the cluster key — must not be read as a fire: the
// coordinator treats a reported success as the task having run and drops it, so
// the task would silently never run.
func TestSchedulerRPCClient_PlaintextAnswerRefused(t *testing.T) {
	auth := newTestAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, _, err := auth.Verify(r); err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)

	client := NewSchedulerRPCClient(newTestAuth(t), 5*time.Second).AllowLoopbackForTesting()
	err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "t-plain", TenantID: testTenant})
	if err == nil {
		t.Fatal("an answer that was not sealed was accepted as a fire")
	}
	if !strings.Contains(err.Error(), "open scheduler response") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// TestSchedulerRPCClient_AnswerSealedForAnotherRequestRefused proves an answer
// sealed for a different request does not answer this one: a recorded sealed
// success replayed onto a later call is refused.
func TestSchedulerRPCClient_AnswerSealedForAnotherRequestRefused(t *testing.T) {
	auth := newTestAuth(t)
	var mu sync.Mutex
	var firstWire []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		wire, err := auth.SealResponse(w.Header(), binding, []byte(`{"success":true}`))
		if err != nil {
			t.Errorf("SealResponse: %v", err)
			return
		}
		func() {
			mu.Lock()
			defer mu.Unlock()
			if firstWire == nil {
				firstWire = wire
			}
			wire = firstWire // every later request is answered with the first answer
		}()
		_, _ = w.Write(wire)
	}))
	t.Cleanup(srv.Close)

	client := NewSchedulerRPCClient(newTestAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "t-1", TenantID: testTenant}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "t-2", TenantID: testTenant})
	if err == nil {
		t.Fatal("an answer replayed from an earlier request was accepted")
	}
	if !strings.Contains(err.Error(), "open scheduler response") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// TestSchedulerRPCClient_TruncatedAnswerRefused proves a sealed answer whose
// last byte is missing — long enough to pass the envelope's length floor, so it
// is the authentication tag that refuses it — is not read as a fire.
func TestSchedulerRPCClient_TruncatedAnswerRefused(t *testing.T) {
	auth := newTestAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		wire, err := auth.SealResponse(w.Header(), binding, []byte(`{"success":true}`))
		if err != nil {
			t.Errorf("SealResponse: %v", err)
			return
		}
		_, _ = w.Write(wire[:len(wire)-1])
	}))
	t.Cleanup(srv.Close)

	client := NewSchedulerRPCClient(newTestAuth(t), 5*time.Second).AllowLoopbackForTesting()
	err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "t-trunc", TenantID: testTenant})
	if err == nil {
		t.Fatal("a truncated answer was accepted")
	}
	if !strings.Contains(err.Error(), "open scheduler response") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// TestSchedulerRPCClient_FollowsNoRedirect: a 3xx on the path between nodes
// must not send the signed task on to an address that never passed the
// peer-address guard. Every redirect is refused, whatever its status.
func TestSchedulerRPCClient_FollowsNoRedirect(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var reached bool
			elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(elsewhere.Close)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+schedulerTaskPath, status)
			}))
			t.Cleanup(srv.Close)

			client := NewSchedulerRPCClient(newTestAuth(t), 5*time.Second).AllowLoopbackForTesting()
			if err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "t-redirect", TenantID: testTenant}); err == nil {
				t.Fatal("a redirected task was accepted as a fire")
			}
			if reached {
				t.Error("the task was sent on to an address that never passed the peer-address guard")
			}
		})
	}
}

// TestSchedulerRPC_SealedRoundTrip proves the real handler and the real client
// agree on the sealed answer over a real HTTP round trip: a fire that ran
// arrives as success, and one the peer's engine refused arrives as that
// failure's sanitized error rather than as a lost answer.
func TestSchedulerRPC_SealedRoundTrip(t *testing.T) {
	serve := func(t *testing.T, fake *fakeSchedEngine) *httptest.Server {
		t.Helper()
		mux := http.NewServeMux()
		NewSchedulerRPCHandler(fake, newTestAuth(t)).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("success", func(t *testing.T) {
		fake := &fakeSchedEngine{outcome: "fired"}
		srv := serve(t, fake)
		client := NewSchedulerRPCClient(newTestAuth(t), 5*time.Second).AllowLoopbackForTesting()

		if err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "rt-ok", TenantID: testTenant}); err != nil {
			t.Fatalf("ExecuteScheduledTask: %v", err)
		}
		if fake.calls != 1 || fake.gotTask.ID != "rt-ok" {
			t.Errorf("peer engine calls = %d, task = %q", fake.calls, fake.gotTask.ID)
		}
	})

	t.Run("application failure", func(t *testing.T) {
		fake := &fakeSchedEngine{err: errors.New("engine said no")}
		srv := serve(t, fake)
		client := NewSchedulerRPCClient(newTestAuth(t), 5*time.Second).AllowLoopbackForTesting()

		err := client.ExecuteScheduledTask(context.Background(), srv.URL, spi.ScheduledTask{ID: "rt-fail", TenantID: testTenant})
		if err == nil {
			t.Fatal("expected the peer's reported failure to reach the coordinator")
		}
		if !strings.Contains(err.Error(), "scheduled task fire failed") {
			t.Errorf("the peer's sanitized failure did not arrive intact: %v", err)
		}
		if strings.Contains(err.Error(), "engine said no") {
			t.Errorf("the peer leaked its internal error: %v", err)
		}
	})
}

// TestSchedulerRPCHandler_AnswerIsSealedForItsRequest proves the handler's
// answer to a verified request is on the wire under seal — the sealed content
// type, bytes that are neither readable JSON nor carry the plaintext marker —
// and opens only under that request's binding.
func TestSchedulerRPCHandler_AnswerIsSealedForItsRequest(t *testing.T) {
	auth := newTestAuth(t)
	fake := &fakeSchedEngine{outcome: "fired"}
	mux := http.NewServeMux()
	NewSchedulerRPCHandler(fake, auth).Register(mux)

	req, binding := signedSchedulerRequest(t, auth, spi.ScheduledTask{ID: "sealed-task", TenantID: testTenant})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != dispatch.DispatchContentType {
		t.Errorf("Content-Type = %q, want %q", got, dispatch.DispatchContentType)
	}
	wire := rec.Body.Bytes()
	if json.Valid(wire) {
		t.Errorf("the answer is readable JSON on the wire: %s", wire)
	}
	if bytes.Contains(wire, []byte("success")) {
		t.Error("the answer carries its plaintext marker on the wire")
	}

	plain, err := auth.OpenResponse(rec.Header(), binding, wire)
	if err != nil {
		t.Fatalf("the answer does not open under its request's binding: %v", err)
	}
	var resp SchedulerTaskResponse
	if err := json.Unmarshal(plain, &resp); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false, want true: %+v", resp)
	}
}
