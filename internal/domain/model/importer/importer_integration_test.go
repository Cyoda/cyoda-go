package importer_test

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestSampleDataImporterJSON(t *testing.T) {
	imp := importer.NewSampleDataImporter()
	node, err := imp.Import(strings.NewReader(`{"name": "Alice", "age": 30}`), "JSON")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Object().Child("name") == nil {
		t.Error("expected 'name' field")
	}
	if node.Object().Child("age") == nil {
		t.Error("expected 'age' field")
	}
}

func TestSampleDataImporterXML(t *testing.T) {
	imp := importer.NewSampleDataImporter()
	node, err := imp.Import(strings.NewReader(`<root><name>Alice</name></root>`), "XML")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Object().Child("name") == nil {
		t.Error("expected 'name' field")
	}
}

func TestSampleDataImporterUnsupportedFormat(t *testing.T) {
	imp := importer.NewSampleDataImporter()
	_, err := imp.Import(strings.NewReader(`data`), "YAML")
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestSampleDataImporterSuccessiveMerge(t *testing.T) {
	imp := importer.NewSampleDataImporter()
	first, _ := imp.Import(strings.NewReader(`{"name": "Alice"}`), "JSON")
	second, _ := imp.Import(strings.NewReader(`{"age": 30}`), "JSON")
	merged := schema.Merge(first, second)
	if merged.Object().Child("name") == nil {
		t.Error("expected 'name' from first import")
	}
	if merged.Object().Child("age") == nil {
		t.Error("expected 'age' from second import")
	}
}
