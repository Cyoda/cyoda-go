package grpc

import (
	"context"
	"strings"
	"testing"
)

// TestRPC_EntityCreate_ModelNeverLocked: an entity is created only in a
// LOCKED model. On the single and the collection create door, a create into a
// model that was imported and never locked answers success=false with a
// client error carrying MODEL_NOT_LOCKED, not retryable.
func TestRPC_EntityCreate_ModelNeverLocked(t *testing.T) {
	item := func(model string) map[string]any {
		return map[string]any{"model": map[string]any{"name": model, "version": 1}, "data": map[string]any{"name": "Bob"}}
	}
	doors := []struct {
		name   string
		create func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope
	}{
		{"Create", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			return manageEnvelope(t, svc, ctx, makeCE(EntityCreateRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payload": item(model),
			}))
		}},
		{"CreateCollection", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			return collectionEnvelope(t, svc, ctx, makeCE(EntityCreateCollectionRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payloads": []any{item(model)},
			}))
		}},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			model := "never-locked-" + strings.ToLower(door.name)
			importModel(t, svc, ctx, model)

			env := door.create(t, svc, ctx, model)
			if env.success {
				t.Fatal("expected success=false: the model is not locked")
			}
			if env.class != "CLIENT_ERROR" || !strings.HasPrefix(env.message, "MODEL_NOT_LOCKED:") {
				t.Fatalf("expected CLIENT_ERROR with a message prefixed MODEL_NOT_LOCKED:, got class=%s message=%s", env.class, env.message)
			}
			if env.retryable {
				t.Error("MODEL_NOT_LOCKED is not retryable")
			}
		})
	}
}
