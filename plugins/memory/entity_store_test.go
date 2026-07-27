package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func ctxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

// ctxWithUserKind returns a context carrying a UserContext with an explicit
// PrincipalKind, for attribution tests that need deterministic control over
// Kind (ctxWithTenant's fixed "test-user" leaves Kind at its zero value).
func ctxWithUserKind(tid spi.TenantID, userID string, kind spi.PrincipalKind) context.Context {
	uc := &spi.UserContext{
		UserID: userID,
		Kind:   kind,
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func TestFactoryReturnsStoreForTenant(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestFactoryReturnsErrorWithoutTenant(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := context.Background()
	_, err := factory.EntityStore(ctx)
	if err == nil {
		t.Fatal("expected error for missing tenant, got nil")
	}
}

func TestStoreAndRetrieve(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-001", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"amount": 100}`),
	}
	_, err := store.Save(ctx, entity)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	got, err := store.Get(ctx, "e-001")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if string(got.Data) != `{"amount": 100}` {
		t.Errorf("data mismatch: %s", got.Data)
	}
}

func TestTenantIsolationDataInvisible(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-001", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{"owner": "A"}`),
	}
	storeA.Save(ctxA, entity)
	got, err := storeB.Get(ctxB, "e-001")
	if err == nil && got != nil {
		t.Error("tenant-B should not see tenant-A's entity")
	}
}

func TestTenantIsolationWritesDontCross(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-001", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{"owner": "A"}`),
	}
	storeA.Save(ctxA, entity)
	all, _ := storeB.GetAll(ctxB, spi.ModelRef{EntityName: "Order", ModelVersion: "1"})
	if len(all) != 0 {
		t.Errorf("expected 0 entities for tenant-B, got %d", len(all))
	}
}

func TestTenantIsolationDeletesDontCross(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	entityA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-001", TenantID: "tenant-A", ModelRef: modelRef},
		Data: []byte(`{"owner": "A"}`),
	}
	entityB := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-001", TenantID: "tenant-B", ModelRef: modelRef},
		Data: []byte(`{"owner": "B"}`),
	}
	storeA.Save(ctxA, entityA)
	storeB.Save(ctxB, entityB)
	storeA.Delete(ctxA, "e-001")
	got, err := storeB.Get(ctxB, "e-001")
	if err != nil {
		t.Fatalf("tenant-B's entity should still exist: %v", err)
	}
	if string(got.Data) != `{"owner": "B"}` {
		t.Errorf("expected tenant-B's data, got: %s", got.Data)
	}
}

func TestSystemTenantIsolated(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxSys := ctxWithTenant(spi.SystemTenantID)
	ctxA := ctxWithTenant("tenant-A")
	storeSys, _ := factory.EntityStore(ctxSys)
	storeA, _ := factory.EntityStore(ctxA)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "sys-001", TenantID: spi.SystemTenantID,
			ModelRef: spi.ModelRef{EntityName: "Config", ModelVersion: "1"},
		},
		Data: []byte(`{"system": true}`),
	}
	storeSys.Save(ctxSys, entity)
	got, err := storeA.Get(ctxA, "sys-001")
	if err == nil && got != nil {
		t.Error("tenant-A should not see SYSTEM's entity")
	}
}

func TestSameEntityIDDifferentTenants(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	entityA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "same-id", TenantID: "tenant-A", ModelRef: modelRef},
		Data: []byte(`{"tenant": "A"}`),
	}
	entityB := &spi.Entity{
		Meta: spi.EntityMeta{ID: "same-id", TenantID: "tenant-B", ModelRef: modelRef},
		Data: []byte(`{"tenant": "B"}`),
	}
	storeA.Save(ctxA, entityA)
	storeB.Save(ctxB, entityB)
	gotA, _ := storeA.Get(ctxA, "same-id")
	gotB, _ := storeB.Get(ctxB, "same-id")
	if string(gotA.Data) != `{"tenant": "A"}` {
		t.Errorf("tenant-A got wrong data: %s", gotA.Data)
	}
	if string(gotB.Data) != `{"tenant": "B"}` {
		t.Errorf("tenant-B got wrong data: %s", gotB.Data)
	}
}

func TestPointInTimeRespectsIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-temporal", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"v": 1}`),
	}
	storeA.Save(ctxA, entity)
	afterSave := time.Now()
	_, err := storeB.GetAsAt(ctxB, "e-temporal", afterSave)
	if err == nil {
		t.Error("tenant-B should not see tenant-A's temporal data")
	}
}

func TestPointInTimeRetrieval(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-002", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"amount": 50}`),
	}
	store.Save(ctx, entity)
	afterV1 := time.Now()
	time.Sleep(time.Millisecond)
	entity.Data = []byte(`{"amount": 75}`)
	store.Save(ctx, entity)
	got, err := store.GetAsAt(ctx, "e-002", afterV1)
	if err != nil {
		t.Fatalf("getAsAt failed: %v", err)
	}
	if string(got.Data) != `{"amount": 50}` {
		t.Errorf("expected v1 data, got: %s", got.Data)
	}
}

func TestSoftDelete(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-del", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"x": 1}`),
	}
	store.Save(ctx, entity)
	err := store.Delete(ctx, "e-del")
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	_, err = store.Get(ctx, "e-del")
	if err == nil {
		t.Error("expected not-found after delete")
	}
	exists, _ := store.Exists(ctx, "e-del")
	if exists {
		t.Error("expected Exists=false after delete")
	}
}

func TestSoftDeleteAsAtBeforeDeletion(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-del2", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"y": 2}`),
	}
	store.Save(ctx, entity)
	afterSave := time.Now()
	time.Sleep(time.Millisecond)
	store.Delete(ctx, "e-del2")

	got, err := store.GetAsAt(ctx, "e-del2", afterSave)
	if err != nil {
		t.Fatalf("expected entity before deletion, got error: %v", err)
	}
	if string(got.Data) != `{"y": 2}` {
		t.Errorf("expected saved data, got: %s", got.Data)
	}
}

func TestSoftDeleteCountExcludes(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-del3", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"z": 3}`),
	}
	store.Save(ctx, entity)
	store.Delete(ctx, "e-del3")
	count, _ := store.Count(ctx, modelRef)
	if count != 0 {
		t.Errorf("expected count 0 after delete, got %d", count)
	}
}

func TestSoftDeleteGetAllExcludes(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	e1 := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-keep", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"keep": true}`),
	}
	e2 := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-gone", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"gone": true}`),
	}
	store.Save(ctx, e1)
	store.Save(ctx, e2)
	store.Delete(ctx, "e-gone")

	all, _ := store.GetAll(ctx, modelRef)
	if len(all) != 1 {
		t.Fatalf("expected 1 entity after delete, got %d", len(all))
	}
	if all[0].Meta.ID != "e-keep" {
		t.Errorf("expected e-keep, got %s", all[0].Meta.ID)
	}
}

func TestGetVersionHistory(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Save with CREATED
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-hist", TenantID: "tenant-A", ModelRef: modelRef,
			State: "NEW", ChangeType: "CREATED", ChangeUser: "user-1",
		},
		Data: []byte(`{"v": 1}`),
	}
	store.Save(ctx, entity)

	// Update with UPDATED
	time.Sleep(time.Millisecond)
	entity.Meta.ChangeType = "UPDATED"
	entity.Meta.ChangeUser = "user-2"
	entity.Data = []byte(`{"v": 2}`)
	store.Save(ctx, entity)

	history, err := store.GetVersionHistory(ctx, "e-hist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(history))
	}

	if history[0].ChangeType != "CREATED" {
		t.Errorf("expected CREATED, got %s", history[0].ChangeType)
	}
	if history[0].User != "user-1" {
		t.Errorf("expected user-1, got %s", history[0].User)
	}
	if history[0].Version != 1 {
		t.Errorf("expected version 1, got %d", history[0].Version)
	}

	if history[1].ChangeType != "UPDATED" {
		t.Errorf("expected UPDATED, got %s", history[1].ChangeType)
	}
	if history[1].User != "user-2" {
		t.Errorf("expected user-2, got %s", history[1].User)
	}
	if history[1].Version != 2 {
		t.Errorf("expected version 2, got %d", history[1].Version)
	}
}

