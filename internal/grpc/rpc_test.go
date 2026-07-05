package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newTestEnv creates a CloudEventsServiceImpl wired to real in-memory stores
// and a context with a test user/tenant injected.
func newTestEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context) {
	t.Helper()

	factory := memory.NewStoreFactory()
	factory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := factory.GetTransactionManager()

	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: "test-tenant", Name: "Test Tenant"},
		Roles:    []string{"ADMIN"},
	}

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchService)
	modelHandler := model.New(factory)

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	ctx := spi.WithUserContext(context.Background(), uc)
	return svc, ctx
}

// importAndLockModel is a test helper that imports a model with sample data
// and locks it so entities can be created against it.
func importAndLockModel(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, entityName, version string, sampleData map[string]any) {
	t.Helper()
	dataBytes, err := json.Marshal(sampleData)
	if err != nil {
		t.Fatalf("failed to marshal sample data: %v", err)
	}
	_, err = svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   entityName,
		ModelVersion: version,
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	})
	if err != nil {
		t.Fatalf("failed to import model: %v", err)
	}
	_, err = svc.modelHandler.LockModel(ctx, entityName, version)
	if err != nil {
		t.Fatalf("failed to lock model: %v", err)
	}
}

// makeCE builds a CloudEvent with the given type and JSON payload fields.
func makeCE(eventType string, fields map[string]any) *cepb.CloudEvent {
	data, _ := json.Marshal(fields)
	return &cepb.CloudEvent{
		Id:          "test-req-1",
		Source:      "test",
		SpecVersion: "1.0",
		Type:        eventType,
		Data:        &cepb.CloudEvent_TextData{TextData: string(data)},
	}
}

// parseResponsePayload extracts the JSON payload from a response CloudEvent.
func parseResponsePayload(t *testing.T, ce *cepb.CloudEvent) map[string]any {
	t.Helper()
	td, ok := ce.Data.(*cepb.CloudEvent_TextData)
	if !ok {
		t.Fatal("expected text_data in response")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(td.TextData), &m); err != nil {
		t.Fatalf("failed to unmarshal response payload: %v", err)
	}
	return m
}

// validateResponse parses a CloudEvent response and validates it against the
// generated schema type. If any required field is missing, UnmarshalJSON
// returns an error and the test fails.
func validateResponse(t *testing.T, ce *cepb.CloudEvent, target any) {
	t.Helper()
	td, ok := ce.Data.(*cepb.CloudEvent_TextData)
	if !ok {
		t.Fatal("expected text_data in response")
	}
	if err := json.Unmarshal([]byte(td.TextData), target); err != nil {
		t.Fatalf("response does not match schema: %v\nPayload: %s", err, td.TextData)
	}
}

// --- Entity tests ---

func TestRPC_EntityCreate(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice", "age": 30},
		},
	})

	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityTransactionResponse {
		t.Errorf("expected type %s, got %s", EntityTransactionResponse, resp.Type)
	}

	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.RequestID == "" {
		t.Error("missing requestId")
	}
	if len(typed.TransactionInfo.EntityIds) == 0 {
		t.Fatal("expected non-empty entityIds in transactionInfo")
	}
}

