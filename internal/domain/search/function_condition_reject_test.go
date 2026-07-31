package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// A FUNCTION clause is a workflow/transition-criterion shape: the engine
// intercepts it in evaluateCriterion and dispatches it to a compute member.
// Search has no such dispatcher — the parsed FunctionCondition is an empty
// struct (name and config are discarded by ParseCondition), ConditionToFilter
// refuses to translate it, and match.Match has no evaluator for it. Left
// unvalidated it reached the evaluator and surfaced as a 500 on a
// client-supplied condition. ValidateCondition is the single boundary every
// search-shaped entry point funnels through, so rejecting it here covers sync
// search, async submit, grouped stats, conditional delete and gRPC at once.

func TestValidateCondition_FunctionRejected(t *testing.T) {
	err := ValidateCondition(&predicate.FunctionCondition{})
	if err == nil {
		t.Fatal("expected a function condition to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition, so it maps to BAD_REQUEST instead of INVALID_CONDITION: %v", err)
	}
	if !strings.Contains(err.Error(), "criteri") {
		t.Errorf("detail should point the caller at workflow criteria; got %q", err)
	}
}

// The walker must find it at any depth — a nested clause reaches the same
// evaluator as a top-level one.
func TestValidateCondition_FunctionNestedInGroupRejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
			&predicate.GroupCondition{
				Operator: "OR",
				Conditions: []predicate.Condition{
					&predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "GREATER_THAN", Value: 10},
					&predicate.FunctionCondition{},
				},
			},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected a nested function condition to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

// Rejection must not spill onto the other condition types — a tree with no
// function clause still validates.
func TestValidateCondition_NonFunctionTreeStillAccepted(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
			&predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "APPROVED"},
			&predicate.ArrayCondition{JsonPath: "$.tags", Values: []any{"go"}},
		},
	}
	if err := ValidateCondition(cond); err != nil {
		t.Errorf("expected a function-free tree to validate; got %v", err)
	}
}
