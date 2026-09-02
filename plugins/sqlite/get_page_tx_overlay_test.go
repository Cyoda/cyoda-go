package sqlite_test

import (
	"fmt"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Every in-tx page equals the corresponding slice of the full in-tx Iterate
// sequence, and every entity on the returned page is recorded into the
// read-set unconditionally (GetPage's SPI contract: no TrackingRead knob),
// buffered own-writes included.
func TestGetPageTx_PagesEqualIterateSlices_RecordsPageOnly(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-pg-ovl", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 12) // e00..e11

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	for _, id := range []string{"e00", "e05", "e06"} {
		if err := store.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete %s: %v", id, err)
		}
	}
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "b00", TenantID: "tenant-ovl", ModelRef: ref}, Data: []byte(`{}`)})
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e07", TenantID: "tenant-ovl", ModelRef: ref}, Data: []byte(`{"u":1}`)})

	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	full := drainIDs(t, it) // b00 e01 e02 e03 e04 e07 e08 e09 e10 e11

	tx := spi.GetTransaction(txCtx)
	for k := range tx.ReadSet {
		delete(tx.ReadSet, k)
	}
	const limit = 4
	for offset := 0; offset < len(full)+limit; offset += limit {
		page, err := store.GetPage(txCtx, ref, limit, offset, nil)
		if err != nil {
			t.Fatalf("GetPage(offset=%d): %v", offset, err)
		}
		hi := offset + limit
		if hi > len(full) {
			hi = len(full)
		}
		var want []string
		if offset < len(full) {
			want = full[offset:hi]
		}
		if got := pageIDs(t, page); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("page(offset=%d) = %v, want %v", offset, got, want)
		}
	}
	var recorded []string
	for id := range tx.ReadSet {
		recorded = append(recorded, id)
	}
	sort.Strings(recorded)
	// Every page was read, so every id on the full sequence was recorded
	// once — buffered own-writes included, unconditionally.
	wantRecorded := full
	if fmt.Sprint(recorded) != fmt.Sprint(wantRecorded) {
		t.Fatalf("ReadSet = %v, want %v", recorded, wantRecorded)
	}
}