func TestRPC_EntityDelete(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create an entity first.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	// Delete the entity.
	deleteCE := makeCE(EntityDeleteRequest, map[string]any{
		"id":       "test",
		"entityId": entityID,
	})

	resp, err := svc.EntityManage(ctx, deleteCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityDeleteResponse {
		t.Errorf("expected type %s, got %s", EntityDeleteResponse, resp.Type)
	}

	var typed events.EntityDeleteResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.EntityID != entityID {
		t.Errorf("expected entityId=%s, got %s", entityID, typed.EntityID)
	}
	if typed.RequestID == "" {
		t.Error("missing requestId")
	}
}

func TestRPC_EntityTransition(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create entity.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	// Transition the entity using the default workflow's UPDATE transition
	// (no custom workflow configured, so the default workflow applies).
	transitionCE := makeCE(EntityTransitionRequest, map[string]any{
		"id":         "test",
		"entityId":   entityID,
		"transition": "UPDATE",
	})

	resp, err := svc.EntityManage(ctx, transitionCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityTransitionResponse {
		t.Errorf("expected type %s, got %s", EntityTransitionResponse, resp.Type)
	}

	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
}

func TestRPC_EntityManageUnsupportedType(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE("com.cyoda.unknown.request", map[string]any{
		"id": "test",
	})
	_, err := svc.EntityManage(ctx, ce)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported entity event type") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// --- Model tests ---

func TestRPC_ModelImport(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityModelImportRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "product", "version": 1},
		"dataFormat": "JSON",
		"converter":  "SAMPLE_DATA",
		"payload":    map[string]any{"field": "value"},
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelImportResponse {
		t.Errorf("expected type %s, got %s", EntityModelImportResponse, resp.Type)
	}

	var typed events.EntityModelImportResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.ModelID == "" {
		t.Error("expected non-empty modelId in response")
	}
}

func TestRPC_ModelExport(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Import a model first.
	importAndLockModel(t, svc, ctx, "product", "1", map[string]any{"field": "value"})

	// Unlock so we can test the export (export works on both locked/unlocked).
	// Actually, export works on any state, and we already have a locked model.

	ce := makeCE(EntityModelExportRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "product", "version": 1},
		"converter": "SIMPLE_VIEW",
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelExportResponse {
		t.Errorf("expected type %s, got %s", EntityModelExportResponse, resp.Type)
	}

	var typed events.EntityModelExportResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.Payload == nil {
		t.Error("expected non-nil payload in export response")
	}
}

func TestRPC_ModelTransitionLock(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Import (but don't lock) a model.
	dataBytes, _ := json.Marshal(map[string]any{"field": "value"})
	_, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   "product",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	})
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	ce := makeCE(EntityModelTransitionRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "product", "version": 1},
		"transition": "LOCK",
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelTransitionResponse {
		t.Errorf("expected type %s, got %s", EntityModelTransitionResponse, resp.Type)
	}

	var typed events.EntityModelTransitionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.State != "LOCKED" {
		t.Errorf("expected state=LOCKED, got %s", typed.State)
	}
}

func TestRPC_ModelDelete(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Import a model (unlocked, no entities) so it can be deleted.
	dataBytes, _ := json.Marshal(map[string]any{"field": "value"})
	_, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   "product",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	})
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	ce := makeCE(EntityModelDeleteRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "product", "version": 1},
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelDeleteResponse {
		t.Errorf("expected type %s, got %s", EntityModelDeleteResponse, resp.Type)
	}

	var typed events.EntityModelDeleteResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
}

func TestRPC_ModelGetAll(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Import two models.
	for _, name := range []string{"alpha", "beta"} {
		dataBytes, _ := json.Marshal(map[string]any{"field": name})
		_, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
			EntityName:   name,
			ModelVersion: "1",
			Format:       "JSON",
			Converter:    "SAMPLE_DATA",
			Data:         dataBytes,
		})
		if err != nil {
			t.Fatalf("import %s failed: %v", name, err)
		}
	}

	ce := makeCE(EntityModelGetAllRequest, map[string]any{
		"id": "test",
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelGetAllResponse {
		t.Errorf("expected type %s, got %s", EntityModelGetAllResponse, resp.Type)
	}

	var typed events.EntityModelGetAllResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if len(typed.Models) != 2 {
		t.Errorf("expected 2 models, got %d", len(typed.Models))
	}
}

func TestRPC_ModelUnsupportedType(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE("com.cyoda.model.unknown", map[string]any{
		"id": "test",
	})
	_, err := svc.EntityModelManage(ctx, ce)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported model event type") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRPC_ModelSetUniqueKeys_200(t *testing.T) {
	svc, ctx := newTestEnv(t)

	dataBytes, _ := json.Marshal(map[string]any{"name": "Alice", "age": 30})
	_, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   "product",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	})
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	ce := makeCE(EntityModelSetUniqueKeysRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "product", "version": 1},
		"uniqueKeys": []map[string]any{
			{"id": "uk1", "fields": []string{"$.name"}},
		},
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelSetUniqueKeysResponse {
		t.Errorf("expected type %s, got %s", EntityModelSetUniqueKeysResponse, resp.Type)
	}

	var typed events.EntityModelTransitionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
}

func TestRPC_ModelSetUniqueKeys_409_Locked(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "product", "1", map[string]any{"name": "Alice"})

	ce := makeCE(EntityModelSetUniqueKeysRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "product", "version": 1},
		"uniqueKeys": []map[string]any{
			{"id": "uk1", "fields": []string{"$.name"}},
		},
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelSetUniqueKeysResponse {
		t.Errorf("expected type %s, got %s", EntityModelSetUniqueKeysResponse, resp.Type)
	}

	var typed events.EntityModelTransitionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for locked model")
	}
	if typed.Error == nil {
		t.Fatal("expected error in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code CLIENT_ERROR, got %s", typed.Error.Code)
	}
}

