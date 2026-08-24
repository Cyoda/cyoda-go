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

// TestFindUnknownFieldPaths_ArrayContainerIsKnown pins the ARRAY-CONTAINER
// form: a path naming the array itself, with no subscript.
//
// The schema records an array's element once, under the "[*]" key, and never
// records the container as a leaf — so for `tags: ["red","blue"]` the only
// FieldsMap entry is "$.tags[*]". The existence probe accepted a path either
// as a leaf or as a prefix of some leaf under "p + \".\"", but never under
// "p + \"[\"" — so "$.tags", the natural spelling for an ArrayCondition, was
// reported unknown and answered 400 INVALID_FIELD_PATH for a field the model
// declares. The sibling probe in condition_type_validate.go
// (isKnownContainerPath) already tested both delimiters.
func TestFindUnknownFieldPaths_ArrayContainerIsKnown(t *testing.T) {
	fields := subscriptSchemaFields()
	t.Run("array condition on the container", func(t *testing.T) {
		cond := &predicate.ArrayCondition{JsonPath: "$.arr", Values: []any{1}}
		if unknown := search.FindUnknownFieldPaths(cond, fields); len(unknown) != 0 {
			t.Errorf("FindUnknownFieldPaths($.arr) = %v, want none — the model declares $.arr[*]", unknown)
		}
	})
	t.Run("simple condition on the container", func(t *testing.T) {
		cond := &predicate.SimpleCondition{JsonPath: "$.items", OperatorType: "NOT_NULL"}
		if unknown := search.FindUnknownFieldPaths(cond, fields); len(unknown) != 0 {
			t.Errorf("FindUnknownFieldPaths($.items) = %v, want none — the model declares $.items[*].name", unknown)
		}
	})
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
		// Container spellings for containers that do not exist. The
		// "p + \"[\"" probe must widen the check by exactly one prefix
		// form, not turn it into a rubber stamp.
		"$.nope",
		"$.ar",
		"$.items[0].nam",
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
