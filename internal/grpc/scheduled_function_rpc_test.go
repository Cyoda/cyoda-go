package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// scheduled_function_rpc_test.go is Task 9.3's gRPC envelope coverage layer
// for the scheduled-transition Function feature (issue #419): it proves the
// error classes tasks 9.1/9.2 already cover at the HTTP entrypoint
// (internal/e2e/scheduled_function_test.go) surface with the SAME envelope
// shape (Success=false, Error.Code, Error.Message) through the gRPC
// EntityManage entrypoint too — HTTP and gRPC are separate entry points
// (.claude/rules/test-coverage.md) and neither is a proxy for the other.
//
// scheduled_transition_test.go's newTestEnvWithWorkflow/setupScheduledWorkflowRPCEnv
// wire NO ProcessorDispatcher at all (workflow.NewEngine with no
// WithExternalProcessing option), which is sufficient for that file's
// static-delayMs scenarios but cannot exercise a real compute-node dispatch.
// This file adds newTestEnvWithDispatch, which wires a real
// ProcessorDispatcher (the same production type cmd/cyoda/app.go wires) to
// the returned CloudEventsServiceImpl's OWN MemberRegistry, so a test can
// register (or deliberately omit) a gRPC compute member and observe the
// resulting dispatch outcome through the full envelope — exactly like the
// ProcessorDispatcher-level unit tests in dispatch_test.go, but one layer
// up, through svc.EntityManage.

// newTestEnvWithDispatch is newTestEnvWithWorkflow (scheduled_transition_test.go)
// extended with a real ProcessorDispatcher wired to the returned service's
// own MemberRegistry via workflow.WithExternalProcessing, so schedule.function/
// processor/criterion dispatch actually runs (rather than failing with "no
// external processing configured") and tests can register a member to answer
// requests, or leave the registry empty to exercise the no-member path.
func newTestEnvWithDispatch(t *testing.T) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
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

	registry := NewMemberRegistry()
	signer, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("token.NewSigner: %v", err)
	}
	dispatcher := NewProcessorDispatcher(registry, common.NewDefaultUUIDGenerator(), signer, "node-test", time.Minute)

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr, workflow.WithExternalProcessing(dispatcher))
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchService)
	modelHandler := model.New(factory)
	workflowHandler := workflow.New(factory, engine)

	svc := &CloudEventsServiceImpl{
		registry:      registry,
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	ctx := spi.WithUserContext(context.Background(), uc)
	return svc, workflowHandler, ctx
}

// testTenant is the tenant ID every test in this file registers gRPC
// compute members under — it must match newTestEnvWithDispatch's
// spi.UserContext.Tenant.ID ("test-tenant") for MemberRegistry.FindByTags
// to resolve them.
const testTenant = spi.TenantID("test-tenant")

// scheduleFunctionRPCWorkflowJSON builds a REPLACE-mode workflow import
// payload (schema version 1.3 — schedule.function requires it) with a single
// non-manual Open -[AutoClose]-> Closed transition carrying fnJSON as the
// raw "function" object literal.
func scheduleFunctionRPCWorkflowJSON(wfName, fnJSON string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": %q, "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": {"function": %s}
				}]},
				"Closed": {}
			}
		}]
	}`, wfName, fnJSON)
}

// schedFnConfigJSON builds a shape-valid schedule.function config literal.
// responseTimeoutMs of 0 omits the field (dispatch uses its own default).
func schedFnConfigJSON(name, tags string, responseTimeoutMs int64) string {
	if responseTimeoutMs > 0 {
		return fmt.Sprintf(`{"name":%q,"resultKind":"Schedule","calculationNodesTags":%q,"attachEntity":true,"responseTimeoutMs":%d}`,
			name, tags, responseTimeoutMs)
	}
	return fmt.Sprintf(`{"name":%q,"resultKind":"Schedule","calculationNodesTags":%q,"attachEntity":true}`, name, tags)
}

// createScheduledEntity issues the gRPC EntityCreateRequest that arms
// modelName's schedule.function transition and returns the decoded typed
// envelope.
func createScheduledEntity(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, modelName string) events.EntityTransactionResponseJson {
	t.Helper()
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	})
	resp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	return typed
}

// assertClientErrorEnvelope asserts the standard operational-AppError
// envelope shape (Error.Code == "CLIENT_ERROR", message contains wantCode)
// shared by every 4xx/503 domain rejection in this package (see
// TestRPC_EntityTransition_ScheduledTransitionRejected's doc comment).
func assertClientErrorEnvelope(t *testing.T, typed events.EntityTransactionResponseJson, wantCode string) {
	t.Helper()
	if typed.Success {
		t.Fatal("expected the operation to fail")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, wantCode) {
		t.Errorf("expected message to contain %s, got %s", wantCode, typed.Error.Message)
	}
}

// --- schedule.function error surfaces ---

// TestRPC_ScheduledFunction_Import_ValidationFailed proves a structurally
// invalid schedule.function shape (delayMs and function both set) is
// rejected at import — the same 400 VALIDATION_FAILED gate
// internal/e2e/scheduled_function_test.go's
// TestScheduledFunction_Import_DelayMsAndFunctionBothSet_400 proves over
// HTTP — reproduced here directly against workflow.Handler (the gRPC
// surface has no workflow-import RPC of its own; see
// newTestEnvWithWorkflow's doc comment in scheduled_transition_test.go), so
// this package's own dispatch-capable environment (newTestEnvWithDispatch)
// is exercised against the identical validation gate too.
func TestRPC_ScheduledFunction_Import_ValidationFailed(t *testing.T) {
	const modelName = "grpc-schedfn-import-invalid"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	importAndLockModel(t, svc, ctx, modelName, "1",
		map[string]any{"name": "Test Order", "amount": 100, "status": "draft"})

	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": "schedfn-invalid-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": {"delayMs": 1000, "function": %s}
				}]},
				"Closed": {}
			}
		}]
	}`, schedFnConfigJSON("calcFire", "some-tag", 0))

	req := httptest.NewRequest(http.MethodPost, "/api/model/"+modelName+"/1/workflow/import",
		bytes.NewReader([]byte(wf))).WithContext(ctx)
	rec := httptest.NewRecorder()
	wfHandler.ImportEntityModelWorkflow(rec, req, modelName, 1)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for delayMs+function both set, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_FAILED") {
		t.Errorf("expected VALIDATION_FAILED in body, got: %s", rec.Body.String())
	}
}