func TestRPC_ModelSetUniqueKeys_422_BadField(t *testing.T) {
	svc, ctx := newTestEnv(t)

	dataBytes, _ := json.Marshal(map[string]any{"name": "Alice"})
	_, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   "product",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	})
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	ce := makeCE(EntityModelSetUniqueKeysRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "product", "version": 1},
		"uniqueKeys": []map[string]any{
			{"id": "uk1", "fields": []string{"$.nonexistent_xyz"}},
		},
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelSetUniqueKeysResponse {
		t.Errorf("expected type %s, got %s", EntityModelSetUniqueKeysResponse, resp.Type)
	}

	var typed events.EntityModelTransitionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for bad field")
	}
	if typed.Error == nil {
		t.Fatal("expected error in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code CLIENT_ERROR, got %s", typed.Error.Code)
	}
}

func TestRPC_ModelSetUniqueKeys_422_Unsupported(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Replace the model handler with one wired to an incapable factory.
	// incapableFactory embeds spi.StoreFactory as an interface so the
	// concrete SupportsCompositeUniqueKeys method on the real factory is NOT
	// promoted — the type assertion in SetUniqueKeys returns ok=false.
	type incapableFactory struct{ spi.StoreFactory }
	svc.modelHandler = model.New(incapableFactory{memory.NewStoreFactory()})

	ce := makeCE(EntityModelSetUniqueKeysRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "product", "version": 1},
		"uniqueKeys": []map[string]any{
			{"id": "uk1", "fields": []string{"$.name"}},
		},
	})

	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityModelSetUniqueKeysResponse {
		t.Errorf("expected type %s, got %s", EntityModelSetUniqueKeysResponse, resp.Type)
	}

	var typed events.EntityModelTransitionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for unsupported backend")
	}
	if typed.Error == nil {
		t.Fatal("expected error in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "COMPOSITE_KEY_UNSUPPORTED") {
		t.Errorf("expected message to contain COMPOSITE_KEY_UNSUPPORTED, got %s", typed.Error.Message)
	}
}

// importSetKeysAndLockModel imports a model, sets composite unique keys on it
// (while still unlocked), then locks it. Use instead of importAndLockModel
// whenever entity-write unique-key enforcement is needed in a test.
func importSetKeysAndLockModel(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, entityName, version string, sampleData map[string]any, keys []spi.UniqueKey) {
	t.Helper()
	dataBytes, err := json.Marshal(sampleData)
	if err != nil {
		t.Fatalf("failed to marshal sample data: %v", err)
	}
	_, err = svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   entityName,
		ModelVersion: version,
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         dataBytes,
	})
	if err != nil {
		t.Fatalf("failed to import model: %v", err)
	}
	_, err = svc.modelHandler.SetUniqueKeys(ctx, entityName, version, keys)
	if err != nil {
		t.Fatalf("failed to set unique keys: %v", err)
	}
	_, err = svc.modelHandler.LockModel(ctx, entityName, version)
	if err != nil {
		t.Fatalf("failed to lock model: %v", err)
	}
}

// TestRPC_EntityCreate_UniqueViolation verifies that creating a second entity
// with the same composite-key values returns Success=false with a message
// containing UNIQUE_VIOLATION.
func TestRPC_EntityCreate_UniqueViolation(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importSetKeysAndLockModel(t, svc, ctx, "order", "1",
		map[string]any{"email": "a@b.com", "accountId": "acc-1"},
		[]spi.UniqueKey{{ID: "uk1", Fields: []string{"$.email", "$.accountId"}}},
	)

	createPayload := func(id string) *cepb.CloudEvent {
		return makeCE(EntityCreateRequest, map[string]any{
			"id":         id,
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "order", "version": 1},
				"data":  map[string]any{"email": "a@b.com", "accountId": "acc-1"},
			},
		})
	}

	// First create must succeed.
	resp1, err := svc.EntityManage(ctx, createPayload("req-1"))
	if err != nil {
		t.Fatalf("first create: unexpected gRPC error: %v", err)
	}
	var typed1 events.EntityTransactionResponseJson
	validateResponse(t, resp1, &typed1)
	if !typed1.Success {
		t.Fatalf("expected first create to succeed; got error: %v", typed1.Error)
	}

	// Second create with identical key values must fail.
	resp2, err := svc.EntityManage(ctx, createPayload("req-2"))
	if err != nil {
		t.Fatalf("second create: unexpected gRPC error: %v", err)
	}
	var typed2 events.EntityTransactionResponseJson
	validateResponse(t, resp2, &typed2)
	if typed2.Success {
		t.Fatal("expected second create to fail with UNIQUE_VIOLATION")
	}
	if typed2.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed2.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code CLIENT_ERROR, got %s", typed2.Error.Code)
	}
	if !strings.Contains(typed2.Error.Message, "UNIQUE_VIOLATION") {
		t.Errorf("expected message to contain UNIQUE_VIOLATION, got %s", typed2.Error.Message)
	}
}

