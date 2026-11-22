package entity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda/cyoda-go/internal/domain/workflow"
	"github.com/cyoda/cyoda-go/internal/platformclock"
	"github.com/cyoda/cyoda-go/internal/txcoord"
	"github.com/cyoda/cyoda-go/plugins/memory"
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

// TestUpdateEntityCollection_NonObjectRoot_ValidationFailed: an item whose data
// is not a JSON object is refused with 400 VALIDATION_FAILED on a model with
// unique keys too, as on every other write path. The object-root refusal runs
// ahead of the unique-key pre-check; before, the pre-check could not decode a
// scalar or array root and answered 500.
func TestUpdateEntityCollection_NonObjectRoot_ValidationFailed(t *testing.T) {
	h, ctx := newOrderTestHandler(t)
	res, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	for _, root := range []string{`5`, `"x"`, `[1]`, `null`} {
		_, err := h.UpdateEntityCollection(ctx, []UpdateCollectionItem{
			{EntityID: res.EntityIDs[0], Payload: json.RawMessage(root)},
		})
		var appErr *common.AppError
		if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeValidationFailed {
			t.Errorf("root %s: got %v; want 400 VALIDATION_FAILED", root, err)
		}
	}
}

// TestUpdateEntityCollection_PartialKeyRefused_ModelUnchanged: an item refused
// for an incomplete unique key changes nothing, the model included. The
// pre-check runs before the schema check may extend the model; the memory
// backend applies a schema extension at once, outside the entity transaction,
// so a later refusal could not take it back.
func TestUpdateEntityCollection_PartialKeyRefused_ModelUnchanged(t *testing.T) {
	h, ctx := newOrderTestHandler(t)
	res, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	if err := modelStore.SetChangeLevel(ctx, ref, spi.ChangeLevelStructural); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	before, err := modelStore.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	_, err = h.UpdateEntityCollection(ctx, []UpdateCollectionItem{
		{EntityID: res.EntityIDs[0], Payload: json.RawMessage(`{"email":"a@b.com","injected":"x"}`)},
	})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeInvalidUniqueKey {
		t.Fatalf("got %v; want 422 INVALID_UNIQUE_KEY", err)
	}
	after, err := modelStore.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(after.Schema) != string(before.Schema) {
		t.Errorf("the refused item extended the model:\nbefore %s\nafter  %s", before.Schema, after.Schema)
	}
}

// ----- Helpers -----

// newOrderTestHandler builds a Handler wired to a fresh in-memory store with a
// "Order/1" model. The model schema has {email: String, accountId: String} and a
// 2-field composite unique key [{ID:"uk1", Fields:["$.email","$.accountId"]}].
func newOrderTestHandler(t *testing.T) (*Handler, context.Context) {
	t.Helper()
	// The schema-apply function production wires (app/app.go).
	factory := memory.NewStoreFactory(memory.WithApplyFunc(schema.ApplyMany))
	ctx := orderTestCtx()

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(cachedFactory(factory), common.NewDefaultUUIDGenerator(), txMgr, platformclock.Real())
	h := New(cachedFactory(factory), txMgr, common.NewDefaultUUIDGenerator(), engine, txcoord.New(txMgr), newTestConsistencyFor(txMgr), platformclock.Real())

	registerOrderModel(t, ctx, factory)
	return h, ctx
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
// into the factory's model store.
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
	}); err != nil {
		t.Fatalf("ModelStore.Save (Product): %v", err)
	}
}

// TestCreateEntity_DuplicateUniqueKey_Returns409 verifies that the unique keys
// declared on the stored model are enforced at commit: the handler passes
// nothing about keys to the store, which reads them from its own model.
func TestCreateEntity_DuplicateUniqueKey_Returns409(t *testing.T) {
	h, ctx := newOrderTestHandler(t)
	in := CreateEntityInput{
		EntityName:   "Order",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"email":"a@b.com","accountId":"acc-1"}`),
	}
	if _, err := h.CreateEntity(ctx, in); err != nil {
		t.Fatalf("first CreateEntity: %v", err)
	}
	_, err := h.CreateEntity(ctx, in)
	assertStatus(t, err, http.StatusConflict)
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeUniqueViolation {
		t.Fatalf("expected %s, got %v", common.ErrCodeUniqueViolation, err)
	}
}

// TestUpdateEntityCollection_MixedModels_KeysFromModel verifies a batch update
// mixing a keyed and an unkeyed model: the keyed item is held to its model's
// key, the unkeyed item is not.
func TestUpdateEntityCollection_MixedModels_KeysFromModel(t *testing.T) {
	h, ctx := newOrderTestHandler(t)
	registerProductModel(t, ctx, h.factory)
	mk := func(name, ver, data string) string {
		res, err := h.CreateEntity(ctx, CreateEntityInput{EntityName: name, ModelVersion: ver, Format: "JSON", Data: json.RawMessage(data)})
		if err != nil {
			t.Fatalf("CreateEntity %s: %v", name, err)
		}
		return res.EntityIDs[0]
	}
	o1 := mk("Order", "1", `{"email":"a@b.com","accountId":"acc-1"}`)
	mk("Order", "1", `{"email":"b@b.com","accountId":"acc-1"}`)
	p := mk("Product", "1", `{"name":"Widget"}`)

	// Unkeyed Product item alone is fine.
	if _, err := h.UpdateEntityCollection(ctx, []UpdateCollectionItem{
		{EntityID: p, Payload: json.RawMessage(`{"name":"Gadget"}`)},
	}); err != nil {
		t.Fatalf("Product update: %v", err)
	}
	// Moving Order o1 onto the other Order's key is refused.
	_, err := h.UpdateEntityCollection(ctx, []UpdateCollectionItem{
		{EntityID: o1, Payload: json.RawMessage(`{"email":"b@b.com","accountId":"acc-1"}`)},
		{EntityID: p, Payload: json.RawMessage(`{"name":"Other"}`)},
	})
	assertStatus(t, err, http.StatusConflict)

	// Moving o1 to a fresh key, in a batch with the unkeyed item, commits
	// both; o1's old value is free afterwards.
	if _, err := h.UpdateEntityCollection(ctx, []UpdateCollectionItem{
		{EntityID: o1, Payload: json.RawMessage(`{"email":"c@b.com","accountId":"acc-1"}`)},
		{EntityID: p, Payload: json.RawMessage(`{"name":"Other"}`)},
	}); err != nil {
		t.Fatalf("mixed batch with a fresh key: %v", err)
	}
	for id, want := range map[string]string{o1: `"c@b.com"`, p: `"Other"`} {
		env, err := h.GetEntity(ctx, GetOneEntityInput{EntityID: id})
		if err != nil {
			t.Fatalf("GetEntity %s: %v", id, err)
		}
		data, err := json.Marshal(env.Data)
		if err != nil {
			t.Fatalf("marshal data of %s: %v", id, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("entity %s data = %s; want it to hold %s", id, data, want)
		}
	}
	mk("Order", "1", `{"email":"a@b.com","accountId":"acc-1"}`)
}
