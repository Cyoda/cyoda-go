package search

import (
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// Each operand-shape refusal reaches the client as a 400 INVALID_CONDITION
// whose detail is the error text, so the text is part of the contract. This
// pins every message exactly, on both entry points (search and workflow
// criteria), for a simple and a lifecycle leaf.
func TestValidateCondition_OperandRefusalMessages(t *testing.T) {
	cases := []struct {
		name    string
		op      string
		value   string // a JSON value, or "" for a missing value
		wantMsg string
	}{
		{"missing", "EQUALS", "",
			"invalid condition: operator \"EQUALS\" takes an operand, and `value` is missing or null; only IS_NULL and NOT_NULL take none"},
		{"null", "CONTAINS", "null",
			"invalid condition: operator \"CONTAINS\" takes an operand, and `value` is missing or null; only IS_NULL and NOT_NULL take none"},
		{"null under BETWEEN", "BETWEEN", "null",
			"invalid condition: operator \"BETWEEN\" takes an operand, and `value` is missing or null; only IS_NULL and NOT_NULL take none"},
		{"object", "EQUALS", `{"a":1}`,
			"condition value is an object, which is not a valid operand: invalid condition"},
		{"object under IS_NULL", "IS_NULL", `{}`,
			"condition value is an object, which is not a valid operand: invalid condition"},
		{"object under BETWEEN", "BETWEEN", `{}`,
			"condition value is an object, which is not a valid operand: invalid condition"},
		{"array", "EQUALS", `["a"]`,
			"invalid condition: operator \"EQUALS\" takes a single value, not an array; only BETWEEN and BETWEEN_INCLUSIVE take a [low, high] pair"},
		{"array under NOT_NULL", "NOT_NULL", `["a","b"]`,
			"invalid condition: operator \"NOT_NULL\" takes a single value, not an array; only BETWEEN and BETWEEN_INCLUSIVE take a [low, high] pair"},
		{"range string", "BETWEEN", `"a"`,
			"invalid condition: operator \"BETWEEN\" requires exactly two operands, got a string"},
		{"range number", "BETWEEN", `5`,
			"invalid condition: operator \"BETWEEN\" requires exactly two operands, got a number"},
		{"range boolean", "BETWEEN_INCLUSIVE", `true`,
			"invalid condition: operator \"BETWEEN_INCLUSIVE\" requires exactly two operands, got a boolean"},
		{"range one bound", "BETWEEN_INCLUSIVE", `["a"]`,
			"invalid condition: operator \"BETWEEN_INCLUSIVE\" requires exactly two operands, got 1"},
		{"range three bounds", "BETWEEN", `["a","b","c"]`,
			"invalid condition: operator \"BETWEEN\" requires exactly two operands, got 3"},
		{"range empty", "BETWEEN", `[]`,
			"invalid condition: operator \"BETWEEN\" requires exactly two operands, got 0"},
		{"range null bound", "BETWEEN", `["a",null]`,
			"invalid condition: operator \"BETWEEN\" bounds must not be null"},
		{"range object bound", "BETWEEN_INCLUSIVE", `[{},"b"]`,
			"invalid condition: operator \"BETWEEN_INCLUSIVE\" bounds must be scalars, not objects or arrays"},
		{"range array bound", "BETWEEN", `["a",["b"]]`,
			"invalid condition: operator \"BETWEEN\" bounds must be scalars, not objects or arrays"},
	}
	for _, tc := range cases {
		valueMember := ""
		if tc.value != "" {
			valueMember = `,"value":` + tc.value
		}
		for _, body := range []string{
			`{"type":"simple","jsonPath":"$.n","operatorType":"` + tc.op + `"` + valueMember + `}`,
			`{"type":"lifecycle","field":"state","operatorType":"` + tc.op + `"` + valueMember + `}`,
		} {
			c, err := predicate.ParseCondition([]byte(body))
			if err != nil {
				t.Fatalf("%s: parse %s: %v", tc.name, body, err)
			}
			assertOperandRefusal(t, tc.name+": "+body, c, tc.wantMsg)
		}
	}
}

// A Go caller can build a leaf whose operand JSON decoding never produces: a
// struct, a pointer, a named type. Such an operand is refused as a client
// error too, never compared as its printed form.
func TestValidateCondition_NonScalarOperandRefused(t *testing.T) {
	type label string
	for name, v := range map[string]any{
		"struct":     time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		"pointer":    new(string),
		"named type": label("a"),
	} {
		c := &predicate.SimpleCondition{JsonPath: "$.n", OperatorType: "EQUALS", Value: v}
		assertOperandRefusal(t, name, c, "invalid condition: condition value is not a scalar")
	}
}

func assertOperandRefusal(t *testing.T, name string, c predicate.Condition, wantMsg string) {
	t.Helper()
	for entry, err := range map[string]error{
		"ValidateCondition":          ValidateCondition(c),
		"ValidateCriterionCondition": ValidateCriterionCondition(c),
	} {
		if !errors.Is(err, ErrInvalidCondition) {
			t.Errorf("%s: %s: want ErrInvalidCondition, got %v", name, entry, err)
			continue
		}
		if err.Error() != wantMsg {
			t.Errorf("%s: %s:\n got %q\nwant %q", name, entry, err.Error(), wantMsg)
		}
	}
}
