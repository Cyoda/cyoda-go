package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Workflow-level selection is criterion-driven and applies on EVERY engine
// entry point, not just creation (`cyoda help workflows`, "Workflow-level
// selection"). The tests below all use the same shape: two active workflows
// on one model that DECLARE THE SAME STATE NAMES — the normal shape for a
// per-kind machine — distinguished only by their `criterion`. Resolving by
// "first active workflow that declares the entity's current state" cannot
// tell them apart and always picks the first, so every assertion here fails
// under that resolver and passes only under criterion-based selection.
//
// The transitions carry different `next` states per workflow, so the
// observed end state alone identifies which definition ran. No compute node
// is involved: the criteria are inline predicates over the payload.

// kindWorkflows builds two workflows selected on $.kind, both declaring
// state VALIDATE with a transition named `transitionName`, but landing in
// different target states. wfA is declared FIRST, so it is what a
// state-based resolver returns for every entity.
func kindWorkflows(transitionName string, manual bool) []spi.WorkflowDefinition {
	mk := func(name, kind, next string) spi.WorkflowDefinition {
		return spi.WorkflowDefinition{
			Version: "1.1", Name: name, InitialState: "VALIDATE", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", kind),
			States: map[string]spi.StateDefinition{
				"VALIDATE": {Transitions: []spi.TransitionDefinition{
					{Name: transitionName, Next: next, Manual: manual},
				}},
				next: {},
			},
		}
	}
	return []spi.WorkflowDefinition{
		mk("kind-a-wf", "a", "A_DONE"),
		mk("kind-b-wf", "b", "B_DONE"),
	}
}

// setupKindModel saves the two kind workflows and registers the $.kind field
// so inline criteria type-resolve.
func setupKindModel(t *testing.T, factory spi.StoreFactory, ctx context.Context, modelRef spi.ModelRef, workflows []spi.WorkflowDefinition) {
	t.Helper()
	saveWorkflow(t, factory, ctx, modelRef, workflows)
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{"kind": schema.String})
}

// auditDetailsFor returns the Details strings of entityID's audit events of
// the given type, in recorded order.
func auditDetailsFor(t *testing.T, factory spi.StoreFactory, ctx context.Context, entityID string, eventType spi.StateMachineEventType) []string {
	t.Helper()
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, entityID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	var out []string
	for _, ev := range events {
		if ev.EventType == eventType {
			out = append(out, ev.Details)
		}
	}
	return out
}

// TestManualTransition_ResolvesWorkflowByCriterion pins the defect from the
// report: a manual transition on a multi-workflow model must run the
// definition the entity's criterion selects, not the first one that happens
// to declare its current state.
func TestManualTransition_ResolvesWorkflowByCriterion(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "manual-select", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, kindWorkflows("check", true))

	entity := makeEntity("manual-b", modelRef, map[string]any{"kind": "b"})
	entity.Meta.State = "VALIDATE"

	if _, err := engine.ManualTransition(ctx, entity, "check"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if entity.Meta.State != "B_DONE" {
		t.Errorf("state = %q, want B_DONE (kind-b-wf selected by criterion); "+
			"A_DONE means the first workflow declaring VALIDATE was used instead", entity.Meta.State)
	}
}

// TestManualTransition_EmitsWorkflowSelectionAudit asserts the selection
// audit trail is restored on the manual path: the skipped workflow is
// recorded as WORKFLOW_SKIP and the selected one as WORKFLOW_FOUND, so an
// operator can see which definition ran.
func TestManualTransition_EmitsWorkflowSelectionAudit(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "manual-audit", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, kindWorkflows("check", true))

	entity := makeEntity("manual-audit-b", modelRef, map[string]any{"kind": "b"})
	entity.Meta.State = "VALIDATE"

	if _, err := engine.ManualTransition(ctx, entity, "check"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}

	skipped := auditDetailsFor(t, factory, ctx, "manual-audit-b", spi.SMEventWorkflowSkipped)
	if len(skipped) != 1 {
		t.Fatalf("WORKFLOW_SKIP events = %d (%v), want 1 (kind-a-wf)", len(skipped), skipped)
	}
	found := auditDetailsFor(t, factory, ctx, "manual-audit-b", spi.SMEventWorkflowFound)
	if len(found) != 1 {
		t.Fatalf("WORKFLOW_FOUND events = %d (%v), want 1 (kind-b-wf)", len(found), found)
	}
}

