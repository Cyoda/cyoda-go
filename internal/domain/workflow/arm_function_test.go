package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// setupEngineWithClockAndExtProc is setupEngineWithClock plus an injected
// ExternalProcessingService, for the arm-time Function dispatch tests.
func setupEngineWithClockAndExtProc(t *testing.T, nowMs int64, extProc contract.ExternalProcessingService) (*Engine, spi.StoreFactory) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr, WithScheduledClock(fixedClock(nowMs)), WithExternalProcessing(extProc))
	return engine, factory
}

func scheduleFunctionWorkflow(name, functionName string) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1.1", Name: name, InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{
					Function: &spi.ScheduleFunction{
						Name:                 functionName,
						ResultKind:           "Schedule",
						CalculationNodesTags: "sched",
					},
				}},
			}},
			"CLOSED": {},
		},
	}
}

func TestReconcile_FunctionAbsoluteFireArms(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	const fireAt = nowMs + 9_000

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(fmt.Sprintf(`{"fireAt":%d}`, fireAt))}, nil
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "abs-fire-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("AbsFireWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("abs-fire-e1", modelRef, map[string]any{})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "abs-fire-e1", "OPEN", "AutoClose")
	task, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q armed", wantID)
	}
	if task.ScheduledTime != fireAt {
		t.Errorf("ScheduledTime = %d, want %d", task.ScheduledTime, fireAt)
	}
	if task.TimeoutMs != nil {
		t.Errorf("TimeoutMs = %v, want nil", *task.TimeoutMs)
	}
	if lp.FunctionCallCount("calcFire") != 1 {
		t.Errorf("FunctionCallCount = %d, want 1", lp.FunctionCallCount("calcFire"))
	}
}

func TestReconcile_FunctionRelativeFireArms(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	const fireAfterMs = int64(4_000)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(fmt.Sprintf(`{"fireAfterMs":%d}`, fireAfterMs))}, nil
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "rel-fire-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("RelFireWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("rel-fire-e1", modelRef, map[string]any{})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "rel-fire-e1", "OPEN", "AutoClose")
	task, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q armed", wantID)
	}
	if task.ScheduledTime != nowMs+fireAfterMs {
		t.Errorf("ScheduledTime = %d, want %d", task.ScheduledTime, nowMs+fireAfterMs)
	}
	if task.ArmedAt != nowMs {
		t.Errorf("ArmedAt = %d, want %d", task.ArmedAt, nowMs)
	}
}

func TestReconcile_FunctionExpiryStoredAsTimeoutMs(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	const fireAfterMs = int64(4_000)
	const expireAfterMs = int64(600)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(
			fmt.Sprintf(`{"fireAfterMs":%d,"expireAfterMs":%d}`, fireAfterMs, expireAfterMs))}, nil
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "expiry-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("ExpiryWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("expiry-e1", modelRef, map[string]any{})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "expiry-e1", "OPEN", "AutoClose")
	task, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q armed", wantID)
	}
	if task.TimeoutMs == nil {
		t.Fatalf("TimeoutMs = nil, want %d", expireAfterMs)
	}
	if *task.TimeoutMs != expireAfterMs {
		t.Errorf("TimeoutMs = %d, want %d", *task.TimeoutMs, expireAfterMs)
	}
}

// TestReconcile_FunctionBornExpiredCancelsAndEmitsExpire covers the full
// lifecycle: a Function first arms a real future row, then — on a
// same-state loopback — resolves to a born-expired result. The pre-existing
// row must be cancelled (via ReconcileRequest.Cancel, not the
// SourceState-mismatch branch, since the entity never left OPEN) and the
// audit trail must show SCHEDULED_TRANSITION_EXPIRE, never
// SCHEDULED_TRANSITION_CANCEL, for this transition.
func TestReconcile_FunctionBornExpiredCancelsAndEmitsExpire(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)

	callCount := 0
	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		callCount++
		if callCount == 1 {
			// First arm: a real future fire time.
			return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":5000}`)}, nil
		}
		// Second call (loopback): expiry resolves to exactly the fire
		// time itself (expireAfterMs:0) — born expired per resolveSchedule
		// ("expiry <= scheduledTime").
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":1000,"expireAfterMs":0}`)}, nil
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "born-expired-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("BornExpiredWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// First write: enters OPEN, arms a real future row.
	txID1, txCtx1, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := makeEntity("born-expired-e1", modelRef, map[string]any{})
	entity.Meta.TransactionID = txID1
	if _, err := engine.Execute(txCtx1, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ctx, txID1); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	wantID := taskID(testTenant, "born-expired-e1", "OPEN", "AutoClose")
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if _, found, err := sts.Get(ctx, wantID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if !found {
		t.Fatalf("expected task %q armed after first Execute", wantID)
	}

	// Second write: same-state loopback, Function now resolves born-expired.
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

	if _, found, err := sts.Get(ctx, wantID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if found {
		t.Errorf("expected task %q cancelled (born expired), still present", wantID)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "born-expired-e1", txID2)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	var sawExpire, sawCancel bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventScheduledTransitionExpired {
			sawExpire = true
		}
		if ev.EventType == spi.SMEventScheduledTransitionCancelled {
			sawCancel = true
		}
	}
	if !sawExpire {
		t.Error("expected a SCHEDULED_TRANSITION_EXPIRE audit event")
	}
	if sawCancel {
		t.Error("born-expired must audit as EXPIRE, not CANCEL")
	}
}

func TestReconcile_FunctionDispatchFailureFailsWrite(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{}, common.Operational(503, common.ErrCodeDispatchTimeout, "dispatch timed out")
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "dispatch-fail-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("DispatchFailWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("dispatch-fail-e1", modelRef, map[string]any{})
	if _, err := engine.Execute(ctx, entity, ""); err == nil {
		t.Fatal("expected error from failing Function dispatch")
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "dispatch-fail-e1", "OPEN", "AutoClose")
	if _, found, err := sts.Get(ctx, wantID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if found {
		t.Error("dispatch failure must not arm a task")
	}
}

func TestReconcile_FunctionWrongResultKindIs500(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{Kind: "NotSchedule", Value: json.RawMessage(`{}`)}, nil
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "wrong-kind-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("WrongKindWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("wrong-kind-e1", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "")
	assertScheduleFunctionInvalidResult(t, err)
}

func TestReconcile_FunctionMalformedResultIs500(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction) (contract.FunctionResult, error) {
		// Neither fireAt nor fireAfterMs — malformed.
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"expireAfterMs":600}`)}, nil
	})

	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "malformed-order", ModelVersion: "1.0"}
	wf := scheduleFunctionWorkflow("MalformedWF", "calcFire")
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("malformed-e1", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "")
	assertScheduleFunctionInvalidResult(t, err)
}

func assertScheduleFunctionInvalidResult(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeScheduleFunctionInvalidResult {
		t.Errorf("Code = %q, want %q", appErr.Code, common.ErrCodeScheduleFunctionInvalidResult)
	}
	if appErr.Status != 500 {
		t.Errorf("Status = %d, want 500", appErr.Status)
	}
}
