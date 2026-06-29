package entity

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// ----- Tests -----

// TestCreateEntity_PartialUniqueKey_Returns422 verifies that CreateEntity returns
// 422 INVALID_UNIQUE_KEY when the input document satisfies only some (not all)
// fields of a composite unique key.
func TestCreateEntity_PartialUniqueKey_Returns422(t *testing.T) {
	h, ctx := newOrderTestHandler(t)

	// Provide only "email"; "accountId" is absent → partial key.
	_, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com"}`),
	})
	if err == nil {
		t.Fatal("expected 422 error, got nil")
	}
	assertStatus(t, err, http.StatusUnprocessableEntity)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKey {
		t.Errorf("expected error code %q, got %q", common.ErrCodeInvalidUniqueKey, appErr.Code)
	}
}

// TestCreateEntity_FullUniqueKey_ContextCarriesKeys verifies that CreateEntity
// succeeds when all key fields are present AND that the context reaching the
// entity store's Save carries the model's unique-key definitions (via
// spi.UniqueKeysFromContext). The spy entity store captures the ctx.
func TestCreateEntity_FullUniqueKey_ContextCarriesKeys(t *testing.T) {
	h, ctx, rec := newOrderTestHandlerWithSpy(t)

	res, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(res.EntityIDs) != 1 {
		t.Fatalf("expected 1 entity ID, got %d", len(res.EntityIDs))
	}

	savedCtx, ok := rec.last()
	if !ok {
		t.Fatal("spy: Save was never called")
	}
	keys := spi.UniqueKeysFromContext(savedCtx)
	if len(keys) == 0 {
		t.Fatal("unique keys not present on context passed to entity store Save")
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 unique key, got %d", len(keys))
	}
	if keys[0].ID != "uk1" {
		t.Errorf("expected key ID %q, got %q", "uk1", keys[0].ID)
	}
	if len(keys[0].Fields) != 2 {
		t.Errorf("expected 2 fields in key, got %d: %v", len(keys[0].Fields), keys[0].Fields)
	}
}

// TestPatchEntity_NullsKeyField_Returns422 verifies that PatchEntity returns
// 422 INVALID_UNIQUE_KEY when the RFC 7386 merge result nullifies a key field,
// producing a partial-key document.
func TestPatchEntity_NullsKeyField_Returns422(t *testing.T) {
	h, ctx := newOrderTestHandler(t)

	// Create entity with both key fields present.
	res, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	entityID := res.EntityIDs[0]

	// PATCH with null on accountId → merge removes it → merged doc has only email → partial key.
	_, err = h.PatchEntity(ctx, PatchEntityInput{
		EntityID:    entityID,
		Patch:       json.RawMessage(`{"accountId":null}`),
		PatchFormat: "MERGE_PATCH",
		IfMatch:     "*",
	})
	if err == nil {
		t.Fatal("expected 422 error, got nil")
	}
	assertStatus(t, err, http.StatusUnprocessableEntity)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKey {
		t.Errorf("expected error code %q, got %q", common.ErrCodeInvalidUniqueKey, appErr.Code)
	}
}

// ----- Helpers -----

// newOrderTestHandler builds a Handler wired to a fresh in-memory store with a
// "Order/1" model. The model schema has {email: String, accountId: String} and a
// 2-field composite unique key [{ID:"uk1", Fields:["$.email","$.accountId"]}].
func newOrderTestHandler(t *testing.T) (*Handler, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	ctx := orderTestCtx()

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine)

	registerOrderModel(t, ctx, factory)
	return h, ctx
}

// newOrderTestHandlerWithSpy is like newOrderTestHandler but uses a spy store factory
// so callers can inspect the context received by EntityStore.Save.
func newOrderTestHandlerWithSpy(t *testing.T) (*Handler, context.Context, *ctxRecorder) {
	t.Helper()
	memFactory := memory.NewStoreFactory()
	ctx := orderTestCtx()
	rec := &ctxRecorder{}
	spy := &spyStoreFactory{delegate: memFactory, recorder: rec}

	txMgr, err := spy.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(spy, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(spy, txMgr, common.NewDefaultUUIDGenerator(), engine)

	registerOrderModel(t, ctx, spy)
	return h, ctx, rec
}

// orderTestCtx returns a context with a UserContext for test use.
func orderTestCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "uk-test-user",
		UserName: "UK Test",
		Tenant:   spi.Tenant{ID: "uk-tenant", Name: "UK"},
		Roles:    []string{"user"},
	})
}

// registerOrderModel saves a locked Order/1 model with a 2-field composite unique key
// into the factory's model store.
func registerOrderModel(t *testing.T, ctx context.Context, factory spi.StoreFactory) {
	t.Helper()

	node := schema.NewObjectNode()
	node.SetChild("email", schema.NewLeafNode(schema.String))
	node.SetChild("accountId", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}

	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
		UniqueKeys: []spi.UniqueKey{
			{ID: "uk1", Fields: []string{"$.email", "$.accountId"}},
		},
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}
}

