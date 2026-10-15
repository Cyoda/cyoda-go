package match_test

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// Prepare holds every leaf operand to spi.ValidateOperand before it
// stringifies it. Stringified, a missing operand reads as "" (EQUALS would
// match the empty string, CONTAINS every string) and an object or array as
// its printed form, so each is refused as an unevaluable leaf that names the
// SPI's refusal. The declared type is STRING, under which every one of these
// operands would otherwise expand.
func TestPrepare_OperandShapeRefused(t *testing.T) {
	cases := []struct {
		name string
		cond predicate.Condition
		want error
	}{
		{"missing operand under EQUALS",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS"}, spi.ErrOperandMissing},
		{"missing operand under CONTAINS",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "CONTAINS"}, spi.ErrOperandMissing},
		{"missing operand on a lifecycle leaf",
			&predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS"}, spi.ErrOperandMissing},
		{"object operand",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: map[string]any{"a": "b"}}, spi.ErrOperandObject},
		{"array operand outside a range",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: []any{"a"}}, spi.ErrOperandArray},
		{"range operand as a comma string",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "BETWEEN", Value: "a,z"}, spi.ErrBetweenNotArray},
		{"range operand with one bound",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "BETWEEN", Value: []any{"a"}}, spi.ErrBetweenCount},
		{"range operand with a null bound",
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "BETWEEN_INCLUSIVE", Value: []any{"a", nil}}, spi.ErrBetweenNullBound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := match.Prepare(tc.cond, typed(spi.String))
			if !errors.Is(err, match.ErrUnevaluableLeaf) || !strings.Contains(err.Error(), tc.want.Error()) {
				t.Fatalf("Prepare() error = %v, want ErrUnevaluableLeaf naming %q", err, tc.want)
			}
			if p.Match([]byte(`{"name":""}`), spi.EntityMeta{State: ""}) {
				t.Error("the Prepared returned with the error matched")
			}
		})
	}
}

// IS_NULL and NOT_NULL take no operand, so a missing one is accepted.
func TestPrepare_NullTestsTakeNoOperand(t *testing.T) {
	for _, op := range []string{"IS_NULL", "NOT_NULL"} {
		if _, err := match.Prepare(&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: op}, typed(spi.String)); err != nil {
			t.Errorf("%s with no operand: Prepare() error = %v", op, err)
		}
	}
}
