package memory_test

import (
	"context"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/plugins/memory"
)

func txIndexCtx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "alice",
		Kind:   spi.PrincipalUser,
		Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

// TestGetVersionByTransaction_SurvivesDelete pins the lifetime of the
// tx-index: deleting an entity appends a tombstone but keeps its version
// history, and the SQL backends still answer GetVersionByTransaction from
// entity_versions afterwards (their DELETE writes a new row, it does not
// remove the old ones). Purging the tx-index on Delete/DeleteAll — a tempting
// way to "reclaim" it — would silently drop answers the other backends still
// give.
func TestGetVersionByTransaction_SurvivesDelete(t *testing.T) {
	f := memory.NewStoreFactory()
	lockModels(t, f, ctxWithTenant("tenant-txidx-del"), spi.ModelRef{EntityName: "m-txidx-del", ModelVersion: "1"})
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx("tenant-txidx-del")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm := f.NewTransactionManager(newTestUUIDGenerator())
	ref := spi.ModelRef{EntityName: "m-txidx-del", ModelVersion: "1"}

	createTx := saveCommitted(t, tm, store, ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: ref},
		Data: []byte(`{"v":1}`),
	})
	if err := store.Delete(ctx, "e1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ev, err := store.GetVersionByTransaction(ctx, "e1", createTx)
	if err != nil {
		t.Fatalf("GetVersionByTransaction after Delete: %v", err)
	}
	if ev.Version != 1 {
		t.Errorf("GetVersionByTransaction after Delete returned version %d, want 1", ev.Version)
	}

	// DeleteAll takes the same path and must not change the answer either.
	createTx2 := saveCommitted(t, tm, store, ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: ref},
		Data: []byte(`{"v":1}`),
	})
	if err := store.DeleteAll(ctx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if _, err := store.GetVersionByTransaction(ctx, "e2", createTx2); err != nil {
		t.Errorf("GetVersionByTransaction after DeleteAll: %v", err)
	}
	if _, err := store.GetVersionByTransaction(ctx, "e1", createTx); err != nil {
		t.Errorf("GetVersionByTransaction after DeleteAll (earlier entity): %v", err)
	}
}
