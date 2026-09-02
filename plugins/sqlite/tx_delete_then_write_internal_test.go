package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Deletes and DeleteAttribution always cover the same key set (SPI
// TransactionState doc). Save after Delete must clear both, not just Deletes.
func TestSave_AfterDelete_UnstagesBothMaps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "unstage.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	ctx := attrInternalCtx("tenant-A", "alice", spi.PrincipalUser)
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-unstage", ModelVersion: "1"}
	e := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", TenantID: "tenant-A", ModelRef: ref}, Data: []byte(`{}`)}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	txID, txCtx, err := f.tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = f.tm.Rollback(txCtx, txID) }()
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if !tx.Deletes["e1"] || len(tx.DeleteAttribution) != 1 {
		t.Fatalf("precondition: delete not staged in both maps: Deletes=%v Attribution=%v", tx.Deletes, tx.DeleteAttribution)
	}
	if _, err := store.Save(txCtx, e); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if tx.Deletes["e1"] {
		t.Errorf("tx.Deletes still holds e1 after Save")
	}
	if _, ok := tx.DeleteAttribution["e1"]; ok {
		t.Errorf("tx.DeleteAttribution still holds e1 after Save")
	}
}
