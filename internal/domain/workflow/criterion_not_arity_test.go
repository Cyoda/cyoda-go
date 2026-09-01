package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Task 12 of the NOT-node plan: import-time acceptance of a well-formed
// NOT criterion, and rejection of a malformed one, via the same
// validateCriterion -> search.ValidateCriterionCondition path
// criterion_operator_test.go already pins for an unknown operatorType.
//
// The arity check deliberately lives in search.ValidateCriterionCondition,
// not in predicate.ParseCondition: validateCriterion returns nil (accepts)
// whenever ParseCondition itself fails, so a parser-level arity check would
// never run for a malformed NOT and the criterion would import 200 anyway —
// then fail every subsequent evaluation of that transition, permanently,
// with nothing surfaced at import to explain why. Routing the check through
// ValidateCriterionCondition instead means a bad-arity NOT is caught here,
// at the only boundary a criterion crosses.

func groupNotCriterion(conditions string) json.RawMessage {
	return json.RawMessage(`{"type":"group","operator":"NOT","conditions":[` + conditions + `]}`)
}

const oneSimpleCondition = `{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":1}`
const twoSimpleConditions = oneSimpleCondition + `,` +
	`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"}`

func TestValidateImportRequest_AcceptsCriterionNOT_ExactlyOneCondition(t *testing.T) {
	wf := wfWithTransitionCriterion(groupNotCriterion(oneSimpleCondition))
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("NOT with exactly one condition must be accepted at import: %v", err)
	}
}

func TestValidateImportRequest_RejectsCriterionNOT_ZeroConditions(t *testing.T) {
	wf := wfWithTransitionCriterion(groupNotCriterion(""))
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("NOT with zero conditions must be rejected at import")
	}
	if !strings.Contains(err.Error(), "wf-regex") || !strings.Contains(err.Error(), "go") {
		t.Errorf("error must name the offending workflow/transition, got: %v", err)
	}
}

func TestValidateImportRequest_RejectsCriterionNOT_TwoConditions(t *testing.T) {
	wf := wfWithTransitionCriterion(groupNotCriterion(twoSimpleConditions))
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("NOT with two conditions must be rejected at import")
	}
}

// Workflow-level (not just transition-level) NOT criteria are checked too —
// mirrors TestValidateImportRequest_RejectsUnknownCriterionOperator_WorkflowLevel.
func TestValidateImportRequest_RejectsCriterionNOT_ZeroConditions_WorkflowLevel(t *testing.T) {
	wf := wfWithWorkflowCriterion(groupNotCriterion(""))
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("NOT with zero conditions must be rejected at import (workflow-level criterion)")
	}
}

// A NOT nested inside an AND/OR group with bad arity is caught too — the
// walker must find it at any depth.
func TestValidateImportRequest_RejectsCriterionNOT_NestedBadArity(t *testing.T) {
	criterion := json.RawMessage(`{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"},
			{"type":"group","operator":"NOT","conditions":[` + twoSimpleConditions + `]}
		]
	}`)
	wf := wfWithTransitionCriterion(criterion)
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("a nested NOT with bad arity must be rejected at import")
	}
}
