package grpc

import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const taskDeleteTenant spi.TenantID = "task-delete-tenant"

// newTaskDeleteEnv is newTestEnv with a taskconflict.Factory in front of the
// task store and a person workflow that arms AutoClose one hour after an
// entity enters OPEN.
func newTaskDeleteEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context, *memory.StoreFactory, *taskconflict.Plan) {
	t.Helper()
	real := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
	t.Cleanup(func() { real.Close() })
	real.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := real.GetTransactionManager()
	plan := taskconflict.NewPlan()
	factory := &taskconflict.Factory{StoreFactory: real, Plan: plan}

	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "task-delete-user", UserName: "Task Delete",
		Tenant: spi.Tenant{ID: taskDeleteTenant, Name: "Task Delete"},
		Roles:  []string{"ADMIN"},
	})

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New()),
		modelHandler:  model.New(factory),
		searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore),
	}
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	wfStore, err := real.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := wfStore.Save(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "person-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 3_600_000}}}},
			"CLOSED": {},
		},
	}}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}
	return svc, ctx, real, plan
}

func createPersonRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context) string {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "create", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info := parseResponsePayload(t, resp)["transactionInfo"].(map[string]any)
	return info["entityIds"].([]any)[0].(string)
}

func personTasks(t *testing.T, real *memory.StoreFactory, ctx context.Context, entityID string) int {
	t.Helper()
	sts, err := real.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(ctx, taskDeleteTenant, spi.ScheduledTaskQuery{EntityID: entityID, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(page.Items)
}

func requireConflictEnvelope(t *testing.T, success bool, code, message string, retryable *bool) {
	t.Helper()
	if success {
		t.Fatal("success = true, want false")
	}
	if code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", code)
	}
	if !strings.HasPrefix(message, common.ErrCodeConflict+":") {
		t.Errorf("message = %q, want prefix %q", message, common.ErrCodeConflict+":")
	}
	if retryable == nil || !*retryable {
		t.Errorf("retryable = %v, want true", retryable)
	}
}

func TestRPC_EntityDelete_RemovesTheEntitysTasks(t *testing.T) {
	svc, ctx, real, _ := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	if n := personTasks(t, real, ctx, id); n != 1 {
		t.Fatalf("tasks before the delete = %d, want 1", n)
	}

	resp, err := svc.EntityManage(ctx, makeCE(EntityDeleteRequest, map[string]any{"id": "del", "entityId": id}))
	if err != nil {
		t.Fatalf("EntityManage: %v", err)
	}
	var typed events.EntityDeleteResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("delete failed: %+v", typed.Error)
	}
	if n := personTasks(t, real, ctx, id); n != 0 {
		t.Errorf("tasks after the delete = %d, want 0", n)
	}
}

func TestRPC_EntityDelete_TaskConflictPersists_ConflictEnvelope(t *testing.T) {
	svc, ctx, real, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	plan.Refuse(taskconflict.DeleteForEntities, 100)

	resp, err := svc.EntityManage(ctx, makeCE(EntityDeleteRequest, map[string]any{"id": "del", "entityId": id}))
	if err != nil {
		t.Fatalf("EntityManage: %v", err)
	}
	var typed events.EntityDeleteResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Fatalf("error = nil, want the conflict envelope; success=%v", typed.Success)
	}
	requireConflictEnvelope(t, typed.Success, typed.Error.Code, typed.Error.Message, typed.Error.Retryable)
	if n := personTasks(t, real, ctx, id); n != 1 {
		t.Errorf("tasks = %d, want 1 (every attempt rolled back)", n)
	}
}

func TestRPC_EntityUpdate_TaskRowConflict_ConflictEnvelope(t *testing.T) {
	svc, ctx, _, plan := newTaskDeleteEnv(t)
	id := createPersonRPC(t, svc, ctx)
	plan.Refuse(taskconflict.ReconcileForEntity, 1)

	resp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id": "upd", "dataFormat": "JSON",
		"payload": map[string]any{"entityId": id, "data": map[string]any{"name": "Alicia"}},
	}))
	if err != nil {
		t.Fatalf("EntityManage: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Fatalf("error = nil, want the conflict envelope; success=%v", typed.Success)
	}
	requireConflictEnvelope(t, typed.Success, typed.Error.Code, typed.Error.Message, typed.Error.Retryable)
}
