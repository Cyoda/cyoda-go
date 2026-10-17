package workflow

import (
	"context"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// recordEngineEvents drives every engine path that records a state-machine
// event, on one memory store, with the set-ups of the event-data tests. It
// returns the store, a context of the test tenant and the ids of the entities
// the events belong to. Every entity is saved, so that the audit endpoint
// lists its events.
func recordEngineEvents(t *testing.T) (spi.StoreFactory, context.Context, []string) {
	t.Helper()
	ext := &scriptedExtProc{
		processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
			if proc.Name == "declines" {
				return nil, &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", MemberText: "card declined"}
			}
			return nil, nil
		},
		criterion: func(context.Context) (bool, string, error) { return false, "too small", nil },
	}
	env := newRunEnv(t, ext)
	ctx := env.ctx
	var ids []string
	save := func(e *spi.Entity) {
		t.Helper()
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}
		if _, err := es.Save(ctx, e); err != nil {
			t.Fatalf("save entity %s: %v", e.Meta.ID, err)
		}
		ids = append(ids, e.Meta.ID)
	}

	// Create, manual transition, abort, loopback: one model with a skipped
	// workflow and a selected one.
	ref := spi.ModelRef{EntityName: "event-conformance", ModelVersion: "1"}
	registerModelFields(t, ctx, env.factory, ref, map[string]schema.DataType{"kind": schema.String, "x": schema.Integer})
	saveWorkflow(t, env.factory, ctx, ref, []spi.WorkflowDefinition{
		{
			Version: "1.1", Name: "SkippedWF", InitialState: "NEW", Active: true,
			Criterion: simpleCriterion("$.kind", "EQUALS", "b"),
			States:    map[string]spi.StateDefinition{"NEW": {}},
		},
		{
			Version: "1.1", Name: "MainWF", InitialState: "NEW", Active: true,
			States: map[string]spi.StateDefinition{
				"NEW": {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "MID", Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "passes", ExecutionMode: ExecutionModeSync},
					{Type: ProcessorTypeExternalized, Name: "declines", ExecutionMode: ExecutionModeAsyncNewTx},
				}}}},
				"MID": {Transitions: []spi.TransitionDefinition{
					{Name: "check", Next: "DONE", Criterion: []byte(evFn("f", ""))},
					{Name: "later", Next: "DONE", Schedule: &spi.TransitionSchedule{DelayMs: 3_600_000}},
					{Name: "finish", Next: "DONE", Manual: true},
				}},
				"DONE": {},
			},
		},
	})
	main := makeEntity(uuid.NewString(), ref, map[string]any{"kind": "a", "x": 1})
	// START, WORKFLOW_SKIP, WORKFLOW_FOUND, TRANSITION_MAKE,
	// PAUSE_FOR_PROCESSING, STATE_PROCESS_RESULT (one failed, with the
	// member's text), TRANSITION_NOT_MATCH_CRITERION with functions,
	// SCHEDULED_TRANSITION_ARM, FINISH with rejected transitions.
	// An ASYNC_NEW_TX processor runs under a savepoint of the caller's
	// transaction.
	txID, txCtx, err := env.txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	main.Meta.TransactionID = txID
	if _, err := env.engine.Execute(txCtx, main, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := env.txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	stale := IfMatch{Expected: "tx-stale", Current: "tx-current"}
	// TRANSITION_ABORTED, with and without a transition.
	if _, err := env.engine.ManualTransitionWithIfMatch(ctx, main, "finish", stale); err == nil {
		t.Fatal("ManualTransitionWithIfMatch succeeded, want a conflict")
	}
	if _, err := env.engine.LoopbackWithIfMatch(ctx, main, stale); err == nil {
		t.Fatal("LoopbackWithIfMatch succeeded, want a conflict")
	}
	// The arm-time SCHEDULED_TRANSITION_CANCEL of the task armed in MID.
	if _, err := env.engine.ManualTransition(ctx, main, "finish"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	// TRANSITION_NOT_FOUND.
	if _, err := env.engine.ManualTransition(ctx, main, "missing"); err == nil {
		t.Fatal("ManualTransition of an undeclared transition succeeded")
	}
	save(main)

	// FORCE_SUCCESS: a loopback in a state the selected workflow lacks.
	orphan := makeEntity(uuid.NewString(), ref, map[string]any{"kind": "a", "x": 1})
	orphan.Meta.State = "ORPHAN"
	if _, err := env.engine.Loopback(ctx, orphan); err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	save(orphan)

	// CANCEL: the visit limit.
	loopRef := spi.ModelRef{EntityName: "event-conformance-loop", ModelVersion: "1"}
	saveWorkflow(t, env.factory, ctx, loopRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "LoopWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{Name: "again", Next: "A"}}},
		},
	}})
	loop := makeEntity(uuid.NewString(), loopRef, map[string]any{})
	if _, err := env.engine.Execute(ctx, loop, ""); err == nil {
		t.Fatal("Execute succeeded, want the visit limit")
	}
	save(loop)

	// The scheduled runs. Each task is claimed and run before the next one
	// is armed, so each claim takes exactly one task.
	run := func(task spi.ScheduledTask, want ScheduledOutcome) {
		t.Helper()
		claimed := env.claimOne(t, testOwner, false)
		if claimed.ID != task.ID {
			t.Fatalf("claimed task %s, want %s", claimed.ID, task.ID)
		}
		r, _ := env.run(claimed)
		if r.Outcome != want || r.Err != nil {
			t.Fatalf("run of %s = %+v, want %s", task.EntityID, r, want)
		}
		ids = append(ids, task.EntityID)
	}
	// SCHEDULED_TRANSITION_FIRE.
	fired := env.setup(t, uuid.NewString(), oneHopWF("CLOSED", nil, nil))
	run(fired, OutcomeFired)

	// The fire-time SCHEDULED_TRANSITION_EXPIRE.
	expiring := env.setup(t, uuid.NewString(), oneHopWF("CLOSED", nil, nil))
	timeout := int64(1_000)
	expiring.TimeoutMs = &timeout
	armTask(t, env.factory, ctx, expiring)
	env.advance(timeout + 1)
	run(expiring, OutcomeExpired)

	// The fire-time SCHEDULED_TRANSITION_CANCEL: the selected workflow no
	// longer declares the transition.
	orphaned := env.setup(t, uuid.NewString(), oneHopWF("CLOSED", nil, nil))
	saveWorkflow(t, env.factory, ctx, spi.ModelRef{EntityName: orphaned.ModelName, ModelVersion: "1.0"}, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "RunWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{"OPEN": {}, "CLOSED": {}},
	}})
	run(orphaned, OutcomeCancelled)

	// The fire-time SCHEDULED_TRANSITION_CANCEL: the entity carries no
	// committed transaction id.
	unstampedID := uuid.NewString()
	unstampedRef := spi.ModelRef{EntityName: "run-" + unstampedID, ModelVersion: "1.0"}
	saveWorkflow(t, env.factory, ctx, unstampedRef, []spi.WorkflowDefinition{oneHopWF("CLOSED", nil, nil)})
	seedFireEntity(t, env.factory, ctx, unstampedID, unstampedRef, "OPEN", "", map[string]any{})
	unstamped := armTask(t, env.factory, ctx, spi.ScheduledTask{
		ID: taskID(testTenant, unstampedID, "OPEN", "AutoClose"), TenantID: testTenant,
		Type: spi.ScheduledTaskFireTransition, ScheduledTime: env.nowMs(), EntityID: unstampedID,
		ModelName: unstampedRef.EntityName, ModelVersion: 1, Transition: "AutoClose", SourceState: "OPEN",
		ArmedAt: env.nowMs(),
	})
	run(unstamped, OutcomeCancelled)

	return env.factory, ctx, ids
}
