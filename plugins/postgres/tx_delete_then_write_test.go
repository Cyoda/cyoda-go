package postgres_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The other half of the same-transaction-delete story: an empty expected ID
// means "expect no entity", which is exactly what the delete left, so the
// compare-and-save is allowed and re-creates the entity — present after
// commit and carrying no DELETED version. Postgres applies a Delete eagerly
// on the transaction's own connection rather than buffering it, so there is
// no unstageDelete step to get wrong here; this pins the same outcome the
// buffered backends reach a different way.
func TestTx_DeleteThenCompareAndSaveEmpty_RecreatesPresent(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("tenant-dtw")
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	seed := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-cas-recreate", TenantID: "tenant-dtw", ModelRef: ref, State: "open", TransactionID: "tx-seed", ChangeUser: "user-1"},
		Data: []byte(`{"n":1}`),
	}
	if _, err := store.Save(ctx, seed); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, err := store.Get(ctx, "e-cas-recreate")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-cas-recreate"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.CompareAndSave(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}, ""); err != nil {
		t.Fatalf("CompareAndSave with empty expected ID after same-tx Delete: %v, want success", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := store.Get(ctx, "e-cas-recreate")
	if err != nil {
		t.Fatalf("after commit Get: %v", err)
	}
	if string(got.Data) != `{"n":2}` {
		t.Fatalf("after commit Data = %s, want {\"n\":2}", got.Data)
	}
	// Unlike memory/sqlite, postgres applies the Delete eagerly, so the
	// audit trail does carry a DELETED row (the eager tombstone) before the
	// re-create's version — that is expected, not a bug. What must not
	// happen is a DELETED row AFTER it: that would mean the tombstone landed
	// on top of the re-created entity instead of under it.
	versions, err := store.GetVersionMetadata(ctx, "e-cas-recreate", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	for _, v := range versions {
		if v.Deleted && v.Version > got.Meta.Version {
			t.Fatalf("a DELETED version (%d) follows the re-create's version (%d): %+v", v.Version, got.Meta.Version, versions)
		}
	}
}
