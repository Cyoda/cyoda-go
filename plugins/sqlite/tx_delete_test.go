package sqlite_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A second Delete of the same entity in one transaction finds it already
// gone, as on PostgreSQL (its staged delete hides the entity from the
// transaction, as Get already sees).
func TestTxDelete_SecondDeleteIsNotFound(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-twodel", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-twodel", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 1)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txID) }()
	if err := store.Delete(txCtx, ids[0]); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := store.Delete(txCtx, ids[0]); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("second Delete = %v, want spi.ErrNotFound", err)
	}
}

// An entity deleted and then created again in the same transaction can be
// deleted again: the create clears the staged delete.
func TestTxDelete_DeleteSaveDelete(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-dsd", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-dsd", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 1)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, ids[0]); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: ids[0], TenantID: "tenant-dsd", ModelRef: modelRef}, Data: []byte(`{"again":true}`)}); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	if err := store.Delete(txCtx, ids[0]); err != nil {
		t.Fatalf("second Delete = %v, want the re-created entity deleted", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, ids[0]); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Get after commit = %v, want spi.ErrNotFound", err)
	}
}

// A DeleteAll in a transaction that already deleted one of the model's
// entities leaves that delete as it was: the tombstone names the first
// deleter, as on PostgreSQL, whose DeleteAll skips a row already deleted.
func TestTxDeleteAll_KeepsAnEarlierDeletesAttribution(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-dall", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-dall", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 2)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	asUser := func(user string) context.Context {
		return spi.WithUserContext(txCtx, &spi.UserContext{UserID: user, Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: "tenant-dall", Name: "tenant-dall"}})
	}
	if err := store.Delete(asUser("first"), ids[0]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.DeleteAll(asUser("second"), modelRef); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	versions, err := store.GetVersionMetadata(ctx, ids[0], spi.VersionMetadataOptions{})
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