func TestGetVersionHistoryWithDelete(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-hist-del", TenantID: "tenant-A", ModelRef: modelRef,
			State: "NEW", ChangeType: "CREATED", ChangeUser: "user-1",
		},
		Data: []byte(`{"v": 1}`),
	}
	store.Save(ctx, entity)

	time.Sleep(time.Millisecond)
	store.Delete(ctx, "e-hist-del")

	history, err := store.GetVersionHistory(ctx, "e-hist-del")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(history))
	}

	if history[0].ChangeType != "CREATED" {
		t.Errorf("expected CREATED, got %s", history[0].ChangeType)
	}
	if history[0].Deleted {
		t.Error("expected first version not deleted")
	}

	if history[1].ChangeType != "DELETED" {
		t.Errorf("expected DELETED, got %s", history[1].ChangeType)
	}
	if !history[1].Deleted {
		t.Error("expected second version to be deleted")
	}
	if history[1].User != "test-user" {
		t.Errorf("expected test-user from context, got %s", history[1].User)
	}
}

func TestGetAllAsAt(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	e1 := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-aa-1", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"v": 1}`),
	}
	e2 := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-aa-2", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"v": 1}`),
	}
	store.Save(ctx, e1)
	store.Save(ctx, e2)
	t1 := time.Now()

	time.Sleep(time.Millisecond)

	// Update entity 1 at t2
	e1.Data = []byte(`{"v": 2}`)
	store.Save(ctx, e1)
	t2 := time.Now()

	// GetAllAsAt(t1) → both at original state
	gotT1, err := store.GetAllAsAt(ctx, modelRef, t1)
	if err != nil {
		t.Fatalf("GetAllAsAt(t1) failed: %v", err)
	}
	if len(gotT1) != 2 {
		t.Fatalf("expected 2 entities at t1, got %d", len(gotT1))
	}
	for _, e := range gotT1 {
		if string(e.Data) != `{"v": 1}` {
			t.Errorf("expected original data at t1 for %s, got: %s", e.Meta.ID, e.Data)
		}
	}

	// GetAllAsAt(t2) → entity 1 updated, entity 2 original
	gotT2, err := store.GetAllAsAt(ctx, modelRef, t2)
	if err != nil {
		t.Fatalf("GetAllAsAt(t2) failed: %v", err)
	}
	if len(gotT2) != 2 {
		t.Fatalf("expected 2 entities at t2, got %d", len(gotT2))
	}
	for _, e := range gotT2 {
		if e.Meta.ID == "e-aa-1" {
			if string(e.Data) != `{"v": 2}` {
				t.Errorf("expected updated data for e-aa-1 at t2, got: %s", e.Data)
			}
		} else {
			if string(e.Data) != `{"v": 1}` {
				t.Errorf("expected original data for e-aa-2 at t2, got: %s", e.Data)
			}
		}
	}
}

func TestGetAllAsAtWithDelete(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-aad-1", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"x": 1}`),
	}
	store.Save(ctx, entity)
	beforeDelete := time.Now()

	time.Sleep(time.Millisecond)
	store.Delete(ctx, "e-aad-1")
	afterDelete := time.Now()

	// Before delete → entity present
	got, err := store.GetAllAsAt(ctx, modelRef, beforeDelete)
	if err != nil {
		t.Fatalf("GetAllAsAt(beforeDelete) failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entity before delete, got %d", len(got))
	}
	if string(got[0].Data) != `{"x": 1}` {
		t.Errorf("expected original data, got: %s", got[0].Data)
	}

	// After delete → empty
	got, err = store.GetAllAsAt(ctx, modelRef, afterDelete)
	if err != nil {
		t.Fatalf("GetAllAsAt(afterDelete) failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entities after delete, got %d", len(got))
	}
}

func TestGetVersionHistoryNotFound(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)

	_, err := store.GetVersionHistory(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent entity")
	}
}

func TestCompareAndSaveMatchingTxID(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas-1", TenantID: "tenant-A", ModelRef: modelRef,
			State: "NEW", TransactionID: "tx-001",
		},
		Data: []byte(`{"v": 1}`),
	}
	store.Save(ctx, entity)

	// CompareAndSave with matching txID should succeed.
	entity.Data = []byte(`{"v": 2}`)
	entity.Meta.TransactionID = "tx-002"
	ver, err := store.CompareAndSave(ctx, entity, "tx-001")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if ver != 2 {
		t.Errorf("expected version 2, got %d", ver)
	}

	got, _ := store.Get(ctx, "e-cas-1")
	if string(got.Data) != `{"v": 2}` {
		t.Errorf("expected updated data, got: %s", got.Data)
	}
}

