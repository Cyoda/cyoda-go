package workflow

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// seedFireEntity saves entity id directly to the EntityStore in state,
// bypassing engine.Execute/reconcile entirely — FireScheduledTransition
// reads the entity from the store, not from any in-memory reference, so
// tests exercising it need a durably-saved row. transactionID is stamped on
// the entity before saving so it becomes the row's "last committed" txID —
// the value FireScheduledTransition's guard captures as its CAS precondition.
func seedFireEntity(t *testing.T, factory spi.StoreFactory, ctx context.Context, id string, modelRef spi.ModelRef, state, transactionID string, data map[string]any) *spi.Entity {
	t.Helper()
	entity := makeEntity(id, modelRef, data)
	entity.Meta.State = state
	entity.Meta.TenantID = testTenant
	entity.Meta.TransactionID = transactionID
	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		t.Fatalf("Save entity: %v", err)
	}
	return entity
}

// armTask directly Upserts a ScheduledTask row, bypassing the engine's
// reconcile so each test can set exactly the ScheduledTime/TimeoutMs it
// needs to exercise a specific guard/grace-band branch.
func armTask(t *testing.T, factory spi.StoreFactory, ctx context.Context, task spi.ScheduledTask) {
	t.Helper()
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if err := sts.Upsert(ctx, task); err != nil {
		t.Fatalf("Upsert task: %v", err)
	}
}

// getTask reads back a ScheduledTask by id (test helper: fails the test on
// a store error, but returns found=false as a normal, assertable result).
func getTask(t *testing.T, factory spi.StoreFactory, ctx context.Context, id string) (*spi.ScheduledTask, bool) {
	t.Helper()
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	task, found, err := sts.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	return task, found
}

// getEntityState reads back an entity's current state from the store.
func getEntityState(t *testing.T, factory spi.StoreFactory, ctx context.Context, id string) string {
	t.Helper()
	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get entity: %v", err)
	}
	return entity.Meta.State
}

// countAuditEvents returns how many of entityID's audit events carry
// eventType.
func countAuditEvents(t *testing.T, factory spi.StoreFactory, ctx context.Context, entityID string, eventType spi.StateMachineEventType) int {
	t.Helper()
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, entityID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.EventType == eventType {
			n++
		}
	}
	return n
}

func TestFireScheduled_FiresOnTime(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "FireWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "fire-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "fire-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs) // lateness == 0

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}

	if got := getEntityState(t, factory, ctx, "fire-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected task deleted after fire")
	}
	if n := countAuditEvents(t, factory, ctx, "fire-e1", spi.SMEventScheduledTransitionFired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d, want 1", n)
	}
}

func TestFireScheduled_DeclineOnCriterionFalse(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "decline-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "DeclineWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{
					Name: "AutoClose", Next: "CLOSED",
					Schedule:  &spi.TransitionSchedule{DelayMs: delayMs},
					Criterion: simpleCriterion("$.flag", "EQUALS", true),
				},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "decline-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{"flag": false})

	id := taskID(testTenant, "decline-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "decline-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDeclined {
		t.Fatalf("outcome = %v, want Declined", outcome)
	}

	if got := getEntityState(t, factory, ctx, "decline-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged)", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected task deleted after decline")
	}
	if n := countAuditEvents(t, factory, ctx, "decline-e1", spi.SMEventTransitionCriterionNoMatch); n != 1 {
		t.Errorf("criterion-no-match events = %d, want 1", n)
	}
}

func TestFireScheduled_ExpireBeyondGrace(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const timeoutMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "expire-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ExpireWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs, TimeoutMs: ptrInt64(timeoutMs)}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "expire-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "expire-e1", "OPEN", "AutoClose")
	scheduledTime := armMs + delayMs
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime, TimeoutMs: ptrInt64(timeoutMs),
		EntityID: "expire-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	grace := defaultExpiryGraceMs
	// now = scheduledTime + timeout + 2*grace: past timeout+grace, so Expired.
	advance(delayMs + timeoutMs + 2*grace)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeExpired {
		t.Fatalf("outcome = %v, want Expired", outcome)
	}

	if got := getEntityState(t, factory, ctx, "expire-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged)", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected task deleted after expiry")
	}
	if n := countAuditEvents(t, factory, ctx, "expire-e1", spi.SMEventScheduledTransitionExpired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_EXPIRE events = %d, want exactly 1 (delete-gated)", n)
	}
}

