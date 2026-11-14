package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestValidateConforming(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("name", schema.NewLeafNode(schema.String))
	model.SetChild("age", schema.NewLeafNode(schema.Integer))

	data := map[string]any{"name": "Alice", "age": json.Number("30")}
	errs := schema.Validate(model, data)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidateTypeMismatch(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("age", schema.NewLeafNode(schema.Integer))

	data := map[string]any{"age": "not a number"}
	errs := schema.Validate(model, data)
	if len(errs) == 0 {
		t.Error("expected validation error for type mismatch")
	}
}

func TestValidateExtraFieldRejected(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("name", schema.NewLeafNode(schema.String))

	data := map[string]any{"name": "Alice", "extra": "field"}
	errs := schema.Validate(model, data)
	if len(errs) == 0 {
		t.Error("expected validation error for extra field, got none")
	}
}

func TestValidateNestedObject(t *testing.T) {
	inner := schema.NewObjectNode()
	inner.SetChild("city", schema.NewLeafNode(schema.String))
	model := schema.NewObjectNode()
	model.SetChild("address", inner)

	data := map[string]any{
		"address": map[string]any{"city": json.Number("12345")},
	}
	errs := schema.Validate(model, data)
	if len(errs) == 0 {
		t.Error("expected validation error for nested type mismatch")
	}
}

func TestValidatePolymorphicAcceptsBothTypes(t *testing.T) {
	leaf := schema.NewLeafNode(schema.Integer)
	leaf.AddScalarTypes(schema.String)
	model := schema.NewObjectNode()
	model.SetChild("value", leaf)

	// Integer should pass
	errs := schema.Validate(model, map[string]any{"value": json.Number("42")})
	if len(errs) != 0 {
		t.Errorf("integer should be accepted: %v", errs)
	}

	// String should pass
	errs = schema.Validate(model, map[string]any{"value": "hello"})
	if len(errs) != 0 {
		t.Errorf("string should be accepted: %v", errs)
	}

	// Boolean should fail
	errs = schema.Validate(model, map[string]any{"value": true})
	if len(errs) == 0 {
		t.Error("boolean should be rejected for [INTEGER, STRING] field")
	}
}

func TestValidateArray(t *testing.T) {
	elemModel := schema.NewLeafNode(schema.String)
	arrModel := schema.NewArrayNode(elemModel)
	model := schema.NewObjectNode()
	model.SetChild("tags", arrModel)

	// Valid array
	errs := schema.Validate(model, map[string]any{"tags": []any{"a", "b"}})
	if len(errs) != 0 {
		t.Errorf("valid array should pass: %v", errs)
	}

	// Invalid element type
	errs = schema.Validate(model, map[string]any{"tags": []any{"a", json.Number("1")}})
	if len(errs) == 0 {
		t.Error("expected error for invalid array element type")
	}
}

func TestValidateWrongStructure(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("name", schema.NewLeafNode(schema.String))

	// Passing array where object expected
	errs := schema.Validate(model, []any{"not", "an", "object"})
	if len(errs) == 0 {
		t.Error("expected error for wrong top-level structure")
	}
}

func TestValidateNullCompatible(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("name", schema.NewLeafNode(schema.String))

	// Null should be compatible with any type
	errs := schema.Validate(model, map[string]any{"name": nil})
	if len(errs) != 0 {
		t.Errorf("null should be compatible with STRING: %v", errs)
	}
}

// TestValidateJSONNumberAgainstNumeric — XML and JSON importers both
// produce json.Number for numeric leaves.
// inferDataType must classify json.Number as numeric, otherwise
// validation falsely rejects every numeric XML/JSON-imported field.
func TestValidateJSONNumberAgainstNumeric(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("age", schema.NewLeafNode(schema.Integer))
	model.SetChild("rate", schema.NewLeafNode(schema.Double))
	model.SetChild("big", schema.NewLeafNode(schema.Long))

	data := map[string]any{
		"age":  json.Number("30"),
		"rate": json.Number("3.14"),
		"big":  json.Number("9007199254740993"), // > 2^53
	}
	errs := schema.Validate(model, data)
	if len(errs) != 0 {
		t.Errorf("json.Number should be compatible with numeric model types, got: %v", errs)
	}
}

func TestValidate_IntegerSchema_RejectsDecimalValue(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.Integer))
	data := map[string]any{"x": json.Number("13.111")}
	errs := schema.Validate(model, data)
	if len(errs) == 0 {
		t.Fatal("expected rejection")
	}
}

