package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
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
