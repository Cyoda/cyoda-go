package workflow

import (
	"strings"
	"testing"
)

// Tests for import-time rejection of type-unsound lifecycle/meta criteria
// (temporal fields compared with a non-comparison operator, or a
// non-offset-RFC3339 operand on creationDate/lastUpdateTime; unknown meta
// field paths). These conditions previously imported successfully and only
// failed — or silently misbehaved — at transition-evaluation time
// (engine.go's match.Match -> matchLifecycle path). This closes that
// fail-open gap by delegating to the shared search.ValidateLifecycleCondition
// at import time, the same validator the search API boundary already uses.

// lifecycleCriterion (scenarios_test.go) already builds the
// {"type":"lifecycle","field":...,"operatorType":...,"value":...} shape
// used below.

// (a) a non-comparison operator (CONTAINS) on a temporal field is rejected.
func TestValidateWorkflowStructure_RejectsNonComparisonOperatorOnTemporalField(t *testing.T) {
	wf := wfWithTransitionCriterion(lifecycleCriterion("creationDate", "CONTAINS", "2021"))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for CONTAINS operator on temporal field creationDate, got nil")
	}
	if !strings.Contains(err.Error(), "wf-regex") || !strings.Contains(err.Error(), "go") {
		t.Errorf("error must name the offending workflow/transition, got: %v", err)
	}
}

// (b) a non-RFC3339 operand on a temporal field is rejected.
func TestValidateWorkflowStructure_RejectsNonTemporalOperandOnTemporalField(t *testing.T) {
	wf := wfWithTransitionCriterion(lifecycleCriterion("creationDate", "GREATER_THAN", "not-a-date"))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for non-timestamp operand on temporal field creationDate, got nil")
	}
}

// (c) an unknown meta field is rejected.
func TestValidateWorkflowStructure_RejectsUnknownMetaField(t *testing.T) {
	wf := wfWithTransitionCriterion(lifecycleCriterion("bogus", "EQUALS", "x"))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for unknown meta field %q, got nil", "bogus")
	}
}

// (d) a valid temporal criterion (comparison operator + offset RFC3339
// operand) is accepted — no over-rejection.
func TestValidateWorkflowStructure_ValidTemporalCriterionAccepted(t *testing.T) {
	wf := wfWithTransitionCriterion(lifecycleCriterion("creationDate", "GREATER_THAN", "2021-01-01T00:00:00Z"))
	if err := validateWorkflowStructure(wf); err != nil {
		t.Fatalf("valid temporal criterion must be accepted: %v", err)
	}
}

// (e) a valid non-temporal meta criterion (state EQUALS) is accepted —
// no over-rejection of the pre-existing string-meta comparison shape.
func TestValidateWorkflowStructure_ValidStringMetaCriterionAccepted(t *testing.T) {
	wf := wfWithTransitionCriterion(lifecycleCriterion("state", "EQUALS", "CREATED"))
	if err := validateWorkflowStructure(wf); err != nil {
		t.Fatalf("valid string-meta criterion must be accepted: %v", err)
	}
}

// (f) workflow-level lifecycle criterion is also validated, not just
// transition-level.
func TestValidateWorkflowStructure_RejectsTypeUnsoundWorkflowLevelLifecycleCriterion(t *testing.T) {
	wf := wfWithWorkflowCriterion(lifecycleCriterion("creationDate", "CONTAINS", "2021"))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for type-unsound workflow-level lifecycle criterion, got nil")
	}
	if !strings.Contains(err.Error(), "wf-regex") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}
