package search

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
		if tc.name == "all-null values" {
			if err := ValidateCondition(tc.cond); !errors.Is(err, spi.ErrArrayClauseTestsNothing) {
				t.Errorf("%s: want spi.ErrArrayClauseTestsNothing in the chain, got %v", tc.name, err)
			}
		}
		if tc.want == ErrInvalidCondition {
			if err := ValidateCriterionCondition(tc.cond); !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s: ValidateCriterionCondition: want ErrInvalidCondition, got %v", tc.name, err)
			}
		}
	}
}

func TestValidateCondition_ArrayClauseAcceptsOperators(t *testing.T) {
	for _, c := range acceptedArrayClauses {
		if err := ValidateCondition(c); err != nil {
			t.Errorf("%+v: unexpected error: %v", c, err)
		}
		if err := ValidateCriterionCondition(c); err != nil {
			t.Errorf("%+v: ValidateCriterionCondition: unexpected error: %v", c, err)
		}
	}
}

var acceptedArrayClauses = func() []*predicate.ArrayCondition {
	return []*predicate.ArrayCondition{
		{JsonPath: "$.s[*]", OperatorType: "GREATER_THAN", Values: []any{50, nil, 70}},
		{JsonPath: "$.s[*]", OperatorType: "BETWEEN_INCLUSIVE", Values: []any{[]any{1, 5}}},
		{JsonPath: "$.s[*]", OperatorType: "IS_NULL", Values: []any{nil, true}},
		{JsonPath: "$.orders[0].lines[*]", OperatorType: "EQUALS", Values: []any{"a"}},
	}
}()

// A BETWEEN without a two-element pair is INVALID_CONDITION on every clause.
func TestValidateCondition_BetweenArity_IsInvalidCondition(t *testing.T) {
	for _, c := range []predicate.Condition{
		&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: "BETWEEN", Value: 5},
		&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: "BETWEEN", Value: []any{1, 2, 3}},
		&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "BETWEEN", Value: "2021-01-01T00:00:00Z"},
	} {
		if err := ValidateCondition(c); !errors.Is(err, ErrInvalidCondition) {
			t.Errorf("%+v: want ErrInvalidCondition, got %v", c, err)
		}
	}
}

// A null bound inside a BETWEEN pair is INVALID_CONDITION on every clause.
func TestValidateCondition_BetweenNullBound_IsInvalidCondition(t *testing.T) {
	pairs := [][]any{{nil, 5}, {5, nil}, {nil, nil}}
	for _, op := range []string{"BETWEEN", "BETWEEN_INCLUSIVE"} {
		for _, p := range pairs {
			lifePair := make([]any, len(p))
			for i, v := range p {
				if v != nil {
					lifePair[i] = "2021-01-01T00:00:00Z"
				}
			}
			for _, c := range []predicate.Condition{
				&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: op, Value: p},
				&predicate.LifecycleCondition{Field: "creationDate", OperatorType: op, Value: lifePair},
				&predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: op, Values: []any{p}},
			} {
				if err := ValidateCondition(c); !errors.Is(err, ErrInvalidCondition) {
					t.Errorf("%s %v %+v: want ErrInvalidCondition, got %v", op, p, c, err)
				}
			}
		}
	}
}

