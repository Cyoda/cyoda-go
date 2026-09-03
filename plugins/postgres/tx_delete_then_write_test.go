package postgres_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The other half of the same-transaction-delete story: the empty expected ID
// does NOT mean "expect no entity" — it is a caller error, so it cannot
// re-create what the same-transaction delete removed either. The delete
// stands at commit. A caller that wants the entity back calls Save. Same
// answer the buffered backends give.
func TestTx_DeleteThenCompareAndSaveEmpty_Rejected(t *testing.T) {
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
	// A t.Fatal below would otherwise leave the transaction holding a pooled
	// connection, and the fixture's pool.Close would block forever.
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, err = store.CompareAndSave(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}, "")
	if err == nil {
		t.Fatal("CompareAndSave with an empty expected ID after same-tx Delete: err = nil, want a rejection")
	}
	if errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave with an empty expected ID: err = %v, want a plain caller error, not ErrConflict", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, "e-cas-recreate"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("after commit Get: err = %v, want ErrNotFound (the delete must stand)", err)
	}
}
