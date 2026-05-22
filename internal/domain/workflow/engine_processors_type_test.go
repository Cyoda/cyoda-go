package workflow

import (
	"context"
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
