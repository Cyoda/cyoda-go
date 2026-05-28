package memory_test

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func sampleWorkflows() []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{
		{
			Version:      "1.0",
			Name:         "order-workflow",
			Description:  "Handles order lifecycle",
			InitialState: "NEW",
			Active:       true,
			Criterion:    json.RawMessage(`{"type":"all"}`),
			States: map[string]spi.StateDefinition{
				"NEW": {
					Transitions: []spi.TransitionDefinition{
						{
							Name: "approve",
							Next: "APPROVED",
							Processors: []spi.ProcessorDefinition{
								{
									Type: "externalized",
									Name: "validate-order",
									Config: spi.ProcessorConfig{
										AttachEntity:      true,
										ResponseTimeoutMs: 5000,
									},
								},
							},
						},
					},
				},
				"APPROVED": {},
			},
		},
	}
}

func TestWorkflowStoreSaveAndGet(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	wfs := sampleWorkflows()

	if err := store.Save(ctx, ref, wfs); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(got))
	}
	if got[0].Name != "order-workflow" {
		t.Errorf("expected name order-workflow, got %s", got[0].Name)
	}
	if got[0].InitialState != "NEW" {
		t.Errorf("expected initialState NEW, got %s", got[0].InitialState)
	}
	if !got[0].Active {
		t.Error("expected active=true")
	}
	if len(got[0].States) != 2 {
		t.Errorf("expected 2 states, got %d", len(got[0].States))
	}
	newState := got[0].States["NEW"]
	if len(newState.Transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(newState.Transitions))
	}
	if newState.Transitions[0].Next != "APPROVED" {
		t.Errorf("expected next=APPROVED, got %s", newState.Transitions[0].Next)
	}
	if len(newState.Transitions[0].Processors) != 1 {
		t.Fatalf("expected 1 processor, got %d", len(newState.Transitions[0].Processors))
	}
	if newState.Transitions[0].Processors[0].Config.ResponseTimeoutMs != 5000 {
		t.Errorf("expected timeout 5000, got %d", newState.Transitions[0].Processors[0].Config.ResponseTimeoutMs)
	}

	// Verify deep copy — mutating returned value must not affect stored data.
	got[0].Name = "mutated"
	got2, _ := store.Get(ctx, ref)
	if got2[0].Name == "mutated" {
		t.Error("store returned a shallow copy — mutation leaked")
	}
}

func TestWorkflowStoreDelete(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.WorkflowStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	_ = store.Save(ctx, ref, sampleWorkflows())

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	// SPI contract: Get after delete returns empty slice, not an error.
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("expected nil error after delete, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice after delete, got %d entries", len(got))
	}
}

func TestWorkflowStoreGetNotFound_ReturnsEmpty(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SPI contract: unknown model returns empty slice, not ErrNotFound.
	ref := spi.ModelRef{EntityName: "NonExistent", ModelVersion: "1"}
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("expected nil error for missing model, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice for missing model, got %d entries", len(got))
	}
}

func TestWorkflowStoreTenantIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.WorkflowStore(ctxA)
	storeB, _ := factory.WorkflowStore(ctxB)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	_ = storeA.Save(ctxA, ref, sampleWorkflows())

	// SPI contract: tenant B sees empty slice, not an error.
	got, err := storeB.Get(ctxB, ref)
	if err != nil {
		t.Fatalf("expected nil error for tenant isolation, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice for tenant B, got %d entries", len(got))
	}
}
