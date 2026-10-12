package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// recordingCriteria answers each criterion function by name and records the
// JSON each callout was dispatched with.
type recordingCriteria struct {
	mu      sync.Mutex
	answers map[string]criteriaAnswer
	calls   []json.RawMessage
}

type criteriaAnswer struct {
	matches bool
	reason  string
	err     error
}

func (r *recordingCriteria) DispatchProcessor(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
	return nil, errors.New("no processor in these tests")
}

func (r *recordingCriteria) DispatchCriteria(_ context.Context, _ *spi.Entity, criterion json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	fn, _, err := contract.ParseCriterionFunction(criterion)
	if err != nil {
		return false, "", err
	}
	r.mu.Lock()
	r.calls = append(r.calls, append(json.RawMessage(nil), criterion...))
	r.mu.Unlock()
	a, ok := r.answers[fn.Name]
	if !ok {
		return false, "", fmt.Errorf("no answer scripted for %q", fn.Name)
	}
	return a.matches, a.reason, a.err
}

func (r *recordingCriteria) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, errors.New("no function in these tests")
}

func (r *recordingCriteria) calledNames(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		fn, _, err := contract.ParseCriterionFunction(c)
		if err != nil {
			t.Fatalf("recorded callout JSON does not parse: %v", err)
		}
		out = append(out, fn.Name)
	}
	return out
}

func evalEnv(t *testing.T, answers map[string]criteriaAnswer) (*Engine, *recordingCriteria, *spi.Entity, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	rec := &recordingCriteria{answers: answers}
	engine := NewEngine(factory, uuids, factory.NewTransactionManager(uuids), WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "fn-eval", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})
	entity := makeEntity("e1", ref, map[string]any{"amount": 50})
	entity.Meta.State = "S"
	return engine, rec, entity, ctx
}

const (
	evAmountGT100 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	evAmountGT10  = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10}`
)

func evFn(name, preCheck string) string {
	if preCheck == "" {
		return `{"type":"function","function":{"name":"` + name + `"}}`
	}
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func evGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

func TestEvaluateCriterion_FunctionConditions(t *testing.T) {
	yes, no := criteriaAnswer{matches: true}, criteriaAnswer{matches: false}
	cases := []struct {
		name      string
		criterion string
		answers   map[string]criteriaAnswer
		want      bool
		wantCalls []string
	}{
		{"pre-check false: no callout", evFn("a", evAmountGT100), map[string]criteriaAnswer{"a": yes}, false, nil},
		{"pre-check true: callout decides", evFn("a", evAmountGT10), map[string]criteriaAnswer{"a": no}, false, []string{"a"}},
		{"no pre-check: callout decides", evFn("a", ""), map[string]criteriaAnswer{"a": yes}, true, []string{"a"}},
		{"AND stops at the first false", evGroup("AND", evFn("a", ""), evFn("b", "")), map[string]criteriaAnswer{"a": no, "b": yes}, false, []string{"a"}},
		{"AND calls in order", evGroup("AND", evFn("a", ""), evFn("b", "")), map[string]criteriaAnswer{"a": yes, "b": no}, false, []string{"a", "b"}},
		{"AND local false first: no callout", evGroup("AND", evAmountGT100, evFn("a", "")), map[string]criteriaAnswer{"a": yes}, false, nil},
		{"OR stops at the first true", evGroup("OR", evFn("a", ""), evFn("b", "")), map[string]criteriaAnswer{"a": yes, "b": no}, true, []string{"a"}},
		{"OR local true first: no callout", evGroup("OR", evAmountGT10, evFn("a", "")), map[string]criteriaAnswer{"a": no}, true, nil},
		{"NOT inverts the answer", evGroup("NOT", evFn("a", "")), map[string]criteriaAnswer{"a": yes}, false, []string{"a"}},
		{"NOT of a skipped function is true", evGroup("NOT", evFn("a", evAmountGT100)), map[string]criteriaAnswer{"a": yes}, true, nil},
		{"nested", evGroup("OR", evGroup("AND", evFn("a", ""), evFn("b", "")), evGroup("NOT", evFn("c", ""))),
			map[string]criteriaAnswer{"a": yes, "b": no, "c": no}, true, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, rec, entity, ctx := evalEnv(t, tc.answers)
			out, err := engine.evaluateCriterion([]byte(tc.criterion), entity, &criterionContext{ctx: ctx, target: "TRANSITION"})
			if err != nil {
				t.Fatalf("evaluateCriterion: %v", err)
			}
			if out.matched != tc.want {
				t.Errorf("matched = %t, want %t", out.matched, tc.want)
			}
			if got := rec.calledNames(t); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("callouts = %v, want %v", got, tc.wantCalls)
			}
		})
	}
}

func TestEvaluateCriterion_DispatchesTheFunctionConditionsOwnJSON(t *testing.T) {
	fnB := evFn("b", evAmountGT10)
	engine, rec, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"b": {matches: true}})
	if _, err := engine.evaluateCriterion([]byte(evGroup("AND", evAmountGT10, fnB)), entity, &criterionContext{ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || string(rec.calls[0]) != fnB {
		t.Fatalf("dispatched %s, want the function condition's own JSON %s", rec.calls, fnB)
	}
}

func TestEvaluateCriterion_CompilePhaseFailsBeforeAnyCallout(t *testing.T) {
	undeclared := `{"type":"simple","jsonPath":"$.amonut","operatorType":"GREATER_THAN","value":1}`
	for name, c := range map[string]string{
		"undeclared path in a member the stop rule skips": evGroup("OR", evFn("a", ""), undeclared),
		"undeclared path in a pre-check":                  evGroup("AND", evFn("a", ""), evFn("b", undeclared)),
		"engine group with two NOT members":               evGroup("AND", evFn("a", ""), evGroup("NOT", evFn("b", ""), evAmountGT10)),
		"function without a name":                         evGroup("AND", evFn("a", ""), `{"type":"function","function":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			engine, rec, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"a": {matches: true}, "b": {matches: true}})
			if _, err := engine.evaluateCriterion([]byte(c), entity, &criterionContext{ctx: ctx}); err == nil {
				t.Fatal("want an error")
			}
			if n := len(rec.calledNames(t)); n != 0 {
				t.Errorf("%d callouts made before the fault was found; want 0", n)
			}
		})
	}
}

