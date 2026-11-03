package workflow

import (
	"context"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"reflect"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// allEvents returns entityID's audit events in recorded order.
func allEvents(t *testing.T, factory spi.StoreFactory, ctx context.Context, entityID string) []spi.StateMachineEvent {
	t.Helper()
	store, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := store.GetEvents(ctx, entityID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	return events
}

// onlyEvent returns the single event of type typ, failing unless there is
// exactly one.
func onlyEvent(t *testing.T, events []spi.StateMachineEvent, typ spi.StateMachineEventType) spi.StateMachineEvent {
	t.Helper()
	var found []spi.StateMachineEvent
	for _, ev := range events {
		if ev.EventType == typ {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s events, want 1: %v", len(found), typ, events)
	}
	return found[0]
}

func TestEngineEventData_Create(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "event-data-create", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "CreateWF", InitialState: "NEW", Active: true,
		States: map[string]spi.StateDefinition{
			"NEW":  {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "DONE"}}},
			"DONE": {},
		},
	}})

	entity := makeEntity("e1", ref, map[string]any{"x": 1})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	events := allEvents(t, factory, ctx, "e1")

	if d := onlyEvent(t, events, spi.SMEventStarted).Data; d == nil || d["state"] != nil {
		t.Errorf("START data = %v, want an object without state", d)
	}
	if d := onlyEvent(t, events, spi.SMEventWorkflowFound).Data; !reflect.DeepEqual(d, map[string]any{"workflowName": "CreateWF"}) {
		t.Errorf("WORKFLOW_FOUND data = %v", d)
	}
	made := onlyEvent(t, events, spi.SMEventTransitionMade)
	if want := map[string]any{"workflowName": "CreateWF", "transition": "go", "newState": "DONE"}; !reflect.DeepEqual(made.Data, want) {
		t.Errorf("TRANSITION_MAKE data = %v, want %v", made.Data, want)
	}
	if made.State != "NEW" {
		t.Errorf("TRANSITION_MAKE state = %q, want NEW (the state left)", made.State)
	}
}

func TestEngineEventData_ManualStartHasState(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "event-data-manual", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "ManualWF", InitialState: "NEW", Active: true,
		States: map[string]spi.StateDefinition{
			"NEW":  {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "DONE", Manual: true}}},
			"DONE": {},
		},
	}})
	entity := makeEntity("e1", ref, map[string]any{"x": 1})
	entity.Meta.State = "NEW"
	if _, err := engine.ManualTransition(ctx, entity, "go"); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if d := onlyEvent(t, allEvents(t, factory, ctx, "e1"), spi.SMEventStarted).Data; !reflect.DeepEqual(d, map[string]any{"state": "NEW"}) {
		t.Errorf("START data = %v, want state NEW", d)
	}
}

func TestEngineEventData_TransitionNotFound(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "event-data-not-found", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "NotFoundWF", InitialState: "NEW", Active: true,
		States: map[string]spi.StateDefinition{
			"NEW": {Transitions: []spi.TransitionDefinition{
				{Name: "off", Next: "DONE", Manual: true, Disabled: true},
				{Name: "later", Next: "DONE", Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"DONE": {},
		},
	}})

	for _, name := range []string{"missing", "off", "later"} {
		t.Run(name, func(t *testing.T) {
			id := "e-" + name
			entity := makeEntity(id, ref, map[string]any{"x": 1})
			entity.Meta.State = "NEW"
			if _, err := engine.ManualTransition(ctx, entity, name); err == nil {
				t.Fatal("ManualTransition succeeded, want an error")
			}
			ev := onlyEvent(t, allEvents(t, factory, ctx, id), spi.SMEventTransitionNotFound)
			if want := map[string]any{"workflowName": "NotFoundWF", "transition": name}; !reflect.DeepEqual(ev.Data, want) {
				t.Errorf("TRANSITION_NOT_FOUND data = %v, want %v", ev.Data, want)
			}
			if ev.Details == "" {
				t.Error("TRANSITION_NOT_FOUND details is empty, want the cause")
			}
		})
	}
}