// TestManualTransition_StateAbsentFromSelectedWorkflow_Fails asserts the
// engine does NOT hop to another definition that happens to declare the
// entity's state. The entity selects kind-a-wf, which has no ORPHAN state;
// kind-b-wf does. That must fail, not silently run kind-b-wf's transition.
func TestManualTransition_StateAbsentFromSelectedWorkflow_Fails(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "manual-orphan", ModelVersion: "1.0"}

	wfA := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-a-wf", InitialState: "START", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States: map[string]spi.StateDefinition{
			"START": {},
		},
	}
	wfB := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-b-wf", InitialState: "ORPHAN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
		States: map[string]spi.StateDefinition{
			"ORPHAN": {Transitions: []spi.TransitionDefinition{{Name: "check", Next: "B_DONE", Manual: true}}},
			"B_DONE": {},
		},
	}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wfA, wfB})

	entity := makeEntity("manual-orphan-a", modelRef, map[string]any{"kind": "a"})
	entity.Meta.State = "ORPHAN"

	if _, err := engine.ManualTransition(ctx, entity, "check"); err == nil {
		t.Fatalf("ManualTransition succeeded and moved to %q; want an error — "+
			"ORPHAN is not in the criterion-selected workflow kind-a-wf", entity.Meta.State)
	}
	if entity.Meta.State != "ORPHAN" {
		t.Errorf("state = %q, want ORPHAN (unchanged)", entity.Meta.State)
	}
}

// TestLoopback_ResolvesWorkflowByCriterion is the automated-cascade
// counterpart: a loopback re-evaluation must cascade the definition the
// criterion selects.
func TestLoopback_ResolvesWorkflowByCriterion(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loopback-select", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, kindWorkflows("advance", false))

	entity := makeEntity("loopback-b", modelRef, map[string]any{"kind": "b"})
	entity.Meta.State = "VALIDATE"

	if _, err := engine.Loopback(ctx, entity); err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	if entity.Meta.State != "B_DONE" {
		t.Errorf("state = %q, want B_DONE (kind-b-wf selected by criterion)", entity.Meta.State)
	}
}

// TestLoopback_EmitsWorkflowSelectionAudit mirrors the manual-path audit
// assertion for the loopback door.
func TestLoopback_EmitsWorkflowSelectionAudit(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loopback-audit", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, kindWorkflows("advance", false))

	entity := makeEntity("loopback-audit-b", modelRef, map[string]any{"kind": "b"})
	entity.Meta.State = "VALIDATE"

	if _, err := engine.Loopback(ctx, entity); err != nil {
		t.Fatalf("Loopback: %v", err)
	}

	if skipped := auditDetailsFor(t, factory, ctx, "loopback-audit-b", spi.SMEventWorkflowSkipped); len(skipped) != 1 {
		t.Errorf("WORKFLOW_SKIP events = %d (%v), want 1", len(skipped), skipped)
	}
	if found := auditDetailsFor(t, factory, ctx, "loopback-audit-b", spi.SMEventWorkflowFound); len(found) != 1 {
		t.Errorf("WORKFLOW_FOUND events = %d (%v), want 1", len(found), found)
	}
}