func TestValidate_DoubleSchema_AcceptsIntegerValue(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.Double))
	data := map[string]any{"x": json.Number("13")}
	errs := schema.Validate(model, data)
	if len(errs) != 0 {
		t.Errorf("expected acceptance; got errors: %v", errs)
	}
}

func TestValidate_BigDecimalSchema_AcceptsHighPrecision(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.BigDecimal))
	data := map[string]any{"x": json.Number("3.141592653589793238")}
	errs := schema.Validate(model, data)
	if len(errs) != 0 {
		t.Errorf("expected acceptance; got errors: %v", errs)
	}
}

func TestValidate_IntegerSchema_AcceptsInteger(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.Integer))
	data := map[string]any{"x": json.Number("13")}
	errs := schema.Validate(model, data)
	if len(errs) != 0 {
		t.Errorf("expected acceptance; got errors: %v", errs)
	}
}

func TestValidate_LongSchema_RejectsDouble(t *testing.T) {
	// LONG → DOUBLE is blocked in the widening lattice, but this is
	// about a data value classified as LONG landing in a DOUBLE schema
	// (that's allowed: LONG → DOUBLE? actually no, LONG→DOUBLE is
	// blocked). Assert what actually happens:
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.Long))
	// A value classified as Double (has fractional part) against Long schema.
	data := map[string]any{"x": json.Number("3.14")}
	errs := schema.Validate(model, data)
	if len(errs) == 0 {
		t.Errorf("expected rejection; Double value cannot validate against Long schema")
	}
}

// TestValidate_PolymorphicArrayElement_AcceptsParticipatingType covers the
// 14/01 scenario: $.some-array[*].some-object is an object in element 0 and
// a plain string in element 1.  After merging, the array element node has
// Kind=KindObject but Types contains String.  The validator must accept both
// branches instead of rejecting the string with "expected object, got string".
func TestValidate_PolymorphicArrayElement_AcceptsParticipatingType(t *testing.T) {
	// Build the merged element node by hand, mirroring what walker+Merge produce.
	// Element 0: KindObject with child "some-key"
	objElem := schema.NewObjectNode()
	objElem.SetChild("some-key", schema.NewLeafNode(schema.String))

	// Element 1: KindLeaf(String)
	strElem := schema.NewLeafNode(schema.String)

	// Merge as the walker would after seeing both array elements.
	merged := schema.Merge(objElem, strElem)

	arrModel := schema.NewArrayNode(merged)
	root := schema.NewObjectNode()
	root.SetChild("some-array", arrModel)

	// Object branch must validate cleanly.
	objBranch := map[string]any{
		"some-array": []any{
			map[string]any{"some-key": "v1"},
		},
	}
	if errs := schema.Validate(root, objBranch); len(errs) != 0 {
		t.Errorf("object branch: expected no errors, got %v", errs)
	}

	// String branch must also validate cleanly (was rejected before the fix).
	strBranch := map[string]any{
		"some-array": []any{"abc"},
	}
	if errs := schema.Validate(root, strBranch); len(errs) != 0 {
		t.Errorf("string branch: expected no errors, got %v", errs)
	}

	// Both branches together must validate cleanly.
	bothBranch := map[string]any{
		"some-array": []any{
			map[string]any{"some-key": "v1"},
			"abc",
		},
	}
	if errs := schema.Validate(root, bothBranch); len(errs) != 0 {
		t.Errorf("both branches: expected no errors, got %v", errs)
	}
}

