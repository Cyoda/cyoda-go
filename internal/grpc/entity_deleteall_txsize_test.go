package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Task 15: gRPC EntityDeleteAllRequest honors an explicitly-sent
// transactionSize (schema default 1000 removed). See
// .superpowers/sdd/2026-08-11-transaction-control-params-plan/task-15-brief.md.

// --- (a) decode: absent transactionSize -> nil, not the old baked 1000 default ---

// TestEntityDeleteAllRequestJson_TransactionSize_NoDefault pins that removing
// the schema's `"default": 1000` regenerates TransactionSize as *int with no
// UnmarshalJSON default-stamping: a payload that omits the field decodes to
// nil, not a silently-injected 1000.
func TestEntityDeleteAllRequestJson_TransactionSize_NoDefault(t *testing.T) {
	payload := []byte(`{"id":"test","model":{"name":"person","version":1}}`)

	var req events.EntityDeleteAllRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.TransactionSize != nil {
		t.Errorf("TransactionSize = %v, want nil (no baked default)", *req.TransactionSize)
	}
}

// --- shared spy/env helpers ---

// deleteAllTxSizeSpyStore wraps a real spi.EntityStore and records whether
// DeleteAll was invoked, so a test can distinguish the batched
// (enumerate-then-delete) path from the single-tx DeleteAll fast path —
// mirrors internal/domain/entity's deleteAllSpyStore.
type deleteAllTxSizeSpyStore struct {
	spi.EntityStore
	mu     sync.Mutex
	called bool
}

func (s *deleteAllTxSizeSpyStore) DeleteAll(ctx context.Context, ref spi.ModelRef) error {
	s.mu.Lock()
	s.called = true
	s.mu.Unlock()
	return s.EntityStore.DeleteAll(ctx, ref)
}

func (s *deleteAllTxSizeSpyStore) wasCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

// deleteAllTxSizeSpyFactory wraps a real spi.StoreFactory but always hands
// out the given spy EntityStore; every other accessor delegates unchanged.
type deleteAllTxSizeSpyFactory struct {
	spi.StoreFactory
	store *deleteAllTxSizeSpyStore
}

func (f *deleteAllTxSizeSpyFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// newDeleteAllTxSizeEnv wires a CloudEventsServiceImpl against a memory
// backend with a DeleteAll-spying EntityStore and a commit-counting
// TransactionManager, so a test can assert both "was the single-tx DeleteAll
// fast path used" and "how many transactions committed" for the same run.
func newDeleteAllTxSizeEnv(t *testing.T) (svc *CloudEventsServiceImpl, ctx context.Context, spyStore *deleteAllTxSizeSpyStore, rtm *onCommitTxMgr) {
	t.Helper()

	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realFactory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	realTxMgr := realFactory.GetTransactionManager()
	rtm = &onCommitTxMgr{TransactionManager: realTxMgr}

	uc := &spi.UserContext{
		UserID:   "deleteall-txsize-user",
		UserName: "DeleteAll TxSize",
		Tenant:   spi.Tenant{ID: "deleteall-txsize-tenant", Name: "DeleteAll TxSize"},
		Roles:    []string{"ADMIN"},
	}
	ctx = spi.WithUserContext(context.Background(), uc)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spyStore = &deleteAllTxSizeSpyStore{EntityStore: realStore}
	factory := &deleteAllTxSizeSpyFactory{StoreFactory: realFactory, store: spyStore}

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), rtm)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, rtm, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchService)
	modelHandler := model.New(factory)

	svc = &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         rtm,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}
	return svc, ctx, spyStore, rtm
}

// seedDeleteAllTxSizePersons imports+locks the "person" model ({name: String})
// and creates n entities via svc's gRPC door, returning once all n creates
// have succeeded.
func seedDeleteAllTxSizePersons(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, n int) {
	t.Helper()
	dataBytes, err := json.Marshal(map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("marshal sample data: %v", err)
	}
	if _, err := svc.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName: "person", ModelVersion: "1", Format: "JSON",
		Converter: "SAMPLE_DATA", Data: dataBytes,
	}); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if _, err := svc.modelHandler.LockModel(ctx, "person", "1"); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	for i := 0; i < n; i++ {
		ce := makeCE(EntityCreateRequest, map[string]any{
			"id":         "seed",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": "Alice"},
			},
		})
		if _, err := svc.EntityManage(ctx, ce); err != nil {
			t.Fatalf("seed create[%d]: %v", i, err)
		}
	}
}

// --- (b) transactionSize:2 over 5 entities -> batched path ---

