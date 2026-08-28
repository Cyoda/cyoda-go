package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md. Both rules land on doors reachable here: the
// entity write validates against the declared kind set, and the model import
// reads its payload as a document or a collection of documents.

// TestRPC_EntityCreate_KindMismatch_400 pins the strict-validation door over
// gRPC: a value whose kind the field does not declare comes back as an envelope
// CLIENT_ERROR that explains the mismatch.
func TestRPC_EntityCreate_KindMismatch_400(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "kind-grpc", "1", map[string]any{"s": "x"})

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "kind-grpc", "version": 1},
			"data":  map[string]any{"s": []any{"A"}},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for an array written into a scalar field")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "expected scalar, got array") {
		t.Errorf("message must explain the kind mismatch, got %s", typed.Error.Message)
	}
}

// TestRPC_ModelImport_CollectionBody_200 pins the collection reading over gRPC:
// an array payload derives the merge of its documents, and the registered model
// then admits each of them.
func TestRPC_ModelImport_CollectionBody_200(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityModelImportRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "collection-grpc", "version": 1},
		"dataFormat": "JSON",
		"converter":  "SAMPLE_DATA",
		"payload": []any{
			map[string]any{"name": "A", "tags": []any{"A", "B"}},
			map[string]any{"name": "B", "sku": 1},
		},
	})
	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntityModelImportResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true for a collection of sample documents, got %+v", typed.Error)
	}
	if _, err := svc.modelHandler.LockModel(ctx, "collection-grpc", "1"); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	for _, data := range []map[string]any{
		{"name": "A", "tags": []any{"A", "B"}},
		{"name": "B", "sku": 1},
	} {
		write := makeCE(EntityCreateRequest, map[string]any{
			"id":         "test",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "collection-grpc", "version": 1},
				"data":  data,
			},
		})
		wResp, err := svc.EntityManage(ctx, write)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var wTyped events.EntityTransactionResponseJson
		validateResponse(t, wResp, &wTyped)
		if !wTyped.Success {
			t.Errorf("the model must admit the document it was derived from (%v), got %+v", data, wTyped.Error)
		}
	}
}

// TestRPC_ModelImport_NonDocumentBody_400 pins the boundary: a payload that is
// no kind of document collection is refused rather than registered.
func TestRPC_ModelImport_NonDocumentBody_400(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityModelImportRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "non-document-grpc", "version": 1},
		"dataFormat": "JSON",
		"converter":  "SAMPLE_DATA",
		"payload":    []any{"A", "B"},
	})
	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityModelImportResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for a payload of scalars")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "VALIDATION_FAILED") {
		t.Errorf("expected VALIDATION_FAILED in message, got %s", typed.Error.Message)
	}
}