// TestLoopback_StateAbsentFromSelectedWorkflow_IsStable asserts loopback
// keeps its STATE_NOT_IN_WORKFLOW stable-state contract when the state is
// absent from the SELECTED workflow — rather than cascading some other
// definition that declares it.
func TestLoopback_StateAbsentFromSelectedWorkflow_IsStable(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loopback-orphan", ModelVersion: "1.0"}

	wfA := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-a-wf", InitialState: "START", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States:    map[string]spi.StateDefinition{"START": {}},
	}
	wfB := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-b-wf", InitialState: "ORPHAN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
		States: map[string]spi.StateDefinition{
			"ORPHAN": {Transitions: []spi.TransitionDefinition{{Name: "advance", Next: "B_DONE", Manual: false}}},
			"B_DONE": {},
		},
	}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wfA, wfB})

	entity := makeEntity("loopback-orphan-a", modelRef, map[string]any{"kind": "a"})
	entity.Meta.State = "ORPHAN"

	res, err := engine.Loopback(ctx, entity)
	if err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	if entity.Meta.State != "ORPHAN" {
		t.Errorf("state = %q, want ORPHAN (unchanged — kind-b-wf must not cascade)", entity.Meta.State)
	}
	if res.StopReason != "STATE_NOT_IN_WORKFLOW" {
		t.Errorf("StopReason = %q, want STATE_NOT_IN_WORKFLOW", res.StopReason)
	}
}

// TestFireScheduled_ResolvesWorkflowByCriterion is the scheduler-door
// counterpart. Both workflows declare OPEN with a scheduled AutoClose, but
// land in different states; the fire must run the criterion-selected one.
func TestFireScheduled_ResolvesWorkflowByCriterion(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-select", ModelVersion: "1.0"}

	mk := func(name, kind, next string) spi.WorkflowDefinition {
		return spi.WorkflowDefinition{
			Version: "1.1", Name: name, InitialState: "OPEN", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", kind),
			States: map[string]spi.StateDefinition{
				"OPEN": {Transitions: []spi.TransitionDefinition{
					{Name: "AutoClose", Next: next, Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
				}},
				next: {},
			},
		}
	}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{
		mk("kind-a-wf", "a", "A_CLOSED"),
		mk("kind-b-wf", "b", "B_CLOSED"),
	})
	seedFireEntity(t, factory, ctx, "fire-select-b", modelRef, "OPEN", "seed-tx-1", map[string]any{"kind": "b"})

	id := taskID(testTenant, "fire-select-b", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-select-b", ModelName: modelRef.EntityName,
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
	if got := getEntityState(t, factory, ctx, "fire-select-b"); got != "B_CLOSED" {
		t.Errorf("entity state = %q, want B_CLOSED (kind-b-wf selected by criterion)", got)
	}
}

// TestFireScheduled_TransitionAbsentFromSelectedWorkflow_Dropped asserts the
// scheduler door drops (and self-heals) a task whose transition is not in
// the criterion-selected workflow, instead of firing another definition's
// same-named transition.
func TestFireScheduled_TransitionAbsentFromSelectedWorkflow_Dropped(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-orphan", ModelVersion: "1.0"}

	// kind-b-wf is declared FIRST and owns the AutoClose the stale task
	// names, so a state-based resolver fires it for every entity. kind-a-wf
	// — the one this entity's criterion selects — declares OPEN with no
	// scheduled transition at all.
	wfA := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-a-wf", InitialState: "OPEN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States:    map[string]spi.StateDefinition{"OPEN": {}},
	}
	wfB := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-b-wf", InitialState: "OPEN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "B_CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"B_CLOSED": {},
		},
	}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wfB, wfA})
	seedFireEntity(t, factory, ctx, "fire-orphan-a", modelRef, "OPEN", "seed-tx-1", map[string]any{"kind": "a"})

	id := taskID(testTenant, "fire-orphan-a", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-orphan-a", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}
	if got := getEntityState(t, factory, ctx, "fire-orphan-a"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (kind-b-wf's AutoClose must not fire)", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected the obsolete task to be deleted (self-heal)")
	}
}

// TestGetAvailableTransitions_ResolvesByCriterion pins that the query door
// reports the criterion-selected workflow's transitions.
func TestGetAvailableTransitions_ResolvesByCriterion(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "query-select", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, kindWorkflows("check", true))

	entity := makeEntity("query-b", modelRef, map[string]any{"kind": "b"})
	entity.Meta.State = "VALIDATE"

	names, err := engine.GetAvailableTransitionsForEntity(ctx, entity)
	if err != nil {
		t.Fatalf("GetAvailableTransitionsForEntity: %v", err)
	}
	if len(names) != 1 || names[0] != "check" {
		t.Fatalf("transitions = %v, want [check]", names)
	}
}

