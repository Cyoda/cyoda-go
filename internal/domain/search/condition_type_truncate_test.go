package search

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// TestValidateSimpleConditionType_MismatchTruncatesLargeOperand is the M2(b)
// security-review regression. validateSimpleConditionType's mismatch messages
// interpolated the raw operand with %v and no bound, so a single oversized
// operand (search request bodies are capped at 10 MiB, far larger than any
// sane echoed value) could blow a 400 CONDITION_TYPE_MISMATCH response body up
// to request size. The operand must be truncated the way the SPI kernel's own
// truncateOperand convention does (eval_leaf.go), while the message still
// names the offending field.
func TestValidateSimpleConditionType_MismatchTruncatesLargeOperand(t *testing.T) {
	model := buildDoubleModel()
	bigOperand := strings.Repeat("x", 1024*1024) // 1 MiB, parses into no DOUBLE

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "EQUALS",
		Value:        bigOperand,
	}

	err := ValidateConditionValueTypes(model.FieldsMap(), cond)
	if err == nil {
		t.Fatal("want a condition type mismatch error for a non-numeric operand on a DOUBLE field")
	}
	msg := err.Error()
	if len(msg) >= 512 {
		t.Errorf("message length = %d, want it bounded (<512 bytes) despite a 1 MiB operand", len(msg))
	}
	if !strings.Contains(msg, `"$.price"`) {
		t.Errorf("message = %q, want it to still name the field", msg)
	}
}

// TestValidateSimpleConditionType_ArrayMismatchTruncatesLargeOperand covers
// the sibling array-element branch (BETWEEN-style values), which carries the
// identical unbounded %v interpolation.
func TestValidateSimpleConditionType_ArrayMismatchTruncatesLargeOperand(t *testing.T) {
	model := buildDoubleModel()
	bigOperand := strings.Repeat("y", 1024*1024)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.price",
		OperatorType: "BETWEEN",
		Value:        []any{bigOperand, "irrelevant"},
	}

	err := ValidateConditionValueTypes(model.FieldsMap(), cond)
	if err == nil {
		t.Fatal("want a condition type mismatch error for a non-numeric BETWEEN bound on a DOUBLE field")
	}
	msg := err.Error()
	if len(msg) >= 512 {
		t.Errorf("message length = %d, want it bounded (<512 bytes) despite a 1 MiB operand", len(msg))
	}
	if !strings.Contains(msg, `"$.price"`) {
		t.Errorf("message = %q, want it to still name the field", msg)
	}
}

// TestValidateLifecycleType_MismatchTruncatesLargeOperand covers the sibling
// temporal meta-field mismatch, discovered during this review to carry the
// same unbounded %v interpolation as the two data-field sites above.
func TestValidateLifecycleType_MismatchTruncatesLargeOperand(t *testing.T) {
	bigOperand := strings.Repeat("z", 1024*1024)
	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "EQUALS",
		Value:        bigOperand,
	}

	err := ValidateLifecycleCondition(cond)
	if err == nil {
		t.Fatal("want a condition type mismatch error for a non-temporal operand on creationDate")
	}
	msg := err.Error()
	if len(msg) >= 512 {
		t.Errorf("message length = %d, want it bounded (<512 bytes) despite a 1 MiB operand", len(msg))
	}
	if !strings.Contains(msg, `"creationDate"`) {
		t.Errorf("message = %q, want it to still name the field", msg)
	}
}
