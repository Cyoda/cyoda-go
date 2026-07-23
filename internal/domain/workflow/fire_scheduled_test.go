package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// testTenantB is a second tenant distinct from testTenant, used only by the
// tenant-mismatch security test below: it plays the role of the attacker-
// asserted tenant in a forged dispatch payload (task.TenantID), while
// testTenant plays the victim whose real task/entity rows must be
// unaffected.
const testTenantB = spi.TenantID("test-tenant-b")

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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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
	// Finding #2 guard: the fired task must not also collect a spurious
	// SCHEDULED_TRANSITION_CANCEL from reconcile treating its own
	// (now-stale) SourceState as "left behind".
	if n := countAuditEvents(t, factory, ctx, "fire-e1", spi.SMEventScheduledTransitionCancelled); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 0 (the fired task must not self-cancel)", n)
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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

// TestFireScheduled_TenantMismatch_DropsWithoutDeletingVictimTask is the
// Gate-3 security regression: a forged dispatch whose task.ID is real (it
// names a genuine pending task belonging to testTenant, the "victim") but
// whose task.TenantID asserts a different tenant (testTenantB, the
// "attacker's" asserted identity) must have ZERO effect on the victim's
// row.
//
// This mirrors the real dispatch shape exactly: the ctx is built via
// scheduler.SystemUserContext(task.TenantID) — precisely what both
// LocalExecutor.Execute and the peer RPC handler do with the (here,
// attacker-controlled) task.TenantID field — scoping every tenant-aware
// store the engine opens (EntityStore, WorkflowStore, ...) to testTenantB.
// Only task.ID is authoritative; ScheduledTaskStore.Get is tenant-agnostic
// by design (see plugins/memory/store_factory.go's ScheduledTaskStore
// godoc), so it returns the VICTIM's real row (cur.TenantID == testTenant)
// even though the surrounding ctx/task assert testTenantB.
//
// Before the fix: cur.TenantID is never compared to task.TenantID, so the
// subsequent es.Get(txCtx, cur.EntityID) — scoped to testTenantB — misses
// the victim's entity (it lives under testTenant) and returns
// spi.ErrNotFound. That trips the "entity hard-deleted, self-heal" branch,
// which unconditionally deletes the task by ID — destroying the victim's
// live, legitimate pending task — and reports a silent OutcomeDropped, nil
// with no audit trail.
//
// After the fix: the tenant mismatch must be caught immediately after the
// task re-read, before the entity is ever touched, and must never invoke
// sts.Delete.
func TestFireScheduled_TenantMismatch_DropsWithoutDeletingVictimTask(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	victimCtx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "tenant-mismatch-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "TenantMismatchWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, victimCtx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, victimCtx, "victim-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	// The victim's real task ID, keyed off the victim's real tenant.
	id := taskID(testTenant, "victim-e1", "OPEN", "AutoClose")
	armTask(t, factory, victimCtx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "victim-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs) // due

	// Forged dispatch: real task.ID, but task.TenantID asserts testTenantB.
	// The ctx is built exactly as the real dispatch paths build it — scoped
	// to the (attacker-controlled) task.TenantID.
	forgedCtx := scheduler.SystemUserContext(testTenantB)
	forgedTask := spi.ScheduledTask{ID: id, TenantID: testTenantB}

	outcome, err := engine.FireScheduledTransition(forgedCtx, forgedTask)
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	// The victim's task row must survive untouched — read back using the
	// victim's own context (ScheduledTaskStore is tenant-agnostic, so any
	// ctx would do, but the victim's is the natural choice here).
	if _, found := getTask(t, factory, victimCtx, id); !found {
		t.Error("expected victim's real task to survive a tenant-mismatched forged dispatch")
	}
	// The victim's entity must not have fired.
	if got := getEntityState(t, factory, victimCtx, "victim-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged by forged cross-tenant dispatch)", got)
	}
	// No audit trail should be attributed to the victim's entity for this
	// forged, silently-dropped request.
	for _, et := range []spi.StateMachineEventType{
		spi.SMEventScheduledTransitionCancelled, spi.SMEventScheduledTransitionExpired,
		spi.SMEventScheduledTransitionFired, spi.SMEventTransitionCriterionNoMatch,
	} {
		if n := countAuditEvents(t, factory, victimCtx, "victim-e1", et); n != 0 {
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
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

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: openTaskID, TenantID: testTenant})
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

	// Finding #2 guard: neither the fired OPEN task nor the cascade should
	// leave any spurious SCHEDULED_TRANSITION_CANCEL behind.
	if n := countAuditEvents(t, factory, ctx, "casc-e1", spi.SMEventScheduledTransitionCancelled); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 0 (fired task must not self-cancel)", n)
	}
}

