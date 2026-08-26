package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// Import-time pattern validation derives its accept/reject set from the
// kernel's own derivation rather than compiling the operand bare. Two classes
// of criterion were mis-handled before that:
//
//   - an operand that compiles standalone but not once the kernel anchors it
//     (an unterminated `\Q` swallows the appended `)\z`) imported cleanly and
//     then failed on every evaluation of the transition;
//   - a malformed LIKE operand was never checked at all, so it imported
//     cleanly and became a leaf that silently never matched — a guarded
//     transition that could never fire.

// simplePatternCriterion builds a criterion for op carrying a raw (already
// JSON-escaped) operand.
func simplePatternCriterion(op, escapedValue string) json.RawMessage {
	return json.RawMessage(`{"type":"simple","jsonPath":"$.orderId","operatorType":"` + op + `","value":"` + escapedValue + `"}`)
}

// (a) an operand that only fails once anchored is rejected at import.
func TestValidateWorkflowStructure_RejectsAnchorSkewPatternInCriterion(t *testing.T) {
	wf := wfWithTransitionCriterion(simplePatternCriterion("MATCHES_PATTERN", `\\Q`))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatal(`expected error for MATCHES_PATTERN operand "\Q", got nil`)
	}
	if !strings.Contains(err.Error(), "wf-regex") || !strings.Contains(err.Error(), "go") {
		t.Errorf("error must name the offending workflow/transition, got: %v", err)
	}
}

// (b) an operand whose parens rebalance only under the anchor wrapper is
// rejected — `)|(` compiles as `\A(?:)|()\z` and matches every stored value.
func TestValidateWorkflowStructure_RejectsUnbalancedParenPatternInCriterion(t *testing.T) {
	wf := wfWithTransitionCriterion(simplePatternCriterion("MATCHES_PATTERN", `)|(`))
	if err := validateWorkflowStructure(wf); err == nil {
		t.Fatal(`expected error for MATCHES_PATTERN operand ")|(", got nil`)
	}
}

// (c) a malformed LIKE operand — a trailing unpaired escape — is rejected at
// import, where before it was not validated at all.
func TestValidateWorkflowStructure_RejectsMalformedLikeInTransitionCriterion(t *testing.T) {
	wf := wfWithTransitionCriterion(simplePatternCriterion("LIKE", `abc\\`))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatal(`expected error for LIKE operand "abc\", got nil`)
	}
	if !strings.Contains(err.Error(), "wf-regex") || !strings.Contains(err.Error(), "go") {
		t.Errorf("error must name the offending workflow/transition, got: %v", err)
	}
}

// (d) the same for a workflow-level criterion.
func TestValidateWorkflowStructure_RejectsMalformedLikeInWorkflowCriterion(t *testing.T) {
	wf := wfWithWorkflowCriterion(simplePatternCriterion("LIKE", `abc\\`))
	err := validateWorkflowStructure(wf)
	if err == nil {
		t.Fatal(`expected error for LIKE operand "abc\", got nil`)
	}
	if !strings.Contains(err.Error(), "wf-regex") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// (e) a malformed LIKE nested in a group is found too.
func TestValidateWorkflowStructure_RejectsMalformedLikeNestedInGroup(t *testing.T) {
	criterion := json.RawMessage(`{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"},
			{"type":"simple","jsonPath":"$.orderId","operatorType":"LIKE","value":"abc\\"}
		]
	}`)
	wf := wfWithTransitionCriterion(criterion)
	if err := validateWorkflowStructure(wf); err == nil {
		t.Fatal("expected error for malformed LIKE nested in a group condition, got nil")
	}
}

// (f) a lifecycle leaf carries the identical exposure and is validated too.
func TestValidateWorkflowStructure_RejectsMalformedLikeInLifecycleCriterion(t *testing.T) {
	criterion := json.RawMessage(`{"type":"lifecycle","field":"state","operatorType":"LIKE","value":"abc\\"}`)
	wf := wfWithTransitionCriterion(criterion)
	if err := validateWorkflowStructure(wf); err == nil {
		t.Fatal("expected error for malformed LIKE on a lifecycle criterion, got nil")
	}
}

// (g) accept side: a well-formed LIKE glob still imports.
func TestValidateWorkflowStructure_ValidLikeCriterionAccepted(t *testing.T) {
	for name, escaped := range map[string]string{
		"wildcards":    `ORD-%`,
		"singleChar":   `ORD-_`,
		"pairedEscape": `50\\%`,
	} {
		t.Run(name, func(t *testing.T) {
			wf := wfWithTransitionCriterion(simplePatternCriterion("LIKE", escaped))
			if err := validateWorkflowStructure(wf); err != nil {
				t.Fatalf("valid LIKE criterion must be accepted: %v", err)
			}
		})
	}
}
