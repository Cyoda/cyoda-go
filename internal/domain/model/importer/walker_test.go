package importer_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestWalkFlatObject(t *testing.T) {
	data := map[string]any{
		"name": "Alice",
		"age":  json.Number("30"),
	}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Object() == nil {
		t.Fatalf("expected the object branch, got %v", node.Kinds())
	}
	nameChild := node.Object().Child("name")
	if nameChild == nil {
		t.Fatal("expected 'name' child")
	}
	types := nameChild.DeclaredTypes()
	if len(types) != 1 || types[0] != schema.String {
		t.Errorf("expected [STRING], got %v", types)
	}
}

func TestWalkNestedObject(t *testing.T) {
	data := map[string]any{
		"address": map[string]any{
			"city": "Berlin",
			"zip":  "10115",
		},
	}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	addr := node.Object().Child("address")
	if addr == nil {
		t.Fatal("expected 'address' child")
	}
	if addr.Object() == nil {
		t.Errorf("expected the object branch, got %v", addr.Kinds())
	}
	if addr.Object().Child("city") == nil {
		t.Error("expected 'city' under address")
	}
}

func TestWalkArray(t *testing.T) {
	data := map[string]any{
		"tags": []any{"a", "b", "c"},
	}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tags := node.Object().Child("tags")
	if tags == nil {
		t.Fatal("expected 'tags' child")
	}
	if tags.Array() == nil {
		t.Errorf("expected the array branch, got %v", tags.Kinds())
	}
	if tags.Array().Element() == nil {
		t.Fatal("expected element descriptor")
	}
	elemTypes := tags.Array().Element().DeclaredTypes()
	if len(elemTypes) != 1 || elemTypes[0] != schema.String {
		t.Errorf("expected [STRING] elements, got %v", elemTypes)
	}
}

func TestWalkArrayOfObjects(t *testing.T) {
	data := map[string]any{
		"items": []any{
			map[string]any{"name": "x"},
			map[string]any{"name": "y", "price": json.Number("10")},
		},
	}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	items := node.Object().Child("items")
	if items == nil || items.Array() == nil {
		t.Fatal("expected 'items' as ARRAY")
	}
	elem := items.Array().Element()
	if elem == nil || elem.Object() == nil {
		t.Fatal("expected element to be OBJECT")
	}
	if elem.Object().Child("name") == nil {
		t.Error("expected 'name' in array element")
	}
	if elem.Object().Child("price") == nil {
		t.Error("expected 'price' in array element (merged from second item)")
	}
}

func TestWalkBoolean(t *testing.T) {
	data := map[string]any{"active": true}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	active := node.Object().Child("active")
	types := active.DeclaredTypes()
	if len(types) != 1 || types[0] != schema.Boolean {
		t.Errorf("expected [BOOLEAN], got %v", types)
	}
}

func TestWalkNumericInference(t *testing.T) {
	// Task 15 value-based classifier: whole-number magnitudes bucket by size
	// across Integer/Long/BigInteger/UnboundInteger; fractional decimals go
	// through ClassifyDecimal.
	tests := []struct {
		name     string
		literal  string
		expected schema.DataType
	}{
		{"127 → Integer", "127", schema.Integer},
		{"128 → Integer", "128", schema.Integer},
		{"32767 → Integer", "32767", schema.Integer},
		{"32768 → Integer", "32768", schema.Integer},
		{"2147483647 → Integer", "2147483647", schema.Integer},
		{"2147483648 → Long", "2147483648", schema.Long},
		{"-129 → Integer", "-129", schema.Integer},
		{"1.5 → Double", "1.5", schema.Double},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{"v": json.Number(tc.literal)}
			node, err := importer.Walk(data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			types := node.Object().Child("v").DeclaredTypes()
			if len(types) != 1 || types[0] != tc.expected {
				t.Errorf("expected [%v], got %v", tc.expected, types)
			}
		})
	}
}