func TestFireScheduled_DropInGraceBand(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const timeoutMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "graceband-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "GraceWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs, TimeoutMs: ptrInt64(timeoutMs)}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "grace-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "grace-e1", "OPEN", "AutoClose")
	scheduledTime := armMs + delayMs
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime, TimeoutMs: ptrInt64(timeoutMs),
		EntityID: "grace-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	grace := defaultExpiryGraceMs
	// lateness = timeout + grace/2: strictly inside (timeout, timeout+grace].
	advance(delayMs + timeoutMs + grace/2)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	if got := getEntityState(t, factory, ctx, "grace-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged)", got)
	}
	if _, found := getTask(t, factory, ctx, id); !found {
		t.Error("expected task to remain in the grace band")
	}
	if n := countAuditEvents(t, factory, ctx, "grace-e1", spi.SMEventScheduledTransitionExpired); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_EXPIRE events = %d, want 0 (grace band must not expire)", n)
	}
	if n := countAuditEvents(t, factory, ctx, "grace-e1", spi.SMEventScheduledTransitionFired); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d, want 0", n)
	}
}

func TestFireScheduled_GuardEntityMovedOn(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "movedon-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "MovedOnWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	// Entity has already moved on to CLOSED by some other path — the stale
	// OPEN task below simulates a race where the reconcile hasn't (yet, or
	// ever will) clean it up.
	seedFireEntity(t, factory, ctx, "movedon-e1", modelRef, "CLOSED", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "movedon-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs, EntityID: "movedon-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(1) // due

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	if got := getEntityState(t, factory, ctx, "movedon-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED (untouched)", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected stale task deleted")
	}
	// Silent drop: no Cancelled/Expired/Fired/Declined audit event.
	for _, et := range []spi.StateMachineEventType{
		spi.SMEventScheduledTransitionCancelled, spi.SMEventScheduledTransitionExpired,
		spi.SMEventScheduledTransitionFired, spi.SMEventTransitionCriterionNoMatch,
	} {
		if n := countAuditEvents(t, factory, ctx, "movedon-e1", et); n != 0 {
			t.Errorf("event %v: got %d, want 0 (silent drop)", et, n)
		}
	}
}

func TestFireScheduled_GuardReArmedToFuture(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, _ := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "rearmed-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "RearmedWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "rearmed-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "rearmed-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, // due in the future relative to "now" below
		EntityID:      "rearmed-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	// Do NOT advance the clock — "now" (armMs) is before ScheduledTime.

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	if got := getEntityState(t, factory, ctx, "rearmed-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged)", got)
	}
	task, found := getTask(t, factory, ctx, id)
	if !found {
		t.Fatal("expected re-armed task to remain")
	}
	if task.ScheduledTime != armMs+delayMs {
		t.Errorf("ScheduledTime = %d, want %d (untouched)", task.ScheduledTime, armMs+delayMs)
	}
}

func TestFireScheduled_OrphanedTransitionDropped(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "orphan-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "OrphanWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "orphan-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "orphan-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "orphan-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs)

	// Simulate a workflow re-import that removes the AutoClose transition
	// from OPEN — the task now references a transition that no longer
	// exists.
	reimported := spi.WorkflowDefinition{
		Version: "1.2", Name: "OrphanWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{reimported})

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	if got := getEntityState(t, factory, ctx, "orphan-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged)", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected orphaned task deleted")
	}
}

func TestFireScheduled_CascadeAfterFire(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const nextDelayMs = int64(2000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cascade-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CascadeWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "MID", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"MID": {Transitions: []spi.TransitionDefinition{
				// Automated, unconditional — cascadeAutomated fires it
				// immediately after AutoClose lands the entity in MID.
				{Name: "AutoAdvance", Next: "DONE"},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoFinal", Next: "ARCHIVED", Schedule: &spi.TransitionSchedule{DelayMs: nextDelayMs}},
			}},
			"ARCHIVED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "casc-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	openTaskID := taskID(testTenant, "casc-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: openTaskID, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "casc-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs)
	fireNowMs := armMs + delayMs

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: openTaskID})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}

	// Cascade must have carried the entity all the way to DONE (MID's
	// AutoAdvance is automated and unconditional).
	if got := getEntityState(t, factory, ctx, "casc-e1"); got != "DONE" {
		t.Fatalf("entity state = %q, want DONE (cascade past MID)", got)
	}

	// The fired task is gone.
	if _, found := getTask(t, factory, ctx, openTaskID); found {
		t.Error("expected OPEN's task deleted after fire")
	}

	// DONE's own scheduled transition must be armed by the same reconcile,
	// atomically within the fire — using the fire's "now", not any later
	// wall-clock time.
	doneTaskID := taskID(testTenant, "casc-e1", "DONE", "AutoFinal")
	doneTask, found := getTask(t, factory, ctx, doneTaskID)
	if !found {
		t.Fatal("expected DONE's scheduled task armed after cascade")
	}
	if doneTask.ScheduledTime != fireNowMs+nextDelayMs {
		t.Errorf("DONE task ScheduledTime = %d, want %d", doneTask.ScheduledTime, fireNowMs+nextDelayMs)
	}
}

// ptrInt64 returns a pointer to v — shorthand for constructing
// *spi.TransitionSchedule.TimeoutMs / spi.ScheduledTask.TimeoutMs literals.
func ptrInt64(v int64) *int64 { return &v }
