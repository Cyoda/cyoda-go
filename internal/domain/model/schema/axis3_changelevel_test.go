// internal/domain/model/schema/axis3_changelevel_test.go
package schema_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

type axis3Cell struct {
	Name     string
	Old      *schema.ModelNode
	Incoming any
	Level    spi.ChangeLevel
	Accept   bool
}

func TestAxis3ChangeLevelMatrix(t *testing.T) {
	leaf := func(dt schema.DataType) *schema.ModelNode { return schema.NewLeafNode(dt) }
	obj := func() *schema.ModelNode {
		n := schema.NewObjectNode()
		n.SetChild("k", leaf(schema.Integer))
		return n
	}
	arr := func(dt schema.DataType) *schema.ModelNode { return schema.NewArrayNode(leaf(dt)) }

	// Cells: each row is a (base, incoming, level, accept?) tuple.
	// Linear order: "" < ArrayLength < ArrayElements < Type < Structural.
	cells := []axis3Cell{
		// ArrayLength permits length changes only
		{"empty_level_rejects_new_field", obj(), map[string]any{"k": json.Number("1"), "new": "s"}, "", false},
		{"arrayLength_permits_growing", arr(schema.Integer), []any{json.Number("1"), json.Number("2")}, spi.ChangeLevelArrayLength, true},
		{"arrayLength_rejects_new_element_type", arr(schema.Integer), []any{"s"}, spi.ChangeLevelArrayLength, false},
		{"arrayElements_permits_new_element_type", arr(schema.Integer), []any{"s"}, spi.ChangeLevelArrayElements, true},
		{"type_permits_broaden", leaf(schema.Integer), json.Number("1.5"), spi.ChangeLevelType, true},
		{"type_rejects_structural", obj(), map[string]any{"k": json.Number("1"), "new": "s"}, spi.ChangeLevelType, false},
		{"structural_permits_add_property", obj(), map[string]any{"k": json.Number("1"), "new": "s"}, spi.ChangeLevelStructural, true},
	}

	for _, c := range cells {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			// Capture old-bytes BEFORE Extend to check I7 atomicity on reject.
			oldBytesBefore, err := schema.Marshal(c.Old)
			if err != nil {
				t.Fatalf("Marshal old: %v", err)
			}
			_, extErr := schema.Extend(c.Old, c.Incoming, c.Level)
			oldBytesAfter, _ := schema.Marshal(c.Old)

			// I7: rejection must not mutate input *ModelNode.
			if !c.Accept {
				if extErr == nil {
					t.Fatalf("%s: expected Extend to reject at level %q, succeeded", c.Name, c.Level)
				}
				if string(oldBytesBefore) != string(oldBytesAfter) {
					t.Fatalf("%s: I7 violated — input mutated by rejected Extend\n  before=%s\n  after =%s", c.Name, oldBytesBefore, oldBytesAfter)
				}
			} else {
				if extErr != nil {
					t.Fatalf("%s: expected accept at level %q, got error: %v", c.Name, c.Level, extErr)
				}
			}
		})
	}
}
