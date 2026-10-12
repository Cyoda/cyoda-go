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

func TestValidateImportRequest_RejectsUnparseableCriterion(t *testing.T) {
	for _, c := range []string{`{"type":"bogus"}`, `[1,2]`, `{"type":"simple","jsonPath":5}`, `{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":"x"}`} {
		err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))})
		if err == nil {
			t.Errorf("%s: an unparseable criterion must be rejected at import", c)
			continue
		}
		if !strings.Contains(err.Error(), "wf-regex") {
			t.Errorf("%s: error must name the workflow, got: %v", c, err)
		}
	}
}

func TestValidateImportRequest_ArrayCriterion(t *testing.T) {
	reject := []string{
		`{"type":"array","jsonPath":"$.t[*]","values":["a"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"FROBNICATE","values":["a"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":[null]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","value":["a"]}`,
		`{"type":"array","jsonPath":"$.o[*].t[*]","operatorType":"EQUALS","values":["a"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"MATCHES_PATTERN","values":["("]}`,
	}
	for _, c := range reject {
		if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))}); err == nil {
			t.Errorf("%s: must be rejected at import", c)
		}
	}
	ok := `{"type":"array","jsonPath":"$.t[*]","operatorType":"GREATER_THAN","values":[1,null,3]}`
	if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(ok))}); err != nil {
		t.Errorf("valid array criterion rejected: %v", err)
	}
}

// An array operand is refused at import under every operator other than
// BETWEEN / BETWEEN_INCLUSIVE — on a simple, a lifecycle and an array clause.
// Imported, it would evaluate differently on each backend.
func TestValidateImportRequest_RejectsArrayOperandOutsideBetween(t *testing.T) {
	for _, c := range []string{
		`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":[1,2]}`,
		`{"type":"lifecycle","field":"state","operatorType":"NOT_EQUAL","value":["A","B"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"NOT_EQUAL","values":[["a","b"]]}`,
	} {
		err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))})
		if err == nil {
			t.Errorf("%s: an array operand outside BETWEEN must be rejected at import", c)
			continue
		}
		if !strings.Contains(err.Error(), "BETWEEN") {
			t.Errorf("%s: detail must say which operators take a pair, got: %v", c, err)
		}
	}
	ok := `{"type":"array","jsonPath":"$.t[*]","operatorType":"BETWEEN","values":[[1,5]]}`
	if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(ok))}); err != nil {
		t.Errorf("a BETWEEN pair must stay accepted: %v", err)
	}
}

// A null BETWEEN bound is refused at import, on a simple and an array clause.
func TestValidateImportRequest_RejectsNullBetweenBound(t *testing.T) {
	for _, c := range []string{
		`{"type":"simple","jsonPath":"$.amount","operatorType":"BETWEEN","value":[null,5]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"BETWEEN_INCLUSIVE","values":[[null,5]]}`,
	} {
		if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))}); err == nil {
			t.Errorf("%s: a null BETWEEN bound must be rejected at import", c)
		}
	}
}

// An object or array BETWEEN bound is refused at import, on a simple and an
// array clause. Imported, it would fail or compare against the bound's
// printed form at every evaluation.
func TestValidateImportRequest_RejectsNonScalarBetweenBound(t *testing.T) {
	for _, c := range []string{
		`{"type":"simple","jsonPath":"$.amount","operatorType":"BETWEEN","value":[{"a":1},5]}`,
		`{"type":"simple","jsonPath":"$.name","operatorType":"BETWEEN_INCLUSIVE","value":[["a"],"z"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"BETWEEN","values":[[{"a":1},"z"]]}`,
	} {
		err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))})
		if err == nil {
			t.Errorf("%s: an object or array BETWEEN bound must be rejected at import", c)
			continue
		}
		if !strings.Contains(err.Error(), "scalar") {
			t.Errorf("%s: detail must say a bound is a scalar, got: %v", c, err)
		}
	}
}
