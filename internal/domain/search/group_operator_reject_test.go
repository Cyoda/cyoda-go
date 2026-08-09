package search

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// A GroupCondition's Operator was never checked by validateConditionAtDepth —
// only its children were recursed into. An operator other than "AND"/"OR"
// then cleared validation and the two execution paths disagreed on what to
// do with it: spi.ConditionToFilter's groupToFilter maps anything non-"OR"
// (case-insensitively) to FilterAnd, silently answering 200 with the wrong
// rows, while match.Prepare requires exactly "AND"/"OR" and returns a bare
// "unknown group operator" error that search/service.go surfaces as a 500 on
// client-supplied input. Rejecting it here, at the boundary both paths funnel
// through, closes the divergence the same way the FunctionCondition arm below
// closes its own 500-on-client-input class.

func TestValidateCondition_GroupOperatorAND_Accepted(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	if err := ValidateCondition(cond); err != nil {
		t.Errorf("expected AND group to be accepted, got: %v", err)
	}
}

func TestValidateCondition_GroupOperatorOR_Accepted(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "OR",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	if err := ValidateCondition(cond); err != nil {
		t.Errorf("expected OR group to be accepted, got: %v", err)
	}
}

func TestValidateCondition_GroupOperatorLowercaseOr_Rejected(t *testing.T) {
	// Lowercase "or" is part of the divergence being closed: the pushdown
	// translator matches it case-insensitively (behaving as OR) while
	// match.Prepare rejects it outright. Both the parser and the predicate
	// evaluator require uppercase, so rejecting it here is correct, not
	// merely convenient.
	cond := &predicate.GroupCondition{
		Operator: "or",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected lowercase \"or\" group operator to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition, so it maps to BAD_REQUEST instead of INVALID_CONDITION: %v", err)
	}
}

func TestValidateCondition_GroupOperatorXOR_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "XOR",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected XOR group operator to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

func TestValidateCondition_GroupOperatorNOT_Rejected(t *testing.T) {
	// "NOT" is an advertised enum value in the generated API types but has
	// never been representable by GroupCondition (which only ever carries
	// "AND"/"OR" children) — it must still be rejected here, not silently
	// treated as AND by the pushdown translator.
	cond := &predicate.GroupCondition{
		Operator: "NOT",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected NOT group operator to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

func TestValidateCondition_GroupOperatorEmpty_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected empty group operator to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

// The walker must find a bad operator at any depth — recursing into a
// nested group must not bypass the check on the outer or inner group.
func TestValidateCondition_NestedGroupBadOperator_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
			&predicate.GroupCondition{
				Operator: "XOR",
				Conditions: []predicate.Condition{
					&predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "GREATER_THAN", Value: 10},
				},
			},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected a nested bad group operator to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}