// TestGetAvailableTransitions_WritesNoAuditEvents asserts the query door is
// a pure read: workflow-selection events belong to an execution and have no
// transaction to key them to here, so none may be recorded.
func TestGetAvailableTransitions_WritesNoAuditEvents(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "query-audit", ModelVersion: "1.0"}
	setupKindModel(t, factory, ctx, modelRef, kindWorkflows("check", true))

	entity := makeEntity("query-audit-b", modelRef, map[string]any{"kind": "b"})
	entity.Meta.State = "VALIDATE"

	if _, err := engine.GetAvailableTransitionsForEntity(ctx, entity); err != nil {
		t.Fatalf("GetAvailableTransitionsForEntity: %v", err)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "query-audit-b")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("audit events after a transitions query = %d, want 0: %+v", len(events), events)
	}
}

// TestGetAvailableTransitions_CriterionErrorFailsClosed asserts the query
// door does not degrade to the default workflow when a workflow criterion
// cannot be evaluated. Returning some other workflow's transitions would be
// a wrong-but-available answer; the engine fails closed instead
// (.claude/rules/correctness-over-availability.md).
func TestGetAvailableTransitions_CriterionErrorFailsClosed(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "query-failclosed", ModelVersion: "1.0"}

	// A FUNCTION workflow criterion with no external processing service
	// configured: evaluateCriterion returns an error rather than a verdict.
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "fn-wf", InitialState: "VALIDATE", Active: true,
		Criterion: functionCriterion(),
		States: map[string]spi.StateDefinition{
			"VALIDATE": {Transitions: []spi.TransitionDefinition{{Name: "check", Next: "DONE", Manual: true}}},
			"DONE":     {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("query-failclosed-1", modelRef, map[string]any{"kind": "a"})
	entity.Meta.State = "VALIDATE"

	names, err := engine.GetAvailableTransitionsForEntity(ctx, entity)
	if err == nil {
		t.Fatalf("GetAvailableTransitionsForEntity returned %v with no error; "+
			"want the criterion-evaluation failure surfaced, not a default-workflow fallback", names)
	}
}

// TestManualTransition_CriterionErrorFailsClosed asserts the same fail-closed
// rule on the manual door: an unevaluable workflow criterion rejects the
// operation instead of running whichever definition declares the state.
func TestManualTransition_CriterionErrorFailsClosed(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "manual-failclosed", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "fn-wf", InitialState: "VALIDATE", Active: true,
		Criterion: functionCriterion(),
		States: map[string]spi.StateDefinition{
			"VALIDATE": {Transitions: []spi.TransitionDefinition{{Name: "check", Next: "DONE", Manual: true}}},
			"DONE":     {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("manual-failclosed-1", modelRef, map[string]any{"kind": "a"})
	entity.Meta.State = "VALIDATE"

	if _, err := engine.ManualTransition(ctx, entity, "check"); err == nil {
		t.Fatal("ManualTransition succeeded; want the criterion-evaluation failure surfaced")
	} else if errors.Is(err, ErrTransitionNotFound) {
		t.Errorf("error wraps ErrTransitionNotFound; want a criterion-evaluation failure: %v", err)
	}
	if entity.Meta.State != "VALIDATE" {
		t.Errorf("state = %q, want VALIDATE (unchanged)", entity.Meta.State)
	}
}

// --- Scheduler-door guards ---

// fnCriterionWorkflow builds a workflow whose SELECTION criterion is a
// FUNCTION with no external processing service configured, so resolution
// always fails. Used to exercise the fire door when the workflow cannot be
// resolved at all.
func fnCriterionWorkflow(transitionName string, delayMs int64) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1.1", Name: "fn-select-wf", InitialState: "OPEN", Active: true,
		Criterion: functionCriterion(),
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: transitionName, Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}
}

// TestFireScheduled_ExpiresEvenWhenWorkflowCannotBeResolved pins the ordering
// of the grace-band gate against workflow resolution. Expiry is a pure
// function of the durable row and the clock, so a task past
// TimeoutMs+grace must expire even while its workflow criterion cannot be
// evaluated. Resolving first made such a task unexpirable and
// unreclaimable — re-dispatched by the coordinator every backoff interval
// for as long as the compute member stayed down.
func TestFireScheduled_ExpiresEvenWhenWorkflowCannotBeResolved(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	const timeoutMs = int64(500)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-expire-unresolvable", ModelVersion: "1.0"}

	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{fnCriterionWorkflow("AutoClose", delayMs)})
	seedFireEntity(t, factory, ctx, "fire-expire-1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	timeout := timeoutMs
	id := taskID(testTenant, "fire-expire-1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, TimeoutMs: &timeout, EntityID: "fire-expire-1",
		ModelName: modelRef.EntityName, Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})

	// Well past TimeoutMs + the expiry grace band.
	advance(delayMs + timeoutMs + defaultExpiryGraceMs + 1000)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeExpired {
		t.Fatalf("outcome = %v, want Expired — expiry must not depend on resolving the workflow", outcome)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected the expired task to be deleted; an unresolvable workflow must not make it immortal")
	}
}

