package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"unicode/utf8"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestNotMatchedReason(t *testing.T) {
	cases := []struct {
		name string
		out  criterionOutcome
		want string
	}{
		{"no function", criterionOutcome{}, "criterion did not match"},
		{"whole function, member reason", criterionOutcome{whole: true, reason: "too small",
			functions: []functionRecord{{name: "f", called: true, reason: "too small"}}}, "too small"},
		{"whole function, no reason", criterionOutcome{whole: true,
			functions: []functionRecord{{name: "f", called: true}}}, "criterion did not match"},
		{"whole function, pre-check false", criterionOutcome{whole: true,
			functions: []functionRecord{{name: "f", hasPreCheck: true}}}, "f: pre-check did not match, not called"},
		{"group, none reached", criterionOutcome{functions: []functionRecord{}}, "criterion did not match"},
		{"group", criterionOutcome{functions: []functionRecord{
			{path: "/conditions/0", name: "f", called: true, matches: true},
			{path: "/conditions/1", name: "f", hasPreCheck: true},
			{path: "/conditions/2", name: "g", called: true, reason: "no credit"},
		}}, "f at /conditions/0: answered true; f at /conditions/1: pre-check did not match, not called; g at /conditions/2: answered false: no credit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.out.notMatchedReason(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNotMatchedReason_IsCappedOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", maxCriterionReasonLen/2)
	out := criterionOutcome{functions: []functionRecord{
		{path: "/conditions/0", name: "a", called: true, reason: long},
		{path: "/conditions/1", name: "b", called: true, reason: long},
	}}
	got := out.notMatchedReason()
	if len(got) > maxCriterionReasonLen || !utf8.ValidString(got) {
		t.Errorf("len %d, valid UTF-8 %t; want ≤ %d and valid", len(got), utf8.ValidString(got), maxCriterionReasonLen)
	}
}

func TestFunctionRecordAuditValue(t *testing.T) {
	notCalled := functionRecord{path: "/conditions/1", name: "f", hasPreCheck: true, preCheck: map[string]any{"type": "simple"}}
	if got := notCalled.auditValue(); !maps.EqualFunc(got, map[string]any{
		"path": "/conditions/1", "name": "f", "called": false, "preCheckMatched": false, "preCheck": map[string]any{"type": "simple"},
	}, func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }) {
		t.Errorf("not called: %v", got)
	}
	called := functionRecord{name: "g", called: true, matches: true}
	got := called.auditValue()
	if _, ok := got["preCheck"]; ok {
		t.Error("preCheck present without a pre-check")
	}
	if _, ok := got["reason"]; ok {
		t.Error("reason present though the member gave none")
	}
	if got["matches"] != true || got["path"] != "" {
		t.Errorf("called: %v", got)
	}
}

func TestCriterionOutcomeFunctionsData(t *testing.T) {
	if d := (criterionOutcome{}).functionsData(); d != nil {
		t.Errorf("no function condition: got %v, want nil", d)
	}
	data := map[string]any{"reason": "r"}
	criterionOutcome{}.addFunctions(data)
	if _, ok := data["functions"]; ok {
		t.Error("addFunctions added functions for a criterion without a function condition")
	}
	out := criterionOutcome{functions: []functionRecord{{name: "f", called: true}}}
	if d := out.functionsData(); len(d) != 1 || len(d["functions"].([]any)) != 1 {
		t.Errorf("functionsData = %v, want only one functions entry", d)
	}
	out.addFunctions(data)
	if len(data["functions"].([]any)) != 1 || data["reason"] != "r" {
		t.Errorf("addFunctions: %v", data)
	}
}

func auditEnv(t *testing.T, answers map[string]criteriaAnswer, wfs ...spi.WorkflowDefinition) (*Engine, spi.StoreFactory, spi.TransactionManager, *recordingCriteria, spi.ModelRef) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	rec := &recordingCriteria{answers: answers}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	name := strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-")
	ref := spi.ModelRef{EntityName: "fn-audit-" + name, ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})
	saveWorkflow(t, factory, ctx, ref, wfs)
	return engine, factory, txMgr, rec, ref
}

func eventsOf(t *testing.T, factory spi.StoreFactory, txCtx context.Context, entityID, txID string, typ spi.StateMachineEventType) []spi.StateMachineEvent {
	t.Helper()
	store, err := factory.StateMachineAuditStore(txCtx)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.GetEventsByTransaction(txCtx, entityID, txID)
	if err != nil {
		t.Fatal(err)
	}
	var out []spi.StateMachineEvent
	for _, ev := range all {
		if ev.EventType == typ {
			out = append(out, ev)
		}
	}
	return out
}

// beginWith opens a transaction and an entity bound to it.
func beginWith(t *testing.T, txMgr spi.TransactionManager, ref spi.ModelRef, id string, amount int) (*spi.Entity, string, context.Context) {
	t.Helper()
	txID, txCtx, err := txMgr.Begin(ctxWithTenant(testTenant))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := makeEntity(id, ref, map[string]any{"amount": amount})
	entity.Meta.TransactionID = txID
	return entity, txID, txCtx
}