// TestRPC_ScheduledFunction_NoMember_Returns503Envelope proves an armed
// schedule.function whose calculationNodesTags match no connected gRPC
// compute member surfaces the Phase-2 uniform-503 NO_COMPUTE_MEMBER_FOR_TAG
// classification through the gRPC EntityManage envelope — the same
// classifyWorkflowError passthrough internal/e2e/dispatch_infra_error_test.go
// proves over HTTP, reproduced here against the gRPC entrypoint (issue #419
// design's "G" coverage column: HTTP and gRPC are separate entry points).
func TestRPC_ScheduledFunction_NoMember_Returns503Envelope(t *testing.T) {
	const modelName = "grpc-schedfn-no-member"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	wf := scheduleFunctionRPCWorkflowJSON("schedfn-nomember-wf", schedFnConfigJSON("calcFire", "no-such-tag", 0))
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	// svc.registry (wired into the dispatcher above) has no registered
	// member — the create's arm-time Function dispatch must fail with
	// ErrNoMatchingMember and surface as a retryable 503.
	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "NO_COMPUTE_MEMBER_FOR_TAG")
}

// TestRPC_ScheduledFunction_DispatchTimeout_Envelope proves a Function
// callout that blows through its configured responseTimeoutMs surfaces a
// retryable 503 DISPATCH_TIMEOUT through the gRPC envelope — the compute-infra
// classification shared by every dispatch kind (dispatch.go's
// dispatchCalloutToMember). The registered member's send func deliberately
// never completes the tracked request, so the dispatcher's own timeout
// fires — mirroring TestDispatchCalloutToMember_SuccessAndTimeout's
// "nobody answers" half, one layer up through the gRPC envelope.
func TestRPC_ScheduledFunction_DispatchTimeout_Envelope(t *testing.T) {
	const modelName = "grpc-schedfn-timeout"
	const tag = "sched-fn-timeout-tag"
	const responseTimeoutMs = 20
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	wf := scheduleFunctionRPCWorkflowJSON("schedfn-timeout-wf", schedFnConfigJSON("calcSlow", tag, responseTimeoutMs))
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	svc.registry.Register(testTenant, []string{tag}, func(ce *cepb.CloudEvent) error {
		return nil // never answers — the request is tracked but never completed
	})

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "DISPATCH_TIMEOUT")
}

