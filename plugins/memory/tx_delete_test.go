package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newTxDeleteEnv(t *testing.T, ids ...string) (spi.EntityStore, spi.TransactionManager, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for _, id := range ids {
		if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "tenant-A", ModelRef: txDeleteModel}, Data: []byte(`{}`)}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	return store, txMgr, ctx
}

var txDeleteModel = spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

// A second Delete of the same entity in one transaction finds it already
// gone, as on PostgreSQL (its staged delete hides the entity from the
// transaction, as Get already sees).
func TestTxDelete_SecondDeleteIsNotFound(t *testing.T) {
	store, txMgr, ctx := newTxDeleteEnv(t, "o-2")
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = txMgr.Rollback(ctx, txID) }()
	if err := store.Delete(txCtx, "o-2"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := store.Delete(txCtx, "o-2"); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("second Delete = %v, want spi.ErrNotFound", err)
	}
}

// An entity deleted and then created again in the same transaction can be
// deleted again: the create clears the staged delete.
func TestTxDelete_DeleteSaveDelete(t *testing.T) {
	store, txMgr, ctx := newTxDeleteEnv(t, "o-3")
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "o-3"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "o-3", TenantID: "tenant-A", ModelRef: txDeleteModel}, Data: []byte(`{"again":true}`)}); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	if err := store.Delete(txCtx, "o-3"); err != nil {
		t.Fatalf("second Delete = %v, want the re-created entity deleted", err)
	}
	if err := txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, "o-3"); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Get after commit = %v, want spi.ErrNotFound", err)
	}
}

// A DeleteAll in a transaction that already deleted one of the model's
// entities leaves that delete as it was: the tombstone names the first
// deleter, as on PostgreSQL, whose DeleteAll skips a row already deleted.
func TestTxDeleteAll_KeepsAnEarlierDeletesAttribution(t *testing.T) {
	store, txMgr, ctx := newTxDeleteEnv(t, "o-4", "o-5")
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	asUser := func(user string) context.Context {
		return spi.WithUserContext(txCtx, &spi.UserContext{UserID: user, Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: "tenant-A", Name: "tenant-A"}})
	}
	if err := store.Delete(asUser("first"), "o-4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.DeleteAll(asUser("second"), txDeleteModel); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if err := txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	versions, err := store.GetVersionMetadata(ctx, "o-4", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	var tombstones []spi.EntityVersionMeta
	for _, v := range versions {
		if v.Deleted {
			tombstones = append(tombstones, v)
		}
	}
	if len(tombstones) != 1 || tombstones[0].User != "first" {
		t.Errorf("tombstones = %+v, want one, by %q: the first delete is the one that stands", tombstones, "first")
	}
}
