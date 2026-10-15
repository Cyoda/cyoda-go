package search_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
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

// unknownPaths runs the field-path check every condition surface runs —
// ConditionFieldPaths, then ValidateKnownPaths — on cond against fields. The
// store has no refresh capability, so the first answer is final. It returns
// nil when every path is known, and otherwise the paths the
// INVALID_FIELD_PATH refusal names.
func unknownPaths(t *testing.T, cond predicate.Condition, fields map[string]schema.FieldDescriptor) []string {
	t.Helper()
	_, err := search.ValidateKnownPaths(context.Background(), nil,
		spi.ModelRef{EntityName: "m", ModelVersion: "1"}, search.ConditionFieldPaths(cond), fields)
	if err == nil {
		return nil
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Fatalf("ValidateKnownPaths: %v; want nil or INVALID_FIELD_PATH", err)
	}
	_, list, ok := strings.Cut(appErr.Message, "unknown field path(s): ")
	if !ok {
		t.Fatalf("INVALID_FIELD_PATH message names no paths: %q", appErr.Message)
	}
	return strings.Split(list, ", ")
}

// A positional subscript is valid JSON Path, is accepted by the boundary
// grammar, and is documented as still-served ("$.arr[0]"). The field-path
// check canonicalises it to the schema's wildcard key, so a field the model
// declares and the entity holds is known.
func TestKnownPaths_PositionalSubscriptIsKnown(t *testing.T) {
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
			if unknown := unknownPaths(t, cond, fields); len(unknown) != 0 {
				t.Errorf("unknown paths for %q = %v, want none — the model declares it", path, unknown)
			}
		})
	}
}

// TestKnownPaths_ArrayContainerIsKnown pins the ARRAY-CONTAINER form: a path
// naming the array itself, with no subscript.
//
// The schema records an array's element once, under the "[*]" key, and never
// records the container as a leaf — so for `tags: ["red","blue"]` the only
// FieldsMap entry is "$.tags[*]". The existence probe accepts a path as a
// leaf, or as a prefix of some leaf under "p + \".\"" or "p + \"[\"", so
// "$.tags", the natural spelling for an ArrayCondition, is known. The sibling
// probe in condition_type_validate.go (isKnownContainerPath) tests both
// delimiters too. The array-condition case also pins that ConditionFieldPaths
// takes an array clause's path.
func TestKnownPaths_ArrayContainerIsKnown(t *testing.T) {
	fields := subscriptSchemaFields()
	t.Run("array condition on the container", func(t *testing.T) {
		cond := &predicate.ArrayCondition{OperatorType: "EQUALS", JsonPath: "$.arr", Values: []any{1}}
		if got := search.ConditionFieldPaths(cond); len(got) != 1 || got[0] != "$.arr" {
			t.Fatalf("ConditionFieldPaths = %v, want [$.arr]", got)
		}
		if unknown := unknownPaths(t, cond, fields); len(unknown) != 0 {
			t.Errorf("unknown paths for $.arr = %v, want none — the model declares $.arr[*]", unknown)
		}
	})
	t.Run("simple condition on the container", func(t *testing.T) {
		cond := &predicate.SimpleCondition{JsonPath: "$.items", OperatorType: "NOT_NULL"}
		if unknown := unknownPaths(t, cond, fields); len(unknown) != 0 {
			t.Errorf("unknown paths for $.items = %v, want none — the model declares $.items[*].name", unknown)
		}
	})
}

// The canonicalisation must not turn the check into a rubber stamp: a path the
// model genuinely does not declare is still unknown, and a subscript on a
// non-array field is still unknown.
func TestKnownPaths_StillRejectsGenuinelyUnknown(t *testing.T) {
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
			unknown := unknownPaths(t, cond, fields)
			if len(unknown) != 1 || unknown[0] != path {
				t.Errorf("unknown paths for %q = %v, want [%q] — the model does not declare it", path, unknown, path)
			}
		})
	}
}
