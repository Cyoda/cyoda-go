package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda/cyoda-go/api/grpc/events"
)

// Unlock of a model that holds entities is refused over gRPC as over HTTP: the
// error envelope carries CLIENT_ERROR and the MODEL_HAS_ENTITIES code in its
// message, and the model stays LOCKED.
func TestRPC_ModelTransitionUnlock_HasEntities_409(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "unlock-entities", "1", map[string]any{"name": "Alice"})
	createEntityGRPC(t, svc, ctx, "unlock-entities", map[string]any{"name": "Alice"})

	resp, err := svc.EntityModelManage(ctx, makeCE(EntityModelTransitionRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "unlock-entities", "version": 1},
		"transition": "UNLOCK",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntityModelTransitionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for unlock of a model with entities")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_HAS_ENTITIES") {
		t.Errorf("expected message to contain MODEL_HAS_ENTITIES, got %s", typed.Error.Message)
	}

	models, err := svc.modelHandler.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	found := false
	for _, m := range models {
		if m.Name == "unlock-entities" && m.Version == 1 {
			found = true
			if m.State != "LOCKED" {
				t.Errorf("model state = %s, want LOCKED", m.State)
			}
		}
	}
	if !found {
		t.Error("model unlock-entities/1 not listed")
	}
}
