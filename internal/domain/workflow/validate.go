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

// validExecutionModes is the set of accepted ExecutionMode values for
// import-time validation (audit §H4). Empty string is also accepted —
// the engine defaults to SYNC when ExecutionMode is unset.
var validExecutionModes = map[string]struct{}{
	"":                                {},
	ExecutionModeSync:                 {},
	ExecutionModeAsyncSameTx:          {},
	ExecutionModeAsyncNewTx:           {},
	ExecutionModeCommitBeforeDispatch: {},
}

// maxIdentifierLen caps the length of workflow / state / transition /
// processor names accepted at import time. The cap is defence-in-depth
// against (a) log-volume amplification via huge identifiers reflected
// into operational log lines and 4xx response bodies, and (b)
// unbounded state-machine identifiers leaking into engine audit events.
//
// 256 was chosen to align with common identifier conventions and to
// stay well above any realistic legitimate use: pre-existing OpenAPI
// patterns in this codebase cap entity field names at 100 and free-text
// descriptions at 1024, so 256 sits in the natural midpoint for
// human-meaningful identifier strings.
const maxIdentifierLen = 256

// validateImportRequest enforces the per-incoming-workflow structural
// rules (audit §H4, §H6.a–e, §M4). Violations are returned as plain
// errors that the caller wraps in a 400 with ErrCodeValidationFailed.
//
// Rules enforced:
//   - H6.c — workflow Name must be non-empty.
//   - H6.d — workflow Names must be unique within the validated slice.
//   - H6.a — InitialState must be non-empty and ∈ States.
//   - H6.b — every Transition.Next must be ∈ States.
//   - H6.e — Transition Names must be unique within a single state.
//   - H4  — ExecutionMode must be one of SYNC, ASYNC_SAME_TX,
//     ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, or empty (defaults to SYNC).
//
// Scope: called only on the **incoming** import request. Legacy stored
// workflows are not retroactively re-checked against these rules, so an
// in-place upgrade does not invalidate previously-imported shapes that
// happened to slip past weaker pre-v0.8.0 validation. The behavioural
// invariants the engine actually relies on at runtime (unguarded loops,
// flag coherence) are enforced separately by validateWorkflows on the
// post-merge result — see that function's doc for the contrast.
//
// All error messages name the offending workflow/state/transition so
// operators can locate the problem without re-reading the import body.
func validateImportRequest(workflows []spi.WorkflowDefinition) error {
	// H6.d — workflow Name uniqueness within the request. The empty-name
	// case is handled below by validateWorkflowStructure (H6.c) so that
	// a single empty-named workflow surfaces as "empty name" rather than
	// the less actionable "duplicate empty name".
	seen := make(map[string]struct{}, len(workflows))
	for _, wf := range workflows {
		if _, dup := seen[wf.Name]; dup && wf.Name != "" {
			return fmt.Errorf("duplicate workflow name %q in request", wf.Name)
		}
		seen[wf.Name] = struct{}{}
	}

	for _, wf := range workflows {
		if err := validateWorkflowStructure(wf); err != nil {
			return err
		}
	}
	return nil
}

// validateWorkflows enforces the behavioural invariants that the engine
// relies on at runtime: definite infinite loops (unguarded automated
// cycles) and StartNewTxOnDispatch / COMMIT_BEFORE_DISPATCH flag
// coherence. Called on the post-merge result so that a legacy stored
// cycle or incoherent flag in MERGE/ACTIVATE mode still surfaces at
// import time (pre-v0.8.0 behaviour preserved for these checks).
//
// The newer structural rules (state graph, name uniqueness,
// ExecutionMode enum) deliberately do NOT run here — see
// validateImportRequest for why.
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

// validateWorkflowStructure enforces the per-workflow structural rules
// (H6.a–e, H4) plus the security-audit follow-ups M-1 (empty state-map
// keys), L-1 (empty transition / processor names) and L-2 (identifier
// length cap). Any violation is a 4xx at import time —
// the engine would otherwise silently degrade at runtime (park entity
// in an undefined state, shadow duplicate transitions, coerce typo'd
// ExecutionMode to SYNC) or accept arbitrarily long identifiers into
// operational logs and audit events.
func validateWorkflowStructure(wf spi.WorkflowDefinition) error {
	// H6.c — Name non-empty.
	if wf.Name == "" {
		return fmt.Errorf("workflow with empty name is not allowed")
	}
	// L-2 — Name length cap.
	if len(wf.Name) > maxIdentifierLen {
		return fmt.Errorf("workflow name length %d exceeds the %d-char limit",
			len(wf.Name), maxIdentifierLen)
	}

	// H6.a — InitialState non-empty and ∈ States.
	if wf.InitialState == "" {
		return fmt.Errorf("workflow %q: initialState must not be empty", wf.Name)
	}
	if _, ok := wf.States[wf.InitialState]; !ok {
		return fmt.Errorf("workflow %q: initialState %q is not declared in states", wf.Name, wf.InitialState)
	}

	// Iterate states once and enforce:
	//   M-1 — state-map keys must be non-empty.
	//   L-2 — state-map keys length cap.
	//   H6.b — Transition.Next ∈ States.
	//   H6.e — Transition Name unique within state.
	//   L-1 — Transition Name non-empty.
	//   L-2 — Transition Name length cap.
	//   L-1 — Processor Name non-empty.
	//   L-2 — Processor Name length cap.
	//   H4  — ExecutionMode ∈ {SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, ""}.
	for stateName, stateDef := range wf.States {
		if stateName == "" {
			return fmt.Errorf("workflow %q: empty state name is not allowed in the states map",
				wf.Name)
		}
		if len(stateName) > maxIdentifierLen {
			return fmt.Errorf("workflow %q: state name length %d exceeds the %d-char limit",
				wf.Name, len(stateName), maxIdentifierLen)
		}

		trNames := make(map[string]struct{}, len(stateDef.Transitions))
		for _, tr := range stateDef.Transitions {
			if tr.Name == "" {
				return fmt.Errorf("workflow %q state %q: empty transition name is not allowed",
					wf.Name, stateName)
			}
			if len(tr.Name) > maxIdentifierLen {
				return fmt.Errorf("workflow %q state %q: transition name length %d exceeds the %d-char limit",
					wf.Name, stateName, len(tr.Name), maxIdentifierLen)
			}
			if _, dup := trNames[tr.Name]; dup {
				return fmt.Errorf("workflow %q state %q: duplicate transition name %q",
					wf.Name, stateName, tr.Name)
			}
			trNames[tr.Name] = struct{}{}

			if _, ok := wf.States[tr.Next]; !ok {
				return fmt.Errorf("workflow %q state %q transition %q: next state %q is not declared in states",
					wf.Name, stateName, tr.Name, tr.Next)
			}

			for _, p := range tr.Processors {
				if p.Name == "" {
					return fmt.Errorf("workflow %q state %q transition %q: empty processor name is not allowed",
						wf.Name, stateName, tr.Name)
				}
				if len(p.Name) > maxIdentifierLen {
					return fmt.Errorf("workflow %q state %q transition %q: processor name length %d exceeds the %d-char limit",
						wf.Name, stateName, tr.Name, len(p.Name), maxIdentifierLen)
				}
				if _, ok := validExecutionModes[p.ExecutionMode]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown executionMode %q (allowed: SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.ExecutionMode)
				}
			}
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
