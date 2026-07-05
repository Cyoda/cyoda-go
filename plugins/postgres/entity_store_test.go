package postgres_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// setupEntityTestWithTM creates a StoreFactory that has a TransactionManager
// wired in. Used by hook tests.
func setupEntityTestWithTM(t *testing.T) (*postgres.StoreFactory, *postgres.TransactionManager) {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	uuids := newTestUUIDGenerator()
	tm := postgres.NewTransactionManager(pool, uuids)
	factory := postgres.NewStoreFactoryWithTMForTest(pool, tm)
	return factory, tm
}

func setupEntityTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func makeEntity(id string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            id,
			ModelRef:      spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:         "NEW",
			TransactionID: "tx-100",
			ChangeUser:    "user-1",
		},
		Data: []byte(`{"name":"Widget","amount":42}`),
	}
}

func TestEntityStore_SaveAndGet(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	ent := makeEntity("ent-001")
	ent.Meta.TransitionForLatestSave = "create"

	version, err := store.Save(ctx, ent)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if version != 1 {
		t.Errorf("expected version 1, got %d", version)
	}

	got, err := store.Get(ctx, "ent-001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Meta.ID != "ent-001" {
		t.Errorf("ID = %q, want %q", got.Meta.ID, "ent-001")
	}
	if got.Meta.TenantID != "entity-tenant" {
		t.Errorf("TenantID = %q, want %q", got.Meta.TenantID, "entity-tenant")
	}
	if got.Meta.ModelRef.EntityName != "Order" {
		t.Errorf("ModelRef.EntityName = %q, want %q", got.Meta.ModelRef.EntityName, "Order")
	}
	if got.Meta.ModelRef.ModelVersion != "1" {
		t.Errorf("ModelRef.ModelVersion = %q, want %q", got.Meta.ModelRef.ModelVersion, "1")
	}
	if got.Meta.State != "NEW" {
		t.Errorf("State = %q, want %q", got.Meta.State, "NEW")
	}
	if got.Meta.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Meta.Version)
	}
	if got.Meta.ChangeType != "CREATED" {
		t.Errorf("ChangeType = %q, want %q", got.Meta.ChangeType, "CREATED")
	}
	if got.Meta.ChangeUser != "user-1" {
		t.Errorf("ChangeUser = %q, want %q", got.Meta.ChangeUser, "user-1")
	}
	if got.Meta.TransactionID != "tx-100" {
		t.Errorf("TransactionID = %q, want %q", got.Meta.TransactionID, "tx-100")
	}
	if got.Meta.TransitionForLatestSave != "create" {
		t.Errorf("TransitionForLatestSave = %q, want %q", got.Meta.TransitionForLatestSave, "create")
	}
	if got.Meta.CreationDate.IsZero() {
		t.Error("CreationDate should not be zero")
	}
	if got.Meta.LastModifiedDate.IsZero() {
		t.Error("LastModifiedDate should not be zero")
	}
	if got.Data == nil {
		t.Fatal("Data should not be nil")
	}
}

func TestEntityStore_SaveIncrementsVersion(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-002")

	v1, err := store.Save(ctx, ent)
	if err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if v1 != 1 {
		t.Errorf("expected version 1, got %d", v1)
	}

	ent.Meta.State = "APPROVED"
	v2, err := store.Save(ctx, ent)
	if err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	if v2 != 2 {
		t.Errorf("expected version 2, got %d", v2)
	}

	got, err := store.Get(ctx, "ent-002")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.Version != 2 {
		t.Errorf("Version = %d, want 2", got.Meta.Version)
	}
	if got.Meta.ChangeType != "UPDATED" {
		t.Errorf("ChangeType = %q, want %q", got.Meta.ChangeType, "UPDATED")
	}
}

func TestEntityStore_CompareAndSave_Match(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-cas-1")
	ent.Meta.TransactionID = "tx-original"
	store.Save(ctx, ent)

	ent.Meta.State = "UPDATED_STATE"
	ent.Meta.TransactionID = "tx-new"
	v, err := store.CompareAndSave(ctx, ent, "tx-original")
	if err != nil {
		t.Fatalf("CompareAndSave with matching txID: %v", err)
	}
	if v != 2 {
		t.Errorf("expected version 2, got %d", v)
	}
}

func TestEntityStore_CompareAndSave_Mismatch(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-cas-2")
	ent.Meta.TransactionID = "tx-original"
	store.Save(ctx, ent)

	ent.Meta.TransactionID = "tx-new"
	_, err := store.CompareAndSave(ctx, ent, "tx-WRONG")
	if err == nil {
		t.Fatal("expected ErrConflict for mismatched txID")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("expected ErrConflict, got: %v", err)
	}
}

