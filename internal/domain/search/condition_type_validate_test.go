package search

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// buildDoubleModel returns a minimal ModelNode with a single DOUBLE leaf at $.price.
func buildDoubleModel() *schema.ModelNode {
	node := schema.NewObjectNode()
	node.SetChild("price", schema.NewLeafNode(schema.Double))
	return node
}

// ---------------------------------------------------------------------------
// BETWEEN — composite array values
// ---------------------------------------------------------------------------

// TestValidateConditionTypes_Between_TypeMismatch verifies that a BETWEEN
// condition with string values against a DOUBLE field is rejected as a type
// mismatch (HTTP 400 CONDITION_TYPE_MISMATCH path).
func TestValidateConditionTypes_Between_TypeMismatch(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "BETWEEN",
		Value:        []any{"abc", "def"},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err == nil {
		t.Fatal("expected error for string values against DOUBLE BETWEEN condition, got nil")
	}
	if !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("expected errConditionTypeMismatch sentinel, got: %v", err)
	}
}

// TestValidateConditionTypes_Between_ValidIntegers verifies that a BETWEEN
// condition with numeric values against a DOUBLE field is accepted.
func TestValidateConditionTypes_Between_ValidIntegers(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "BETWEEN",
		// float64 is what json.Unmarshal produces for numbers
		Value: []any{float64(10), float64(20)},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err != nil {
		t.Fatalf("expected no error for numeric BETWEEN values against DOUBLE field, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Mixed-type array values (IN semantics)
// ---------------------------------------------------------------------------

// TestValidateConditionTypes_In_TypeMismatch verifies that an array containing
// a string element against a DOUBLE field is rejected.
func TestValidateConditionTypes_In_TypeMismatch(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "EQUALS",
		Value:        []any{float64(1), "abc", float64(3)},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err == nil {
		t.Fatal("expected error for mixed-type array value against DOUBLE field, got nil")
	}
	if !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("expected errConditionTypeMismatch sentinel, got: %v", err)
	}
}

// TestValidateConditionTypes_In_AllInts verifies that an array of all-numeric
// values against a DOUBLE field is accepted.
func TestValidateConditionTypes_In_AllInts(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "EQUALS",
		Value:        []any{float64(1), float64(2), float64(3)},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err != nil {
		t.Fatalf("expected no error for all-numeric array value against DOUBLE field, got: %v", err)
	}
}

// TestValidateConditionTypes_EmptyArray_Accepted verifies that an empty array
// value is accepted without error (no elements to mismatch).
func TestValidateConditionTypes_EmptyArray_Accepted(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "BETWEEN",
		Value:        []any{},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err != nil {
		t.Fatalf("expected no error for empty array value, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Object values — never valid for any operator
// ---------------------------------------------------------------------------

// TestValidateConditionTypes_ObjectValue_Rejects verifies that an object value
// (map[string]any) is rejected for any operator type against any field.
func TestValidateConditionTypes_ObjectValue_Rejects(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "EQUALS",
		Value:        map[string]any{"foo": float64(1)},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err == nil {
		t.Fatal("expected error for object value against any operator, got nil")
	}
	if !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("expected errConditionTypeMismatch sentinel, got: %v", err)
	}
}

// TestValidateConditionTypes_ObjectValue_StringField_Rejects verifies that
// object values are rejected even for string fields (object is never valid
// for any search operator).
func TestValidateConditionTypes_ObjectValue_StringField_Rejects(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        map[string]any{"nested": "value"},
	}
	err := ValidateConditionValueTypes(node, cond)
	if err == nil {
		t.Fatal("expected error for object value against string field, got nil")
	}
	if !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("expected errConditionTypeMismatch sentinel, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Null element inside array — accepted (null compatible with any type)
// ---------------------------------------------------------------------------

// TestValidateConditionTypes_ArrayWithNullElement_Accepted verifies that a nil
// element inside an array value is accepted (null is compatible with any type).
func TestValidateConditionTypes_ArrayWithNullElement_Accepted(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "BETWEEN",
		Value:        []any{float64(10), nil},
	}
	err := ValidateConditionValueTypes(model, cond)
	if err != nil {
		t.Fatalf("expected no error for array with null element, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle (meta) conditions — temporal operator/operand validation and
// unknown-meta-field rejection (Task 7).
// ---------------------------------------------------------------------------

// TestValidate_TemporalRejectsStringOp verifies that a string-comparison
// operator (CONTAINS) against a temporal meta field (creationDate) is
// rejected as a type mismatch — temporal fields only support the ordering/
// equality/null comparison family.
func TestValidate_TemporalRejectsStringOp(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2021"}
	if err := validateLifecycleType(c); !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("CONTAINS on creationDate should be CONDITION_TYPE_MISMATCH, got %v", err)
	}
}

// TestValidate_TemporalRejectsBadOperand verifies that a non-RFC3339,
// non-offset-bearing operand against a temporal meta field is rejected.
func TestValidate_TemporalRejectsBadOperand(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_THAN", Value: "not-a-date"}
	if err := validateLifecycleType(c); !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("non-RFC3339 operand on creationDate should be CONDITION_TYPE_MISMATCH, got %v", err)
	}
}

// TestValidate_TemporalAcceptsValidComparison verifies that a well-formed
// comparison operator with an offset-bearing RFC3339 operand against a
// temporal meta field is accepted.
func TestValidate_TemporalAcceptsValidComparison(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_THAN", Value: "2021-01-01T00:00:00Z"}
	if err := validateLifecycleType(c); err != nil {
		t.Errorf("valid comparison on creationDate should be accepted, got %v", err)
	}
}

// TestValidate_TemporalBetweenValidOperands verifies that BETWEEN with two
// offset-bearing RFC3339 operands against a temporal meta field is accepted.
func TestValidate_TemporalBetweenValidOperands(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field: "lastUpdateTime", OperatorType: "BETWEEN",
		Value: []any{"2021-01-01T00:00:00Z", "2021-12-31T00:00:00Z"},
	}
	if err := validateLifecycleType(c); err != nil {
		t.Errorf("valid BETWEEN on lastUpdateTime should be accepted, got %v", err)
	}
}

// TestValidate_TemporalBetweenRejectsBadOperand verifies that BETWEEN with
// one bad operand against a temporal meta field is rejected.
func TestValidate_TemporalBetweenRejectsBadOperand(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field: "lastUpdateTime", OperatorType: "BETWEEN",
		Value: []any{"2021-01-01T00:00:00Z", "not-a-date"},
	}
	if err := validateLifecycleType(c); !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("BETWEEN with a bad operand on lastUpdateTime should be CONDITION_TYPE_MISMATCH, got %v", err)
	}
}

// TestValidate_TemporalIsNullSkipsOperandCheck verifies that IS_NULL/NOT_NULL
// on a temporal meta field skip operand validation (the value is unused).
func TestValidate_TemporalIsNullSkipsOperandCheck(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "IS_NULL", Value: nil}
	if err := validateLifecycleType(c); err != nil {
		t.Errorf("IS_NULL on creationDate should be accepted regardless of operand, got %v", err)
	}
}

// TestValidate_NonTemporalMetaFieldSkipsTemporalChecks verifies that a
// non-temporal meta field (state) is not subject to the comparison-op /
// RFC3339-operand constraints — any operator and operand are accepted.
func TestValidate_NonTemporalMetaFieldSkipsTemporalChecks(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "state", OperatorType: "CONTAINS", Value: "ACT"}
	if err := validateLifecycleType(c); err != nil {
		t.Errorf("CONTAINS on non-temporal meta field state should be accepted, got %v", err)
	}
}

