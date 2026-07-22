package match

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

func TestToFloat64_JSONNumber(t *testing.T) {
	cases := []struct {
		in   json.Number
		want float64
	}{
		{"0", 0},
		{"42", 42},
		{"-1.5", -1.5},
		{"1e10", 1e10},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			got, err := toFloat64(tc.in)
			if err != nil {
				t.Fatalf("toFloat64(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("toFloat64(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestOpCompare_NoStringOperandCoercion pins the numeric-alignment contract
// change (#423 §6.6 / #431 seed): a string operand is not numerically
// coerced against a numeric field, aligning internal/match with
// spi.compareFilterValues (the pushdown evaluator) so the workflow-criteria
// fallback path and search pushdown agree on string-encoded-numeric operands.
func TestOpCompare_NoStringOperandCoercion(t *testing.T) {
	// numeric field value 100, string operand "20": must NOT be numeric
	// (100>20 would be true numerically); lexical "100" > "20" is false.
	actual := gjson.Parse(`100`)
	got, _ := opCompare(actual, "20", func(a, b float64) bool { return a > b }, func(a, b string) bool { return a > b })
	if got {
		t.Error("string operand must not be numerically coerced (align to spi)")
	}
}

// TestOpCompare_GenuineNumericOperandStillCoerces guards the common case:
// a genuine numeric operand type still compares numerically after the
// alignment (only string-encoded numerics change behaviour).
func TestOpCompare_GenuineNumericOperandStillCoerces(t *testing.T) {
	actual := gjson.Parse(`100`)
	got, err := opCompare(actual, float64(20), func(a, b float64) bool { return a > b }, func(a, b string) bool { return a > b })
	if err != nil || !got {
		t.Errorf("genuine numeric operand must still compare numerically (100>20); got=%v err=%v", got, err)
	}
}

// TestOpEquals_NoStringOperandCoercion mirrors the opCompare alignment for
// opEquals: a string operand that looks numeric must not be coerced —
// EQUALS falls through to the lexical string comparison instead.
func TestOpEquals_NoStringOperandCoercion(t *testing.T) {
	actual := gjson.Parse(`100`)
	// "100" lexically equals actual.String() ("100"), so this stays true —
	// the meaningful divergence case is proven by TestOpCompare above where
	// "20" != "100" lexically but would coerce numerically to a different
	// (true) comparison result. Here we assert the numeric-looking-but-
	// non-matching string is NOT coerced into a numeric equals.
	got := opEquals(actual, "100.0")
	if got {
		t.Error("string operand \"100.0\" must not be numerically coerced against numeric 100 (lexical \"100\" != \"100.0\")")
	}
}

// TestOpEquals_JSONNumber proves that the toFloat64 extension propagates
// through opEquals on the scalar EQUALS path — not just the array path.
// Spec Section 4.3 calls for this integration-level coverage explicitly,
// because PR-2's XML import produces json.Number values that flow into
// EQUALS predicates against scalar entity fields, not only into array
// predicates. This test guards against future regressions to opEquals
// or toFloat64 that would silently break the scalar EQUALS path.
func TestOpEquals_JSONNumber(t *testing.T) {
	data := []byte(`{"score":1.5}`)
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.score",
		OperatorType: "EQUALS",
		Value:        json.Number("1.5"),
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected match: scalar EQUALS with json.Number(\"1.5\") against JSON 1.5")
	}
}