// Strict validation and the change-level gate answer the same question, so a
// value a field holds passes strict validation too — including the case no
// changeLevel could work around.
func TestValidate_HeldValuesPassStrict(t *testing.T) {
	cases := []struct {
		name     string
		declared schema.DataType
		value    any
	}{
		{"a date-shaped string in a STRING field", schema.String, "2026-03-01"},
		{"a whole number past 2^31 in a DOUBLE field", schema.Double, num("2147483648")},
		{"an ordinary string", schema.String, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := schema.NewObjectNode()
			model.SetChild("f", schema.NewLeafNode(tc.declared))
			if errs := schema.Validate(model, map[string]any{"f": tc.value}); len(errs) != 0 {
				t.Errorf("want no errors, got %v", errs)
			}
		})
	}
}

// A value the field does not hold keeps its code and its Props.
func TestValidate_UnheldValueKeepsIncompatibleType(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("f", schema.NewLeafNode(schema.String))

	errs := schema.Validate(model, map[string]any{"f": num("5")})
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	if errs[0].Kind != schema.ErrKindIncompatibleType {
		t.Errorf("Kind = %v, want ErrKindIncompatibleType", errs[0].Kind)
	}
	if errs[0].ActualType != schema.Integer {
		t.Errorf("ActualType = %v, want INTEGER", errs[0].ActualType)
	}
	if len(errs[0].ExpectedTypes) != 1 || errs[0].ExpectedTypes[0] != schema.String {
		t.Errorf("ExpectedTypes = %v, want [STRING]", errs[0].ExpectedTypes)
	}
}

// Ruling 21 (final review I1): strict validation does not establish fields
// (docs/cloud-parity/model-field-name-grammar.md: "Strict validation ...
// does not establish fields, so the rule does not apply there"), so an
// unspellable field name must render as the ordinary unknown-field error —
// not as a grammar error via ErrInvalidFieldName. That sentinel's 400 belongs
// only to the doors that DO establish a field set: Admit's ChangeLevel-
// driven extension, and importer.Walk.
func TestValidate_UnspellableFieldNameIsUnknownElement(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("known", schema.NewLeafNode(schema.String))

	errs := schema.Validate(model, map[string]any{"known": "x", "bad name": num("1")})
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	if errs[0].Path != "bad name" {
		t.Errorf("Path = %q, want %q", errs[0].Path, "bad name")
	}
	if errs[0].Message != "unexpected field not present in model" {
		t.Errorf("Message = %q, want the ordinary unknown-field message", errs[0].Message)
	}
}

// Admit aborts its traversal at the first unspellable field name it finds
// (the write door must still refuse the whole write); strict validation must
// not inherit that abort — a document with two bad names gets two errors,
// not one.
func TestValidate_TwoUnspellableFieldNamesBothReported(t *testing.T) {
	model := schema.NewObjectNode()

	errs := schema.Validate(model, map[string]any{"bad name": num("1"), "bad name 2": num("2")})
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
	gotPaths := map[string]bool{}
	for _, e := range errs {
		gotPaths[e.Path] = true
	}
	if !gotPaths["bad name"] || !gotPaths["bad name 2"] {
		t.Errorf("want both bad field names reported, got %v", errs)
	}
}

// Final review M4: Validate's catch-all used to echo the raw Go error
// (including a %T-formatted type name) straight into ValidationError.Message
// — a caller-side contract violation (json.UseNumber not used, or a value
// the decoder never produces) leaking Go internals into a client-facing
// field. It must render a fixed message and, when the traversal knows one,
// the offending path — never the Go type.
func TestValidate_UnsupportedGoValueDoesNotLeakType(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("f", schema.NewLeafNode(schema.String))

	// A raw float64, not json.Number — the shape json.UseNumber is meant to
	// prevent, and the one caller-contract violation reachable through the
	// public API's own decoding path.
	errs := schema.Validate(model, map[string]any{"f": 3.14})
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	if strings.Contains(errs[0].Message, "float64") {
		t.Errorf("Message = %q, must not leak the Go type", errs[0].Message)
	}
	if errs[0].Path != "f" {
		t.Errorf("Path = %q, want %q", errs[0].Path, "f")
	}
}

