package search_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// TestValidateCondition_ClearingImpliesTranslates is the empirical check for
// service_test.go's claim (TestSearch_FallbackBranchIsBounded_TranslateFailureRoute's
// doc comment): once a non-nil condition clears search.ValidateCondition,
// every reachable shape also clears spi.ConditionToFilter — i.e. the
// in-memory translate-failure fallback is unreachable from validated,
// non-nil input.
//
// Before this deliverable's C1 fix that was FALSE and undetected: the
// boundary's subscript check (digit class only) and spi.ParseFilterPath's
// (digit class plus an int-magnitude bound) disagreed on an overflowing
// index, so "$.tags[99999999999999999999]" cleared ValidateCondition and
// then failed ConditionToFilter. ValidateCondition now delegates directly to
// spi.ParseFilterPath (jsonpath_grammar.go) and canonicalOperators is built
// directly from spi.OperatorNames() (operators.go) — both grammars are the
// same function call, not independently maintained copies — so this
// exercises every operator name, both a data path and a meta field, and
// group/array nesting, and asserts the claim holds by running the real
// functions rather than trusting the enumeration argument alone.
func TestValidateCondition_ClearingImpliesTranslates(t *testing.T) {
	operandFor := func(op string) any {
		switch op {
		case "BETWEEN", "BETWEEN_INCLUSIVE":
			return []any{"a", "z"}
		case "IS_NULL", "NOT_NULL":
			return nil
		default:
			return "v"
		}
	}

	for _, op := range spi.OperatorNames() {
		t.Run("simple/"+op, func(t *testing.T) {
			cond := &predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: op, Value: operandFor(op)}
			assertClearingImpliesTranslates(t, cond)
		})
		t.Run("lifecycle/"+op, func(t *testing.T) {
			cond := &predicate.LifecycleCondition{Field: "state", OperatorType: op, Value: operandFor(op)}
			assertClearingImpliesTranslates(t, cond)
		})
	}

	t.Run("group", func(t *testing.T) {
		cond := &predicate.GroupCondition{
			Operator: "AND",
			Conditions: []predicate.Condition{
				&predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "EQUALS", Value: "v"},
				&predicate.SimpleCondition{JsonPath: "$.tags[*]", OperatorType: "NOT_NULL", Value: nil},
			},
		}
		assertClearingImpliesTranslates(t, cond)
	})

	t.Run("array", func(t *testing.T) {
		cond := &predicate.ArrayCondition{JsonPath: "$.tags[*]", Values: []any{"a", nil, "b"}}
		assertClearingImpliesTranslates(t, cond)
	})

	t.Run("array positional index", func(t *testing.T) {
		cond := &predicate.SimpleCondition{JsonPath: "$.arr[0]", OperatorType: "EQUALS", Value: "v"}
		assertClearingImpliesTranslates(t, cond)
	})

	t.Run("array wildcard mid-path", func(t *testing.T) {
		cond := &predicate.SimpleCondition{JsonPath: "$.items[*].name", OperatorType: "EQUALS", Value: "widget"}
		assertClearingImpliesTranslates(t, cond)
	})
}

// assertClearingImpliesTranslates is the shared assertion: if cond clears
// search.ValidateCondition, spi.ConditionToFilter must not error.
func assertClearingImpliesTranslates(t *testing.T, cond predicate.Condition) {
	t.Helper()
	if err := search.ValidateCondition(cond); err != nil {
		t.Fatalf("ValidateCondition = %v, want nil (test fixture is meant to be a valid condition)", err)
	}
	if _, err := spi.ConditionToFilter(cond, nil); err != nil {
		t.Errorf("cleared ValidateCondition but ConditionToFilter failed: %v — the claim in "+
			"TestSearch_FallbackBranchIsBounded_TranslateFailureRoute's doc comment is violated", err)
	}
}