// TestRPC_EntityCreate_InvalidUniqueKey verifies that creating an entity with
// only some fields of a composite unique key returns Success=false with a
// message containing INVALID_UNIQUE_KEY.
func TestRPC_EntityCreate_InvalidUniqueKey(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importSetKeysAndLockModel(t, svc, ctx, "order", "1",
		map[string]any{"email": "a@b.com", "accountId": "acc-1"},
		[]spi.UniqueKey{{ID: "uk1", Fields: []string{"$.email", "$.accountId"}}},
	)

	// Provide only one of the two key fields — partial key.
	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "req-1",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "order", "version": 1},
			"data":  map[string]any{"email": "a@b.com"}, // accountId absent
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected create to fail with INVALID_UNIQUE_KEY")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "INVALID_UNIQUE_KEY") {
		t.Errorf("expected message to contain INVALID_UNIQUE_KEY, got %s", typed.Error.Message)
	}
}

// --- Search tests ---

func TestRPC_EntityGet(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create entity.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	// Get entity via search.
	getCE := makeCE(EntityGetRequest, map[string]any{
		"id":       "test",
		"entityId": entityID,
	})

	resp, err := svc.EntitySearch(ctx, getCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntityResponse {
		t.Errorf("expected type %s, got %s", EntityResponse, resp.Type)
	}

	var typed events.EntityResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.RequestID == "" {
		t.Error("missing requestId")
	}
	if typed.Payload.Data == nil {
		t.Fatal("expected data in payload")
	}
	dataBytes, err := json.Marshal(typed.Payload.Data)
	if err != nil {
		t.Fatalf("failed to marshal payload data: %v", err)
	}
	var dataMap map[string]any
	if err := json.Unmarshal(dataBytes, &dataMap); err != nil {
		t.Fatalf("failed to unmarshal payload data: %v", err)
	}
	if dataMap["name"] != "Alice" {
		t.Errorf("expected name=Alice, got %v", dataMap["name"])
	}
}

func TestRPC_SnapshotSearch(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	// Create an entity so search has something to find.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice", "age": 30},
		},
	})
	_, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Submit async snapshot search.
	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":       "group",
			"operator":   "AND",
			"conditions": []any{},
		},
	})

	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntitySnapshotSearchResponse {
		t.Errorf("expected type %s, got %s", EntitySnapshotSearchResponse, resp.Type)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.Status.SnapshotID == "" {
		t.Error("expected non-empty snapshotId")
	}
}

func TestRPC_DirectSearch(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	// Create entity.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Bob"},
		},
	})
	_, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Direct search.
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":       "group",
			"operator":   "AND",
			"conditions": []any{},
		},
	})

	stream := &mockEntityStream{ctx: ctx}
	err = svc.EntitySearchCollection(ce, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected at least 1 response sent")
	}
	// Check first result has the right type.
	if stream.sent[0].Type != EntityResponse {
		t.Errorf("expected type %s, got %s", EntityResponse, stream.sent[0].Type)
	}

	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
}

// --- Search collection tests ---

func TestRPC_EntityGetAll(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create two entities.
	for _, name := range []string{"Alice", "Bob"} {
		ce := makeCE(EntityCreateRequest, map[string]any{
			"id":         "test",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": name},
			},
		})
		_, err := svc.EntityManage(ctx, ce)
		if err != nil {
			t.Fatalf("create %s failed: %v", name, err)
		}
	}

	ce := makeCE(EntityGetAllRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
	})

	stream := &mockEntityStream{ctx: ctx}
	err := svc.EntitySearchCollection(ce, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(stream.sent))
	}
	if stream.sent[0].Type != EntityResponse {
		t.Errorf("expected type %s, got %s", EntityResponse, stream.sent[0].Type)
	}

	for i, sent := range stream.sent {
		var typed events.EntityResponseJson
		validateResponse(t, sent, &typed)
		if !typed.Success {
			t.Errorf("response %d: expected success=true", i)
		}
	}
}

