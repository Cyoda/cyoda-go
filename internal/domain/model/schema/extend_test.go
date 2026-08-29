package schema_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestExtendStructuralNewField(t *testing.T) {
	// STRUCTURAL allows new fields
	existing := schema.NewObjectNode()
	existing.SetChild("name", schema.NewLeafNode(schema.String))
	incoming := schema.NewObjectNode()
	incoming.SetChild("name", schema.NewLeafNode(schema.String))
	incoming.SetChild("age", schema.NewLeafNode(schema.Integer))
	result, err := schema.Extend(existing, incoming, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object().Child("age") == nil {
		t.Error("expected 'age' field")
	}
}

func TestExtendTypeRejectsNewField(t *testing.T) {
	// TYPE does not allow new fields
	existing := schema.NewObjectNode()
	existing.SetChild("name", schema.NewLeafNode(schema.String))
	incoming := schema.NewObjectNode()
	incoming.SetChild("name", schema.NewLeafNode(schema.String))
	incoming.SetChild("age", schema.NewLeafNode(schema.Integer))
	_, err := schema.Extend(existing, incoming, spi.ChangeLevelType)
	if err == nil {
		t.Error("expected error: TYPE should reject new fields")
	}
}

func TestExtendTypeAllowsTypeWidening(t *testing.T) {
	existing := schema.NewObjectNode()
	existing.SetChild("value", schema.NewLeafNode(schema.Integer))
	incoming := schema.NewObjectNode()
	incoming.SetChild("value", schema.NewLeafNode(schema.String))
	result, err := schema.Extend(existing, incoming, spi.ChangeLevelType)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	types := result.Object().Child("value").DeclaredTypes()
	if len(types) != 2 {
		t.Errorf("expected polymorphic, got %v", types)
	}
}

func TestExtendArrayElementsAllowsElementWidening(t *testing.T) {
	existingArr := schema.NewArrayNode(schema.NewLeafNode(schema.Integer))
	existing := schema.NewObjectNode()
	existing.SetChild("scores", existingArr)
	incomingArr := schema.NewArrayNode(schema.NewLeafNode(schema.String))
	incoming := schema.NewObjectNode()
	incoming.SetChild("scores", incomingArr)
	result, err := schema.Extend(existing, incoming, spi.ChangeLevelArrayElements)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elemTypes := result.Object().Child("scores").Array().Element().DeclaredTypes()
	if len(elemTypes) != 2 {
		t.Errorf("expected widened, got %v", elemTypes)
	}
}

func TestExtendArrayElementsRejectsLeafTypeWidening(t *testing.T) {
	existing := schema.NewObjectNode()
	existing.SetChild("value", schema.NewLeafNode(schema.Integer))
	incoming := schema.NewObjectNode()
	incoming.SetChild("value", schema.NewLeafNode(schema.String))
	_, err := schema.Extend(existing, incoming, spi.ChangeLevelArrayElements)
	if err == nil {
		t.Error("expected error")
	}
}

func TestExtendArrayLengthAllowsWidthChange(t *testing.T) {
	existingArr := schema.NewArrayNode(schema.NewLeafNode(schema.String))
	existingArr.ObserveArrayWidth(3)
	existing := schema.NewObjectNode()
	existing.SetChild("tags", existingArr)
	incomingArr := schema.NewArrayNode(schema.NewLeafNode(schema.String))
	incomingArr.ObserveArrayWidth(5)
	incoming := schema.NewObjectNode()
	incoming.SetChild("tags", incomingArr)
	result, err := schema.Extend(existing, incoming, spi.ChangeLevelArrayLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object().Child("tags").Array().MaxWidth() != 5 {
		t.Error("expected width 5")
	}
}

func TestExtendArrayLengthRejectsElementTypeChange(t *testing.T) {
	existingArr := schema.NewArrayNode(schema.NewLeafNode(schema.Integer))
	existing := schema.NewObjectNode()
	existing.SetChild("scores", existingArr)
	incomingArr := schema.NewArrayNode(schema.NewLeafNode(schema.String))
	incoming := schema.NewObjectNode()
	incoming.SetChild("scores", incomingArr)
	_, err := schema.Extend(existing, incoming, spi.ChangeLevelArrayLength)
	if err == nil {
		t.Error("expected error")
	}
}

func TestExtendEmptyLevelRejectsAll(t *testing.T) {
	existing := schema.NewObjectNode()
	existing.SetChild("name", schema.NewLeafNode(schema.String))
	incoming := schema.NewObjectNode()
	incoming.SetChild("name", schema.NewLeafNode(schema.String))
	incoming.SetChild("extra", schema.NewLeafNode(schema.Integer))
	_, err := schema.Extend(existing, incoming, "")
	if err == nil {
		t.Error("expected error: empty level rejects all changes")
	}
}

func TestExtendConformingDataNoChange(t *testing.T) {
	existing := schema.NewObjectNode()
	existing.SetChild("name", schema.NewLeafNode(schema.String))
	incoming := schema.NewObjectNode()
	incoming.SetChild("name", schema.NewLeafNode(schema.String))
	result, err := schema.Extend(existing, incoming, spi.ChangeLevelArrayLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object().Child("name") == nil {
		t.Error("expected 'name' preserved")
	}
}
