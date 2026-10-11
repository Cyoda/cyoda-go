package search

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// textOperators is every string and pattern operator: the sixteen the kernel
// evaluates as a text test (spi.ExpandLeaf's kindStringOp).
var textOperators = []string{
	"CONTAINS", "NOT_CONTAINS", "STARTS_WITH", "NOT_STARTS_WITH",
	"ENDS_WITH", "NOT_ENDS_WITH", "LIKE", "MATCHES_PATTERN",
	"IEQUALS", "INOT_EQUAL", "ICONTAINS", "INOT_CONTAINS",
	"ISTARTS_WITH", "INOT_STARTS_WITH", "IENDS_WITH", "INOT_ENDS_WITH",
}

// leafModel returns a model with one leaf at $.f carrying the given types.
func leafModel(types ...schema.DataType) *schema.ModelNode {
	leaf := schema.NewLeafNode(types[0])
	leaf.AddScalarTypes(types[1:]...)
	node := schema.NewObjectNode()
	node.SetChild("f", leaf)
	return node
}

// TestValidateConditionTypes_TextOperatorOnNonTextField_Rejects: a text
// operator on a field with no text type is refused rather than answered with an
// empty (or, for a negated operator, a full) result. Text means STRING or
// CHARACTER: a temporal or identifier value is stored as a JSON string too, but
// it compares by its own type, never as text.
func TestValidateConditionTypes_TextOperatorOnNonTextField_Rejects(t *testing.T) {
	for _, typ := range []schema.DataType{
		schema.Double, schema.Integer, schema.Boolean,
		schema.LocalDate, schema.ZonedDateTime, schema.Year,
		schema.UUIDType, schema.TimeUUIDType, schema.ByteArray,
	} {
		for _, op := range textOperators {
			t.Run(typ.String()+"/"+op, func(t *testing.T) {
				cond := &predicate.SimpleCondition{JsonPath: "$.f", OperatorType: op, Value: "1"}
				err := ValidateConditionValueTypes(leafModel(typ), cond)
				if !errors.Is(err, errConditionTypeMismatch) {
					t.Fatalf("want errConditionTypeMismatch, got %v", err)
				}
			})
		}
	}
}

// TestValidateConditionTypes_TextOperatorOnTextField_Accepts: a field with a
// text type takes every text operator, also when it carries other types too.
func TestValidateConditionTypes_TextOperatorOnTextField_Accepts(t *testing.T) {
	for name, types := range map[string][]schema.DataType{
		"STRING":         {schema.String},
		"CHARACTER":      {schema.Character},
		"INTEGER+STRING": {schema.Integer, schema.String},
	} {
		for _, op := range textOperators {
			t.Run(name+"/"+op, func(t *testing.T) {
				cond := &predicate.SimpleCondition{JsonPath: "$.f", OperatorType: op, Value: "1"}
				if err := ValidateConditionValueTypes(leafModel(types...), cond); err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
			})
		}
	}
}

// TestValidateConditionTypes_OrderingOperatorOnUnorderedField_Rejects: an
// ordering or range operator needs an ordered type. A boolean, an identifier
// or a byte array has no order the kernel compares by, so GREATER_THAN "false"
// on a BOOLEAN field would answer no entity at all.
func TestValidateConditionTypes_OrderingOperatorOnUnorderedField_Rejects(t *testing.T) {
	operand := map[schema.DataType]any{
		schema.Boolean:      "false",
		schema.UUIDType:     "6f1c5a5e-8f43-4b5e-9a39-3f2d2b8f2a10",
		schema.TimeUUIDType: "e274f6a8-c50a-11f1-a81b-ae468cd3ed16",
		schema.ByteArray:    "AAEC",
	}
	for typ, v := range operand {
		for _, tc := range []struct {
			op    string
			value any
		}{
			{"GREATER_THAN", v}, {"GREATER_OR_EQUAL", v}, {"LESS_THAN", v}, {"LESS_OR_EQUAL", v},
			{"BETWEEN", []any{v, v}}, {"BETWEEN_INCLUSIVE", []any{v, v}},
		} {
			t.Run(typ.String()+"/"+tc.op, func(t *testing.T) {
				cond := &predicate.SimpleCondition{JsonPath: "$.f", OperatorType: tc.op, Value: tc.value}
				err := ValidateConditionValueTypes(leafModel(typ), cond)
				if !errors.Is(err, errConditionTypeMismatch) {
					t.Fatalf("want errConditionTypeMismatch, got %v", err)
				}
			})
		}
	}
}

// TestValidateConditionTypes_OrderingOperatorOnOrderedField_Accepts: numbers,
// text and temporal values are ordered; a field with one of them among other
// types takes the operator too.
func TestValidateConditionTypes_OrderingOperatorOnOrderedField_Accepts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		types []schema.DataType
		value any
	}{
		{"DOUBLE", []schema.DataType{schema.Double}, "1"},
		{"STRING", []schema.DataType{schema.String}, "m"},
		{"CHARACTER", []schema.DataType{schema.Character}, "m"},
		{"LOCAL_DATE", []schema.DataType{schema.LocalDate}, "2024-01-01"},
		{"BOOLEAN+STRING", []schema.DataType{schema.Boolean, schema.String}, "m"},
	} {
		for _, op := range []string{"GREATER_THAN", "LESS_OR_EQUAL"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				cond := &predicate.SimpleCondition{JsonPath: "$.f", OperatorType: op, Value: tc.value}
				if err := ValidateConditionValueTypes(leafModel(tc.types...), cond); err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
			})
		}
		t.Run(tc.name+"/BETWEEN", func(t *testing.T) {
			cond := &predicate.SimpleCondition{JsonPath: "$.f", OperatorType: "BETWEEN", Value: []any{tc.value, tc.value}}
			if err := ValidateConditionValueTypes(leafModel(tc.types...), cond); err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
	}
}

// TestValidateConditionTypes_EqualityOnUnorderedField_Accepts: equality needs
// no order — EQUALS and NOT_EQUAL still apply to a boolean or an identifier.
func TestValidateConditionTypes_EqualityOnUnorderedField_Accepts(t *testing.T) {
	for _, op := range []string{"EQUALS", "NOT_EQUAL"} {
		cond := &predicate.SimpleCondition{JsonPath: "$.f", OperatorType: op, Value: "true"}
		if err := ValidateConditionValueTypes(leafModel(schema.Boolean), cond); err != nil {
			t.Fatalf("%s on BOOLEAN: want accepted, got %v", op, err)
		}
	}
}
