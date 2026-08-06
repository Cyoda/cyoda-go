package grpc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestUnaryRecoveryInterceptor_RecoversAndMarksUnhealthy(t *testing.T) {
	var health atomic.Bool
	health.Store(true)

	interceptor := UnaryRecoveryInterceptor(&health)
	handler := func(ctx context.Context, req any) (any, error) { panic("injected") }

	resp, err := interceptor(context.Background(), nil,
		&googlegrpc.UnaryServerInfo{FullMethod: "/cyoda.CloudEventsService/Test"}, handler)

	if err == nil {
		t.Fatal("panic was not converted to an error; the process would have died")
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if health.Load() {
		t.Fatal("health flag not marked; a node that has panicked has unknown state")
	}
	if strings.Contains(err.Error(), "injected") {
		t.Fatal("panic value leaked to the client")
	}
}

func TestStreamRecoveryInterceptor_RecoversAndMarksUnhealthy(t *testing.T) {
	var health atomic.Bool
	health.Store(true)

	interceptor := StreamRecoveryInterceptor(&health)
	handler := func(srv any, ss googlegrpc.ServerStream) error { panic("injected") }

	err := interceptor(nil, nil,
		&googlegrpc.StreamServerInfo{FullMethod: "/cyoda.CloudEventsService/Stream"}, handler)

	if err == nil {
		t.Fatal("panic was not converted to an error; the process would have died")
	}
	if health.Load() {
		t.Fatal("health flag not marked")
	}
	if strings.Contains(err.Error(), "injected") {
		t.Fatal("panic value leaked to the client")
	}
}

// TestUnaryRecoveryInterceptor_NilHealthFlag proves the nil-healthFlag guard:
// test constructors (and any future caller) that pass nil must not panic on
// the Store call inside the panic handler itself — that would defeat the
// entire point of the interceptor.
func TestUnaryRecoveryInterceptor_NilHealthFlag(t *testing.T) {
	interceptor := UnaryRecoveryInterceptor(nil)
	handler := func(ctx context.Context, req any) (any, error) { panic("injected") }

	resp, err := interceptor(context.Background(), nil,
		&googlegrpc.UnaryServerInfo{FullMethod: "/cyoda.CloudEventsService/Test"}, handler)

	if err == nil {
		t.Fatal("panic was not converted to an error")
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
}

// --- real network round trip -----------------------------------------------
//
// TestServer_HandlerPanic_ProcessSurvives drives a real gRPC round trip
// against a server built with NewServer, so the recovery interceptor is
// proven to be WIRED, not merely to exist: without it this whole test binary
// would die on the panic rather than report a failure.
//
// The panic is triggered by a genuine production code path — a FUNCTION
// criterion whose callback panics. internal/testing/localproc's
// DispatchProcessor and DispatchFunction recover panics and convert them to
// errors; only DispatchCriteria has none, so a panicking criterion is the one
// callout that reaches the handler (and from there, the interceptor) intact.
// This single scenario also covers coverage row 7's transaction half: the
// panic unwinds through the deferred rollback Tasks 1-5 built, so by the time
// it reaches this interceptor the transaction it opened must already be gone.
func TestServer_HandlerPanic_ProcessSurvives(t *testing.T) {
	var health atomic.Bool
	health.Store(true)

	env := startRecoveryTestServer(t, &health)

	// Import and lock a model with an automated transition out of its initial
	// state, guarded by a FUNCTION criterion that panics.
	env.importAndLockModel(t, "widget-panic", "1", map[string]any{"name": "w"})
	env.saveWorkflow(t, spi.ModelRef{EntityName: "widget-panic", ModelVersion: "1"}, spi.WorkflowDefinition{
		Version: "1.1", Name: "PanicGateWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{
				Name: "gate", Next: "B",
				Criterion: json.RawMessage(`{"type":"function","function":{"name":"boom"}}`),
			}}},
			"B": {},
		},
	})
	env.localProc.RegisterCriteria("boom", func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
		panic("criterion callout panicked: boom")
	})

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "widget-panic", "version": 1},
			"data":  map[string]any{"name": "w"},
		},
	})

	resp, err := env.client.EntityManage(env.authedCtx(t), createCE)
	if err == nil {
		t.Fatal("expected an error from the recovered panic")
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil — a recovered panic must not carry a partial response", resp)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Fatal("panic value leaked to the client")
	}
	if health.Load() {
		t.Fatal("health flag not marked through the real server path")
	}

	if open := env.tracker.openTxIDs(); len(open) != 0 {
		t.Fatalf("panic through the gRPC door leaked %d transaction(s): %v", len(open), open)
	}

	// The server is still answering: the panic did not take the process down.
	listCE := makeCE(EntityModelGetAllRequest, map[string]any{"id": "test-2"})
	listResp, err := env.client.EntityModelManage(env.authedCtx(t), listCE)
	if err != nil {
		t.Fatalf("server stopped serving after a recovered panic: %v", err)
	}
	var typed events.EntityModelGetAllResponseJson
	validateResponse(t, listResp, &typed)
	if !typed.Success {
		t.Fatal("expected the follow-up request to succeed")
	}
}

// --- test harness ------------------------------------------------------

