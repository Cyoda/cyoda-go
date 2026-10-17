package workflow

import (
	"encoding/json"
	"reflect"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// finishOf returns the single FINISH event and checks its state and success.
func finishOf(t *testing.T, events []spi.StateMachineEvent, wantState string) map[string]any {
	t.Helper()
	ev := onlyEvent(t, events, spi.SMEventFinished)
	if ev.Data["state"] != wantState || ev.Data["success"] != true {
		t.Errorf("FINISH state = %v, success = %v; want %q and true", ev.Data["state"], ev.Data["success"], wantState)
	}
	stop, ok := ev.Data["stopReason"].(map[string]any)
	if !ok {
		t.Fatalf("FINISH stopReason = %T (%v), want an object", ev.Data["stopReason"], ev.Data["stopReason"])
	}
	return stop
}

func TestStop_NoAutomatedTransitions(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "stop-no-auto", ModelVersion: "1"}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "NoAutoWF", InitialState: "NEW", Active: true,
		States: map[string]spi.StateDefinition{
			"NEW": {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "DONE", Manual: true}}},
			// None of these is automated: manual, disabled, scheduled.
			"DONE": {Transitions: []spi.TransitionDefinition{
				{Name: "back", Next: "NEW", Manual: true},
				{Name: "off", Next: "NEW", Disabled: true},
				{Name: "later", Next: "NEW", Schedule: &spi.TransitionSchedule{DelayMs: 60_000}},
			}},
		},
	}})

	cases := []struct {
		door  string
		state string // the starting state; "" on create
		run   func(*spi.Entity) error
		want  string
	}{
		{"execute", "", func(e *spi.Entity) error { _, err := engine.Execute(ctx, e, ""); return err }, "NEW"},
		{"manual", "NEW", func(e *spi.Entity) error { _, err := engine.ManualTransition(ctx, e, "go"); return err }, "DONE"},
		{"loopback", "DONE", func(e *spi.Entity) error { _, err := engine.Loopback(ctx, e); return err }, "DONE"},
	}
	for _, tc := range cases {
		t.Run(tc.door, func(t *testing.T) {
			id := "e-" + tc.door
			entity := makeEntity(id, ref, map[string]any{"x": 1})
			entity.Meta.State = tc.state
			if err := tc.run(entity); err != nil {
				t.Fatalf("%s: %v", tc.door, err)
			}
			stop := finishOf(t, allEvents(t, factory, ctx, id), tc.want)
			want := map[string]any{
				"type":        "COMPLETED_NO_AUTO_TRANSITIONS",
				"description": `No automated transition from state "` + tc.want + `"`,
			}
			if !reflect.DeepEqual(stop, want) {
				t.Errorf("stopReason = %v, want %v", stop, want)
			}
		})
	}
}

func TestStop_AllCriteriaRejected_LastPassOnly(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "RejectWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "a-refused", Next: "C", Criterion: json.RawMessage(evAmountGT100)},
				{Name: "a-go", Next: "B"},
			}},
			"B": {Transitions: []spi.TransitionDefinition{
				{Name: "b-fn", Next: "C", Criterion: json.RawMessage(evFn("chk", ""))},
				{Name: "b-manual", Next: "C", Manual: true},
				{Name: "b-simple", Next: "C", Criterion: json.RawMessage(evAmountGT100)},
			}},
			"C": {},
		},
	}
	engine, factory, txMgr, _, ref := auditEnv(t, map[string]criteriaAnswer{"chk": {matches: false, reason: "too small"}}, wf)
	entity, txID, txCtx := beginWith(t, txMgr, ref, "e1", 50)
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if entity.Meta.State != "B" {
		t.Fatalf("state = %q, want B", entity.Meta.State)
	}

	refusals := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventTransitionCriterionNoMatch)
	if len(refusals) != 3 {
		t.Fatalf("%d TRANSITION_NOT_MATCH_CRITERION events, want 3 (A once, B twice)", len(refusals))
	}
	var wantRejected []any
	for _, ev := range refusals[1:] {
		wantRejected = append(wantRejected, map[string]any{
			"transitionName": ev.Data["transition"],
			"criterion":      ev.Data["criterion"],
			"reason":         ev.Data["reason"],
		})
	}
	if names := []any{refusals[1].Data["transition"], refusals[2].Data["transition"]}; !reflect.DeepEqual(names, []any{"b-fn", "b-simple"}) {
		t.Fatalf("B's refusals = %v, want b-fn then b-simple", names)
	}

	store, err := factory.StateMachineAuditStore(txCtx)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.GetEventsByTransaction(txCtx, "e1", txID)
	if err != nil {
		t.Fatal(err)
	}
	stop := finishOf(t, all, "B")
	want := map[string]any{
		"type":                "COMPLETED_ALL_CRITERIA_REJECTED",
		"description":         `Every automated transition from state "B" was refused by its criterion`,
		"rejectedTransitions": wantRejected,
	}
	if !reflect.DeepEqual(stop, want) {
		t.Errorf("stopReason = %v, want %v", stop, want)
	}
}

func TestStop_ForceSuccess(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "stop-force-success", ModelVersion: "1"}
	setupKindModel(t, factory, ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "kind-a-wf", InitialState: "START", Active: true,
		Criterion: simpleCriterion("$.kind", "EQUALS", "a"),
		States:    map[string]spi.StateDefinition{"START": {}},
	}})

	entity := makeEntity("e1", ref, map[string]any{"kind": "a"})
	entity.Meta.State = "ORPHAN"
	if _, err := engine.Loopback(ctx, entity); err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	events := allEvents(t, factory, ctx, "e1")
	if d := onlyEvent(t, events, spi.SMEventForcedSuccess).Data; !reflect.DeepEqual(d, map[string]any{}) {
		t.Errorf("FORCE_SUCCESS data = %v, want an empty object", d)
	}
	stop := finishOf(t, events, "ORPHAN")
	want := map[string]any{
		"type":        "FORCE_SUCCESS",
		"description": `State "ORPHAN" is not declared in workflow "kind-a-wf"`,
	}
	if !reflect.DeepEqual(stop, want) {
		t.Errorf("stopReason = %v, want %v", stop, want)
	}
}
