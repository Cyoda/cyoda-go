package workflow

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Processor execution-mode tokens. Sourced from the OpenAPI enum in
// api/openapi.yaml (mirrored in api/generated.go's
// ExternalizedProcessorDefinitionDtoExecutionMode constants). Centralised
// here as untyped strings so engine logic, validator rules, and tests can
// compare against a single source — the SPI's ExecutionMode field is itself
// a plain string, so an enum type would not buy compile-time safety.
const (
	ExecutionModeSync                 = "SYNC"
	ExecutionModeAsyncSameTx          = "ASYNC_SAME_TX"
	ExecutionModeAsyncNewTx           = "ASYNC_NEW_TX"
	ExecutionModeCommitBeforeDispatch = "COMMIT_BEFORE_DISPATCH"
)

// Processor execution-location tokens. Sourced from the OpenAPI enum in
// api/openapi.yaml (mirrored in api/generated.go's ProcessorDefinitionDto
// type constants). Centralised here as untyped strings so engine logic,
// validator rules, and tests can compare against a single source — the
// SPI's ProcessorDefinition.Type field is itself a plain string, so an
// enum type would not buy compile-time safety.
//
// Empty value is treated as ProcessorTypeExternalized at dispatch. Any
// value other than ProcessorTypeInternalized falls through to the
// ExecutionMode dispatch path at engine fire time; the only Type
// rejection performed by the engine is on the exact value
// ProcessorTypeInternalized.
const (
	ProcessorTypeExternalized = "externalized"
	ProcessorTypeInternalized = "internalized"
)

// validateWorkflows checks workflow definitions for definite infinite loops.
// A definite infinite loop exists when there is a cycle of automated transitions
// (manual=false) with NO criteria guards (nil/empty criterion = always fires).
// Transitions WITH criteria are not flagged because the criterion might return
// false and break the cycle.
func validateWorkflows(workflows []spi.WorkflowDefinition) error {
	for _, wf := range workflows {
		if err := validateWorkflowLoops(wf); err != nil {
			return fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		if err := validateProcessorFlags(wf); err != nil {
			return err
		}
	}
	return nil
}

// validateProcessorFlags rejects misuse of processor-level flags whose
// semantics are tied to a specific ExecutionMode. Currently enforces:
//   - StartNewTxOnDispatch=true is only valid with ExecutionMode=COMMIT_BEFORE_DISPATCH.
//
// The check is asymmetric: COMMIT_BEFORE_DISPATCH processors with the flag
// nil/false are equally valid (the flag just defaults to false). We never
// inspect the flag for COMMIT_BEFORE_DISPATCH processors.
func validateProcessorFlags(wf spi.WorkflowDefinition) error {
	for _, st := range wf.States {
		for _, tr := range st.Transitions {
			for _, p := range tr.Processors {
				if p.Config.StartNewTxOnDispatch != nil && *p.Config.StartNewTxOnDispatch &&
					p.ExecutionMode != ExecutionModeCommitBeforeDispatch {
					return fmt.Errorf(
						"workflow %q transition %q processor %q: startNewTxOnDispatch=true is only valid with executionMode=COMMIT_BEFORE_DISPATCH (got %q)",
						wf.Name, tr.Name, p.Name, p.ExecutionMode)
				}
			}
		}
	}
	return nil
}

// validateWorkflowLoops performs DFS cycle detection on unguarded automated
// transitions within a single workflow definition.
func validateWorkflowLoops(wf spi.WorkflowDefinition) error {
	// Build adjacency list: state -> list of target states reachable via
	// unguarded automated transitions.
	adj := make(map[string][]string)
	for stateName, stateDef := range wf.States {
		for _, tr := range stateDef.Transitions {
			if tr.Manual || tr.Disabled {
				continue
			}
			if len(tr.Criterion) > 0 && string(tr.Criterion) != "null" {
				continue // guarded — skip
			}
			adj[stateName] = append(adj[stateName], tr.Next)
		}
	}

	// DFS with three-color marking: white (unseen), gray (in stack), black (done).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	parent := make(map[string]string) // tracks the path for error messages

	var dfs func(state string) error
	dfs = func(state string) error {
		color[state] = gray
		for _, next := range adj[state] {
			switch color[next] {
			case gray:
				// Back edge — cycle found. Reconstruct path.
				path := reconstructCyclePath(parent, state, next)
				return fmt.Errorf("infinite loop detected: %s via unguarded automated transitions", path)
			case white:
				parent[next] = state
				if err := dfs(next); err != nil {
					return err
				}
			}
		}
		color[state] = black
		return nil
	}

	for stateName := range wf.States {
		if color[stateName] == white {
			if err := dfs(stateName); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconstructCyclePath builds a human-readable cycle string like "A -> B -> C -> A".
func reconstructCyclePath(parent map[string]string, tail, cycleStart string) string {
	var parts []string
	// Walk backward from tail to cycleStart.
	cur := tail
	for cur != cycleStart {
		parts = append([]string{cur}, parts...)
		cur = parent[cur]
	}
	parts = append([]string{cycleStart}, parts...)
	parts = append(parts, cycleStart)
	return strings.Join(parts, " -> ")
}
