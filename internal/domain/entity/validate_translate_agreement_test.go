package entity_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// TestDeleteAndGroupedStats_ClearingImpliesTranslates is the entity-package
// half of internal/domain/search's TestValidateCondition_ClearingImpliesTranslates.
//
// Conditional delete (Handler.planDeleteSelection) and grouped statistics
// (GroupedStatsService.queryGroupedStatsInner) each run
// search.ValidateCondition and then spi.ConditionToFilter — the SAME pair of
// functions the search service runs, reached through the same exported
// helpers, not re-implementations. Both therefore refuse a translation
// failure rather than serving it another way, and this is the check that
// refusing costs no valid request.
//
// It lives here rather than being folded into the search test so that a
// change to either of these two entry points' validation — which is where
// they could drift from search — has a failing test in its own package.
func TestDeleteAndGroupedStats_ClearingImpliesTranslates(t *testing.T) {
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

	assert := func(t *testing.T, cond predicate.Condition) {
		t.Helper()
		if err := search.ValidateCondition(cond); err != nil {
			t.Fatalf("ValidateCondition = %v, want nil (fixture is meant to be valid)", err)
		}
		if _, err := spi.ConditionToFilter(cond, nil); err != nil {
			t.Errorf("cleared ValidateCondition but ConditionToFilter failed: %v — "+
				"conditional delete and grouped stats refuse a translation failure, "+
				"which is only free while this holds", err)
		}
	}

	for _, op := range spi.OperatorNames() {
		t.Run("simple/"+op, func(t *testing.T) {
			assert(t, &predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: op, Value: operandFor(op)})
		})
		t.Run("lifecycle/"+op, func(t *testing.T) {
			assert(t, &predicate.LifecycleCondition{Field: "state", OperatorType: op, Value: operandFor(op)})
		})
	}

	// The shapes a delete or a grouped-stats condition actually arrives as:
	// a nested group, and an array clause, which desugars before the switch.
	t.Run("group", func(t *testing.T) {
		assert(t, &predicate.GroupCondition{
			Operator: "AND",
			Conditions: []predicate.Condition{
				&predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "EQUALS", Value: "v"},
				&predicate.SimpleCondition{JsonPath: "$.tags[*]", OperatorType: "NOT_NULL", Value: nil},
			},
		})
	})
	t.Run("group/not", func(t *testing.T) {
		assert(t, &predicate.GroupCondition{
			Operator: "NOT",
			Conditions: []predicate.Condition{
				&predicate.SimpleCondition{JsonPath: "$.tags[*]", OperatorType: "EQUALS", Value: "red"},
			},
		})
	})
	t.Run("positional subscript", func(t *testing.T) {
		assert(t, &predicate.SimpleCondition{JsonPath: "$.lines[0].sku", OperatorType: "EQUALS", Value: "v"})
	})
}