// TestFireScheduled_UnresolvableWorkflowLeavesTaskForRetry is the companion:
// before expiry is due, an unresolvable workflow must NOT fire and must NOT
// consume the task — it is left in place for the next scan (fail closed,
// then retry).
func TestFireScheduled_UnresolvableWorkflowLeavesTaskForRetry(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-retry-unresolvable", ModelVersion: "1.0"}

	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{fnCriterionWorkflow("AutoClose", delayMs)})
	seedFireEntity(t, factory, ctx, "fire-retry-1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "fire-retry-1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-retry-1",
		ModelName: modelRef.EntityName, Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err == nil {
		t.Fatal("expected the resolution failure to be surfaced")
	}
	if outcome != OutcomeDropped {
		t.Errorf("outcome = %v, want Dropped", outcome)
	}
	if got := getEntityState(t, factory, ctx, "fire-retry-1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (nothing may fire)", got)
	}
	if _, found := getTask(t, factory, ctx, id); !found {
		t.Error("expected the task to survive for the next scan")
	}
}

// TestFireScheduled_NeverFiresAManualTransition pins that the scheduler and
// the arm side agree on what is fireable. reconcileScheduledTasks arms only
// transitions that carry a Schedule and are neither manual nor disabled; the
// fire door must apply the same test, or a task that outlives the definition
// that armed it can drive a MANUAL transition — running its processors and
// moving the entity with nobody having asked.
//
// Reachable through the ordinary API: kind-b-wf arms AutoClose; a write
// re-binds the entity to kind-a-wf, where the same name is manual. Reconcile
// does not cancel the task, because it only cancels rows whose SourceState
// the entity has left — and here it has not moved.
func TestFireScheduled_NeverFiresAManualTransition(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-manual-guard", ModelVersion: "1.0"}

	wfA := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-a-wf", InitialState: "OPEN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States: map[string]spi.StateDefinition{
			// Same NAME, but manual — the scheduler must never fire it.
			"OPEN":     {Transitions: []spi.TransitionDefinition{{Name: "AutoClose", Next: "A_CLOSED", Manual: true}}},
			"A_CLOSED": {},
		},
	}
	wfB := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-b-wf", InitialState: "OPEN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "B_CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"B_CLOSED": {},
		},
	}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wfA, wfB})
	// Armed under kind-b-wf, but the payload now selects kind-a-wf.
	seedFireEntity(t, factory, ctx, "fire-manual-1", modelRef, "OPEN", "seed-tx-1", map[string]any{"kind": "a"})

	id := taskID(testTenant, "fire-manual-1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-manual-1",
		ModelName: modelRef.EntityName, Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Fatalf("outcome = %v, want Dropped", outcome)
	}
	if got := getEntityState(t, factory, ctx, "fire-manual-1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN — the scheduler fired a manual transition", got)
	}
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected the obsolete task to be deleted (self-heal)")
	}
}