// TestFireScheduled_SiblingScheduledTaskStillCancelled proves Finding #2's
// fix doesn't overcorrect: reconcile must still cancel a genuine sibling —
// another pending scheduled task out of the SAME old state — while leaving
// the just-fired task's own (now-explicit) delete uncounted as a cancel.
func TestFireScheduled_SiblingScheduledTaskStillCancelled(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sibling-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "SiblingWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
				{Name: "AutoAbandon", Next: "ABANDONED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs * 10}},
			}},
			"CLOSED":    {},
			"ABANDONED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "sib-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	closeID := taskID(testTenant, "sib-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: closeID, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "sib-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	abandonID := taskID(testTenant, "sib-e1", "OPEN", "AutoAbandon")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: abandonID, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs*10, EntityID: "sib-e1", ModelName: modelRef.EntityName,
		Transition: "AutoAbandon", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs) // only AutoClose is due

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: closeID, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}

	if got := getEntityState(t, factory, ctx, "sib-e1"); got != "CLOSED" {
		t.Fatalf("entity state = %q, want CLOSED", got)
	}
	if _, found := getTask(t, factory, ctx, closeID); found {
		t.Error("expected AutoClose's own task deleted after fire")
	}
	if _, found := getTask(t, factory, ctx, abandonID); found {
		t.Error("expected AutoAbandon's task cancelled once the entity left OPEN")
	}
	if n := countAuditEvents(t, factory, ctx, "sib-e1", spi.SMEventScheduledTransitionFired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d, want 1", n)
	}
	// Exactly one cancel — for the genuine sibling AutoAbandon — not two.
	if n := countAuditEvents(t, factory, ctx, "sib-e1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want exactly 1 (sibling only, not the fired task)", n)
	}
}

// ptrInt64 returns a pointer to v — shorthand for constructing
// *spi.TransitionSchedule.TimeoutMs / spi.ScheduledTask.TimeoutMs literals.
func ptrInt64(v int64) *int64 { return &v }

// --- Attribution: durable ArmedBy seed, verify-or-abort, anchor stamp ---

// latestVersion returns the most recently persisted spi.EntityVersion for
// entityID (GetVersionHistory appends in insertion order, so the last entry
// is the anchor write this test suite is asserting against).
func latestVersion(t *testing.T, factory spi.StoreFactory, ctx context.Context, entityID string) spi.EntityVersion {
	t.Helper()
	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	versions, err := es.GetVersionHistory(ctx, entityID)
	if err != nil {
		t.Fatalf("GetVersionHistory: %v", err)
	}
	if len(versions) == 0 {
		t.Fatalf("expected at least one version for %q", entityID)
	}
	return versions[len(versions)-1]
}

