package grpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// search_unevaluable_leaf_test.go covers the gRPC entry point for Task 5's
// classification of spi.ErrUnevaluableLeaf. Spec §10: "Over gRPC a rejection
// is an envelope error with success=false and error.code = CLIENT_ERROR, the
// message carrying the code above — never an empty stream, which a client
// reads as 'no matches'."
//
// Reproduction is the same bare-typeless-leaf shape used by the search-package
// unit tests and the internal/e2e Postgres test: a model field recorded as a
// bare {"kind":"LEAF"} node (no declared scalar type) is a real schema shape
// the condition-type boundary explicitly accepts ("no constraint") but the
// leaf kernel cannot evaluate a comparison against. It cannot be reached via
// modelHandler.ImportModel's SAMPLE_DATA converter (every concrete sample
// value infers a real scalar type), so this test builds its own env with
// direct access to the memory factory's ModelStore, exactly like
// newTestEnv/importAndLockModel do for everything else.
func newTestEnvWithFactory(t *testing.T) (*CloudEventsServiceImpl, context.Context, *memory.StoreFactory) {
	t.Helper()
	factory := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
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
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())
	modelHandler := model.New(factory)

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	ctx := spi.WithUserContext(context.Background(), uc)
	return svc, ctx, factory
}

// saveBareLeafModelGRPC registers a model whose single field is a bare
// {"kind":"LEAF"} node — present (a known path) but carrying NO declared
// scalar type — directly through the memory factory's ModelStore, bypassing
// ImportModel's SAMPLE_DATA-only converter.
func saveBareLeafModelGRPC(t *testing.T, ctx context.Context, factory *memory.StoreFactory, entityName, version, field string) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"kind":"OBJECT","children":{%q:{"kind":"LEAF"}}}`, field))
	node, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatalf("schema.Unmarshal: %v", err)
	}
	marshalled, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: entityName, ModelVersion: version}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: marshalled}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// TestRPC_DirectSearch_UnevaluableLeaf_400_InvalidCondition covers the
// bounded gRPC search route (EntitySearchCollection -> SearchService.Search
// with a resolved positive limit -> the memory plugin's Search, which calls
// spi.Prepare on the whole filter unconditionally). Asserts the full spec §10
// envelope contract: Success=false, Error.Code=CLIENT_ERROR, and the message
// carries the domain code — never an empty (matchless) stream.
func TestRPC_DirectSearch_UnevaluableLeaf_400_InvalidCondition(t *testing.T) {
	svc, ctx, factory := newTestEnvWithFactory(t)
	saveBareLeafModelGRPC(t, ctx, factory, "widget", "1", "score")

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "widget", "version": 1},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.score",
			"operatorType": "EQUALS",
			"value":        5,
		},
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response sent (never an empty/matchless stream), got %d", len(stream.sent))
	}
	if stream.sent[0].Type != EntityResponse {
		t.Errorf("expected type %s, got %s", EntityResponse, stream.sent[0].Type)
	}

	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Error("expected success=false for an unevaluable leaf")
	}
	if typed.Error == nil {
		t.Fatal("expected error in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "INVALID_CONDITION") {
		t.Errorf("expected message to contain INVALID_CONDITION, got %s", typed.Error.Message)
	}
}

// TestRPC_DirectSearch_UnevaluableLeaf_ValidTypedField_200 is the
// accept-side counterpart: the SAME condition shape against a field that DOES
// carry a declared type must still search normally (no accept/reject skew
// introduced by this classification).
//
// A matching entity is created first and the response count is asserted
// (mirrors TestRPC_DirectSearch's own "expected at least 1 response sent"
// check, rpc_test.go) — EntitySearchCollection sends exactly one message PER
// MATCHED ENTITY (search.go's handleDirectSearchRequest: `for _, e := range
// results { ... stream.Send(...) }`), so a zero-entity search sends ZERO
// messages. Without a seeded match, ranging over an empty stream.sent runs
// no assertions at all and the test passes vacuously — silently, exactly
// the failure mode a `NOT` rejection must never look like (spec §10: "never
// an empty stream, which a client reads as 'no matches'").
func TestRPC_DirectSearch_UnevaluableLeaf_ValidTypedField_200(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "widget", "1", map[string]any{"score": 0})

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "widget", "version": 1},
			"data":  map[string]any{"score": 5},
		},
	})
	if _, err := svc.EntityManage(ctx, createCE); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "widget", "version": 1},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.score",
			"operatorType": "EQUALS",
			"value":        5,
		},
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response sent (the seeded match), got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Fatalf("expected success=true for a declared-type field, got error: %v", typed.Error)
	}
}