// TestFireScheduled_ObsoleteTaskDiscardIsAudited pins that a scheduled
// transition vanishing because the entity re-bound to another definition is
// attributable. A scheduled transition is often a time-based control
// (auto-expire, escalate-if-not-approved); a client write that silently
// deletes one must leave a trace.
func TestFireScheduled_ObsoleteTaskDiscardIsAudited(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-obsolete-audit", ModelVersion: "1.0"}

	wfA := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-a-wf", InitialState: "OPEN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States:    map[string]spi.StateDefinition{"OPEN": {}},
	}
	wfB := spi.WorkflowDefinition{
		Version: "1.1", Name: "kind-b-wf", InitialState: "OPEN", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "B_CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"B_CLOSED": {},
		},
	}
	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wfA, wfB})
	seedFireEntity(t, factory, ctx, "fire-obsolete-1", modelRef, "OPEN", "seed-tx-1", map[string]any{"kind": "a"})

	id := taskID(testTenant, "fire-obsolete-1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-obsolete-1",
		ModelName: modelRef.EntityName, Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)

	if _, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant}); err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	if n := countAuditEvents(t, factory, ctx, "fire-obsolete-1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1 (the discarded timer must be attributable)", n)
	}
}

// TestFireScheduled_SilentGuardsRecordNoSelectionEvents pins the fire door's
// audit contract (docs/cloud-parity/scheduled-transitions.md §6): the guards
// that resolve a task silently — here, the entity having already left the
// task's source state — must stay silent. Resolving the workflow before
// those guards would stamp WORKFLOW_SKIP / WORKFLOW_FOUND onto every one of
// them.
//
// This guard already sat above resolution, so the test is a regression pin
// rather than evidence for the expiry reorder; TestFireScheduled_ExpiresEven-
// WhenWorkflowCannotBeResolved is what discriminates that.
func TestFireScheduled_SilentGuardsRecordNoSelectionEvents(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-silent-guard", ModelVersion: "1.0"}

	setupKindModel(t, factory, ctx, modelRef, []spi.WorkflowDefinition{
		{
			Version: "1.1", Name: "kind-a-wf", InitialState: "OPEN", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
			States: map[string]spi.StateDefinition{
				"OPEN": {Transitions: []spi.TransitionDefinition{
					{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
				}},
				"CLOSED": {},
			},
		},
	})
	// The entity has already moved on — the task's SourceState no longer matches.
	seedFireEntity(t, factory, ctx, "fire-silent-1", modelRef, "CLOSED", "seed-tx-1", map[string]any{"kind": "a"})

	id := taskID(testTenant, "fire-silent-1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-silent-1",
		ModelName: modelRef.EntityName, Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)

	if _, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant}); err != nil {
		t.Fatalf("FireScheduledTransition: %v", err)
	}
	for _, et := range []spi.StateMachineEventType{spi.SMEventWorkflowSkipped, spi.SMEventWorkflowFound} {
		if n := countAuditEvents(t, factory, ctx, "fire-silent-1", et); n != 0 {
			t.Errorf("%s events = %d, want 0 — a silent guard must not record selection", et, n)
		}
	}
}

// --- Infra-failure classification during selection ---

