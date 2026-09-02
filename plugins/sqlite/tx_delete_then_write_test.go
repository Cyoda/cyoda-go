package sqlite_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type contextT = context.Context

func seedOne(t *testing.T, store spi.EntityStore, ctx contextT, id string, ref spi.ModelRef) *spi.Entity {
	t.Helper()
	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: "tenant-dtw", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	return got
}

// A write compares against the transaction's own view: the same-tx delete
// is the current latest, so CompareAndSave must conflict — on every backend.
// Postgres already does; sqlite looked past the buffered delete at the
// committed row and let the save through, resurrecting the entity at commit.
func TestTx_DeleteThenCompareAndSave_Conflicts(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-dtw", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	committed := seedOne(t, store, ctx, "e-cas", ref)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-cas"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	update := &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}
	_, err = store.CompareAndSave(txCtx, update, committed.Meta.TransactionID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave after same-tx Delete: err = %v, want ErrConflict", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, "e-cas"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("after commit Get: err = %v, want ErrNotFound (the delete must stand)", err)
	}
}

// Save after a same-tx Delete is last-write-wins: the entity is present after
// commit and carries the new payload — and no tombstone is written for it.
func TestTx_DeleteThenSave_CommitsPresent(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-dtw", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	committed := seedOne(t, store, ctx, "e-save", ref)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-save"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := store.Get(ctx, "e-save")
	if err != nil {
		t.Fatalf("after commit Get: %v", err)
	}
	if string(got.Data) != `{"n":2}` {
		t.Fatalf("after commit Data = %s, want {\"n\":2}", got.Data)
	}
	versions, err := store.GetVersionMetadata(ctx, "e-save", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	for _, v := range versions {
		if v.Deleted {
			t.Fatalf("a DELETED version was written for an entity whose delete was unstaged: %+v", versions)
		}
	}
}
