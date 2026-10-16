package entity

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestBatchCreate_StateAndTransitionForLatestSave: a batch create sets each
// entity's initial state and TransitionForLatestSave exactly as the single
// create does — the state the engine sets, and "loopback".
func TestBatchCreate_StateAndTransitionForLatestSave(t *testing.T) {
	h, ctx := newOrderTestHandler(t)
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	wfStore, err := h.factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := wfStore.Save(ctx, ref, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "OrderWF", InitialState: "PLACED", Active: true,
		States: map[string]spi.StateDefinition{"PLACED": {}},
	}}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}

	single, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName: "Order", ModelVersion: "1", Format: "JSON",
		Data: json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	batch, err := h.CreateEntityCollection(ctx, []CollectionItem{
		{ModelName: "Order", ModelVersion: 1, Payload: json.RawMessage(`{"email":"b@b.com","accountId":"acc-2"}`)},
		{ModelName: "Order", ModelVersion: 1, Payload: json.RawMessage(`{"email":"c@b.com","accountId":"acc-3"}`)},
	})
	if err != nil {
		t.Fatalf("CreateEntityCollection: %v", err)
	}

	store, err := h.factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for _, id := range append(single.EntityIDs, batch.EntityIDs...) {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.Meta.State != "PLACED" || got.Meta.TransitionForLatestSave != "loopback" {
			t.Errorf("entity %s: state %q, transitionForLatestSave %q; want PLACED and loopback",
				id, got.Meta.State, got.Meta.TransitionForLatestSave)
		}
	}
}
