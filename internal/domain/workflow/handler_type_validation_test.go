package workflow

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestValidator_TypeInternalized_WithStartNewTxOnDispatch asserts that
// the existing StartNewTxOnDispatch flag-coherence check (validate.go:51-55)
// fires at import time and short-circuits before any engine-time Type
// rejection can run. Two sub-cases pin the ordering.
func TestValidator_TypeInternalized_WithStartNewTxOnDispatch(t *testing.T) {
	t.Run("internalized+SYNC+startNewTxOnDispatch=true is rejected at import", func(t *testing.T) {
		trueVal := true
		wf := spi.WorkflowDefinition{
			Version: "1.1", Name: "BadFlagsWF", InitialState: "INITIAL", Active: true,
			States: map[string]spi.StateDefinition{
				"INITIAL": {Transitions: []spi.TransitionDefinition{
					{Name: "RUN", Next: "DONE", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{
								Type:          ProcessorTypeInternalized,
								Name:          "p",
								ExecutionMode: ExecutionModeSync,
								Config: spi.ProcessorConfig{
									StartNewTxOnDispatch: &trueVal,
								},
							},
						}},
				}},
				"DONE": {},
			},
		}

		err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
		if err == nil {
			t.Fatalf("expected validator to reject startNewTxOnDispatch=true with non-CBD ExecutionMode, got nil")
		}
		// Error should reference startNewTxOnDispatch — confirms it's the
		// flag-coherence check firing, NOT the Type-axis engine rejection.
		if !strings.Contains(err.Error(), "startNewTxOnDispatch") {
			t.Errorf("error = %q, want it to mention startNewTxOnDispatch (proves flag-coherence check fired, not Type-axis)", err.Error())
		}
	})

	t.Run("internalized+COMMIT_BEFORE_DISPATCH+startNewTxOnDispatch=true passes validator (Type rejection happens at engine fire only)", func(t *testing.T) {
		trueVal := true
		wf := spi.WorkflowDefinition{
			Version: "1.1", Name: "ValidFlagsWF", InitialState: "INITIAL", Active: true,
			States: map[string]spi.StateDefinition{
				"INITIAL": {Transitions: []spi.TransitionDefinition{
					{Name: "RUN", Next: "DONE", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{
								Type:          ProcessorTypeInternalized,
								Name:          "p",
								ExecutionMode: ExecutionModeCommitBeforeDispatch,
								Config: spi.ProcessorConfig{
									StartNewTxOnDispatch: &trueVal,
								},
							},
						}},
				}},
				"DONE": {},
			},
		}

		err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
		if err != nil {
			t.Fatalf("validator should pass for coherent CBD flags regardless of Type; got %v", err)
		}
		// Engine-time rejection is covered by
		// TestInternalizedRejection_ExecutionModeMatrix/ExecutionMode_COMMIT_BEFORE_DISPATCH.
	})
}