// TestFireScheduled_AttributesToArmedByUser_IncludingCascade covers Task
// 13's core positive case: a task whose durable row carries a user ArmedBy
// fires under a plain system dispatch ctx (mirroring the real
// scheduler.SystemUserContext dispatch — no user identity on ctx at all),
// yet the anchor write attributes to the ARMING user, executed by system.
// The workflow cascades automatically past an intermediate state
// (OPEN->MID->DONE) before the single anchor persist, proving the
// cascade's net result — not just a bare one-hop fire — carries the same
// attribution.
func TestFireScheduled_AttributesToArmedByUser_IncludingCascade(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	modelRef := spi.ModelRef{EntityName: "attr-user-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AttrUserWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "MID", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"MID": {Transitions: []spi.TransitionDefinition{
				// Automated, unconditional — cascadeAutomated fires it
				// immediately after AutoClose lands the entity in MID, all
				// within the SAME fire transaction and the SAME anchor
				// persist.
				{Name: "AutoAdvance", Next: "DONE"},
			}},
			"DONE": {},
		},
	}
	setupCtx := ctxWithTenant(testTenant)
	saveWorkflow(t, factory, setupCtx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, setupCtx, "attr-user-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "attr-user-e1", "OPEN", "AutoClose")
	armTask(t, factory, setupCtx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "attr-user-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
		ArmedBy: spi.Principal{ID: "arm-user", Kind: spi.PrincipalUser},
	})

	advance(delayMs)

	// The real dispatch ctx carries no user identity at all — just the
	// synthesised system UserContext scheduler.LocalExecutor/the peer RPC
	// handler build. Attribution must come from the durable row, not ctx.
	dispatchCtx := scheduler.SystemUserContext(testTenant)
	outcome, err := engine.FireScheduledTransition(dispatchCtx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}
	if got := getEntityState(t, factory, setupCtx, "attr-user-e1"); got != "DONE" {
		t.Fatalf("entity state = %q, want DONE (cascade past MID)", got)
	}

	es, err := factory.EntityStore(setupCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(setupCtx, "attr-user-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wantUser := spi.Principal{ID: "arm-user", Kind: spi.PrincipalUser}
	if entity.Meta.ChangeUser != wantUser.ID {
		t.Errorf("ChangeUser = %q, want %q", entity.Meta.ChangeUser, wantUser.ID)
	}
	if entity.Meta.ChangeUserKind != wantUser.Kind {
		t.Errorf("ChangeUserKind = %v, want %v", entity.Meta.ChangeUserKind, wantUser.Kind)
	}
	if entity.Meta.ChangeExecutor != scheduler.SystemPrincipal() {
		t.Errorf("ChangeExecutor = %+v, want %+v", entity.Meta.ChangeExecutor, scheduler.SystemPrincipal())
	}

	// Anchor version, independently of Entity — matches the EntityVersion
	// shape the brief specifies (User/AttributedKind/Executor).
	v := latestVersion(t, factory, setupCtx, "attr-user-e1")
	if v.User != wantUser.ID {
		t.Errorf("version User = %q, want %q", v.User, wantUser.ID)
	}
	if v.AttributedKind != wantUser.Kind {
		t.Errorf("version AttributedKind = %v, want %v", v.AttributedKind, wantUser.Kind)
	}
	if v.Executor != scheduler.SystemPrincipal() {
		t.Errorf("version Executor = %+v, want %+v", v.Executor, scheduler.SystemPrincipal())
	}
}

// TestFireScheduled_LegacyZeroArmedBy_AttributesToSystem_NeverSchedulerString
// covers the legacy-row fallback: a task armed before ArmedBy existed (zero
// Principal) must attribute to the system principal — never the bare
// string "scheduler", which was never a valid PrincipalKind/identity.
func TestFireScheduled_LegacyZeroArmedBy_AttributesToSystem_NeverSchedulerString(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "attr-legacy-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AttrLegacyWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "attr-legacy-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "attr-legacy-e1", "OPEN", "AutoClose")
	// No ArmedBy set — the zero Principal, exactly as a pre-attribution
	// legacy row would have persisted.
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "attr-legacy-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(ctx, "attr-legacy-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entity.Meta.ChangeUser != scheduler.SystemPrincipal().ID {
		t.Errorf("ChangeUser = %q, want %q", entity.Meta.ChangeUser, scheduler.SystemPrincipal().ID)
	}
	if entity.Meta.ChangeUser == "scheduler" {
		t.Error("ChangeUser must never be the bare string \"scheduler\"")
	}
	if entity.Meta.ChangeUserKind != spi.PrincipalSystem {
		t.Errorf("ChangeUserKind = %v, want %v", entity.Meta.ChangeUserKind, spi.PrincipalSystem)
	}
	if entity.Meta.ChangeExecutor != scheduler.SystemPrincipal() {
		t.Errorf("ChangeExecutor = %+v, want %+v", entity.Meta.ChangeExecutor, scheduler.SystemPrincipal())
	}
}