func TestEvaluateCriterion_CalloutFailureFailsTheEvaluation(t *testing.T) {
	boom := errors.New("member down")
	engine, rec, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"a": {matches: true}, "b": {err: boom}, "c": {matches: true}})
	_, err := engine.evaluateCriterion([]byte(evGroup("AND", evFn("a", ""), evFn("b", ""), evFn("c", ""))), entity, &criterionContext{ctx: ctx})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the callout's error", err)
	}
	if got := rec.calledNames(t); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("callouts = %v, want [a b]: nothing after the failure", got)
	}
}

func TestEvaluateCriterion_NoDispatcherFailsAnywhere(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "fn-nodisp", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})
	entity := makeEntity("e1", ref, map[string]any{"amount": 50})
	_, err := engine.evaluateCriterion([]byte(evGroup("OR", evAmountGT10, evFn("a", ""))), entity, &criterionContext{ctx: ctx})
	if err == nil || !strings.Contains(err.Error(), "no external processing service configured") {
		t.Fatalf("err = %v, want the no-dispatcher refusal even though the stop rule would skip the function", err)
	}
}

func TestEvaluateCriterion_ModelStoreFailureBeforeAnyCallout(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	rec := &recordingCriteria{answers: map[string]criteriaAnswer{"a": {matches: true}}}
	engine := NewEngine(&errModelStoreFactory{StoreFactory: base, err: errors.New("model store down")}, uuids,
		base.NewTransactionManager(uuids), WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	entity := makeEntity("e1", spi.ModelRef{EntityName: "fn-infra", ModelVersion: "1.0"}, map[string]any{"amount": 50})
	_, err := engine.evaluateCriterion([]byte(evGroup("AND", evFn("a", ""), evFn("b", evAmountGT10))), entity, &criterionContext{ctx: ctx})
	if !errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("err = %v, want ErrCriterionTypingInfra", err)
	}
	if n := len(rec.calledNames(t)); n != 0 {
		t.Errorf("%d callouts; want 0", n)
	}
}

func TestEvaluateCriterion_WholeFunctionWithoutPreCheckLoadsNoModel(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	rec := &recordingCriteria{answers: map[string]criteriaAnswer{"a": {matches: true}}}
	engine := NewEngine(&errModelStoreFactory{StoreFactory: base, err: errors.New("model store down")}, uuids,
		base.NewTransactionManager(uuids), WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	entity := makeEntity("e1", spi.ModelRef{EntityName: "fn-nomodel", ModelVersion: "1.0"}, map[string]any{"amount": 50})
	out, err := engine.evaluateCriterion([]byte(evFn("a", "")), entity, &criterionContext{ctx: ctx})
	if err != nil {
		t.Fatalf("evaluateCriterion: %v; a criterion without a local unit must not load the model", err)
	}
	if !out.matched {
		t.Error("matched = false, want the member's true")
	}
	if got := rec.calledNames(t); !slices.Equal(got, []string{"a"}) {
		t.Errorf("callouts = %v, want [a]", got)
	}
}

func TestEvaluateCriterion_ReasonOnlyForACalledWholeFunction(t *testing.T) {
	cases := []struct {
		name      string
		criterion string
		want      string
	}{
		{"whole function, called", evFn("a", ""), "too small"},
		{"function in a group", evGroup("AND", evFn("a", "")), ""},
		{"whole function, pre-check false", evFn("a", evAmountGT100), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, _, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"a": {matches: false, reason: "too small"}})
			out, err := engine.evaluateCriterion([]byte(tc.criterion), entity, &criterionContext{ctx: ctx})
			if err != nil {
				t.Fatalf("evaluateCriterion: %v", err)
			}
			if out.reason != tc.want {
				t.Errorf("reason = %q, want %q", out.reason, tc.want)
			}
		})
	}
}