// errWorkflowStoreFactory makes WorkflowStore fail, modelling a genuine
// store outage during workflow resolution. Every other capability delegates
// so the rest of the engine still works.
type errWorkflowStoreFactory struct {
	spi.StoreFactory
	err error
}

func (f *errWorkflowStoreFactory) WorkflowStore(context.Context) (spi.WorkflowStore, error) {
	return nil, f.err
}

// TestResolveWorkflow_StoreOutageIsSanitizedInfraError asserts a workflow-store
// outage during selection is reported as a server-side condition, not as a
// client-attributable one. Callers classify a bare engine error as
// 400 WORKFLOW_FAILED with err.Error() in the response body, which would put
// raw store text (pgx messages, connection detail) in front of the caller —
// the output-sanitization rule in .claude/rules/security.md.
func TestResolveWorkflow_StoreOutageIsSanitizedInfraError(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := base.NewTransactionManager(uuids)
	storeErr := errors.New("pgx: connection refused to 10.0.0.7:5432")
	engine := NewEngine(&errWorkflowStoreFactory{StoreFactory: base, err: storeErr}, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "store-outage", ModelVersion: "1.0"}
	entity := makeEntity("store-outage-1", modelRef, map[string]any{"kind": "a"})
	entity.Meta.State = "VALIDATE"

	_, err := engine.ManualTransition(ctx, entity, "check")
	if err == nil {
		t.Fatal("expected the store outage to fail the transition")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected a classified *common.AppError so the caller cannot emit it as a 4xx detail; got %T: %v", err, err)
	}
	if appErr.Status < 500 {
		t.Errorf("status = %d, want 5xx (a store outage is not client-attributable)", appErr.Status)
	}
	if strings.Contains(appErr.Message, "connection refused") {
		t.Errorf("client-facing message leaks store detail: %q", appErr.Message)
	}
	if !errors.Is(err, storeErr) {
		t.Error("the underlying store error must stay in the chain for server-side logging")
	}
}

// TestResolveWorkflow_QueryDoorStoreOutageIsSanitized is the read-door
// counterpart: the transitions query must not answer, and must not hand the
// caller raw store text either.
func TestResolveWorkflow_QueryDoorStoreOutageIsSanitized(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := base.NewTransactionManager(uuids)
	storeErr := errors.New("pgx: connection refused to 10.0.0.7:5432")
	engine := NewEngine(&errWorkflowStoreFactory{StoreFactory: base, err: storeErr}, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "store-outage-query", ModelVersion: "1.0"}
	entity := makeEntity("store-outage-q1", modelRef, map[string]any{"kind": "a"})
	entity.Meta.State = "VALIDATE"

	names, err := engine.GetAvailableTransitionsForEntity(ctx, entity)
	if err == nil {
		t.Fatalf("GetAvailableTransitionsForEntity returned %v; a store outage must fail the read", names)
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status < 500 {
		t.Fatalf("expected a 5xx *common.AppError; got %T: %v", err, err)
	}
}

// TestEvaluateCriterion_TypingFailureIsMarkedInfra asserts the model-load
// failure a type-directed criterion needs is tagged as infrastructure, so
// callers map it to a sanitized 5xx rather than echoing store text in a
// 400 body. The causal chain must survive for server-side logging.
func TestEvaluateCriterion_TypingFailureIsMarkedInfra(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := base.NewTransactionManager(uuids)
	storeErr := errors.New("pgx: model store unreachable")
	engine := NewEngine(&errModelStoreFactory{StoreFactory: base, err: storeErr}, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "typing-infra", ModelVersion: "1.0"}
	entity := makeEntity("typing-infra-1", ref, map[string]any{"age": 30})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.age", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("expected the model-load failure to be surfaced")
	}
	if !errors.Is(err, ErrCriterionTypingInfra) {
		t.Errorf("error must wrap ErrCriterionTypingInfra so callers can sanitize it; got: %v", err)
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("error must keep the store cause in the chain for logging; got: %v", err)
	}
}

