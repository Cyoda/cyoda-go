package memory_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestTx_UnstampedWrite_CarriesTheTransactionID pins the stamping contract a
// caller sees from inside its own transaction: a write made in a transaction
// carries THAT transaction's ID from the moment it is written, whether or not
// the caller supplied one. A compare-and-save against tx.ID must therefore
// match the transaction's own buffered write.
//
// Backends diverged here. This one stamped only at commit, so an in-tx
// compare-and-save compared against the raw value the caller staged — empty
// for a caller that does not stamp — while postgres stamped at write time and
// matched. Callers in this repo all stamp, which is why it stayed latent.
func TestTx_UnstampedWrite_CarriesTheTransactionID(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-stamp"))
	ref := spi.ModelRef{EntityName: "m-stamp", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// No TransactionID supplied: the store owns the stamp.
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-stamp", TenantID: "tenant-stamp", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save(tx): %v", err)
	}

	got, err := store.Get(txCtx, "e-stamp")
	if err != nil {
		t.Fatalf("Get(tx): %v", err)
	}
	if got.Meta.TransactionID != txID {
		t.Errorf("in-tx read TransactionID = %q, want %q", got.Meta.TransactionID, txID)
	}

	if _, err := store.CompareAndSave(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-stamp", TenantID: "tenant-stamp", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"n":2}`),
	}, txID); err != nil {
		t.Fatalf("CompareAndSave(tx, txID): %v — the transaction's own write is what tx.ID names", err)
	}

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	committed, err := store.Get(ctx, "e-stamp")
	if err != nil {
		t.Fatalf("Get(committed): %v", err)
	}
	if committed.Meta.TransactionID != txID {
		t.Errorf("committed TransactionID = %q, want %q", committed.Meta.TransactionID, txID)
	}
}

// TestTx_ForeignStampIsOverwritten pins the other half: a caller cannot make a
// row claim it was committed by a transaction that did not commit it.
func TestTx_ForeignStampIsOverwritten(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-stamp2"))
	ref := spi.ModelRef{EntityName: "m-stamp2", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-foreign", TenantID: "tenant-stamp2", ModelRef: ref, State: "NEW", TransactionID: "some-other-tx"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save(tx): %v", err)
	}
	got, err := store.Get(txCtx, "e-foreign")
	if err != nil {
		t.Fatalf("Get(tx): %v", err)
	}
	if got.Meta.TransactionID != txID {
		t.Errorf("in-tx read TransactionID = %q, want %q (a caller-supplied ID is not authoritative)", got.Meta.TransactionID, txID)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	committed, err := store.Get(ctx, "e-foreign")
	if err != nil {
		t.Fatalf("Get(committed): %v", err)
	}
	if committed.Meta.TransactionID != txID {
		t.Errorf("committed TransactionID = %q, want %q", committed.Meta.TransactionID, txID)
	}
}
