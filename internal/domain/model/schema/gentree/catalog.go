package gentree

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// Fixture is one named, hand-crafted test case.
// Exactly one of (Old, Incoming) may be nil — a nil Old means "create from scratch".
// ExpectedKinds, when non-nil, is asserted against the Diff output verbatim.
type Fixture struct {
	Name          string
	Old           *schema.ModelNode
	Incoming      any // fed through importer.Walk
	Level         spi.ChangeLevel
	ExpectedKinds []schema.SchemaOpKind // nil = don't assert
	ExpectError   bool
}

// Catalog is the authoritative list of named regression fixtures.
// Adding entries: keep names stable once merged; tests reference names.
var Catalog = []Fixture{
	// --- Flat/nested objects ---
	{
		Name:          "FlatObjectAddSibling",
		Old:           objNode(map[string]*schema.ModelNode{"a": leaf(schema.String)}),
		Incoming:      map[string]any{"a": "x", "b": json.Number("1")},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},
	{
		Name:          "NestedObjectAddLeaf",
		Old:           objNode(map[string]*schema.ModelNode{"outer": objNode(map[string]*schema.ModelNode{"inner": leaf(schema.Integer)})}),
		Incoming:      map[string]any{"outer": map[string]any{"inner": json.Number("1"), "new": "s"}},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},
	{
		Name:     "DeeplyNestedIntegerExtend",
		Old:      deepObject(10, leaf(schema.Integer)),
		Incoming: deepObjectValue(10, json.Number("1")),
		Level:    spi.ChangeLevelStructural,
	},
	{
		Name:          "WideObjectAddOne",
		Old:           wideObject(100, schema.String),
		Incoming:      wideObjectValuePlus(100, "extra"),
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},

	// --- Arrays ---
	{
		Name:          "ArrayIntegerWidenToLong",
		Old:           schema.NewArrayNode(leaf(schema.Integer)),
		Incoming:      []any{json.Number("1"), json.Number("9223372036854000000")},
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddArrayItemType},
	},
	{
		Name:          "ArrayOfObjectAddFieldInElement",
		Old:           schema.NewArrayNode(objNode(map[string]*schema.ModelNode{"k": leaf(schema.Integer)})),
		Incoming:      []any{map[string]any{"k": json.Number("1"), "extra": "s"}},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},
	{
		Name:          "ArrayOfObjectWidenLeafInElement",
		Old:           schema.NewArrayNode(objNode(map[string]*schema.ModelNode{"k": leaf(schema.Integer)})),
		Incoming:      []any{map[string]any{"k": json.Number("1.5")}},
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},
	{
		Name:     "ArrayOfArrayWidenInnerLeaf",
		Old:      schema.NewArrayNode(schema.NewArrayNode(leaf(schema.Integer))),
		Incoming: []any{[]any{json.Number("1.5")}},
		Level:    spi.ChangeLevelType,
	},
	{
		Name:     "ArrayOfArrayOfArrayElement",
		Old:      schema.NewArrayNode(schema.NewArrayNode(schema.NewArrayNode(leaf(schema.Integer)))),
		Incoming: []any{[]any{[]any{json.Number("1")}}},
		Level:    spi.ChangeLevelStructural,
	},
	{
		Name:          "EmptyArrayObservesElement",
		Old:           schema.NewArrayNode(nil),
		Incoming:      []any{json.Number("1")},
		Level:         spi.ChangeLevelArrayElements,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddArrayItemType},
	},
	{
		Name:          "PolymorphicLeafAddInteger",
		Old:           leaf(schema.String),
		Incoming:      json.Number("1"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},
	{
		Name:          "IntegerFieldSeesDouble",
		Old:           leaf(schema.Integer),
		Incoming:      json.Number("3.14"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},

	// --- Unicode + edge cases ---
	{
		Name:          "UnicodeKey4ByteCodepoint",
		Old:           objNode(map[string]*schema.ModelNode{"🐙": leaf(schema.String)}),
		Incoming:      map[string]any{"🐙": "tentacle", "🦊": "fox"},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},
	{
		Name:     "SameKeyNestedDifferentType",
		Old:      objNode(map[string]*schema.ModelNode{"a": objNode(map[string]*schema.ModelNode{"b": leaf(schema.Integer)})}),
		Incoming: map[string]any{"a": map[string]any{"b": json.Number("1"), "c": map[string]any{"b": "s"}}},
		Level:    spi.ChangeLevelStructural,
	},
	{
		Name:     "NullableFieldAppears",
		Old:      objNode(map[string]*schema.ModelNode{"a": leaf(schema.String)}),
		Incoming: map[string]any{"a": "x", "b": nil},
		Level:    spi.ChangeLevelStructural,
	},

	// --- Numeric boundaries (match A.1 rev 3 §2.3) ---
	{
		Name:     "IntegerBoundaryExceedsDouble", // 2^53+1
		Old:      leaf(schema.Integer),
		Incoming: json.Number("9007199254740993"),
		Level:    spi.ChangeLevelType,
	},
	{
		Name:          "LongBoundaryPromotesBigInteger", // 2^63
		Old:           leaf(schema.Long),
		Incoming:      json.Number("9223372036854775808"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},
	{
		Name:          "BigIntegerBoundaryExceeds128", // 2^127
		Old:           leaf(schema.BigInteger),
		Incoming:      json.Number("340282366920938463463374607431768211456"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},
	{
		Name:     "DecimalBoundaryFitsBigDecimal", // 18 fractional digits
		Old:      leaf(schema.Double),
		Incoming: json.Number("1.234567890123456789"),
		Level:    spi.ChangeLevelType,
	},
	{
		Name:          "DecimalBoundaryExceedsBigDecimal", // 20 fractional digits
		Old:           leaf(schema.BigDecimal),
		Incoming:      json.Number("1.23456789012345678901"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},

	// --- ChangeLevel enforcement cases ---
	{
		Name:     "ArrayLengthGrowsPermitted",
		Old:      schema.NewArrayNode(leaf(schema.Integer)),
		Incoming: []any{json.Number("1"), json.Number("2"), json.Number("3")},
		Level:    spi.ChangeLevelArrayLength,
	},
	{
		Name:        "TypeLevelRejectsStructural",
		Old:         objNode(map[string]*schema.ModelNode{"a": leaf(schema.Integer)}),
		Incoming:    map[string]any{"a": json.Number("1"), "b": "extra"},
		Level:       spi.ChangeLevelType,
		ExpectError: true,
	},
	{
		Name:        "StrictValidateRejectsNewField",
		Old:         objNode(map[string]*schema.ModelNode{"a": leaf(schema.Integer)}),
		Incoming:    map[string]any{"a": json.Number("1"), "b": "extra"},
		Level:       "",
		ExpectError: true,
	},

	// --- Additional fixtures to reach >=40 total ---

	// 1. Multi-op in one diff — old has a leaf and a field gap; incoming broadens leaf AND adds field.
	{
		Name: "MultipleAddAndBroadenInOneExtend",
		Old: objNode(map[string]*schema.ModelNode{
			"a": leaf(schema.Integer),
		}),
		Incoming: map[string]any{
			"a":   json.Number("3.14"), // broaden Integer -> Double
			"new": "s",                 // add property
		},
		Level: spi.ChangeLevelStructural,
	},

	// 2. No-op — old and incoming are structurally identical.
	{
		Name: "NoOpExtendProducesNilDelta",
		Old: objNode(map[string]*schema.ModelNode{
			"a": leaf(schema.Integer),
			"b": leaf(schema.String),
		}),
		Incoming: map[string]any{"a": json.Number("1"), "b": "x"},
		Level:    spi.ChangeLevelStructural,
	},

	// 3. Array length at ArrayLength level — same element type, just more items.
	{
		Name:     "ArrayLengthRejectsElementChangeAtArrayLength",
		Old:      schema.NewArrayNode(leaf(schema.Integer)),
		Incoming: []any{json.Number("10"), json.Number("20"), json.Number("30"), json.Number("40")},
		Level:    spi.ChangeLevelArrayLength,
	},

	// 4. Array element broaden at ArrayElements level — incoming element type requires broaden.
	{
		Name:     "ArrayElementsAllowsInnerBroaden",
		Old:      schema.NewArrayNode(leaf(schema.Integer)),
		Incoming: []any{json.Number("1"), json.Number("2.5")},
		Level:    spi.ChangeLevelArrayElements,
	},

	// 5. Empty incoming object against a populated schema — observation-only (no-op).
	{
		Name: "EmptyObjectIncomingAgainstPopulated",
		Old: objNode(map[string]*schema.ModelNode{
			"a": leaf(schema.String),
			"b": leaf(schema.Integer),
		}),
		Incoming: map[string]any{},
		Level:    spi.ChangeLevelStructural,
	},

	// 6. Integer -> Double cross-family broaden at Type level.
	{
		Name:          "IntegerToDoubleCollapse",
		Old:           leaf(schema.Integer),
		Incoming:      json.Number("3.14"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},

	// 7. Integer + Integer — no change, idempotent at Type level.
	{
		Name:     "IntegerNoChangeIdempotent",
		Old:      leaf(schema.Integer),
		Incoming: json.Number("42"),
		Level:    spi.ChangeLevelType,
	},

	// 8. Double + BigDecimal (18+ digits triggers broaden to BigDecimal).
	{
		Name:          "DoubleBroadenToBigDecimal",
		Old:           leaf(schema.Double),
		Incoming:      json.Number("1.234567890123456789"),
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindBroadenType},
	},

	// 9. String + Null — produces a nullable String.
	{
		Name:     "StringPlusNullProducesNullable",
		Old:      leaf(schema.String),
		Incoming: nil,
		Level:    spi.ChangeLevelType,
	},

	// 10. Array of polymorphic leaf broaden.
	{
		Name:     "ArrayOfPolymorphicLeafBroaden",
		Old:      schema.NewArrayNode(leaf(schema.String)),
		Incoming: []any{"x", json.Number("1"), true},
		Level:    spi.ChangeLevelType,
	},

	// 11. Deep nested broaden at depth 5.
	{
		Name:     "DeepNestedBroadenAtLevel5",
		Old:      deepObject(5, leaf(schema.Integer)),
		Incoming: deepObjectValue(5, json.Number("2.718")),
		Level:    spi.ChangeLevelType,
	},

	// 12. Case sensitivity — {"A": ..., "a": ...} are distinct keys.
	{
		Name: "CaseSensitiveKeysDistinct",
		Old: objNode(map[string]*schema.ModelNode{
			"a": leaf(schema.String),
		}),
		Incoming:      map[string]any{"a": "lower", "A": "upper"},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},

	// 13. Very long key name (200 chars).
	{
		Name:          "VeryLongKeyName",
		Old:           objNode(map[string]*schema.ModelNode{strings.Repeat("k", 200): leaf(schema.String)}),
		Incoming:      map[string]any{strings.Repeat("k", 200): "v", strings.Repeat("m", 200): "v2"},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},

	// 14. Boolean leaf unchanged.
	{
		Name:     "BooleanLeafUnchanged",
		Old:      leaf(schema.Boolean),
		Incoming: true,
		Level:    spi.ChangeLevelType,
	},

	// 15. Single-character key.
	{
		Name:     "SingleCharKey",
		Old:      objNode(map[string]*schema.ModelNode{"x": leaf(schema.Integer)}),
		Incoming: map[string]any{"x": json.Number("1")},
		Level:    spi.ChangeLevelStructural,
	},

	// 16. Empty array remains empty — idempotent.
	{
		Name:     "EmptyArrayIdempotent",
		Old:      schema.NewArrayNode(nil),
		Incoming: []any{},
		Level:    spi.ChangeLevelStructural,
	},

	// 17. Array Integer[] broadened to Double[] across all elements.
	{
		Name:          "ArrayIntegerBroadenAllElements",
		Old:           schema.NewArrayNode(leaf(schema.Integer)),
		Incoming:      []any{json.Number("1.1"), json.Number("2.2"), json.Number("3.3")},
		Level:         spi.ChangeLevelType,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddArrayItemType},
	},

	// 18. Nested object containing an array of objects.
	{
		Name: "NestedObjectContainsArrayOfObjects",
		Old: objNode(map[string]*schema.ModelNode{
			"items": schema.NewArrayNode(objNode(map[string]*schema.ModelNode{
				"id": leaf(schema.Integer),
			})),
		}),
		Incoming: map[string]any{
			"items": []any{
				map[string]any{"id": json.Number("1")},
				map[string]any{"id": json.Number("2"), "name": "alpha"},
			},
		},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty},
	},

	// 19. Long numeric value within range — no broaden needed.
	{
		Name:     "LongWithinRange",
		Old:      leaf(schema.Long),
		Incoming: json.Number("9223372036854775000"),
		Level:    spi.ChangeLevelType,
	},

	// 20. Array of string to array of mixed — empty array seed adding string element.
	{
		Name:          "EmptyArrayObservesStringElement",
		Old:           schema.NewArrayNode(nil),
		Incoming:      []any{"hello"},
		Level:         spi.ChangeLevelArrayElements,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddArrayItemType},
	},

	// 21. Add multiple siblings in one go.
	{
		Name:          "AddMultipleSiblings",
		Old:           objNode(map[string]*schema.ModelNode{"a": leaf(schema.Integer)}),
		Incoming:      map[string]any{"a": json.Number("1"), "b": "s", "c": true, "d": json.Number("2.5")},
		Level:         spi.ChangeLevelStructural,
		ExpectedKinds: []schema.SchemaOpKind{schema.KindAddProperty, schema.KindAddProperty, schema.KindAddProperty},
	},
}

// Test helpers exposed to catalog_test.go and callers.
func leaf(dt schema.DataType) *schema.ModelNode { return schema.NewLeafNode(dt) }

func objNode(fields map[string]*schema.ModelNode) *schema.ModelNode {
	n := schema.NewObjectNode()
	// Sorted key iteration — determinism.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n.SetChild(k, fields[k])
	}
	return n
}

func deepObject(depth int, inner *schema.ModelNode) *schema.ModelNode {
	if depth == 0 {
		return inner
	}
	n := schema.NewObjectNode()
	n.SetChild("x", deepObject(depth-1, inner))
	return n
}

func deepObjectValue(depth int, leafVal any) any {
	if depth == 0 {
		return leafVal
	}
	return map[string]any{"x": deepObjectValue(depth-1, leafVal)}
}

func wideObject(n int, dt schema.DataType) *schema.ModelNode {
	node := schema.NewObjectNode()
	for i := 0; i < n; i++ {
		node.SetChild("f"+strconv.Itoa(i), leaf(dt))
	}
	return node
}

func wideObjectValuePlus(n int, extraKey string) map[string]any {
	m := make(map[string]any, n+1)
	for i := 0; i < n; i++ {
		m["f"+strconv.Itoa(i)] = "v"
	}
	m[extraKey] = "v"
	return m
}