// countUnknownElement returns how many errs carry {Kind: ErrKindGeneric,
// Path: path}, so a test can assert "exactly one" without caring how many
// OTHER errors (a different Kind, or a different Path) the document also
// produces.
func countUnknownElement(errs []schema.ValidationError, path string) int {
	n := 0
	for _, e := range errs {
		if e.Kind == schema.ErrKindGeneric && e.Path == path {
			n++
		}
	}
	return n
}

// Re-review finding #2: describeAt built its own admitter from scratch
// (`&admitter{}`), so re-entering it from object's new-field branch,
// wrongKind's container branch, or array's element-learning branch lost
// Validate's continueOnInvalidName flag — an unspellable name ANYWHERE
// under a brand-new subtree still hard-aborted the whole traversal with
// ErrInvalidFieldName, which M4's catch-all then rendered as
// ErrKindGeneric/"unsupported value" with an empty Path: worse than
// before this fix wave. All three shapes must instead report the nested
// bad name as the ordinary ErrKindGeneric at its own path, AND
// keep reporting a sibling unknown field elsewhere in the same document
// (the abort must be gone here too, not just at the top level ruling 21
// already covers).

// Shape 1: object's new-field branch (admit.go's object(), the describeAt
// call for a field the model does not declare at all).
func TestValidate_UnspellableNameNestedUnderNewField(t *testing.T) {
	model := schema.NewObjectNode() // declares nothing

	errs := schema.Validate(model, map[string]any{
		"newfield": map[string]any{"bad name": num("1")},
		"sibling":  "x",
	})
	if got := countUnknownElement(errs, "newfield.bad name"); got != 1 {
		t.Errorf("want exactly 1 ErrKindGeneric at %q, got %d in %v", "newfield.bad name", got, errs)
	}
	if got := countUnknownElement(errs, "sibling"); got != 1 {
		t.Errorf("sibling unknown field must also be reported, got %d in %v", got, errs)
	}
}

// Shape 2: wrongKind's container branch (a declared scalar receiving an
// object — the describeAt call wrongKind makes to derive the overlay for
// the mismatched container).
func TestValidate_UnspellableNameNestedUnderWrongKindContainer(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.String)) // scalar only, no object branch

	errs := schema.Validate(model, map[string]any{
		"x":       map[string]any{"bad name": num("1")},
		"sibling": "y",
	})
	if got := countUnknownElement(errs, "x.bad name"); got != 1 {
		t.Errorf("want exactly 1 ErrKindGeneric at %q, got %d in %v", "x.bad name", got, errs)
	}
	if got := countUnknownElement(errs, "sibling"); got != 1 {
		t.Errorf("sibling unknown field must also be reported, got %d in %v", got, errs)
	}
}

// Shape 3: array's element-learning branch (an array whose element was
// never observed — the describeAt call array() makes per element while
// learning one).
func TestValidate_UnspellableNameNestedUnderArrayElementLearning(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("a", schema.NewArrayNode(nil)) // array, element never observed

	errs := schema.Validate(model, map[string]any{
		"a":       []any{map[string]any{"bad name": num("1")}},
		"sibling": "z",
	})
	if got := countUnknownElement(errs, "a[0].bad name"); got != 1 {
		t.Errorf("want exactly 1 ErrKindGeneric at %q, got %d in %v", "a[0].bad name", got, errs)
	}
	if got := countUnknownElement(errs, "sibling"); got != 1 {
		t.Errorf("sibling unknown field must also be reported, got %d in %v", got, errs)
	}
}
