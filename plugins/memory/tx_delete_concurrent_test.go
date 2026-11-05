package memory_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A transaction that deletes an entity its snapshot sees, which another
// transaction deleted and committed meanwhile, loses a write race: its
// Delete is staged and its Commit is refused as a conflict, as on SQLite and
// PostgreSQL — not answered "not found" at once.
func TestTxDelete_ConcurrentlyDeletedEntityIsAConflict(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "o-1", TenantID: "tenant-A", ModelRef: modelRef}, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	otherID, otherCtx, err := txMgr.Begin(ctx) // another writer, committed
	if err != nil {
		t.Fatalf("Begin other: %v", err)
	}
	if err := store.Delete(otherCtx, "o-1"); err != nil {
		t.Fatalf("concurrent Delete: %v", err)
	}
	if err := txMgr.Commit(otherCtx, otherID); err != nil {
		t.Fatalf("concurrent Commit: %v", err)
	}

	if err := store.Delete(txCtx, "o-1"); err != nil {
		t.Fatalf("Delete in the transaction = %v, want it staged: its snapshot sees the entity", err)
	}
	if err := txMgr.Commit(txCtx, txID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit = %v, want spi.ErrConflict", err)
	}
}