// TestRPC_ScheduledFunction_MalformedResult_Envelope proves a structurally
// invalid Schedule result (both fireAt and fireAfterMs set — resolveSchedule
// requires exactly one) surfaces a sanitized 500 SERVER_ERROR through the
// gRPC envelope: the fault is in the compute node's response, not the
// caller's request, so — per the sanitized-5xx contract
// (.claude/rules/error-handling.md) — the envelope carries a ticket, not the
// internal SCHEDULE_FUNCTION_INVALID_RESULT domain code or cause text
// (buildErrorFields discards AppError.Code for LevelInternal errors; see
// errors.go).
func TestRPC_ScheduledFunction_MalformedResult_Envelope(t *testing.T) {
	const modelName = "grpc-schedfn-malformed"
	const tag = "sched-fn-malformed-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	wf := scheduleFunctionRPCWorkflowJSON("schedfn-malformed-wf", schedFnConfigJSON("calcBadResult", tag, 0))
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	var memberID string
	memberID = svc.registry.Register(testTenant, []string{tag}, func(ce *cepb.CloudEvent) error {
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return nil
		}
		member := svc.registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success:    true,
			ResultKind: "Schedule",
			// Both fireAt and fireAfterMs set — resolveSchedule requires exactly one.
			Result: json.RawMessage(`{"fireAt":1234567890000,"fireAfterMs":1000}`),
		})
		return nil
	})

	typed := createScheduledEntity(t, svc, ctx, modelName)
	if typed.Success {
		t.Fatal("expected create to fail on a malformed compute-node Schedule result")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "SERVER_ERROR" {
		t.Errorf("expected envelope code SERVER_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "ticket") {
		t.Errorf("expected a correlation ticket in the sanitized 5xx message, got %s", typed.Error.Message)
	}
	if strings.Contains(typed.Error.Message, "SCHEDULE_FUNCTION_INVALID_RESULT") || strings.Contains(typed.Error.Message, "fireAt") {
		t.Errorf("sanitized-5xx contract violated: internal code/cause text leaked into the client-facing envelope: %s", typed.Error.Message)
	}
}

// TestRPC_ScheduledFunction_ExplicitFire_ReturnsTransitionNotFound proves the
// inherited explicit-fire rejection (TestRPC_EntityTransition_ScheduledTransitionRejected's
// legacy-delayMs precedent) holds identically for a schedule.function
// transition through the gRPC entrypoint: engine.go's manual-transition path
// checks transition.Schedule != nil regardless of which Schedule variant is
// configured, so a client-issued ManualTransition against the transition by
// name is rejected with TRANSITION_NOT_FOUND — it is never manually
// fireable.
func TestRPC_ScheduledFunction_ExplicitFire_ReturnsTransitionNotFound(t *testing.T) {
	const modelName = "grpc-schedfn-explicit-fire"
	const tag = "sched-fn-explicit-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	wf := scheduleFunctionRPCWorkflowJSON("schedfn-explicit-wf", schedFnConfigJSON("calcExplicit", tag, 0))
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	var memberID string
	memberID = svc.registry.Register(testTenant, []string{tag}, func(ce *cepb.CloudEvent) error {
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return nil
		}
		member := svc.registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success:    true,
			ResultKind: "Schedule",
			// Far future — never fires mid-test.
			Result: json.RawMessage(`{"fireAfterMs":600000}`),
		})
		return nil
	})

	createTyped := createScheduledEntity(t, svc, ctx, modelName)
	if !createTyped.Success {
		t.Fatalf("expected create to succeed (a real, non-expired arm); got error: %v", createTyped.Error)
	}
	entityID := createTyped.TransactionInfo.EntityIds[0]

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
		t.Fatal("expected explicit fire of a schedule.function transition to fail")
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
}

// --- Phase-2 uniform-503 reconciliation: processor/criterion no-member,
// via the gRPC entrypoint (issue #419's Function inherits this
// classification; these two pin the processor/criterion siblings it
// inherited it FROM, at the entrypoint dispatch_test.go's unit seam and
// internal/e2e's HTTP coverage don't reach). ---

// TestRPC_ProcessorNoMember_Returns503Envelope proves a processor whose
// calculationNodesTags match no connected member surfaces the Phase-2
// uniform-503 NO_COMPUTE_MEMBER_FOR_TAG classification through the gRPC
// EntityManage envelope, mirroring internal/e2e's HTTP-level
// TestProcessorNoMember_Returns503 (Phase 2 Task 2.1) at the gRPC
// entrypoint.
func TestRPC_ProcessorNoMember_Returns503Envelope(t *testing.T) {
	const modelName = "grpc-proc-no-member"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "proc-nomember-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "init", "next": "Closed", "manual": false,
					"processors": [{"type": "calculator", "name": "tag-with-foo", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": "no-such-tag"}}]
				}]},
				"Closed": {}
			}
		}]
	}`
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	})
	resp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	assertClientErrorEnvelope(t, typed, "NO_COMPUTE_MEMBER_FOR_TAG")
}

// TestRPC_CriterionNoMember_Returns503Envelope is
// TestRPC_ProcessorNoMember_Returns503Envelope's FUNCTION-criterion sibling:
// an automated transition guarded by a criterion whose calculationNodesTags
// match no connected member must surface the same 503
// NO_COMPUTE_MEMBER_FOR_TAG classification through the gRPC envelope.
func TestRPC_CriterionNoMember_Returns503Envelope(t *testing.T) {
	const modelName = "grpc-criterion-no-member"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "criterion-nomember-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-approve", "next": "APPROVED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "always-true",
						"config": {"calculationNodesTags": "no-such-tag"}}}
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, wf)

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	})
	resp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	assertClientErrorEnvelope(t, typed, "NO_COMPUTE_MEMBER_FOR_TAG")
}
