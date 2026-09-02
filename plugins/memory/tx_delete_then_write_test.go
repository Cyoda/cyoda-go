package memory_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestTx_DeleteThenCompareAndSave_Conflicts(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-dtw"))
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	// A real seed transaction ID, so the compare-and-save below names a
	// version the delete superseded rather than the empty ID — which means
	// "expect no entity" and would legitimately re-create.
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-cas", TenantID: "tenant-dtw", ModelRef: ref, TransactionID: "tx-seed"}, Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, err := store.Get(ctx, "e-cas")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-cas"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = store.CompareAndSave(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}, committed.Meta.TransactionID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave after same-tx Delete: err = %v, want ErrConflict", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, "e-cas"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("after commit Get: err = %v, want ErrNotFound", err)
	}
}

func TestTx_DeleteThenSave_CommitsPresent(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-dtw"))
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-save", TenantID: "tenant-dtw", ModelRef: ref}, Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, _ := store.Get(ctx, "e-save")

	txID, txCtx, _ := tm.Begin(ctx)
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
		t.Fatalf("Data = %s, want {\"n\":2}", got.Data)
	}
	versions, err := store.GetVersionMetadata(ctx, "e-save", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	for _, v := range versions {
		if v.Deleted {
			t.Fatalf("a DELETED version was written for an unstaged delete: %+v", versions)
		}
	}
}

// A write compares against the transaction's own view. The buffered
// own-write carries this transaction's ID, so an expected ID naming the
// committed version is stale and conflicts — as postgres answers.
func TestTx_SaveThenCompareAndSave_Conflicts(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-dtw"))
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-scas", TenantID: "tenant-dtw", ModelRef: ref}, Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, err := store.Get(ctx, "e-scas")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// The engine stamps a buffered entity with the transaction's own ID.
	inTx := committed.Meta
	inTx.TransactionID = txID
	if _, err := store.Save(txCtx, &spi.Entity{Meta: inTx, Data: []byte(`{"n":2}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err = store.CompareAndSave(txCtx, &spi.Entity{Meta: inTx, Data: []byte(`{"n":3}`)}, committed.Meta.TransactionID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave with the committed version's transaction ID: err = %v, want ErrConflict", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := store.Get(ctx, "e-scas")
	if err != nil {
		t.Fatalf("after commit Get: %v", err)
	}
	if string(got.Data) != `{"n":2}` {
		t.Fatalf("after commit Data = %s, want {\"n\":2} (the Save must stand)", got.Data)
	}
}

// The other half of the same rule: an expected ID naming the transaction's
// own buffered version matches, and the compare-and-save proceeds. This is
// how a joined callback updates an entity a processor created inside the
// transaction it joined.
func TestTx_SaveThenCompareAndSave_WithOwnTxID_Succeeds(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-dtw"))
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-scas-own", TenantID: "tenant-dtw", ModelRef: ref}, Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, err := store.Get(ctx, "e-scas-own")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	inTx := committed.Meta
	inTx.TransactionID = txID
	if _, err := store.Save(txCtx, &spi.Entity{Meta: inTx, Data: []byte(`{"n":2}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := store.CompareAndSave(txCtx, &spi.Entity{Meta: inTx, Data: []byte(`{"n":3}`)}, txID); err != nil {
		t.Fatalf("CompareAndSave with the transaction's own ID: %v, want success", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := store.Get(ctx, "e-scas-own")
	if err != nil {
		t.Fatalf("after commit Get: %v", err)
	}
	if string(got.Data) != `{"n":3}` {
		t.Fatalf("after commit Data = %s, want {\"n\":3}", got.Data)
	}
}
