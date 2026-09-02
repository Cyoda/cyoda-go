package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The id/state projection yields the same merged view as the full projection —
// committed snapshot ∪ buffer − staged deletes, in entity-ID order — carrying
// the state and no payload at all.
func TestOpenTxOverlay_IDStateProjection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "overlay-idstate.db")
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
	es, ok := store.(*entityStore)
	if !ok {
		t.Fatalf("EntityStore returned %T, want *entityStore", store)
	}
	ref := spi.ModelRef{EntityName: "m-idstate", ModelVersion: "1"}
	seed := []struct{ id, state string }{{"e01", "open"}, {"e02", "closed"}, {"e03", "open"}}
	for _, s := range seed {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: s.id, TenantID: "tenant-A", ModelRef: ref, State: s.state},
			Data: []byte(fmt.Sprintf(`{"id":%q}`, s.id)),
		}); err != nil {
			t.Fatalf("seed Save(%s): %v", s.id, err)
		}
	}

	txID, txCtx, err := f.tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = f.tm.Rollback(txCtx, txID) }()
	if _, err := store.Save(txCtx, &spi.Entity{ // buffered add, sorts first
		Meta: spi.EntityMeta{ID: "a00", TenantID: "tenant-A", ModelRef: ref, State: "draft"},
		Data: []byte(`{"id":"a00"}`),
	}); err != nil {
		t.Fatalf("Save add: %v", err)
	}
	if err := store.Delete(txCtx, "e02"); err != nil { // staged delete
		t.Fatalf("Delete: %v", err)
	}
	tx := spi.GetTransaction(txCtx)

	var overlay *txOverlay
	err = func() error {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		var oErr error
		overlay, oErr = es.openTxOverlay(ctx, tx, ref, spi.Filter{}, projectIDState)
		return oErr
	}()
	if err != nil {
		t.Fatalf("openTxOverlay: %v", err)
	}
	defer overlay.Close()

	var got []string
	for {
		e, ok, err := overlay.pull()
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if !ok {
			break
		}
		if e.Data != nil {
			t.Errorf("%s: Data = %q, want nil (the projection reads no payload)", e.Meta.ID, e.Data)
		}
		got = append(got, e.Meta.ID+"="+e.Meta.State)
	}
	want := []string{"a00=draft", "e01=open", "e03=open"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("id/state pairs = %v, want %v", got, want)
	}
}
