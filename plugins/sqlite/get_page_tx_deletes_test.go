package sqlite_test

import (
	"context"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// seedPageDeleteEntities saves n entities with byte-wise-ordered IDs
// "e00".."e09" (etc.) under modelRef, non-transactionally, and returns the
// IDs in ascending order.
func seedPageDeleteEntities(t *testing.T, store spi.EntityStore, ctx context.Context, modelRef spi.ModelRef, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%02d", i)
		ids[i] = id
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-pgdel", ModelRef: modelRef},
			Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatalf("seed Save(%s) failed: %v", id, err)
		}
	}
	return ids
}

func pageIDs(t *testing.T, page []*spi.Entity) []string {
	t.Helper()
	ids := make([]string, len(page))
	for i, e := range page {
		ids[i] = e.Meta.ID
	}
	return ids
}

// TestGetPageTx_DeletesAtHeadOfPage is the reviewer's repro: 10 committed
// entities e00..e09, stage deletes on the first 5 inside an ambient tx, then
// GetPage(limit=5, offset=0) must return e05..e09 — not an empty page.
// getPageTx's committed prefetch was bounded to LIMIT offset+limit, which
// only accounts for buffered adds shrinking the need, not staged deletes
// shadowing (and thus wasting) committed rows within that same bound.
func TestGetPageTx_DeletesAtHeadOfPage(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-pgdel", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-pgdel-head", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 10)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx): %v", err)
	}
	for _, id := range ids[:5] {
		if err := txStore.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete(%s): %v", id, err)
		}
	}

	page, err := txStore.GetPage(txCtx, modelRef, 5, 0, nil)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	want := ids[5:10]
	got := pageIDs(t, page)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("GetPage(limit=5,offset=0) with first 5 deleted = %v, want %v", got, want)
	}
}

// TestGetPageTx_DeletesSpanningPageBoundary interleaves deletes among
// surviving entities so the buggy fixed-size committed prefetch runs out
// mid-page (silent under-fill) rather than going fully empty.
func TestGetPageTx_DeletesSpanningPageBoundary(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-pgdel", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-pgdel-span", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 10)
	// Delete e02, e04, e06, e08 — interleaved with survivors, so the
	// remaining committed set is e00,e01,e03,e05,e07,e09.
	toDelete := []string{ids[2], ids[4], ids[6], ids[8]}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx): %v", err)
	}
	for _, id := range toDelete {
		if err := txStore.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete(%s): %v", id, err)
		}
	}

	page, err := txStore.GetPage(txCtx, modelRef, 5, 0, nil)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	want := []string{ids[0], ids[1], ids[3], ids[5], ids[7]}
	got := pageIDs(t, page)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("GetPage(limit=5,offset=0) with e02/e04/e06/e08 deleted = %v, want %v", got, want)
	}
}

// TestGetPageTx_DeletesWithOffset combines a non-zero offset with deletes
// concentrated at the head of the committed set, so the buggy prefetch
// (LIMIT offset+limit) fetches ONLY deleted rows and returns an empty page
// instead of paging into the survivors.
func TestGetPageTx_DeletesWithOffset(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-pgdel", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-pgdel-offset", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 10)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx): %v", err)
	}
	for _, id := range ids[:5] {
		if err := txStore.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete(%s): %v", id, err)
		}
	}

	// Remaining committed set: e05..e09. offset=2,limit=2 -> e07,e08.
	page, err := txStore.GetPage(txCtx, modelRef, 2, 2, nil)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	want := []string{ids[7], ids[8]}
	got := pageIDs(t, page)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("GetPage(limit=2,offset=2) with first 5 deleted = %v, want %v", got, want)
	}
}
