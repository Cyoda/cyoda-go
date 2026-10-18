package importer_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
)

func TestJSONParserFlat(t *testing.T) {
	input := `{"name": "Alice", "age": 30}`
	parsed, err := importer.ParseJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	if m["name"] != "Alice" {
		t.Errorf("expected Alice, got %v", m["name"])
	}
}

func TestJSONParserPreservesNumbers(t *testing.T) {
	input := `{"count": 42, "rate": 3.14}`
	parsed, err := importer.ParseJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	count, ok := m["count"].(json.Number)
	if !ok {
		t.Fatalf("expected json.Number for count, got %T", m["count"])
	}
	if count.String() != "42" {
		t.Errorf("expected 42, got %v", count)
	}
}

func TestJSONParserBigInteger(t *testing.T) {
	input := `{"big": 9007199254740993}`
	parsed, err := importer.ParseJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	big, ok := m["big"].(json.Number)
	if !ok {
		t.Fatalf("expected json.Number for big, got %T", m["big"])
	}
	// json.Number preserves the exact string representation; float64 would
	// mangle this to 9007199254740992.
	if big.String() != "9007199254740993" {
		t.Errorf("expected 9007199254740993, got %v", big)
	}
}

func TestJSONParserBigIntegerExceedsInt64(t *testing.T) {
	input := `{"huge": 99999999999999999999}`
	parsed, err := importer.ParseJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	huge, ok := m["huge"].(json.Number)
	if !ok {
		t.Fatalf("expected json.Number for huge, got %T", m["huge"])
	}
	if huge.String() != "99999999999999999999" {
		t.Errorf("expected 99999999999999999999, got %v", huge)
	}
}

func TestXMLParserSimple(t *testing.T) {
	input := `<root><name>Alice</name><age>30</age></root>`
	parsed, err := importer.ParseXML(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	if m["name"] != "Alice" {
		t.Errorf("expected Alice, got %v", m["name"])
	}
}

func TestXMLParserNested(t *testing.T) {
	input := `<root><address><city>Berlin</city></address></root>`
	parsed, err := importer.ParseXML(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	addr, ok := m["address"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested map for address, got %T", m["address"])
	}
	if addr["city"] != "Berlin" {
		t.Errorf("expected Berlin, got %v", addr["city"])
	}
}

func TestXMLParserRepeatedElements(t *testing.T) {
	input := `<root><item>a</item><item>b</item></root>`
	parsed, err := importer.ParseXML(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	items, ok := m["item"].([]any)
	if !ok {
		t.Fatalf("expected []any for repeated elements, got %T", m["item"])
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

func TestXMLParserAttributes(t *testing.T) {
	input := `<root><item id="5" active="true">text</item></root>`
	parsed, err := importer.ParseXML(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	item, ok := m["item"].(map[string]any)
	if !ok {
		t.Fatalf("expected map for item, got %T", m["item"])
	}
	id, ok := item["id"].(json.Number)
	if !ok {
		t.Fatalf("expected json.Number for id, got %T", item["id"])
	}
	if id.String() != "5" {
		t.Errorf("expected id=5, got %v", id)
	}
	active, ok := item["active"].(bool)
	if !ok {
		t.Fatalf("expected bool for active, got %T", item["active"])
	}
	if !active {
		t.Errorf("expected active=true, got %v", active)
	}
	text, ok := item["_text"].(string)
	if !ok {
		t.Fatalf("expected string for _text, got %T", item["_text"])
	}
	if text != "text" {
		t.Errorf("expected _text=text, got %v", text)
	}
}

func TestJSONParserError(t *testing.T) {
	input := `{invalid}`
	_, err := importer.ParseJSON(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// TestJSONParser_OneReading: sample data has one reading. A member written
// twice in one object, and anything after the value, are refused — the
// gRPC import refuses them in the event's payload, so the HTTP import must
// too. Before, the last duplicate won and trailing text was ignored.
func TestJSONParser_OneReading(t *testing.T) {
	for _, input := range []string{
		`{"name":"Alice","name":2}`,
		`{"items":[{"a":1,"a":"x"}]}`,
		`{"name":"Alice"} {"name":"Bob"}`,
		`{"name":"Alice"}}`,
	} {
		if parsed, err := importer.ParseJSON(strings.NewReader(input)); err == nil {
			t.Errorf("%s: accepted as %v", input, parsed)
		}
	}
}

func TestXMLParserError(t *testing.T) {
	input := `<unclosed>`
	_, err := importer.ParseXML(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for malformed XML, got nil")
	}
}

func TestXMLParserEmpty(t *testing.T) {
	input := ""
	_, err := importer.ParseXML(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for empty XML document, got nil")
	}
}
