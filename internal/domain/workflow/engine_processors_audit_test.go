package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// errProcessorRefused is the failure the mock compute member answers with in
// the failed-processor audit tests below.
var errProcessorRefused = errors.New("compute member refused the request")

// failingProcessorEngine returns an engine whose compute member fails every
// processor dispatch with errProcessorRefused.
func failingProcessorEngine(t *testing.T) (*Engine, spi.StoreFactory) {
	t.Helper()
	engine, factory := setupEngine(t)
	engine.extProc = &mockExternalProcessing{
		dispatchFunc: func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
			return nil, errProcessorRefused
		},
	}
	return engine, factory
}

// TestFailedProcessor_AuditEvents_ManualTransition asserts the full
// audit-event sequence for a manual-fired SYNC processor that fails:
// 1× SMEventProcessingPaused, 1× SMEventStateProcessResult (the processor's
// own record, without the wrapped error text), 0× SMEventFinished.
func TestFailedProcessor_AuditEvents_ManualTransition(t *testing.T) {
	engine, factory := failingProcessorEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit-manual", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AuditManualWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "FIRE", Next: "DONE", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "failing-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "FIRE")
	if !errors.Is(err, errProcessorRefused) {
		t.Fatalf("Execute error = %v, want the processor's failure", err)
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
	if got, want := len(stateProcessResults), 1; got != want {
		t.Fatalf("SMEventStateProcessResult count = %d, want %d (the processor's own record only)", got, want)
	}
	if stateMachineFinished != 0 {
		t.Errorf("SMEventFinished count = %d, want 0 (cascade aborted)", stateMachineFinished)
	}

	only := stateProcessResults[0]
	if !strings.Contains(only.Details, `Processor "failing-proc" completed`) {
		t.Errorf("SMEventStateProcessResult Details = %q, want 'Processor \"failing-proc\" completed'", only.Details)
	}
	if strings.Contains(only.Details, errProcessorRefused.Error()) {
		t.Errorf("SMEventStateProcessResult Details = %q holds the wrapped error text", only.Details)
	}
	if mode, _ := only.Data["executionMode"].(string); mode != ExecutionModeSync {
		t.Errorf("SMEventStateProcessResult Data.executionMode = %v, want %q", only.Data["executionMode"], ExecutionModeSync)
	}
	if success, _ := only.Data["success"].(bool); success {
		t.Errorf("SMEventStateProcessResult Data.success = true, want false")
	}
}

// TestFailedProcessor_AuditEvents_CascadedTransition asserts the same audit
// sequence for a failing processor on a non-manual transition reached via
// engine.cascadeAutomated.
func TestFailedProcessor_AuditEvents_CascadedTransition(t *testing.T) {
	engine, factory := failingProcessorEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit-cascade", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AuditCascadeWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "AUTO", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "failing-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e2", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "")
	if !errors.Is(err, errProcessorRefused) {
		t.Fatalf("Execute error = %v, want the processor's failure in the cascade path", err)
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

	if got, want := len(stateProcessResults), 1; got != want {
		t.Fatalf("SMEventStateProcessResult count = %d, want %d (the processor's own record only)", got, want)
	}
	if strings.Contains(stateProcessResults[0].Details, errProcessorRefused.Error()) {
		t.Errorf("cascade-path SMEventStateProcessResult Details = %q holds the wrapped error text", stateProcessResults[0].Details)
	}
}

// TestExecuteProcessors_EmptyPipeline_NoPause asserts that a transition with
// no processors fires and reaches the target state without entering the
// per-processor loop: no SMEventProcessingPaused is recorded.
func TestExecuteProcessors_EmptyPipeline_NoPause(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-procs", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "NoProcsWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entity.Meta.State != "DONE" {
		t.Errorf("entity state = %q, want DONE", entity.Meta.State)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	for _, ev := range events {
		if ev.EventType == spi.SMEventProcessingPaused {
			t.Errorf("SMEventProcessingPaused was recorded for an empty processors[]; want none")
		}
	}
}

// TestEngine_RecordEvent_StampsAttribution asserts that every state-machine
// audit event the engine records under an on-behalf-of context carries the
// {attributed, executor} pair spi.AttributionFor(ctx) computes: here alice
// (user) as the attributed principal, the OBO client (service) as executor.
func TestEngine_RecordEvent_StampsAttribution(t *testing.T) {
	engine, factory := setupEngine(t)
	uc := &spi.UserContext{
		UserID: "alice", UserName: "alice", Kind: spi.PrincipalUser,
		Tenant:   spi.Tenant{ID: testTenant, Name: string(testTenant)},
		Roles:    []string{"USER"},
		Executor: &spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService},
	}
	ctx := spi.WithUserContext(context.Background(), uc)
	modelRef := spi.ModelRef{EntityName: "audit-attribution", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AuditAttributionWF", InitialState: "NONE", Active: true,
		States: map[string]spi.StateDefinition{
			"NONE":   {Transitions: []spi.TransitionDefinition{{Name: "store", Next: "STORED", Manual: false}}},
			"STORED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	if _, err := engine.Execute(ctx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one state machine event")
	}
	for _, ev := range events {
		if ev.Attributed.ID != "alice" || ev.Attributed.Kind != spi.PrincipalUser {
			t.Errorf("event %s: Attributed = %+v, want {alice user}", ev.EventType, ev.Attributed)
		}
		if ev.Executor.ID != "OBOCLIENT0000001" || ev.Executor.Kind != spi.PrincipalService {
			t.Errorf("event %s: Executor = %+v, want {OBOCLIENT0000001 service}", ev.EventType, ev.Executor)
		}
	}
}