// --- Guard-path store failures must not be swallowed ---

// failingDeleteTaskStore delegates everything except Delete, which always
// fails. Models a store error on the self-heal paths that resolve a task by
// removing its row.
type failingDeleteTaskStore struct {
	spi.ScheduledTaskStore
	err error
}

func (s *failingDeleteTaskStore) Delete(context.Context, string) (bool, error) {
	return false, s.err
}

type failingDeleteTaskFactory struct {
	spi.StoreFactory
	err error
}

func (f *failingDeleteTaskFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	real, err := f.StoreFactory.ScheduledTaskStore(ctx)
	if err != nil {
		return nil, err
	}
	return &failingDeleteTaskStore{ScheduledTaskStore: real, err: f.err}, nil
}

// TestFireScheduled_EntityMovedOnDeleteFailureIsSurfaced asserts the
// entity-moved-on guard does not commit after a failed delete. Swallowing
// the error and committing anyway would report the task resolved while its
// row is still live, so the coordinator re-dispatches it on every scan
// forever.
func TestFireScheduled_EntityMovedOnDeleteFailureIsSurfaced(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := realFactory.NewTransactionManager(uuids)
	clock, advance := steppableClock(armMs)
	deleteErr := errors.New("scheduled task store unavailable")
	engine := NewEngine(&failingDeleteTaskFactory{StoreFactory: realFactory, err: deleteErr},
		uuids, txMgr, WithScheduledClock(clock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-del-fail", ModelVersion: "1.0"}
	saveWorkflow(t, realFactory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "wf", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"CLOSED": {},
		},
	}})
	// Entity has already left the task's SourceState — the guard fires.
	seedFireEntity(t, realFactory, ctx, "fire-del-1", modelRef, "CLOSED", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "fire-del-1", "OPEN", "AutoClose")
	armTask(t, realFactory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-del-1",
		ModelName: modelRef.EntityName, Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)

	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id, TenantID: testTenant})
	if err == nil {
		t.Fatal("expected the delete failure to be surfaced, not swallowed before a commit")
	}
	if !errors.Is(err, deleteErr) {
		t.Errorf("error must carry the store cause; got: %v", err)
	}
	if outcome != OutcomeDropped {
		t.Errorf("outcome = %v, want Dropped", outcome)
	}
}

// errEntityStoreFactory makes EntityStore.Get fail with a non-ErrNotFound
// error, modelling a store outage rather than a missing entity.
type errEntityStoreFactory struct {
	spi.StoreFactory
	err error
}

func (f *errEntityStoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	real, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &errEntityStore{EntityStore: real, err: f.err}, nil
}

type errEntityStore struct {
	spi.EntityStore
	err error
}

func (s *errEntityStore) Get(context.Context, string) (*spi.Entity, error) { return nil, s.err }

// TestGetAvailableTransitions_StoreOutageIsNot404 asserts an entity-store
// outage is not reported as "entity not found". Answering 404 for an entity
// that may well exist is a wrong-but-available result
// (.claude/rules/correctness-over-availability.md) and would send a caller
// down a create-it-again path against live data.
func TestGetAvailableTransitions_StoreOutageIsNot404(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := base.NewTransactionManager(uuids)
	storeErr := errors.New("pgx: connection refused")
	engine := NewEngine(&errEntityStoreFactory{StoreFactory: base, err: storeErr}, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "outage-404", ModelVersion: "1.0"}

	_, err := engine.GetAvailableTransitions(ctx, "some-entity-id", modelRef)
	if err == nil {
		t.Fatal("expected the store outage to fail the read")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected a classified *common.AppError; got %T: %v", err, err)
	}
	if appErr.Status == 404 {
		t.Errorf("status = 404 %q; a store outage is not a missing entity", appErr.Message)
	}
	if appErr.Status < 500 {
		t.Errorf("status = %d, want 5xx", appErr.Status)
	}
}
