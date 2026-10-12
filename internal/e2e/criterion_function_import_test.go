package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Function conditions and their pre-checks at workflow import and export.

func cfWorkflow(name, criterion string) string {
	return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"` + name + `","initialState":"NONE","active":true,
	"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
	"CREATED":{"transitions":[{"name":"promote","next":"PROMOTED","manual":false,"criterion":` + criterion + `}]},
	"PROMOTED":{}}}]}`
}

func cfFn(name, preCheck string) string {
	if preCheck == "" {
		return `{"type":"function","function":{"name":"` + name + `"}}`
	}
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func cfGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

const (
	cfAmountGT100    = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	cfAmountGT10     = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10}`
	cfStateIsCreated = `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	cfTagsEqA        = `{"type":"array","jsonPath":"$.tags[*]","operatorType":"EQUALS","values":["a"]}`
)

// cfImportInto creates and locks a fresh model, then imports the workflow.
func cfImportInto(t *testing.T, model, wf string) (int, string) {
	t.Helper()
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	return importWorkflowE2E(t, model, 1, wf)
}

func TestCriterionFunctionImport_Accepted(t *testing.T) {
	rows := []struct{ name, criterion string }{
		{"pre-check simple", cfFn("f", cfAmountGT100)},
		{"pre-check lifecycle", cfFn("f", cfStateIsCreated)},
		{"pre-check array", cfFn("f", cfTagsEqA)},
		{"pre-check group", cfFn("f", cfGroup("AND", cfAmountGT100, cfStateIsCreated))},
		{"pre-check null", cfFn("f", "null")},
		{"function in AND", cfGroup("AND", cfAmountGT100, cfFn("f", ""))},
		{"function in OR", cfGroup("OR", cfFn("f", ""), cfAmountGT100)},
		{"function in NOT", cfGroup("NOT", cfFn("f", ""))},
		{"function nested", cfGroup("AND", cfGroup("OR", cfAmountGT100, cfGroup("NOT", cfFn("g", cfAmountGT100))))},
		{"same name twice", cfGroup("OR", cfFn("f", cfAmountGT100), cfFn("f", cfStateIsCreated))},
	}
	for i, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			model := fmt.Sprintf("e2e-cfi-acc-%d", i)
			status, body := cfImportInto(t, model, cfWorkflow("cfi-acc", tc.criterion))
			if status != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", status, body)
			}
		})
	}
}

func TestCriterionFunctionImport_Refused(t *testing.T) {
	entries := make([]string, 10000)
	for i := range entries {
		entries[i] = `"v"`
	}
	bigArray := `{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":[` + strings.Join(entries, ",") + `]}`
	deep := cfAmountGT100
	for i := 0; i < 50; i++ {
		deep = cfGroup("AND", deep)
	}
	rows := []struct{ name, criterion, want string }{
		{"function in pre-check", cfFn("f", cfFn("f", "")), "its pre-check holds a function condition at /function/criterion"},
		{"function deep in pre-check", cfGroup("AND", cfAmountGT100, cfFn("f", cfGroup("OR", cfAmountGT100, cfGroup("NOT", cfFn("f", ""))))),
			"at /conditions/1/function/criterion/conditions/1/conditions/0"},
		{"pre-check not a condition", cfFn("f", `{"type":"bogus"}`), "criterion is not a valid condition"},
		{"pre-check bad path", cfGroup("AND", cfFn("f", `{"type":"simple","jsonPath":"$.a[0]b","operatorType":"EQUALS","value":1}`)),
			"condition at /conditions/0/function/criterion"},
		{"pre-check bad operator", cfFn("f", `{"type":"simple","jsonPath":"$.a","operatorType":"NOPE","value":1}`),
			"condition at /function/criterion"},
		{"pre-check bad pattern", cfFn("f", `{"type":"simple","jsonPath":"$.a","operatorType":"MATCHES_PATTERN","value":")|("}`),
			"condition at /function/criterion"},
		{"local member bad operator beside a function", cfGroup("OR", cfFn("f", ""), `{"type":"simple","jsonPath":"$.a","operatorType":"NOPE","value":1}`),
			"condition at /conditions/1"},
		{"no function member", cfGroup("AND", cfAmountGT100, `{"type":"function"}`), "function condition at /conditions/1 has no `function` object"},
		{"null function member", cfGroup("AND", cfAmountGT100, `{"type":"function","function":null}`), "function condition at /conditions/1 has no `function` object"},
		{"no name", cfGroup("AND", `{"type":"function","function":{}}`), "function condition at /conditions/0 has no function name"},
		{"bad retryPolicy at depth", cfGroup("OR", cfAmountGT100, `{"type":"function","function":{"name":"g","config":{"retryPolicy":"SOMETIMES"}}}`),
			`criterion function "g" at /conditions/1: unknown retryPolicy "SOMETIMES"`},
		{"negative responseTimeoutMs at depth", cfGroup("OR", cfAmountGT100, `{"type":"function","function":{"name":"g","config":{"responseTimeoutMs":-1}}}`),
			`criterion function "g" at /conditions/1: responseTimeoutMs must not be negative (got -1)`},
		{"engine group with unknown operator", cfGroup("XOR", cfFn("f", "")), `unknown group operator "XOR"`},
		{"pre-check unknown lifecycle field", cfFn("f", `{"type":"lifecycle","field":"nope","operatorType":"EQUALS","value":"x"}`),
			`condition at /function/criterion: unknown meta filter field "nope"`},
		{"pre-check array clause without [*]", cfFn("f", `{"type":"array","jsonPath":"$.tags","operatorType":"EQUALS","values":["a"]}`),
			`condition at /function/criterion: invalid field path: jsonPath "$.tags" must end in a trailing array wildcard`},
		{"pre-check group with unknown operator", cfGroup("OR", cfAmountGT100, cfFn("f", cfGroup("XOR", cfAmountGT100))),
			`condition at /conditions/1/function/criterion: invalid condition: unknown group operator "XOR"`},
		{"pre-check NOT with two members", cfFn("f", cfGroup("NOT", cfAmountGT100, cfStateIsCreated)),
			"condition at /function/criterion: invalid condition: NOT requires exactly one condition, got 2"},
		{"config not an object at depth", cfGroup("OR", cfAmountGT100, `{"type":"function","function":{"name":"g","config":"x"}}`),
			"function condition at /conditions/1: failed to parse criterion function"},
		{"entry limit through a pre-check", cfFn("f", bigArray), "the limit is 10000"},
		{"depth limit in a pre-check", cfFn("f", deep), "maximum depth"},
	}
	for i, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			model := fmt.Sprintf("e2e-cfi-ref-%d", i)
			status, body := cfImportInto(t, model, cfWorkflow("cfi-ref", tc.criterion))
			if status != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %.500s", status, body)
			}
			assertErrorCode(t, body, "VALIDATION_FAILED")
			var pd struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal([]byte(body), &pd); err != nil {
				t.Fatalf("decode problem detail: %v\nbody: %.800s", err, body)
			}
			if !strings.Contains(pd.Detail, tc.want) {
				t.Errorf("detail does not contain %q:\n%.800s", tc.want, pd.Detail)
			}
		})
	}
}

func TestCriterionFunctionImport_ExportRoundTrip(t *testing.T) {
	const model = "e2e-cfi-export"
	criterion := cfGroup("AND", cfAmountGT10, cfGroup("NOT", cfFn("cf-exp-a", cfStateIsCreated)), cfFn("cf-exp-b", ""))
	status, body := cfImportInto(t, model, cfWorkflow("cfi-export", criterion))
	if status != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", status, body)
	}
	status, exp := exportWorkflowE2E(t, model, 1)
	if status != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %v", status, exp)
	}
	wfs, _ := exp["workflows"].([]any)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	wf, _ := wfs[0].(map[string]any)
	states, _ := wf["states"].(map[string]any)
	created, _ := states["CREATED"].(map[string]any)
	trs, _ := created["transitions"].([]any)
	if len(trs) != 1 {
		t.Fatalf("expected 1 transition in CREATED, got %d", len(trs))
	}
	tr, _ := trs[0].(map[string]any)
	var want any
	if err := json.Unmarshal([]byte(criterion), &want); err != nil {
		t.Fatal(err)
	}
	if got := tr["criterion"]; !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		t.Errorf("exported criterion differs\n got: %s\nwant: %s", gj, criterion)
	}
}
