package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// buildArrayModel returns a ModelNode with an INTEGER array at $.arr and an
// array of objects carrying a STRING $.items[*].name.
func buildArrayModel() *schema.ModelNode {
	root := schema.NewObjectNode()
	root.SetChild("arr", schema.NewArrayNode(schema.NewLeafNode(schema.Integer)))
	item := schema.NewObjectNode()
	item.SetChild("name", schema.NewLeafNode(schema.String))
	root.SetChild("items", schema.NewArrayNode(item))
	return root
}

// A positional subscript addresses the array's element, whose declared type the
// schema records under the wildcard key. Looking the positional spelling up raw
// missed, so the leaf was treated as an unknown path carrying no type
// constraint: an operand that cannot be an INTEGER was accepted and then
// evaluated to an empty page instead of 400 CONDITION_TYPE_MISMATCH. The
// wildcard spelling of the SAME path was rejected correctly, so two spellings
// of one path disagreed.
func TestValidateConditionTypes_PositionalSubscript_TypeMismatch(t *testing.T) {
	model := buildArrayModel()
	for _, path := range []string{"$.arr[*]", "$.arr[0]", "$.arr[7]"} {
		t.Run(path, func(t *testing.T) {
			err := ValidateConditionValueTypes(model, &predicate.SimpleCondition{
				JsonPath: path, OperatorType: "EQUALS", Value: "not-a-number",
			})
			if !errors.Is(err, errConditionTypeMismatch) {
				t.Errorf(`ValidateConditionValueTypes(%q EQUALS "not-a-number") = %v, want errConditionTypeMismatch`, path, err)
			}
		})
	}
}

// The positive control: an operand that DOES parse into the declared type is
// accepted at every spelling.
func TestValidateConditionTypes_PositionalSubscript_Accepted(t *testing.T) {
	model := buildArrayModel()
	cases := []struct {
		path  string
		value any
	}{
		{"$.arr[*]", float64(10)},
		{"$.arr[0]", float64(10)},
		{"$.items[*].name", "x"},
		{"$.items[1].name", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if err := ValidateConditionValueTypes(model, &predicate.SimpleCondition{
				JsonPath: tc.path, OperatorType: "EQUALS", Value: tc.value,
			}); err != nil {
				t.Errorf("ValidateConditionValueTypes(%q EQUALS %v) = %v, want accepted", tc.path, tc.value, err)
			}
		})
	}
}

// A positional subscript on a CONTAINER must still be refused as a container,
// and the diagnostic must name the path the request actually sent rather than
// a canonicalised form the caller never wrote.
func TestValidateConditionTypes_PositionalSubscript_ContainerRejected(t *testing.T) {
	model := buildArrayModel()
	err := ValidateConditionValueTypes(model, &predicate.SimpleCondition{
		JsonPath: "$.items[0]", OperatorType: "EQUALS", Value: "x",
	})
	if !errors.Is(err, errInvalidFieldPath) {
		t.Fatalf(`ValidateConditionValueTypes("$.items[0]" EQUALS "x") = %v, want errInvalidFieldPath`, err)
	}
	if !strings.Contains(err.Error(), "$.items[0]") {
		t.Errorf("error %q does not name the path the request sent", err)
	}
}
