package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newTestEnvWithLocalCriteria is newTestEnvWithWorkflow with a localproc
// external-processing service, so criteria callouts run in-process and each
// name's calls are counted.
func newTestEnvWithLocalCriteria(t *testing.T) (*CloudEventsServiceImpl, *workflow.Handler, context.Context, *localproc.LocalProcessingService) {
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

	lp := localproc.New()
	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr, workflow.WithExternalProcessing(lp))
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore, newTestConsistency(t, factory))
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), newTestConsistency(t, factory))
	modelHandler := model.New(factory)
	workflowHandler := workflow.New(factory, engine, 60*time.Second)

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	ctx := spi.WithUserContext(context.Background(), uc)
	return svc, workflowHandler, ctx, lp
}

const (
	cfgrpcPreCheckKindA = `{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"a"}`
	cfgrpcUndeclared    = `{"type":"simple","jsonPath":"$.amonut","operatorType":"GREATER_THAN","value":1}`
)

func cfgrpcFn(name, preCheck string) string {
	if preCheck == "" {
		return `{"type":"function","function":{"name":"` + name + `"}}`
	}
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func cfgrpcGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

// cfgrpcWorkflow rests an entity in READY after create; READY's single
// transition "approve" carries criterion and is automatic or manual.
func cfgrpcWorkflow(criterion string, manual bool) string {
	m := "false"
	if manual {
		m = "true"
	}
	return `{"importMode":"REPLACE","workflows":[{"version":"1.1","name":"cf-grpc-wf","initialState":"NONE","active":true,
		"states":{
			"NONE":{"transitions":[{"name":"init","next":"READY","manual":false}]},
			"READY":{"transitions":[{"name":"approve","next":"APPROVED","manual":` + m + `,"criterion":` + criterion + `}]},
			"APPROVED":{}
		}}]}`
}

func cfgrpcImport(t *testing.T, svc *CloudEventsServiceImpl, wfHandler *workflow.Handler, ctx context.Context, modelName, wf string) {
	t.Helper()
	importAndLockModel(t, svc, ctx, modelName, "1", map[string]any{"name": "Test", "kind": "a", "amount": 100})
	req := httptest.NewRequest(http.MethodPost, "/api/model/"+modelName+"/1/workflow/import",
		bytes.NewReader([]byte(wf))).WithContext(ctx)
	rec := httptest.NewRecorder()
	wfHandler.ImportEntityModelWorkflow(rec, req, modelName, 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("workflow import: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// cfgrpcCreate issues the gRPC create and returns the typed envelope.
func cfgrpcCreate(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, modelName, kind string) events.EntityTransactionResponseJson {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id":         "create-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test", "kind": kind, "amount": 100},
		},
	}))
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	return typed
}

func cfgrpcEntityID(t *testing.T, typed events.EntityTransactionResponseJson) string {
	t.Helper()
	if !typed.Success {
		t.Fatalf("create failed: %+v", typed.Error)
	}
	b, err := json.Marshal(typed)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		TransactionInfo struct {
			EntityIds []string `json:"entityIds"`
		} `json:"transactionInfo"`
	}
	if err := json.Unmarshal(b, &m); err != nil || len(m.TransactionInfo.EntityIds) == 0 {
		t.Fatalf("no entity id in %s (%v)", b, err)
	}
	return m.TransactionInfo.EntityIds[0]
}

func cfgrpcState(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, id string) string {
	t.Helper()
	env, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: id})
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	s, _ := env.Meta["state"].(string)
	return s
}

func cfgrpcTransition(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, id string) events.EntityTransitionResponseJson {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityTransitionRequest, map[string]any{
		"id": "transition-1", "entityId": id, "transition": "approve",
	}))
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	return typed
}

func cfgrpcAnswer(lp *localproc.LocalProcessingService, name string, matches bool, reason string) {
	lp.RegisterCriteriaReason(name, func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) {
		return matches, reason, nil
	})
}

func cfgrpcAssertWorkflowFailed(t *testing.T, typed events.EntityTransactionResponseJson) {
	t.Helper()
	if typed.Success {
		t.Fatal("expected the operation to fail")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "WORKFLOW_FAILED") {
		t.Errorf("Error.Message = %q, want it to contain WORKFLOW_FAILED", typed.Error.Message)
	}
}

