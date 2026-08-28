package schema

import (
	"encoding/json"
	"testing"
)

func TestValidationError_ErrKindUnknownElement_SetForExtraField(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("name", NewLeafNode(String))
	// Data has an extra field not in the schema.
	data := map[string]any{"name": "alice", "email": "a@b.c"}
	errs := Validate(model, data)
	if len(errs) == 0 {
		t.Fatal("expected at least one validation error")
	}
	found := false
	for _, e := range errs {
		if e.Kind == ErrKindUnknownElement {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ErrKindUnknownElement for extra field, got: %+v", errs)
	}
}

func TestValidationError_ErrKindGeneric_ForTypeMismatch(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("age", NewLeafNode(Integer))
	data := map[string]any{"age": "not a number"}
	errs := Validate(model, data)
	if len(errs) == 0 {
		t.Fatal("expected type-mismatch error")
	}
	for _, e := range errs {
		if e.Kind == ErrKindUnknownElement {
			t.Errorf("type mismatch should NOT be ErrKindUnknownElement: %+v", e)
		}
	}
}

// TestValidationError_ErrKindIncompatibleType_LeafTypeMismatch asserts that
// scalar value-vs-type leaf mismatches (the canonical "incompatible type"
// signal — Cloud's FoundIncompatibleTypeWithEntityModelException) are
// classified as ErrKindIncompatibleType and carry the expected/actual
// DataType structure for downstream Props rendering.
func TestValidationError_ErrKindIncompatibleType_LeafTypeMismatch(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("price", NewLeafNode(Integer))
	// price is INTEGER; submit a STRING value (unambiguously incompatible).
	data := map[string]any{"price": "not-a-number"}
	errs := Validate(model, data)
	if len(errs) == 0 {
		t.Fatal("expected type-mismatch error")
	}
	var match *ValidationError
	for i := range errs {
		if errs[i].Kind == ErrKindIncompatibleType {
			match = &errs[i]
			break
		}
	}
	if match == nil {
		t.Fatalf("expected at least one ErrKindIncompatibleType error, got: %+v", errs)
	}
	if match.Path != "price" {
		t.Errorf("path: got %q, want %q", match.Path, "price")
	}
	if match.ActualType != String {
		t.Errorf("ActualType: got %v, want %v", match.ActualType, String)
	}
	if len(match.ExpectedTypes) != 1 || match.ExpectedTypes[0] != Integer {
		t.Errorf("ExpectedTypes: got %v, want [INTEGER]", match.ExpectedTypes)
	}
}

// TestValidationError_ErrKindIncompatibleType_LeafTypeMismatchTable broadens
// coverage of the INCOMPATIBLE_TYPE classification across additional
// scalar combinations the reviewer flagged for PR #129: BOOLEAN-vs-INTEGER,
// DOUBLE-vs-STRING, and STRING-vs-BOOLEAN. Each case asserts the
// downstream-relevant fields the entity handler renders into RFC 9457
// problem-detail Props (`fieldPath`, `expectedType`, `actualType`).
func TestValidationError_ErrKindIncompatibleType_LeafTypeMismatchTable(t *testing.T) {
	cases := []struct {
		name         string
		fieldName    string
		schemaType   DataType
		value        any
		wantActual   DataType
		wantExpected []DataType
	}{
		{
			name:         "BOOLEAN value vs INTEGER schema",
			fieldName:    "active",
			schemaType:   Integer,
			value:        true,
			wantActual:   Boolean,
			wantExpected: []DataType{Integer},
		},
		{
			name:         "DOUBLE value vs STRING schema",
			fieldName:    "label",
			schemaType:   String,
			value:        json.Number("13.111"),
			wantActual:   Double,
			wantExpected: []DataType{String},
		},
		{
			name:         "STRING value vs BOOLEAN schema",
			fieldName:    "flag",
			schemaType:   Boolean,
			value:        "yes",
			wantActual:   String,
			wantExpected: []DataType{Boolean},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := NewObjectNode()
			model.SetChild(tc.fieldName, NewLeafNode(tc.schemaType))
			data := map[string]any{tc.fieldName: tc.value}

			errs := Validate(model, data)
			if len(errs) == 0 {
				t.Fatalf("expected validation error, got none")
			}
			match := FirstIncompatibleType(errs)
			if match == nil {
				t.Fatalf("expected ErrKindIncompatibleType entry, got: %+v", errs)
			}
			if match.Kind != ErrKindIncompatibleType {
				t.Errorf("Kind: got %v, want ErrKindIncompatibleType", match.Kind)
			}
			if match.Path != tc.fieldName {
				t.Errorf("Path: got %q, want %q", match.Path, tc.fieldName)
			}
			if match.ActualType != tc.wantActual {
				t.Errorf("ActualType: got %v, want %v", match.ActualType, tc.wantActual)
			}
			if len(match.ExpectedTypes) != len(tc.wantExpected) {
				t.Fatalf("ExpectedTypes length: got %d, want %d (%v vs %v)",
					len(match.ExpectedTypes), len(tc.wantExpected),
					match.ExpectedTypes, tc.wantExpected)
			}
			for i, want := range tc.wantExpected {
				if match.ExpectedTypes[i] != want {
					t.Errorf("ExpectedTypes[%d]: got %v, want %v",
						i, match.ExpectedTypes[i], want)
				}
			}
		})
	}
}

