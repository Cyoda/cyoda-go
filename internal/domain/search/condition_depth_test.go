package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// buildNestedGroupCondition constructs a chain of GroupConditions each
// containing one child, terminated by a SimpleCondition. The total nesting
// depth is `depth` (0 = bare SimpleCondition).
func buildNestedGroupCondition(depth int) predicate.Condition {
	var cur predicate.Condition = &predicate.SimpleCondition{
		JsonPath:     "$.x",
		OperatorType: "EQUALS",
		Value:        "v",
	}
	for i := 0; i < depth; i++ {
		cur = &predicate.GroupCondition{
			Operator:   "AND",
			Conditions: []predicate.Condition{cur},
		}
	}
	return cur
}

// buildNestedNotCondition constructs a chain of single-child NOT
// GroupConditions, terminated by a SimpleCondition. The total nesting depth
// is `depth` (0 = bare SimpleCondition) — the NOT-only counterpart of
// buildNestedGroupCondition, which builds AND chains exclusively and so
// never exercised whether a NOT node counts against the depth cap the same
// way an AND/OR node does.
func buildNestedNotCondition(depth int) predicate.Condition {
	var cur predicate.Condition = &predicate.SimpleCondition{
		JsonPath:     "$.x",
		OperatorType: "EQUALS",
		Value:        "v",
	}
	for i := 0; i < depth; i++ {
		cur = &predicate.GroupCondition{
			Operator:   "NOT",
			Conditions: []predicate.Condition{cur},
		}
	}
	return cur
}

// TestValidateCondition_RejectsExcessivelyDeepNotGroup confirms a NOT chain
// counts one level per node against MaxConditionDepth exactly like an AND
// chain does, in the operator validator (operators.go).
func TestValidateCondition_RejectsExcessivelyDeepNotGroup(t *testing.T) {
	const tooDeep = 1000
	if tooDeep <= MaxConditionDepth {
		t.Fatalf("test invariant: tooDeep (%d) must exceed MaxConditionDepth (%d)", tooDeep, MaxConditionDepth)
	}

	cond := buildNestedNotCondition(tooDeep)

	err := ValidateCondition(cond)
	if err == nil {
		t.Fatalf("expected depth-exceeded error for %d-level NOT chain, got nil", tooDeep)
	}
	if !strings.Contains(err.Error(), "condition depth exceeded") {
		t.Errorf("expected error mentioning 'condition depth exceeded', got: %v", err)
	}
}

// TestValidateCondition_AcceptsNotBoundary confirms a NOT chain at exactly
// MaxConditionDepth-1 (the deepest legal nesting) still validates.
func TestValidateCondition_AcceptsNotBoundary(t *testing.T) {
	cond := buildNestedNotCondition(MaxConditionDepth - 1)
	if err := ValidateCondition(cond); err != nil {
		t.Errorf("expected NOT chain at depth %d (= MaxConditionDepth-1) to validate cleanly, got: %v", MaxConditionDepth-1, err)
	}
}

// TestValidateConditionValueTypes_RejectsExcessivelyDeepNotGroup confirms the
// type-checking walker (condition_type_validate.go) also caps recursion
// through a NOT chain, not just an AND chain.
func TestValidateConditionValueTypes_RejectsExcessivelyDeepNotGroup(t *testing.T) {
	const tooDeep = 1000
	if tooDeep <= MaxConditionDepth {
		t.Fatalf("test invariant: tooDeep (%d) must exceed MaxConditionDepth (%d)", tooDeep, MaxConditionDepth)
	}

	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.String))
	cond := buildNestedNotCondition(tooDeep)

	err := ValidateConditionValueTypes(model, cond)
	if err == nil {
		t.Fatalf("expected depth-exceeded error for %d-level NOT chain, got nil", tooDeep)
	}
	if !strings.Contains(err.Error(), "condition depth exceeded") {
		t.Errorf("expected error mentioning 'condition depth exceeded', got: %v", err)
	}
	if errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("depth-exceeded error must not satisfy errConditionTypeMismatch sentinel: %v", err)
	}
}

