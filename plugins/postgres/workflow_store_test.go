package postgres_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func setupWorkflowTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func sampleWorkflows() []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{
		{
			Version:      "1.1",
			Name:         "default",
			InitialState: "NONE",
			Active:       true,
			States: map[string]spi.StateDefinition{
				"NONE":    {Transitions: []spi.TransitionDefinition{{Name: "create", Next: "CREATED"}}},
				"CREATED": {},
			},
		},
	}
}

func TestWorkflowStore_SaveAndGet(t *testing.T) {
	factory := setupWorkflowTest(t)
	ctx := ctxWithTenant("wf-tenant")

	store, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	wfs := sampleWorkflows()

	if err := store.Save(ctx, ref, wfs); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(got))
	}
	if got[0].Name != "default" {
		t.Errorf("expected name 'default', got %q", got[0].Name)
	}
	if got[0].InitialState != "NONE" {
		t.Errorf("expected initialState 'NONE', got %q", got[0].InitialState)
	}
	if len(got[0].States) != 2 {
		t.Errorf("expected 2 states, got %d", len(got[0].States))
	}
}

func TestWorkflowStore_SaveOverwrites(t *testing.T) {
	factory := setupWorkflowTest(t)
	ctx := ctxWithTenant("wf-tenant")
	store, _ := factory.WorkflowStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	store.Save(ctx, ref, sampleWorkflows())

	updated := []spi.WorkflowDefinition{{
		Version: "1.1", Name: "updated", InitialState: "START", Active: true,
		States: map[string]spi.StateDefinition{"START": {}},
	}}
	store.Save(ctx, ref, updated)

	got, _ := store.Get(ctx, ref)
	if len(got) != 1 || got[0].Name != "updated" {
		t.Errorf("expected overwritten workflow 'updated', got %v", got)
	}
}

func TestWorkflowStore_GetNotFound(t *testing.T) {
	factory := setupWorkflowTest(t)
	ctx := ctxWithTenant("wf-tenant")
	store, _ := factory.WorkflowStore(ctx)

	ref := spi.ModelRef{EntityName: "Nonexistent", ModelVersion: "1"}
	got, err := store.Get(ctx, ref)
	// SPI contract: Get for an unknown model returns an empty slice, not an error.
	if err != nil {
		t.Fatalf("expected nil error for nonexistent workflows, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice for nonexistent workflows, got %d items", len(got))
	}
}

func TestWorkflowStore_Delete(t *testing.T) {
	factory := setupWorkflowTest(t)
	ctx := ctxWithTenant("wf-tenant")
	store, _ := factory.WorkflowStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	store.Save(ctx, ref, sampleWorkflows())

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// SPI contract: Get after Delete returns empty slice, not an error.
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("expected nil error after delete, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice after delete, got %d items", len(got))
	}
}

func TestWorkflowStore_DeleteNonexistent(t *testing.T) {
	factory := setupWorkflowTest(t)
	ctx := ctxWithTenant("wf-tenant")
	store, _ := factory.WorkflowStore(ctx)

	ref := spi.ModelRef{EntityName: "Nonexistent", ModelVersion: "1"}
	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete nonexistent should not error: %v", err)
	}
}

func TestWorkflowStore_TenantIsolation(t *testing.T) {
	factory := setupWorkflowTest(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, _ := factory.WorkflowStore(ctxA)
	storeB, _ := factory.WorkflowStore(ctxB)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	storeA.Save(ctxA, ref, sampleWorkflows())

	// SPI contract: Get for a model that exists only in tenant-A returns empty
	// slice for tenant-B, not an error.
	got, err := storeB.Get(ctxB, ref)
	if err != nil {
		t.Fatalf("expected nil error for tenant-B (isolation), got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tenant-B should see 0 workflows (tenant isolation), got %d", len(got))
	}
}