func TestCriterionFunction_Selection_PreCheckFalse_OtherWorkflowChosen(t *testing.T) {
	const modelName = "grpc-cf-selection"
	svc, wfHandler, ctx, lp := newTestEnvWithLocalCriteria(t)
	cfgrpcAnswer(lp, "sel", true, "")

	wf := `{"importMode":"REPLACE","workflows":[
		{"version":"1.1","name":"kind-a-wf","initialState":"NONE","active":true,
		 "criterion":{"type":"function","function":{"name":"sel","criterion":` + cfgrpcPreCheckKindA + `}},
		 "states":{"NONE":{"transitions":[{"name":"init","next":"VALIDATE","manual":false}]},
		           "VALIDATE":{"transitions":[{"name":"check","next":"A_CHECKED","manual":true}]},"A_CHECKED":{}}},
		{"version":"1.1","name":"kind-b-wf","initialState":"NONE","active":true,
		 "criterion":{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"b"},
		 "states":{"NONE":{"transitions":[{"name":"init","next":"VALIDATE","manual":false}]},
		           "VALIDATE":{"transitions":[{"name":"check","next":"B_CHECKED","manual":true}]},"B_CHECKED":{}}}]}`
	cfgrpcImport(t, svc, wfHandler, ctx, modelName, wf)

	id := cfgrpcEntityID(t, cfgrpcCreate(t, svc, ctx, modelName, "b"))
	resp, err := svc.EntityManage(ctx, makeCE(EntityTransitionRequest, map[string]any{
		"id": "transition-1", "entityId": id, "transition": "check",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("transition failed: %+v", typed.Error)
	}
	if got := cfgrpcState(t, svc, ctx, id); got != "B_CHECKED" {
		t.Errorf("state = %q, want B_CHECKED (kind-b-wf chosen)", got)
	}
	if n := lp.CriteriaCallCount("sel"); n != 0 {
		t.Errorf("sel callouts = %d, want 0 (pre-check false)", n)
	}
}

func TestCriterionFunction_Cascade_PreCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		wantState string
		wantCalls int
	}{
		{"pre-check false stays at source, no callout", "b", "READY", 0},
		// Control for the false case: the same workflow does advance and call out.
		{"pre-check true advances, one callout", "a", "APPROVED", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, wfHandler, ctx, lp := newTestEnvWithLocalCriteria(t)
			cfgrpcAnswer(lp, "a", true, "")
			cfgrpcImport(t, svc, wfHandler, ctx, "grpc-cf-cascade-pre",
				cfgrpcWorkflow(cfgrpcFn("a", cfgrpcPreCheckKindA), false))
			id := cfgrpcEntityID(t, cfgrpcCreate(t, svc, ctx, "grpc-cf-cascade-pre", tc.kind))
			if got := cfgrpcState(t, svc, ctx, id); got != tc.wantState {
				t.Errorf("state = %q, want %q", got, tc.wantState)
			}
			if n := lp.CriteriaCallCount("a"); n != tc.wantCalls {
				t.Errorf("a callouts = %d, want %d", n, tc.wantCalls)
			}
		})
	}
}

func TestCriterionFunction_Cascade_Groups(t *testing.T) {
	for _, tc := range []struct {
		name      string
		criterion string
		a, b      bool
		wantState string
		wantA     int
		wantB     int
	}{
		{"AND stops at the first false", cfgrpcGroup("AND", cfgrpcFn("a", ""), cfgrpcFn("b", "")), false, true, "READY", 1, 0},
		{"OR stops at the first true", cfgrpcGroup("OR", cfgrpcFn("a", ""), cfgrpcFn("b", "")), true, false, "APPROVED", 1, 0},
		{"NOT of a true function: no transition", cfgrpcGroup("NOT", cfgrpcFn("a", "")), true, false, "READY", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, wfHandler, ctx, lp := newTestEnvWithLocalCriteria(t)
			cfgrpcAnswer(lp, "a", tc.a, "")
			if strings.Contains(tc.criterion, `"name":"b"`) {
				cfgrpcAnswer(lp, "b", tc.b, "")
			}
			cfgrpcImport(t, svc, wfHandler, ctx, "grpc-cf-cascade-grp", cfgrpcWorkflow(tc.criterion, false))
			id := cfgrpcEntityID(t, cfgrpcCreate(t, svc, ctx, "grpc-cf-cascade-grp", "a"))
			if got := cfgrpcState(t, svc, ctx, id); got != tc.wantState {
				t.Errorf("state = %q, want %q", got, tc.wantState)
			}
			if n := lp.CriteriaCallCount("a"); n != tc.wantA {
				t.Errorf("a callouts = %d, want %d", n, tc.wantA)
			}
			if n := lp.CriteriaCallCount("b"); n != tc.wantB {
				t.Errorf("b callouts = %d, want %d", n, tc.wantB)
			}
		})
	}
}