// TestValidateConditionValueTypes_AcceptsNotBoundary confirms a NOT chain at
// exactly MaxConditionDepth-1 still type-checks cleanly.
func TestValidateConditionValueTypes_AcceptsNotBoundary(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.String))
	cond := buildNestedNotCondition(MaxConditionDepth - 1)
	if err := ValidateConditionValueTypes(model, cond); err != nil {
		t.Errorf("expected NOT chain at depth %d (= MaxConditionDepth-1) to validate cleanly, got: %v", MaxConditionDepth-1, err)
	}
}

// TestValidateCondition_RejectsExcessivelyDeepGroup confirms the operator
// validator (operators.go) terminates with a depth-exceeded error rather
// than recursing without bound when the predicate tree nests deeper than
// MaxConditionDepth. Programmatic call sites (workflow engine criteria,
// internal constructions) bypass the parser's depth cap, so the walker
// itself must guard.
func TestValidateCondition_RejectsExcessivelyDeepGroup(t *testing.T) {
	const tooDeep = 1000
	if tooDeep <= MaxConditionDepth {
		t.Fatalf("test invariant: tooDeep (%d) must exceed MaxConditionDepth (%d)", tooDeep, MaxConditionDepth)
	}

	cond := buildNestedGroupCondition(tooDeep)

	err := ValidateCondition(cond)
	if err == nil {
		t.Fatalf("expected depth-exceeded error for %d-level group, got nil", tooDeep)
	}
	if !strings.Contains(err.Error(), "condition depth exceeded") {
		t.Errorf("expected error mentioning 'condition depth exceeded', got: %v", err)
	}
}

// TestValidateCondition_AcceptsBoundary confirms a predicate at exactly
// MaxConditionDepth-1 (the deepest legal nesting) still validates.
func TestValidateCondition_AcceptsBoundary(t *testing.T) {
	cond := buildNestedGroupCondition(MaxConditionDepth - 1)
	if err := ValidateCondition(cond); err != nil {
		t.Errorf("expected predicate at depth %d (= MaxConditionDepth-1) to validate cleanly, got: %v", MaxConditionDepth-1, err)
	}
}

// TestValidateConditionValueTypes_RejectsExcessivelyDeepGroup confirms the
// type-checking walker (condition_type_validate.go) also caps recursion.
// Same defense-in-depth rationale: callers other than the HTTP parser may
// supply arbitrarily-nested predicates.
func TestValidateConditionValueTypes_RejectsExcessivelyDeepGroup(t *testing.T) {
	const tooDeep = 1000
	if tooDeep <= MaxConditionDepth {
		t.Fatalf("test invariant: tooDeep (%d) must exceed MaxConditionDepth (%d)", tooDeep, MaxConditionDepth)
	}

	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.String))
	cond := buildNestedGroupCondition(tooDeep)

	err := ValidateConditionValueTypes(model, cond)
	if err == nil {
		t.Fatalf("expected depth-exceeded error for %d-level group, got nil", tooDeep)
	}
	if !strings.Contains(err.Error(), "condition depth exceeded") {
		t.Errorf("expected error mentioning 'condition depth exceeded', got: %v", err)
	}
	// The depth-exceeded error must not impersonate a type mismatch — that
	// would route through the wrong error code in the handler.
	if errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("depth-exceeded error must not satisfy errConditionTypeMismatch sentinel: %v", err)
	}
}

// TestValidateConditionValueTypes_AcceptsBoundary confirms a predicate at
// exactly MaxConditionDepth-1 still type-checks cleanly.
func TestValidateConditionValueTypes_AcceptsBoundary(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.String))
	cond := buildNestedGroupCondition(MaxConditionDepth - 1)
	if err := ValidateConditionValueTypes(model, cond); err != nil {
		t.Errorf("expected predicate at depth %d (= MaxConditionDepth-1) to validate cleanly, got: %v", MaxConditionDepth-1, err)
	}
}
