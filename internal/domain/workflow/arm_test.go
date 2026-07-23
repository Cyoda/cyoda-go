package workflow

import (
	"context"
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

// steppableClock returns a clock function backed by a mutable ms value, plus
// an advance func the test can call between engine operations to move "now"
// forward. Unlike fixedClock — which is frozen for the whole test and so
// cannot distinguish "re-armed with a fresh ScheduledTime" from "left the
// stale row untouched" (both land on the same nowMs+delayMs value) —
// steppableClock lets a test observe a SECOND, later "now" from a subsequent
// engine call, so a re-arm is only provably correct if the assertion tracks
// that later instant.
func steppableClock(initialMs int64) (clock func() time.Time, advance func(deltaMs int64)) {
	ms := initialMs
	return func() time.Time { return time.UnixMilli(ms) },
		func(deltaMs int64) { ms += deltaMs }
}

// setupEngineWithSteppableClock is setupEngineWithClock but returns an
// advance func instead of freezing the clock for the whole test.
func setupEngineWithSteppableClock(t *testing.T, initialMs int64) (*Engine, spi.StoreFactory, func(deltaMs int64)) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	clock, advance := steppableClock(initialMs)
	engine := NewEngine(factory, uuids, txMgr, WithScheduledClock(clock))
	return engine, factory, advance
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
	const advanceMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, nowMs)
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
	firstTask, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q armed after Execute", wantID)
	}
	// Baseline from the first arm, at the ORIGINAL now.
	if firstTask.ScheduledTime != nowMs+delayMs {
		t.Errorf("after Execute: ScheduledTime = %d, want %d", firstTask.ScheduledTime, nowMs+delayMs)
	}
	if firstTask.ArmedAt != nowMs {
		t.Errorf("after Execute: ArmedAt = %d, want %d", firstTask.ArmedAt, nowMs)
	}

	// Advance the clock before the loopback so a re-armed row is observably
	// distinguishable from the untouched first-arm row: if reconcile were a
	// no-op on the loopback path, ScheduledTime/ArmedAt below would still read
	// the FIRST now (nowMs), not the second (nowMs+advanceMs), and the
	// assertions after Loopback would fail.
	advance(advanceMs)
	secondNowMs := nowMs + advanceMs

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
	// Proves the loopback reconcile actually re-armed the row (upsert) rather
	// than leaving the stale first-arm row in place: both ScheduledTime and
	// ArmedAt must reflect the SECOND, later now — a no-op would still show
	// the first now's values (nowMs+delayMs / nowMs), which is what the old
	// frozen-clock version of this test could not tell apart.
	if task.ScheduledTime != secondNowMs+delayMs {
		t.Errorf("ScheduledTime = %d, want %d (second now + delay)", task.ScheduledTime, secondNowMs+delayMs)
	}
	if task.ArmedAt != secondNowMs {
		t.Errorf("ArmedAt = %d, want %d (second now)", task.ArmedAt, secondNowMs)
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

// armOriginUserCtx builds a user ctx with Kind explicitly set to
// spi.PrincipalUser, for the "user arms in their own tx" ArmedBy scenario.
// Mirrors attributionUserCtx in service_attribution_test.go — ctxWithTenant
// (used elsewhere in this package) predates the attribution work and leaves
// Kind unset, which would leave these assertions checking the legacy
// fallback rather than the documented user-kind behavior.
func armOriginUserCtx(userID string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   userID,
		UserName: userID,
		Kind:     spi.PrincipalUser,
		Tenant:   spi.Tenant{ID: testTenant, Name: string(testTenant)},
		Roles:    []string{"USER"},
	})
}

// armServiceCtx builds a service-kind ctx (no tx of its own — always joined
// onto an existing one), for the "service executor stages inside a
// user-origin tx" ArmedBy scenario.
func armServiceCtx(serviceID string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   serviceID,
		UserName: serviceID,
		Kind:     spi.PrincipalService,
		Tenant:   spi.Tenant{ID: testTenant, Name: string(testTenant)},
	})
}

// TestReconcile_ArmedByCapturesUserOrigin covers §5.2 case (a): a user ctx
// arming inside that same user's own transaction stamps ArmedBy with that
// user.
func TestReconcile_ArmedByCapturesUserOrigin(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := armOriginUserCtx("arm-user")
	modelRef := spi.ModelRef{EntityName: "armedby-user-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ArmedByUserWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
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

	entity := makeEntity("armedby-user-e1", modelRef, map[string]any{})
	entity.Meta.TransactionID = txID
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "armedby-user-e1", "OPEN", "AutoClose")
	task, found, err := sts.Get(ctx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q armed", wantID)
	}
	want := spi.Principal{ID: "arm-user", Kind: spi.PrincipalUser}
	if task.ArmedBy != want {
		t.Errorf("ArmedBy = %+v, want %+v", task.ArmedBy, want)
	}
}

// TestReconcile_ArmedByCapturesChainOriginOverServiceExecutor covers §5.2
// case (b): a service-kind executor staging inside another principal's
// (user's) transaction arms with the TX ORIGIN, not the service executor —
// this deliberately differs from the write-stamp rule (spi.AttributionFor),
// which records the service as ChangeExecutor while inheriting the origin
// only for ChangeUser. Arming has no executor/attributed split: ArmedBy is
// always the chain origin.
func TestReconcile_ArmedByCapturesChainOriginOverServiceExecutor(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ownerCtx := armOriginUserCtx("origin-user")
	modelRef := spi.ModelRef{EntityName: "armedby-chain-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ArmedByChainWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ownerCtx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ownerCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	ownerTxID, ownerTxCtx, err := txMgr.Begin(ownerCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	serviceCtx := armServiceCtx("arm-service")
	joinedCtx, err := txMgr.Join(serviceCtx, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	entity := makeEntity("armedby-chain-e1", modelRef, map[string]any{})
	entity.Meta.TransactionID = ownerTxID
	if _, err := engine.Execute(joinedCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ownerTxCtx, ownerTxID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(ownerCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	wantID := taskID(testTenant, "armedby-chain-e1", "OPEN", "AutoClose")
	task, found, err := sts.Get(ownerCtx, wantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("expected task %q armed", wantID)
	}
	wantOrigin := spi.Principal{ID: "origin-user", Kind: spi.PrincipalUser}
	if task.ArmedBy != wantOrigin {
		t.Errorf("ArmedBy = %+v, want tx origin %+v (not the service executor)", task.ArmedBy, wantOrigin)
	}
	notWant := spi.Principal{ID: "arm-service", Kind: spi.PrincipalService}
	if task.ArmedBy == notWant {
		t.Error("ArmedBy must not be the service executor — arming always uses the chain origin")
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
