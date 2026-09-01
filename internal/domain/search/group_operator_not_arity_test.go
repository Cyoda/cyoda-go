package search

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// group_operator_not_arity_test.go pins Task 12 of the NOT-node plan: the
// two group-condition validator arms (ValidateCondition's search-condition
// walk and ValidateCriterionCondition's criterion walk) now accept "NOT" as
// a group operator, but only with EXACTLY one entry in Conditions. Zero, or
// two or more, is rejected as 400 INVALID_CONDITION — the same sentinel
// (ErrInvalidCondition) every other structural rejection in this file wraps,
// so it maps identically on every surface that funnels through these
// validators (search.StructuralConditionErrCode / the criterion import
// path's location-wrapped error).
//
// The check deliberately lives here, not in predicate.ParseCondition: a
// parser-level arity check would still leave validateCriterion (which
// discards a parse failure and returns nil) unable to catch a malformed NOT
// criterion, and it would let it import 200 and then fail every subsequent
// evaluation of that transition permanently. See operators.go's group-arm
// comments for the full rationale this test file exercises.
//
// The operator match stays case-sensitive: "not" (lowercase) is not "NOT"
// and is rejected as an unknown group operator regardless of arity, exactly
// like "or"/"and" already are — TestValidateCondition_GroupOperatorLowercaseOr_Rejected
// in group_operator_reject_test.go pins that half of the contract.

func simpleAliceCondition() *predicate.SimpleCondition {
	return &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
}

func simpleAmountCondition() *predicate.SimpleCondition {
	return &predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "GREATER_THAN", Value: 10}
}

// --- ValidateCondition (search-condition surface) ---

func TestValidateCondition_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator:   "NOT",
		Conditions: []predicate.Condition{simpleAliceCondition()},
	}
	if err := ValidateCondition(cond); err != nil {
		t.Errorf("expected NOT with exactly one condition to be accepted, got: %v", err)
	}
}

func TestValidateCondition_GroupOperatorNOT_ZeroConditions_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator:   "NOT",
		Conditions: []predicate.Condition{},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected NOT with zero conditions to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition, so it maps to BAD_REQUEST instead of INVALID_CONDITION: %v", err)
	}
}

func TestValidateCondition_GroupOperatorNOT_TwoConditions_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator:   "NOT",
		Conditions: []predicate.Condition{simpleAliceCondition(), simpleAmountCondition()},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected NOT with two conditions to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

// --- ValidateCriterionCondition (workflow-criterion surface) ---

func TestValidateCriterionCondition_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator:   "NOT",
		Conditions: []predicate.Condition{simpleAliceCondition()},
	}
	if err := ValidateCriterionCondition(cond); err != nil {
		t.Errorf("expected NOT with exactly one condition to be accepted, got: %v", err)
	}
}

func TestValidateCriterionCondition_GroupOperatorNOT_ZeroConditions_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator:   "NOT",
		Conditions: []predicate.Condition{},
	}
	err := ValidateCriterionCondition(cond)
	if err == nil {
		t.Fatal("expected NOT with zero conditions to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

func TestValidateCriterionCondition_GroupOperatorNOT_TwoConditions_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator:   "NOT",
		Conditions: []predicate.Condition{simpleAliceCondition(), simpleAmountCondition()},
	}
	err := ValidateCriterionCondition(cond)
	if err == nil {
		t.Fatal("expected NOT with two conditions to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}

// A nested NOT with bad arity must be caught at any depth, mirroring
// TestValidateCondition_NestedGroupBadOperator_Rejected in
// group_operator_reject_test.go for the operator-name check.
func TestValidateCondition_NestedGroupNOTBadArity_Rejected(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			simpleAliceCondition(),
			&predicate.GroupCondition{
				Operator:   "NOT",
				Conditions: []predicate.Condition{simpleAliceCondition(), simpleAmountCondition()},
			},
		},
	}
	err := ValidateCondition(cond)
	if err == nil {
		t.Fatal("expected a nested NOT with bad arity to be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("error does not wrap ErrInvalidCondition: %v", err)
	}
}