func TestWalkEmptyArray(t *testing.T) {
	data := map[string]any{"items": []any{}}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	items := node.Object().Child("items")
	if items == nil || items.Array() == nil {
		t.Fatal("expected 'items' as ARRAY")
	}
	elem := items.Array().Element()
	if elem == nil {
		t.Fatal("expected element descriptor")
	}
	elemTypes := elem.DeclaredTypes()
	if len(elemTypes) != 1 || elemTypes[0] != schema.Null {
		t.Errorf("expected [NULL] element type, got %v", elemTypes)
	}
}

func TestWalkJsonNumber(t *testing.T) {
	// With default scope (intScope=INTEGER), 42 is clamped to INTEGER.
	data := map[string]any{"v": json.Number("42")}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	types := node.Object().Child("v").DeclaredTypes()
	if len(types) != 1 || types[0] != schema.Integer {
		t.Errorf("expected [INTEGER], got %v", types)
	}
}

func TestWalkJsonNumberLarge(t *testing.T) {
	// 2^53+1 exceeds int32 range → Long per ClassifyInteger.
	data := map[string]any{"v": json.Number("9007199254740993")}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	types := node.Object().Child("v").DeclaredTypes()
	if len(types) != 1 || types[0] != schema.Long {
		t.Errorf("expected [LONG], got %v", types)
	}
}

func TestWalkJsonNumberBigInteger(t *testing.T) {
	// Value exceeds int64 range → BigInteger.
	data := map[string]any{"v": json.Number("99999999999999999999")}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	types := node.Object().Child("v").DeclaredTypes()
	if len(types) != 1 || types[0] != schema.BigInteger {
		t.Errorf("expected [BIG_INTEGER], got %v", types)
	}
}

func TestWalkJsonNumberDecimal(t *testing.T) {
	data := map[string]any{"v": json.Number("3.14")}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	types := node.Object().Child("v").DeclaredTypes()
	if len(types) != 1 || types[0] != schema.Double {
		t.Errorf("expected [DOUBLE], got %v", types)
	}
}

func TestWalkUnsupportedType(t *testing.T) {
	data := map[string]any{"x": struct{}{}}
	_, err := importer.Walk(data)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

func TestWalker_ValueBasedClassification(t *testing.T) {
	cases := []struct {
		in   string
		want schema.DataType
	}{
		// Integer-family literals (including decimal-shaped whole numbers).
		{`42`, schema.Integer},
		{`"1.0"`, schema.String},                                           // quoted is STRING
		{`9007199254740993`, schema.Long},                                  // 2^53 + 1, beyond int32
		{`9223372036854775808`, schema.BigInteger},                         // 2^63, beyond int64
		{`170141183460469231731687303715884105728`, schema.UnboundInteger}, // 2^127
		// Whole-number decimals route to integer branch.
		{`1.0`, schema.Integer},
		{`1e0`, schema.Integer},
		{`100`, schema.Integer}, // strip → (1, -2), value-based → Integer
		// Fractional decimals route to decimal branch.
		{`0.1`, schema.Double},
		{`1.5`, schema.Double},
		// Pi-18 is BIG_DECIMAL; pi-20 is UNBOUND_DECIMAL.
		{`3.141592653589793238`, schema.BigDecimal},
		{`3.14159265358979323846`, schema.UnboundDecimal},
		// Overflow: 10^400 is a whole number; value-based → integer branch.
		{`1e400`, schema.UnboundInteger},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(c.in))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			node, err := importer.Walk(v)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if node.Scalar() == nil {
				t.Fatalf("expected a scalar branch, got %v", node.Kinds())
			}
			types := node.DeclaredTypes()
			if len(types) != 1 {
				t.Fatalf("expected single type, got %v", types)
			}
			if types[0] != c.want {
				t.Errorf("walker for %q: got %s, want %s", c.in, types[0], c.want)
			}
		})
	}
}

func TestWalkNull(t *testing.T) {
	data := map[string]any{"missing": nil}
	node, err := importer.Walk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	missing := node.Object().Child("missing")
	types := missing.DeclaredTypes()
	if len(types) != 1 || types[0] != schema.Null {
		t.Errorf("expected [NULL], got %v", types)
	}
}
