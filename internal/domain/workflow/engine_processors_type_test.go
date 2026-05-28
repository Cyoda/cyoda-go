package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestInternalizedRejection_ExecutionModeMatrix asserts that Type:
// "internalized" is rejected at fire time regardless of the declared
// ExecutionMode. The critical case is ASYNC_NEW_TX — the existing abort
// gate at engine_processors.go:109 keys on proc.ExecutionMode, so a
// rejection that fell through to the gate would be silently swallowed
// for ASYNC_NEW_TX and the transition would succeed.
func TestInternalizedRejection_ExecutionModeMatrix(t *testing.T) {
	cases := []struct {
		name          string
		executionMode string
	}{
		{name: "ExecutionMode unset", executionMode: ""},
		{name: "ExecutionMode SYNC", executionMode: ExecutionModeSync},
		{name: "ExecutionMode ASYNC_NEW_TX", executionMode: ExecutionModeAsyncNewTx},
		{name: "ExecutionMode COMMIT_BEFORE_DISPATCH", executionMode: ExecutionModeCommitBeforeDispatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "internalized-reject-" + tc.executionMode, ModelVersion: "1.0"}

			engine.extProc = &mockExternalProcessing{
				// Should NEVER be called — the Type-axis early-return must
				// short-circuit before any ExecutionMode dispatch.
				dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
					t.Fatalf("mockExternalProcessing.DispatchProcessor was called for %q (proc=%s) — internalized rejection should have short-circuited before dispatch", tc.executionMode, proc.Name)
					return entity, nil
				},
			}

			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "InternalizedRejectWF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: false,
							Processors: []spi.ProcessorDefinition{
								{
									Type:          ProcessorTypeInternalized,
									Name:          "internal-proc",
									ExecutionMode: tc.executionMode,
								},
							}},
					}},
					"DONE": {},
				},
			}
			saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

			entity := makeEntity("e1", modelRef, map[string]any{})
			result, err := engine.Execute(ctx, entity, "")
			if err == nil {
				t.Fatalf("expected error from internalized processor rejection, got nil (result=%+v)", result)
			}

			msg := err.Error()
			if !strings.Contains(msg, `execution type "internalized" is not yet implemented`) {
				t.Errorf("error message missing rejection text: %q", msg)
			}
			if !strings.Contains(msg, "processor internal-proc failed:") {
				t.Errorf("error message missing outer-wrap prefix: %q", msg)
			}

			// Entity must not have advanced to the transition's Next state.
			// Execute places the entity at InitialState ("INITIAL") before
			// running the cascade, so after a processor failure the entity
			// sits at the initial state — it did not advance to "DONE".
			if entity.Meta.State != "INITIAL" {
				t.Errorf("entity state expected initial state (\"INITIAL\"), got %q", entity.Meta.State)
			}
		})
	}
}

// TestType_Externalized_FallsThroughToExecutionModeDispatch asserts that
// Type: "externalized" (and Type: "" / unset) is treated identically by
// the engine — both fall through to the ExecutionMode dispatch path.
func TestType_Externalized_FallsThroughToExecutionModeDispatch(t *testing.T) {
	cases := []struct {
		name    string
		typeVal string
	}{
		{name: "Type unset", typeVal: ""},
		{name: "Type externalized", typeVal: ProcessorTypeExternalized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "fall-through-" + tc.typeVal, ModelVersion: "1.0"}

			var dispatched bool
			engine.extProc = &mockExternalProcessing{
				dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
					dispatched = true
					return entity, nil
				},
			}

			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "FallThroughWF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: false,
							Processors: []spi.ProcessorDefinition{
								{Type: tc.typeVal, Name: "p", ExecutionMode: ExecutionModeSync},
							}},
					}},
					"DONE": {},
				},
			}
			saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

			entity := makeEntity("e1", modelRef, map[string]any{})
			_, err := engine.Execute(ctx, entity, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !dispatched {
				t.Errorf("mockExternalProcessing.DispatchProcessor was NOT called — externalized fall-through is broken")
			}
			if entity.Meta.State != "DONE" {
				t.Errorf("entity state = %q, want DONE", entity.Meta.State)
			}
		})
	}
}

// TestType_UnknownValues_FallThrough asserts that any Type value other
// than the exact string "internalized" falls through to the ExecutionMode
// dispatch path. Pins the no-normalisation, exact-match contract.
func TestType_UnknownValues_FallThrough(t *testing.T) {
	cases := []string{
		"scheduled",            // legacy value removed from OpenAPI in this PR
		"EXTERNALIZED",         // case-mismatched uppercase
		" externalized ",       // whitespace-padded
		"internalized\nfoo",    // newline injection — exact-match boundary
		"future_unknown_value", // arbitrary unknown
	}

	for _, typeVal := range cases {
		t.Run("Type="+typeVal, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "unknown-" + typeVal, ModelVersion: "1.0"}

			var dispatched bool
			engine.extProc = &mockExternalProcessing{
				dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
					dispatched = true
					return entity, nil
				},
			}

			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "UnknownTypeWF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: false,
							Processors: []spi.ProcessorDefinition{
								{Type: typeVal, Name: "p", ExecutionMode: ExecutionModeSync},
							}},
					}},
					"DONE": {},
				},
			}
			saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

			entity := makeEntity("e1", modelRef, map[string]any{})
			_, err := engine.Execute(ctx, entity, "")
			if err != nil {
				t.Fatalf("Type=%q produced error %v — should have fallen through to externalized dispatch", typeVal, err)
			}
			if !dispatched {
				t.Errorf("Type=%q did not reach the ExecutionMode dispatch — fall-through broken", typeVal)
			}
			if entity.Meta.State != "DONE" {
				t.Errorf("Type=%q entity state = %q, want DONE", typeVal, entity.Meta.State)
			}
		})
	}
}