func TestRPC_EntityStats(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create an entity.
	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})
	_, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	statsCE := makeCE(EntityStatsGetRequest, map[string]any{
		"id": "test",
	})

	stream := &mockEntityStream{ctx: ctx}
	err = svc.EntitySearchCollection(statsCE, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) < 1 {
		t.Fatal("expected at least 1 stats response")
	}
	if stream.sent[0].Type != EntityStatsResponse {
		t.Errorf("expected type %s, got %s", EntityStatsResponse, stream.sent[0].Type)
	}

	var typed events.EntityStatsResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.RequestID == "" {
		t.Error("missing requestId")
	}
}

func TestRPC_EntityChangesMetadata(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create entity.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	changesCE := makeCE(EntityChangesMetadataGetRequest, map[string]any{
		"id":       "test",
		"entityId": entityID,
	})

	stream := &mockEntityStream{ctx: ctx}
	err = svc.EntitySearchCollection(changesCE, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) < 1 {
		t.Fatal("expected at least 1 change metadata response")
	}
	if stream.sent[0].Type != EntityChangesMetadataResponse {
		t.Errorf("expected type %s, got %s", EntityChangesMetadataResponse, stream.sent[0].Type)
	}

	var typed events.EntityChangesMetadataResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.RequestID == "" {
		t.Error("missing requestId")
	}
}

// --- Entity manage collection tests ---

func TestRPC_EntityCreateCollection(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	ce := makeCE(EntityCreateCollectionRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payloads": []any{
			map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": "A"},
			},
			map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": "B"},
			},
		},
	})

	stream := &mockManageStream{ctx: ctx}
	err := svc.EntityManageCollection(ce, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}

	var typed events.EntityTransactionResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if len(typed.TransactionInfo.EntityIds) != 2 {
		t.Errorf("expected 2 entityIds, got %d", len(typed.TransactionInfo.EntityIds))
	}
}

func TestRPC_EntityDeleteAll(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Create an entity first.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})
	_, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
	})

	stream := &mockManageStream{ctx: ctx}
	err = svc.EntityManageCollection(ce, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}
	if stream.sent[0].Type != EntityDeleteAllResponse {
		t.Errorf("expected type %s, got %s", EntityDeleteAllResponse, stream.sent[0].Type)
	}

	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
}

// TestRPC_EntityDeleteAll_Unconditional documents that gRPC EntityDeleteAllRequest
// is always unconditional (D2): it removes every entity of the model and returns
// Success, with no condition field available at the gRPC layer (conditional delete
// is HTTP-only). The test creates two entities, calls EntityDeleteAllRequest, asserts
// Success=true, then confirms an EntityGetAllRequest returns zero results.
func TestRPC_EntityDeleteAll_Unconditional(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	// Create two entities.
	for _, name := range []string{"Alice", "Bob"} {
		ce := makeCE(EntityCreateRequest, map[string]any{
			"id": "test", "dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": name, "age": 30},
			},
		})
		if _, err := svc.EntityManage(ctx, ce); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	// Delete all — gRPC has no condition field, this is always delete-all (D2).
	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
	})
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("EntityDeleteAll: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}
	if stream.sent[0].Type != EntityDeleteAllResponse {
		t.Errorf("expected type %s, got %s", EntityDeleteAllResponse, stream.sent[0].Type)
	}
	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}

	// Verify all entities are gone — gRPC unconditional delete-all must remove
	// every entity; no condition field exists at this layer (D2).
	getAllCE := makeCE(EntityGetAllRequest, map[string]any{
		"id":    "verify",
		"model": map[string]any{"name": "person", "version": 1},
	})
	verifyStream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(getAllCE, verifyStream); err != nil {
		t.Fatalf("GetAll after DeleteAll: %v", err)
	}
	if len(verifyStream.sent) != 0 {
		t.Errorf("expected 0 entities after DeleteAll, got %d (gRPC delete-all must be unconditional)", len(verifyStream.sent))
	}
}

// --- Search unsupported type ---