func functionsOf(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	list, ok := data["functions"].([]any)
	if !ok {
		t.Fatalf("data.functions is %T, want a list: %v", data["functions"], data)
	}
	out := make([]map[string]any, len(list))
	for i, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("functions[%d] is %T, want an object", i, v)
		}
		out[i] = m
	}
	return out
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func automatedWF(name string, criteria ...string) spi.WorkflowDefinition {
	states := map[string]spi.StateDefinition{}
	from := "S0"
	for i, c := range criteria {
		next := fmt.Sprintf("S%d", i+1)
		states[from] = spi.StateDefinition{Transitions: []spi.TransitionDefinition{
			{Name: fmt.Sprintf("t%d", i+1), Next: next, Criterion: json.RawMessage(c)},
		}}
		from = next
	}
	states[from] = spi.StateDefinition{}
	return spi.WorkflowDefinition{Version: "1.1", Name: name, InitialState: "S0", Active: true, States: states}
}

func manualWF(criterion string) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1.1", Name: "manual-wf", InitialState: "PENDING", Active: true,
		States: map[string]spi.StateDefinition{
			"PENDING": {Transitions: []spi.TransitionDefinition{
				{Name: "approve", Next: "APPROVED", Manual: true, Criterion: json.RawMessage(criterion)},
			}},
			"APPROVED": {},
		},
	}
}

func TestAudit_CascadeNoMatch_FunctionsAndReason(t *testing.T) {
	engine, factory, txMgr, _, ref := auditEnv(t, map[string]criteriaAnswer{"a": {matches: true}},
		automatedWF("wf", evGroup("AND", evFn("a", ""), evFn("a", evAmountGT100))))
	entity, txID, txCtx := beginWith(t, txMgr, ref, "e1", 50)
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if entity.Meta.State != "S0" {
		t.Errorf("state = %q, want S0", entity.Meta.State)
	}
	evs := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventTransitionCriterionNoMatch)
	if len(evs) != 1 {
		t.Fatalf("%d TRANSITION_NOT_MATCH_CRITERION events, want 1", len(evs))
	}
	d := evs[0].Data
	if want := "a at /conditions/0: answered true; a at /conditions/1: pre-check did not match, not called"; d["reason"] != want {
		t.Errorf("reason = %q, want %q", d["reason"], want)
	}
	if d["criterion"] != "group" {
		t.Errorf("criterion = %v, want group", d["criterion"])
	}
	fns := functionsOf(t, d)
	if len(fns) != 2 || fns[0]["path"] != "/conditions/0" || fns[1]["path"] != "/conditions/1" {
		t.Errorf("functions = %v, want entries at /conditions/0 and /conditions/1", fns)
	}
}

func TestAudit_CascadeMatch_TransitionMakeCarriesFunctions(t *testing.T) {
	engine, factory, txMgr, _, ref := auditEnv(t, map[string]criteriaAnswer{"a": {matches: true}},
		automatedWF("wf", evFn("a", evAmountGT10), evAmountGT10))
	entity, txID, txCtx := beginWith(t, txMgr, ref, "e1", 50)
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if entity.Meta.State != "S2" {
		t.Fatalf("state = %q, want S2", entity.Meta.State)
	}
	evs := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventTransitionMade)
	if len(evs) != 2 {
		t.Fatalf("%d TRANSITION_MAKE events, want 2", len(evs))
	}
	var withFn, local *spi.StateMachineEvent
	for i := range evs {
		switch {
		case strings.HasPrefix(evs[i].Details, `Transition "t1"`):
			withFn = &evs[i]
		case strings.HasPrefix(evs[i].Details, `Transition "t2"`):
			local = &evs[i]
		}
	}
	if withFn == nil || local == nil {
		t.Fatalf("TRANSITION_MAKE events for t1 and t2 not found: %v", evs)
	}
	want := `{"functions":[{"called":true,"matches":true,"name":"a","path":"","preCheck":` +
		`{"jsonPath":"$.amount","operatorType":"GREATER_THAN","type":"simple","value":10},"preCheckMatched":true}]}`
	if got := jsonOf(t, withFn.Data); got != want {
		t.Errorf("t1 data = %s, want %s", got, want)
	}
	if local.Data != nil {
		t.Errorf("t2 (local criterion) data = %v, want nil", local.Data)
	}
}

