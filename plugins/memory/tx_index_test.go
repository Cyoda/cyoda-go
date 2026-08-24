package memory_test

import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func txIndexCtx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "alice",
		Kind:   spi.PrincipalUser,
		Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

// TestRecordTxIndex_FirstWriteWins: the tx-index promises "the earliest
// NON-DELETED version that transaction txID wrote for that entity", and
// GetVersionByTransaction's contract is earliest-wins. The transactional path
// upholds that by passing the first version once per commit, but the
// non-transactional path calls recordTxIndex per save with the caller's
// TransactionID taken verbatim — so two non-tx saves carrying the same txID
// recorded the LATEST version, where the SQL backends' ORDER BY version ASC
// LIMIT 1 returns the earliest. The invariant has to hold structurally, not
// by caller discipline.
func TestRecordTxIndex_FirstWriteWins(t *testing.T) {
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx("tenant-txidx")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	ref := spi.ModelRef{EntityName: "m-txidx", ModelVersion: "1"}
	const sharedTx = "tx-shared"
	for _, payload := range []string{`{"v":1}`, `{"v":2}`} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "e1", ModelRef: ref, TransactionID: sharedTx},
			Data: []byte(payload),
		}); err != nil {
			t.Fatalf("Save %s: %v", payload, err)
		}
	}

	ev, err := store.GetVersionByTransaction(ctx, "e1", sharedTx)
	if err != nil {
		t.Fatalf("GetVersionByTransaction: %v", err)
	}
	if ev.Version != 1 {
		t.Errorf("GetVersionByTransaction returned version %d, want 1 (earliest wins)", ev.Version)
	}
	if !strings.Contains(string(ev.Entity.Data), `"v":1`) {
		t.Errorf("GetVersionByTransaction returned %s, want the earliest save's payload", ev.Entity.Data)
	}
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
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx("tenant-txidx-del")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-txidx-del", ModelVersion: "1"}
	const createTx = "tx-create"

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: ref, TransactionID: createTx},
		Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
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
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: ref, TransactionID: "tx-create-2"},
		Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Save e2: %v", err)
	}
	if err := store.DeleteAll(ctx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if _, err := store.GetVersionByTransaction(ctx, "e2", "tx-create-2"); err != nil {
		t.Errorf("GetVersionByTransaction after DeleteAll: %v", err)
	}
	if _, err := store.GetVersionByTransaction(ctx, "e1", createTx); err != nil {
		t.Errorf("GetVersionByTransaction after DeleteAll (earlier entity): %v", err)
	}
}
