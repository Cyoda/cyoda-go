package workflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

func mustParse(t *testing.T, s string) predicate.Condition {
	t.Helper()
	c, err := predicate.ParseCondition([]byte(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return c
}

const (
	treeLocal = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	treeFnA   = `{"type":"function","function":{"name":"a"}}`
	treeFnB   = `{"type":"function","function":{"name":"b","criterion":{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"S"}}}`
)

func TestBuildCriterionTree_FunctionFreeIsOneLocalUnit(t *testing.T) {
	root, err := buildCriterionTree(mustParse(t, `{"type":"group","operator":"AND","conditions":[`+treeLocal+`]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	units, fns := collectCriterion(root)
	if l, ok := root.(*critLocal); !ok || l.ptr != "" || len(units) != 1 || len(fns) != 0 {
		t.Fatalf("root = %#v units=%d fns=%d, want one local unit at \"\"", root, len(units), len(fns))
	}
}

func TestBuildCriterionTree_SplitsAtFunctions(t *testing.T) {
	c := `{"type":"group","operator":"OR","conditions":[` + treeLocal + `,{"type":"group","operator":"NOT","conditions":[` + treeFnA + `]},` + treeFnB + `]}`
	root, err := buildCriterionTree(mustParse(t, c), "")
	if err != nil {
		t.Fatal(err)
	}
	g, ok := root.(*critGroup)
	if !ok || g.op != "OR" || len(g.members) != 3 {
		t.Fatalf("root = %#v, want an OR engine group of 3", root)
	}
	units, fns := collectCriterion(root)
	var gotUnits, gotFns []string
	for _, u := range units {
		gotUnits = append(gotUnits, u.ptr)
	}
	for _, f := range fns {
		gotFns = append(gotFns, f.fn.Name+"@"+f.ptr)
	}
	if want := []string{"/conditions/0", "/conditions/2/function/criterion"}; !slices.Equal(gotUnits, want) {
		t.Errorf("units = %v, want %v", gotUnits, want)
	}
	if want := []string{"a@/conditions/1/conditions/0", "b@/conditions/2"}; !slices.Equal(gotFns, want) {
		t.Errorf("functions = %v, want %v", gotFns, want)
	}
	if fns[1].preCheckValue == nil || fns[0].preCheck != nil {
		t.Errorf("pre-check: a=%v b=%v", fns[0].preCheck, fns[1].preCheckValue)
	}
	if string(fns[0].raw) != treeFnA {
		t.Errorf("raw = %s, want the function condition's own JSON %s", fns[0].raw, treeFnA)
	}
}

func TestBuildCriterionTree_Refusals(t *testing.T) {
	cases := []struct{ name, criterion, want string }{
		{"no function member", `{"type":"function"}`, "has no `function` object"},
		{"function member null", `{"type":"function","function":null}`, "has no `function` object"},
		{"no name", `{"type":"group","operator":"AND","conditions":[{"type":"function","function":{"config":{}}}]}`, "function condition at /conditions/0 has no function name"},
		{"bad config", `{"type":"function","function":{"name":"f","config":{"responseTimeoutMs":"5s"}}}`, "failed to parse criterion function"},
		{"function in pre-check", `{"type":"function","function":{"name":"f","criterion":` + treeFnA + `}}`, `function condition "f": its pre-check holds a function condition at /function/criterion`},
		{"function nested in pre-check", `{"type":"group","operator":"AND","conditions":[` + treeLocal + `,{"type":"function","function":{"name":"f","criterion":{"type":"group","operator":"OR","conditions":[` + treeLocal + `,` + treeFnA + `]}}}]}`, "at /conditions/1/function/criterion/conditions/1"},
		{"unknown operator on an engine group", `{"type":"group","operator":"XOR","conditions":[` + treeFnA + `]}`, `unknown group operator "XOR"`},
		{"NOT of two around a function", `{"type":"group","operator":"NOT","conditions":[` + treeFnA + `,` + treeLocal + `]}`, "NOT requires exactly one condition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildCriterionTree(mustParse(t, tc.criterion), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBuildCriterionTree_PreCheckKeepsBigNumbers(t *testing.T) {
	root, err := buildCriterionTree(mustParse(t, `{"type":"function","function":{"name":"f","criterion":{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":12345678901234567890}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(root.(*critFunction).preCheckValue)
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Errorf("preCheckValue = %s, want every digit kept", out)
	}
}
