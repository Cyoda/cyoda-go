package grpc

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newTestEnvWithWorkflow mirrors newTestEnv but also returns a
// *workflow.Handler wired to the same StoreFactory/Engine. The gRPC surface
// (EntityModelManage) has no RPC for workflow import — only the HTTP route
// `POST /api/model/{name}/{version}/workflow/import` (internal/domain/workflow.Handler)
// — so tests that need a custom workflow (e.g. one with a scheduled
// transition) drive that Handler directly via httptest, mirroring
// internal/e2e's importWorkflowE2E without standing up the HTTP router.
func newTestEnvWithWorkflow(t *testing.T) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
	t.Helper()

	factory := memory.NewStoreFactory()
	factory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := factory.GetTransactionManager()

	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: "test-tenant", Name: "Test Tenant"},
		Roles:    []string{"ADMIN"},
	}

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchService)
	modelHandler := model.New(factory)
	workflowHandler := workflow.New(factory, engine)

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	ctx := spi.WithUserContext(context.Background(), uc)
	return svc, workflowHandler, ctx
}

// setupScheduledWorkflowRPCEnv imports and locks entityName/1, then imports
// workflowJSON via workflow.Handler.ImportEntityModelWorkflow directly
// (bypassing the HTTP router, which package grpc's unit tests don't stand
// up). Fails the test unless both steps report success.
func setupScheduledWorkflowRPCEnv(t *testing.T, svc *CloudEventsServiceImpl, wfHandler *workflow.Handler, ctx context.Context, entityName, workflowJSON string) {
	t.Helper()
	importAndLockModel(t, svc, ctx, entityName, "1",
		map[string]any{"name": "Test Order", "amount": 100, "status": "draft"})

	req := httptest.NewRequest(http.MethodPost, "/api/model/"+entityName+"/1/workflow/import",
		bytes.NewReader([]byte(workflowJSON))).WithContext(ctx)
	rec := httptest.NewRecorder()
	wfHandler.ImportEntityModelWorkflow(rec, req, entityName, 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("workflow import for %s: expected 200, got %d: %s", entityName, rec.Code, rec.Body.String())
	}
}

// TestRPC_EntityTransition_ScheduledTransitionRejected drives ManualTransition
// through the gRPC entry point (EntityManage/EntityTransitionRequest) for a
// transition that is scheduled (Schedule.DelayMs > 0, manual=false) rather
// than manually fireable. Mirrors
// TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound
// (internal/e2e/scheduled_transition_test.go) but proves the rejection also
// flows through the gRPC surface, not just HTTP (design §10, §11 "G" column,
// #251).
//
// At the gRPC envelope layer, operational AppErrors are reported as
// Error.Code == "CLIENT_ERROR" with the domain error code embedded as a
// "CODE: detail" prefix in Error.Message — this is the established pattern
// for every other domain rejection in this file (see
// TestRPC_EntityCreate_UniqueViolation, TestRPC_EntityCreate_InvalidUniqueKey),
// so this test follows the same shape rather than asserting
// Error.Code == "TRANSITION_NOT_FOUND" directly.
func TestRPC_EntityTransition_ScheduledTransitionRejected(t *testing.T) {
	const modelName = "grpc-scheduled-explicit-fire"
	svc, wfHandler, ctx := newTestEnvWithWorkflow(t)

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-explicit-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 1000}}]},
				"Closed": {}
			}
		}]
	}`
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	// Create entity instance. Cascade silently skips the scheduled
	// transition (no other automated exit), so the entity rests in Open.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	// Fire AutoClose by name through the gRPC ManualTransition path. Expect
	// Success=false, CLIENT_ERROR envelope, TRANSITION_NOT_FOUND embedded in
	// the message, and the C4 rejection-cause wording.
	transitionCE := makeCE(EntityTransitionRequest, map[string]any{
		"id":         "transition-1",
		"entityId":   entityID,
		"transition": "AutoClose",
	})
	resp, err := svc.EntityManage(ctx, transitionCE)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}

	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected explicit fire of scheduled transition to fail")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "TRANSITION_NOT_FOUND") {
		t.Errorf("expected message to contain TRANSITION_NOT_FOUND, got %s", typed.Error.Message)
	}
	const wantCause = "scheduled and fires automatically"
	if !strings.Contains(typed.Error.Message, wantCause) {
		t.Errorf("expected message to contain %q, got %s", wantCause, typed.Error.Message)
	}

	// Entity must remain in the source state after rejection.
	envelope, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("failed to re-fetch entity: %v", err)
	}
	if state, _ := envelope.Meta["state"].(string); state != "Open" {
		t.Errorf("expected entity to remain in Open after rejected explicit-fire; got %q", state)
	}
}
