package workflow

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/testing/fakeclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

var clockTestEpoch = time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)

func TestEngine_AuditTimestampFromClock(t *testing.T) {
	clk := fakeclock.New(clockTestEpoch)
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	engine := NewEngine(factory, uuids, factory.NewTransactionManager(uuids), WithClock(clk))
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit", ModelVersion: "1.0"}
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AuditWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "GO", Next: "DONE"}}},
			"DONE":    {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	if _, err := engine.Execute(ctx, makeEntity("clk-e1", modelRef, map[string]any{"x": 1}), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "clk-e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no audit events recorded")
	}
	for _, ev := range events {
		if !ev.Timestamp.Equal(clockTestEpoch) {
			t.Errorf("event %v Timestamp = %v, want %v", ev.EventType, ev.Timestamp, clockTestEpoch)
		}
	}
}

func TestArm_ScheduledTimeFromClock(t *testing.T) {
	clk := fakeclock.New(clockTestEpoch)
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr, WithClock(clk))
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}
	const delayMs = int64(5000)
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "SchedWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	e := makeEntity("clk-sched", modelRef, map[string]any{})
	e.Meta.TransactionID = txID
	if _, err := engine.Execute(txCtx, e, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	task, found, err := sts.Get(ctx, testTenant, taskID(testTenant, "clk-sched", "OPEN", "AutoClose"))
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if want := clockTestEpoch.UnixMilli() + delayMs; task.ScheduledTime != want {
		t.Errorf("ScheduledTime = %d, want %d", task.ScheduledTime, want)
	}
	if want := clockTestEpoch.UnixMilli(); task.ArmedAt != want {
		t.Errorf("ArmedAt = %d, want %d", task.ArmedAt, want)
	}
}

func TestWithClock_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithClock(nil) did not panic")
		}
	}()
	WithClock(nil)(&Engine{})
}