// TestValidate_PreviousTransitionAliasKnown verifies that the
// previousTransition alias is recognized as a known meta filter field (it
// canonicalizes to transitionForLatestSave, a non-temporal field).
func TestValidate_PreviousTransitionAliasKnown(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "previousTransition", OperatorType: "EQUALS", Value: "SUBMIT"}
	if err := validateLifecycleType(c); err != nil {
		t.Errorf("previousTransition alias should be a known field, got %v", err)
	}
}

// TestValidate_UnknownMetaField verifies that an unrecognized meta field
// name is rejected via errInvalidFieldPath (mapped by the handler to 400
// INVALID_FIELD_PATH).
func TestValidate_UnknownMetaField(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "bogus", OperatorType: "EQUALS", Value: "x"}
	err := validateLifecycleType(c)
	if err == nil {
		t.Fatal("unknown meta field must be rejected")
	}
	if !errors.Is(err, errInvalidFieldPath) {
		t.Errorf("expected errInvalidFieldPath sentinel, got: %v", err)
	}
}

// TestValidate_WalkConditionTypes_LifecycleNoLongerExempt verifies that
// walkConditionTypes (invoked via ValidateConditionValueTypes) now routes
// LifecycleCondition through validateLifecycleType instead of the old
// blanket exemption.
func TestValidate_WalkConditionTypes_LifecycleNoLongerExempt(t *testing.T) {
	model := buildDoubleModel()
	cond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2021"}
	err := ValidateConditionValueTypes(model, cond)
	if !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("expected errConditionTypeMismatch through ValidateConditionValueTypes, got %v", err)
	}
}
