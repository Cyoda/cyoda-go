package entity

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// taskTenant is the tenant deleteBatchedUserCtx binds.
const taskTenant spi.TenantID = "delete-batched-tenant"

// taskEnv is the Person model on memory. Its workflow arms AutoClose one
// hour after an entity enters OPEN, so every seeded entity has exactly one
// task and no scheduler finds it due. The handler reaches the task store
// through a taskconflict.Factory.
type taskEnv struct {
	h     *Handler
	ctx   context.Context
	real  *memory.StoreFactory
	txMgr spi.TransactionManager
	plan  *taskconflict.Plan
}

func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	real := memory.NewStoreFactory()
	t.Cleanup(func() { real.Close() })
	txMgr := mustTxMgr(t, real)
	ctx := newDeleteBatchedCtx(t, real)
	saveScheduledPersonWorkflow(t, real, ctx)
	plan := taskconflict.NewPlan()
	h := buildDeleteBatchedHandler(t, &taskconflict.Factory{StoreFactory: real, Plan: plan}, txMgr)
	return &taskEnv{h: h, ctx: ctx, real: real, txMgr: txMgr, plan: plan}
}

// withEntityStore rebuilds the handler so that EntityStore is store and the
// task store still goes through the plan.
func (e *taskEnv) withEntityStore(t *testing.T, store spi.EntityStore) {
	t.Helper()
	f := &entityStoreOverride{StoreFactory: &taskconflict.Factory{StoreFactory: e.real, Plan: e.plan}, store: store}
	e.h = buildDeleteBatchedHandler(t, f, e.txMgr)
}

type entityStoreOverride struct {
	spi.StoreFactory
	store spi.EntityStore
}

func (f *entityStoreOverride) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

func saveScheduledPersonWorkflow(t *testing.T, factory spi.StoreFactory, ctx context.Context) {
	t.Helper()
	wfStore, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := wfStore.Save(ctx, spi.ModelRef{EntityName: "Person", ModelVersion: "1"}, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "person-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 3_600_000}},
				{Name: "Leave", Next: "LEFT", Manual: true},
			}},
			"CLOSED": {},
			"LEFT":   {},
		},
	}}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}
}

// tasksOf counts entityID's tasks, read from the real store.
func (e *taskEnv) tasksOf(t *testing.T, entityID string) int {
	t.Helper()
	sts, err := e.real.ScheduledTaskStore(e.ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(e.ctx, taskTenant, spi.ScheduledTaskQuery{EntityID: entityID, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(page.Items)
}

// modelTasks counts the tasks of a model, read from the real store.
func (e *taskEnv) modelTasks(t *testing.T, modelName string) int {
	t.Helper()
	sts, err := e.real.ScheduledTaskStore(e.ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(e.ctx, taskTenant, spi.ScheduledTaskQuery{ModelName: modelName, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(page.Items)
}

func (e *taskEnv) exists(t *testing.T, id string) bool {
	t.Helper()
	store, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = store.Get(e.ctx, id)
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("Get %s: %v", id, err)
	}
	return err == nil
}

func requireConflict409(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T), want *common.AppError", err, err)
	}
	if appErr.Status != 409 || appErr.Code != common.ErrCodeConflict || !appErr.Retryable {
		t.Fatalf("got %d %s retryable=%v, want 409 %s retryable=true", appErr.Status, appErr.Code, appErr.Retryable, common.ErrCodeConflict)
	}
}
