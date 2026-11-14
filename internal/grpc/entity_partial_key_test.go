package grpc

import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// TestRPC_EntityWrite_PartialKeyDoesNotExtendModel: on every gRPC door that
// can extend a model, a write whose data holds only part of a composite unique
// key AND a field the model does not have is refused with INVALID_UNIQUE_KEY
// before the schema check, so the model does not gain the field. A schema
// extension stays even when its write fails, so the key check must come
// first. A collection create checks every item's key before any item's schema,
// so an earlier valid item that adds the field does not extend the model
// either. A collection update updates item by item, and each item's key is
// checked before that item's own schema check.
func TestRPC_EntityWrite_PartialKeyDoesNotExtendModel(t *testing.T) {
	const field = "partialKeyNewField"
	partial := map[string]any{"name": "Part", field: "v"}
	extending := map[string]any{"name": "Whole", "amount": 2, field: "v"}

	createItem := func(model string, data map[string]any) map[string]any {
		return map[string]any{"model": map[string]any{"name": model, "version": 1}, "data": data}
	}

	doors := []struct {
		name  string
		write func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope
	}{
		{"Create", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			return manageEnvelope(t, svc, ctx, makeCE(EntityCreateRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payload": createItem(model, partial),
			}))
		}},
		{"Update", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			id := seedKeyedEntity(t, svc, ctx, model)
			return manageEnvelope(t, svc, ctx, makeCE(EntityUpdateRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payload": map[string]any{"entityId": id, "data": partial},
			}))
		}},
		{"CreateCollection", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			return collectionEnvelope(t, svc, ctx, makeCE(EntityCreateCollectionRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payloads": []any{createItem(model, partial)},
			}))
		}},
		{"CreateCollectionExtendingItemFirst", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			return collectionEnvelope(t, svc, ctx, makeCE(EntityCreateCollectionRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payloads": []any{
					createItem(model, extending),
					createItem(model, map[string]any{"name": "Part"}),
				},
			}))
		}},
		{"UpdateCollection", func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) rpcEnvelope {
			id := seedKeyedEntity(t, svc, ctx, model)
			return collectionEnvelope(t, svc, ctx, makeCE(EntityUpdateCollectionRequest, map[string]any{
				"id": "test", "dataFormat": "JSON", "payloads": []any{map[string]any{"entityId": id, "data": partial}},
			}))
		}},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			model := "partial-key-noext-" + strings.ToLower(door.name)
			importSetKeysAndLockModel(t, svc, ctx, model, "1",
				map[string]any{"name": "a", "amount": 1},
				[]spi.UniqueKey{{ID: "name-amount", Fields: []string{"$.name", "$.amount"}}},
			)
			if err := svc.modelHandler.SetChangeLevel(ctx, model, "1", "STRUCTURAL"); err != nil {
				t.Fatalf("SetChangeLevel(STRUCTURAL): %v", err)
			}

			env := door.write(t, svc, ctx, model)
			if env.success {
				t.Fatal("expected success=false: the write holds a partial composite key")
			}
			if env.class != "CLIENT_ERROR" || !strings.Contains(env.message, "INVALID_UNIQUE_KEY") {
				t.Fatalf("expected CLIENT_ERROR with INVALID_UNIQUE_KEY, got class=%s message=%s", env.class, env.message)
			}

			exported, err := svc.modelHandler.ExportModel(ctx, model, "1", "JSON_SCHEMA")
			if err != nil {
				t.Fatalf("export model: %v", err)
			}
			if strings.Contains(string(exported.Payload), field) {
				t.Errorf("model %s gained %q from a write refused for a partial key: %s", model, field, exported.Payload)
			}
		})
	}
}

// seedKeyedEntity creates an entity with the whole key and returns its id.
func seedKeyedEntity(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) string {
	t.Helper()
	resp := manageReply(t, svc, ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "seed", "dataFormat": "JSON", "payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "Kept", "amount": 1},
		},
	}))
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success || len(typed.TransactionInfo.EntityIds) == 0 {
		t.Fatalf("seeding the entity failed: %+v", typed.Error)
	}
	return typed.TransactionInfo.EntityIds[0]
}