// TestFireScheduled_ForgedTaskArgArmedByIgnored_DurableRowWins is the
// negative regression for the trust contract (§9): the RPC-deserialized
// task ARGUMENT's ArmedBy must never be consulted — only the durable row,
// re-read by task.ID, is trusted. A forged argument asserting a different
// principal must have zero effect on attribution.
func TestFireScheduled_ForgedTaskArgArmedByIgnored_DurableRowWins(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "attr-forged-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AttrForgedWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "attr-forged-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "attr-forged-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "attr-forged-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
		ArmedBy: spi.Principal{ID: "real-user", Kind: spi.PrincipalUser},
	})

	advance(delayMs)

	// Forged dispatch: the argument's ArmedBy asserts a DIFFERENT principal
	// than the durable row's. Only task.ID is trusted from this argument.
	forgedTask := spi.ScheduledTask{
		ID: id, TenantID: testTenant,
		ArmedBy: spi.Principal{ID: "attacker", Kind: spi.PrincipalUser},
	}

	outcome, err := engine.FireScheduledTransition(ctx, forgedTask)
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(ctx, "attr-forged-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entity.Meta.ChangeUser != "real-user" {
		t.Errorf("ChangeUser = %q, want %q (the durable row's ArmedBy, not the forged argument)", entity.Meta.ChangeUser, "real-user")
	}
	if entity.Meta.ChangeUser == "attacker" {
		t.Error("ChangeUser must never be the forged task-argument ArmedBy")
	}
}

// raceInjectingScheduledTaskStore wraps a real spi.ScheduledTaskStore. Get
// fires a caller-supplied hook once — the FIRST time it is called for the
// target ID, right after fetching (and before returning) the pre-race
// snapshot — simulating a concurrent re-arm landing in the window between
// FireScheduledTransition's pre-Begin point-read and its in-tx re-read.
type raceInjectingScheduledTaskStore struct {
	spi.ScheduledTaskStore
	targetID string
	hook     func()
	fired    bool
}

func (s *raceInjectingScheduledTaskStore) Get(ctx context.Context, id string) (*spi.ScheduledTask, bool, error) {
	task, found, err := s.ScheduledTaskStore.Get(ctx, id)
	if err == nil && found && id == s.targetID && !s.fired {
		s.fired = true
		if s.hook != nil {
			s.hook()
		}
	}
	return task, found, err
}

// raceInjectingTaskFactory wraps a real spi.StoreFactory, returning a
// (cached, across calls) raceInjectingScheduledTaskStore from
// ScheduledTaskStore() while delegating every other store unchanged —
// mirrors raceInjectingFactory above, but for the task store instead of the
// entity store.
type raceInjectingTaskFactory struct {
	spi.StoreFactory
	targetID string
	hook     func()
	store    *raceInjectingScheduledTaskStore
}

func (f *raceInjectingTaskFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	real, err := f.StoreFactory.ScheduledTaskStore(ctx)
	if err != nil {
		return nil, err
	}
	if f.store == nil {
		f.store = &raceInjectingScheduledTaskStore{ScheduledTaskStore: real, targetID: f.targetID, hook: f.hook}
	} else {
		f.store.ScheduledTaskStore = real
	}
	return f.store, nil
}