func TestRPC_SearchUnsupportedType(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE("com.cyoda.search.unknown", map[string]any{
		"id": "test",
	})
	_, err := svc.EntitySearch(ctx, ce)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported search event type") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// --- Snapshot cancel test ---

func TestRPC_SnapshotCancel(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Submit async search to get a snapshot ID.
	searchCE := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":       "group",
			"operator":   "AND",
			"conditions": []any{},
		},
	})
	searchResp, err := svc.EntitySearch(ctx, searchCE)
	if err != nil {
		t.Fatalf("snapshot search failed: %v", err)
	}
	searchPayload := parseResponsePayload(t, searchResp)
	statusMap := searchPayload["status"].(map[string]any)
	snapshotID := statusMap["snapshotId"].(string)

	// Cancel the snapshot.
	cancelCE := makeCE(SnapshotCancelRequest, map[string]any{
		"id":         "test",
		"snapshotId": snapshotID,
	})

	resp, err := svc.EntitySearch(ctx, cancelCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != EntitySnapshotSearchResponse {
		t.Errorf("expected type %s, got %s", EntitySnapshotSearchResponse, resp.Type)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
}

// --- Snapshot get status test ---

func TestRPC_SnapshotGetStatus(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	// Submit async search.
	searchCE := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":       "group",
			"operator":   "AND",
			"conditions": []any{},
		},
	})
	searchResp, err := svc.EntitySearch(ctx, searchCE)
	if err != nil {
		t.Fatalf("snapshot search failed: %v", err)
	}
	searchPayload := parseResponsePayload(t, searchResp)
	statusMap := searchPayload["status"].(map[string]any)
	snapshotID := statusMap["snapshotId"].(string)

	// Wait briefly for the async job to complete.
	time.Sleep(100 * time.Millisecond)

	// Get status.
	statusCE := makeCE(SnapshotGetStatusRequest, map[string]any{
		"id":         "test",
		"snapshotId": snapshotID,
	})

	resp, err := svc.EntitySearch(ctx, statusCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Error("expected success=true")
	}
	if typed.Status.SnapshotID != snapshotID {
		t.Errorf("expected snapshotId=%s, got %s", snapshotID, typed.Status.SnapshotID)
	}
}

// --- Error-path schema validation tests ---

func TestRPC_EntityCreate_Error_SchemaValid(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// Don't create any model — entity create will fail.
	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "nonexistent", "version": 1},
			"data":  map[string]any{"x": 1},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}

	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Error("expected error field to be populated")
	}
	if typed.RequestID == "" {
		t.Error("missing requestId in error response")
	}
}

func TestRPC_EntityDelete_Error_SchemaValid(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// Delete a non-existent entity.
	ce := makeCE(EntityDeleteRequest, map[string]any{
		"id":       "test",
		"entityId": "00000000-0000-0000-0000-000000000000",
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}

	var typed events.EntityDeleteResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Error("expected error field to be populated")
	}
	if typed.RequestID == "" {
		t.Error("missing requestId in error response")
	}
}

func TestRPC_ModelExport_Error_SchemaValid(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// Export a non-existent model.
	ce := makeCE(EntityModelExportRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "nonexistent", "version": 1},
		"converter": "SIMPLE_VIEW",
	})
	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}

	var typed events.EntityModelExportResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Error("expected error field to be populated")
	}
}

func TestRPC_EntityGet_Error_SchemaValid(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// Get a non-existent entity.
	ce := makeCE(EntityGetRequest, map[string]any{
		"id":       "test",
		"entityId": "00000000-0000-0000-0000-000000000000",
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}

	var typed events.EntityResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Error("expected error field to be populated")
	}
	if typed.RequestID == "" {
		t.Error("missing requestId in error response")
	}
}

func TestRPC_SnapshotSearch_Error_SchemaValid(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// Get status of a non-existent snapshot to trigger an error path.
	ce := makeCE(SnapshotGetStatusRequest, map[string]any{
		"id":         "test",
		"snapshotId": "00000000-0000-0000-0000-000000000000",
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error == nil {
		t.Error("expected error field to be populated")
	}
}

func TestRPC_EntityPatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "Alice", "age": 30}},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"age": 31}, "ifMatch": "*"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success; got %#v", typed.Error)
	}
	if typed.RequestID == "" {
		t.Error("missing requestId")
	}
	if len(typed.TransactionInfo.EntityIds) == 0 {
		t.Error("expected at least one entityId in response")
	}
}