// TestValidationError_ErrKindIncompatibleType_UnionExpectedTypes covers the
// reviewer-flagged "widened TypeSet" case where the schema leaf carries more
// than one DataType (e.g. a polymorphic merge produced {STRING, BOOLEAN}) and
// the submitted scalar matches neither. The test asserts the structured
// ValidationError fields and round-trips ExpectedTypes through the same
// shape the entity handler builds for INCOMPATIBLE_TYPE Props — i.e.
// `expectedType` must serialize as a JSON array of DataType names, mirroring
// classifyValidateOrExtendErr in internal/domain/entity/handler.go.
func TestValidationError_ErrKindIncompatibleType_UnionExpectedTypes(t *testing.T) {
	// Build a leaf with a polymorphic TypeSet {STRING, BOOLEAN}. Both types
	// are non-numeric so the collapse rule preserves both members; the
	// TypeSet sorts by DataType-iota order (STRING=7 < BOOLEAN=17).
	leaf := NewLeafNode(String)
	leaf.AddScalarTypes(Boolean)
	if len(leaf.DeclaredTypes()) != 2 {
		t.Fatalf("precondition: expected two declared types, got %v",
			leaf.DeclaredTypes())
	}

	model := NewObjectNode()
	model.SetChild("value", leaf)

	// Submit an INTEGER — incompatible with both STRING and BOOLEAN.
	data := map[string]any{"value": json.Number("42")}
	errs := Validate(model, data)
	if len(errs) == 0 {
		t.Fatalf("expected validation error, got none")
	}
	match := FirstIncompatibleType(errs)
	if match == nil {
		t.Fatalf("expected ErrKindIncompatibleType entry, got: %+v", errs)
	}
	if match.Path != "value" {
		t.Errorf("Path: got %q, want %q", match.Path, "value")
	}
	if match.ActualType != Integer {
		t.Errorf("ActualType: got %v, want %v", match.ActualType, Integer)
	}
	if len(match.ExpectedTypes) != 2 {
		t.Fatalf("ExpectedTypes length: got %d, want 2 (got %v)",
			len(match.ExpectedTypes), match.ExpectedTypes)
	}
	// TypeSet sorts by DataType-iota order: STRING (7) before BOOLEAN (17).
	if match.ExpectedTypes[0] != String || match.ExpectedTypes[1] != Boolean {
		t.Errorf("ExpectedTypes: got %v, want [STRING BOOLEAN]",
			match.ExpectedTypes)
	}

	// Round-trip the handler-side rendering: convert []DataType → []string
	// and JSON-marshal a Props map shaped identically to the one emitted by
	// classifyValidateOrExtendErr. Assert `expectedType` decodes as a JSON
	// array (not a scalar / not a stringified slice).
	expected := make([]string, len(match.ExpectedTypes))
	for i, dt := range match.ExpectedTypes {
		expected[i] = dt.String()
	}
	props := map[string]any{
		"errorCode":    "INCOMPATIBLE_TYPE",
		"fieldPath":    match.Path,
		"expectedType": expected,
		"actualType":   match.ActualType.String(),
	}
	body, err := json.Marshal(props)
	if err != nil {
		t.Fatalf("marshal Props: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal Props: %v; body=%s", err, string(body))
	}
	if got := decoded["errorCode"]; got != "INCOMPATIBLE_TYPE" {
		t.Errorf("errorCode: got %v, want %q", got, "INCOMPATIBLE_TYPE")
	}
	if got := decoded["fieldPath"]; got != "value" {
		t.Errorf("fieldPath: got %v, want %q", got, "value")
	}
	if got := decoded["actualType"]; got != "INTEGER" {
		t.Errorf("actualType: got %v, want %q", got, "INTEGER")
	}
	expectedAny, ok := decoded["expectedType"].([]any)
	if !ok {
		t.Fatalf("expectedType must JSON-encode as an array, got %T (%v)",
			decoded["expectedType"], decoded["expectedType"])
	}
	if len(expectedAny) != 2 {
		t.Fatalf("expectedType length: got %d, want 2 (%v)",
			len(expectedAny), expectedAny)
	}
	if expectedAny[0] != "STRING" || expectedAny[1] != "BOOLEAN" {
		t.Errorf("expectedType: got %v, want [STRING BOOLEAN]", expectedAny)
	}
}

