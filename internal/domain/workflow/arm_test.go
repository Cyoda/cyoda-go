package workflow

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// fixedClock returns a clock function pinned to a fixed instant, for
// deterministic scheduledTime/armedAt assertions.
func fixedClock(ms int64) func() time.Time {
	t := time.UnixMilli(ms)
	return func() time.Time { return t }
}

// setupEngineWithClock is setupEngine plus an injected deterministic clock.
func setupEngineWithClock(t *testing.T, nowMs int64) (*Engine, spi.StoreFactory) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr, WithScheduledClock(fixedClock(nowMs)))
	return engine, factory
}

func TestReconcile_ArmsCurrentStateSchedules(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}

	delayMs := int64(1000)
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

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := makeEntity("sched-e1", modelRef, map[string]any{})
	entity.Meta.TransactionID = txID

	_, err = engine.Execute(txCtx, entity, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "sched-e1", "OPEN", "AutoClose")
	task, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected scheduled task %q to be armed", wantID)
	}
	if task.ScheduledTime != nowMs+delayMs {
		t.Errorf("ScheduledTime = %d, want %d", task.ScheduledTime, nowMs+delayMs)
	}
	if task.ArmedAt != nowMs {
		t.Errorf("ArmedAt = %d, want %d", task.ArmedAt, nowMs)
	}
	if task.SourceState != "OPEN" || task.Transition != "AutoClose" || task.EntityID != "sched-e1" {
		t.Errorf("unexpected task payload: %+v", task)
	}
	if task.TenantID != testTenant {
		t.Errorf("TenantID = %q, want %q", task.TenantID, testTenant)
	}

	// Audit trail must record the arm.
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "sched-e1", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	var sawArm bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventScheduledTransitionArmed {
			sawArm = true
		}
	}
	if !sawArm {
		t.Error("expected a SCHEDULED_TRANSITION_ARM audit event")
	}
}

func TestReconcile_LoopbackReArmsNoCancel(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loop-order", ModelVersion: "1.0"}

	delayMs := int64(2000)
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "LoopSchedWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// First write: enter OPEN and arm.
	txID1, txCtx1, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := makeEntity("loop-e1", modelRef, map[string]any{})
	entity.Meta.TransactionID = txID1
	if _, err := engine.Execute(txCtx1, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ctx, txID1); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	wantID := taskID(testTenant, "loop-e1", "OPEN", "AutoClose")
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if _, found, _ := sts.Get(ctx, wantID); !found {
		t.Fatalf("expected task %q armed after Execute", wantID)
	}

	// Second write: loopback while still in OPEN (data update, no state change).
	txID2, txCtx2, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity.Meta.TransactionID = txID2
	if _, err := engine.Loopback(txCtx2, entity); err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	if err := txMgr.Commit(ctx, txID2); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	task, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q to still be armed (re-armed via upsert) after loopback", wantID)
	}
	if task.ScheduledTime != nowMs+delayMs {
		t.Errorf("ScheduledTime = %d, want %d", task.ScheduledTime, nowMs+delayMs)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "loop-e1", txID2)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	for _, ev := range events {
		if ev.EventType == spi.SMEventScheduledTransitionCancelled {
			t.Errorf("unexpected SCHEDULED_TRANSITION_CANCEL event on same-state loopback: %+v", ev)
		}
	}
}

func TestReconcile_TransitionCancelsOldState(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cancel-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CancelSchedWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
				{Name: "advance", Next: "REVIEW", Manual: true},
			}},
			"REVIEW": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoExpire", Next: "EXPIRED", Schedule: &spi.TransitionSchedule{DelayMs: 5000}},
			}},
			"EXPIRED": {},
			"CLOSED":  {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// Enter OPEN → arms AutoClose.
	txID1, txCtx1, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := makeEntity("cancel-e1", modelRef, map[string]any{})
	entity.Meta.TransactionID = txID1
	if _, err := engine.Execute(txCtx1, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ctx, txID1); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	openTaskID := taskID(testTenant, "cancel-e1", "OPEN", "AutoClose")
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if _, found, _ := sts.Get(ctx, openTaskID); !found {
		t.Fatalf("expected task %q armed after entering OPEN", openTaskID)
	}

	// Manual transition OPEN -> REVIEW: should delete OPEN's task and arm REVIEW's.
	txID2, txCtx2, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity.Meta.TransactionID = txID2
	if _, err := engine.ManualTransition(txCtx2, entity, "advance"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if err := txMgr.Commit(ctx, txID2); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, found, err := sts.Get(ctx, openTaskID); err != nil {
		t.Fatalf("Get(openTaskID): %v", err)
	} else if found {
		t.Errorf("expected OPEN's scheduled task %q to be cancelled after leaving the state", openTaskID)
	}

	reviewTaskID := taskID(testTenant, "cancel-e1", "REVIEW", "AutoExpire")
	if _, found, err := sts.Get(ctx, reviewTaskID); err != nil {
		t.Fatalf("Get(reviewTaskID): %v", err)
	} else if !found {
		t.Errorf("expected REVIEW's scheduled task %q to be armed", reviewTaskID)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "cancel-e1", txID2)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	var sawCancel, sawArm bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventScheduledTransitionCancelled {
			sawCancel = true
		}
		if ev.EventType == spi.SMEventScheduledTransitionArmed {
			sawArm = true
		}
	}
	if !sawCancel {
		t.Error("expected a SCHEDULED_TRANSITION_CANCEL audit event")
	}
	if !sawArm {
		t.Error("expected a SCHEDULED_TRANSITION_ARM audit event for REVIEW's schedule")
	}
}