func TestRPC_EntityPatch_MissingIfMatch428(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A"})
	createResp, _ := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A"}},
	}))
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"name": "B"}, "ifMatch": ""},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected failure envelope")
	}
	if !strings.Contains(typed.Error.Message, "PRECONDITION_REQUIRED") {
		t.Errorf("expected PRECONDITION_REQUIRED in message, got %q", typed.Error.Message)
	}

	// Also cover the absent-ifMatch branch (*string == nil): omit the ifMatch key entirely.
	patchCENoKey := makeCE(EntityPatchRequest, map[string]any{
		"id": "p2", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"name": "C"}},
	})
	resp2, err := svc.EntityManage(ctx, patchCENoKey)
	if err != nil {
		t.Fatalf("unexpected transport error (absent ifMatch): %v", err)
	}
	var typed2 events.EntityTransactionResponseJson
	validateResponse(t, resp2, &typed2)
	if typed2.Success || typed2.Error == nil {
		t.Fatalf("expected failure envelope for absent ifMatch")
	}
	if !strings.Contains(typed2.Error.Message, "PRECONDITION_REQUIRED") {
		t.Errorf("expected PRECONDITION_REQUIRED in message for absent ifMatch, got %q", typed2.Error.Message)
	}
}

func TestRPC_EntityPatch_JSONPatchNotImplemented(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A", "amount": 1})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "JSON_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": []any{}, "ifMatch": "*"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected failure envelope")
	}
	if !strings.Contains(typed.Error.Message, "NOT_IMPLEMENTED") {
		t.Errorf("expected NOT_IMPLEMENTED in message, got %q", typed.Error.Message)
	}
}

func TestRPC_EntityPatch_StaleTokenIs412(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A", "amount": 1})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)
	createTxID := txInfo["transactionId"].(string)

	// First patch with the create txId — should succeed and advance the entity.
	firstPatchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"amount": 31}, "ifMatch": createTxID},
	})
	firstResp, err := svc.EntityManage(ctx, firstPatchCE)
	if err != nil {
		t.Fatalf("first patch transport error: %v", err)
	}
	var firstTyped events.EntityTransactionResponseJson
	validateResponse(t, firstResp, &firstTyped)
	if !firstTyped.Success {
		t.Fatalf("expected first patch to succeed; got %#v", firstTyped.Error)
	}

	// Second patch with the same now-stale createTxID — should fail with ENTITY_MODIFIED.
	stalePatchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p2", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"amount": 99}, "ifMatch": createTxID},
	})
	staleResp, err := svc.EntityManage(ctx, stalePatchCE)
	if err != nil {
		t.Fatalf("stale patch transport error: %v", err)
	}
	var staleTyped events.EntityTransactionResponseJson
	validateResponse(t, staleResp, &staleTyped)
	if staleTyped.Success || staleTyped.Error == nil {
		t.Fatalf("expected failure envelope for stale token")
	}
	if !strings.Contains(staleTyped.Error.Message, "ENTITY_MODIFIED") {
		t.Errorf("expected ENTITY_MODIFIED in message, got %q", staleTyped.Error.Message)
	}
}

func TestRPC_EntityPatch_StarUnconditional(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A", "amount": 1})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"amount": 42}, "ifMatch": "*"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success; got %#v", typed.Error)
	}

	// Read back and verify the field was applied.
	envelope, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	dataBytes, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatalf("marshal entity data: %v", err)
	}
	var dataMap map[string]any
	if err := json.Unmarshal(dataBytes, &dataMap); err != nil {
		t.Fatalf("unmarshal entity data: %v", err)
	}
	// json.Unmarshal decodes numbers as float64 by default.
	if dataMap["amount"] != float64(42) {
		t.Errorf("expected amount=42, got %v", dataMap["amount"])
	}
}

func TestRPC_EntityPatch_NotFound(t *testing.T) {
	svc, ctx := newTestEnv(t)

	missingID := uuid.NewString()
	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": missingID, "patch": map[string]any{"name": "X"}, "ifMatch": "*"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected failure envelope")
	}
	if !strings.Contains(typed.Error.Message, "ENTITY_NOT_FOUND") {
		t.Errorf("expected ENTITY_NOT_FOUND in message, got %q", typed.Error.Message)
	}
}

func TestRPC_EntityPatch_TypeMismatchIs400(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A", "amount": 1})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"amount": "not-a-number"}, "ifMatch": "*"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected failure envelope")
	}
	msg := typed.Error.Message
	if !strings.Contains(msg, "INCOMPATIBLE_TYPE") && !strings.Contains(msg, "VALIDATION_FAILED") && !strings.Contains(msg, "BAD_REQUEST") {
		t.Errorf("expected INCOMPATIBLE_TYPE, VALIDATION_FAILED, or BAD_REQUEST in message, got %q", msg)
	}
}

