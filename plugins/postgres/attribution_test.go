package postgres_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// ctxWithTenantAndUser returns a context carrying a UserContext with an
// explicit PrincipalKind, for attribution tests that need deterministic
// control over Kind (unlike ctxWithTenant elsewhere in this package, which
// leaves Kind at its zero value).
func ctxWithTenantAndUser(tenant spi.TenantID, userID string, kind spi.PrincipalKind) context.Context {
	uc := &spi.UserContext{
		UserID: userID,
		Kind:   kind,
		Tenant: spi.Tenant{ID: tenant, Name: string(tenant)},
	}
	return spi.WithUserContext(context.Background(), uc)
}

// TestTxManager_Begin_CapturesOrigin verifies that Begin resolves
// TransactionState.Origin via spi.ResolveOrigin from the caller's
// UserContext.
func TestTxManager_Begin_CapturesOrigin(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenantAndUser("tx-tenant", "alice", spi.PrincipalUser)

	_, txCtx := beginGuarded(t, tm, ctx)

	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		t.Fatal("expected Begin to populate TransactionState in the returned context")
	}
	want := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	if tx.Origin != want {
		t.Errorf("tx.Origin = %+v, want %+v", tx.Origin, want)
	}
}

// TestTxManager_Join_RepopulatesOrigin is THE load-bearing postgres case:
// unlike memory/sqlite (which share a single *spi.TransactionState pointer
// across Join calls), postgres rebuilds a brand-new TransactionState{ID,
// TenantID} on every Join. Without repopulating Origin from the per-tx
// origins map, a joined caller's writes would silently lose attribution to
// the tx's causal root — this is the primary acceptance criterion of the
// whole follow-on-action attribution feature (cross-node cascade
// attribution depends on it).
func TestTxManager_Join_RepopulatesOrigin(t *testing.T) {
	tm, _ := newTestTxManager(t)
	tenant := spi.TenantID("tx-tenant")
	rootCtx := ctxWithTenantAndUser(tenant, "root-user", spi.PrincipalUser)

	txID, txCtx1 := beginGuarded(t, tm, rootCtx)

	tx1 := spi.GetTransaction(txCtx1)
	wantOrigin := spi.Principal{ID: "root-user", Kind: spi.PrincipalUser}
	if tx1.Origin != wantOrigin {
		t.Fatalf("tx1.Origin = %+v, want %+v", tx1.Origin, wantOrigin)
	}

	// Join from a second context: same tenant, different (service-kind)
	// actor — mirrors a cascaded processor callout joining the tx on
	// another node/goroutine.
	joinCtx := ctxWithTenantAndUser(tenant, "joiner-svc", spi.PrincipalService)
	txCtx2, err := tm.Join(joinCtx, txID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	tx2 := spi.GetTransaction(txCtx2)
	if tx2 == nil {
		t.Fatal("expected Join to populate TransactionState in the returned context")
	}
	if tx2.Origin != wantOrigin {
		t.Errorf("tx2.Origin = %+v, want %+v (Join must repopulate Origin from the per-tx map, not the joiner's own Principal)",
			tx2.Origin, wantOrigin)
	}
}

// TestTxManager_Commit_NoOriginLeak verifies that Commit's cleanup removes
// the per-tx origin map entry — the same leak-free lifecycle as tm.tenants.
func TestTxManager_Commit_NoOriginLeak(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenantAndUser("tx-tenant", "alice", spi.PrincipalUser)

	before := postgres.OriginMapLenForTest(tm)
	txID, txCtx := beginGuarded(t, tm, ctx)
	if got := postgres.OriginMapLenForTest(tm); got != before+1 {
		t.Fatalf("origins map len after Begin = %d, want %d", got, before+1)
	}

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := postgres.OriginMapLenForTest(tm); got != before {
		t.Errorf("origins map len after Commit = %d, want %d (leaked entry)", got, before)
	}
}

// TestTxManager_Rollback_NoOriginLeak mirrors TestTxManager_Commit_NoOriginLeak
// for the Rollback exit path.
func TestTxManager_Rollback_NoOriginLeak(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenantAndUser("tx-tenant", "alice", spi.PrincipalUser)

	before := postgres.OriginMapLenForTest(tm)
	txID, txCtx := beginGuarded(t, tm, ctx)
	if got := postgres.OriginMapLenForTest(tm); got != before+1 {
		t.Fatalf("origins map len after Begin = %d, want %d", got, before+1)
	}

	if err := tm.Rollback(txCtx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := postgres.OriginMapLenForTest(tm); got != before {
		t.Errorf("origins map len after Rollback = %d, want %d (leaked entry)", got, before)
	}
}

// newAttrFactory creates a StoreFactory wired to its own TransactionManager
// against a freshly migrated schema, for attribution tests that need both
// EntityStore and direct TransactionManager control.
func newAttrFactory(t *testing.T) (*postgres.StoreFactory, *postgres.TransactionManager) {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	tm := postgres.NewTransactionManager(pool, newTestUUIDGenerator())
	factory := postgres.NewStoreFactoryWithTMForTest(pool, tm)
	return factory, tm
}

// TestEntityStore_Delete_Tx_StampsDeleterNotPriorWriter is the central
// regression test for the tombstone-attribution bug: entity_store.go's
// Delete used to re-marshal the PRIOR doc as-is, so the tombstone's
// ChangeUser/ChangeUserKind/ChangeExecutor recorded the entity's PREVIOUS
// writer, not whoever actually deleted it. Save as "creator", delete as a
// distinct "deleter" inside a transaction, and verify the tombstone records
// the deleter.
func TestEntityStore_Delete_Tx_StampsDeleterNotPriorWriter(t *testing.T) {
	factory, tm := newAttrFactory(t)
	tenant := spi.TenantID("tenant-A")
	creatorCtx := ctxWithTenantAndUser(tenant, "creator-user", spi.PrincipalUser)

	store, err := factory.EntityStore(creatorCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-tomb-tx",
			TenantID: tenant,
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(creatorCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}

	deleterCtx := ctxWithTenantAndUser(tenant, "deleter-user", spi.PrincipalUser)
	txID, txCtx := beginGuarded(t, tm, deleterCtx)
	deleterStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx): %v", err)
	}
	if err := deleterStore.Delete(txCtx, "e-tomb-tx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	wantDeleter := spi.Principal{ID: "deleter-user", Kind: spi.PrincipalUser}

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	outStore, err := factory.EntityStore(creatorCtx)
	if err != nil {
		t.Fatalf("EntityStore(creatorCtx): %v", err)
	}
	metas, err := outStore.GetVersionMetadata(creatorCtx, "e-tomb-tx", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 versions (CREATE + DELETE), got %d", len(metas))
	}
	// Newest first: metas[0] is the DELETE tombstone.
	tombstone := metas[0]
	if !tombstone.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	if tombstone.User != "deleter-user" {
		t.Errorf("tombstone.User = %q, want %q (must be the deleter, not the prior writer)", tombstone.User, "deleter-user")
	}
	if tombstone.AttributedKind != spi.PrincipalUser {
		t.Errorf("tombstone.AttributedKind = %q, want %q", tombstone.AttributedKind, spi.PrincipalUser)
	}
	if tombstone.Executor != wantDeleter {
		t.Errorf("tombstone.Executor = %+v, want %+v", tombstone.Executor, wantDeleter)
	}
}

// TestEntityStore_Delete_Tx_StampsDeletingTransactionID is the regression
// test for a bug GetVersionByTransaction's conformance coverage exposed:
// Delete unmarshals `current` from the entity's PRIOR doc and, before this
// fix, never overwrote its TransactionID — so a tombstone silently kept the
// CREATING transaction's ID instead of the DELETING one. That made
// GetVersionByTransaction(id, deleteTxID) resolve deleteTxID back to the
// (non-deleted) CREATE version and return it instead of ErrNotFound,
// because deleteTxID and createTxID were indistinguishable.
//
// Save and Delete run in two SEPARATE transactions here specifically so
// their IDs differ — proving the tombstone records its OWN transaction, not
// a carried-over stale one.
func TestEntityStore_Delete_Tx_StampsDeletingTransactionID(t *testing.T) {
	factory, tm := newAttrFactory(t)
	tenant := spi.TenantID("tenant-A")
	ctx := ctxWithTenantAndUser(tenant, "creator-user", spi.PrincipalUser)

	createTxID, createTxCtx := beginGuarded(t, tm, ctx)
	createStore, err := factory.EntityStore(createTxCtx)
	if err != nil {
		t.Fatalf("EntityStore(createTxCtx): %v", err)
	}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-tomb-txid",
			TenantID: tenant,
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}
	if _, err := createStore.Save(createTxCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(createTxCtx, createTxID); err != nil {
		t.Fatalf("Commit (create): %v", err)
	}

	deleteTxID, deleteTxCtx := beginGuarded(t, tm, ctx)
	if deleteTxID == createTxID {
		t.Fatal("test setup invalid: create and delete transactions must have different IDs")
	}
	deleteStore, err := factory.EntityStore(deleteTxCtx)
	if err != nil {
		t.Fatalf("EntityStore(deleteTxCtx): %v", err)
	}
	if err := deleteStore.Delete(deleteTxCtx, "e-tomb-txid"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := tm.Commit(deleteTxCtx, deleteTxID); err != nil {
		t.Fatalf("Commit (delete): %v", err)
	}

	outStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	metas, err := outStore.GetVersionMetadata(ctx, "e-tomb-txid", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 versions (CREATE + DELETE), got %d", len(metas))
	}
	tombstone := metas[0] // newest first
	if !tombstone.Deleted {
		t.Fatal("expected newest version to be the DELETE tombstone")
	}
	if tombstone.TransactionID != deleteTxID {
		t.Errorf("tombstone.TransactionID = %q, want %q (the deleting transaction, not the creating one %q)",
			tombstone.TransactionID, deleteTxID, createTxID)
	}

	// GetVersionByTransaction must never resolve the deleting tx's ID back
	// to a live entity — the tombstone carries no payload.
	if _, err := outStore.GetVersionByTransaction(ctx, "e-tomb-txid", deleteTxID); err == nil {
		t.Error("GetVersionByTransaction(deleteTxID) must return an error (the tombstone never matches), got nil")
	}
}

// TestEntityStore_Delete_NonTx_StampsDeleter mirrors the tx case above for a
// non-transactional Delete call (postgres executes it immediately either
// way, but the ctx-plumbing path differs slightly with no TransactionState
// in scope).
func TestEntityStore_Delete_NonTx_StampsDeleter(t *testing.T) {
	factory, _ := newAttrFactory(t)
	tenant := spi.TenantID("tenant-A")
	creatorCtx := ctxWithTenantAndUser(tenant, "creator-user", spi.PrincipalUser)

	store, err := factory.EntityStore(creatorCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-tomb-notx",
			TenantID: tenant,
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(creatorCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}

	deleterCtx := ctxWithTenantAndUser(tenant, "deleter-user", spi.PrincipalUser)
	deleterStore, err := factory.EntityStore(deleterCtx)
	if err != nil {
		t.Fatalf("EntityStore(deleterCtx): %v", err)
	}
	if err := deleterStore.Delete(deleterCtx, "e-tomb-notx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	metas, err := store.GetVersionMetadata(context.Background(), "e-tomb-notx", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	// Newest first: metas[0] is the DELETE tombstone.
	tombstone := metas[0]
	if !tombstone.Deleted {
		t.Fatal("expected newest version to be the DELETE tombstone")
	}
	if tombstone.User != "deleter-user" {
		t.Errorf("tombstone.User = %q, want %q (must be the deleter, not the prior writer)", tombstone.User, "deleter-user")
	}
}

// TestEntityStore_DeleteAll_StampsDeleter verifies DeleteAll (which loops
// over Delete per-id) carries the same attribution-stamping fix.
func TestEntityStore_DeleteAll_StampsDeleter(t *testing.T) {
	factory, _ := newAttrFactory(t)
	tenant := spi.TenantID("tenant-A")
	creatorCtx := ctxWithTenantAndUser(tenant, "creator-user", spi.PrincipalUser)
	mref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	store, err := factory.EntityStore(creatorCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(creatorCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-deleteall", TenantID: tenant, ModelRef: mref},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	deleterCtx := ctxWithTenantAndUser(tenant, "deleter-user", spi.PrincipalUser)
	deleterStore, err := factory.EntityStore(deleterCtx)
	if err != nil {
		t.Fatalf("EntityStore(deleterCtx): %v", err)
	}
	if err := deleterStore.DeleteAll(deleterCtx, mref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	metas, err := store.GetVersionMetadata(context.Background(), "e-deleteall", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	// Newest first: metas[0] is the DELETE tombstone.
	tombstone := metas[0]
	if tombstone.User != "deleter-user" {
		t.Errorf("tombstone.User = %q, want %q", tombstone.User, "deleter-user")
	}
}