func TestEntityStore_CompareAndSave_NewEntity(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-cas-new")
	v, err := store.CompareAndSave(ctx, ent, "any-tx-id")
	if err != nil {
		t.Fatalf("CompareAndSave for new entity: %v", err)
	}
	if v != 1 {
		t.Errorf("expected version 1, got %d", v)
	}
}

func TestEntityStore_GetNotFound(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	_, err := store.Get(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent entity")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestEntityStore_GetAsAt(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-asat")
	ent.Data = []byte(`{"value":"v1"}`)
	store.Save(ctx, ent)

	// Record time after v1 was saved
	time.Sleep(2 * time.Millisecond)
	afterV1 := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)

	ent.Data = []byte(`{"value":"v2"}`)
	ent.Meta.State = "MODIFIED"
	store.Save(ctx, ent)

	// GetAsAt at afterV1 should return v1
	got, err := store.GetAsAt(ctx, "ent-asat", afterV1)
	if err != nil {
		t.Fatalf("GetAsAt: %v", err)
	}
	if got.Meta.Version != 1 {
		t.Errorf("GetAsAt version = %d, want 1", got.Meta.Version)
	}

	// GetAsAt at now should return v2
	got2, err := store.GetAsAt(ctx, "ent-asat", time.Now().UTC())
	if err != nil {
		t.Fatalf("GetAsAt now: %v", err)
	}
	if got2.Meta.Version != 2 {
		t.Errorf("GetAsAt now version = %d, want 2", got2.Meta.Version)
	}
}

func TestEntityStore_GetAll(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	store.Save(ctx, makeEntity("ent-a"))
	store.Save(ctx, makeEntity("ent-b"))
	store.Save(ctx, makeEntity("ent-c"))

	// Also save one with a different model
	diff := makeEntity("ent-d")
	diff.Meta.ModelRef = spi.ModelRef{EntityName: "Invoice", ModelVersion: "1"}
	store.Save(ctx, diff)

	all, err := store.GetAll(ctx, ref)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3, got %d", len(all))
	}
}

func TestEntityStore_GetAllAsAt(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	store.Save(ctx, makeEntity("ent-aa-1"))
	store.Save(ctx, makeEntity("ent-aa-2"))

	time.Sleep(2 * time.Millisecond)
	afterFirst := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)

	store.Save(ctx, makeEntity("ent-aa-3"))

	// GetAllAsAt at afterFirst should return 2
	all, err := store.GetAllAsAt(ctx, ref, afterFirst)
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2, got %d", len(all))
	}

	// GetAllAsAt at now should return 3
	allNow, err := store.GetAllAsAt(ctx, ref, time.Now().UTC())
	if err != nil {
		t.Fatalf("GetAllAsAt now: %v", err)
	}
	if len(allNow) != 3 {
		t.Errorf("expected 3, got %d", len(allNow))
	}
}

func TestEntityStore_Delete(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-del")
	store.Save(ctx, ent)

	if err := store.Delete(ctx, "ent-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := store.Get(ctx, "ent-del")
	if err == nil {
		t.Fatal("expected not found after delete")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}

	// Exists should also return false
	exists, err := store.Exists(ctx, "ent-del")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("expected Exists=false after delete")
	}
}

func TestEntityStore_DeleteNotFound(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	err := store.Delete(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent entity")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestEntityStore_DeleteAll(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	store.Save(ctx, makeEntity("ent-da-1"))
	store.Save(ctx, makeEntity("ent-da-2"))

	if err := store.DeleteAll(ctx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	all, err := store.GetAll(ctx, ref)
	if err != nil {
		t.Fatalf("GetAll after DeleteAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 after DeleteAll, got %d", len(all))
	}
}

func TestEntityStore_Exists(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	exists, err := store.Exists(ctx, "no-such")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("expected false for nonexistent entity")
	}

	store.Save(ctx, makeEntity("ent-exists"))
	exists, err = store.Exists(ctx, "ent-exists")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("expected true for existing entity")
	}
}

