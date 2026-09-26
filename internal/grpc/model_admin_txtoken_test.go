package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
)

// Model and workflow administration never runs inside a transaction. On the
// gRPC door the transaction token rides as tx-token metadata; EntityModelManage
// refuses a state-changing request that carries one in the request's own
// envelope, before anything is read or written. The token's validity is not
// consulted: the value here names nothing.

// withTxToken puts a transaction token on the incoming metadata, as a callback
// from a compute member presents.
func withTxToken(ctx context.Context) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(proxy.GRPCTxTokenKey, "not-a-real-pass"))
}

// modelEnvelope is the success/error part every model response shares.
type modelEnvelope struct {
	Success bool `json:"success"`
	Error   *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func requireAdminRefusal(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, eventType, wantResponseType string, fields map[string]any) {
	t.Helper()
	resp, err := svc.EntityModelManage(withTxToken(ctx), makeCE(eventType, fields))
	if err != nil {
		t.Fatalf("transport error, want an envelope refusal: %v", err)
	}
	if resp.Type != wantResponseType {
		t.Fatalf("response type = %s, want %s", resp.Type, wantResponseType)
	}
	_, payload, err := ParseCloudEvent(resp)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	var env modelEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Success || env.Error == nil {
		t.Fatalf("envelope = %s, want success=false with an error", payload)
	}
	if env.Error.Code != "CLIENT_ERROR" || env.Error.Retryable {
		t.Errorf("error = %+v, want CLIENT_ERROR, not retryable", env.Error)
	}
	if !strings.HasPrefix(env.Error.Message, common.ErrCodeModelAdminInJoinedTransaction+":") {
		t.Errorf("message = %q, want prefix %q", env.Error.Message, common.ErrCodeModelAdminInJoinedTransaction+":")
	}
	if !strings.Contains(env.Error.Message, "cannot run inside a transaction") {
		t.Errorf("message = %q, want the reason", env.Error.Message)
	}
}

func TestRPC_ModelImport_TxToken_Refused(t *testing.T) {
	svc, ctx := newTestEnv(t)
	requireAdminRefusal(t, svc, ctx, EntityModelImportRequest, EntityModelImportResponse, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "adm-import", "version": 1},
		"dataFormat": "JSON",
		"converter":  "SAMPLE_DATA",
		"payload":    map[string]any{"name": "A"},
	})
	// Nothing was written: the model does not exist.
	if _, err := svc.modelHandler.ExportModel(ctx, "adm-import", "1", "JSON_SCHEMA"); err == nil {
		t.Fatal("model exists after a refused import")
	}
}

func TestRPC_ModelTransition_TxToken_Refused(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importModel(t, svc, ctx, "adm-lock")
	for _, transition := range []string{"LOCK", "UNLOCK"} {
		requireAdminRefusal(t, svc, ctx, EntityModelTransitionRequest, EntityModelTransitionResponse, map[string]any{
			"id":         "test",
			"model":      map[string]any{"name": "adm-lock", "version": 1},
			"transition": transition,
		})
	}
	// Nothing was written: the model is still unlocked, so a lock succeeds.
	if _, err := svc.modelHandler.LockModel(ctx, "adm-lock", "1"); err != nil {
		t.Fatalf("LockModel after a refused transition: %v", err)
	}
}

func TestRPC_ModelDelete_TxToken_Refused(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importModel(t, svc, ctx, "adm-delete")
	requireAdminRefusal(t, svc, ctx, EntityModelDeleteRequest, EntityModelDeleteResponse, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "adm-delete", "version": 1},
	})
	if _, err := svc.modelHandler.ExportModel(ctx, "adm-delete", "1", "JSON_SCHEMA"); err != nil {
		t.Fatalf("model gone after a refused delete: %v", err)
	}
}

func TestRPC_ModelSetUniqueKeys_TxToken_Refused(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importModel(t, svc, ctx, "adm-keys")
	requireAdminRefusal(t, svc, ctx, EntityModelSetUniqueKeysRequest, EntityModelSetUniqueKeysResponse, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "adm-keys", "version": 1},
		"uniqueKeys": []map[string]any{{"id": "uk1", "fields": []string{"$.name"}}},
	})
}

// The read-only model requests are not administration: a token on them is
// neither refused nor joined (EntityModelManage is not a tx-routed RPC).
func TestRPC_ModelReads_TxToken_NotRefused(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importModel(t, svc, ctx, "adm-read")

	resp, err := svc.EntityModelManage(withTxToken(ctx), makeCE(EntityModelExportRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "adm-read", "version": 1},
		"converter": "SIMPLE_VIEW",
	}))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var export events.EntityModelExportResponseJson
	validateResponse(t, resp, &export)
	if !export.Success {
		t.Fatalf("export with a token: %+v", export.Error)
	}

	resp, err = svc.EntityModelManage(withTxToken(ctx), makeCE(EntityModelGetAllRequest, map[string]any{"id": "test"}))
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	var all events.EntityModelGetAllResponseJson
	validateResponse(t, resp, &all)
	if !all.Success {
		t.Fatalf("get all with a token: %+v", all.Error)
	}
}

// importModel imports (and does not lock) a one-field model.
func importModel(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, name string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"name": "Alice"})
	if _, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName: name, ModelVersion: "1", Format: "JSON", Converter: "SAMPLE_DATA", Data: data,
	}); err != nil {
		t.Fatalf("import %s: %v", name, err)
	}
}