func TestRPC_EntityPatch_WithTransition(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "Alice"}},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"name": "Renamed"}, "ifMatch": "*", "transition": "UPDATE"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("patch with transition: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success; got %#v", typed.Error)
	}

	// Read back and verify the field was applied.
	envelope, err := svc.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	dataBytes, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatalf("marshal entity data: %v", err)
	}
	var dataMap map[string]any
	if err := json.Unmarshal(dataBytes, &dataMap); err != nil {
		t.Fatalf("unmarshal entity data: %v", err)
	}
	if dataMap["name"] != "Renamed" {
		t.Errorf("expected name=Renamed, got %v", dataMap["name"])
	}
}

// TestRPC_EntityDirectSearch_MetaIncludesModelKey asserts that the gRPC direct-search
// path (EntitySearchRequest via EntitySearchCollection) includes modelKey in the
// entity meta — parity with the HTTP getOne shape (design §6.3).
func TestRPC_EntityDirectSearch_MetaIncludesModelKey(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	// Create an entity so the search has something to return.
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice", "age": 30},
		},
	})
	if _, err := svc.EntityManage(ctx, createCE); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Direct search returns *spi.Entity results via buildEntityMeta.
	searchCE := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":       "group",
			"operator":   "AND",
			"conditions": []any{},
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(searchCE, stream); err != nil {
		t.Fatalf("direct search: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected at least one search result")
	}

	meta := parseEntityResponseMeta(t, stream.sent[0])
	mk, ok := meta["modelKey"].(map[string]any)
	if !ok {
		t.Fatalf("gRPC direct-search meta missing modelKey; got meta=%v", meta)
	}
	if mk["name"] != "person" {
		t.Errorf("modelKey.name = %v, want person", mk["name"])
	}
}

// parseEntityResponseMeta extracts the meta map from an EntityResponseJson CloudEvent.
func parseEntityResponseMeta(t *testing.T, ce *cepb.CloudEvent) map[string]any {
	t.Helper()
	td, ok := ce.Data.(*cepb.CloudEvent_TextData)
	if !ok {
		t.Fatal("expected text_data in response")
	}
	var resp struct {
		Payload struct {
			Meta map[string]any `json:"meta"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(td.TextData), &resp); err != nil {
		t.Fatalf("failed to unmarshal entity response: %v", err)
	}
	return resp.Payload.Meta
}

func TestRPC_ModelDelete_409_Locked(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "del-locked", "1", map[string]any{"name": "Alice"})

	ce := makeCE(EntityModelDeleteRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "del-locked", "version": 1},
	})
	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntityModelDeleteResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for locked-model delete")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_ALREADY_LOCKED") {
		t.Errorf("expected message to contain MODEL_ALREADY_LOCKED, got %s", typed.Error.Message)
	}
}

// --- mock stream implementations ---

// mockManageStream implements CloudEventsService_EntityManageCollectionServer.
type mockManageStream struct {
	ctx  context.Context
	sent []*cepb.CloudEvent
}

func (m *mockManageStream) Send(ce *cepb.CloudEvent) error {
	m.sent = append(m.sent, ce)
	return nil
}

func (m *mockManageStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockManageStream) SendHeader(metadata.MD) error { return nil }
func (m *mockManageStream) SetTrailer(metadata.MD)       {}
func (m *mockManageStream) Context() context.Context     { return m.ctx }
func (m *mockManageStream) SendMsg(any) error            { return nil }
func (m *mockManageStream) RecvMsg(any) error            { return nil }

// mockEntityStream implements CloudEventsService_EntitySearchCollectionServer.
type mockEntityStream struct {
	ctx  context.Context
	sent []*cepb.CloudEvent
}

func (m *mockEntityStream) Send(ce *cepb.CloudEvent) error {
	m.sent = append(m.sent, ce)
	return nil
}

func (m *mockEntityStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockEntityStream) SendHeader(metadata.MD) error { return nil }
func (m *mockEntityStream) SetTrailer(metadata.MD)       {}
func (m *mockEntityStream) Context() context.Context     { return m.ctx }
func (m *mockEntityStream) SendMsg(any) error            { return nil }
func (m *mockEntityStream) RecvMsg(any) error            { return nil }
