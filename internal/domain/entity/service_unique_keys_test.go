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
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
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

// TestCreateEntityCollection_PartialKey_Returns422 verifies that
// CreateEntityCollection returns 422 INVALID_UNIQUE_KEY before opening a
// transaction when a batch item's payload is a partial match for its model's
// composite unique key.
func TestCreateEntityCollection_PartialKey_Returns422(t *testing.T) {
	h, ctx := newOrderTestHandler(t)

	// Order model has a 2-field key {$.email, $.accountId}. Provide only one.
	_, err := h.CreateEntityCollection(ctx, []CollectionItem{
		{ModelName: "Order", ModelVersion: 1, Payload: json.RawMessage(`{"email":"a@b.com"}`)},
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

// TestCreateEntityCollection_MixedModels_PerItemKeys verifies that
// CreateEntityCollection passes the correct model-scoped unique keys on the
// context for each item's Save call, and that a key-less item (Product/1) does
// not inherit the preceding keyed item's (Order/1) keys — i.e. no leakage
// between items sharing the same reused currentCtx.
func TestCreateEntityCollection_MixedModels_PerItemKeys(t *testing.T) {
	memFactory := memory.NewStoreFactory()
	ctx := orderTestCtx()
	rec := &ctxRecorder{}
	spy := &spyStoreFactory{delegate: memFactory, recorder: rec}

	txMgr, err := spy.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(spy, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(spy, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

	registerOrderModel(t, ctx, spy)
	registerProductModel(t, ctx, spy)

	// Batch: item 0 = Order (has key uk1), item 1 = Product (no keys).
	_, callErr := h.CreateEntityCollection(ctx, []CollectionItem{
		{ModelName: "Order", ModelVersion: 1, Payload: json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`)},
		{ModelName: "Product", ModelVersion: 1, Payload: json.RawMessage(`{"name":"Widget"}`)},
	})
	if callErr != nil {
		t.Fatalf("CreateEntityCollection: %v", callErr)
	}

	saved := rec.all()
	if len(saved) != 2 {
		t.Fatalf("expected 2 Save calls, got %d", len(saved))
	}

	// Item 0 (Order): context must carry uk1.
	keysA := spi.UniqueKeysFromContext(saved[0])
	if len(keysA) != 1 {
		t.Errorf("item 0 (Order): expected 1 unique key, got %d: %v", len(keysA), keysA)
	} else if keysA[0].ID != "uk1" {
		t.Errorf("item 0 (Order): expected key ID %q, got %q", "uk1", keysA[0].ID)
	}

	// Item 1 (Product): context must carry NO keys — proving no leakage from item 0.
	keysB := spi.UniqueKeysFromContext(saved[1])
	if len(keysB) != 0 {
		t.Errorf("item 1 (Product): expected 0 unique keys (no leakage from Order), got %d: %v", len(keysB), keysB)
	}
}

// TestUpdateEntityCollection_MixedModels_PerItemKeys verifies that
// UpdateEntityCollection passes the correct model-scoped unique keys on the
// context for each item's Save call, with no leakage between items.
func TestUpdateEntityCollection_MixedModels_PerItemKeys(t *testing.T) {
	memFactory := memory.NewStoreFactory()
	ctx := orderTestCtx()
	rec := &ctxRecorder{}
	spy := &spyStoreFactory{delegate: memFactory, recorder: rec}

	txMgr, err := spy.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(spy, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(spy, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

	registerOrderModel(t, ctx, spy)
	registerProductModel(t, ctx, spy)

	// Create one Order entity and one Product entity individually first.
	resOrder, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity Order: %v", err)
	}
	resProduct, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Product",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"name":"Widget"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity Product: %v", err)
	}

	// Reset recorder so we only see the Update saves.
	rec.reset()

	orderID := resOrder.EntityIDs[0]
	productID := resProduct.EntityIDs[0]

	// Batch update: item 0 = Order (has key uk1), item 1 = Product (no keys).
	_, updateErr := h.UpdateEntityCollection(ctx, []UpdateCollectionItem{
		{EntityID: orderID, Payload: json.RawMessage(`{"email":"b@b.com","accountId":"acc-1"}`)},
		{EntityID: productID, Payload: json.RawMessage(`{"name":"Gadget"}`)},
	})
	if updateErr != nil {
		t.Fatalf("UpdateEntityCollection: %v", updateErr)
	}

	saved := rec.all()
	if len(saved) != 2 {
		t.Fatalf("expected 2 Save calls, got %d", len(saved))
	}

	// Item 0 (Order): context must carry uk1.
	keysA := spi.UniqueKeysFromContext(saved[0])
	if len(keysA) != 1 {
		t.Errorf("item 0 (Order): expected 1 unique key, got %d: %v", len(keysA), keysA)
	} else if keysA[0].ID != "uk1" {
		t.Errorf("item 0 (Order): expected key ID %q, got %q", "uk1", keysA[0].ID)
	}

	// Item 1 (Product): context must carry NO keys — proving no leakage from item 0.
	keysB := spi.UniqueKeysFromContext(saved[1])
	if len(keysB) != 0 {
		t.Errorf("item 1 (Product): expected 0 unique keys (no leakage from Order), got %d: %v", len(keysB), keysB)
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
	h := New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

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
	h := New(spy, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

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

// registerProductModel saves a locked Product/1 model with NO unique keys
// into the factory's model store. Used in mixed-model batch tests.
func registerProductModel(t *testing.T, ctx context.Context, factory spi.StoreFactory) {
	t.Helper()

	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}

	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Product", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
		// No unique keys — used to verify no leakage from a preceding keyed item.
	}); err != nil {
		t.Fatalf("ModelStore.Save (Product): %v", err)
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

// all returns a snapshot of all recorded contexts in order.
func (r *ctxRecorder) all() []context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]context.Context, len(r.ctxs))
	copy(out, r.ctxs)
	return out
}

// reset clears all recorded contexts.
func (r *ctxRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctxs = r.ctxs[:0]
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
func (f *spyStoreFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	return f.delegate.ScheduledTaskStore(ctx)
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
