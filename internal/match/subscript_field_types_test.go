package match_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// subscriptDoc and subscriptFields are a REALISTIC pairing: the document, and
// the FieldsMap a schema inferred from it would produce. The declared-type
// lookup is what makes the kernel type-directed, and it is keyed on the
// wildcard spelling — "$.arr[*]", never "$.arr[0]".
const subscriptDoc = `{"arr":[10,20,30],"items":[{"name":"first"},{"name":"second"}]}`

func subscriptFields() match.FieldTypes {
	root := schema.NewObjectNode()
	root.SetChild("arr", schema.NewArrayNode(schema.NewLeafNode(schema.Integer)))
	item := schema.NewObjectNode()
	item.SetChild("name", schema.NewLeafNode(schema.String))
	root.SetChild("items", schema.NewArrayNode(item))

	fields := root.FieldsMap()
	// The raw-map lookup every production caller performs — the workflow
	// engine's criterion closure and the search fallback both index the
	// FieldsMap by the key match hands them, with no rewriting of their own.
	return func(p string) []spi.DataType {
		if d, ok := fields[p]; ok {
			return d.Types
		}
		return nil
	}
}

// TestPrepare_SubscriptPathResolvesDeclaredTypes is the end-to-end proof for
// the in-process evaluator: a positional path must both resolve its declared
// type (so the comparison is type-directed rather than expanding into nothing)
// AND address the right gjson node. Missing either one makes the leaf false
// for every entity — an empty page, or a criterion that never fires, for a
// field that holds the value.
func TestPrepare_SubscriptPathResolvesDeclaredTypes(t *testing.T) {
	tests := []struct {
		name     string
		jsonPath string
		operator string
		value    any
		want     bool
	}{
		{"index leaf numeric", "$.arr[0]", "EQUALS", 10, true},
		{"index leaf numeric other position", "$.arr[2]", "EQUALS", 30, true},
		{"index leaf numeric wrong value", "$.arr[0]", "EQUALS", 999, false},
		{"index leaf comparison", "$.arr[1]", "GREATER_THAN", 15, true},
		{"index mid-path string", "$.items[1].name", "EQUALS", "second", true},
		{"wildcard still works", "$.items[*].name", "EQUALS", "second", true},
		{"plain path still works", "$.arr[0]", "NOT_NULL", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := match.Prepare(&predicate.SimpleCondition{
				JsonPath: tc.jsonPath, OperatorType: tc.operator, Value: tc.value,
			}, subscriptFields())
			if err != nil {
				t.Fatalf("Prepare(%q): %v", tc.jsonPath, err)
			}
			if got := p.Match([]byte(subscriptDoc), spi.EntityMeta{}); got != tc.want {
				t.Errorf("%s %s %v on %q = %v, want %v",
					tc.jsonPath, tc.operator, tc.value, subscriptDoc, got, tc.want)
			}
		})
	}
}

// TestPrepareArray_SubscriptBaseResolvesDeclaredTypes is the ArrayCondition
// twin: a container path carrying a subscript must resolve its element type
// from the wildcard key too.
func TestPrepareArray_SubscriptBaseResolvesDeclaredTypes(t *testing.T) {
	p, err := match.Prepare(
		&predicate.ArrayCondition{JsonPath: "$.arr", Values: []any{10, 20}},
		subscriptFields())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !p.Match([]byte(subscriptDoc), spi.EntityMeta{}) {
		t.Error(`ArrayCondition on "$.arr" with values [10,20] did not match`)
	}
}