// TestFireScheduled_VerifyOrAbort_ArmedByChangedConcurrently is the
// verify-or-abort regression: the arming principal changes (a concurrent
// re-arm) between FireScheduledTransition's pre-Begin point-read (which
// seeds the ambient origin) and its in-tx re-read. The fire must fail
// closed — abort with an error, commit nothing — rather than silently
// attribute the fire to the now-stale seeded principal.
func TestFireScheduled_VerifyOrAbort_ArmedByChangedConcurrently(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := realFactory.NewTransactionManager(uuids)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "race-armedby-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "RaceArmedByWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, realFactory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, realFactory, ctx, "race-armedby-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "race-armedby-e1", "OPEN", "AutoClose")
	armTask(t, realFactory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs, EntityID: "race-armedby-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
		ArmedBy: spi.Principal{ID: "orig-user", Kind: spi.PrincipalUser},
	})

	racingFactory := &raceInjectingTaskFactory{StoreFactory: realFactory, targetID: id}
	racingFactory.hook = func() {
		// A concurrent re-arm: some OTHER path (a racing loopback save)
		// changes the arming principal between the point-read and the
		// in-tx re-read, independent of the fire in progress. Upserted with
		// a plain (non-tx) ctx, so it applies immediately — matching a
		// genuinely concurrent writer's already-committed change.
		armTask(t, realFactory, ctx, spi.ScheduledTask{
			ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: armMs, EntityID: "race-armedby-e1", ModelName: modelRef.EntityName,
			Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
			ArmedBy: spi.Principal{ID: "racer-user", Kind: spi.PrincipalUser},
		})
	}

	engine := NewEngine(racingFactory, uuids, txMgr, WithScheduledClock(fixedClock(armMs)))

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err == nil {
		t.Fatal("expected FireScheduledTransition to return an error (arming principal changed concurrently)")
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	// Nothing committed: the entity never fired.
	if got := getEntityState(t, realFactory, ctx, "race-armedby-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged; verify-or-abort must commit nothing)", got)
	}
	// The task itself survives (the racer's concurrent re-arm stands,
	// untouched by the aborted fire) for the next scan to retry against the
	// now-current ArmedBy.
	task, found := getTask(t, realFactory, ctx, id)
	if !found {
		t.Fatal("expected task to remain for retry after the aborted fire")
	}
	wantArmedBy := spi.Principal{ID: "racer-user", Kind: spi.PrincipalUser}
	if task.ArmedBy != wantArmedBy {
		t.Errorf("task.ArmedBy = %+v, want %+v (the racer's concurrent re-arm, untouched by the aborted fire)", task.ArmedBy, wantArmedBy)
	}
}

// --- Exact grace-band boundary tests (design §5.5's strict ">" comparisons) ---

func TestFireScheduled_GraceBoundary_LatenessEqualsTimeout_Fires(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const timeoutMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "boundary-eq-timeout", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "BoundaryEqTimeoutWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs, TimeoutMs: ptrInt64(timeoutMs)}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "eq-timeout-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "eq-timeout-e1", "OPEN", "AutoClose")
	scheduledTime := armMs + delayMs
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime, TimeoutMs: ptrInt64(timeoutMs),
		EntityID: "eq-timeout-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	// lateness == timeoutMs exactly: NOT > timeoutMs, so the grace-band gate
	// falls through to fire (design's strict ">" comparisons).
	advance(delayMs + timeoutMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired (lateness == timeout is not '>' timeout)", outcome)
	}
	if got := getEntityState(t, factory, ctx, "eq-timeout-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
}

func TestFireScheduled_GraceBoundary_LatenessEqualsTimeoutPlusGrace_DropsAndWaits(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const timeoutMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "boundary-eq-grace", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "BoundaryEqGraceWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs, TimeoutMs: ptrInt64(timeoutMs)}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "eq-grace-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "eq-grace-e1", "OPEN", "AutoClose")
	scheduledTime := armMs + delayMs
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime, TimeoutMs: ptrInt64(timeoutMs),
		EntityID: "eq-grace-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	grace := defaultExpiryGraceMs
	// lateness == timeout+grace exactly: NOT > timeout+grace (expire gate),
	// but IS > timeout (grace-band gate) -> drop-and-wait, row remains.
	advance(delayMs + timeoutMs + grace)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped (lateness == timeout+grace is not '>' timeout+grace)", outcome)
	}
	if got := getEntityState(t, factory, ctx, "eq-grace-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (unchanged)", got)
	}
	if _, found := getTask(t, factory, ctx, id); !found {
		t.Error("expected task to remain at the exact timeout+grace boundary")
	}
	if n := countAuditEvents(t, factory, ctx, "eq-grace-e1", spi.SMEventScheduledTransitionExpired); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_EXPIRE events = %d, want 0 at the boundary", n)
	}
}