// TestRPC_EntityDeleteAll_TransactionSize_Batched pins that an explicit
// transactionSize switches gRPC delete-all onto the batched
// enumerate-then-delete path (Task 10's DeleteEntitiesConditional): the
// single-tx EntityStore.DeleteAll fast path must NOT be used, the delete must
// span at least 3 commits (5 entities, batch size 2 => ceil(5/2)=3 batches,
// each its own commit, beyond the resolution tx), and the response reports
// all 5 entities removed.
func TestRPC_EntityDeleteAll_TransactionSize_Batched(t *testing.T) {
	svc, ctx, spyStore, rtm := newDeleteAllTxSizeEnv(t)
	seedDeleteAllTxSizePersons(t, svc, ctx, 5)

	commitsBefore := rtm.commitCount()

	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":              "test",
		"model":           map[string]any{"name": "person", "version": 1},
		"transactionSize": 2,
	})
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}

	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.NumDeleted != 5 {
		t.Errorf("NumDeleted = %d, want 5", typed.NumDeleted)
	}

	if spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must NOT be called on the batched (transactionSize>0) delete-all path")
	}
	if commits := rtm.commitCount() - commitsBefore; commits < 3 {
		t.Errorf("commits during delete-all = %d, want >=3 (batch granularity must be observable)", commits)
	}
}

// --- (c) transactionSize:0 -> CLIENT_ERROR BAD_REQUEST ---

// TestRPC_EntityDeleteAll_TransactionSize_Zero_BadRequest pins that a
// non-positive transactionSize is rejected as a client error before any
// delete is attempted.
func TestRPC_EntityDeleteAll_TransactionSize_Zero_BadRequest(t *testing.T) {
	svc, ctx, spyStore, _ := newDeleteAllTxSizeEnv(t)
	seedDeleteAllTxSizePersons(t, svc, ctx, 1)

	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":              "test",
		"model":           map[string]any{"name": "person", "version": 1},
		"transactionSize": 0,
	})
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}

	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeBadRequest+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeBadRequest+":")
	}
	if spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must not be called when transactionSize is rejected")
	}
}

// --- (d) tx-token'd (joined) request + transactionSize -> same rejection ---

// TestRPC_EntityDeleteAll_TransactionSize_Joined_Rejected pins that a
// transactionSize on a request already joined to an open transaction
// (spi.GetTransaction(ctx) != nil — how a routed compute-node callback
// presents at param-resolution time) is rejected the same way
// resolveEventTimeout rejects transactionTimeoutMs on one: honoring it would
// let a participant unilaterally fragment a transaction the owner still
// controls.
func TestRPC_EntityDeleteAll_TransactionSize_Joined_Rejected(t *testing.T) {
	svc, ctx, spyStore, _ := newDeleteAllTxSizeEnv(t)
	seedDeleteAllTxSizePersons(t, svc, ctx, 1)

	joinedCtx := spi.WithTransaction(ctx, &spi.TransactionState{ID: "tx-1"})

	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":              "test",
		"model":           map[string]any{"name": "person", "version": 1},
		"transactionSize": 2,
	})
	stream := &mockManageStream{ctx: joinedCtx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}

	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeBadRequest+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeBadRequest+":")
	}
	if !strings.Contains(typed.Error.Message, "joins an open transaction") {
		t.Errorf("message = %q, want mention of the joined transaction", typed.Error.Message)
	}
	if spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must not be called when transactionSize is rejected")
	}
}

// --- (e) absent transactionSize -> existing single-tx DeleteAllEntities path ---

// TestRPC_EntityDeleteAll_TransactionSize_Absent_SingleTx pins that an
// EntityDeleteAllRequest with no transactionSize field keeps today's
// single-tx DeleteAllEntities fast path byte-for-byte: EntityStore.DeleteAll
// IS called exactly once, and the delete spans exactly one commit.
func TestRPC_EntityDeleteAll_TransactionSize_Absent_SingleTx(t *testing.T) {
	svc, ctx, spyStore, rtm := newDeleteAllTxSizeEnv(t)
	seedDeleteAllTxSizePersons(t, svc, ctx, 3)

	commitsBefore := rtm.commitCount()

	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
	})
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}

	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.NumDeleted != 3 {
		t.Errorf("NumDeleted = %d, want 3", typed.NumDeleted)
	}

	if !spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must be called when transactionSize is absent")
	}
	if commits := rtm.commitCount() - commitsBefore; commits != 1 {
		t.Errorf("commits during delete-all = %d, want exactly 1 (single-tx path, unchanged)", commits)
	}
}
