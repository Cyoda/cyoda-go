package search

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// C1/M4 regression tests — a malformed BETWEEN operand (a scalar, or a 1- or
// 3-element array instead of the required 2-element [lo, hi] pair) previously
// passed ValidateCondition unchecked. betweenValues then returned nil Values
// for the built spi.Filter, which downstream diverged catastrophically across
// backends: postgres panicked (index out of range on f.Values[0]), sqlite
// matched every row ("1=1"), and memory excluded every row (correct). These
// tests prove the arity is rejected at the single validation boundary shared
// by every transport, closing the gap before it ever reaches a storage plugin.

func TestValidateCondition_SimpleBetween_ScalarValue_Rejected(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        "not-an-array",
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for scalar BETWEEN operand, got nil")
	}
}

func TestValidateCondition_SimpleBetween_OneElementArray_Rejected(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        []any{float64(18)},
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for 1-element BETWEEN operand, got nil")
	}
}

func TestValidateCondition_SimpleBetween_ThreeElementArray_Rejected(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        []any{float64(18), float64(40), float64(65)},
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for 3-element BETWEEN operand, got nil")
	}
}

func TestValidateCondition_SimpleBetween_TwoElementArray_Accepted(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        []any{float64(18), float64(65)},
	}
	if err := ValidateCondition(c); err != nil {
		t.Errorf("well-formed 2-element BETWEEN should be accepted, got: %v", err)
	}
}

func TestValidateCondition_SimpleBetweenInclusive_WrongArity_Rejected(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN_INCLUSIVE",
		Value:        float64(18),
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for scalar BETWEEN_INCLUSIVE operand, got nil")
	}
}

// Temporal-specific: the C1 repro condition — a scalar RFC3339 string against
// a lifecycle temporal field. This is exactly the malformed condition from the
// bug report:
// {"type":"lifecycle","field":"creationDate","operatorType":"BETWEEN","value":"2021-01-01T00:00:00Z"}
func TestValidateCondition_LifecycleBetween_ScalarTemporalValue_Rejected(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        "2021-01-01T00:00:00Z",
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for scalar BETWEEN operand on lifecycle condition, got nil")
	}
}

func TestValidateCondition_LifecycleBetween_OneElementArray_Rejected(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2021-01-01T00:00:00Z"},
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for 1-element BETWEEN operand on lifecycle condition, got nil")
	}
}

func TestValidateCondition_LifecycleBetween_ThreeElementArray_Rejected(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2021-01-01T00:00:00Z", "2021-06-01T00:00:00Z", "2021-12-31T00:00:00Z"},
	}
	if err := ValidateCondition(c); err == nil {
		t.Fatal("expected error for 3-element BETWEEN operand on lifecycle condition, got nil")
	}
}

func TestValidateCondition_LifecycleBetween_TwoElementArray_Accepted(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2021-01-01T00:00:00Z", "2021-12-31T00:00:00Z"},
	}
	if err := ValidateCondition(c); err != nil {
		t.Errorf("well-formed 2-element lifecycle BETWEEN should be accepted, got: %v", err)
	}
}

// Non-BETWEEN operators must not be affected by the arity check at all.
func TestValidateCondition_NonBetweenOperator_ArityCheckSkipped(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}
	if err := ValidateCondition(c); err != nil {
		t.Errorf("EQUALS should not be subject to BETWEEN arity check, got: %v", err)
	}
}
