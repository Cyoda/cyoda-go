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

func TestValidateImportRequest_RejectsEmptyWorkflowName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for empty workflow name, got nil")
	}
	// Pin on "empty name" so an `initialState … name` style error from a
	// different rule can't accidentally satisfy this assertion.
	if !strings.Contains(err.Error(), "empty name") {
		t.Errorf("error must name the empty-name rule, got: %v", err)
	}
}

// --- H6.d / M4 — Workflow Name unique within request ------------------

func TestValidateImportRequest_RejectsDuplicateWorkflowNamesInRequest(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "dup-wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf, wf})
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

func TestValidateImportRequest_RejectsEmptyInitialState(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-empty-initial", InitialState: "", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
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

func TestValidateImportRequest_RejectsInitialStateNotInStates(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-bad-initial", InitialState: "MISSING", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
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

func TestValidateImportRequest_RejectsTransitionNextNotInStates(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-bad-next", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "NOWHERE", Manual: true},
			}},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
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

func TestValidateImportRequest_RejectsDuplicateTransitionNameWithinState(t *testing.T) {
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
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
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
func TestValidateImportRequest_AcceptsSameTransitionNameAcrossStates(t *testing.T) {
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
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error for same-name transitions in different states, got: %v", err)
	}
}

// --- H4 — ExecutionMode enum check ------------------------------------

func TestValidateImportRequest_RejectsUnknownExecutionMode(t *testing.T) {
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
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
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
func TestValidateImportRequest_AcceptsEmptyExecutionMode(t *testing.T) {
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
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error for empty ExecutionMode, got: %v", err)
	}
}

// --- Security-audit follow-ups: M-1 (empty state-map keys) + L-1 (empty
// transition / processor names) + L-2 (name-length cap). Same failure
// class as H6.a / H6.b / H6.c / H6.e — names embedded in lookups,
// matched against engine state machines, and reflected into operational
// logs. Trivial-cost fences, applied uniformly.

// --- M-1: empty state-map key --------------------------------------

func TestValidateImportRequest_RejectsEmptyStateMapKey(t *testing.T) {
	// H6.a says initialState must be ∈ states, and H6.b says next must
	// be ∈ states — but neither rules out the empty string sitting in
	// the states map itself. Without this check, an attacker can set
	// initialState=S1, declare states={"S1": …, "": {}}, route a
	// transition to next="" and re-create the silent-park behaviour
	// H6.b was supposed to close.
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-empty-state-key", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
			"":   {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for empty state-map key, got nil")
	}
	if !strings.Contains(err.Error(), "empty state name") {
		t.Errorf("error must name the empty-state-key rule, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wf-empty-state-key") {
		t.Errorf("error must name the offending workflow, got: %v", err)
	}
}

// --- L-1: empty transition name ------------------------------------

func TestValidateImportRequest_RejectsEmptyTransitionName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-empty-trans-name", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "", Next: "S2", Manual: true},
			}},
			"S2": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for empty transition name, got nil")
	}
	if !strings.Contains(err.Error(), "transition") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("error must name the empty-transition-name rule, got: %v", err)
	}
	if !strings.Contains(err.Error(), "S1") {
		t.Errorf("error must name the offending state, got: %v", err)
	}
}

// --- L-1: empty processor name -------------------------------------

func TestValidateImportRequest_RejectsEmptyProcessorName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf-empty-proc-name", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "", ExecutionMode: ExecutionModeSync},
				}},
			}},
			"S2": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for empty processor name, got nil")
	}
	if !strings.Contains(err.Error(), "processor") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("error must name the empty-processor-name rule, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"t"`) {
		t.Errorf("error must name the offending transition, got: %v", err)
	}
}

// --- L-2: identifier length cap (defense-in-depth against log-volume
// amplification + unbounded engine state-machine names).
//
// 256 is generous enough that no legitimate identifier should be
// affected (cf. OpenAPI patterns already in the spec — entity field
// names cap at 100, descriptions at 1024). Names appear in error
// strings, log lines, audit events, and state lookups; bounding them
// at validation time prevents a tenant from spamming the operational
// log with multi-KB per error line.

