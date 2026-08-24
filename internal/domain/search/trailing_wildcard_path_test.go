package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// Whether a TRAILING array wildcard ("$.tags[*]") is a valid condition path is a
// question about the MODEL, not about the path grammar: it addresses the array's
// elements, so it is meaningful exactly when an element can be compared to a
// scalar. A field can be polymorphic, and the schema's flattening contract
// (schema.collectFields) already encodes the answer at the exact FieldsMap key:
//
//   - An array whose element is itself a leaf emits a descriptor AT the "[*]"
//     key with IsArray set — the primitive-array case. VALID.
//   - An array of objects that were ALSO observed as a bare concrete scalar
//     emits a self-descriptor at the "[*]" key carrying those scalar types —
//     the object-or-scalar case. VALID: the scalar occurrences are genuinely
//     comparable.
//   - An array of PURE objects emits no descriptor at the "[*]" key at all,
//     only its children ("$.items[*].price"). The path is a container, and
//     comparing the element to a scalar could only ever be false. INVALID.
//
// So the rule is: a trailing "[*]" admits a scalar comparison iff FieldsMap has a
// descriptor at that exact key. That is not a new predicate — it is precisely
// the leaf-vs-container split validateSimpleConditionType already applies to
// every path, via the single isKnownContainerPath predicate. The tests below pin
// the three model shapes against it so the rule cannot be narrowed to
// "reject if the element is an object", which would wrongly kill the
// polymorphic case.

// buildTrailingWildcardModel returns a model carrying one array of each shape:
//
//	$.arr[*]    INTEGER              — primitive array
//	$.mixed[*]  STRING, + .k INTEGER — object-or-scalar element
//	$.items[*]  (container), + .name — array of pure objects
func buildTrailingWildcardModel() *schema.ModelNode {
	root := schema.NewObjectNode()
	root.SetChild("arr", schema.NewArrayNode(schema.NewLeafNode(schema.Integer)))

	mixed := schema.NewObjectNode()
	mixed.SetChild("k", schema.NewLeafNode(schema.Integer))
	mixed.Types().Add(schema.String)
	root.SetChild("mixed", schema.NewArrayNode(mixed))

	item := schema.NewObjectNode()
	item.SetChild("name", schema.NewLeafNode(schema.String))
	root.SetChild("items", schema.NewArrayNode(item))
	return root
}

// TestFieldsMap_TrailingWildcardDescriptors pins the flattening contract the
// accept/reject rule is derived from. If schema.collectFields ever stops
// emitting the object-or-scalar self-descriptor, or starts emitting one for a
// pure container, the rule below silently changes meaning — so assert the
// premise, not just the conclusion.
func TestFieldsMap_TrailingWildcardDescriptors(t *testing.T) {
	fm := buildTrailingWildcardModel().FieldsMap()

	arr, ok := fm["$.arr[*]"]
	if !ok {
		t.Fatal(`FieldsMap has no "$.arr[*]": a primitive array must emit a descriptor at its own "[*]" key`)
	}
	if !arr.IsArray {
		t.Error(`"$.arr[*]".IsArray = false; the primitive-array element must carry it`)
	}
	if len(arr.Types) == 0 {
		t.Error(`"$.arr[*]" carries no declared types`)
	}

	mixed, ok := fm["$.mixed[*]"]
	if !ok {
		t.Fatal(`FieldsMap has no "$.mixed[*]": an object-or-scalar element must emit a self-descriptor`)
	}
	if len(mixed.Types) == 0 {
		t.Error(`"$.mixed[*]" carries no declared types; only concrete (non-NULL) types make it comparable`)
	}
	if _, ok := fm["$.mixed[*].k"]; !ok {
		t.Error(`FieldsMap has no "$.mixed[*].k": the object occurrences must stay independently searchable`)
	}

	if fd, ok := fm["$.items[*]"]; ok {
		t.Errorf(`FieldsMap has "$.items[*]" = %+v; an array of PURE objects must emit no self-descriptor`, fd)
	}
	if _, ok := fm["$.items[*].name"]; !ok {
		t.Error(`FieldsMap has no "$.items[*].name"`)
	}
}

