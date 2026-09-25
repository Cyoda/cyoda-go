package workflow

import (
	"context"
	"errors"
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
	task, found, err := sts.Get(ctx, testTenant, wantID)
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
	firstTask, found, err := sts.Get(ctx, testTenant, wantID)
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

	task, found, err := sts.Get(ctx, testTenant, wantID)
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
	task, found, err := sts.Get(ctx, testTenant, wantID)
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
	task, found, err := sts.Get(ownerCtx, testTenant, wantID)
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
	if _, found, _ := sts.Get(ctx, testTenant, openTaskID); !found {
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

	if _, found, err := sts.Get(ctx, testTenant, openTaskID); err != nil {
		t.Fatalf("Get(openTaskID): %v", err)
	} else if found {
		t.Errorf("expected OPEN's scheduled task %q to be cancelled after leaving the state", openTaskID)
	}

	reviewTaskID := taskID(testTenant, "cancel-e1", "REVIEW", "AutoExpire")
	if _, found, err := sts.Get(ctx, testTenant, reviewTaskID); err != nil {
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

// --- Scheduled-task store failures must be marked as infrastructure ---

// failingReconcileTaskStore delegates everything except ReconcileForEntity,
// which always fails. Models the store call the settle-time arm/cancel pass
// makes on every save.
type failingReconcileTaskStore struct {
	spi.ScheduledTaskStore
	err error
}

func (s *failingReconcileTaskStore) ReconcileForEntity(context.Context, spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	return nil, s.err
}

// failingReconcileTaskFactory fails either the store lookup itself or the
// reconcile write, depending on which field is set.
type failingReconcileTaskFactory struct {
	spi.StoreFactory
	reconcileErr error
	lookupErr    error
}

func (f *failingReconcileTaskFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	real, err := f.StoreFactory.ScheduledTaskStore(ctx)
	if err != nil {
		return nil, err
	}
	return &failingReconcileTaskStore{ScheduledTaskStore: real, err: f.reconcileErr}, nil
}

// TestReconcile_StoreFailureIsMarkedInfra — the settle-time arm/cancel pass
// touches the scheduled-task store on EVERY save of an entity whose workflow
// carries a schedule. A store failure there is a server-side condition that can
// never be attributed to the caller's input, so it must carry an infrastructure
// marker: without one it reaches the entity service's catch-all, which mints a
// 400 WORKFLOW_FAILED whose detail is the raw error text — driver wording and a
// SQLSTATE, straight onto the wire.
func TestReconcile_StoreFailureIsMarkedInfra(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	// The exact shape a cancelled statement produces, and exactly what must
	// never reach a client.
	storeErr := errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

	cases := []struct {
		name string
		wrap func(spi.StoreFactory) spi.StoreFactory
	}{
		{"reconcile write", func(base spi.StoreFactory) spi.StoreFactory {
			return &failingReconcileTaskFactory{StoreFactory: base, reconcileErr: storeErr}
		}},
		{"store lookup", func(base spi.StoreFactory) spi.StoreFactory {
			return &failingReconcileTaskFactory{StoreFactory: base, lookupErr: storeErr}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := memory.NewStoreFactory()
			t.Cleanup(func() { base.Close() })
			uuids := common.NewTestUUIDGenerator()
			txMgr := base.NewTransactionManager(uuids)
			engine := NewEngine(tc.wrap(base), uuids, txMgr, WithScheduledClock(fixedClock(nowMs)))

			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "sched-infra", ModelVersion: "1.0"}
			saveWorkflow(t, base, ctx, modelRef, []spi.WorkflowDefinition{{
				Version: "1.1", Name: "SchedInfraWF", InitialState: "OPEN", Active: true,
				States: map[string]spi.StateDefinition{
					"OPEN": {Transitions: []spi.TransitionDefinition{
						{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
					}},
					"CLOSED": {},
				},
			}})

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			t.Cleanup(func() { _ = txMgr.Rollback(ctx, txID) })

			entity := makeEntity("sched-infra-1", modelRef, map[string]any{})
			entity.Meta.TransactionID = txID

			_, err = engine.Execute(txCtx, entity, "")
			if err == nil {
				t.Fatal("expected the scheduled-task store failure to surface")
			}
			if !errors.Is(err, ErrScheduledTaskInfra) {
				t.Errorf("error must wrap ErrScheduledTaskInfra so callers can sanitize it; got: %v", err)
			}
			if !errors.Is(err, storeErr) {
				t.Errorf("error must keep the store cause in the chain for logging; got: %v", err)
			}
		})
	}
}

// armsOnSchedule is the one arm rule.
func TestArmsOnSchedule(t *testing.T) {
	sched := &spi.TransitionSchedule{DelayMs: 1000}
	for _, tc := range []struct {
		name string
		tr   spi.TransitionDefinition
		want bool
	}{
		{"scheduled", spi.TransitionDefinition{Name: "T", Schedule: sched}, true},
		{"not scheduled", spi.TransitionDefinition{Name: "T"}, false},
		{"scheduled but manual", spi.TransitionDefinition{Name: "T", Schedule: sched, Manual: true}, false},
		{"scheduled but disabled", spi.TransitionDefinition{Name: "T", Schedule: sched, Disabled: true}, false},
	} {
		if got := armsOnSchedule(&tc.tr); got != tc.want {
			t.Errorf("%s: armsOnSchedule = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The model-level flag counts a workflow whether it is active or not, and
// counts only a transition the arm rule arms: a model whose only scheduled
// transitions are manual or disabled has no task to reconcile.
func TestModelHasSchedule_CountsOnlyArmingTransitions(t *testing.T) {
	sched := &spi.TransitionSchedule{DelayMs: 1000}
	wf := func(active bool, tr spi.TransitionDefinition) spi.WorkflowDefinition {
		return spi.WorkflowDefinition{Version: "1.1", Name: "wf", InitialState: "OPEN", Active: active,
			States: map[string]spi.StateDefinition{"OPEN": {Transitions: []spi.TransitionDefinition{tr}}, "CLOSED": {}}}
	}
	for _, tc := range []struct {
		name string
		wfs  []spi.WorkflowDefinition
		want bool
	}{
		{"no workflow", nil, false},
		{"active, arming", []spi.WorkflowDefinition{wf(true, spi.TransitionDefinition{Name: "T", Next: "CLOSED", Schedule: sched})}, true},
		{"inactive, arming", []spi.WorkflowDefinition{wf(false, spi.TransitionDefinition{Name: "T", Next: "CLOSED", Schedule: sched})}, true},
		{"manual schedule only", []spi.WorkflowDefinition{wf(true, spi.TransitionDefinition{Name: "T", Next: "CLOSED", Schedule: sched, Manual: true})}, false},
		{"disabled schedule only", []spi.WorkflowDefinition{wf(true, spi.TransitionDefinition{Name: "T", Next: "CLOSED", Schedule: sched, Disabled: true})}, false},
		{"no schedule", []spi.WorkflowDefinition{wf(true, spi.TransitionDefinition{Name: "T", Next: "CLOSED"})}, false},
	} {
		if got := modelHasSchedule(tc.wfs); got != tc.want {
			t.Errorf("%s: modelHasSchedule = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// loopbackInTx runs Loopback on the stored entity in a transaction of its own and commits.
func loopbackInTx(t *testing.T, engine *Engine, factory spi.StoreFactory, ctx context.Context, entityID string) {
	t.Helper()
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(txCtx, entityID)
	if err != nil {
		t.Fatalf("Get entity: %v", err)
	}
	entity.Meta.TransactionID = txID
	if _, err := engine.Loopback(txCtx, entity); err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestReconcile_ModelLevelFlag_RemovesTaskArmedByAnotherWorkflow(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "flag-order", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{
		{Version: "1.1", Name: "kind-a-wf", InitialState: "OPEN", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
			States: map[string]spi.StateDefinition{
				"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
				"CLOSED": {},
			}},
		{Version: "1.1", Name: "kind-b-wf", InitialState: "OPEN", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
			States: map[string]spi.StateDefinition{
				"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "close", Next: "CLOSED", Manual: true}}},
				"CLOSED": {},
			}},
	})
	// Armed under kind-a-wf; the entity's data now binds it to kind-b-wf, which schedules nothing.
	seedFireEntity(t, factory, ctx, "flag-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{"kind": "b"})
	id := taskID(testTenant, "flag-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: nowMs + 1000, EntityID: "flag-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: nowMs})

	loopbackInTx(t, engine, factory, ctx, "flag-e1")

	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("a task the selected workflow does not arm must be removed at the next write")
	}
	if n := countAuditEvents(t, factory, ctx, "flag-e1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1", n)
	}
}

func TestReconcile_LoopbackStateNotInWorkflow_RemovesTasks(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "legacy-order", ModelVersion: "1.0"}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "wf", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
			"CLOSED": {},
		},
	}})
	seedFireEntity(t, factory, ctx, "legacy-e1", modelRef, "LEGACY", "seed-tx-1", map[string]any{})
	id := taskID(testTenant, "legacy-e1", "LEGACY", "Tick")
	armTask(t, factory, ctx, spi.ScheduledTask{ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: nowMs + 1000, EntityID: "legacy-e1", ModelName: modelRef.EntityName,
		Transition: "Tick", SourceState: "LEGACY", ArmedAt: nowMs})

	loopbackInTx(t, engine, factory, ctx, "legacy-e1")

	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("a write to an entity whose state the selected workflow does not declare must remove its tasks")
	}
	if n := countAuditEvents(t, factory, ctx, "legacy-e1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1", n)
	}
}

func TestReconcile_FailedTaskReArmedAsNewLife(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "failed-e1", oneHopWF("CLOSED", nil, nil))
	ref := spi.TaskRef{TenantID: testTenant, ID: claimed.ID, ArmToken: claimed.ArmToken, ClaimToken: claimed.Claim.Token}
	if err := env.sts.MarkUnsafe(env.ctx, ref); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := env.sts.Fail(env.ctx, ref, spi.Failure{Reason: spi.FailureUnsafeWorkNotCompleted, Error: "x", AtMs: env.nowMs()}); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	loopbackInTx(t, env.engine, env.factory, env.ctx, "failed-e1")

	got, found := env.task(t, claimed.ID)
	if !found {
		t.Fatal("an update in the state must re-arm the FAILED task")
	}
	if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting || got.Attempts != 0 ||
		got.LostOwners != 0 || got.UnsafeMarked || got.FailureReason != "" || got.LastError != "" || got.Claim != nil {
		t.Errorf("task = %+v, want a fresh WAITING life with no mark", got)
	}
}

func TestReconcile_FailedTaskCancelledWhenEntityLeavesState(t *testing.T) {
	env := newRunEnv(t, nil)
	wf := oneHopWF("CLOSED", nil, nil)
	st := wf.States["OPEN"]
	st.Transitions = append(st.Transitions, spi.TransitionDefinition{Name: "advance", Next: "CLOSED", Manual: true})
	wf.States["OPEN"] = st
	claimed := env.claimed(t, "leave-e1", wf)
	ref := spi.TaskRef{TenantID: testTenant, ID: claimed.ID, ArmToken: claimed.ArmToken, ClaimToken: claimed.Claim.Token}
	if err := env.sts.Fail(env.ctx, ref, spi.Failure{Reason: spi.FailureOwnerLostRepeatedly, AtMs: env.nowMs()}); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, _ := env.factory.EntityStore(txCtx)
	entity, err := es.Get(txCtx, "leave-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Meta.TransactionID = txID
	if _, err := env.engine.ManualTransition(txCtx, entity, "advance"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := env.txMgr.Commit(env.ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, found := env.task(t, claimed.ID); found {
		t.Error("a FAILED task must be removed when the entity leaves its state")
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "leave-e1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1", n)
	}
}

func TestReconcile_ReArmResetsPartialCommit(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "partial-e1", oneHopWF("CLOSED", nil, nil))
	ref := spi.TaskRef{TenantID: testTenant, ID: claimed.ID, ArmToken: claimed.ArmToken, ClaimToken: claimed.Claim.Token}
	if err := env.sts.StampSegment(env.ctx, ref, true); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}

	loopbackInTx(t, env.engine, env.factory, env.ctx, "partial-e1")

	if got, _ := env.task(t, claimed.ID); got.PartialCommit {
		t.Errorf("task = %+v, want PartialCommit false on the new life", got)
	}
}

// countingWorkflowFactory counts WorkflowStore().Get calls.
type countingWorkflowFactory struct {
	spi.StoreFactory
	gets *int
}

type countingWorkflowStore struct {
	spi.WorkflowStore
	gets *int
}

func (f countingWorkflowFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	ws, err := f.StoreFactory.WorkflowStore(ctx)
	if err != nil {
		return nil, err
	}
	return countingWorkflowStore{WorkflowStore: ws, gets: f.gets}, nil
}

func (s countingWorkflowStore) Get(ctx context.Context, ref spi.ModelRef) ([]spi.WorkflowDefinition, error) {
	*s.gets++
	return s.WorkflowStore.Get(ctx, ref)
}

func TestReconcile_ModelFlagNeedsNoExtraWorkflowRead(t *testing.T) {
	gets := 0
	env := newRunEnvWith(t, nil, func(f spi.StoreFactory) spi.StoreFactory {
		return countingWorkflowFactory{StoreFactory: f, gets: &gets}
	}, nil)
	env.setup(t, "reads-e1", oneHopWF("CLOSED", nil, nil))
	gets = 0

	loopbackInTx(t, env.engine, env.factory, env.ctx, "reads-e1")

	if gets != 1 {
		t.Errorf("workflow reads per write = %d, want 1", gets)
	}
}

// executeInTx creates entity through Execute in a transaction of its own and commits.
func executeInTx(t *testing.T, engine *Engine, factory spi.StoreFactory, ctx context.Context, entity *spi.Entity) {
	t.Helper()
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity.Meta.TransactionID = txID
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// The arm filter applies the arm rule: a scheduled transition that is
// disabled or manual is not armed, even when the model has another
// transition that is (so reconcile runs).
func TestReconcile_DisabledScheduledTransitionNotArmed(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "disabled-order", ModelVersion: "1.0"}
	sched := &spi.TransitionSchedule{DelayMs: 1000}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "wf", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: sched, Disabled: true},
				{Name: "Escalate", Next: "CLOSED", Schedule: sched, Manual: true},
			}},
			"CLOSED": {Transitions: []spi.TransitionDefinition{{Name: "Reopen", Next: "OPEN", Schedule: sched}}},
		},
	}})

	executeInTx(t, engine, factory, ctx, makeEntity("disabled-e1", modelRef, map[string]any{}))

	for _, tr := range []string{"AutoClose", "Escalate"} {
		if _, found := getTask(t, factory, ctx, taskID(testTenant, "disabled-e1", "OPEN", tr)); found {
			t.Errorf("a task was armed for %s, which the arm rule does not arm", tr)
		}
	}
}

