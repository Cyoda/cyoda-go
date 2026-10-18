package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// Import accepts function conditions at any depth of a criterion and checks
// each one, and each pre-check, naming the position in the criterion.

// fnImportWF wraps criterion as the transition criterion of a minimal workflow.
func fnImportWF(criterion string) []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(criterion))}
}

const (
	impLocal    = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	impFn       = `{"type":"function","function":{"name":"f"}}`
	impLifePre  = `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"S1"}`
	impArrayPre = `{"type":"array","jsonPath":"$.tags[*]","operatorType":"EQUALS","values":["a"]}`
)

func impFnWith(name, preCheck string) string {
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func impGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

func TestImport_FunctionConditionsAccepted(t *testing.T) {
	for name, c := range map[string]string{
		"pre-check simple":    impFnWith("f", impLocal),
		"pre-check lifecycle": impFnWith("f", impLifePre),
		"pre-check array":     impFnWith("f", impArrayPre),
		"pre-check group":     impFnWith("f", impGroup("AND", impLocal, impLifePre)),
		"pre-check null":      impFnWith("f", "null"),
		"function in AND":     impGroup("AND", impLocal, impFn),
		"function in OR":      impGroup("OR", impFn, impLocal),
		"function in NOT":     impGroup("NOT", impFn),
		"function nested":     impGroup("AND", impGroup("OR", impLocal, impGroup("NOT", impFnWith("g", impLocal)))),
		"same name twice":     impGroup("OR", impFnWith("f", impLocal), impFnWith("f", impLifePre)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateImportRequest(fnImportWF(c)); err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
	}
}

func TestImport_FunctionConditionsRefused(t *testing.T) {
	cases := []struct{ name, criterion, want string }{
		{"function in pre-check", impFnWith("f", impFn), "its pre-check holds a function condition at /function/criterion"},
		{"function deep in pre-check", impGroup("AND", impLocal, impFnWith("f", impGroup("OR", impLocal, impGroup("NOT", impFn)))),
			"at /conditions/1/function/criterion/conditions/1/conditions/0"},
		{"pre-check not a condition", impFnWith("f", `{"type":"bogus"}`), "criterion is not a valid condition"},
		{"pre-check bad path", impGroup("AND", impFnWith("f", `{"type":"simple","jsonPath":"$.a[0]b","operatorType":"EQUALS","value":1}`)),
			"condition at /conditions/0/function/criterion"},
		{"pre-check bad operator", impFnWith("f", `{"type":"simple","jsonPath":"$.a","operatorType":"NOPE","value":1}`),
			"condition at /function/criterion"},
		{"pre-check bad pattern", impFnWith("f", `{"type":"simple","jsonPath":"$.a","operatorType":"MATCHES_PATTERN","value":")|("}`),
			"condition at /function/criterion"},
		{"local member bad operator beside a function", impGroup("OR", impFn, `{"type":"simple","jsonPath":"$.a","operatorType":"NOPE","value":1}`),
			"condition at /conditions/1"},
		{"no function member", impGroup("AND", impLocal, `{"type":"function"}`), "function condition at /conditions/1 has no `function` object"},
		{"no name", impGroup("AND", `{"type":"function","function":{}}`), "function condition at /conditions/0 has no function name"},
		{"bad retryPolicy at depth", impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":{"retryPolicy":"SOMETIMES"}}}`),
			`criterion function "g" at /conditions/1: unknown retryPolicy "SOMETIMES"`},
		{"engine group with unknown operator", impGroup("XOR", impFn), `unknown group operator "XOR"`},
		{"pre-check unknown lifecycle field", impFnWith("f", `{"type":"lifecycle","field":"nope","operatorType":"EQUALS","value":"x"}`),
			`condition at /function/criterion: unknown meta filter field "nope"`},
		{"pre-check array clause without [*]", impFnWith("f", `{"type":"array","jsonPath":"$.tags","operatorType":"EQUALS","values":["a"]}`),
			`condition at /function/criterion: invalid field path: jsonPath "$.tags" must end in a trailing array wildcard`},
		{"pre-check group with unknown operator", impGroup("OR", impLocal, impFnWith("f", impGroup("XOR", impLocal))),
			`condition at /conditions/1/function/criterion: invalid condition: unknown group operator "XOR"`},
		{"pre-check NOT with two members", impFnWith("f", impGroup("NOT", impLocal, impLifePre)),
			"condition at /function/criterion: invalid condition: NOT requires exactly one condition, got 2"},
		{"config not an object at depth", impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":"x"}}`),
			`function condition: invalid JSON at "/conditions/1/function/config": a JSON string where an object is expected`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImportRequest(fnImportWF(tc.criterion))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestImport_LimitsCountThroughPreChecks(t *testing.T) {
	// MaxConditionEntries entries in the pre-check, plus the function
	// condition itself, is one over the limit.
	values := make([]any, search.MaxConditionEntries)
	pre, _ := json.Marshal(map[string]any{"type": "array", "jsonPath": "$.t[*]", "operatorType": "EQUALS", "values": values})
	if err := validateImportRequest(fnImportWF(impFnWith("f", string(pre)))); err == nil || !strings.Contains(err.Error(), "the limit is 10000") {
		t.Fatalf("entries: err = %v, want the entry-limit refusal", err)
	}
	deep := impLocal
	for i := 0; i < 50; i++ {
		deep = impGroup("AND", deep)
	}
	if err := validateImportRequest(fnImportWF(impFnWith("f", deep))); err == nil || !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("depth: err = %v, want the parser's depth refusal", err)
	}
}

// Import counts the criterion before any other check, so an oversized
// criterion is refused for its size even when an entry would also be refused
// for its shape (an object).
func TestImport_SizeCheckedFirst(t *testing.T) {
	values := make([]any, search.MaxConditionEntries+1)
	values[1] = map[string]any{"a": 1}
	c, _ := json.Marshal(map[string]any{"type": "array", "jsonPath": "$.t[*]", "operatorType": "EQUALS", "values": values})
	if err := validateImportRequest(fnImportWF(string(c))); err == nil || !strings.Contains(err.Error(), "the limit is 10000") {
		t.Fatalf("err = %v, want the entry-limit refusal", err)
	}
}

func TestCalloutLimits_ResponseTimeoutAtDepth(t *testing.T) {
	c := impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":{"responseTimeoutMs":60001}}}`)
	err := validateCalloutLimits(fnImportWF(c), testMaxResponseTimeout)
	if err == nil || !strings.Contains(err.Error(), `criterion function "g" at /conditions/1: responseTimeoutMs 60001 exceeds`) {
		t.Fatalf("err = %v, want the bound refused with the function's position", err)
	}
	if err := validateCalloutLimits(fnImportWF(impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":{"responseTimeoutMs":-1}}}`)), testMaxResponseTimeout); err == nil {
		t.Fatal("a negative responseTimeoutMs at depth must be refused")
	}
}