// recoveryTestServer bundles a real, network-listening gRPC server (built via
// the production NewServer constructor) with the pieces a test needs to
// drive it and observe transaction state afterward.
type recoveryTestServer struct {
	client    cyodapb.CloudEventsServiceClient
	uc        *spi.UserContext
	localProc *localproc.LocalProcessingService
	tracker   *recoveryTrackingTxMgr
	raw       spi.StoreFactory
}

// authedCtx returns a context carrying an authorization header the test's
// fake AuthenticationService accepts unconditionally.
func (env *recoveryTestServer) authedCtx(t *testing.T) context.Context {
	t.Helper()
	md := metadata.Pairs("authorization", "Bearer test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return metadata.NewOutgoingContext(ctx, md)
}

func (env *recoveryTestServer) importAndLockModel(t *testing.T, entityName, version string, sampleData map[string]any) {
	t.Helper()
	ctx := spi.WithUserContext(context.Background(), env.uc)
	dataBytes, err := json.Marshal(sampleData)
	if err != nil {
		t.Fatalf("marshal sample data: %v", err)
	}
	mh := model.New(env.raw)
	if _, err := mh.ImportModel(ctx, model.ImportModelInput{
		EntityName:   entityName,
		ModelVersion: version,
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	}); err != nil {
		t.Fatalf("import model: %v", err)
	}
	if _, err := mh.LockModel(ctx, entityName, version); err != nil {
		t.Fatalf("lock model: %v", err)
	}
}

func (env *recoveryTestServer) saveWorkflow(t *testing.T, ref spi.ModelRef, wf spi.WorkflowDefinition) {
	t.Helper()
	ctx := spi.WithUserContext(context.Background(), env.uc)
	ws, err := env.raw.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := ws.Save(ctx, ref, []spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}
}

// recoveryTrackingTxMgr wraps a real TransactionManager to record which
// transactions were opened, so a test can ask whether any is still open after
// a panic — mirroring internal/domain/entity/service_rollback_test.go's
// trackingTxMgr. Join is the only SPI-level, non-destructive liveness probe:
// it succeeds only while the plugin still holds the transaction active.
type recoveryTrackingTxMgr struct {
	spi.TransactionManager

	mu    sync.Mutex
	begun []string
}

func (m *recoveryTrackingTxMgr) Begin(ctx context.Context) (string, context.Context, error) {
	txID, txCtx, err := m.TransactionManager.Begin(ctx)
	if err != nil {
		return txID, txCtx, err
	}
	m.mu.Lock()
	m.begun = append(m.begun, txID)
	m.mu.Unlock()
	return txID, txCtx, nil
}

func (m *recoveryTrackingTxMgr) openTxIDs() []string {
	m.mu.Lock()
	ids := make([]string, len(m.begun))
	copy(ids, m.begun)
	m.mu.Unlock()

	probeCtx := context.Background()
	var open []string
	for _, id := range ids {
		if _, err := m.TransactionManager.Join(probeCtx, id); err == nil {
			open = append(open, id)
		}
	}
	return open
}

// fixedAuthService is a contract.AuthenticationService test double that
// authenticates any request as the same fixed principal, regardless of the
// bearer token presented. Real JWT validation is exercised elsewhere
// (internal/auth); this test is only about panic recovery.
type fixedAuthService struct {
	uc *spi.UserContext
}

func (a *fixedAuthService) Authenticate(context.Context, *http.Request) (*spi.UserContext, error) {
	return a.uc, nil
}

// startRecoveryTestServer builds a real, network-listening gRPC server wired
// with an in-memory backend and a localproc external-processing service, so a
// test can drive genuine production code paths (including panics deep inside
// workflow criterion evaluation) over an actual gRPC connection.
func startRecoveryTestServer(t *testing.T, healthFlag *atomic.Bool) *recoveryTestServer {
	t.Helper()

	factory := memory.NewStoreFactory()
	t.Cleanup(func() { _ = factory.Close() })

	realTxMgr, err := factory.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	tracker := &recoveryTrackingTxMgr{TransactionManager: realTxMgr}

	localProc := localproc.New()
	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), tracker,
		wfengine.WithExternalProcessing(localProc))

	searchStore, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	searchSvc := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, tracker, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchSvc)
	modelHandler := model.New(factory)

	uc := &spi.UserContext{
		UserID:   "recovery-test-user",
		UserName: "Recovery Test",
		Tenant:   spi.Tenant{ID: "recovery-tenant", Name: "Recovery Tenant"},
		Roles:    []string{"ADMIN"},
	}
	authSvc := &fixedAuthService{uc: uc}

	tokenSigner, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("token.NewSigner: %v", err)
	}

	srv := NewServer(authSvc, NewMemberRegistry(), tracker, entityHandler, modelHandler, searchSvc,
		tokenSigner, nil /* nodeRegistry: unused, no tx-token sent */, "recovery-test-node",
		false, 0, true, healthFlag)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &recoveryTestServer{
		client:    cyodapb.NewCloudEventsServiceClient(conn),
		uc:        uc,
		localProc: localProc,
		tracker:   tracker,
		raw:       factory,
	}
}