func TestEngineEventData_CancelVisitLimit(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	engine := NewEngine(factory, uuids, factory.NewTransactionManager(uuids), platformclock.Real(), WithMaxStateVisits(2))
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "event-data-cancel", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "LoopWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{Name: "again", Next: "A"}}},
		},
	}})
	entity := makeEntity("e1", ref, map[string]any{"x": 1})
	if _, err := engine.Execute(ctx, entity, ""); err == nil {
		t.Fatal("Execute succeeded, want the visit limit")
	}
	ev := onlyEvent(t, allEvents(t, factory, ctx, "e1"), spi.SMEventCancelled)
	if want := map[string]any{"reason": `state "A" visited 3 times (limit: 2)`}; !reflect.DeepEqual(ev.Data, want) {
		t.Errorf("CANCEL data = %v, want %v", ev.Data, want)
	}
	if ev.Details != "State machine aborted" {
		t.Errorf("CANCEL details = %q, want %q", ev.Details, "State machine aborted")
	}
}

func TestEngineEventData_TransitionAborted(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "event-data-aborted", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "AbortWF", InitialState: "NEW", Active: true,
		States: map[string]spi.StateDefinition{
			"NEW":  {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "DONE", Manual: true}}},
			"DONE": {},
		},
	}})
	stale := IfMatch{Expected: "tx-stale", Current: "tx-current"}

	t.Run("manual", func(t *testing.T) {
		entity := makeEntity("e-manual", ref, map[string]any{"x": 1})
		entity.Meta.State = "NEW"
		if _, err := engine.ManualTransitionWithIfMatch(ctx, entity, "go", stale); err == nil {
			t.Fatal("want a conflict")
		}
		ev := onlyEvent(t, allEvents(t, factory, ctx, "e-manual"), SMEventTransitionAborted)
		want := map[string]any{"reason": "ENTITY_MODIFIED", "transition": "go", "expectedTxId": "tx-stale", "actualTxId": "tx-current"}
		if !reflect.DeepEqual(ev.Data, want) {
			t.Errorf("TRANSITION_ABORTED data = %v, want %v", ev.Data, want)
		}
	})
	t.Run("loopback", func(t *testing.T) {
		entity := makeEntity("e-loopback", ref, map[string]any{"x": 1})
		entity.Meta.State = "NEW"
		if _, err := engine.LoopbackWithIfMatch(ctx, entity, stale); err == nil {
			t.Fatal("want a conflict")
		}
		ev := onlyEvent(t, allEvents(t, factory, ctx, "e-loopback"), SMEventTransitionAborted)
		want := map[string]any{"reason": "ENTITY_MODIFIED", "expectedTxId": "tx-stale", "actualTxId": "tx-current"}
		if !reflect.DeepEqual(ev.Data, want) {
			t.Errorf("TRANSITION_ABORTED data = %v, want %v", ev.Data, want)
		}
	})
}

func TestEngineEventData_PauseProcessNamesInPipelineOrder(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	engine := NewEngine(factory, uuids, factory.NewTransactionManager(uuids), platformclock.Real(), WithExternalProcessing(&mockExternalProcessing{
		dispatchFunc: func(_ context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			return entity, nil
		},
	}))
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "event-data-pause", ModelVersion: "1"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"x": schema.Integer})
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PauseWF", InitialState: "NEW", Active: true,
		States: map[string]spi.StateDefinition{
			"NEW": {Transitions: []spi.TransitionDefinition{{Name: "run", Next: "DONE", Processors: []spi.ProcessorDefinition{
				{Type: ProcessorTypeExternalized, Name: "zeta", ExecutionMode: "SYNC"},
				{Type: ProcessorTypeExternalized, Name: "alpha", ExecutionMode: "SYNC"},
			}}}},
			"DONE": {},
		},
	}})
	entity := makeEntity("e1", ref, map[string]any{"x": 1})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ev := onlyEvent(t, allEvents(t, factory, ctx, "e1"), spi.SMEventProcessingPaused)
	want := map[string]any{"workflowName": "PauseWF", "transition": "run", "processNames": []any{"zeta", "alpha"}}
	if !reflect.DeepEqual(ev.Data, want) {
		t.Errorf("PAUSE_FOR_PROCESSING data = %v, want %v", ev.Data, want)
	}
}
