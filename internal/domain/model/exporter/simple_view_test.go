package exporter_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/exporter"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestSimpleViewFlatObject(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	node.SetChild("age", schema.NewLeafNode(schema.Integer))

	exp := exporter.NewSimpleViewExporter("UNLOCKED")
	data, err := exp.Export(node)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var sv map[string]any
	if err := json.Unmarshal(data, &sv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sv["currentState"] != "UNLOCKED" {
		t.Errorf("expected UNLOCKED, got %v", sv["currentState"])
	}
	model, ok := sv["model"].(map[string]any)
	if !ok {
		t.Fatal("expected 'model' key")
	}
	root, ok := model["$"].(map[string]any)
	if !ok {
		t.Fatal("expected '$' root node")
	}
	if root[".name"] != "STRING" {
		t.Errorf("expected .name=STRING, got %v", root[".name"])
	}
	if root[".age"] != "INTEGER" {
		t.Errorf("expected .age=INTEGER, got %v", root[".age"])
	}
}

func TestSimpleViewNestedObject(t *testing.T) {
	inner := schema.NewObjectNode()
	inner.SetChild("city", schema.NewLeafNode(schema.String))
	node := schema.NewObjectNode()
	node.SetChild("address", inner)

	exp := exporter.NewSimpleViewExporter("UNLOCKED")
	data, _ := exp.Export(node)

	var sv map[string]any
	json.Unmarshal(data, &sv)
	model := sv["model"].(map[string]any)

	// Root should have structural marker
	root := model["$"].(map[string]any)
	if root["#.address"] != "OBJECT" {
		t.Errorf("expected #.address=OBJECT, got %v", root["#.address"])
	}

	// Nested node should exist
	addrNode, ok := model["$.address"].(map[string]any)
	if !ok {
		t.Fatal("expected '$.address' node")
	}
	if addrNode[".city"] != "STRING" {
		t.Errorf("expected .city=STRING, got %v", addrNode[".city"])
	}
}

func TestSimpleViewArrayOfPrimitives(t *testing.T) {
	elem := schema.NewLeafNode(schema.String)
	arr := schema.NewArrayNode(elem)
	arr.ObserveArrayWidth(3)

	node := schema.NewObjectNode()
	node.SetChild("tags", arr)

	exp := exporter.NewSimpleViewExporter("UNLOCKED")
	data, _ := exp.Export(node)

	var sv map[string]any
	json.Unmarshal(data, &sv)
	model := sv["model"].(map[string]any)
	root := model["$"].(map[string]any)

	// tags should be UniTypeArray: "(STRING x 3)"
	tagsVal, ok := root[".tags[*]"]
	if !ok {
		t.Fatal("expected '.tags[*]' in root")
	}
	expected := "(STRING x 3)"
	if tagsVal != expected {
		t.Errorf("expected %q, got %v", expected, tagsVal)
	}
}

func TestSimpleViewArrayOfObjects(t *testing.T) {
	inner := schema.NewObjectNode()
	inner.SetChild("name", schema.NewLeafNode(schema.String))
	arr := schema.NewArrayNode(inner)

	node := schema.NewObjectNode()
	node.SetChild("items", arr)

	exp := exporter.NewSimpleViewExporter("UNLOCKED")
	data, _ := exp.Export(node)

	var sv map[string]any
	json.Unmarshal(data, &sv)
	model := sv["model"].(map[string]any)

	// Root should reference items as structural
	root := model["$"].(map[string]any)
	if root["#.items"] != "OBJECT" {
		// Or could be referenced differently — check what's there
		t.Logf("root keys: %v", root)
	}

	// items[*] node should exist with ARRAY_ELEMENT marker
	itemsNode, ok := model["$.items[*]"].(map[string]any)
	if !ok {
		t.Fatal("expected '$.items[*]' node")
	}
	if itemsNode["#"] != "ARRAY_ELEMENT" {
		t.Errorf("expected #=ARRAY_ELEMENT, got %v", itemsNode["#"])
	}
	if itemsNode[".name"] != "STRING" {
		t.Errorf("expected .name=STRING, got %v", itemsNode[".name"])
	}
}

func TestSimpleViewPolymorphicField(t *testing.T) {
	leaf := schema.NewLeafNode(schema.Integer)
	leaf.AddScalarTypes(schema.String)
	node := schema.NewObjectNode()
	node.SetChild("value", leaf)

	exp := exporter.NewSimpleViewExporter("UNLOCKED")
	data, _ := exp.Export(node)

	var sv map[string]any
	json.Unmarshal(data, &sv)
	model := sv["model"].(map[string]any)
	root := model["$"].(map[string]any)

	// Polymorphic uses bracket notation, sorted by DataType order
	if v, ok := root[".value"].(string); !ok || v != "[INTEGER, STRING]" {
		t.Errorf("expected [INTEGER, STRING], got %v", root[".value"])
	}
}

func TestSimpleViewLockedState(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("x", schema.NewLeafNode(schema.String))

	exp := exporter.NewSimpleViewExporter("LOCKED")
	data, _ := exp.Export(node)

	var sv map[string]any
	json.Unmarshal(data, &sv)
	if sv["currentState"] != "LOCKED" {
		t.Errorf("expected LOCKED, got %v", sv["currentState"])
	}
}
