package schema_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// One definition. The engine's field walk and a self-executing backend's field
// walk are the same code now, so they cannot drift apart — and they had, on
// every field observed as more than one kind.
func TestModelNodeIsTheSPIType(t *testing.T) {
	var n *schema.ModelNode = spi.NewObjectNode()
	n.SetChild("k", schema.NewLeafNode(schema.String))

	// object ∪ array, object ∪ scalar, array ∪ scalar — the three unions Merge
	// produces, and the three the single label could not name.
	n.SetChild("f", schema.Merge(schema.NewObjectNode(), schema.NewArrayNode(schema.NewLeafNode(schema.Integer))))
	obj := schema.NewObjectNode()
	obj.SetChild("in", schema.NewLeafNode(schema.Integer))
	n.SetChild("g", schema.Merge(obj, schema.NewLeafNode(schema.String)))
	n.SetChild("h", schema.Merge(schema.NewArrayNode(schema.NewLeafNode(schema.String)), schema.NewLeafNode(schema.String)))

	raw, err := schema.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	viaEngine := n.FieldsMap()
	viaSPI, err := spi.FieldsMapFromSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaEngine) != len(viaSPI) {
		t.Fatalf("the two field walks disagree on %s\n  engine=%v\n  spi   =%v", raw, viaEngine, viaSPI)
	}
	for p := range viaEngine {
		if _, ok := viaSPI[p]; !ok {
			t.Errorf("path %q missing from the SPI walk", p)
		}
	}
}