func TestValidateImportRequest_RejectsOverlongWorkflowName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: strings.Repeat("x", 257), InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{"S1": {}},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for overlong workflow name (257 chars), got nil")
	}
	if !strings.Contains(err.Error(), "256") {
		t.Errorf("error must mention the 256-char limit, got: %v", err)
	}
}

func TestValidateImportRequest_AcceptsMaxLengthWorkflowName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: strings.Repeat("x", 256), InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{"S1": {}},
	}
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error for exactly-256-char workflow name, got: %v", err)
	}
}

func TestValidateImportRequest_RejectsOverlongStateName(t *testing.T) {
	long := strings.Repeat("s", 257)
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {},
			long: {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for overlong state name, got nil")
	}
	if !strings.Contains(err.Error(), "256") {
		t.Errorf("error must mention the 256-char limit, got: %v", err)
	}
}

func TestValidateImportRequest_RejectsOverlongTransitionName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: strings.Repeat("t", 257), Next: "S2", Manual: true},
			}},
			"S2": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for overlong transition name, got nil")
	}
	if !strings.Contains(err.Error(), "256") {
		t.Errorf("error must mention the 256-char limit, got: %v", err)
	}
}

func TestValidateImportRequest_RejectsOverlongProcessorName(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: strings.Repeat("p", 257), ExecutionMode: ExecutionModeSync},
				}},
			}},
			"S2": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for overlong processor name, got nil")
	}
	if !strings.Contains(err.Error(), "256") {
		t.Errorf("error must mention the 256-char limit, got: %v", err)
	}
}

// --- Manual + Schedule mutual exclusion -----------------------------------

func TestValidator_ManualAndSchedule_Rejected(t *testing.T) {
	tm := int64(1000)
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{
						Name:     "T1",
						Next:     "S1",
						Manual:   true,
						Schedule: &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tm},
					},
				},
			},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("expected validation error for manual=true + schedule, got nil")
	}
	if !strings.Contains(err.Error(), "manual and scheduled are mutually exclusive") {
		t.Errorf("error message missing expected substring; got: %v", err)
	}
}

// --- Schedule.DelayMs must be > 0 -------------------------------------------

func TestValidator_DelayMsZero_Rejected(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{
						Name:     "T1",
						Next:     "S1",
						Manual:   false,
						Schedule: &spi.TransitionSchedule{DelayMs: 0},
					},
				},
			},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil || !strings.Contains(err.Error(), "delayMs must be > 0") {
		t.Errorf("expected delayMs error, got: %v", err)
	}
}

func TestValidator_DelayMsNegative_Rejected(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{
						Name:     "T1",
						Next:     "S1",
						Manual:   false,
						Schedule: &spi.TransitionSchedule{DelayMs: -100},
					},
				},
			},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil || !strings.Contains(err.Error(), "delayMs must be > 0") {
		t.Errorf("expected delayMs error, got: %v", err)
	}
}

// --- Schedule happy paths — regression guards for TimeoutMs pointer states ------
//
// These four shapes MUST validate successfully. Their primary value is as a
// regression guard against the bogus rev 1/rev 2 TimeoutMs >= DelayMs rule
// (removed in rev 3 as nonsensical under the late-tolerance semantic).
// No validator changes are needed — the tests document an existing acceptance
// contract and pin that contract against future regressions.
//
// Semantic notes:
//
//   - timeoutMs absent (nil) — "no-timeout" semantic: fire indefinitely late.
//   - timeoutMs explicit zero (&0) — "strictest" semantic: drop if any
//     lateness is observed.
//   - timeoutMs < delayMs (e.g. delay 1000ms, timeout 500ms) — VALID under the
//     late-tolerance semantic. TimeoutMs is not a maximum age of the armed
//     timer; it is the maximum lateness of actual firing vs. scheduled time.
//     A transition armed at t=0, scheduled to fire at t=1000ms, with a
//     timeout of 500ms, will fire at t=1000ms and be dropped only if it
//     fires after t=1500ms.
//   - timeoutMs > delayMs — valid under the same semantic.

