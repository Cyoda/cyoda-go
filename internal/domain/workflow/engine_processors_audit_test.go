package workflow

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestInternalizedRejection_AuditEvents_ManualTransition asserts the
// full audit-event sequence for a manual-fired internalized processor:
// 1× SMEventProcessingPaused, 2× SMEventStateProcessResult (one inside
// executeProcessors, one in the caller-level failure emit), 0×
// SMEventFinished.
func TestInternalizedRejection_AuditEvents_ManualTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit-manual", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AuditManualWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "FIRE", Next: "DONE", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeInternalized, Name: "internal-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "FIRE")
	if err == nil {
		t.Fatalf("expected rejection error from internalized processor, got nil")
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	var (
		processingPausedCount int
		stateProcessResults   []spi.StateMachineEvent
		stateMachineFinished  int
	)
	for _, ev := range events {
		switch ev.EventType {
		case spi.SMEventProcessingPaused:
			processingPausedCount++
		case spi.SMEventStateProcessResult:
			stateProcessResults = append(stateProcessResults, ev)
		case spi.SMEventFinished:
			stateMachineFinished++
		}
	}

	if processingPausedCount != 1 {
		t.Errorf("SMEventProcessingPaused count = %d, want 1", processingPausedCount)
	}
	if got, want := len(stateProcessResults), 2; got != want {
		t.Fatalf("SMEventStateProcessResult count = %d, want %d (one from executeProcessors, one from caller failure emit)", got, want)
	}
	if stateMachineFinished != 0 {
		t.Errorf("SMEventFinished count = %d, want 0 (cascade aborted)", stateMachineFinished)
	}

	// First emit comes from executeProcessors (engine_processors.go:72-74) with
	// mode = declared ExecutionMode.
	first := stateProcessResults[0]
	if !strings.Contains(first.Details, `Processor "internal-proc" completed`) {
		t.Errorf("first SMEventStateProcessResult Details = %q, want 'Processor \"internal-proc\" completed'", first.Details)
	}
	if mode, _ := first.Data["mode"].(string); mode != ExecutionModeSync {
		t.Errorf("first SMEventStateProcessResult Data.mode = %v, want %q", first.Data["mode"], ExecutionModeSync)
	}
	if success, _ := first.Data["success"].(bool); success {
		t.Errorf("first SMEventStateProcessResult Data.success = true, want false")
	}

	// Second emit comes from engine.go:465-467 (attemptTransition) with the full
	// wrapped error in Details and no mode key.
	second := stateProcessResults[1]
	if !strings.Contains(second.Details, `Processor failed for transition "FIRE":`) {
		t.Errorf("second SMEventStateProcessResult Details = %q, want 'Processor failed for transition \"FIRE\":' prefix", second.Details)
	}
	if !strings.Contains(second.Details, `execution type "internalized" is not yet implemented`) {
		t.Errorf("second SMEventStateProcessResult Details = %q, want rejection sub-string", second.Details)
	}
	if _, hasMode := second.Data["mode"]; hasMode {
		t.Errorf("second SMEventStateProcessResult Data should not have a 'mode' key (caller emit does not set it); got %v", second.Data)
	}
}

// TestInternalizedRejection_AuditEvents_CascadedTransition asserts the
// same audit sequence for a non-manual transition reached via
// engine.cascadeAutomated (engine.go:553-556 emit path).
func TestInternalizedRejection_AuditEvents_CascadedTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit-cascade", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AuditCascadeWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "AUTO", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeInternalized, Name: "internal-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e2", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatalf("expected rejection error from internalized processor in cascade path, got nil")
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e2")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	var stateProcessResults []spi.StateMachineEvent
	for _, ev := range events {
		if ev.EventType == spi.SMEventStateProcessResult {
			stateProcessResults = append(stateProcessResults, ev)
		}
	}

	if got, want := len(stateProcessResults), 2; got != want {
		t.Fatalf("SMEventStateProcessResult count = %d, want %d (one inside executeProcessors, one from cascade failure emit at engine.go:553-556)", got, want)
	}

	// The second emit (from engine.go:553-556) carries the wrapped error in Details.
	second := stateProcessResults[1]
	if !strings.Contains(second.Details, `Processor failed for transition "AUTO":`) {
		t.Errorf("cascade-path second emit Details = %q, want 'Processor failed for transition \"AUTO\":' prefix", second.Details)
	}
	if !strings.Contains(second.Details, `execution type "internalized" is not yet implemented`) {
		t.Errorf("cascade-path second emit Details = %q, want rejection sub-string", second.Details)
	}
}
