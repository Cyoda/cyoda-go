package exporter_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/exporter"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestJSONSchemaFlatObject(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	node.SetChild("age", schema.NewLeafNode(schema.Integer))

	exp := exporter.NewJSONSchemaExporter("UNLOCKED")
	data, err := exp.Export(node)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s["currentState"] != "UNLOCKED" {
		t.Errorf("expected currentState=UNLOCKED, got %v", s["currentState"])
	}
	model, ok := s["model"].(map[string]any)
	if !ok {
		t.Fatal("expected 'model' envelope")
	}
	if model["type"] != "object" {
		t.Errorf("expected type=object, got %v", model["type"])
	}
	props, ok := model["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected 'properties' map")
	}
	nameProp, ok := props["name"].(map[string]any)
	if !ok {
		t.Fatal("expected name property as map")
	}
	if nameProp["type"] != "string" {
		t.Errorf("expected name type=string, got %v", nameProp["type"])
	}
	ageProp, ok := props["age"].(map[string]any)
	if !ok {
		t.Fatal("expected age property as map")
	}
	if ageProp["type"] != "integer" {
		t.Errorf("expected age type=integer, got %v", ageProp["type"])
	}
}

func TestJSONSchemaArray(t *testing.T) {
	elem := schema.NewLeafNode(schema.String)
	arr := schema.NewArrayNode(elem)
	node := schema.NewObjectNode()
	node.SetChild("tags", arr)

	exp := exporter.NewJSONSchemaExporter("UNLOCKED")
	data, err := exp.Export(node)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var s map[string]any
	json.Unmarshal(data, &s)
	model := s["model"].(map[string]any)
	props := model["properties"].(map[string]any)
	tagsProp := props["tags"].(map[string]any)
	if tagsProp["type"] != "array" {
		t.Errorf("expected tags type=array, got %v", tagsProp["type"])
	}
	items, ok := tagsProp["items"].(map[string]any)
	if !ok {
		t.Fatal("expected items spec")
	}
	if items["type"] != "string" {
		t.Errorf("expected items type=string, got %v", items["type"])
	}
}

func TestJSONSchemaPolymorphic(t *testing.T) {
	leaf := schema.NewLeafNode(schema.Integer)
	leaf.Types().Add(schema.String)
	node := schema.NewObjectNode()
	node.SetChild("value", leaf)

	exp := exporter.NewJSONSchemaExporter("UNLOCKED")
	data, err := exp.Export(node)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var s map[string]any
	json.Unmarshal(data, &s)
	model := s["model"].(map[string]any)
	props := model["properties"].(map[string]any)
	valueProp := props["value"].(map[string]any)
	// Polymorphic types should use anyOf — two DataTypes can render the same
	// JSON Schema shape, and oneOf would then reject a value the model admits.
	anyOf, ok := valueProp["anyOf"].([]any)
	if !ok {
		t.Fatal("expected anyOf array for polymorphic field")
	}
	if len(anyOf) != 2 {
		t.Errorf("expected 2 types in anyOf, got %d", len(anyOf))
	}
}

func TestJSONSchemaNestedObject(t *testing.T) {
	inner := schema.NewObjectNode()
	inner.SetChild("city", schema.NewLeafNode(schema.String))
	node := schema.NewObjectNode()
	node.SetChild("address", inner)

	exp := exporter.NewJSONSchemaExporter("UNLOCKED")
	data, err := exp.Export(node)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var s map[string]any
	json.Unmarshal(data, &s)
	model := s["model"].(map[string]any)
	props := model["properties"].(map[string]any)
	addrProp, ok := props["address"].(map[string]any)
	if !ok {
		t.Fatal("expected address property")
	}
	if addrProp["type"] != "object" {
		t.Errorf("expected type=object, got %v", addrProp["type"])
	}
	innerProps, ok := addrProp["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected nested properties")
	}
	cityProp := innerProps["city"].(map[string]any)
	if cityProp["type"] != "string" {
		t.Errorf("expected city type=string, got %v", cityProp["type"])
	}
}
