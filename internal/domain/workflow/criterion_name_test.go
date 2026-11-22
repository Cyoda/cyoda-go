package workflow

import "testing"

// criterionName reads the criterion with the same parsers import used, so a
// shape import refuses names nothing. A function's name is under `function`;
// a top-level `name` is not part of the function condition.
func TestCriterionName(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"function","function":{"name":"credit-check"}}`:               "credit-check",
		`{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1}`: "simple",
		`{"type":"group","operator":"AND","conditions":[]}`:                    "group",
		`{"type":"function","name":"legacy"}`:                                  "",
		`{"type":"function","function":{"Name":"case-variant"}}`:               "",
		`{"type":"simple","JsonPath":"$.a","operatorType":"EQUALS","value":1}`: "",
		`not json`: "",
	} {
		if got := criterionName([]byte(in)); got != want {
			t.Errorf("criterionName(%s) = %q, want %q", in, got, want)
		}
	}
}