// With no workflow imported, the model-level flag is computed on the
// workflows the engine runs: the default workflow. A default workflow with a
// scheduled transition arms it.
func TestReconcile_ModelFlagCountsDefaultWorkflow(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	engine.defaultWorkflows = []spi.WorkflowDefinition{{
		Version: "1.1", Name: "default-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
			"CLOSED": {},
		},
	}}
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-wf-order", ModelVersion: "1.0"}

	executeInTx(t, engine, factory, ctx, makeEntity("default-e1", modelRef, map[string]any{}))

	if _, found := getTask(t, factory, ctx, taskID(testTenant, "default-e1", "OPEN", "AutoClose")); !found {
		t.Error("the default workflow's scheduled transition must be armed")
	}
}

// When stored workflows exist but none matches the entity, the engine falls
// back to the default workflow; the model-level flag counts it there too.
func TestReconcile_ModelFlagCountsDefaultWorkflowWhenNoneMatches(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	engine.defaultWorkflows = []spi.WorkflowDefinition{{
		Version: "1.1", Name: "default-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
			"CLOSED": {},
		},
	}}
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "unmatched-order", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "kind-a-wf", InitialState: "NEW", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States:    map[string]spi.StateDefinition{"NEW": {}},
	}})

	executeInTx(t, engine, factory, ctx, makeEntity("unmatched-e1", modelRef, map[string]any{"kind": "b"}))

	if _, found := getTask(t, factory, ctx, taskID(testTenant, "unmatched-e1", "OPEN", "AutoClose")); !found {
		t.Error("the default workflow's scheduled transition must be armed when no stored workflow matches")
	}
}
