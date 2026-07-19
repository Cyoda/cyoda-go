package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
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

// Processor retry-policy tokens. Sourced from the workflow author's
// selector across the two server-resolved retry strategies (audit §M1).
// Centralised here as untyped strings so engine logic, validator rules,
// and tests can compare against a single source — the SPI's
// ProcessorConfig.RetryPolicy field is itself a plain string, so an enum
// type would not buy compile-time safety.
//
//   - NONE  — single attempt, no retry on member-level failure.
//   - FIXED — default when unset. Up to N additional attempts with a
//     fixed delay between tries; N and delay come from server-side
//     config and are not carried in the workflow.
//
// cyoda-go currently captures the policy at import time but does not yet
// honour it at dispatch — the dispatcher remains single-shot. The full
// retry loop is not yet implemented.
const (
	RetryPolicyNone  = "NONE"
	RetryPolicyFixed = "FIXED"
)

// validRetryPolicies is the set of accepted RetryPolicy values for
// import-time validation (audit §M1). Empty string is also accepted —
// the server defaults to FIXED when RetryPolicy is unset.
var validRetryPolicies = map[string]struct{}{
	"":               {},
	RetryPolicyNone:  {},
	RetryPolicyFixed: {},
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

// maxAnnotationsBytes caps each individual annotations object at 64 KB,
// measured on its compacted form. Aggregate annotations across a workflow
// are already bounded by the 10 MB import-body cap; this per-field guard
// stops a single bloated blob. Sits above the 256-char identifier cap by
// three orders of magnitude — annotations carry structured client data
// (role lists, labels, UI hints), not identifiers.
const maxAnnotationsBytes = 64 * 1024

// canonicalizeAnnotations validates one annotations value and returns its
// canonical (compacted) form, or nil when the value is absent, blank, or
// the JSON literal null. The engine never interprets the contents — this
// only enforces that what is stored is a bounded JSON object. The location
// string (e.g. `workflow "x" state "y"`) is used solely to build errors.
func canonicalizeAnnotations(raw json.RawMessage, location string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	// The value is already syntactically valid JSON (the strict decoder
	// validated it when populating the RawMessage), so a leading '{' is a
	// sufficient and necessary marker of a JSON object.
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%s: annotations must be a JSON object", location)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		// Unreachable in practice (decoder already validated); defensive.
		return nil, fmt.Errorf("%s: annotations is not valid JSON: %v", location, err)
	}
	if buf.Len() > maxAnnotationsBytes {
		return nil, fmt.Errorf("%s: annotations size %d bytes exceeds the %d-byte limit",
			location, buf.Len(), maxAnnotationsBytes)
	}
	out := make(json.RawMessage, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

// validateAndNormalizeAnnotations canonicalises the annotations on every
// workflow, state, transition, and processor — plus the workflow/transition
// criterionAnnotations sibling — in the incoming slice, mutating each in
// place. Returns the first validation error (object-only, size cap). Run on
// the incoming import request only — consistent with the other structural
// validators, which are not retroactive against already-stored workflows.
func validateAndNormalizeAnnotations(workflows []spi.WorkflowDefinition) error {
	for i := range workflows {
		wf := &workflows[i]
		canon, err := canonicalizeAnnotations(wf.Annotations, fmt.Sprintf("workflow %q", wf.Name))
		if err != nil {
			return err
		}
		wf.Annotations = canon
		// Criterion annotations sit beside the (verbatim, opaque) criterion.
		// The criterion blob itself is never parsed here.
		wfCrit, err := canonicalizeAnnotations(wf.CriterionAnnotations,
			fmt.Sprintf("workflow %q criterionAnnotations", wf.Name))
		if err != nil {
			return err
		}
		wf.CriterionAnnotations = wfCrit
		for stateName, stateDef := range wf.States {
			sCanon, err := canonicalizeAnnotations(stateDef.Annotations,
				fmt.Sprintf("workflow %q state %q", wf.Name, stateName))
			if err != nil {
				return err
			}
			stateDef.Annotations = sCanon
			for j := range stateDef.Transitions {
				tr := &stateDef.Transitions[j]
				tCanon, err := canonicalizeAnnotations(tr.Annotations,
					fmt.Sprintf("workflow %q state %q transition %q", wf.Name, stateName, tr.Name))
				if err != nil {
					return err
				}
				tr.Annotations = tCanon
				trCrit, err := canonicalizeAnnotations(tr.CriterionAnnotations,
					fmt.Sprintf("workflow %q state %q transition %q criterionAnnotations", wf.Name, stateName, tr.Name))
				if err != nil {
					return err
				}
				tr.CriterionAnnotations = trCrit
				// Processors must be mutated by index — a range-value copy
				// would discard the canonicalised blob.
				for k := range tr.Processors {
					p := &tr.Processors[k]
					pCanon, err := canonicalizeAnnotations(p.Annotations,
						fmt.Sprintf("workflow %q state %q transition %q processor %q", wf.Name, stateName, tr.Name, p.Name))
					if err != nil {
						return err
					}
					p.Annotations = pCanon
				}
			}
			// Map values are not addressable — write the state back so its
			// canonicalised Annotations persist. Transitions/processors mutate
			// through the shared slice backing array and need no write-back.
			wf.States[stateName] = stateDef
		}
	}
	return nil
}

// validateCriterionRegex rejects a criterion whose MATCHES_PATTERN operator
// carries a regex that fails to compile. Criteria are stored opaquely
// (json.RawMessage) and previously were only parsed at transition-evaluation
// time (engine.go's evaluateCriterion -> match.Match -> operators.go's
// opMatchesPattern), so a malformed regex imported successfully and then
// errored on every subsequent evaluation of that transition. This closes
// that fail-open gap by compiling MATCHES_PATTERN regexes at import time.
//
// location names the workflow/state/transition the criterion belongs to, for
// the error message. Empty/null criteria are skipped. A criterion that does
// not parse as a predicate.Condition at all is left alone — this validator
// only tightens already-parseable conditions, it does not newly reject
// shapes ParseCondition already rejects (that is out of scope for this
// check; those criteria fail at evaluation time exactly as before).
func validateCriterionRegex(criterion json.RawMessage, location string) error {
	trimmed := bytes.TrimSpace(criterion)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	cond, err := predicate.ParseCondition(trimmed)
	if err != nil {
		return nil
	}
	return walkCriterionForRegex(cond, location)
}

// walkCriterionForRegex recurses into GroupCondition.Conditions and checks
// every SimpleCondition / LifecycleCondition leaf whose OperatorType is
// MATCHES_PATTERN. Other condition kinds (FunctionCondition, ArrayCondition)
// carry no OperatorType to check and are silently skipped — in particular
// this is how a FUNCTION criterion is exempted from regex validation.
func walkCriterionForRegex(cond predicate.Condition, location string) error {
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return compileMatchesPattern(c.OperatorType, c.Value, location)
	case *predicate.LifecycleCondition:
		return compileMatchesPattern(c.OperatorType, c.Value, location)
	case *predicate.GroupCondition:
		for _, sub := range c.Conditions {
			if err := walkCriterionForRegex(sub, location); err != nil {
				return err
			}
		}
	}
	return nil
}

// compileMatchesPattern compiles value as a regex exactly the way
// the evaluator does — internal/match/operators.go's opMatchesPattern calls
// regexp.MatchString(fmt.Sprintf("%v", expected), actual.String()), which
// itself compiles via regexp.Compile. Mirroring the %v stringification
// and Compile call here means a pattern accepted here is guaranteed
// compilable at evaluation time, and vice versa — no accept/reject skew.
func compileMatchesPattern(operatorType string, value any, location string) error {
	if operatorType != "MATCHES_PATTERN" {
		return nil
	}
	pattern := fmt.Sprintf("%v", value)
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("%s: invalid MATCHES_PATTERN regex %q: %v", location, pattern, err)
	}
	return nil
}

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
//   - M1  — RetryPolicy must be one of NONE, FIXED, or empty (defaults to FIXED).
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
func validateWorkflows(workflows []spi.WorkflowDefinition, allowCycles bool) error {
	for _, wf := range workflows {
		if !allowCycles {
			if err := validateWorkflowLoops(wf); err != nil {
				return fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
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
// length cap), plus criterion regex validation. Any violation is a 4xx at
// import time — the engine would otherwise silently degrade at runtime
// (park entity in an undefined state, shadow duplicate transitions, coerce
// typo'd ExecutionMode to SYNC) or accept arbitrarily long identifiers into
// operational logs and audit events. A malformed MATCHES_PATTERN regex in a
// workflow-level or transition-level criterion is rejected here rather than
// at every subsequent transition-evaluation attempt.
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

	// Workflow-level criterion — reject a malformed MATCHES_PATTERN regex
	// at import instead of failing every evaluation.
	if err := validateCriterionRegex(wf.Criterion, fmt.Sprintf("workflow %q", wf.Name)); err != nil {
		return err
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
	//   M1  — RetryPolicy ∈ {NONE, FIXED, ""}.
	//   Transition.Criterion — a MATCHES_PATTERN regex, if present, must compile.
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

			if err := validateCriterionRegex(tr.Criterion,
				fmt.Sprintf("workflow %q state %q transition %q", wf.Name, stateName, tr.Name)); err != nil {
				return err
			}

			if tr.Manual && tr.Schedule != nil {
				return fmt.Errorf(
					"workflow %q state %q transition %q: manual and scheduled are mutually exclusive",
					wf.Name, stateName, tr.Name)
			}
			if tr.Schedule != nil {
				hasDelay := tr.Schedule.DelayMs > 0
				hasFn := tr.Schedule.Function != nil
				// Exactly one of delayMs or function is required — a static
				// delay or a Function callout that computes the firing time
				// per entity. Both-present and neither-present are rejected
				// alike; the mutual-exclusion is not expressible in the
				// OpenAPI schema (see TransitionScheduleDto), so it is
				// enforced here.
				if hasDelay == hasFn {
					return fmt.Errorf(
						"workflow %q state %q transition %q: exactly one of schedule.delayMs or schedule.function is required",
						wf.Name, stateName, tr.Name)
				}
				if hasFn {
					f := tr.Schedule.Function
					if f.ResultKind != "Schedule" {
						return fmt.Errorf(
							`workflow %q state %q transition %q: schedule.function.resultKind must be "Schedule" (got %q)`,
							wf.Name, stateName, tr.Name, f.ResultKind)
					}
					if f.Name == "" || f.CalculationNodesTags == "" {
						return fmt.Errorf(
							"workflow %q state %q transition %q: schedule.function requires name and calculationNodesTags",
							wf.Name, stateName, tr.Name)
					}
				}
				// No separate `else if DelayMs <= 0` branch: once the XOR
				// check above has passed with hasFn == false, hasDelay must
				// be true (DelayMs > 0), so a dedicated delayMs<=0 rejection
				// here would be unreachable. DelayMs <= 0 with no function
				// is already caught by the XOR check as the "neither
				// present" shape.
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
				// M6 (audit) — asyncResult=true requests a runtime semantic
				// this backend does not implement; reject rather than
				// silently degrade to sync dispatch. Consuming engines that
				// cannot honour async-result semantics must reject at the
				// configuration-import boundary.
				if p.Config.AsyncResult != nil && *p.Config.AsyncResult {
					return fmt.Errorf(
						"workflow %q state %q transition %q processor %q: asyncResult=true is not supported on this backend (async/crossover semantics are not implemented)",
						wf.Name, stateName, tr.Name, p.Name)
				}
				// M6 (audit) — crossoverToAsyncMs is a tuner for the
				// asyncResult semantic. With that semantic unsupported,
				// any non-nil value has no honourable home. Defence in
				// depth — covers both the orphan (no asyncResult=true)
				// and the paired cases that the AsyncResult rule above
				// would have rejected first. The paired-case branch is
				// unreachable today (the AsyncResult rule early-returns
				// before we get here); if these rules are ever refactored
				// into a deferred-collection pattern, the branch will
				// start firing operationally.
				if p.Config.CrossoverToAsyncMs != nil {
					return fmt.Errorf(
						"workflow %q state %q transition %q processor %q: crossoverToAsyncMs is not supported on this backend (async/crossover semantics are not implemented)",
						wf.Name, stateName, tr.Name, p.Name)
				}
				if _, ok := validExecutionModes[p.ExecutionMode]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown executionMode %q (allowed: SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.ExecutionMode)
				}
				if _, ok := validRetryPolicies[p.Config.RetryPolicy]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown retryPolicy %q (allowed: NONE, FIXED, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.Config.RetryPolicy)
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

// validateSchemaVersions checks each workflow's Version field against
// SupportedSchemaRanges. Returns an error wrapping one of the
// sentinel schema errors so callers can branch with errors.Is. The
// error message names the offending workflow by name so a multi-
// workflow import surfaces a clear diagnosis without iterating again.
//
// This check is intentionally separate from validateWorkflows: it
// maps to ErrCodeWorkflowSchemaVersionUnsupported in the handler,
// not ErrCodeValidationFailed, and it must run BEFORE
// applyImportMode mutates anything.
func validateSchemaVersions(workflows []spi.WorkflowDefinition) error {
	for _, wf := range workflows {
		maj, min, err := ParseSchemaVersion(wf.Version)
		if err != nil {
			return fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		if err := Supports(maj, min); err != nil {
			return fmt.Errorf("workflow %q: %w", wf.Name, err)
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

	// Sort state names before iteration so the cycle reported by the
	// detector is deterministic across runs. Go map iteration is
	// randomised per process, which previously made the reported cycle
	// vary across CI runs on a workflow with multiple disjoint cycles.
	// Lexicographic order is an arbitrary but stable choice.
	stateNames := make([]string, 0, len(wf.States))
	for stateName := range wf.States {
		stateNames = append(stateNames, stateName)
	}
	sort.Strings(stateNames)
	for _, stateName := range stateNames {
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