func TestCompareAndSaveMismatchTxID(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas-2", TenantID: "tenant-A", ModelRef: modelRef,
			State: "NEW", TransactionID: "tx-001",
		},
		Data: []byte(`{"v": 1}`),
	}
	store.Save(ctx, entity)

	// CompareAndSave with mismatched txID should fail with ErrConflict.
	entity.Data = []byte(`{"v": 2}`)
	entity.Meta.TransactionID = "tx-002"
	_, err := store.CompareAndSave(ctx, entity, "stale-tx")
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}

	// Verify the entity was not modified.
	got, _ := store.Get(ctx, "e-cas-2")
	if string(got.Data) != `{"v": 1}` {
		t.Errorf("entity should not have been modified, got: %s", got.Data)
	}
}

func TestCompareAndSaveNewEntity(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// CompareAndSave on a new entity (no prior versions) should succeed
	// because there's nothing to conflict with.
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas-new", TenantID: "tenant-A", ModelRef: modelRef,
			State: "NEW", TransactionID: "tx-001",
		},
		Data: []byte(`{"v": 1}`),
	}
	ver, err := store.CompareAndSave(ctx, entity, "any-tx-id")
	if err != nil {
		t.Fatalf("expected success for new entity, got error: %v", err)
	}
	if ver != 1 {
		t.Errorf("expected version 1, got %d", ver)
	}
}

// --- Transaction-aware tests ---

func TestTransactionReadYourOwnWrites(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Begin transaction
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	// Save entity within transaction
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-tx-1", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"tx": true}`),
	}
	_, err = store.Save(txCtx, entity)
	if err != nil {
		t.Fatalf("save in tx failed: %v", err)
	}

	// Get within same transaction → should see it
	got, err := store.Get(txCtx, "e-tx-1")
	if err != nil {
		t.Fatalf("expected to read own write in tx, got error: %v", err)
	}
	if string(got.Data) != `{"tx": true}` {
		t.Errorf("expected buffered data, got: %s", got.Data)
	}
}

func TestTransactionIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Begin transaction
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	// Save entity within transaction
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-tx-iso", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"isolated": true}`),
	}
	_, err = store.Save(txCtx, entity)
	if err != nil {
		t.Fatalf("save in tx failed: %v", err)
	}

	// Get OUTSIDE transaction → should NOT see it
	_, err = store.Get(ctx, "e-tx-iso")
	if err == nil {
		t.Error("expected not-found outside transaction, but entity was visible")
	}

	// Exists outside transaction → false
	exists, _ := store.Exists(ctx, "e-tx-iso")
	if exists {
		t.Error("expected Exists=false outside transaction")
	}

	// Count outside transaction → 0
	count, _ := store.Count(ctx, modelRef)
	if count != 0 {
		t.Errorf("expected count 0 outside tx, got %d", count)
	}
}

func TestTransactionDeleteVisibility(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Pre-create entity outside any transaction (auto-commit)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-tx-del", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"delete-me": true}`),
	}
	store.Save(ctx, entity)

	// Begin transaction, delete within it
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	err = store.Delete(txCtx, "e-tx-del")
	if err != nil {
		t.Fatalf("delete in tx failed: %v", err)
	}

	// Within tx: entity should be invisible
	_, err = store.Get(txCtx, "e-tx-del")
	if err == nil {
		t.Error("expected not-found within tx after delete, but entity was visible")
	}

	// Outside tx: entity should still be visible (not committed yet)
	got, err := store.Get(ctx, "e-tx-del")
	if err != nil {
		t.Fatalf("expected entity visible outside tx, got error: %v", err)
	}
	if string(got.Data) != `{"delete-me": true}` {
		t.Errorf("expected original data outside tx, got: %s", got.Data)
	}
}

