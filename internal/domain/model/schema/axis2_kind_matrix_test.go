// internal/domain/model/schema/axis2_kind_matrix_test.go
package schema_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// axis2Cell describes one cell of the (existingKind, incomingKind) matrix.
// Action: "roundtrip" asserts I1; "extendContract" asserts Extend is a no-op
// (silent-drop); "skip" parks a cell with a reason.
type axis2Cell struct {
	Name    string
	Old     *schema.ModelNode
	Value   any
	Level   spi.ChangeLevel
	Action  string // roundtrip | extendContract | skip
	SkipMsg string
}

func TestAxis2KindMatrix(t *testing.T) {
	leaf := func(dt schema.DataType) *schema.ModelNode { return schema.NewLeafNode(dt) }
	obj := func() *schema.ModelNode {
		n := schema.NewObjectNode()
		n.SetChild("k", leaf(schema.Integer))
		return n
	}
	arr := func() *schema.ModelNode { return schema.NewArrayNode(leaf(schema.Integer)) }

	cells := []axis2Cell{
		// Same-kind: round-trip properly.
		{"LL_same_type", leaf(schema.Integer), json.Number("1"), spi.ChangeLevelStructural, "roundtrip", ""},
		{"LL_broaden", leaf(schema.Integer), json.Number("1.5"), spi.ChangeLevelType, "roundtrip", ""},
		{"OO_add_field", obj(), map[string]any{"k": json.Number("1"), "new": "s"}, spi.ChangeLevelStructural, "roundtrip", ""},
		{"AA_same_element", arr(), []any{json.Number("1")}, spi.ChangeLevelStructural, "roundtrip", ""},

		// Kind-union cells: at STRUCTURAL the write adds the kind, and the
		// delta must replay to exactly the model Extend produced.
		{"LO_leaf_to_object", leaf(schema.Integer), map[string]any{"x": json.Number("1")}, spi.ChangeLevelStructural, "roundtrip", ""},
		{"LA_leaf_to_array", leaf(schema.Integer), []any{json.Number("1")}, spi.ChangeLevelStructural, "roundtrip", ""},
		{"OL_object_to_leaf", obj(), json.Number("1"), spi.ChangeLevelStructural, "roundtrip", ""},
		{"OA_object_to_array", obj(), []any{json.Number("1")}, spi.ChangeLevelStructural, "roundtrip", ""},
		{"AL_array_to_leaf", arr(), json.Number("1"), spi.ChangeLevelStructural, "roundtrip", ""},
		{"AO_array_to_object", arr(), map[string]any{"k": json.Number("1")}, spi.ChangeLevelStructural, "roundtrip", ""},

		// Silent-drop Extend-contract cells: verify Extend returns old unchanged
		// when confronted with incompatible kinds at restricted levels.
		{"LO_restricted_levelType_no_op", leaf(schema.Integer), map[string]any{"k": json.Number("1")}, spi.ChangeLevelType, "extendContract", ""},
	}

	for _, c := range cells {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if c.Action == "skip" {
				t.Skip(c.SkipMsg)
			}
			incomingNode, err := importer.Walk(c.Value)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			switch c.Action {
			case "roundtrip":
				extended, err := schema.Extend(c.Old, incomingNode, c.Level)
				if err != nil {
					t.Fatalf("Extend: %v", err)
				}
				assertRoundTrip(t, c.Old, extended, c.Name)
			case "extendContract":
				extended, err := schema.Extend(c.Old, incomingNode, c.Level)
				if err != nil {
					// Reject is an acceptable Extend-contract outcome;
					// the contract is "no partial mutation".
					return
				}
				oldBytes, _ := schema.Marshal(c.Old)
				extBytes, _ := schema.Marshal(extended)
				if string(oldBytes) != string(extBytes) {
					t.Fatalf("%s: Extend silently mutated without error\n  old=%s\n  extended=%s", c.Name, oldBytes, extBytes)
				}
			default:
				t.Fatalf("unknown Action %q", c.Action)
			}
		})
	}
}
