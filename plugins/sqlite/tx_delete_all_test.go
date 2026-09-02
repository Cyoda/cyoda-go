package sqlite_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// In-tx DeleteAll stages every committed id of the model and every buffered
// id, and after commit none of them exist. (Behavioural pin for the
// id-only rewrite; the rewrite itself is reviewed, not measured.)
func TestTxDeleteAll_StagesCommittedAndBuffered(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-dall", ModelVersion: "1"}
	other := spi.ModelRef{EntityName: "m-keep", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 3)
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "k00", TenantID: "tenant-ovl", ModelRef: other}, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	txID, txCtx, _ := tm.Begin(ctx)
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "b00", TenantID: "tenant-ovl", ModelRef: ref}, Data: []byte(`{}`)})
	if err := store.DeleteAll(txCtx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if n, _ := store.Count(txCtx, ref); n != 0 {
		t.Fatalf("in-tx Count after DeleteAll = %d, want 0", n)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, id := range []string{"e00", "e01", "e02", "b00"} {
		if _, err := store.Get(ctx, id); !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("Get(%s) after commit: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := store.Get(ctx, "k00"); err != nil {
		t.Fatalf("other model's entity must survive: %v", err)
	}
}