func TestEntityStore_Count(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	count, err := store.Count(ctx, ref)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	store.Save(ctx, makeEntity("ent-cnt-1"))
	store.Save(ctx, makeEntity("ent-cnt-2"))

	count, err = store.Count(ctx, ref)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}

	// Delete one, count should be 1
	store.Delete(ctx, "ent-cnt-1")
	count, err = store.Count(ctx, ref)
	if err != nil {
		t.Fatalf("Count after delete: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}
}

func TestEntityStore_GetVersionHistory(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-hist")
	store.Save(ctx, ent)

	ent.Meta.State = "APPROVED"
	store.Save(ctx, ent)

	ent.Meta.State = "COMPLETED"
	store.Save(ctx, ent)

	history, err := store.GetVersionHistory(ctx, "ent-hist")
	if err != nil {
		t.Fatalf("GetVersionHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(history))
	}

	if history[0].Version != 1 {
		t.Errorf("history[0].Version = %d, want 1", history[0].Version)
	}
	if history[1].Version != 2 {
		t.Errorf("history[1].Version = %d, want 2", history[1].Version)
	}
	if history[2].Version != 3 {
		t.Errorf("history[2].Version = %d, want 3", history[2].Version)
	}

	if history[0].ChangeType != "CREATED" {
		t.Errorf("history[0].ChangeType = %q, want CREATED", history[0].ChangeType)
	}
	if history[1].ChangeType != "UPDATED" {
		t.Errorf("history[1].ChangeType = %q, want UPDATED", history[1].ChangeType)
	}
}

func TestEntityStore_TenantIsolation(t *testing.T) {
	factory := setupEntityTest(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)

	ent := makeEntity("ent-iso")
	storeA.Save(ctxA, ent)

	// Tenant B cannot see tenant A's entity
	_, err := storeB.Get(ctxB, "ent-iso")
	if err == nil {
		t.Fatal("tenant-B should not see tenant-A's entity")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}

	// Tenant B GetAll should be empty
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	all, err := storeB.GetAll(ctxB, ref)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("tenant-B should see 0, got %d", len(all))
	}

	// Tenant B Exists should be false
	exists, err := storeB.Exists(ctxB, "ent-iso")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("tenant-B should not find tenant-A's entity via Exists")
	}

	// Tenant B Count should be 0
	count, err := storeB.Count(ctxB, ref)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("tenant-B count should be 0, got %d", count)
	}

	// Tenant A still sees it
	got, err := storeA.Get(ctxA, "ent-iso")
	if err != nil {
		t.Fatalf("tenant-A Get: %v", err)
	}
	if got.Meta.ID != "ent-iso" {
		t.Errorf("tenant-A entity ID = %q, want %q", got.Meta.ID, "ent-iso")
	}
}

func TestEntityStore_DeleteCreatesVersionEntry(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, _ := factory.EntityStore(ctx)

	ent := makeEntity("ent-del-ver")
	store.Save(ctx, ent)
	store.Delete(ctx, "ent-del-ver")

	history, err := store.GetVersionHistory(ctx, "ent-del-ver")
	if err != nil {
		t.Fatalf("GetVersionHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 versions (create + delete), got %d", len(history))
	}
	if !history[1].Deleted {
		t.Error("expected last version to be marked deleted")
	}
	if history[1].ChangeType != "DELETED" {
		t.Errorf("expected ChangeType=DELETED, got %q", history[1].ChangeType)
	}
}

// ---------------------------------------------------------------------------
// readSet / writeSet hook tests
// ---------------------------------------------------------------------------

// TestEntityStore_Get_PopulatesReadSet verifies that a Get within a
// transaction records the entity's version in the readSet.
func TestEntityStore_Get_PopulatesReadSet(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("hook-tenant")

	// Seed e1 at version 3 (save three times).
	seedStore, _ := factory.EntityStore(ctx)
	e := makeEntity("e1-read")
	e.Meta.ModelRef = spi.ModelRef{EntityName: "Hook", ModelVersion: "1"}
	seedStore.Save(ctx, e) // v1
	seedStore.Save(ctx, e) // v2
	seedStore.Save(ctx, e) // v3

	// Begin a transaction.
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(txCtx, txID) //nolint:errcheck

	// Get within the transaction.
	txStore, _ := factory.EntityStore(txCtx)
	got, err := txStore.Get(txCtx, "e1-read")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.Version != 3 {
		t.Fatalf("expected version 3, got %d", got.Meta.Version)
	}

	// Verify readSet recorded version 3.
	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	if v := postgres.ReadSetVersionForTest(state, "e1-read"); v != 3 {
		t.Errorf("readSet[e1-read] = %d, want 3", v)
	}
}

// TestEntityStore_Get_NoTxContext_DoesNotRecord verifies that Get outside
// a transaction does not panic and records nothing.
func TestEntityStore_Get_NoTxContext_DoesNotRecord(t *testing.T) {
	factory, _ := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("hook-tenant")

	store, _ := factory.EntityStore(ctx)
	e := makeEntity("e1-notx")
	e.Meta.ModelRef = spi.ModelRef{EntityName: "Hook", ModelVersion: "1"}
	store.Save(ctx, e)

	// Get outside a transaction — no panic expected.
	got, err := store.Get(ctx, "e1-notx")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil entity")
	}
}

