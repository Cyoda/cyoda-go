package match

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// --- operandToString ---

func TestOperandToString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{true, "true"},
		{false, "false"},
		{json.Number("1.5"), "1.5"},
		{json.Number("1e10"), "1e10"},
		{float64(42), "42"},
		{int(7), "7"},
	}
	for _, tc := range cases {
		if got := operandToString(tc.in); got != tc.want {
			t.Errorf("operandToString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- betweenBounds ---

func TestBetweenBounds(t *testing.T) {
	if got := betweenBounds([]any{float64(25), float64(35)}); len(got) != 2 || got[0] != "25" || got[1] != "35" {
		t.Errorf("[]any bounds: got %v", got)
	}
	if got := betweenBounds("25,35"); len(got) != 2 || got[0] != "25" || got[1] != "35" {
		t.Errorf("comma-string bounds: got %v", got)
	}
	if got := betweenBounds([]any{float64(1)}); got != nil {
		t.Errorf("1-element []any must yield nil, got %v", got)
	}
	if got := betweenBounds("only-one"); got != nil {
		t.Errorf("comma-less string must yield nil, got %v", got)
	}
	if got := betweenBounds(42); got != nil {
		t.Errorf("unsupported type must yield nil, got %v", got)
	}
}

// --- opNameToFilterOp / negatedStringOp ---

func TestOpNameToFilterOp(t *testing.T) {
	if op, ok := opNameToFilterOp("EQUALS"); !ok || op != spi.FilterEq {
		t.Errorf("EQUALS → %v,%v", op, ok)
	}
	if op, ok := opNameToFilterOp("BETWEEN_INCLUSIVE"); !ok || op != spi.FilterBetweenInclusive {
		t.Errorf("BETWEEN_INCLUSIVE → %v,%v", op, ok)
	}
	if op, ok := opNameToFilterOp("MATCHES_PATTERN"); !ok || op != spi.FilterMatchesRegex {
		t.Errorf("MATCHES_PATTERN → %v,%v", op, ok)
	}
	// Dropped operators are not mapped (applyOperator turns these into errors).
	for _, name := range []string{"IS_CHANGED", "IS_UNCHANGED", "TOTALLY_UNKNOWN"} {
		if _, ok := opNameToFilterOp(name); ok {
			t.Errorf("%s must not map to a FilterOp", name)
		}
	}
	// Case-sensitive negatives are handled by negatedStringOp, not opNameToFilterOp.
	for _, name := range []string{"NOT_CONTAINS", "NOT_STARTS_WITH", "NOT_ENDS_WITH"} {
		if _, ok := opNameToFilterOp(name); ok {
			t.Errorf("%s must be routed via negatedStringOp, not opNameToFilterOp", name)
		}
		if _, ok := negatedStringOp(name); !ok {
			t.Errorf("negatedStringOp(%s) must be recognised", name)
		}
	}
}

// --- applyOperator: kernel routing ---

// TestApplyOperator_ComparisonNeedsDeclared proves the type-directed contract:
// a comparison operator with NO declared types degrades to non-match (the
// kernel cannot classify the operand), while the same comparison with a
// declared numeric type matches.
func TestApplyOperator_ComparisonNeedsDeclared(t *testing.T) {
	stored := gjson.Parse(`100`)

	// No declared types → non-match.
	got, err := applyOperator("GREATER_THAN", stored, float64(20), nil)
	if err != nil || got {
		t.Errorf("untyped GREATER_THAN must non-match; got=%v err=%v", got, err)
	}

	// Declared numeric → numeric comparison, matches.
	got, err = applyOperator("GREATER_THAN", stored, float64(20), []spi.DataType{spi.Integer})
	if err != nil || !got {
		t.Errorf("typed GREATER_THAN (100>20) must match; got=%v err=%v", got, err)
	}
}

// TestApplyOperator_StringOpsAreDeclarationIndependent proves string operators
// and the null tests work without declared types (they are not type-directed).
func TestApplyOperator_StringOpsAreDeclarationIndependent(t *testing.T) {
	stored := gjson.Parse(`"Alice"`)
	if got, err := applyOperator("CONTAINS", stored, "lic", nil); err != nil || !got {
		t.Errorf("CONTAINS without declared must still match; got=%v err=%v", got, err)
	}
	if got, err := applyOperator("STARTS_WITH", stored, "Al", nil); err != nil || !got {
		t.Errorf("STARTS_WITH without declared must still match; got=%v err=%v", got, err)
	}
	absent := gjson.Result{}
	if got, err := applyOperator("IS_NULL", absent, nil, nil); err != nil || !got {
		t.Errorf("IS_NULL on absent must match; got=%v err=%v", got, err)
	}
}

// TestApplyOperator_NegatedStringOpNullUniformity pins the kernel-aligned
// null/non-textual uniformity for the case-sensitive negatives: a present
// textual value that does not contain the operand matches; an absent value is
// a non-match (not a spurious vacuous match, unlike the pre-kernel behaviour).
func TestApplyOperator_NegatedStringOpNullUniformity(t *testing.T) {
	present := gjson.Parse(`"Alice"`)
	if got, err := applyOperator("NOT_CONTAINS", present, "xyz", nil); err != nil || !got {
		t.Errorf("NOT_CONTAINS on present non-containing value must match; got=%v err=%v", got, err)
	}
	if got, err := applyOperator("NOT_CONTAINS", present, "lic", nil); err != nil || got {
		t.Errorf("NOT_CONTAINS on present containing value must non-match; got=%v err=%v", got, err)
	}
	absent := gjson.Result{}
	if got, err := applyOperator("NOT_CONTAINS", absent, "xyz", nil); err != nil || got {
		t.Errorf("NOT_CONTAINS on absent must non-match (null uniformity); got=%v err=%v", got, err)
	}
}

// TestApplyOperator_UnsupportedOperatorErrors confirms IS_CHANGED / IS_UNCHANGED
// and unknown operators are hard errors (not silent non-matches).
func TestApplyOperator_UnsupportedOperatorErrors(t *testing.T) {
	stored := gjson.Parse(`"x"`)
	for _, name := range []string{"IS_CHANGED", "IS_UNCHANGED", "BOGUS_OP"} {
		if _, err := applyOperator(name, stored, "x", nil); err == nil {
			t.Errorf("expected error for unsupported operator %s", name)
		}
	}
}

// TestApplyOperator_JSONNumberScalarEquals proves json.Number operands (from
// XML import) route through the kernel as exact numeric text on the scalar
// EQUALS path against a numeric-declared field.
func TestApplyOperator_JSONNumberScalarEquals(t *testing.T) {
	data := []byte(`{"score":1.5}`)
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.score",
		OperatorType: "EQUALS",
		Value:        json.Number("1.5"),
	}
	types := func(p string) []spi.DataType {
		if p == "$.score" {
			return []spi.DataType{spi.UnboundDecimal}
		}
		return nil
	}
	got, err := Match(cond, data, meta(), types)
	if err != nil || !got {
		t.Errorf("json.Number scalar EQUALS against declared numeric must match; got=%v err=%v", got, err)
	}
}
