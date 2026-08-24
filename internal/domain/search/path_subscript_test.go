package search_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// subscriptSchemaFields is the FieldsMap of a model with an array of integers
// and an array of objects. Array hops are keyed with "[*]" — that is the only
// spelling the schema ever records, because it describes the shape, not the
// data.
func subscriptSchemaFields() map[string]schema.FieldDescriptor {
	root := schema.NewObjectNode()
	root.SetChild("arr", schema.NewArrayNode(schema.NewLeafNode(schema.Integer)))
	item := schema.NewObjectNode()
	item.SetChild("name", schema.NewLeafNode(schema.String))
	root.SetChild("items", schema.NewArrayNode(item))
	return root.FieldsMap()
}

// A positional subscript is valid JSON Path, is accepted by the boundary
// grammar, and is documented as still-served ("$.arr[0]"). Field-existence
// validation compared it against the FieldsMap raw, missed the wildcard key,
// and rejected the condition 400 as naming a field the model does not declare
// — for a field the model declares and the entity holds.
func TestFindUnknownFieldPaths_PositionalSubscriptIsKnown(t *testing.T) {
	fields := subscriptSchemaFields()
	for _, path := range []string{
		"$.arr[0]",
		"$.arr[2]",
		"$.arr[*]",
		"$.items[1].name",
		"$.items[*].name",
	} {
		t.Run(path, func(t *testing.T) {
			cond := &predicate.SimpleCondition{JsonPath: path, OperatorType: "EQUALS", Value: 1}
			if unknown := search.FindUnknownFieldPaths(cond, fields); len(unknown) != 0 {
				t.Errorf("FindUnknownFieldPaths(%q) = %v, want none — the model declares it", path, unknown)
			}
		})
	}
}

// The canonicalisation must not turn the check into a rubber stamp: a path the
// model genuinely does not declare is still unknown, and a subscript on a
// non-array field is still unknown.
func TestFindUnknownFieldPaths_StillRejectsGenuinelyUnknown(t *testing.T) {
	fields := subscriptSchemaFields()
	for _, path := range []string{
		"$.nope[0]",
		"$.arr[0].nested",
		"$.items[0].missing",
		"$.arr[-1]",
		"$.arr[0:2]",
	} {
		t.Run(path, func(t *testing.T) {
			cond := &predicate.SimpleCondition{JsonPath: path, OperatorType: "EQUALS", Value: 1}
			unknown := search.FindUnknownFieldPaths(cond, fields)
			if len(unknown) != 1 || unknown[0] != path {
				t.Errorf("FindUnknownFieldPaths(%q) = %v, want [%q] — the model does not declare it", path, unknown, path)
			}
		})
	}
}