// ----- Spy infrastructure -----

// ctxRecorder records every context passed to EntityStore.Save.
type ctxRecorder struct {
	mu   sync.Mutex
	ctxs []context.Context
}

func (r *ctxRecorder) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctxs = append(r.ctxs, ctx)
}

// last returns the most recently recorded context, or (nil, false) if none.
func (r *ctxRecorder) last() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ctxs) == 0 {
		return nil, false
	}
	return r.ctxs[len(r.ctxs)-1], true
}

// spyStoreFactory wraps a delegate StoreFactory, intercepting EntityStore() to
// inject a spy that records the ctx received on each Save call.
type spyStoreFactory struct {
	delegate spi.StoreFactory
	recorder *ctxRecorder
}

func (f *spyStoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	d, err := f.delegate.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &spyEntityStore{delegate: d, recorder: f.recorder}, nil
}

func (f *spyStoreFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	return f.delegate.ModelStore(ctx)
}
func (f *spyStoreFactory) KeyValueStore(ctx context.Context) (spi.KeyValueStore, error) {
	return f.delegate.KeyValueStore(ctx)
}
func (f *spyStoreFactory) MessageStore(ctx context.Context) (spi.MessageStore, error) {
	return f.delegate.MessageStore(ctx)
}
func (f *spyStoreFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	return f.delegate.WorkflowStore(ctx)
}
func (f *spyStoreFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	return f.delegate.StateMachineAuditStore(ctx)
}
func (f *spyStoreFactory) AsyncSearchStore(ctx context.Context) (spi.AsyncSearchStore, error) {
	return f.delegate.AsyncSearchStore(ctx)
}
func (f *spyStoreFactory) TransactionManager(ctx context.Context) (spi.TransactionManager, error) {
	return f.delegate.TransactionManager(ctx)
}
func (f *spyStoreFactory) Close() error { return f.delegate.Close() }

// Compile-time contract check.
var _ spi.StoreFactory = (*spyStoreFactory)(nil)

// spyEntityStore wraps a delegate EntityStore and records every Save context.
type spyEntityStore struct {
	delegate spi.EntityStore
	recorder *ctxRecorder
}

func (s *spyEntityStore) Save(ctx context.Context, entity *spi.Entity) (int64, error) {
	s.recorder.record(ctx)
	return s.delegate.Save(ctx, entity)
}
func (s *spyEntityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	return s.delegate.CompareAndSave(ctx, entity, expectedTxID)
}
func (s *spyEntityStore) SaveAll(ctx context.Context, entities iter.Seq[*spi.Entity]) ([]int64, error) {
	return s.delegate.SaveAll(ctx, entities)
}
func (s *spyEntityStore) Get(ctx context.Context, entityID string) (*spi.Entity, error) {
	return s.delegate.Get(ctx, entityID)
}
func (s *spyEntityStore) GetAsAt(ctx context.Context, entityID string, asAt time.Time) (*spi.Entity, error) {
	return s.delegate.GetAsAt(ctx, entityID, asAt)
}
func (s *spyEntityStore) GetAll(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	return s.delegate.GetAll(ctx, modelRef)
}
func (s *spyEntityStore) GetAllAsAt(ctx context.Context, modelRef spi.ModelRef, asAt time.Time) ([]*spi.Entity, error) {
	return s.delegate.GetAllAsAt(ctx, modelRef, asAt)
}
func (s *spyEntityStore) Delete(ctx context.Context, entityID string) error {
	return s.delegate.Delete(ctx, entityID)
}
func (s *spyEntityStore) DeleteAll(ctx context.Context, modelRef spi.ModelRef) error {
	return s.delegate.DeleteAll(ctx, modelRef)
}
func (s *spyEntityStore) Exists(ctx context.Context, entityID string) (bool, error) {
	return s.delegate.Exists(ctx, entityID)
}
func (s *spyEntityStore) Count(ctx context.Context, modelRef spi.ModelRef) (int64, error) {
	return s.delegate.Count(ctx, modelRef)
}
func (s *spyEntityStore) CountByState(ctx context.Context, modelRef spi.ModelRef, states []string) (map[string]int64, error) {
	return s.delegate.CountByState(ctx, modelRef, states)
}
func (s *spyEntityStore) GetVersionHistory(ctx context.Context, entityID string) ([]spi.EntityVersion, error) {
	return s.delegate.GetVersionHistory(ctx, entityID)
}

// Compile-time contract check.
var _ spi.EntityStore = (*spyEntityStore)(nil)