func TestValidator_ScheduleHappyPaths(t *testing.T) {
	tmZero := int64(0)
	tmSmall := int64(500)
	tmPositive := int64(5000)

	cases := []struct {
		name string
		sch  *spi.TransitionSchedule
	}{
		{"timeoutMs_absent_nil", &spi.TransitionSchedule{DelayMs: 1000}},
		{"timeoutMs_explicit_zero", &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tmZero}},
		{"timeoutMs_less_than_delayMs", &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tmSmall}},
		{"timeoutMs_greater_than_delayMs", &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tmPositive}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := spi.WorkflowDefinition{
				Version:      "1",
				Name:         "test",
				InitialState: "S1",
				Active:       true,
				States: map[string]spi.StateDefinition{
					"S1": {
						Transitions: []spi.TransitionDefinition{
							{Name: "T1", Next: "S1", Manual: false, Schedule: tc.sch},
						},
					},
				},
			}
			if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
				t.Errorf("expected acceptance for shape %q; got: %v", tc.name, err)
			}
		})
	}
}

// --- allowCycles bypass behaviour ----------------------------------------

// cyclicWorkflow builds S1 -automated, no criterion-> S2 -automated, no
// criterion-> S1. validateWorkflowLoops rejects this by default.
func cyclicWorkflow(t *testing.T) spi.WorkflowDefinition {
	t.Helper()
	return spi.WorkflowDefinition{
		Version:      "1",
		Name:         "cyclic",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{{Name: "to-S2", Next: "S2", Manual: false}}},
			"S2": {Transitions: []spi.TransitionDefinition{{Name: "to-S1", Next: "S1", Manual: false}}},
		},
	}
}

func TestValidator_DefaultFalse_RejectsCycle(t *testing.T) {
	wf := cyclicWorkflow(t)
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, false); err == nil {
		t.Fatal("expected cycle rejection at default allowCycles=false")
	}
}

func TestValidator_AllowCyclesTrue_BypassesCycleCheck(t *testing.T) {
	wf := cyclicWorkflow(t)
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, true); err != nil {
		t.Errorf("expected acceptance with allowCycles=true; got: %v", err)
	}
}

func TestValidator_AllowCyclesTrue_DoesNotBypassScheduleRules(t *testing.T) {
	tm := int64(1000)
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "cyclic_and_incoherent",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S2", Next: "S2", Manual: true, Schedule: &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tm}},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S1", Next: "S1", Manual: false},
			}},
		},
	}
	// validateImportRequest catches the structural (manual+schedule) rule,
	// regardless of allowCycles. Note this calls validateImportRequest, not
	// validateWorkflows — the Schedule rules live in validateWorkflowStructure.
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err == nil {
		t.Fatal("expected manual+schedule rejection even when cyclic")
	} else if !strings.Contains(err.Error(), "manual and scheduled are mutually exclusive") {
		t.Errorf("expected Schedule-rule error, got: %v", err)
	}
}

func TestValidator_AllowCyclesTrue_PollingScheduledWorkflow_Accepted(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "polling",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S2", Next: "S2", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S1", Next: "S1", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
		},
	}
	// Default (allowCycles=false) rejects (the scheduled-polling cycle).
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, false); err == nil {
		t.Fatal("expected cycle rejection on polling workflow at allowCycles=false")
	}
	// allowCycles=true accepts.
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, true); err != nil {
		t.Errorf("expected polling workflow to validate with allowCycles=true; got: %v", err)
	}
	// Structural validation (Schedule rules) also passes.
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("expected structural validation to pass on polling workflow; got: %v", err)
	}
}

// All four named modes must be accepted.
func TestValidateImportRequest_AcceptsAllKnownExecutionModes(t *testing.T) {
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
			if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
				t.Fatalf("expected no error for ExecutionMode=%q, got: %v", mode, err)
			}
		})
	}
}

// --- AsyncResult + CrossoverToAsyncMs reject-at-import (#261) ------------

