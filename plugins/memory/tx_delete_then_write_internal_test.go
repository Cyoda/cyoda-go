package memory

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func tenantCtxInternal(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u1", Kind: spi.PrincipalUser,
		Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

func TestSave_AfterDelete_UnstagesBothMaps(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()
	ctx := tenantCtxInternal("tenant-A")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-unstage", ModelVersion: "1"}
	e := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", TenantID: "tenant-A", ModelRef: ref}, Data: []byte(`{}`)}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	tm, _ := f.TransactionManager(ctx)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if !tx.Deletes["e1"] || len(tx.DeleteAttribution) != 1 {
		t.Fatalf("precondition: Deletes=%v Attribution=%v", tx.Deletes, tx.DeleteAttribution)
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