func TestTransactionGetAllIncludesBuffer(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Pre-create one entity outside tx
	existing := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-existing", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"existing": true}`),
	}
	store.Save(ctx, existing)

	// Begin transaction, save another entity
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	newEntity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-buffered", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"buffered": true}`),
	}
	store.Save(txCtx, newEntity)

	// GetAll within tx → should include both existing and buffered
	all, err := store.GetAll(txCtx, modelRef)
	if err != nil {
		t.Fatalf("GetAll in tx failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 entities in tx GetAll, got %d", len(all))
	}

	ids := make(map[string]bool)
	for _, e := range all {
		ids[e.Meta.ID] = true
	}
	if !ids["e-existing"] || !ids["e-buffered"] {
		t.Errorf("expected both e-existing and e-buffered, got IDs: %v", ids)
	}

	// GetAll outside tx → should only include existing
	allOutside, err := store.GetAll(ctx, modelRef)
	if err != nil {
		t.Fatalf("GetAll outside tx failed: %v", err)
	}
	if len(allOutside) != 1 {
		t.Fatalf("expected 1 entity outside tx GetAll, got %d", len(allOutside))
	}
}

func TestImplicitAutoCommit(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Save without transaction → visible immediately (existing behavior)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-auto", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"auto": true}`),
	}
	ver, err := store.Save(ctx, entity)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if ver != 1 {
		t.Errorf("expected version 1, got %d", ver)
	}

	got, err := store.Get(ctx, "e-auto")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if string(got.Data) != `{"auto": true}` {
		t.Errorf("expected auto data, got: %s", got.Data)
	}
}

func TestTransactionExistsAndCount(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Begin transaction, save entity
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-tx-exists", TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
		},
		Data: []byte(`{"exists": true}`),
	}
	store.Save(txCtx, entity)

	// Within tx: Exists should be true, Count should be 1
	exists, _ := store.Exists(txCtx, "e-tx-exists")
	if !exists {
		t.Error("expected Exists=true within tx")
	}

	count, _ := store.Count(txCtx, modelRef)
	if count != 1 {
		t.Errorf("expected count 1 within tx, got %d", count)
	}
}

func TestTransactionDeleteAllVisibility(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Pre-create entities
	for _, id := range []string{"e-da-1", "e-da-2"} {
		e := &spi.Entity{
			Meta: spi.EntityMeta{
				ID: id, TenantID: "tenant-A", ModelRef: modelRef, State: "NEW",
			},
			Data: []byte(`{"da": true}`),
		}
		store.Save(ctx, e)
	}

	// Begin tx, DeleteAll
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	err = store.DeleteAll(txCtx, modelRef)
	if err != nil {
		t.Fatalf("deleteAll in tx failed: %v", err)
	}

	// Within tx: GetAll should return 0
	all, _ := store.GetAll(txCtx, modelRef)
	if len(all) != 0 {
		t.Errorf("expected 0 entities in tx after DeleteAll, got %d", len(all))
	}

	// Outside tx: still 2
	allOutside, _ := store.GetAll(ctx, modelRef)
	if len(allOutside) != 2 {
		t.Errorf("expected 2 entities outside tx, got %d", len(allOutside))
	}
}

func TestTransactionCompareAndSave(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	// Pre-create entity with txID
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-tx-cas", TenantID: "tenant-A", ModelRef: modelRef,
			State: "NEW", TransactionID: "tx-original",
		},
		Data: []byte(`{"v": 1}`),
	}
	store.Save(ctx, entity)

	// Begin tx, CompareAndSave with matching txID
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	entity.Data = []byte(`{"v": 2}`)
	entity.Meta.TransactionID = "tx-new"
	_, err = store.CompareAndSave(txCtx, entity, "tx-original")
	if err != nil {
		t.Fatalf("CAS in tx failed: %v", err)
	}

	// Read within tx → should see buffered version
	got, err := store.Get(txCtx, "e-tx-cas")
	if err != nil {
		t.Fatalf("get in tx failed: %v", err)
	}
	if string(got.Data) != `{"v": 2}` {
		t.Errorf("expected buffered data, got: %s", got.Data)
	}

	// CompareAndSave with wrong txID → conflict
	entity.Data = []byte(`{"v": 3}`)
	_, err = store.CompareAndSave(txCtx, entity, "wrong-tx")
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}
}