// TestEntityStore_Save_FreshInsertNotRecorded verifies that a fresh Save
// (new entity) does NOT record in the writeSet. Fresh inserts are not tracked
// because the UPSERT's ON CONFLICT DO UPDATE handles concurrent inserts
// gracefully at the DB level, and tracking would cause false positives when
// validateInChunks (running inside the same tx) sees the tx's own insert.
func TestEntityStore_Save_FreshInsertNotRecorded(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("hook-tenant")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(txCtx, txID) //nolint:errcheck

	txStore, _ := factory.EntityStore(txCtx)
	e := makeEntity("new-e1")
	e.Meta.ModelRef = spi.ModelRef{EntityName: "Hook", ModelVersion: "1"}
	if _, err := txStore.Save(txCtx, e); err != nil {
		t.Fatalf("Save: %v", err)
	}

	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	_, present := postgres.WriteSetVersionForTest(state, "new-e1")
	if present {
		t.Error("fresh insert must NOT be recorded in writeSet")
	}
}

// TestEntityStore_Save_UpdateRecordsPreWriteVersion verifies that updating
// an existing entity records preWriteVersion = nextVersion-1 in the writeSet.
func TestEntityStore_Save_UpdateRecordsPreWriteVersion(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("hook-tenant")

	// Seed existing entity at v4.
	seedStore, _ := factory.EntityStore(ctx)
	e := makeEntity("existing-e1")
	e.Meta.ModelRef = spi.ModelRef{EntityName: "Hook", ModelVersion: "1"}
	for i := 0; i < 4; i++ {
		seedStore.Save(ctx, e)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(txCtx, txID) //nolint:errcheck

	txStore, _ := factory.EntityStore(txCtx)
	if _, err := txStore.Save(txCtx, e); err != nil {
		t.Fatalf("Save: %v", err)
	}

	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	v, present := postgres.WriteSetVersionForTest(state, "existing-e1")
	if !present {
		t.Fatal("existing-e1 not found in writeSet")
	}
	if v != 4 {
		t.Errorf("writeSet[existing-e1] = %d, want 4 (preWriteVersion = nextVersion-1)", v)
	}
}

// TestEntityStore_Delete_RecordsWriteSet verifies that Delete records the
// entity's pre-delete version in the writeSet.
func TestEntityStore_Delete_RecordsWriteSet(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("hook-tenant")

	// Seed del-e at v6.
	seedStore, _ := factory.EntityStore(ctx)
	e := makeEntity("del-e")
	e.Meta.ModelRef = spi.ModelRef{EntityName: "Hook", ModelVersion: "1"}
	for i := 0; i < 6; i++ {
		seedStore.Save(ctx, e)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(txCtx, txID) //nolint:errcheck

	txStore, _ := factory.EntityStore(txCtx)
	if err := txStore.Delete(txCtx, "del-e"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	v, present := postgres.WriteSetVersionForTest(state, "del-e")
	if !present {
		t.Fatal("del-e not found in writeSet")
	}
	if v != 6 {
		t.Errorf("writeSet[del-e] = %d, want 6", v)
	}
}

// TestEntityStore_GetAll_RecordsEachReadSet verifies that GetAll within a
// transaction records each returned entity's version in the readSet.
func TestEntityStore_GetAll_RecordsEachReadSet(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("hook-tenant")

	ref := spi.ModelRef{EntityName: "Hook", ModelVersion: "1"}

	seedStore, _ := factory.EntityStore(ctx)
	for _, id := range []string{"ga-1", "ga-2", "ga-3"} {
		e := makeEntity(id)
		e.Meta.ModelRef = ref
		seedStore.Save(ctx, e)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(txCtx, txID) //nolint:errcheck

	txStore, _ := factory.EntityStore(txCtx)
	all, err := txStore.GetAll(txCtx, ref)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entities, got %d", len(all))
	}

	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	for _, id := range []string{"ga-1", "ga-2", "ga-3"} {
		if v := postgres.ReadSetVersionForTest(state, id); v != 1 {
			t.Errorf("readSet[%s] = %d, want 1", id, v)
		}
	}
}

// TestEntityStore_GetVersionHistory_NonExistent asserts that GetVersionHistory
// returns spi.ErrNotFound for an entity that has never been saved. This pins
// the contract that the postgres plugin matches the memory and sqlite backends.
func TestEntityStore_GetVersionHistory_NonExistent(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-nonexistent-tenant")
	store, _ := factory.EntityStore(ctx)

	_, err := store.GetVersionHistory(ctx, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for non-existent entity, got nil")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected spi.ErrNotFound, got: %v", err)
	}
}
