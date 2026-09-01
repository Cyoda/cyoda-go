package workflow

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
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
			// Criterion on a DECLARED field with a value the entity's data
			// does not hold — a genuine, evaluable non-match. A reference to
			// an undeclared field would instead fail Prepare outright
			// (leafNode's expansion-failure branch, internal/match/prepared.go):
			// an unevaluable comparison leaf fails the transition rather than
			// silently reading as "not satisfied" (correctness-over-availability),
			// so it can no longer stand in for "criterion legitimately
			// doesn't match" the way this test needs.
			Criterion: simpleCriterion("$.algorithm", "EQUALS", "ES256"),
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

	// Register the model declaring the entity's real leaves, including
	// $.algorithm — the criterion's declared type comes from here, so the
	// EQUALS comparison evaluates cleanly and genuinely doesn't match (the
	// entity's algorithm is RS256, not ES256), driving the intended fallback
	// to the default workflow.
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{
		"keyId":     schema.String,
		"algorithm": schema.String,
	})

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
