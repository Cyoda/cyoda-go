package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Tests for import-time rejection of an unknown criterion operatorType.
//
// A criterion is never re-validated after import (validate.go's own doc:
// "Import is the only boundary a criterion crosses"). Before this
// validator, an unknown operatorType imported cleanly — walkCriterion only
// checked jsonPath grammar and lifecycle type-soundness, never the operator
// — and the transition it guards then silently never fires, because
// match.Prepare's leaf expansion rejects the unknown operator at evaluation
// time on every subsequent save. That is the failure mode with no result
// page to look wrong: the workflow simply stalls.
//
// operator-semantics.md §4: "An operator name outside this set is 400
// INVALID_CONDITION, on every surface that carries a condition, workflow
// import included."

func TestValidateImportRequest_RejectsUnknownCriterionOperator(t *testing.T) {
	criterion := json.RawMessage(`{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_EQUALS","value":1}`)
	wf := wfWithTransitionCriterion(criterion)
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("unknown criterion operator must be rejected at import")
	}
	if !strings.Contains(err.Error(), "NOT_EQUALS") {
		t.Errorf("detail must name the offending operator: %v", err)
	}
	if !strings.Contains(err.Error(), "wf-regex") || !strings.Contains(err.Error(), "go") {
		t.Errorf("error must name the offending workflow/transition, got: %v", err)
	}
}

// A workflow-level criterion carrying an unknown operator must be rejected
// too — the same surface wfWithWorkflowCriterion exercises for the regex
// checks in criterion_regex_test.go.
func TestValidateImportRequest_RejectsUnknownCriterionOperator_WorkflowLevel(t *testing.T) {
	criterion := json.RawMessage(`{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_EQUALS","value":1}`)
	wf := wfWithWorkflowCriterion(criterion)
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("unknown criterion operator must be rejected at import")
	}
	if !strings.Contains(err.Error(), "NOT_EQUALS") {
		t.Errorf("detail must name the offending operator: %v", err)
	}
}

// An unknown operator nested inside a GROUP condition is rejected too.
func TestValidateImportRequest_RejectsUnknownCriterionOperatorNestedInGroup(t *testing.T) {
	criterion := json.RawMessage(`{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"},
			{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_EQUALS","value":1}
		]
	}`)
	wf := wfWithTransitionCriterion(criterion)
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("unknown criterion operator nested in a group must be rejected at import")
	}
}

// A malformed BETWEEN arity in a criterion is rejected too — the operand
// checks travel with the operator check through the same shared boundary.
func TestValidateImportRequest_RejectsMalformedBetweenArityInCriterion(t *testing.T) {
	criterion := json.RawMessage(`{"type":"simple","jsonPath":"$.amount","operatorType":"BETWEEN","value":[1,2,3]}`)
	wf := wfWithTransitionCriterion(criterion)
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("malformed BETWEEN arity in a criterion must be rejected at import")
	}
}

// A FUNCTION criterion legitimately carries no operator and must stay
// accepted — the operator check must not newly reject the shape
// criterion_regex_test.go's FunctionCriterionSkipped test already pins for
// the pattern/path validators.
func TestValidateImportRequest_FunctionCriterionOperatorCheckSkipped(t *testing.T) {
	wf := wfWithTransitionCriterion(functionCriterion())
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("FUNCTION criterion must not be rejected by operator validation: %v", err)
	}
}

// A criterion using a canonical operator is unaffected.
func TestValidateImportRequest_KnownCriterionOperatorAccepted(t *testing.T) {
	criterion := json.RawMessage(`{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_EQUAL","value":1}`)
	wf := wfWithTransitionCriterion(criterion)
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("a known operator must be accepted: %v", err)
	}
}
