package postgres_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestTx_OpAfterCommit_WrapsSentinel reproduces the defect: a store
// operation issued against a transaction's context AFTER that transaction
// has committed must classify through an SPI sentinel the handler layer can
// act on. Postgres retains proof of a successful commit (submitTimes,
// populated at Commit and outliving cleanupTx) for the operations this test
// runs, so the strict assertion is spi.ErrTxAlreadyCommitted — the looser
// spi.ErrTxNotFound fallback is for backends/paths that cannot tell
// "committed" from "unknown" at this seam. Before the fix, postgres returned
// a bare deadTxError with no Unwrap, so neither errors.Is check held for any
// of these operations — memory and sqlite already classify the same
// sequence correctly, and this pins postgres to the same contract.
func TestTx_OpAfterCommit_WrapsSentinel(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("tenant-op-after-commit")
	ref := spi.ModelRef{EntityName: "m-op-after-commit", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	seed := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-oac", TenantID: "tenant-op-after-commit", ModelRef: ref, State: "open", TransactionID: "tx-seed", ChangeUser: "user-1"},
		Data: []byte(`{"n":1}`),
	}
	if _, err := store.Save(ctx, seed); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, err := store.Get(ctx, "e-oac")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	assertSentinel := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error on a committed transaction's context, got nil", name)
		}
		if !errors.Is(err, spi.ErrTxAlreadyCommitted) {
			t.Fatalf("%s: expected errors.Is(err, spi.ErrTxAlreadyCommitted), got: %v", name, err)
		}
	}

	_, err = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-oac-new", ModelRef: ref, State: "open", ChangeUser: "user-1"}, Data: []byte(`{}`)})
	assertSentinel("Save", err)

	_, err = store.CompareAndSave(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}, committed.Meta.TransactionID)
	assertSentinel("CompareAndSave", err)

	err = store.Delete(txCtx, "e-oac")
	assertSentinel("Delete", err)

	err = store.DeleteAll(txCtx, ref)
	assertSentinel("DeleteAll", err)

	_, err = store.GetPage(txCtx, ref, 10, 0, nil)
	assertSentinel("GetPage", err)

	_, err = store.Count(txCtx, ref)
	assertSentinel("Count", err)

	_, err = store.CountByState(txCtx, ref, nil)
	assertSentinel("CountByState", err)

	_, err = store.Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	assertSentinel("Iterate", err)

	_, err = store.Get(txCtx, "e-oac")
	assertSentinel("Get", err)
}
