package workflow

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Tests for the structural validator rules added in issue #255 (audit
// §H4, §H6, §M4). Each sub-test pins a single rule so a regression is
// localised to the rule it breaks.
//
// Conventions:
//   - All "good" baselines use distinct state names so a single-rule
//     violation can be introduced without colliding with another rule.
//   - Error strings are asserted on enough substrings to confirm the
//     correct rule fired and that the message names the offending
//     workflow/state/transition (per the issue's "name the offender"
//     acceptance criterion).

// --- H6.c — Workflow Name non-empty ----------------------------------

func TestValidateWorkflows_RejectsEmptyWorkflowName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for empty workflow name, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error must mention the name rule, got: %v", err)
	}
}

// --- H6.d / M4 — Workflow Name unique within request ------------------

func TestValidateWorkflows_RejectsDuplicateWorkflowNamesInRequest(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "dup-wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf, wf})
	if err == nil {
		t.Fatalf("expected error for duplicate workflow names, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error must mention duplicate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dup-wf") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// --- H6.a — InitialState non-empty and ∈ States -----------------------

func TestValidateWorkflows_RejectsEmptyInitialState(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-empty-initial", InitialState: "", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for empty initialState, got nil")
	}
	if !strings.Contains(err.Error(), "initialState") {
		t.Errorf("error must mention initialState, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wf-empty-initial") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

func TestValidateWorkflows_RejectsInitialStateNotInStates(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-bad-initial", InitialState: "MISSING", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for initialState not in States, got nil")
	}
	if !strings.Contains(err.Error(), "initialState") {
		t.Errorf("error must mention initialState, got: %v", err)
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error must name the offending state %q, got: %v", "MISSING", err)
	}
	if !strings.Contains(err.Error(), "wf-bad-initial") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// --- H6.b — Transition.Next ∈ States ----------------------------------

func TestValidateWorkflows_RejectsTransitionNextNotInStates(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-bad-next", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "NOWHERE", Manual: true},
			}},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for transition Next not in States, got nil")
	}
	if !strings.Contains(err.Error(), "next") {
		t.Errorf("error must mention next, got: %v", err)
	}
	if !strings.Contains(err.Error(), "NOWHERE") {
		t.Errorf("error must name the offending Next %q, got: %v", "NOWHERE", err)
	}
	if !strings.Contains(err.Error(), `"go"`) {
		t.Errorf("error must name the offending transition, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wf-bad-next") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// --- H6.e — Transition Name unique within state -----------------------

func TestValidateWorkflows_RejectsDuplicateTransitionNameWithinState(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-dup-trans", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S2", Manual: true},
				{Name: "go", Next: "S3", Manual: true},
			}},
			"S2": {},
			"S3": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for duplicate transition name within state, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error must mention duplicate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "S1") {
		t.Errorf("error must name the offending state, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"go"`) {
		t.Errorf("error must name the duplicated transition, got: %v", err)
	}
}

// Different states sharing a transition name is fine (the audit only
// flags duplicates within a single state).
func TestValidateWorkflows_AcceptsSameTransitionNameAcrossStates(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-cross-state-name", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S2", Manual: true},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S3", Manual: true},
			}},
			"S3": {},
		},
	}
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error for same-name transitions in different states, got: %v", err)
	}
}

// --- H4 — ExecutionMode enum check ------------------------------------

func TestValidateWorkflows_RejectsUnknownExecutionMode(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-bad-mode", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: "ASYN_SAME_TX"},
				}},
			}},
			"S2": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for typo'd ExecutionMode, got nil")
	}
	if !strings.Contains(err.Error(), "executionMode") {
		t.Errorf("error must mention executionMode, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ASYN_SAME_TX") {
		t.Errorf("error must name the offending mode, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wf-bad-mode") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// Empty ExecutionMode must remain valid — engine treats "" as SYNC.
func TestValidateWorkflows_AcceptsEmptyExecutionMode(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-empty-mode", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: ""},
				}},
			}},
			"S2": {},
		},
	}
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error for empty ExecutionMode, got: %v", err)
	}
}

// All four named modes must be accepted.
func TestValidateWorkflows_AcceptsAllKnownExecutionModes(t *testing.T) {
	for _, mode := range []string{
		ExecutionModeSync,
		ExecutionModeAsyncSameTx,
		ExecutionModeAsyncNewTx,
		ExecutionModeCommitBeforeDispatch,
	} {
		t.Run(mode, func(t *testing.T) {
			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "wf-" + mode, InitialState: "S1", Active: true,
				States: map[string]spi.StateDefinition{
					"S1": {Transitions: []spi.TransitionDefinition{
						{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
							{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: mode},
						}},
					}},
					"S2": {},
				},
			}
			if err := validateWorkflows([]spi.WorkflowDefinition{wf}); err != nil {
				t.Fatalf("expected no error for ExecutionMode=%q, got: %v", mode, err)
			}
		})
	}
}