// An array operand is refused except under BETWEEN / BETWEEN_INCLUSIVE, where
// it is the [low, high] pair. No other operator takes a set of values; left
// unchecked, the backends evaluate such an operand differently (memory
// compares the stringified array, sqlite errors on CONTAINS, matches every
// row on NOT_EQUAL and none on EQUALS). It is a shape error: INVALID_CONDITION
// on every clause, from both the search and the criterion entry point.
func TestValidateCondition_ArrayOperand_RefusedExceptBetween(t *testing.T) {
	pair := []any{"a", "b"}
	for _, op := range []string{"EQUALS", "NOT_EQUAL", "CONTAINS", "GREATER_THAN", "IS_NULL", "MATCHES_PATTERN"} {
		for _, c := range []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: op, Value: pair},
			&predicate.LifecycleCondition{Field: "state", OperatorType: op, Value: pair},
			&predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: op, Values: []any{pair}},
			&predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: op, Values: []any{"a", []any{}}},
		} {
			if err := ValidateCondition(c); !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s %+v: ValidateCondition: want ErrInvalidCondition, got %v", op, c, err)
			}
			if err := ValidateCriterionCondition(c); !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s %+v: ValidateCriterionCondition: want ErrInvalidCondition, got %v", op, c, err)
			}
		}
	}
	// The pair stays accepted where it belongs.
	for _, op := range []string{"BETWEEN", "BETWEEN_INCLUSIVE"} {
		for _, c := range []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: op, Value: []any{1, 5}},
			&predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: op, Values: []any{[]any{1, 5}}},
		} {
			if err := ValidateCondition(c); err != nil {
				t.Errorf("%s %+v: ValidateCondition: unexpected error %v", op, c, err)
			}
			if err := ValidateCriterionCondition(c); err != nil {
				t.Errorf("%s %+v: ValidateCriterionCondition: unexpected error %v", op, c, err)
			}
		}
	}
}

// A clause with no tested position names the field a client must send. A
// client on the old shape (`value` instead of `values`) otherwise reads only
// "tests no position" and has nothing to correct.
func TestValidateCondition_ArrayClauseTestsNothing_NamesValues(t *testing.T) {
	c := &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS"}
	for name, err := range map[string]error{
		"ValidateCondition":          ValidateCondition(c),
		"ValidateCriterionCondition": ValidateCriterionCondition(c),
	} {
		if !errors.Is(err, spi.ErrArrayClauseTestsNothing) || !errors.Is(err, ErrInvalidCondition) {
			t.Fatalf("%s: want ErrInvalidCondition wrapping ErrArrayClauseTestsNothing, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "`values` with at least one non-null entry") {
			t.Errorf("%s: message must name `values`, got %q", name, err.Error())
		}
	}
}

// The criterion entry point refuses a null BETWEEN bound exactly as the
// search entry point does.
func TestValidateCriterionCondition_BetweenNullBound_IsInvalidCondition(t *testing.T) {
	for _, op := range []string{"BETWEEN", "BETWEEN_INCLUSIVE"} {
		for _, c := range []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: op, Value: []any{nil, 5}},
			&predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: op, Values: []any{[]any{nil, 5}}},
		} {
			if err := ValidateCriterionCondition(c); !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s %+v: want ErrInvalidCondition, got %v", op, c, err)
			}
		}
	}
}

// A BETWEEN bound is a non-null scalar. An object or an array bound denotes no
// value the range could compare against: unchecked, PostgreSQL answers 500 and
// the in-memory path compares against the bound's printed form, which on a
// conditional delete chooses the rows deleted. INVALID_CONDITION on every
// clause, from both the search and the criterion entry point.
func TestValidateCondition_BetweenNonScalarBound_IsInvalidCondition(t *testing.T) {
	pairs := [][]any{
		{map[string]any{"a": 1}, "z"},
		{"a", map[string]any{}},
		{[]any{"a"}, "z"},
		{"a", []any{}},
	}
	for _, op := range []string{"BETWEEN", "BETWEEN_INCLUSIVE"} {
		for _, p := range pairs {
			for _, c := range []predicate.Condition{
				&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: op, Value: p},
				&predicate.LifecycleCondition{Field: "creationDate", OperatorType: op, Value: p},
				&predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: op, Values: []any{p}},
			} {
				if err := ValidateCondition(c); !errors.Is(err, ErrInvalidCondition) {
					t.Errorf("%s %v %+v: ValidateCondition: want ErrInvalidCondition, got %v", op, p, c, err)
				}
				if err := ValidateCriterionCondition(c); !errors.Is(err, ErrInvalidCondition) {
					t.Errorf("%s %v %+v: ValidateCriterionCondition: want ErrInvalidCondition, got %v", op, p, c, err)
				}
			}
		}
	}
}
