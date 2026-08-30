package search

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// TestValidateCondition_ArrayClauseRequiresWildcard pins path-grammar.md §8:
// the clause tests elements by position, so its path must address elements.
// A bare path addresses the array itself and cannot carry a positional test —
// the spelling and the meaning disagreed.
func TestValidateCondition_ArrayClauseRequiresWildcard(t *testing.T) {
	err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags", Values: []any{"A"},
	})
	if err == nil {
		t.Fatal("array clause on a bare path must be rejected")
	}
	if !errors.Is(err, ErrInvalidFieldPath) {
		t.Errorf("want ErrInvalidFieldPath, got %v", err)
	}
}

// TestValidateCondition_ArrayClauseAcceptsWildcard is the positive control:
// a trailing wildcard is the well-formed spelling and must be accepted.
func TestValidateCondition_ArrayClauseAcceptsWildcard(t *testing.T) {
	if err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{"A", nil, "C"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestValidateCondition_ArrayClauseRejectsObjectOperand pins the operand-shape
// check the ArrayCondition arm previously skipped entirely. Unchecked, an
// object operand reaches the kernel and is compared as the literal text
// "map[a:1]".
func TestValidateCondition_ArrayClauseRejectsObjectOperand(t *testing.T) {
	err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{map[string]any{"a": 1}},
	})
	if err == nil {
		t.Fatal("array clause with an object operand must be rejected")
	}
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("want ErrInvalidCondition, got %v", err)
	}
}

// TestValidateCondition_ArrayClauseRejectsObjectOperand_NullEntriesSkipped
// proves a null entry (the clause's documented "skip this position" spelling)
// does not itself trip the operand-shape check — only a non-nil object does.
func TestValidateCondition_ArrayClauseAcceptsAllNullValues(t *testing.T) {
	if err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{nil, nil},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