func TestFireScheduled_GraceBoundary_LatenessEqualsTimeoutPlusGracePlusOne_Expires(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const timeoutMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "boundary-past-grace", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "BoundaryPastGraceWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs, TimeoutMs: ptrInt64(timeoutMs)}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "past-grace-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "past-grace-e1", "OPEN", "AutoClose")
	scheduledTime := armMs + delayMs
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime, TimeoutMs: ptrInt64(timeoutMs),
		EntityID: "past-grace-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	grace := defaultExpiryGraceMs
	// lateness == timeout+grace+1: strictly > timeout+grace -> Expired.
	advance(delayMs + timeoutMs + grace + 1)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeExpired {
		t.Fatalf("outcome = %v, want Expired (lateness == timeout+grace+1 is '>' timeout+grace)", outcome)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected task deleted after expiry")
	}
	if n := countAuditEvents(t, factory, ctx, "past-grace-e1", spi.SMEventScheduledTransitionExpired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_EXPIRE events = %d, want 1", n)
	}
}

// --- CBD-segmenting fire tests (Finding #1: post-segmentation tx leak) ---

// txLeakTracker wraps a spi.TransactionManager and tracks which begun
// transactions are still open (neither committed nor rolled back). Tests use
// openCount() after FireScheduledTransition returns to prove no segment was
// left dangling — the memory backend has no exported active-tx inspector, so
// this is a minimal purpose-built substitute for one, mirroring the
// begins/commits/rollbacks bookkeeping countingTxManager already uses
// elsewhere in this package's test suite.
type txLeakTracker struct {
	inner spi.TransactionManager
	open  map[string]bool
}

func newTxLeakTracker(inner spi.TransactionManager) *txLeakTracker {
	return &txLeakTracker{inner: inner, open: make(map[string]bool)}
}

func (t *txLeakTracker) openCount() int { return len(t.open) }

func (t *txLeakTracker) Begin(ctx context.Context) (string, context.Context, error) {
	id, nctx, err := t.inner.Begin(ctx)
	if err == nil {
		t.open[id] = true
	}
	return id, nctx, err
}

func (t *txLeakTracker) Commit(ctx context.Context, txID string) error {
	err := t.inner.Commit(ctx, txID)
	if err == nil {
		delete(t.open, txID)
	}
	return err
}

func (t *txLeakTracker) Rollback(ctx context.Context, txID string) error {
	err := t.inner.Rollback(ctx, txID)
	// A rollback resolves the segment regardless of whether the underlying
	// manager reports an error (e.g. rolling back an already-committed
	// TX_pre is a documented safe no-op) — either way it is no longer a
	// leak candidate.
	delete(t.open, txID)
	return err
}

func (t *txLeakTracker) Join(ctx context.Context, txID string) (context.Context, error) {
	return t.inner.Join(ctx, txID)
}

func (t *txLeakTracker) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	return t.inner.GetSubmitTime(ctx, txID)
}

func (t *txLeakTracker) Savepoint(ctx context.Context, txID string) (string, error) {
	return t.inner.Savepoint(ctx, txID)
}

func (t *txLeakTracker) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	return t.inner.RollbackToSavepoint(ctx, txID, savepointID)
}

func (t *txLeakTracker) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	return t.inner.ReleaseSavepoint(ctx, txID, savepointID)
}

