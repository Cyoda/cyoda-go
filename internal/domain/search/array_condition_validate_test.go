package search

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// TestValidateCondition_ArrayClauseRequiresWildcard pins path-grammar.md §8:
// the clause tests elements by position, so its path must address elements.
// A bare path addresses the array itself and cannot carry a positional test —
// the spelling and the meaning disagreed.
func TestValidateCondition_ArrayClauseRequiresWildcard(t *testing.T) {
	err := ValidateCondition(&predicate.ArrayCondition{
		OperatorType: "EQUALS",
		JsonPath:     "$.tags", Values: []any{"A"},
	})
	if err == nil {
		t.Fatal("array clause on a bare path must be rejected")
	}
	if !errors.Is(err, ErrInvalidFieldPath) {
		t.Errorf("want ErrInvalidFieldPath, got %v", err)
	}
}

// TestValidateCondition_ArrayClauseAcceptsWildcard is the positive control:
// a trailing wildcard is the well-formed spelling and must be accepted.
func TestValidateCondition_ArrayClauseAcceptsWildcard(t *testing.T) {
	if err := ValidateCondition(&predicate.ArrayCondition{
		OperatorType: "EQUALS",
		JsonPath:     "$.tags[*]", Values: []any{"A", nil, "C"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestValidateCondition_ArrayClauseRejectsObjectOperand pins the operand-shape
// check the ArrayCondition arm previously skipped entirely. Unchecked, an
// object operand reaches the kernel and is compared as the literal text
// "map[a:1]".
func TestValidateCondition_ArrayClauseRejectsObjectOperand(t *testing.T) {
	err := ValidateCondition(&predicate.ArrayCondition{
		OperatorType: "EQUALS",
		JsonPath:     "$.tags[*]", Values: []any{map[string]any{"a": 1}},
	})
	if err == nil {
		t.Fatal("array clause with an object operand must be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("want ErrInvalidCondition, got %v", err)
	}
}

func TestValidateCondition_ArrayClauseRefusals(t *testing.T) {
	cases := []struct {
		name string
		cond *predicate.ArrayCondition
		want error
	}{
		{"missing operator", &predicate.ArrayCondition{JsonPath: "$.s[*]", Values: []any{1}}, ErrInvalidCondition},
		{"unknown operator", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "FROBNICATE", Values: []any{1}}, ErrInvalidCondition},
		{"missing values", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS"}, ErrInvalidCondition},
		{"empty values", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS", Values: []any{}}, ErrInvalidCondition},
		{"all-null values", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS", Values: []any{nil, nil}}, ErrInvalidCondition},
		{"BETWEEN entry not a pair", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "BETWEEN", Values: []any{5}}, ErrInvalidCondition},
		{"earlier wildcard", &predicate.ArrayCondition{JsonPath: "$.orders[*].lines[*]", OperatorType: "EQUALS", Values: []any{"a"}}, ErrInvalidFieldPath},
		{"matrix wildcard", &predicate.ArrayCondition{JsonPath: "$.m[*][*]", OperatorType: "EQUALS", Values: []any{1}}, ErrInvalidFieldPath},
	}
	for _, tc := range cases {
		if err := ValidateCondition(tc.cond); !errors.Is(err, tc.want) {
			t.Errorf("%s: ValidateCondition: want %v, got %v", tc.name, tc.want, err)
		}
		if tc.want == ErrInvalidCondition {
			if err := ValidateCriterionCondition(tc.cond); !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s: ValidateCriterionCondition: want ErrInvalidCondition, got %v", tc.name, err)
			}
		}
	}
}

func TestValidateCondition_ArrayClauseAcceptsOperators(t *testing.T) {
	for _, c := range []*predicate.ArrayCondition{
		{JsonPath: "$.s[*]", OperatorType: "GREATER_THAN", Values: []any{50, nil, 70}},
		{JsonPath: "$.s[*]", OperatorType: "BETWEEN_INCLUSIVE", Values: []any{[]any{1, 5}}},
		{JsonPath: "$.s[*]", OperatorType: "IS_NULL", Values: []any{nil, true}},
		{JsonPath: "$.orders[0].lines[*]", OperatorType: "EQUALS", Values: []any{"a"}},
	} {
		if err := ValidateCondition(c); err != nil {
			t.Errorf("%+v: unexpected error: %v", c, err)
		}
	}
}
