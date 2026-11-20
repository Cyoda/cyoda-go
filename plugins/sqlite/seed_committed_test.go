package sqlite_test

import (
	"context"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
)

// saveCommitted saves e in a transaction of its own, commits it and returns
// that transaction's id, the id the stored version carries. A write outside a
// transaction records none, so a test that needs an entity with a real
// transaction id — to name it in a CompareAndSave, say — seeds it this way.
func saveCommitted(t *testing.T, tm spi.TransactionManager, store spi.EntityStore, ctx context.Context, e *spi.Entity) string {
	t.Helper()
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	if _, err := store.Save(txCtx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}
	return txID
}
