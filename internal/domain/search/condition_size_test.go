package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// arrayClauseOfSize is an array clause with n entries, the first non-null
// and the rest null. A null entry tests nothing but is allocated all the
// same, so it counts towards the limit.
func arrayClauseOfSize(n int) *predicate.ArrayCondition {
	values := make([]any, n)
	values[0] = "a"
	return &predicate.ArrayCondition{JsonPath: "$.tags[*]", OperatorType: "EQUALS", Values: values}
}

// groupOfSimpleLeaves is an AND group of n simple clauses.
func groupOfSimpleLeaves(n int) *predicate.GroupCondition {
	children := make([]predicate.Condition, n)
	for i := range children {
		children[i] = &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "a"}
	}
	return &predicate.GroupCondition{Operator: "AND", Conditions: children}
}

// A condition names at most MaxConditionEntries tested entries, counted
// before it is expanded: an array clause counts one per entry in `values`
// and every other leaf counts one. At the limit it is accepted; one past it
// is INVALID_CONDITION naming the limit and the count, from both entry
// points.
func TestValidateCondition_SizeLimit(t *testing.T) {
	if MaxConditionEntries != 10_000 {
		t.Fatalf("MaxConditionEntries = %d, want 10000", MaxConditionEntries)
	}
	for _, tc := range []struct {
		name  string
		build func(int) predicate.Condition
	}{
		{"array clause", func(n int) predicate.Condition { return arrayClauseOfSize(n) }},
		{"group of simple clauses", func(n int) predicate.Condition { return groupOfSimpleLeaves(n) }},
		{"array clause and simple clauses in nested groups", func(n int) predicate.Condition {
			half := n / 2
			return &predicate.GroupCondition{Operator: "OR", Conditions: []predicate.Condition{
				arrayClauseOfSize(half),
				&predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{groupOfSimpleLeaves(n - half)}},
			}}
		}},
	} {
		at := tc.build(MaxConditionEntries)
		if err := ValidateCondition(at); err != nil {
			t.Errorf("%s at the limit: ValidateCondition: unexpected error %v", tc.name, err)
		}
		if err := ValidateCriterionCondition(at); err != nil {
			t.Errorf("%s at the limit: ValidateCriterionCondition: unexpected error %v", tc.name, err)
		}
		over := tc.build(MaxConditionEntries + 1)
		for name, err := range map[string]error{
			"ValidateCondition":     ValidateCondition(over),
			"ValidateConditionSize": ValidateConditionSize(over),
		} {
			if !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s over the limit: %s: want ErrInvalidCondition, got %v", tc.name, name, err)
				continue
			}
			if !strings.Contains(err.Error(), "10000") || !strings.Contains(err.Error(), "10001") {
				t.Errorf("%s over the limit: %s: message must name the limit and the count, got %q", tc.name, name, err.Error())
			}
		}
	}
}

// The size check runs before any other check, so an oversized condition is
// refused for its size before it is expanded: an entry that every later
// check would also refuse (an object) does not decide the answer.
func TestValidateCondition_SizeLimit_CheckedFirst(t *testing.T) {
	c := arrayClauseOfSize(MaxConditionEntries + 1)
	c.Values[1] = map[string]any{"a": 1}
	if err := ValidateCondition(c); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxConditionEntries)) {
		t.Errorf("ValidateCondition: want the size refusal, got %v", err)
	}
}

// ValidateCriterionCondition checks one local unit of a criterion. It does
// not count entries: workflow import counts the whole criterion, pre-checks
// included, with ValidateConditionSize before it splits the criterion into
// units, and a unit's own count would miss the rest of the criterion.
func TestValidateCriterionCondition_DoesNotCountEntries(t *testing.T) {
	if err := ValidateCriterionCondition(groupOfSimpleLeaves(MaxConditionEntries + 1)); err != nil {
		t.Errorf("ValidateCriterionCondition: want no size refusal on a unit, got %v", err)
	}
}

func TestConditionEntries_CountsThePreCheck(t *testing.T) {
	cond, err := predicate.ParseCondition([]byte(`{"type":"group","operator":"AND","conditions":[
		{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1},
		{"type":"function","function":{"name":"f","criterion":{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":[1,null,3]}}}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := conditionEntries(cond); got != 1+1+3 {
		t.Errorf("conditionEntries = %d, want 5 (simple 1, function 1, its array pre-check 3)", got)
	}
}

func TestValidateConditionSize_RefusesAnOversizedPreCheck(t *testing.T) {
	values := make([]any, MaxConditionEntries)
	pre, _ := json.Marshal(map[string]any{"type": "array", "jsonPath": "$.t[*]", "operatorType": "EQUALS", "values": values})
	cond, err := predicate.ParseCondition([]byte(`{"type":"function","function":{"name":"f","criterion":` + string(pre) + `}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = ValidateConditionSize(cond)
	if !errors.Is(err, ErrInvalidCondition) {
		t.Fatalf("err = %v, want ErrInvalidCondition (10,001 entries)", err)
	}
}
