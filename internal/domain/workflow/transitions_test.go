package workflow

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestGetAvailableTransitions_WithTransitions(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "OrderWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "approve", Next: "APPROVED"},
				{Name: "reject", Next: "REJECTED"},
			}},
			"APPROVED": {Transitions: []spi.TransitionDefinition{}},
			"REJECTED": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Save entity directly to store in state OPEN.
	entity := makeEntity("e-trans-1", modelRef, map[string]any{"amount": 42})
	entity.Meta.State = "OPEN"
	entity.Meta.TenantID = testTenant

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	names, err := engine.GetAvailableTransitions(ctx, "e-trans-1", modelRef, time.Now())
	if err != nil {
		t.Fatalf("GetAvailableTransitions failed: %v", err)
	}

	if len(names) != 2 {
		t.Fatalf("expected 2 transitions, got %d: %v", len(names), names)
	}
	if names[0] != "approve" || names[1] != "reject" {
		t.Fatalf("expected [approve, reject], got %v", names)
	}
}

func TestGetAvailableTransitions_TerminalState(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "OrderWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "close", Next: "CLOSED"}}},
			"CLOSED": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e-trans-2", modelRef, map[string]any{})
	entity.Meta.State = "CLOSED"
	entity.Meta.TenantID = testTenant

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	names, err := engine.GetAvailableTransitions(ctx, "e-trans-2", modelRef, time.Now())
	if err != nil {
		t.Fatalf("GetAvailableTransitions failed: %v", err)
	}

	if len(names) != 0 {
		t.Fatalf("expected 0 transitions for terminal state, got %d: %v", len(names), names)
	}
}

func TestGetAvailableTransitions_EntityNotFound(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}

	_, err := engine.GetAvailableTransitions(ctx, "nonexistent-entity", modelRef, time.Now())
	if err == nil {
		t.Fatal("expected error for nonexistent entity")
	}

	appErr, ok := err.(*common.AppError)
	if !ok {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeEntityNotFound {
		t.Fatalf("expected error code %s, got %s", common.ErrCodeEntityNotFound, appErr.Code)
	}
}

func TestGetAvailableTransitions_NoWorkflow_UsesDefault(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "widget", ModelVersion: "1.0"}

	// No workflow registered for this model. Entity in CREATED state.
	// Default workflow has CREATED state with transitions: UPDATE, DELETE.
	entity := makeEntity("e-trans-4", modelRef, map[string]any{})
	entity.Meta.State = "CREATED"
	entity.Meta.TenantID = testTenant

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	names, err := engine.GetAvailableTransitions(ctx, "e-trans-4", modelRef, time.Now())
	if err != nil {
		t.Fatalf("GetAvailableTransitions failed: %v", err)
	}

	// Default workflow CREATED state has UPDATE and DELETE transitions.
	if len(names) != 2 {
		t.Fatalf("expected 2 default transitions, got %d: %v", len(names), names)
	}
	if names[0] != "UPDATE" || names[1] != "DELETE" {
		t.Fatalf("expected [UPDATE, DELETE], got %v", names)
	}
}

func TestGetAvailableTransitions_CustomWorkflowTakesPrecedence(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "invoice", ModelVersion: "1.0"}

	customWF := spi.WorkflowDefinition{
		Version: "1.1", Name: "InvoiceWF", InitialState: "DRAFT", Active: true,
		States: map[string]spi.StateDefinition{
			"DRAFT": {Transitions: []spi.TransitionDefinition{
				{Name: "submit", Next: "SUBMITTED"},
				{Name: "cancel", Next: "CANCELLED"},
			}},
			"SUBMITTED": {Transitions: []spi.TransitionDefinition{}},
			"CANCELLED": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{customWF})

	entity := makeEntity("e-trans-5", modelRef, map[string]any{})
	entity.Meta.State = "DRAFT"
	entity.Meta.TenantID = testTenant

	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	names, err := engine.GetAvailableTransitions(ctx, "e-trans-5", modelRef, time.Now())
	if err != nil {
		t.Fatalf("GetAvailableTransitions failed: %v", err)
	}

	if len(names) != 2 {
		t.Fatalf("expected 2 transitions from custom workflow, got %d: %v", len(names), names)
	}
	if names[0] != "submit" || names[1] != "cancel" {
		t.Fatalf("expected [submit, cancel], got %v", names)
	}
}
