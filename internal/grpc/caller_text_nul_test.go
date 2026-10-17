package grpc

import (
	"reflect"
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

// A transition name or If-Match holding U+0000 is refused on the gRPC door, as
// on HTTP: 400 BAD_REQUEST in the error envelope, and the entity unchanged.
// gRPC is the only door on which an If-Match holding U+0000 reaches a
// single-entity write: net/http refuses control characters in header values.
func TestRPC_CallerTextHoldingNUL_Refused(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})
	entityID, _ := createEntityGRPC(t, svc, ctx, "person", map[string]any{"name": "Alice"})

	before, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	for name, send := range map[string]func(t *testing.T) (bool, string, string){
		"patch with an If-Match holding U+0000": func(t *testing.T) (bool, string, string) {
			resp, err := svc.EntityManage(ctx, makeCE(EntityPatchRequest, map[string]any{
				"id": "test", "patchFormat": "MERGE_PATCH",
				"payload": map[string]any{"entityId": entityID, "ifMatch": "\x00", "patch": map[string]any{"name": "Bob"}},
			}))
			if err != nil {
				t.Fatalf("unexpected gRPC error: %v", err)
			}
			var typed events.EntityTransactionResponseJson
			validateResponse(t, resp, &typed)
			if typed.Error == nil {
				return typed.Success, "", ""
			}
			return typed.Success, typed.Error.Code, typed.Error.Message
		},
		"patch with a transition holding U+0000": func(t *testing.T) (bool, string, string) {
			resp, err := svc.EntityManage(ctx, makeCE(EntityPatchRequest, map[string]any{
				"id": "test", "patchFormat": "MERGE_PATCH",
				"payload": map[string]any{"entityId": entityID, "ifMatch": "*", "transition": "g\x00o", "patch": map[string]any{"name": "Bob"}},
			}))
			if err != nil {
				t.Fatalf("unexpected gRPC error: %v", err)
			}
			var typed events.EntityTransactionResponseJson
			validateResponse(t, resp, &typed)
			if typed.Error == nil {
				return typed.Success, "", ""
			}
			return typed.Success, typed.Error.Code, typed.Error.Message
		},
		"transition holding U+0000": func(t *testing.T) (bool, string, string) {
			resp, err := svc.EntityManage(ctx, makeCE(EntityTransitionRequest, map[string]any{
				"id": "test", "entityId": entityID, "transition": "g\x00o",
			}))
			if err != nil {
				t.Fatalf("unexpected gRPC error: %v", err)
			}
			var typed events.EntityTransitionResponseJson
			validateResponse(t, resp, &typed)
			if typed.Error == nil {
				return typed.Success, "", ""
			}
			return typed.Success, typed.Error.Code, typed.Error.Message
		},
	} {
		t.Run(name, func(t *testing.T) {
			success, code, message := send(t)
			if success {
				t.Fatal("success = true; want the request refused")
			}
			if code != "CLIENT_ERROR" {
				t.Errorf("error code = %q, want CLIENT_ERROR", code)
			}
			if !strings.HasPrefix(message, common.ErrCodeBadRequest+":") {
				t.Errorf("message = %q, want prefix %q", message, common.ErrCodeBadRequest+":")
			}
			after, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
			if err != nil {
				t.Fatalf("GetEntity: %v", err)
			}
			if !reflect.DeepEqual(after.Data, before.Data) || !reflect.DeepEqual(after.Meta, before.Meta) {
				t.Errorf("entity changed: %+v / %+v, want %+v / %+v", after.Data, after.Meta, before.Data, before.Meta)
			}
		})
	}
}