// TestType_EmptyProcessors_NoOp asserts that a transition with no
// processors at all imports, fires, and reaches the target state without
// entering the per-processor loop. No SMEventProcessingPaused is emitted.
// Pins that the new Type-axis early-return does not regress the
// empty-pipeline path (executeProcessors early-return at lines 43-45).
func TestType_EmptyProcessors_NoOp(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-procs", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NoProcsWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entity.Meta.State != "DONE" {
		t.Errorf("entity state = %q, want DONE", entity.Meta.State)
	}

	auditStore, _ := factory.StateMachineAuditStore(ctx)
	events, _ := auditStore.GetEvents(ctx, "e1")
	for _, ev := range events {
		if ev.EventType == spi.SMEventProcessingPaused {
			t.Errorf("SMEventProcessingPaused was emitted for an empty processors[] — expected zero")
		}
	}
}

// TestInternalizedRejection_CascadePosition_SYNC asserts that a SYNC
// predecessor's mutations land in entity.Data, the internalized rejection
// aborts the pipeline, and the successor processor is never invoked.
// entity.Meta.State stays in the source state.
//
// Per spec §6.1 test 5 the variant tests for A:ASYNC_NEW_TX and
// A:COMMIT_BEFORE_DISPATCH are explicitly out of scope — those
// transactional shapes have their own well-tested abort semantics
// independent of the Type-axis check.
func TestInternalizedRejection_CascadePosition_SYNC(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cascade-position", ModelVersion: "1.0"}

	dispatchOrder := []string{}
	engine.extProc = &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
			dispatchOrder = append(dispatchOrder, proc.Name)
			if proc.Name == "A" {
				modified := *entity
				modified.Data = []byte(`{"A_ran":true}`)
				return &modified, nil
			}
			return entity, nil
		},
	}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CascadePosWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "A", ExecutionMode: ExecutionModeSync},
						{Type: ProcessorTypeInternalized, Name: "B", ExecutionMode: ExecutionModeSync},
						{Type: ProcessorTypeExternalized, Name: "C", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", modelRef, map[string]any{"original": true})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatalf("expected rejection error from B (internalized), got nil")
	}
	if !strings.Contains(err.Error(), `execution type "internalized" is not yet implemented`) {
		t.Errorf("error = %q, want rejection sub-string", err.Error())
	}

	// A ran, B never reached the mock (early-return short-circuit), C never ran.
	if len(dispatchOrder) != 1 || dispatchOrder[0] != "A" {
		t.Errorf("dispatch order = %v, want [A] (B should not reach mock; C should not be dispatched)", dispatchOrder)
	}

	// A's mutation applied to in-memory entity.Data.
	if string(entity.Data) != `{"A_ran":true}` {
		t.Errorf("entity.Data = %s, want A's mutation (in-memory layer)", entity.Data)
	}

	// entity.Meta.State stays in source state. NOTE: the engine sets
	// entity.Meta.State = selectedWF.InitialState ("INITIAL") at
	// engine.go:139 before the cascade runs, so the source state is
	// "INITIAL", not "" — see the matrix test for the same reasoning.
	if entity.Meta.State != "INITIAL" {
		t.Errorf("entity.Meta.State = %q, want source state (\"INITIAL\")", entity.Meta.State)
	}
}

// TestType_RoundTrip_PreservesUnknownValues confirms that removing the
// ScheduledTransitionProcessorDefinitionDto from OpenAPI does not change
// wire behaviour for the SPI's free-string Type field. An unknown value
// like "scheduled" or "future_unknown_value" survives the JSON round-trip
// through spi.WorkflowDefinition verbatim.
func TestType_RoundTrip_PreservesUnknownValues(t *testing.T) {
	cases := []string{"scheduled", "future_unknown_value"}

	for _, typeVal := range cases {
		t.Run("Type="+typeVal, func(t *testing.T) {
			wf := spi.WorkflowDefinition{
				Version: "1.0", Name: "RoundTrip", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{
						{Name: "RUN", Next: "DONE", Manual: true,
							Processors: []spi.ProcessorDefinition{
								{Type: typeVal, Name: "p", ExecutionMode: ExecutionModeSync},
							}},
					}},
					"DONE": {},
				},
			}

			raw, err := json.Marshal(wf)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var roundTrip spi.WorkflowDefinition
			if err := json.Unmarshal(raw, &roundTrip); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			gotType := roundTrip.States["INITIAL"].Transitions[0].Processors[0].Type
			if gotType != typeVal {
				t.Errorf("round-trip Type = %q, want %q", gotType, typeVal)
			}
		})
	}
}