// asyncResultRejectFixture builds a minimal valid two-state workflow with
// one externalized SYNC processor on the only transition, then lets the
// caller mutate the processor's ProcessorConfig before validation.
func asyncResultRejectFixture(mutate func(*spi.ProcessorConfig)) spi.WorkflowDefinition {
	wf := spi.WorkflowDefinition{
		Version: "1", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: false, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: ExecutionModeSync},
				}},
			}},
			"S2": {},
		},
	}
	if mutate != nil {
		mutate(&wf.States["S1"].Transitions[0].Processors[0].Config)
	}
	return wf
}

func TestValidator_AsyncResultTrue_Rejected(t *testing.T) {
	tt := true
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &tt
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for asyncResult=true, got nil")
	}
	for _, want := range []string{`workflow "wf"`, `state "S1"`, `transition "t"`, `processor "p"`, "asyncResult=true is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message must contain %q; got: %v", want, err)
		}
	}
}

func TestValidator_AsyncResultFalse_Accepted(t *testing.T) {
	ff := false
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &ff
	})
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("explicit asyncResult=false must be accepted; got: %v", err)
	}
}

func TestValidator_AsyncResultAbsent_Accepted(t *testing.T) {
	wf := asyncResultRejectFixture(nil)
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("absent asyncResult must be accepted; got: %v", err)
	}
}

func TestValidator_CrossoverOrphan_Rejected(t *testing.T) {
	cv := int64(5000)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for orphan crossoverToAsyncMs, got nil")
	}
	for _, want := range []string{`processor "p"`, "crossoverToAsyncMs is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message must contain %q; got: %v", want, err)
		}
	}
}

func TestValidator_CrossoverZero_Rejected(t *testing.T) {
	cv := int64(0)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for crossoverToAsyncMs=&0 (non-nil zero), got nil")
	}
	if !strings.Contains(err.Error(), "crossoverToAsyncMs is not supported") {
		t.Errorf("error must name the crossover rule; got: %v", err)
	}
}

func TestValidator_AsyncTrueAndCrossover_AsyncRuleFiresFirst(t *testing.T) {
	tt := true
	cv := int64(5000)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &tt
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for asyncResult=true + crossoverToAsyncMs, got nil")
	}
	// Async rule fires first per documented ordering (spec §4).
	if !strings.Contains(err.Error(), "asyncResult=true is not supported") {
		t.Errorf("expected asyncResult message (ordering: async-rule-first); got: %v", err)
	}
	if strings.Contains(err.Error(), "crossoverToAsyncMs is not supported") {
		t.Errorf("crossover message should not surface when async rule fires first; got: %v", err)
	}
}

func TestValidator_AsyncFalseAndCrossover_CrossoverRuleFires(t *testing.T) {
	ff := false
	cv := int64(5000)
	wf := asyncResultRejectFixture(func(c *spi.ProcessorConfig) {
		c.AsyncResult = &ff
		c.CrossoverToAsyncMs = &cv
	})
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error for asyncResult=&false + crossoverToAsyncMs, got nil")
	}
	if !strings.Contains(err.Error(), "crossoverToAsyncMs is not supported") {
		t.Errorf("expected crossover message when async is false; got: %v", err)
	}
}

func TestValidator_MultiProcessor_OnlyOneBad_Rejected(t *testing.T) {
	tt := true
	// Transition with three processors; the SECOND has the bad field.
	wf := spi.WorkflowDefinition{
		Version: "1", Name: "wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: false, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
					{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync,
						Config: spi.ProcessorConfig{AsyncResult: &tt}},
					{Type: ProcessorTypeExternalized, Name: "p3", ExecutionMode: ExecutionModeSync},
				}},
			}},
			"S2": {},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error naming p2, got nil")
	}
	if !strings.Contains(err.Error(), `processor "p2"`) {
		t.Errorf("error must name the offending processor p2 specifically; got: %v", err)
	}
	if strings.Contains(err.Error(), `processor "p1"`) || strings.Contains(err.Error(), `processor "p3"`) {
		t.Errorf("error must not name unrelated processors; got: %v", err)
	}
}