func TestAudit_Selection_FoundAndSkipped(t *testing.T) {
	wfA := automatedWF("wf-a")
	wfA.Criterion = json.RawMessage(evFn("sel", evAmountGT100))
	wfB := automatedWF("wf-b")

	t.Run("amount 50", func(t *testing.T) {
		engine, factory, txMgr, _, ref := auditEnv(t, map[string]criteriaAnswer{"sel": {matches: true}}, wfA, wfB)
		entity, txID, txCtx := beginWith(t, txMgr, ref, "e1", 50)
		if _, err := engine.Execute(txCtx, entity, ""); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		skips := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventWorkflowSkipped)
		if len(skips) != 1 {
			t.Fatalf("%d WORKFLOW_SKIP events, want 1", len(skips))
		}
		d := skips[0].Data
		if d["workflowName"] != "wf-a" || d["reason"] != "sel: pre-check did not match, not called" {
			t.Errorf("WORKFLOW_SKIP data = %v", d)
		}
		if fns := functionsOf(t, d); len(fns) != 1 || fns[0]["called"] != false {
			t.Errorf("functions = %v, want one entry not called", fns)
		}
		found := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventWorkflowFound)
		if len(found) != 1 || found[0].Data != nil {
			t.Errorf("WORKFLOW_FOUND = %v, want one event with nil data", found)
		}
	})
	t.Run("amount 150", func(t *testing.T) {
		engine, factory, txMgr, _, ref := auditEnv(t, map[string]criteriaAnswer{"sel": {matches: true}}, wfA, wfB)
		entity, txID, txCtx := beginWith(t, txMgr, ref, "e1", 150)
		if _, err := engine.Execute(txCtx, entity, ""); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		found := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventWorkflowFound)
		if len(found) != 1 {
			t.Fatalf("%d WORKFLOW_FOUND events, want 1", len(found))
		}
		if fns := functionsOf(t, found[0].Data); len(fns) != 1 || fns[0]["called"] != true {
			t.Errorf("functions = %v, want one entry called", fns)
		}
	})
}

// manualFire saves a workflow whose manual transition approve carries the
// criterion, and fires it.
func manualFire(t *testing.T, criterion string, answers map[string]criteriaAnswer, amount int) (spi.StoreFactory, *recordingCriteria, string, context.Context, error) {
	t.Helper()
	engine, factory, txMgr, rec, ref := auditEnv(t, answers, manualWF(criterion))
	entity, txID, txCtx := beginWith(t, txMgr, ref, "e1", amount)
	entity.Meta.State = "PENDING"
	_, err := engine.ManualTransition(txCtx, entity, "approve")
	return factory, rec, txID, txCtx, err
}

func TestAudit_ManualRefused_Message(t *testing.T) {
	_, _, _, _, err := manualFire(t, evGroup("AND", evFn("a", ""), evFn("b", "")),
		map[string]criteriaAnswer{"a": {matches: true}, "b": {matches: false, reason: "too small"}}, 50)
	if !errors.Is(err, ErrCriterionNotMatched) {
		t.Fatalf("err = %v, want ErrCriterionNotMatched", err)
	}
	if want := `transition "approve" criterion not matched: a at /conditions/0: answered true; b at /conditions/1: answered false: too small`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestAudit_ManualRefused_WholeFunctionNoReasonStaysBare(t *testing.T) {
	_, _, _, _, err := manualFire(t, evFn("a", ""), map[string]criteriaAnswer{"a": {matches: false}}, 50)
	if !errors.Is(err, ErrCriterionNotMatched) {
		t.Fatalf("err = %v, want ErrCriterionNotMatched", err)
	}
	if want := `transition "approve" criterion not matched`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestAudit_ManualAllowed_TransitionMakeCarriesFunctions(t *testing.T) {
	factory, _, txID, txCtx, err := manualFire(t, evGroup("OR", evFn("a", "")), map[string]criteriaAnswer{"a": {matches: true}}, 50)
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	evs := eventsOf(t, factory, txCtx, "e1", txID, spi.SMEventTransitionMade)
	if len(evs) != 1 {
		t.Fatalf("%d TRANSITION_MAKE events, want 1", len(evs))
	}
	if fns := functionsOf(t, evs[0].Data); len(fns) != 1 || fns[0]["path"] != "/conditions/0" {
		t.Errorf("functions = %v, want one entry at /conditions/0", fns)
	}
}

// FireScheduledTransition reads ErrCriterionNotMatched as Declined and any
// other error as a failure to retry. Manual and scheduled firing share
// fireTransition, so the manual door shows the classification.
func TestFireTransition_ClassifiesForTheScheduler(t *testing.T) {
	t.Run("pre-check false is not matched, without a callout", func(t *testing.T) {
		_, rec, _, _, err := manualFire(t, evFn("a", evAmountGT100), map[string]criteriaAnswer{"a": {matches: true}}, 50)
		if !errors.Is(err, ErrCriterionNotMatched) {
			t.Fatalf("err = %v, want ErrCriterionNotMatched", err)
		}
		if n := len(rec.calledNames(t)); n != 0 {
			t.Errorf("%d callouts, want 0", n)
		}
	})
	t.Run("a failed callout is a failure, not a decline", func(t *testing.T) {
		_, _, _, _, err := manualFire(t, evGroup("AND", evAmountGT10, evFn("b", "")),
			map[string]criteriaAnswer{"b": {err: errors.New("member down")}}, 50)
		if err == nil || errors.Is(err, ErrCriterionNotMatched) {
			t.Fatalf("err = %v, want a failure that is not ErrCriterionNotMatched", err)
		}
	})
}