func TestCriterionFunction_ManualRefused_ReportsEveryMember(t *testing.T) {
	const modelName = "grpc-cf-manual"
	svc, wfHandler, ctx, lp := newTestEnvWithLocalCriteria(t)
	cfgrpcAnswer(lp, "a", true, "")
	cfgrpcAnswer(lp, "b", false, "too small")
	cfgrpcImport(t, svc, wfHandler, ctx, modelName,
		cfgrpcWorkflow(cfgrpcGroup("AND", cfgrpcFn("a", ""), cfgrpcFn("b", "")), true))
	id := cfgrpcEntityID(t, cfgrpcCreate(t, svc, ctx, modelName, "a"))

	typed := cfgrpcTransition(t, svc, ctx, id)
	if typed.Success {
		t.Fatal("expected the manual transition to be refused")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR", typed.Error.Code)
	}
	for _, want := range []string{"WORKFLOW_FAILED", "b at /conditions/1: answered false: too small"} {
		if !strings.Contains(typed.Error.Message, want) {
			t.Errorf("Error.Message = %q, want it to contain %q", typed.Error.Message, want)
		}
	}
	if got := cfgrpcState(t, svc, ctx, id); got != "READY" {
		t.Errorf("state = %q, want READY", got)
	}
}

func TestCriterionFunction_UnevaluableInSkippedMember_Refused(t *testing.T) {
	const modelName = "grpc-cf-unevaluable"
	svc, wfHandler, ctx, lp := newTestEnvWithLocalCriteria(t)
	cfgrpcAnswer(lp, "a", true, "")
	// OR would stop at a, but the undeclared path is a fault in the criterion
	// itself and is found before any callout.
	cfgrpcImport(t, svc, wfHandler, ctx, modelName,
		cfgrpcWorkflow(cfgrpcGroup("OR", cfgrpcFn("a", ""), cfgrpcUndeclared), false))

	typed := cfgrpcCreate(t, svc, ctx, modelName, "a")
	cfgrpcAssertWorkflowFailed(t, typed)
	if !strings.Contains(typed.Error.Message, "amonut") {
		t.Errorf("Error.Message = %q, want it to name the undeclared path amonut", typed.Error.Message)
	}
	if n := lp.CriteriaCallCount("a"); n != 0 {
		t.Errorf("a callouts = %d, want 0", n)
	}
}

func TestCriterionFunction_CalloutFailure_InGroup(t *testing.T) {
	const modelName = "grpc-cf-calloutfail"
	svc, wfHandler, ctx, lp := newTestEnvWithLocalCriteria(t)
	cfgrpcAnswer(lp, "a", false, "")
	lp.RegisterCriteriaReason("b", func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) {
		return false, "", errors.New("b down")
	})
	cfgrpcImport(t, svc, wfHandler, ctx, modelName,
		cfgrpcWorkflow(cfgrpcGroup("OR", cfgrpcFn("a", ""), cfgrpcFn("b", "")), false))

	typed := cfgrpcCreate(t, svc, ctx, modelName, "a")
	cfgrpcAssertWorkflowFailed(t, typed)
	// The OR reached the failing member: a answered false, then b was called.
	for _, name := range []string{"a", "b"} {
		if n := lp.CriteriaCallCount(name); n != 1 {
			t.Errorf("%s callouts = %d, want 1", name, n)
		}
	}
	if !strings.Contains(typed.Error.Message, "b down") {
		t.Errorf("Error.Message = %q, want it to carry the member's error text", typed.Error.Message)
	}
}

func TestCriterionFunction_NoMemberInGroup_503Envelope(t *testing.T) {
	const modelName = "grpc-cf-nomember"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)
	tagged := `{"type":"function","function":{"name":"x","config":{"calculationNodesTags":"no-such-tag"}}}`
	cfgrpcImport(t, svc, wfHandler, ctx, modelName,
		cfgrpcWorkflow(cfgrpcGroup("AND", `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10}`, tagged), false))

	typed := cfgrpcCreate(t, svc, ctx, modelName, "a")
	assertClientErrorEnvelope(t, typed, "NO_COMPUTE_MEMBER_FOR_TAG")
}
