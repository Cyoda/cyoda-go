package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Tests for import-time rejection of malformed MATCHES_PATTERN regexes in
// workflow/transition criteria. Before this validator, a malformed regex
// (e.g. "[") imported successfully and only surfaced as an error on every
// subsequent transition evaluation (engine.go's predicate.ParseCondition ->
// match.Match path) — a fail-open gap at import time. These tests pin the
// fail-closed behaviour: reject at import instead.

const malformedPattern = "["
const validPattern = "^ORD-[0-9]+$"

func simpleMatchesPatternCriterion(pattern string) json.RawMessage {
	return json.RawMessage(`{"type":"simple","jsonPath":"$.orderId","operatorType":"MATCHES_PATTERN","value":"` + pattern + `"}`)
}

func groupWithNestedMatchesPattern(pattern string) json.RawMessage {
	return json.RawMessage(`{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"},
			{
				"type": "group",
				"operator": "OR",
				"conditions": [
					{"type":"simple","jsonPath":"$.orderId","operatorType":"MATCHES_PATTERN","value":"` + pattern + `"}
				]
			}
		]
	}`)
}

func functionCriterion() json.RawMessage {
	return json.RawMessage(`{"type":"function","function":{"name":"myFn","calculationNodesTags":"tag"}}`)
}

func wfWithTransitionCriterion(criterion json.RawMessage) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1.1", Name: "wf-regex", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{Name: "go", Next: "S2", Manual: true, Criterion: criterion},
				},
			},
			"S2": {},
		},
	}
}

func wfWithWorkflowCriterion(criterion json.RawMessage) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1.1", Name: "wf-regex", InitialState: "S1", Active: true,
		Criterion: criterion,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{Name: "go", Next: "S2", Manual: true},
				},
			},
			"S2": {},
		},
	}
}

// (a) transition-level malformed regex is rejected.
func TestValidateWorkflowStructure_RejectsMalformedRegexInTransitionCriterion(t *testing.T) {
	wf := wfWithTransitionCriterion(simpleMatchesPatternCriterion(malformedPattern))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for malformed MATCHES_PATTERN regex, got nil")
	}
	if !strings.Contains(err.Error(), "wf-regex") || !strings.Contains(err.Error(), "go") {
		t.Errorf("error must name the offending workflow/transition, got: %v", err)
	}
}

// (b) workflow-level malformed regex is rejected.
func TestValidateWorkflowStructure_RejectsMalformedRegexInWorkflowCriterion(t *testing.T) {
	wf := wfWithWorkflowCriterion(simpleMatchesPatternCriterion(malformedPattern))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for malformed MATCHES_PATTERN regex, got nil")
	}
	if !strings.Contains(err.Error(), "wf-regex") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// (c) malformed regex nested inside a GROUP (AND/OR) condition is rejected.
func TestValidateWorkflowStructure_RejectsMalformedRegexNestedInGroup(t *testing.T) {
	wf := wfWithTransitionCriterion(groupWithNestedMatchesPattern(malformedPattern))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatalf("expected error for malformed regex nested in a group condition, got nil")
	}
}

// (d) a FUNCTION criterion is skipped, not rejected.
func TestValidateWorkflowStructure_FunctionCriterionSkipped(t *testing.T) {
	wf := wfWithTransitionCriterion(functionCriterion())
	if err := validateWorkflowStructure(wf); err != nil {
		t.Fatalf("FUNCTION criterion must not be rejected by regex validation: %v", err)
	}
}

// (e) a valid, non-malformed regex (and a valid non-regex criterion) is accepted.
func TestValidateWorkflowStructure_ValidRegexAccepted(t *testing.T) {
	wf := wfWithTransitionCriterion(simpleMatchesPatternCriterion(validPattern))
	if err := validateWorkflowStructure(wf); err != nil {
		t.Fatalf("valid MATCHES_PATTERN regex must be accepted: %v", err)
	}
}

func TestValidateWorkflowStructure_ValidNonRegexCriterionAccepted(t *testing.T) {
	criterion := json.RawMessage(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"}`)
	wf := wfWithTransitionCriterion(criterion)
	if err := validateWorkflowStructure(wf); err != nil {
		t.Fatalf("valid non-regex criterion must be accepted: %v", err)
	}
}

// (f) a criterion that does not parse as a condition at all is left alone
// (not newly rejected by this change) — pre-existing behaviour preserved.
func TestValidateWorkflowStructure_UnparseableCriterionNotNewlyRejected(t *testing.T) {
	criterion := json.RawMessage(`{"type":"totally-unknown-shape"}`)
	wf := wfWithTransitionCriterion(criterion)
	if err := validateWorkflowStructure(wf); err != nil {
		t.Fatalf("unparseable criterion must not be newly rejected by regex validation: %v", err)
	}
}

// Empty / null criteria are not touched by this validator either.
func TestValidateWorkflowStructure_EmptyAndNullCriteriaAccepted(t *testing.T) {
	for name, criterion := range map[string]json.RawMessage{
		"nil":  nil,
		"null": json.RawMessage("null"),
	} {
		t.Run(name, func(t *testing.T) {
			wf := wfWithTransitionCriterion(criterion)
			if err := validateWorkflowStructure(wf); err != nil {
				t.Fatalf("empty/null criterion must be accepted: %v", err)
			}
		})
	}
}