// setupEngineForCBDFire builds an engine wired to a leak-tracking tx manager
// and the given mock external-processing dispatcher, with a steppable clock
// starting at initialMs. maxStateVisits, when > 0, overrides the engine's
// default cascade-loop-protection limit (tests use a small value to trip the
// abort deterministically in a handful of iterations).
func setupEngineForCBDFire(t *testing.T, initialMs int64, mock *mockExternalProcessing, maxStateVisits int) (*Engine, spi.StoreFactory, *txLeakTracker, func(deltaMs int64)) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	rawTxMgr := factory.NewTransactionManager(uuids)
	tracker := newTxLeakTracker(rawTxMgr)
	clock, advance := steppableClock(initialMs)
	opts := []EngineOption{WithScheduledClock(clock), WithExternalProcessing(mock)}
	if maxStateVisits > 0 {
		opts = append(opts, WithMaxStateVisits(maxStateVisits))
	}
	engine := NewEngine(factory, uuids, tracker, opts...)
	return engine, factory, tracker, advance
}

// TestFireScheduled_CBDSegmentedFire_HappyPath drives a scheduled transition
// whose processor is COMMIT_BEFORE_DISPATCH, so the fire segments its
// transaction (TX_pre commits, TX_post opens) exactly like a manual
// transition's cascade would. Asserts the segmented fire completes,
// persists, and leaves no open transaction behind.
func TestFireScheduled_CBDSegmentedFire_HappyPath(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			return e, nil
		},
	}
	engine, factory, tracker, advance := setupEngineForCBDFire(t, armMs, mock, 0)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-fire-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CBDFireWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "MID", Schedule: &spi.TransitionSchedule{DelayMs: delayMs},
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			"MID": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "cbd-fire-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "cbd-fire-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "cbd-fire-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("outcome = %v, want Fired", outcome)
	}
	if got := getEntityState(t, factory, ctx, "cbd-fire-e1"); got != "MID" {
		t.Errorf("entity state = %q, want MID (segmented persist must land)", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected task deleted after segmented fire")
	}
	if n := countAuditEvents(t, factory, ctx, "cbd-fire-e1", spi.SMEventScheduledTransitionFired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d, want 1", n)
	}
	if got := tracker.openCount(); got != 0 {
		t.Errorf("open tx count after successful segmented fire = %d, want 0 (no leaked segment)", got)
	}
}

// TestFireScheduled_CBDSegmentedFire_ErrorAfterTXPre_NoLeak is the Finding #1
// regression: a COMMIT_BEFORE_DISPATCH processor commits TX_pre and opens
// TX_post, then a LATER failure — here, cascadeAutomated's own
// maxStateVisits abort on an unconditional self-loop in the post-segment
// state — occurs entirely outside executeProcessors (which already
// rolls back its own segment on a processor failure; this failure is NOT a
// processor failure, so that safety net does not apply). Before the fix,
// FireScheduledTransition's deferred rollback only ever targeted the entry
// txID (already committed as TX_pre, so the rollback silently no-ops) and
// TX_post leaked. Asserts OutcomeDropped, a non-nil error, and — critically —
// that the tx manager has no open transaction left afterward.
func TestFireScheduled_CBDSegmentedFire_ErrorAfterTXPre_NoLeak(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			return e, nil
		},
	}
	// maxStateVisits=2: MID's unconditional self-loop trips the abort on its
	// 3rd visit — a handful of deterministic iterations, no timing games.
	engine, factory, tracker, advance := setupEngineForCBDFire(t, armMs, mock, 2)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-leak-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CBDLeakWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "MID", Schedule: &spi.TransitionSchedule{DelayMs: delayMs},
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			// MID's own automated, unconditional self-loop — cascadeAutomated
			// keeps re-firing it (no processors, so no further segmenting)
			// until the per-state visit limit aborts the cascade. TX_post
			// (opened by cbd-proc above) is still open at that point.
			"MID": {Transitions: []spi.TransitionDefinition{
				{Name: "Loop", Next: "MID"},
			}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "cbd-leak-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "cbd-leak-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "cbd-leak-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err == nil {
		t.Fatal("expected FireScheduledTransition to return an error (cascade abort after segmenting)")
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	// The entity was never persisted past TX_pre — the state advance to MID
	// and everything after it lived only in TX_post's buffer, which must
	// have been rolled back.
	if got := getEntityState(t, factory, ctx, "cbd-leak-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (segmented cascade never persisted)", got)
	}
	// The task was never explicitly deleted (that only happens after a
	// successful cascade, before reconcile) and the abort's tx rolled back,
	// so it remains for the next scan to retry.
	if _, found := getTask(t, factory, ctx, id); !found {
		t.Error("expected task to remain for retry after the aborted fire")
	}

	if got := tracker.openCount(); got != 0 {
		t.Errorf("open tx count after aborted segmented fire = %d, want 0 (TX_post must not leak)", got)
	}
}