func TestTransactionalDeleteNonExistentEntity(t *testing.T) {
	factory := memory.NewStoreFactory()
	uuids := newTestUUIDGenerator()
	tm := factory.NewTransactionManager(uuids)
	ctx := ctxWithTenant("tenant-A")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	// Get an EntityStore from the transactional context.
	store, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore failed: %v", err)
	}

	// Try to delete a non-existent entity within the transaction.
	// This should return ErrNotFound, just like non-transactional Delete does.
	err = store.Delete(txCtx, "does-not-exist")
	if err == nil {
		t.Fatal("expected error when deleting non-existent entity in transaction, got nil")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}

	// Clean up: rollback the transaction.
	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
}

// TestGetAllReturnsNonNilOnEmptyModel asserts that GetAll returns a non-nil empty
// slice (not nil) when no entities exist for the requested model. The SPI
// contract requires non-nil so callers can range over the result safely.
func TestGetAllReturnsNonNilOnEmptyModel(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-getall-empty")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	modelRef := spi.ModelRef{EntityName: "m-empty", ModelVersion: "1"}
	got, err := store.GetAll(ctx, modelRef)
	if err != nil {
		t.Fatalf("GetAll: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("GetAll on empty model must return non-nil slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("GetAll on empty model must return empty slice, got %d elements", len(got))
	}
}

// TestGetAllAsAtReturnsNonNilOnEmptyModel asserts the same non-nil guarantee
// for GetAllAsAt on an empty model.
func TestGetAllAsAtReturnsNonNilOnEmptyModel(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-getallasat-empty")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	modelRef := spi.ModelRef{EntityName: "m-empty", ModelVersion: "1"}
	got, err := store.GetAllAsAt(ctx, modelRef, time.Now())
	if err != nil {
		t.Fatalf("GetAllAsAt: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("GetAllAsAt on empty model must return non-nil slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("GetAllAsAt on empty model must return empty slice, got %d elements", len(got))
	}
}

// --- Follow-on-action attribution (#430) ---

// TestSaveAndDelete_ExecutorRoundTrip verifies that Meta.ChangeUser/
// ChangeUserKind/ChangeExecutor stamped by the caller before Save round-trip
// through GetVersionHistory as EntityVersion.AttributedKind/Executor, and
// that a DELETED version's Executor is populated even though Entity is nil.
func TestSaveAndDelete_ExecutorRoundTrip(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	wantExecutor := spi.Principal{ID: "svc-1", Kind: spi.PrincipalService}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:             "e-exec-1",
			TenantID:       "tenant-A",
			ModelRef:       spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			ChangeType:     "CREATED",
			ChangeUser:     "origin-user",
			ChangeUserKind: spi.PrincipalUser,
			ChangeExecutor: wantExecutor,
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(ctx, entity); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	delCtx := ctxWithUserKind("tenant-A", "del-user", spi.PrincipalUser)
	if err := store.Delete(delCtx, "e-exec-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-exec-1")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 versions (CREATE + DELETE), got %d", len(history))
	}

	created := history[0]
	if created.AttributedKind != spi.PrincipalUser {
		t.Errorf("CREATE version AttributedKind = %v, want %v", created.AttributedKind, spi.PrincipalUser)
	}
	if created.Executor != wantExecutor {
		t.Errorf("CREATE version Executor = %+v, want %+v", created.Executor, wantExecutor)
	}

	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	if tomb.Entity != nil {
		t.Errorf("expected nil Entity on a DELETED version, got %+v", tomb.Entity)
	}
	wantDel := spi.Principal{ID: "del-user", Kind: spi.PrincipalUser}
	if tomb.Executor != wantDel {
		t.Errorf("DELETE version Executor = %+v, want %+v (readable without Entity)", tomb.Executor, wantDel)
	}
	if tomb.AttributedKind != spi.PrincipalUser {
		t.Errorf("DELETE version AttributedKind = %v, want %v", tomb.AttributedKind, spi.PrincipalUser)
	}
}

// TestDelete_NonTx_AttributionIsCaller verifies that a non-transactional
// Delete stamps the tombstone's attribution from the caller's own context
// (attributed == executor == caller) via spi.AttributionFor.
func TestDelete_NonTx_AttributionIsCaller(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithUserKind("tenant-A", "alice", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-del-nontx",
			TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(ctx, entity); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := store.Delete(ctx, "e-del-nontx"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-del-nontx")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	want := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	if tomb.User != want.ID {
		t.Errorf("tombstone User = %q, want %q", tomb.User, want.ID)
	}
	if tomb.AttributedKind != want.Kind {
		t.Errorf("tombstone AttributedKind = %v, want %v", tomb.AttributedKind, want.Kind)
	}
	if tomb.Executor != want {
		t.Errorf("tombstone Executor = %+v, want %+v (non-tx: attributed == executor == caller)", tomb.Executor, want)
	}
}

// TestDeleteAll_NonTx_Attribution verifies that non-transactional DeleteAll
// stamps every tombstone's attribution from the caller's context, same as
// single-entity Delete.
func TestDeleteAll_NonTx_Attribution(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithUserKind("tenant-A", "bob", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	modelRef := spi.ModelRef{EntityName: "m-delall", ModelVersion: "1"}
	for _, id := range []string{"e-da-1", "e-da-2"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-A", ModelRef: modelRef},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("Save(%s) failed: %v", id, err)
		}
	}

	if err := store.DeleteAll(ctx, modelRef); err != nil {
		t.Fatalf("DeleteAll failed: %v", err)
	}

	want := spi.Principal{ID: "bob", Kind: spi.PrincipalUser}
	for _, id := range []string{"e-da-1", "e-da-2"} {
		history, err := store.GetVersionHistory(ctx, id)
		if err != nil {
			t.Fatalf("GetVersionHistory(%s) failed: %v", id, err)
		}
		tomb := history[len(history)-1]
		if !tomb.Deleted {
			t.Fatalf("expected %s's last version to be the DELETE tombstone", id)
		}
		if tomb.Executor != want {
			t.Errorf("%s tombstone Executor = %+v, want %+v", id, tomb.Executor, want)
		}
		if tomb.AttributedKind != want.Kind {
			t.Errorf("%s tombstone AttributedKind = %v, want %v", id, tomb.AttributedKind, want.Kind)
		}
	}
}

// TestDeleteAll_Tx_AttributionStaged verifies that a transactional DeleteAll
// stages DeleteAttribution for every affected entity ID, paired with Deletes,
// under the caller's context at stage time.
func TestDeleteAll_Tx_AttributionStaged(t *testing.T) {
	factory, tm := newTxManager(t)
	ctx := ctxWithUserKind("tenant-A", "carol", spi.PrincipalUser)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	modelRef := spi.ModelRef{EntityName: "m-delall-tx", ModelVersion: "1"}
	for _, id := range []string{"e-dat-1", "e-dat-2"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-A", ModelRef: modelRef},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("Save(%s) failed: %v", id, err)
		}
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx) failed: %v", err)
	}
	if err := txStore.DeleteAll(txCtx, modelRef); err != nil {
		t.Fatalf("DeleteAll failed: %v", err)
	}

	tx := spi.GetTransaction(txCtx)
	want := spi.Principal{ID: "carol", Kind: spi.PrincipalUser}
	for _, id := range []string{"e-dat-1", "e-dat-2"} {
		if !tx.Deletes[id] {
			t.Fatalf("expected tx.Deletes[%s] to be staged", id)
		}
		attr, ok := tx.DeleteAttribution[id]
		if !ok {
			t.Fatalf("expected tx.DeleteAttribution[%s] to be staged alongside tx.Deletes", id)
		}
		if attr.Attributed != want || attr.Executor != want {
			t.Errorf("DeleteAttribution[%s] = %+v, want Attributed==Executor==%+v", id, attr, want)
		}
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	for _, id := range []string{"e-dat-1", "e-dat-2"} {
		history, err := store.GetVersionHistory(ctx, id)
		if err != nil {
			t.Fatalf("GetVersionHistory(%s) failed: %v", id, err)
		}
		tomb := history[len(history)-1]
		if !tomb.Deleted {
			t.Fatalf("expected %s's last version to be the DELETE tombstone", id)
		}
		if tomb.Executor != want {
			t.Errorf("%s tombstone Executor = %+v, want %+v", id, tomb.Executor, want)
		}
	}
}