// TestHasIncompatibleType_Match returns the first incompatible-type error
// from a slice, or nil when none is present. Mirrors HasUnknownSchemaElement
// shape so handlers can branch on classification.
func TestHasIncompatibleType_Match(t *testing.T) {
	errs := []ValidationError{
		{Path: "x", Message: "extra", Kind: ErrKindUnknownElement},
		{Path: "price", Message: "value of type STRING is not compatible with [INTEGER]", Kind: ErrKindIncompatibleType, ActualType: String, ExpectedTypes: []DataType{Integer}},
	}
	got := FirstIncompatibleType(errs)
	if got == nil {
		t.Fatal("expected non-nil match")
	}
	if got.Path != "price" {
		t.Errorf("path: got %q, want %q", got.Path, "price")
	}
}

func TestHasIncompatibleType_NoMatch(t *testing.T) {
	errs := []ValidationError{
		{Path: "x", Message: "extra", Kind: ErrKindUnknownElement},
	}
	if got := FirstIncompatibleType(errs); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestHasUnknownSchemaElement_Empty(t *testing.T) {
	if HasUnknownSchemaElement(nil) {
		t.Error("nil slice must not match")
	}
	if HasUnknownSchemaElement([]ValidationError{}) {
		t.Error("empty slice must not match")
	}
}

func TestHasUnknownSchemaElement_WithMatch(t *testing.T) {
	errs := []ValidationError{
		{Path: "name", Message: "type", Kind: ErrKindGeneric},
		{Path: "email", Message: "extra", Kind: ErrKindUnknownElement},
	}
	if !HasUnknownSchemaElement(errs) {
		t.Error("expected true when any error is ErrKindUnknownElement")
	}
}

func TestHasUnknownSchemaElement_NoMatch(t *testing.T) {
	errs := []ValidationError{
		{Path: "name", Message: "type", Kind: ErrKindGeneric},
	}
	if HasUnknownSchemaElement(errs) {
		t.Error("expected false when no ErrKindUnknownElement")
	}
}