// --- Guard/CAS race test (best-effort; see report for the determinism note) ---

// raceInjectingEntityStore wraps a real spi.EntityStore. The first time Get
// is called for the target entity ID, it fires a caller-supplied hook AFTER
// fetching (and before returning) the pre-race snapshot — simulating a
// concurrent writer's commit landing in the window between
// FireScheduledTransition's re-read guard and its final CompareAndSave. This
// is a deterministic simulation of the race window (there is no goroutine
// timing to win), not a true concurrent race, but it exercises exactly the
// CAS-conflict code path a genuine race would hit.
type raceInjectingEntityStore struct {
	spi.EntityStore
	targetID string
	hook     func()
	fired    bool
}

func (s *raceInjectingEntityStore) Get(ctx context.Context, id string) (*spi.Entity, error) {
	e, err := s.EntityStore.Get(ctx, id)
	if err == nil && id == s.targetID && !s.fired {
		s.fired = true
		if s.hook != nil {
			s.hook()
		}
	}
	return e, err
}

// raceInjectingFactory wraps a real spi.StoreFactory, returning a
// raceInjectingEntityStore from EntityStore() while delegating every other
// store unchanged.
type raceInjectingFactory struct {
	spi.StoreFactory
	targetID string
	hook     func()
	store    *raceInjectingEntityStore
}

func (f *raceInjectingFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	real, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	if f.store == nil {
		f.store = &raceInjectingEntityStore{EntityStore: real, targetID: f.targetID, hook: f.hook}
	} else {
		f.store.EntityStore = real
	}
	return f.store, nil
}

// TestFireScheduled_GuardCASRace_DropsWithoutTornWrite simulates a
// concurrent write landing between FireScheduledTransition's re-read guard
// (which captures expectedTxID) and its final CompareAndSave: a competing,
// already-committed write bumps the entity's TransactionID and state via a
// hook fired right after the guard's Get. The fire proceeds on its
// now-stale snapshot, so its terminal CompareAndSave must lose the CAS —
// asserting OutcomeDropped, a conflict error, the entity reflecting the
// competing write (not a torn/partial write), and the task surviving for
// retry.
func TestFireScheduled_GuardCASRace_DropsWithoutTornWrite(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := realFactory.NewTransactionManager(uuids)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "race-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "RaceWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, realFactory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, realFactory, ctx, "race-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "race-e1", "OPEN", "AutoClose")
	armTask(t, realFactory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs, EntityID: "race-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	racingFactory := &raceInjectingFactory{StoreFactory: realFactory, targetID: "race-e1"}
	racingFactory.hook = func() {
		// A competing, already-committed write: some OTHER path advances
		// the entity to a different state under a different txID, entirely
		// independent of the fire in progress.
		seedFireEntity(t, realFactory, ctx, "race-e1", modelRef, "RACED", "racer-tx-1", map[string]any{"racer": true})
	}

	engine := NewEngine(racingFactory, uuids, txMgr, WithScheduledClock(fixedClock(armMs)))

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err == nil {
		t.Fatal("expected FireScheduledTransition to return a CAS-conflict error")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrConflict)", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}

	// No torn write: the entity reflects exactly the competing writer's
	// commit, not a partial/mixed state from the losing fire.
	if got := getEntityState(t, realFactory, ctx, "race-e1"); got != "RACED" {
		t.Errorf("entity state = %q, want RACED (the competing write, untouched by the losing fire)", got)
	}
	// The task survives for the next scan to retry against the new state.
	if _, found := getTask(t, realFactory, ctx, id); !found {
		t.Error("expected task to remain for retry after losing the CAS race")
	}
}