// TestValidateConditionTypes_TrailingWildcard is the accept/reject rule itself.
func TestValidateConditionTypes_TrailingWildcard(t *testing.T) {
	model := buildTrailingWildcardModel()
	cases := []struct {
		name       string
		path       string
		op         string
		value      any
		wantReject bool
	}{
		// Primitive array — the element is a scalar, so compare away.
		{"primitive array", "$.arr[*]", "EQUALS", float64(10), false},
		// Object-or-scalar element — the scalar occurrences are comparable.
		// Rejecting this is the mistake a narrower "the element is an object"
		// rule would make.
		{"object-or-scalar element", "$.mixed[*]", "EQUALS", "abc", false},
		// ...and its declared types still constrain the operand.
		{"object-or-scalar element, wrong operand type", "$.mixed[*].k", "EQUALS", "not-a-number", true},
		// Array of pure objects — an element can never equal a scalar.
		{"pure object array", "$.items[*]", "EQUALS", "x", true},
		{"pure object array, string operator", "$.items[*]", "CONTAINS", "x", true},
		// A presence test carries no scalar operand: under element iteration
		// "does any element exist and is non-null" is a meaningful question on
		// an array of objects, so it stays accepted.
		{"pure object array, presence test", "$.items[*]", "NOT_NULL", nil, false},
		{"pure object array, null test", "$.items[*]", "IS_NULL", nil, false},
		// The leaf beneath the container is, as always, fine.
		{"leaf below the container", "$.items[*].name", "EQUALS", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConditionValueTypes(model, &predicate.SimpleCondition{
				JsonPath: tc.path, OperatorType: tc.op, Value: tc.value,
			})
			if tc.wantReject {
				if err == nil {
					t.Fatalf("ValidateConditionValueTypes(%q %s %v) = nil, want a rejection", tc.path, tc.op, tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateConditionValueTypes(%q %s %v) = %v, want accepted", tc.path, tc.op, tc.value, err)
			}
		})
	}
}

// The rejection must be INVALID_FIELD_PATH — the path itself does not address
// anything comparable — and must name the spelling the request actually sent.
func TestValidateConditionTypes_TrailingWildcardOnContainer_IsInvalidFieldPath(t *testing.T) {
	err := ValidateConditionValueTypes(buildTrailingWildcardModel(), &predicate.SimpleCondition{
		JsonPath: "$.items[*]", OperatorType: "EQUALS", Value: "x",
	})
	if !errors.Is(err, errInvalidFieldPath) {
		t.Fatalf(`ValidateConditionValueTypes("$.items[*]" EQUALS "x") = %v, want errInvalidFieldPath`, err)
	}
	if !strings.Contains(err.Error(), "$.items[*]") {
		t.Errorf("error %q does not name the path the request sent", err)
	}
}

// With NO schema bound the check is skipped, exactly as every other
// schema-dependent validator in the stack skips (validateConditionPaths returns
// nil on a nil FieldsMap; validateConditionTypes returns nil on a nil node).
// This is deliberate, not an oversight: with no schema there is no authority
// that says the element is an object, the model may legitimately hold a
// primitive array, and a 400 here would be a false rejection of a valid query.
// Element iteration is the correct semantics either way — only the declared
// types are missing, and an untyped comparison leaf already degrades to
// non-match rather than to a wrong match.
func TestValidateConditionTypes_TrailingWildcard_NoSchemaAccepts(t *testing.T) {
	if err := ValidateConditionValueTypes(nil, &predicate.SimpleCondition{
		JsonPath: "$.items[*]", OperatorType: "EQUALS", Value: "x",
	}); err != nil {
		t.Fatalf("ValidateConditionValueTypes(nil model) = %v, want accepted", err)
	}
}

// The field-path pass classifies a trailing "[*]" by the same contract: the
// primitive array and the object-or-scalar element are recorded leaves, and the
// pure-object array is a known container. None is an UNKNOWN path — the
// container is refused by the type pass above, with a diagnostic that says what
// to do about it, rather than being reported as a field the model never
// declared.
func TestFindUnknownFieldPaths_TrailingWildcardIsKnown(t *testing.T) {
	fm := buildTrailingWildcardModel().FieldsMap()
	for _, path := range []string{"$.arr[*]", "$.mixed[*]", "$.items[*]", "$.items[*].name"} {
		t.Run(path, func(t *testing.T) {
			unknown := FindUnknownFieldPaths(&predicate.SimpleCondition{
				JsonPath: path, OperatorType: "NOT_NULL",
			}, fm)
			if len(unknown) != 0 {
				t.Errorf("FindUnknownFieldPaths(%q) = %v, want none", path, unknown)
			}
		})
	}
}
