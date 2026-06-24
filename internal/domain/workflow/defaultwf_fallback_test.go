package workflow

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestDefaultWorkflowFallback_WhenImportedWorkflowCriterionDoesNotMatch(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "OboSigningKey", ModelVersion: "1"}

	// Import a workflow with a criterion that will NOT match the entity.
	wfStore, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("failed to get workflow store: %v", err)
	}

	importedWF := []spi.WorkflowDefinition{
		{
			Version:      "1.1",
			Name:         "obo-workflow",
			InitialState: "INIT",
			Active:       true,
			// Criterion that requires a field value the entity won't have.
			Criterion: simpleCriterion("$.nonExistentField", "EQUALS", "match-me"),
			States: map[string]spi.StateDefinition{
				"INIT": {
					Transitions: []spi.TransitionDefinition{
						{Name: "PROCESS", Next: "DONE", Manual: false},
					},
				},
				"DONE": {},
			},
		},
	}
	if err := wfStore.Save(ctx, modelRef, importedWF); err != nil {
		t.Fatalf("failed to save workflow: %v", err)
	}

	// Create an entity that does NOT match the workflow criterion.
	entity := makeEntity("obo-1", modelRef, map[string]any{"keyId": "test", "algorithm": "RS256"})

	// Execute — the imported workflow's criterion won't match.
	// The engine should fall back to the default workflow.
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed (should have fallen back to default): %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED from default workflow, got %s", entity.Meta.State)
	}
}
