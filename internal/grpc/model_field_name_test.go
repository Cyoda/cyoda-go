package grpc

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md. Both field-set-establishing doors — the model
// import and the ChangeLevel-driven extension on an entity write — are reachable
// here, so both are pinned: an unaddressable field name must come back as an
// envelope CLIENT_ERROR carrying VALIDATION_FAILED and naming the offending key.

// TestRPC_ModelImport_UnaddressableFieldName_400 pins the explicit-import door.
func TestRPC_ModelImport_UnaddressableFieldName_400(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityModelImportRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "fieldname-grpc", "version": 1},
		"dataFormat": "JSON",
		"converter":  "SAMPLE_DATA",
		"payload":    map[string]any{"first name": "x"},
	})
	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityModelImportResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for an unaddressable field name")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "VALIDATION_FAILED") {
		t.Errorf("expected VALIDATION_FAILED in message, got %s", typed.Error.Message)
	}
	if !strings.Contains(typed.Error.Message, "first name") {
		t.Errorf("message must name the offending field, got %s", typed.Error.Message)
	}
}

// TestRPC_EntityCreate_UnaddressableFieldName_400 pins the auto-evolution door:
// a locked model with a ChangeLevel is allowed to grow its field set on a
// write, which is exactly when the name rule has to fire.
func TestRPC_EntityCreate_UnaddressableFieldName_400(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "fieldname-extend", "1", map[string]any{"name": "Alice"})
	if err := svc.modelHandler.SetChangeLevel(ctx, "fieldname-extend", "1", string(spi.ChangeLevelStructural)); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "fieldname-extend", "version": 1},
			"data":  map[string]any{"name": "Alice", "first name": "x"},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for an unaddressable field name")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "VALIDATION_FAILED") {
		t.Errorf("expected VALIDATION_FAILED in message, got %s", typed.Error.Message)
	}
	if !strings.Contains(typed.Error.Message, "first name") {
		t.Errorf("message must name the offending field, got %s", typed.Error.Message)
	}
}

// TestRPC_EntityCreate_AddressableFieldName_200 is the positive control on the
// auto-evolution door over gRPC.
func TestRPC_EntityCreate_AddressableFieldName_200(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "fieldname-extend-ok", "1", map[string]any{"name": "Alice"})
	if err := svc.modelHandler.SetChangeLevel(ctx, "fieldname-extend-ok", "1", string(spi.ChangeLevelStructural)); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "fieldname-extend-ok", "version": 1},
			"data":  map[string]any{"name": "Alice", "new-field_9": "yes"},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true, got %+v", typed.Error)
	}
}
